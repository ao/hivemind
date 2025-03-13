use crate::storage::StorageManager;
use anyhow::Result;
use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::Mutex;
use uuid::Uuid;
use serde::{Serialize, Deserialize};

#[derive(Clone)]
pub struct ContainerManager {
    // Internal state could be wrapped in Arc<Mutex<>> for thread safety
    containers: Arc<Mutex<Vec<Container>>>,
    images: Arc<Mutex<Vec<String>>>,
    storage: Option<StorageManager>,
    container_stats: Arc<Mutex<HashMap<String, ContainerStats>>>,
}

#[derive(Clone, Serialize, Deserialize)]
pub struct Container {
    pub id: String,
    pub name: String,
    pub image: String,
    pub status: ContainerStatus,
    pub node_id: String,
    pub created_at: i64,
    pub ports: Vec<PortMapping>,
    pub env_vars: Vec<EnvVar>,
    pub service_domain: Option<String>,
}

#[derive(Clone, Serialize, Deserialize)]
pub struct PortMapping {
    pub container_port: u16,
    pub host_port: u16,
    pub protocol: String, // "tcp" or "udp"
}

#[derive(Clone, Serialize, Deserialize)]
pub struct EnvVar {
    pub key: String,
    pub value: String,
}

#[derive(Clone, Serialize, Deserialize)]
pub struct ContainerStats {
    pub cpu_usage: f64,      // Percentage
    pub memory_usage: u64,   // Bytes
    pub network_rx: u64,     // Bytes
    pub network_tx: u64,     // Bytes
    pub last_updated: i64,   // Unix timestamp
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub enum ContainerStatus {
    Running,
    Stopped,
    Failed,
    Pending,
    Restarting,
}

impl std::fmt::Display for ContainerStatus {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.as_str())
    }
}

impl ContainerStatus {
    pub fn as_str(&self) -> &'static str {
        match self {
            ContainerStatus::Running => "running",
            ContainerStatus::Stopped => "stopped",
            ContainerStatus::Failed => "failed",
            ContainerStatus::Pending => "pending",
            ContainerStatus::Restarting => "restarting",
        }
    }

    pub fn from_str(s: &str) -> Self {
        match s {
            "running" => ContainerStatus::Running,
            "stopped" => ContainerStatus::Stopped,
            "failed" => ContainerStatus::Failed,
            "restarting" => ContainerStatus::Restarting,
            _ => ContainerStatus::Pending,
        }
    }
}

impl ContainerManager {
    pub async fn new() -> Result<Self> {
        Ok(Self {
            containers: Arc::new(Mutex::new(Vec::new())),
            images: Arc::new(Mutex::new(Vec::new())),
            storage: None,
            container_stats: Arc::new(Mutex::new(HashMap::new())),
        })
    }

    pub async fn with_storage(storage: StorageManager) -> Result<Self> {
        // Create a container manager with storage
        let manager = Self {
            containers: Arc::new(Mutex::new(Vec::new())),
            images: Arc::new(Mutex::new(Vec::new())),
            storage: Some(storage.clone()),
            container_stats: Arc::new(Mutex::new(HashMap::new())),
        };

        // Load containers from storage
        if let Some(storage) = &manager.storage {
            let container_data = storage.get_containers().await?;
            let mut containers = manager.containers.lock().await;

            for (id, name, image, status, node_id) in container_data {
                containers.push(Container {
                    id,
                    name,
                    image,
                    status: ContainerStatus::from_str(&status),
                    node_id,
                    created_at: std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .unwrap()
                        .as_secs() as i64,
                    ports: Vec::new(),
                    env_vars: Vec::new(),
                    service_domain: None,
                });
            }
        }

        // Load some default images
        {
            let mut images = manager.images.lock().await;
            images.push("nginx:latest".to_string());
            images.push("redis:latest".to_string());
            images.push("postgres:latest".to_string());
            images.push("ubuntu:latest".to_string());
        } // Release the lock before returning manager

        Ok(manager)
    }

    pub async fn list_containers(&self) -> Result<Vec<String>> {
        // If we have storage, use it to get the latest container list
        if let Some(storage) = &self.storage {
            let container_data = storage.get_containers().await?;
            return Ok(container_data
                .into_iter()
                .map(|(id, _, _, _, _)| id)
                .collect());
        }

        // Otherwise, use in-memory data
        let containers = self.containers.lock().await;
        Ok(containers.iter().map(|c| c.id.clone()).collect())
    }

    pub async fn get_container_details(&self) -> Result<Vec<Container>> {
        let containers = self.containers.lock().await;
        Ok(containers.clone())
    }

    pub async fn list_images(&self) -> Result<Vec<String>> {
        let images = self.images.lock().await;
        Ok(images.clone())
    }

    pub async fn monitor_containers(&self) -> Result<()> {
        // Implementation for container monitoring
        println!("Monitoring containers...");

        // Check all containers
        let containers = self.containers.lock().await.clone();
        for container in containers {
            // Simulate checking container status
            println!("Checking container {} ({})", container.name, container.id);

            // In a real implementation, this would check the container's actual status
            // using containerd or docker API
            
            // For now, simulate status changes
            self.update_container_stats(&container.id).await?;
            
            // Check if container needs to be restarted
            if container.status == ContainerStatus::Failed {
                println!("Container {} failed, attempting to restart", container.id);
                self.restart_container(&container.id).await?;
            }
        }

        Ok(())
    }

