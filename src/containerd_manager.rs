use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::Mutex;

#[cfg(feature = "containerd")]
use containerd_client::{self, tonic};

use async_trait::async_trait;

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
    pub volumes: Vec<Volume>,
    pub service_domain: Option<String>,
}

#[derive(Clone, Serialize, Deserialize)]
pub struct Volume {
    pub name: String,
    pub path: String,
    pub size: u64, // in bytes
    pub created_at: i64,
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
    pub cpu_usage: f64,    // Percentage
    pub memory_usage: u64, // Bytes
    pub network_rx: u64,   // Bytes
    pub network_tx: u64,   // Bytes
    pub last_updated: i64, // Unix timestamp
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

// Mock implementation when containerd is not available
#[derive(Clone)]
pub struct MockContainerManager {
    containers: Arc<Mutex<HashMap<String, Container>>>,
    container_stats: Arc<Mutex<HashMap<String, ContainerStats>>>,
    volumes: Arc<Mutex<HashMap<String, Volume>>>,
}

impl MockContainerManager {
    pub fn new() -> Self {
        Self {
            containers: Arc::new(Mutex::new(HashMap::new())),
            container_stats: Arc::new(Mutex::new(HashMap::new())),
            volumes: Arc::new(Mutex::new(HashMap::new())),
        }
    }
}

#[async_trait]
impl crate::app::ContainerRuntime for MockContainerManager {
    async fn pull_image(&self, _image: &str) -> Result<()> {
        // Mock implementation - always succeeds
        Ok(())
    }
    
    async fn create_container(
        &self,
        image: &str,
        name: &str,
        env_vars: Vec<EnvVar>,
        ports: Vec<PortMapping>,
    ) -> Result<String> {
        let container_id = uuid::Uuid::new_v4().to_string();
        let container = Container {
            id: container_id.clone(),
            name: name.to_string(),
            image: image.to_string(),
            status: ContainerStatus::Running,
            node_id: "mock-node".to_string(),
            created_at: chrono::Utc::now().timestamp(),
            ports,
            env_vars,
            volumes: Vec::new(),
            service_domain: None,
        };
        
        let mut containers = self.containers.lock().await;
        containers.insert(container_id.clone(), container);
        
        // Add mock stats
        let stats = ContainerStats {
            cpu_usage: 25.5,
            memory_usage: 512 * 1024 * 1024, // 512MB
            network_rx: 1024 * 1024, // 1MB
            network_tx: 2 * 1024 * 1024, // 2MB
            last_updated: chrono::Utc::now().timestamp(),
        };
        let mut container_stats = self.container_stats.lock().await;
        container_stats.insert(container_id.clone(), stats);
        
        Ok(container_id)
    }
    
    async fn create_container_with_volumes(
        &self,
        image: &str,
        name: &str,
        env_vars: Vec<EnvVar>,
        ports: Vec<PortMapping>,
        volumes: Vec<(String, String)>,
    ) -> Result<String> {
        let container_id = uuid::Uuid::new_v4().to_string();
        let volume_mounts = volumes.into_iter().map(|(name, path)| {
            Volume {
                name,
                path,
                size: 1024 * 1024 * 1024, // 1GB default
                created_at: chrono::Utc::now().timestamp(),
            }
        }).collect();
        
        let container = Container {
            id: container_id.clone(),
            name: name.to_string(),
            image: image.to_string(),
            status: ContainerStatus::Running,
            node_id: "mock-node".to_string(),
            created_at: chrono::Utc::now().timestamp(),
            ports,
            env_vars,
            volumes: volume_mounts,
            service_domain: None,
        };
        
        let mut containers = self.containers.lock().await;
        containers.insert(container_id.clone(), container);
        
        // Add mock stats
        let stats = ContainerStats {
            cpu_usage: 25.5,
            memory_usage: 512 * 1024 * 1024, // 512MB
            network_rx: 1024 * 1024, // 1MB
            network_tx: 2 * 1024 * 1024, // 2MB
            last_updated: chrono::Utc::now().timestamp(),
        };
        let mut container_stats = self.container_stats.lock().await;
        container_stats.insert(container_id.clone(), stats);
        
        Ok(container_id)
    }
    
    async fn stop_container(&self, container_id: &str) -> Result<()> {
        let mut containers = self.containers.lock().await;
        if let Some(container) = containers.get_mut(container_id) {
            container.status = ContainerStatus::Stopped;
        }
        Ok(())
    }
    
