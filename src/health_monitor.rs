use anyhow::Result;
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
    pub custom_health_check_command: Option<String>,
    pub node_check_interval_seconds: u64,
    pub node_failure_threshold: u32,
}

impl Default for HealthCheckConfig {
    fn default() -> Self {
        Self {
            check_interval_seconds: 30,
            failure_threshold: 3,
            restart_delay_seconds: 5,
            max_restart_attempts: 5,
            health_check_timeout_seconds: 10,
            custom_health_check_command: None,
            node_check_interval_seconds: 60,
            node_failure_threshold: 3,
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
    pub health_history: Vec<HealthHistoryEntry>,
    pub custom_check_result: Option<CustomHealthCheckResult>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct HealthHistoryEntry {
    pub timestamp: i64,
    pub status: HealthStatus,
    pub message: Option<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct CustomHealthCheckResult {
    pub exit_code: i32,
    pub output: String,
    pub timestamp: i64,
    pub success: bool,
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
    pub health_history: Vec<NodeHealthHistoryEntry>,
    pub consecutive_failures: u32,
    pub is_healthy: bool,
    pub last_failure: Option<i64>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct NodeHealthHistoryEntry {
    pub timestamp: i64,
    pub cpu_usage: f64,
    pub memory_usage: f64,
    pub disk_usage: f64,
    pub network_status: NetworkHealthStatus,
    pub message: Option<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub enum NetworkHealthStatus {
    Connected,
    Degraded,
    Disconnected,
}

impl std::fmt::Display for NetworkHealthStatus {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            NetworkHealthStatus::Connected => write!(f, "connected"),
            NetworkHealthStatus::Degraded => write!(f, "degraded"),
            NetworkHealthStatus::Disconnected => write!(f, "disconnected"),
        }
    }
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
    alerts: Arc<RwLock<Vec<Alert>>>,
    metrics_history: Arc<RwLock<HashMap<String, Vec<MetricDataPoint>>>>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Alert {
    pub id: String,
    pub timestamp: i64,
    pub severity: AlertSeverity,
    pub source: AlertSource,
    pub message: String,
    pub status: AlertStatus,
    pub resolved_at: Option<i64>,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub enum AlertSeverity {
    Info,
    Warning,
    Critical,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub enum AlertSource {
    Container(String),
    Node(String),
    System,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub enum AlertStatus {
    Active,
    Resolved,
    Acknowledged,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct MetricDataPoint {
    pub timestamp: i64,
    pub value: f64,
    pub metric_type: MetricType,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq, Hash)]
pub enum MetricType {
    CpuUsage,
    MemoryUsage,
    DiskUsage,
    NetworkRx,
    NetworkTx,
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
            alerts: Arc::new(RwLock::new(Vec::new())),
            metrics_history: Arc::new(RwLock::new(HashMap::new())),
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
        let mut interval = interval(Duration::from_secs(self.config.node_check_interval_seconds));

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
                // Generate alert for node health check failure
                self.create_alert(
                    AlertSeverity::Warning,
                    AlertSource::System,
                    format!("Node health check error: {}", e),
                ).await;
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
            let (is_healthy, health_details) = self.check_container_status(&container).await;
            
            let mut health_map = self.container_health.write().await;
            let health = health_map.entry(container.id.clone()).or_insert_with(|| {
                ContainerHealth {
                    container_id: container.id.clone(),
                    last_check: now,
                    consecutive_failures: 0,
                    restart_count: 0,
                    status: HealthStatus::Healthy,
                    last_restart: None,
                    health_history: Vec::new(),
                    custom_check_result: None,
                }
            });

            health.last_check = now;

            // Store custom health check result if available
            if let Some(custom_result) = health_details {
                health.custom_check_result = Some(custom_result);
            }

            // Add entry to health history
            let current_status = if is_healthy {
                HealthStatus::Healthy
            } else if health.consecutive_failures + 1 >= self.config.failure_threshold {
                HealthStatus::Failed
            } else {
                HealthStatus::Unhealthy
            };

            // Add to history (limit to 100 entries)
            health.health_history.push(HealthHistoryEntry {
                timestamp: now,
                status: current_status.clone(),
                message: None,
            });
            
            if health.health_history.len() > 100 {
                health.health_history.remove(0);
            }

            // Store metrics in history
            if let Some(stats) = self.app_manager.get_container_stats(&container.id).await {
                self.store_container_metrics(&container.id, &stats, now).await;
            }

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
                            
                            // Create alert for container failure
                            self.create_alert(
                                AlertSeverity::Warning,
                                AlertSource::Container(container.id.clone()),
                                format!("Container {} health check failed, marked for restart", container.id),
                            ).await;
                        }
                    } else {
                        health.status = HealthStatus::Failed;
                        println!("Container {} has exceeded max restart attempts ({})",
                            container.id, self.config.max_restart_attempts);
                        
                        // Create critical alert for container that exceeded restart attempts
                        self.create_alert(
                            AlertSeverity::Critical,
                            AlertSource::Container(container.id.clone()),
                            format!("Container {} has exceeded maximum restart attempts ({})",
                                container.id, self.config.max_restart_attempts),
                        ).await;
                    }
                } else {
                    health.status = HealthStatus::Unhealthy;
                }
            }
        }

        Ok(())
    }

    async fn check_container_status(&self, container: &Container) -> (bool, Option<CustomHealthCheckResult>) {
        // Check if container is running
        match container.status {
            ContainerStatus::Running => {
                // First, check if there's a custom health check command configured
                if let Some(custom_command) = &self.config.custom_health_check_command {
                    if let Some(result) = self.run_custom_health_check(container, custom_command).await {
                        return (result.success, Some(result));
                    }
                }
                
                // Next, check if the container has a configured health check
                if let Ok(health_status) = self.app_manager.get_container_health_status(&container.id).await {
                    // Use the health check status to determine container health
                    match health_status {
                        crate::containerd_manager::HealthCheckStatus::Healthy => return (true, None),
                        crate::containerd_manager::HealthCheckStatus::Unhealthy => return (false, None),
                        crate::containerd_manager::HealthCheckStatus::Unknown => {
                            // Fall back to metrics check if health status is unknown
                        }
                    }
                }
                
                // If no health check or health status is unknown, check metrics
                if let Some(stats) = self.app_manager.get_container_stats(&container.id).await {
                    // Check if metrics are recent (within last 5 minutes)
                    let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64;
                    if now - stats.last_updated < 300 {
                        // Check for reasonable resource usage
                        if stats.cpu_usage < 100.0 && stats.memory_usage > 0 {
                            return (true, None);
                        }
                    }
                }
                (false, None)
            }
            ContainerStatus::Stopped | ContainerStatus::Failed => (false, None),
            ContainerStatus::Pending | ContainerStatus::Restarting => (true, None), // Don't restart these
        }
    }
    
    async fn run_custom_health_check(&self, container: &Container, command: &str) -> Option<CustomHealthCheckResult> {
        // In a real implementation, this would execute the command inside the container
        // For now, we'll simulate a health check based on container ID
        let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64;
        let success = container.id.as_bytes().iter().sum::<u8>() % 5 != 0; // 80% success rate
        
        Some(CustomHealthCheckResult {
            exit_code: if success { 0 } else { 1 },
            output: if success {
                "Health check passed".to_string()
            } else {
                "Health check failed".to_string()
            },
            timestamp: now,
            success,
        })
    }

    async fn check_node_health(&self) -> Result<()> {
        let nodes = self.node_manager.list_nodes().await?;
        let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;

        for node_id in nodes {
            // Get node information
            let node_info = self.get_node_system_info(&node_id).await;
            
            // Check if node is healthy
            let is_healthy = self.evaluate_node_health(&node_info).await;
            
            let mut health_map = self.node_health.write().await;
            let node_health = health_map.entry(node_id.clone()).or_insert_with(|| {
                NodeHealth {
                    node_id: node_id.clone(),
                    last_seen: now,
                    cpu_usage: node_info.cpu_usage,
                    memory_usage: node_info.memory_usage,
                    disk_usage: node_info.disk_usage,
                    network_status: node_info.network_status.clone(),
                    container_count: node_info.container_count,
                    health_history: Vec::new(),
                    consecutive_failures: 0,
                    is_healthy: true,
                    last_failure: None,
                }
            });
            
            // Update node health
            node_health.last_seen = now;
            node_health.cpu_usage = node_info.cpu_usage;
            node_health.memory_usage = node_info.memory_usage;
            node_health.disk_usage = node_info.disk_usage;
            node_health.network_status = node_info.network_status.clone();
            node_health.container_count = node_info.container_count;
            
            // Add to history (limit to 100 entries)
            node_health.health_history.push(NodeHealthHistoryEntry {
                timestamp: now,
                cpu_usage: node_info.cpu_usage,
                memory_usage: node_info.memory_usage,
                disk_usage: node_info.disk_usage,
                network_status: node_info.network_status.clone(),
                message: None,
            });
            
            if node_health.health_history.len() > 100 {
                node_health.health_history.remove(0);
            }
            
            // Store metrics in history
            self.store_node_metrics(&node_id, &node_info, now).await;
            
            // Update health status
            if is_healthy {
                if node_health.consecutive_failures > 0 {
                    // Node recovered
                    self.create_alert(
                        AlertSeverity::Info,
                        AlertSource::Node(node_id.clone()),
                        format!("Node {} recovered after {} failures", node_id, node_health.consecutive_failures),
                    ).await;
                }
                node_health.consecutive_failures = 0;
                node_health.is_healthy = true;
            } else {
                node_health.consecutive_failures += 1;
                
                if node_health.consecutive_failures >= self.config.node_failure_threshold {
                    if node_health.is_healthy {
                        // Node just became unhealthy
                        node_health.is_healthy = false;
                        node_health.last_failure = Some(now);
                        
                        // Create alert for node failure
                        self.create_alert(
                            AlertSeverity::Critical,
                            AlertSource::Node(node_id.clone()),
                            format!("Node {} is unhealthy: CPU {}%, Memory {}%, Disk {}%, Network {}",
                                node_id,
                                node_info.cpu_usage,
                                node_info.memory_usage,
                                node_info.disk_usage,
                                node_info.network_status),
                        ).await;
                        
                        // Attempt node recovery
                        if let Err(e) = self.attempt_node_recovery(&node_id).await {
                            eprintln!("Failed to recover node {}: {}", node_id, e);
                        }
                    }
                } else if node_health.consecutive_failures == self.config.node_failure_threshold / 2 {
                    // Warning at half the threshold
                    self.create_alert(
                        AlertSeverity::Warning,
                        AlertSource::Node(node_id.clone()),
                        format!("Node {} showing signs of stress: CPU {}%, Memory {}%, Disk {}%",
                            node_id, node_info.cpu_usage, node_info.memory_usage, node_info.disk_usage),
                    ).await;
                }
            }
        }

        Ok(())
    }
    
    async fn evaluate_node_health(&self, node_info: &NodeHealth) -> bool {
        // Check if CPU, memory, or disk usage is too high
        if node_info.cpu_usage > 90.0 || node_info.memory_usage > 90.0 || node_info.disk_usage > 90.0 {
            return false;
        }
        
        // Check network status
        if node_info.network_status != NetworkHealthStatus::Connected {
            return false;
        }
        
        true
    }
    
    async fn attempt_node_recovery(&self, node_id: &str) -> Result<()> {
        println!("Attempting recovery for node {}", node_id);
        
        // In a real implementation, this would attempt to recover the node
        // For example, by restarting services, freeing resources, etc.
        
        // For now, just log the attempt
        println!("Recovery attempt for node {} completed", node_id);
        
        Ok(())
    }

    async fn get_node_system_info(&self, node_id: &str) -> NodeHealth {
        // In a real implementation, this would collect actual system metrics
        // For now, generate reasonable mock data with some variability based on node_id
        
        // Use node_id to seed the randomness for consistent behavior per node
        let seed = node_id.bytes().fold(0u64, |acc, b| acc.wrapping_add(b as u64));
        let mut rng = rand::rngs::StdRng::seed_from_u64(seed);
        
        // Generate base values with some randomness
        let base_cpu = (seed % 30) as f64;
        let base_memory = (seed % 40) as f64;
        let base_disk = (seed % 20) as f64;
        
        // Add time-based variation
        let time_factor = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs() % 100;
        
        let cpu_usage = base_cpu + 10.0 + (time_factor as f64 / 10.0);
        let memory_usage = base_memory + 20.0 + (time_factor as f64 / 20.0);
        let disk_usage = base_disk + 10.0 + (time_factor as f64 / 30.0);
        
        // Determine network status (mostly connected, occasionally degraded)
        let network_status = if rand::random::<u32>() % 20 == 0 {
            NetworkHealthStatus::Degraded
        } else {
            NetworkHealthStatus::Connected
        };
        
        // Container count based on node_id
        let container_count = (seed % 10) as u32;
        
        NodeHealth {
            node_id: node_id.to_string(),
            last_seen: SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64,
            cpu_usage,
            memory_usage,
            disk_usage,
            network_status,
            container_count,
            health_history: Vec::new(),
            consecutive_failures: 0,
            is_healthy: true,
            last_failure: None,
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
        let alerts = self.get_all_alerts().await;

        let healthy_containers = container_health.values()
            .filter(|h| matches!(h.status, HealthStatus::Healthy))
            .count();
        
        let unhealthy_containers = container_health.values()
            .filter(|h| !matches!(h.status, HealthStatus::Healthy))
            .count();

        let healthy_nodes = node_health.values()
            .filter(|h| h.is_healthy)
            .count();
            
        let unhealthy_nodes = node_health.values()
            .filter(|h| !h.is_healthy)
            .count();

        let total_restarts: u32 = container_health.values()
            .map(|h| h.restart_count)
            .sum();
            
        // Calculate average resource usage
        let mut total_cpu = 0.0;
        let mut total_memory = 0.0;
        let mut total_disk = 0.0;
        let node_count = node_health.len() as f64;
        
        for node in node_health.values() {
            total_cpu += node.cpu_usage;
            total_memory += node.memory_usage;
            total_disk += node.disk_usage;
        }
        
        let avg_cpu_usage = if node_count > 0.0 { total_cpu / node_count } else { 0.0 };
        let avg_memory_usage = if node_count > 0.0 { total_memory / node_count } else { 0.0 };
        let avg_disk_usage = if node_count > 0.0 { total_disk / node_count } else { 0.0 };
        
        // Count active and critical alerts
        let active_alerts = alerts.iter()
            .filter(|a| a.status == AlertStatus::Active)
            .count() as u32;
            
        let critical_alerts = alerts.iter()
            .filter(|a| a.status == AlertStatus::Active && a.severity == AlertSeverity::Critical)
            .count() as u32;
            
        // Determine health trend based on alerts and failures in the last hour
        let health_trend = self.calculate_health_trend().await;

        HealthSummary {
            total_containers: container_health.len() as u32,
            healthy_containers: healthy_containers as u32,
            unhealthy_containers: unhealthy_containers as u32,
            total_nodes: node_health.len() as u32,
            healthy_nodes: healthy_nodes as u32,
            unhealthy_nodes: unhealthy_nodes as u32,
            total_restarts,
            last_check: SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64,
            active_alerts,
            critical_alerts,
            avg_cpu_usage,
            avg_memory_usage,
            avg_disk_usage,
            health_trend,
        }
    }
    
    async fn calculate_health_trend(&self) -> HealthTrend {
        // In a real implementation, this would analyze historical data
        // to determine if the system health is improving, stable, or degrading
        
        // For now, use a simple heuristic based on active alerts and container health
        let alerts = self.get_all_alerts().await;
        let container_health = self.get_all_container_health().await;
        
        let critical_alerts = alerts.iter()
            .filter(|a| a.status == AlertStatus::Active && a.severity == AlertSeverity::Critical)
            .count();
            
        let unhealthy_containers = container_health.values()
            .filter(|h| !matches!(h.status, HealthStatus::Healthy))
            .count();
            
        if critical_alerts > 2 || unhealthy_containers > container_health.len() / 2 {
            HealthTrend::Degrading
        } else if critical_alerts == 0 && unhealthy_containers == 0 {
            HealthTrend::Improving
        } else {
            HealthTrend::Stable
        }
    }
    
    // Alert management methods
    async fn create_alert(&self, severity: AlertSeverity, source: AlertSource, message: String) {
        let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64;
        let alert_id = format!("alert-{}-{}", now, uuid::Uuid::new_v4().to_string().split('-').next().unwrap());
        
        let alert = Alert {
            id: alert_id,
            timestamp: now,
            severity,
            source,
            message,
            status: AlertStatus::Active,
            resolved_at: None,
        };
        
        // Add to alerts collection
        let mut alerts = self.alerts.write().await;
        alerts.push(alert.clone());
        
        // Log the alert
        match severity {
            AlertSeverity::Info => println!("INFO ALERT: {}", message),
            AlertSeverity::Warning => println!("WARNING ALERT: {}", message),
            AlertSeverity::Critical => println!("CRITICAL ALERT: {}", message),
        }
    }
    
    async fn resolve_alert(&self, alert_id: &str) -> Result<()> {
        let mut alerts = self.alerts.write().await;
        
        if let Some(alert) = alerts.iter_mut().find(|a| a.id == alert_id) {
            alert.status = AlertStatus::Resolved;
            alert.resolved_at = Some(SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64);
            println!("Alert {} resolved", alert_id);
            Ok(())
        } else {
            Err(anyhow::anyhow!("Alert not found: {}", alert_id))
        }
    }
    
    async fn acknowledge_alert(&self, alert_id: &str) -> Result<()> {
        let mut alerts = self.alerts.write().await;
        
        if let Some(alert) = alerts.iter_mut().find(|a| a.id == alert_id) {
            alert.status = AlertStatus::Acknowledged;
            println!("Alert {} acknowledged", alert_id);
            Ok(())
        } else {
            Err(anyhow::anyhow!("Alert not found: {}", alert_id))
        }
    }
    
    pub async fn get_active_alerts(&self) -> Vec<Alert> {
        let alerts = self.alerts.read().await;
        alerts.iter()
            .filter(|a| a.status == AlertStatus::Active)
            .cloned()
            .collect()
    }
    
    pub async fn get_all_alerts(&self) -> Vec<Alert> {
        let alerts = self.alerts.read().await;
        alerts.clone()
    }
    
    // Metrics history methods
    async fn store_container_metrics(&self, container_id: &str, stats: &crate::containerd_manager::ContainerStats, timestamp: i64) {
        let mut metrics = self.metrics_history.write().await;
        
        // Store CPU usage
        let cpu_key = format!("container:{}.cpu", container_id);
        let cpu_point = MetricDataPoint {
            timestamp,
            value: stats.cpu_usage,
            metric_type: MetricType::CpuUsage,
        };
        
        metrics.entry(cpu_key).or_insert_with(Vec::new).push(cpu_point);
        
        // Store memory usage
        let mem_key = format!("container:{}.memory", container_id);
        let mem_point = MetricDataPoint {
            timestamp,
            value: stats.memory_usage as f64,
            metric_type: MetricType::MemoryUsage,
        };
        
        metrics.entry(mem_key).or_insert_with(Vec::new).push(mem_point);
        
        // Store network metrics
        let net_rx_key = format!("container:{}.net_rx", container_id);
        let net_rx_point = MetricDataPoint {
            timestamp,
            value: stats.network_rx as f64,
            metric_type: MetricType::NetworkRx,
        };
        
        metrics.entry(net_rx_key).or_insert_with(Vec::new).push(net_rx_point);
        
        let net_tx_key = format!("container:{}.net_tx", container_id);
        let net_tx_point = MetricDataPoint {
            timestamp,
            value: stats.network_tx as f64,
            metric_type: MetricType::NetworkTx,
        };
        
        metrics.entry(net_tx_key).or_insert_with(Vec::new).push(net_tx_point);
        
        // Limit history to 1000 points per metric
        for (_, points) in metrics.iter_mut() {
            if points.len() > 1000 {
                points.drain(0..points.len() - 1000);
            }
        }
    }
    
    async fn store_node_metrics(&self, node_id: &str, node_info: &NodeHealth, timestamp: i64) {
        let mut metrics = self.metrics_history.write().await;
        
        // Store CPU usage
        let cpu_key = format!("node:{}.cpu", node_id);
        let cpu_point = MetricDataPoint {
            timestamp,
            value: node_info.cpu_usage,
            metric_type: MetricType::CpuUsage,
        };
        
        metrics.entry(cpu_key).or_insert_with(Vec::new).push(cpu_point);
        
        // Store memory usage
        let mem_key = format!("node:{}.memory", node_id);
        let mem_point = MetricDataPoint {
            timestamp,
            value: node_info.memory_usage,
            metric_type: MetricType::MemoryUsage,
        };
        
        metrics.entry(mem_key).or_insert_with(Vec::new).push(mem_point);
        
        // Store disk usage
        let disk_key = format!("node:{}.disk", node_id);
        let disk_point = MetricDataPoint {
            timestamp,
            value: node_info.disk_usage,
            metric_type: MetricType::DiskUsage,
        };
        
        metrics.entry(disk_key).or_insert_with(Vec::new).push(disk_point);
        
        // Limit history to 1000 points per metric
        for (_, points) in metrics.iter_mut() {
            if points.len() > 1000 {
                points.drain(0..points.len() - 1000);
            }
        }
    }
    
    pub async fn get_metrics_history(&self, entity_id: &str, metric_type: MetricType) -> Vec<MetricDataPoint> {
        let metrics = self.metrics_history.read().await;
        
        // Determine the key based on entity type (container or node) and metric type
        let key = if entity_id.starts_with("container:") {
            match metric_type {
                MetricType::CpuUsage => format!("{}.cpu", entity_id),
                MetricType::MemoryUsage => format!("{}.memory", entity_id),
                MetricType::NetworkRx => format!("{}.net_rx", entity_id),
                MetricType::NetworkTx => format!("{}.net_tx", entity_id),
                _ => return Vec::new(), // Not applicable for containers
            }
        } else {
            match metric_type {
                MetricType::CpuUsage => format!("node:{}.cpu", entity_id),
                MetricType::MemoryUsage => format!("node:{}.memory", entity_id),
                MetricType::DiskUsage => format!("node:{}.disk", entity_id),
                _ => return Vec::new(), // Not applicable for nodes
            }
        };
        
        // Return the metrics history if it exists
        metrics.get(&key).cloned().unwrap_or_default()
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct HealthSummary {
    pub total_containers: u32,
    pub healthy_containers: u32,
    pub unhealthy_containers: u32,
    pub total_nodes: u32,
    pub healthy_nodes: u32,
    pub unhealthy_nodes: u32,
    pub total_restarts: u32,
    pub last_check: i64,
    pub active_alerts: u32,
    pub critical_alerts: u32,
    pub avg_cpu_usage: f64,
    pub avg_memory_usage: f64,
    pub avg_disk_usage: f64,
    pub health_trend: HealthTrend,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub enum HealthTrend {
    Improving,
    Stable,
    Degrading,
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
            alerts: Arc::clone(&self.alerts),
            metrics_history: Arc::clone(&self.metrics_history),
        }
    }
}