    async fn update_container_stats(&self, container_id: &str) -> Result<()> {
        // In a real implementation, this would get actual container stats
        // For now, generate simulated stats
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64;
            
        let cpu_usage = 10.0 + (now % 20) as f64;
        let memory_usage = 50 * 1024 * 1024 + ((now % 50) * 1024 * 1024) as u64;
        let network_rx = 1024 + (now % 10240) as u64;
        let network_tx = 512 + (now % 5120) as u64;
        
        let stats = ContainerStats {
            cpu_usage,
            memory_usage,
            network_rx,
            network_tx,
            last_updated: now,
        };
        
        let mut container_stats = self.container_stats.lock().await;
        container_stats.insert(container_id.to_string(), stats);
        
        Ok(())
    }

    // Add methods for container lifecycle management
    pub async fn deploy_container(
        &self, 
        image: &str, 
        name: &str,
        node_id: Option<&str>,
        service_domain: Option<&str>,
        env_vars: Option<Vec<(&str, &str)>>,
        ports: Option<Vec<(u16, u16)>>
    ) -> Result<String> {
        println!("Deploying container {} with image {}", name, image);
        let container_id = format!("container-{}", Uuid::new_v4());
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64;

        // Convert environment variables
        let env_vars = env_vars.unwrap_or_default()
            .into_iter()
            .map(|(k, v)| EnvVar {
                key: k.to_string(),
                value: v.to_string(),
            })
            .collect();
            
        // Convert port mappings
        let ports = ports.unwrap_or_default()
            .into_iter()
            .map(|(container_port, host_port)| PortMapping {
                container_port,
                host_port,
                protocol: "tcp".to_string(),
            })
            .collect();

        let container = Container {
            id: container_id.clone(),
            name: name.to_string(),
            image: image.to_string(),
            status: ContainerStatus::Pending,
            node_id: node_id.unwrap_or("local").to_string(),
            created_at: now,
            ports,
            env_vars,
            service_domain: service_domain.map(|s| s.to_string()),
        };

        // Save to storage if available
        if let Some(storage) = &self.storage {
            storage
                .save_container(
                    &container_id,
                    name,
                    image,
                    container.status.as_str(),
                    &container.node_id,
                )
                .await?;
        }

        // Update in-memory state
        self.containers.lock().await.push(container);

        // Actually start the container (mock implementation)
        println!("Container {} is starting...", container_id);
        
        // Simulate container starting
        tokio::spawn({
            let container_id = container_id.clone();
            let containers = self.containers.clone();
            
            async move {
                // Simulate container startup time
                tokio::time::sleep(tokio::time::Duration::from_secs(2)).await;
                
                // Update container status to running
                let mut containers_lock = containers.lock().await;
                if let Some(container) = containers_lock.iter_mut().find(|c| c.id == container_id) {
                    container.status = ContainerStatus::Running;
                    println!("Container {} is now running", container_id);
                }
            }
        });

        Ok(container_id)
    }

    pub async fn stop_container(&self, container_id: &str) -> Result<bool> {
        let mut containers = self.containers.lock().await;

        if let Some(idx) = containers.iter().position(|c| c.id == container_id) {
            // Mark container as stopped
            containers[idx].status = ContainerStatus::Stopped;

            // Update in storage if available
            if let Some(storage) = &self.storage {
                storage
                    .save_container(
                        &containers[idx].id,
                        &containers[idx].name,
                        &containers[idx].image,
                        containers[idx].status.as_str(),
                        &containers[idx].node_id,
                    )
                    .await?;
            }

            println!("Container {} stopped", container_id);
            Ok(true)
        } else {
            println!("Container {} not found", container_id);
            Ok(false)
        }
    }
    
    pub async fn restart_container(&self, container_id: &str) -> Result<bool> {
        let mut containers = self.containers.lock().await;

        if let Some(idx) = containers.iter().position(|c| c.id == container_id) {
            // Mark container as restarting
            containers[idx].status = ContainerStatus::Restarting;
            
            // Clone container for async task
            let container = containers[idx].clone();

            // Update in storage if available
            if let Some(storage) = &self.storage {
                storage
                    .save_container(
                        &containers[idx].id,
                        &containers[idx].name,
                        &containers[idx].image,
                        containers[idx].status.as_str(),
                        &containers[idx].node_id,
                    )
                    .await?;
            }

            println!("Container {} restarting", container_id);
            
            // Simulate container restart
            let containers_clone = self.containers.clone();
            let container_id_clone = container_id.to_string(); // Clone the container_id
            tokio::spawn(async move {
                // Simulate restart time
                tokio::time::sleep(tokio::time::Duration::from_secs(3)).await;
                
                // Update container status to running
                let mut containers_lock = containers_clone.lock().await;
                if let Some(container) = containers_lock.iter_mut().find(|c| c.id == container_id_clone) {
                    container.status = ContainerStatus::Running;
                    println!("Container {} restarted successfully", container_id_clone);
                }
            });
            
            Ok(true)
        } else {
            println!("Container {} not found", container_id);
            Ok(false)
        }
    }
    
    pub async fn get_container_by_id(&self, container_id: &str) -> Option<Container> {
        let containers = self.containers.lock().await;
        containers.iter().find(|c| c.id == container_id).cloned()
    }
    
    pub async fn get_containers_by_node(&self, node_id: &str) -> Vec<Container> {
        let containers = self.containers.lock().await;
        containers.iter()
            .filter(|c| c.node_id == node_id)
            .cloned()
            .collect()
    }
    
    pub async fn get_container_stats(&self, container_id: &str) -> Option<ContainerStats> {
        let stats = self.container_stats.lock().await;
        stats.get(container_id).cloned()
    }
}