    async fn list_containers(&self) -> Result<Vec<Container>> {
        let containers = self.containers.lock().await;
        Ok(containers.values().cloned().collect())
    }
    
    async fn get_container_metrics(&self, container_id: &str) -> Result<ContainerStats> {
        let container_stats = self.container_stats.lock().await;
        container_stats.get(container_id)
            .cloned()
            .ok_or_else(|| anyhow::anyhow!("Container {} not found", container_id))
    }
    
    async fn create_volume(&self, name: &str) -> Result<()> {
        let volume = Volume {
            name: name.to_string(),
            path: format!("/var/lib/hivemind/volumes/{}", name),
            size: 1024 * 1024 * 1024, // 1GB default
            created_at: chrono::Utc::now().timestamp(),
        };
        
        let mut volumes = self.volumes.lock().await;
        volumes.insert(name.to_string(), volume);
        Ok(())
    }
    
    async fn delete_volume(&self, name: &str) -> Result<()> {
        let mut volumes = self.volumes.lock().await;
        volumes.remove(name)
            .ok_or_else(|| anyhow::anyhow!("Volume {} not found", name))?;
        Ok(())
    }
    
    async fn list_volumes(&self) -> Result<Vec<Volume>> {
        let volumes = self.volumes.lock().await;
        Ok(volumes.values().cloned().collect())
    }
}

// Real containerd implementation when feature is enabled
#[cfg(feature = "containerd")]
#[derive(Clone)]
pub struct ContainerdManager {
    client: tonic::transport::Channel,
    namespace: String,
    containers: Arc<Mutex<HashMap<String, Container>>>,
    container_stats: Arc<Mutex<HashMap<String, ContainerStats>>>,
    volumes: Arc<Mutex<HashMap<String, Volume>>>,
}

#[cfg(feature = "containerd")]
#[async_trait]
impl crate::app::ContainerRuntime for ContainerdManager {
    async fn pull_image(&self, image: &str) -> Result<()> {
        self.pull_image(image).await
    }
    
    async fn create_container(
        &self,
        image: &str,
        name: &str,
        env_vars: Vec<EnvVar>,
        ports: Vec<PortMapping>,
    ) -> Result<String> {
        self.create_container(image, name, env_vars, ports).await
    }
    
    async fn create_container_with_volumes(
        &self,
        image: &str,
        name: &str,
        env_vars: Vec<EnvVar>,
        ports: Vec<PortMapping>,
        volumes: Vec<(String, String)>,
    ) -> Result<String> {
        self.create_container_with_volumes(image, name, env_vars, ports, volumes).await
    }
    
    async fn stop_container(&self, container_id: &str) -> Result<()> {
        self.stop_container(container_id).await
    }
    
    async fn list_containers(&self) -> Result<Vec<Container>> {
        self.list_containers().await
    }
    
    async fn get_container_metrics(&self, container_id: &str) -> Result<ContainerStats> {
        self.get_container_metrics(container_id).await
    }
    
    async fn create_volume(&self, name: &str) -> Result<()> {
        self.create_volume(name).await
    }
    
    async fn delete_volume(&self, name: &str) -> Result<()> {
        self.delete_volume(name).await
    }
    
    async fn list_volumes(&self) -> Result<Vec<Volume>> {
        self.list_volumes().await
    }
}

#[cfg(feature = "containerd")]
impl ContainerdManager {
    pub async fn new(socket_path: String, namespace: String) -> anyhow::Result<Self> {
        // Connect to containerd
        let client = containerd_client::connect(socket_path)
            .await
            .context("Failed to connect to containerd")?;
        
        // Create manager instance
        let manager = Self {
            client,
            namespace,
            containers: Arc::new(Mutex::new(HashMap::new())),
            container_stats: Arc::new(Mutex::new(HashMap::new())),
            volumes: Arc::new(Mutex::new(HashMap::new())),
        };

        // Verify namespace exists
        manager.verify_namespace().await?;

        Ok(manager)
    }

    async fn verify_namespace(&self) -> anyhow::Result<()> {
        // For now, just assume the namespace exists since we can't access the namespace API
        // In a production environment, this would verify the namespace exists
        Ok(())
    }

