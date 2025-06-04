use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::sync::{Mutex, RwLock};
use tokio::time::{interval, sleep};

use crate::app::AppManager;
use crate::containerd_manager::{Container, ContainerStatus};
use crate::node::NodeManager;
use crate::service_discovery::ServiceDiscovery;

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct HealthCheckConfig {
    pub check_interval_seconds: u64,
    pub failure_threshold: u32,
    pub restart_delay_seconds: u64,
    pub max_restart_attempts: u32,
    pub health_check_timeout_seconds: u64,
}

impl Default for HealthCheckConfig {
    fn default() -> Self {
        Self {
            check_interval_seconds: 30,
            failure_threshold: 3,
            restart_delay_seconds: 5,
            max_restart_attempts: 5,
            health_check_timeout_seconds: 10,
        }
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ContainerHealth {
    pub container_id: String,
    pub last_check: i64,
    pub consecutive_failures: u32,
    pub restart_count: u32,
    pub status: HealthStatus,
    pub last_restart: Option<i64>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub enum HealthStatus {
    Healthy,
    Unhealthy,
    Degraded,
    Failed,
    Restarting,
}

impl std::fmt::Display for HealthStatus {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            HealthStatus::Healthy => write!(f, "healthy"),
            HealthStatus::Unhealthy => write!(f, "unhealthy"),
            HealthStatus::Degraded => write!(f, "degraded"),
            HealthStatus::Failed => write!(f, "failed"),
            HealthStatus::Restarting => write!(f, "restarting"),
        }
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct NodeHealth {
    pub node_id: String,
    pub last_seen: i64,
    pub cpu_usage: f64,
    pub memory_usage: f64,
    pub disk_usage: f64,
    pub network_status: NetworkHealthStatus,
    pub container_count: u32,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub enum NetworkHealthStatus {
    Connected,
    Degraded,
    Disconnected,
}

pub struct HealthMonitor {
    config: HealthCheckConfig,
    app_manager: Arc<AppManager>,
    node_manager: Arc<NodeManager>,
    service_discovery: Arc<ServiceDiscovery>,
    container_health: Arc<RwLock<HashMap<String, ContainerHealth>>>,
    node_health: Arc<RwLock<HashMap<String, NodeHealth>>>,
    restart_queue: Arc<Mutex<Vec<String>>>,
    is_running: Arc<Mutex<bool>>,
}

impl HealthMonitor {
    pub fn new(
        app_manager: Arc<AppManager>,
        node_manager: Arc<NodeManager>,
        service_discovery: Arc<ServiceDiscovery>,
    ) -> Self {
        Self::with_config(
            app_manager,
            node_manager,
            service_discovery,
            HealthCheckConfig::default(),
        )
    }

    pub fn with_config(
        app_manager: Arc<AppManager>,
        node_manager: Arc<NodeManager>,
        service_discovery: Arc<ServiceDiscovery>,
        config: HealthCheckConfig,
    ) -> Self {
        Self {
            config,
            app_manager,
            node_manager,
            service_discovery,
            container_health: Arc::new(RwLock::new(HashMap::new())),
            node_health: Arc::new(RwLock::new(HashMap::new())),
            restart_queue: Arc::new(Mutex::new(Vec::new())),
            is_running: Arc::new(Mutex::new(false)),
        }
    }

    pub async fn start(&self) -> Result<()> {
        let mut is_running = self.is_running.lock().await;
        if *is_running {
            return Ok(());
        }
        *is_running = true;
        drop(is_running);

        println!("Starting health monitor with check interval: {}s", self.config.check_interval_seconds);

        // Start container health checking task
        let container_monitor = self.clone();
        tokio::spawn(async move {
            container_monitor.container_health_loop().await;
        });

        // Start node health checking task
        let node_monitor = self.clone();
        tokio::spawn(async move {
            node_monitor.node_health_loop().await;
        });

        // Start restart processor task
        let restart_processor = self.clone();
        tokio::spawn(async move {
            restart_processor.restart_processor_loop().await;
        });

        Ok(())
    }

    pub async fn stop(&self) {
        let mut is_running = self.is_running.lock().await;
        *is_running = false;
        println!("Health monitor stopped");
    }

    async fn container_health_loop(&self) {
        let mut interval = interval(Duration::from_secs(self.config.check_interval_seconds));

        loop {
            {
                let is_running = self.is_running.lock().await;
                if !*is_running {
                    break;
                }
            }

            interval.tick().await;
            
            if let Err(e) = self.check_container_health().await {
                eprintln!("Error checking container health: {}", e);
            }
        }
    }

    async fn node_health_loop(&self) {
        let mut interval = interval(Duration::from_secs(self.config.check_interval_seconds * 2));

        loop {
            {
                let is_running = self.is_running.lock().await;
                if !*is_running {
                    break;
                }
            }

            interval.tick().await;
            
            if let Err(e) = self.check_node_health().await {
                eprintln!("Error checking node health: {}", e);
            }
        }
    }

    async fn restart_processor_loop(&self) {
        let mut interval = interval(Duration::from_secs(5));

        loop {
            {
                let is_running = self.is_running.lock().await;
                if !*is_running {
                    break;
                }
            }

            interval.tick().await;
            
            if let Err(e) = self.process_restart_queue().await {
                eprintln!("Error processing restart queue: {}", e);
            }
        }
    }

    async fn check_container_health(&self) -> Result<()> {
        let containers = self.app_manager.get_container_details().await?;
        let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;

        for container in containers {
            let is_healthy = self.check_container_status(&container).await;
            
            let mut health_map = self.container_health.write().await;
            let health = health_map.entry(container.id.clone()).or_insert_with(|| {
                ContainerHealth {
                    container_id: container.id.clone(),
                    last_check: now,
                    consecutive_failures: 0,
                    restart_count: 0,
                    status: HealthStatus::Healthy,
                    last_restart: None,
                }
            });

            health.last_check = now;

            if is_healthy {
                health.consecutive_failures = 0;
                health.status = HealthStatus::Healthy;
            } else {
                health.consecutive_failures += 1;
                
                if health.consecutive_failures >= self.config.failure_threshold {
                    if health.restart_count < self.config.max_restart_attempts {
                        health.status = HealthStatus::Failed;
                        
                        // Add to restart queue
                        let mut restart_queue = self.restart_queue.lock().await;
                        if !restart_queue.contains(&container.id) {
                            restart_queue.push(container.id.clone());
                            println!("Container {} marked for restart (failure #{}/{})", 
                                container.id, health.consecutive_failures, self.config.failure_threshold);
                        }
                    } else {
                        health.status = HealthStatus::Failed;
                        println!("Container {} has exceeded max restart attempts ({})", 
                            container.id, self.config.max_restart_attempts);
                    }
                } else {
                    health.status = HealthStatus::Unhealthy;
                }
            }
        }

        Ok(())
    }

    async fn check_container_status(&self, container: &Container) -> bool {
        // Check if container is running
        match container.status {
            ContainerStatus::Running => {
                // Additional health checks could be added here:
                // - HTTP health check endpoints
                // - Process health checks
                // - Resource usage checks
                
                // For now, check if container metrics are available
                if let Some(stats) = self.app_manager.get_container_stats(&container.id).await {
                    // Check if metrics are recent (within last 5 minutes)
                    let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64;
                    if now - stats.last_updated < 300 {
                        // Check for reasonable resource usage
                        if stats.cpu_usage < 100.0 && stats.memory_usage > 0 {
                            return true;
                        }
                    }
                }
                false
            }
            ContainerStatus::Stopped | ContainerStatus::Failed => false,
            ContainerStatus::Pending | ContainerStatus::Restarting => true, // Don't restart these
        }
    }

    async fn check_node_health(&self) -> Result<()> {
        let nodes = self.node_manager.list_nodes().await?;
        let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;

        for node_id in nodes {
            // Get node information
            let node_info = self.get_node_system_info(&node_id).await;
            
            let mut health_map = self.node_health.write().await;
            health_map.insert(node_id.clone(), NodeHealth {
                node_id: node_id.clone(),
                last_seen: now,
                cpu_usage: node_info.cpu_usage,
                memory_usage: node_info.memory_usage,
                disk_usage: node_info.disk_usage,
                network_status: node_info.network_status,
                container_count: node_info.container_count,
            });
        }

        Ok(())
    }

    async fn get_node_system_info(&self, _node_id: &str) -> NodeHealth {
        // In a real implementation, this would collect actual system metrics
        // For now, generate reasonable mock data
        NodeHealth {
            node_id: _node_id.to_string(),
            last_seen: SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64,
            cpu_usage: rand::random::<f64>() * 50.0 + 10.0, // 10-60%
            memory_usage: rand::random::<f64>() * 40.0 + 20.0, // 20-60%
            disk_usage: rand::random::<f64>() * 30.0 + 10.0, // 10-40%
            network_status: NetworkHealthStatus::Connected,
            container_count: rand::random::<u32>() % 10, // 0-9 containers
        }
    }

    async fn process_restart_queue(&self) -> Result<()> {
        let mut restart_queue = self.restart_queue.lock().await;
        if restart_queue.is_empty() {
            return Ok(());
        }

        let container_ids: Vec<String> = restart_queue.drain(..).collect();
        drop(restart_queue);

        for container_id in container_ids {
            if let Err(e) = self.restart_container(&container_id).await {
                eprintln!("Failed to restart container {}: {}", container_id, e);
                
                // Mark as failed in health status
                let mut health_map = self.container_health.write().await;
                if let Some(health) = health_map.get_mut(&container_id) {
                    health.status = HealthStatus::Failed;
                }
            }
        }

        Ok(())
    }

    async fn restart_container(&self, container_id: &str) -> Result<()> {
        println!("Attempting to restart container: {}", container_id);

        // Update health status to restarting
        {
            let mut health_map = self.container_health.write().await;
            if let Some(health) = health_map.get_mut(container_id) {
                health.status = HealthStatus::Restarting;
                health.restart_count += 1;
                health.last_restart = Some(SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64);
            }
        }

        // Wait for restart delay
        sleep(Duration::from_secs(self.config.restart_delay_seconds)).await;

        // Restart the container
        match self.app_manager.restart_container(container_id).await {
            Ok(success) => {
                if success {
                    println!("Successfully restarted container: {}", container_id);
                    
                    // Reset failure count on successful restart
                    let mut health_map = self.container_health.write().await;
                    if let Some(health) = health_map.get_mut(container_id) {
                        health.consecutive_failures = 0;
                        health.status = HealthStatus::Healthy;
                    }
                } else {
                    return Err(anyhow::anyhow!("Container restart returned false"));
                }
            }
            Err(e) => {
                return Err(e.context("Failed to restart container"));
            }
        }

        Ok(())
    }

    pub async fn get_container_health(&self, container_id: &str) -> Option<ContainerHealth> {
        let health_map = self.container_health.read().await;
        health_map.get(container_id).cloned()
    }

    pub async fn get_all_container_health(&self) -> HashMap<String, ContainerHealth> {
        let health_map = self.container_health.read().await;
        health_map.clone()
    }

    pub async fn get_node_health(&self, node_id: &str) -> Option<NodeHealth> {
        let health_map = self.node_health.read().await;
        health_map.get(node_id).cloned()
    }

    pub async fn get_all_node_health(&self) -> HashMap<String, NodeHealth> {
        let health_map = self.node_health.read().await;
        health_map.clone()
    }

    pub async fn force_restart_container(&self, container_id: &str) -> Result<()> {
        let mut restart_queue = self.restart_queue.lock().await;
        if !restart_queue.contains(&container_id.to_string()) {
            restart_queue.push(container_id.to_string());
        }
        Ok(())
    }

    pub async fn get_health_summary(&self) -> HealthSummary {
        let container_health = self.get_all_container_health().await;
        let node_health = self.get_all_node_health().await;

        let healthy_containers = container_health.values()
            .filter(|h| matches!(h.status, HealthStatus::Healthy))
            .count();
        
        let unhealthy_containers = container_health.values()
            .filter(|h| !matches!(h.status, HealthStatus::Healthy))
            .count();

        let healthy_nodes = node_health.values()
            .filter(|h| matches!(h.network_status, NetworkHealthStatus::Connected))
            .count();

        let total_restarts: u32 = container_health.values()
            .map(|h| h.restart_count)
            .sum();

        HealthSummary {
            total_containers: container_health.len() as u32,
            healthy_containers: healthy_containers as u32,
            unhealthy_containers: unhealthy_containers as u32,
            total_nodes: node_health.len() as u32,
            healthy_nodes: healthy_nodes as u32,
            total_restarts,
            last_check: SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64,
        }
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct HealthSummary {
    pub total_containers: u32,
    pub healthy_containers: u32,
    pub unhealthy_containers: u32,
    pub total_nodes: u32,
    pub healthy_nodes: u32,
    pub total_restarts: u32,
    pub last_check: i64,
}

impl Clone for HealthMonitor {
    fn clone(&self) -> Self {
        Self {
            config: self.config.clone(),
            app_manager: Arc::clone(&self.app_manager),
            node_manager: Arc::clone(&self.node_manager),
            service_discovery: Arc::clone(&self.service_discovery),
            container_health: Arc::clone(&self.container_health),
            node_health: Arc::clone(&self.node_health),
            restart_queue: Arc::clone(&self.restart_queue),
            is_running: Arc::clone(&self.is_running),
        }
    }
}