    // Create a volume
    pub async fn create_volume(&self, name: &str) -> Result<()> {
        println!("Creating volume: {}", name);

        // Create volume directory
        let volume_path = format!("/var/lib/hivemind/volumes/{}", name);
        tokio::fs::create_dir_all(&volume_path).await?;

        // Add to in-memory state
        let mut volumes = self.volumes.lock().await;
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64;

        volumes.insert(
            name.to_string(),
            Volume {
                name: name.to_string(),
                path: volume_path,
                size: 0, // Will be calculated later
                created_at: now,
            },
        );

        Ok(())
    }

    // Delete a volume
    pub async fn delete_volume(&self, name: &str) -> Result<()> {
        println!("Deleting volume: {}", name);

        // Remove from in-memory state
        let mut volumes = self.volumes.lock().await;
        if let Some(volume) = volumes.remove(name) {
            // Remove volume directory
            if let Err(e) = tokio::fs::remove_dir_all(&volume.path).await {
                eprintln!("Failed to remove volume directory {}: {}", volume.path, e);
            }
        } else {
            return Err(anyhow::anyhow!("Volume {} not found", name));
        }

        Ok(())
    }

    // List volumes
    pub async fn list_volumes(&self) -> Result<Vec<Volume>> {
        let volumes = self.volumes.lock().await;
        Ok(volumes.values().cloned().collect())
    }

    // Pull an image
    pub async fn pull_image(&self, image: &str) -> Result<()> {
        println!("Pulling image: {}", image);
        // TODO: Implement actual image pulling using containerd client
        Ok(())
    }

    // Create and start a container
    pub async fn create_container(
        &self,
        image: &str,
        name: &str,
        env_vars: Vec<EnvVar>,
        ports: Vec<PortMapping>,
    ) -> Result<String> {
        println!("Creating container: {} from image: {}", name, image);
        
        // TODO: Implement actual container creation using containerd client
        // For now, return a mock container ID
        let container_id = uuid::Uuid::new_v4().to_string();
        
        let container = Container {
            id: container_id.clone(),
            name: name.to_string(),
            image: image.to_string(),
            status: ContainerStatus::Running,
            node_id: "local".to_string(),
            created_at: chrono::Utc::now().timestamp(),
            ports,
            env_vars,
            volumes: Vec::new(),
            service_domain: None,
        };
        
        let mut containers = self.containers.lock().await;
        containers.insert(container_id.clone(), container);
        
        Ok(container_id)
    }

    // Create container with volumes
    pub async fn create_container_with_volumes(
        &self,
        image: &str,
        name: &str,
        env_vars: Vec<EnvVar>,
        ports: Vec<PortMapping>,
        volumes: Vec<(String, String)>,
    ) -> Result<String> {
        println!("Creating container with volumes: {} from image: {}", name, image);
        
        // TODO: Implement actual container creation with volumes using containerd client
        let container_id = uuid::Uuid::new_v4().to_string();
        
        let volume_mounts = volumes.into_iter().map(|(name, path)| {
            Volume {
                name,
                path,
                size: 1024 * 1024 * 1024, // 1GB default
                created_at: chrono::Utc::now().timestamp(),
            }
        }).collect();
        
        let container = Container {
            id: container_id.clone(),
            name: name.to_string(),
            image: image.to_string(),
            status: ContainerStatus::Running,
            node_id: "local".to_string(),
            created_at: chrono::Utc::now().timestamp(),
            ports,
            env_vars,
            volumes: volume_mounts,
            service_domain: None,
        };
        
        let mut containers = self.containers.lock().await;
        containers.insert(container_id.clone(), container);
        
        Ok(container_id)
    }

    // Stop and remove a container
    pub async fn stop_container(&self, container_id: &str) -> Result<()> {
        println!("Stopping container: {}", container_id);
        
        // TODO: Implement actual container stopping using containerd client
        let mut containers = self.containers.lock().await;
        if let Some(container) = containers.get_mut(container_id) {
            container.status = ContainerStatus::Stopped;
        }
        
        Ok(())
    }

    // List all containers
    pub async fn list_containers(&self) -> Result<Vec<Container>> {
        let containers = self.containers.lock().await;
        Ok(containers.values().cloned().collect())
    }

    // Get container metrics
    pub async fn get_container_metrics(&self, container_id: &str) -> Result<ContainerStats> {
        // TODO: Implement actual metrics collection using containerd client
        Ok(ContainerStats {
            cpu_usage: 25.5,
            memory_usage: 512 * 1024 * 1024, // 512MB
            network_rx: 1024 * 1024, // 1MB
            network_tx: 2 * 1024 * 1024, // 2MB
            last_updated: chrono::Utc::now().timestamp(),
        })
    }
}
