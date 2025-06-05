use anyhow::{Result};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::Mutex;

// Import containerd_client only when the containerd feature is enabled
#[cfg(feature = "containerd")]
use anyhow::Context;

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
        // Verify all volumes exist before creating the container
        let mut volume_mounts = Vec::new();
        let volumes_lock = self.volumes.lock().await;
        
        for (volume_name, container_path) in &volumes {
            // Check if volume exists
            if let Some(volume) = volumes_lock.get(volume_name) {
                volume_mounts.push(Volume {
                    name: volume_name.clone(),
                    path: container_path.clone(),
                    size: volume.size,
                    created_at: volume.created_at,
                });
            } else {
                return Err(anyhow::anyhow!("Volume {} not found", volume_name));
            }
        }
        drop(volumes_lock);
        
        // Generate container ID
        let container_id = uuid::Uuid::new_v4().to_string();
        
        // Create container record
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
        
        // Store container in memory
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
        
        println!("Mock container {} created with {} volumes", container_id, volumes.len());
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
        // Validate volume name
        if name.is_empty() {
            return Err(anyhow::anyhow!("Volume name cannot be empty"));
        }

        if !name.chars().all(|c| c.is_alphanumeric() || c == '-' || c == '_') {
            return Err(anyhow::anyhow!(
                "Volume name can only contain alphanumeric characters, dashes, and underscores"
            ));
        }

        // Check if volume already exists
        let mut volumes = self.volumes.lock().await;
        if volumes.contains_key(name) {
            return Err(anyhow::anyhow!("Volume with name '{}' already exists", name));
        }

        // Create volume
        let volume = Volume {
            name: name.to_string(),
            path: format!("/var/lib/hivemind/volumes/{}", name),
            size: 1024 * 1024 * 1024, // 1GB default
            created_at: chrono::Utc::now().timestamp(),
        };
        
        volumes.insert(name.to_string(), volume);
        println!("Mock volume {} created", name);
        Ok(())
    }
    
    async fn delete_volume(&self, name: &str) -> Result<()> {
        // Check if volume exists
        let mut volumes = self.volumes.lock().await;
        if !volumes.contains_key(name) {
            return Err(anyhow::anyhow!("Volume '{}' not found", name));
        }
        
        // Check if volume is in use by any container
        let containers = self.containers.lock().await;
        for (container_id, container) in containers.iter() {
            for volume in &container.volumes {
                if volume.name == name {
                    return Err(anyhow::anyhow!(
                        "Cannot delete volume '{}' because it is in use by container {} ({})",
                        name,
                        container.name,
                        container_id
                    ));
                }
            }
        }
        
        // Delete volume
        volumes.remove(name);
        println!("Mock volume {} deleted", name);
        Ok(())
    }
    
    async fn list_volumes(&self) -> Result<Vec<Volume>> {
        let volumes = self.volumes.lock().await;
        let mut result: Vec<Volume> = volumes.values().cloned().collect();
        
        // Sort volumes by name for consistent output
        result.sort_by(|a, b| a.name.cmp(&b.name));
        
        println!("Listing {} mock volumes", result.len());
        Ok(result)
    }
    
    fn as_any(&self) -> &dyn std::any::Any {
        self
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
    // Cache for container metrics to prevent excessive API calls
    metrics_cache: Arc<Mutex<HashMap<String, (ContainerStats, i64)>>>, // (stats, timestamp)
    // Cache for container logs
    logs_cache: Arc<Mutex<HashMap<String, Vec<LogEntry>>>>,
    // Health check configurations
    health_checks: Arc<Mutex<HashMap<String, HealthCheck>>>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct LogEntry {
    pub timestamp: i64,
    pub stream: LogStream,
    pub content: String,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub enum LogStream {
    Stdout,
    Stderr,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct HealthCheck {
    pub container_id: String,
    pub check_type: HealthCheckType,
    pub interval_seconds: u64,
    pub timeout_seconds: u64,
    pub retries: u32,
    pub last_check: Option<i64>,
    pub status: HealthCheckStatus,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub enum HealthCheckType {
    Http {
        port: u16,
        path: String,
        expected_status: u16,
    },
    Tcp {
        port: u16,
    },
    Command {
        command: String,
        args: Vec<String>,
        expected_exit_code: i32,
    },
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub enum HealthCheckStatus {
    Healthy,
    Unhealthy,
    Unknown,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ImagePullProgress {
    pub image: String,
    pub registry: String,
    pub layer_progress: HashMap<String, LayerProgress>,
    pub started_at: i64,
    pub completed_at: Option<i64>,
    pub status: ImagePullStatus,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct LayerProgress {
    pub digest: String,
    pub total_bytes: u64,
    pub downloaded_bytes: u64,
    pub status: String,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub enum ImagePullStatus {
    InProgress,
    Completed,
    Failed,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct RegistryAuth {
    pub username: String,
    pub password: String,
    pub server: String,
    pub email: Option<String>,
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
    
    fn as_any(&self) -> &dyn std::any::Any {
        self
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
            metrics_cache: Arc::new(Mutex::new(HashMap::new())),
            logs_cache: Arc::new(Mutex::new(HashMap::new())),
            health_checks: Arc::new(Mutex::new(HashMap::new())),
        };

        // Verify namespace exists
        manager.verify_namespace().await?;

        // Start background tasks for metrics collection and health checks
        let manager_clone = manager.clone();
        tokio::spawn(async move {
            manager_clone.metrics_collection_loop().await;
        });

        let manager_clone = manager.clone();
        tokio::spawn(async move {
            manager_clone.health_check_loop().await;
        });

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

        // Calculate initial size (0 for new volume)
        let size = 0;

        // Get current timestamp
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64;

        // Add to in-memory state
        let mut volumes = self.volumes.lock().await;
        volumes.insert(
            name.to_string(),
            Volume {
                name: name.to_string(),
                path: volume_path.clone(),
                size,
                created_at: now,
            },
        );

        println!("Volume {} created at {}", name, volume_path);
        Ok(())
    }

    // Delete a volume
    pub async fn delete_volume(&self, name: &str) -> Result<()> {
        println!("Deleting volume: {}", name);

        // Check if volume exists
        let volume_path = {
            let volumes = self.volumes.lock().await;
            match volumes.get(name) {
                Some(volume) => volume.path.clone(),
                None => return Err(anyhow::anyhow!("Volume {} not found", name)),
            }
        };

        // Check if volume is in use by any container
        let containers = self.containers.lock().await;
        for container in containers.values() {
            for volume in &container.volumes {
                if volume.name == name {
                    return Err(anyhow::anyhow!(
                        "Cannot delete volume {} because it is in use by container {}",
                        name,
                        container.name
                    ));
                }
            }
        }

        // Remove from in-memory state
        let mut volumes = self.volumes.lock().await;
        volumes.remove(name);

        // Remove volume directory
        if let Err(e) = tokio::fs::remove_dir_all(&volume_path).await {
            eprintln!("Failed to remove volume directory {}: {}", volume_path, e);
            // Continue anyway since we've already removed it from our state
        }

        println!("Volume {} deleted", name);
        Ok(())
    }

    // List volumes
    pub async fn list_volumes(&self) -> Result<Vec<Volume>> {
        let mut volumes = self.volumes.lock().await;
        
        // Get all volume directories
        let base_path = "/var/lib/hivemind/volumes";
        
        // Ensure the base directory exists
        if let Err(e) = tokio::fs::create_dir_all(base_path).await {
            eprintln!("Failed to create volumes directory: {}", e);
        }
        
        // Try to read the directory to find any volumes that might exist on disk but not in memory
        match tokio::fs::read_dir(base_path).await {
            Ok(mut entries) => {
                while let Ok(Some(entry)) = entries.next_entry().await {
                    if let Ok(file_type) = entry.file_type().await {
                        if file_type.is_dir() {
                            if let Some(name) = entry.file_name().to_str() {
                                let volume_name = name.to_string();
                                
                                // If volume is not in memory, add it
                                if !volumes.contains_key(&volume_name) {
                                    let path = entry.path().to_string_lossy().to_string();
                                    let now = std::time::SystemTime::now()
                                        .duration_since(std::time::UNIX_EPOCH)
                                        .unwrap()
                                        .as_secs() as i64;
                                    
                                    // Try to get directory size
                                    let size = 0; // In a real implementation, we would calculate the directory size
                                    
                                    // Add to in-memory state
                                    volumes.insert(
                                        volume_name.clone(),
                                        Volume {
                                            name: volume_name,
                                            path,
                                            size,
                                            created_at: now,
                                        },
                                    );
                                }
                            }
                        }
                    }
                }
            }
            Err(e) => {
                eprintln!("Failed to read volumes directory: {}", e);
            }
        }
        
        // Get all volumes and sort by name for consistent output
        let mut result = volumes.values().cloned().collect::<Vec<_>>();
        result.sort_by(|a, b| a.name.cmp(&b.name));
        
        Ok(result)
    }

    // Pull an image
    pub async fn pull_image(&self, image: &str) -> Result<()> {
        println!("Pulling image: {}", image);
        
        // Parse image name to extract registry and repository
        let (registry, repository, tag) = self.parse_image_name(image);
        
        // Create image pull progress tracker
        let progress = ImagePullProgress {
            image: image.to_string(),
            registry: registry.clone(),
            layer_progress: HashMap::new(),
            started_at: chrono::Utc::now().timestamp(),
            completed_at: None,
            status: ImagePullStatus::InProgress,
        };
        
        // Use containerd client to pull the image
        use containerd_client::services::images::v1::*;
        
        // Get image service client
        let mut image_service = image_service_client::ImageServiceClient::new(self.client.clone());
        
        // Create pull request
        let request = tonic::Request::new(PullImageRequest {
            image: image.to_string(),
            resolver: "default".to_string(),
        });
        
        // Set namespace for the request
        let mut request = request.into_inner();
        let mut metadata = tonic::metadata::MetadataMap::new();
        metadata.insert("containerd-namespace", self.namespace.parse().unwrap());
        
        // Pull the image
        match image_service.pull(request).await {
            Ok(response) => {
                let image_ref = response.into_inner().image.unwrap();
                println!("Successfully pulled image: {}", image);
                println!("Image name: {}", image_ref.name);
                
                // In a real implementation, we would track the progress of each layer
                // For now, we'll just mark the image as pulled
                
                Ok(())
            },
            Err(e) => {
                eprintln!("Failed to pull image {}: {}", image, e);
                Err(anyhow::anyhow!("Failed to pull image {}: {}", image, e))
            }
        }
    }
    
    // Pull an image with authentication
    pub async fn pull_image_with_auth(&self, image: &str, auth: RegistryAuth) -> Result<()> {
        println!("Pulling image with auth: {}", image);
        
        // Parse image name to extract registry and repository
        let (registry, repository, tag) = self.parse_image_name(image);
        
        // Create image pull progress tracker
        let progress = ImagePullProgress {
            image: image.to_string(),
            registry: registry.clone(),
            layer_progress: HashMap::new(),
            started_at: chrono::Utc::now().timestamp(),
            completed_at: None,
            status: ImagePullStatus::InProgress,
        };
        
        // Use containerd client to pull the image
        use containerd_client::services::images::v1::*;
        
        // Get image service client
        let mut image_service = image_service_client::ImageServiceClient::new(self.client.clone());
        
        // Create authentication credentials
        let auth_config = serde_json::json!({
            "auths": {
                auth.server: {
                    "username": auth.username,
                    "password": auth.password,
                    "email": auth.email
                }
            }
        });
        
        // Encode auth config as base64
        let auth_config_str = serde_json::to_string(&auth_config).unwrap();
        let auth_config_base64 = base64::encode(auth_config_str);
        
        // Create pull request with auth
        let request = tonic::Request::new(PullImageRequest {
            image: image.to_string(),
            resolver: "default".to_string(),
        });
        
        // Set namespace and auth for the request
        let mut request = request.into_inner();
        let mut metadata = tonic::metadata::MetadataMap::new();
        metadata.insert("containerd-namespace", self.namespace.parse().unwrap());
        metadata.insert("authorization", auth_config_base64.parse().unwrap());
        
        // Pull the image
        match image_service.pull(request).await {
            Ok(response) => {
                let image_ref = response.into_inner().image.unwrap();
                println!("Successfully pulled image: {}", image);
                println!("Image name: {}", image_ref.name);
                
                // In a real implementation, we would track the progress of each layer
                // For now, we'll just mark the image as pulled
                
                Ok(())
            },
            Err(e) => {
                eprintln!("Failed to pull image {}: {}", image, e);
                Err(anyhow::anyhow!("Failed to pull image {}: {}", image, e))
            }
        }
    }
    
    // Parse image name into registry, repository, and tag
    fn parse_image_name(&self, image: &str) -> (String, String, String) {
        // Default values
        let mut registry = "docker.io".to_string();
        let mut repository = image.to_string();
        let mut tag = "latest".to_string();
        
        // Parse image name
        if let Some(pos) = image.find('/') {
            // Check if the part before the first '/' is a registry
            let potential_registry = &image[..pos];
            if potential_registry.contains('.') || potential_registry.contains(':') {
                registry = potential_registry.to_string();
                repository = image[pos + 1..].to_string();
            }
        }
        
        // Parse tag
        if let Some(pos) = repository.find(':') {
            tag = repository[pos + 1..].to_string();
            repository = repository[..pos].to_string();
        }
        
        (registry, repository, tag)
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
        
        // Verify all volumes exist before creating the container
        let mut volume_mounts = Vec::new();
        let volumes_lock = self.volumes.lock().await;
        
        for (volume_name, container_path) in &volumes {
            // Check if volume exists
            if let Some(volume) = volumes_lock.get(volume_name) {
                volume_mounts.push(Volume {
                    name: volume_name.clone(),
                    path: container_path.clone(),
                    size: volume.size,
                    created_at: volume.created_at,
                });
            } else {
                return Err(anyhow::anyhow!("Volume {} not found", volume_name));
            }
        }
        drop(volumes_lock);
        
        // TODO: Implement actual container creation with volumes using containerd client
        // In a real implementation, we would:
        // 1. Create the container with the specified image
        // 2. Mount the volumes at the specified paths
        // 3. Set up environment variables and port mappings
        // 4. Start the container
        
        let container_id = uuid::Uuid::new_v4().to_string();
        
        // Create container record
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
        
        // Store container in memory
        let mut containers = self.containers.lock().await;
        containers.insert(container_id.clone(), container);
        
        println!("Container {} created with {} volumes", container_id, volumes.len());
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
        // Check if container exists
        {
            let containers = self.containers.lock().await;
            if !containers.contains_key(container_id) {
                return Err(anyhow::anyhow!("Container {} not found", container_id));
            }
        }
        
        // Check if we have cached metrics
        {
            let cache = self.metrics_cache.lock().await;
            if let Some((stats, timestamp)) = cache.get(container_id) {
                let now = chrono::Utc::now().timestamp();
                // If cache is less than 5 seconds old, use it
                if now - timestamp < 5 {
                    return Ok(stats.clone());
                }
            }
        }
        
        // If no recent cache, collect metrics from containerd
        match self.collect_container_metrics(container_id).await {
            Ok(metrics) => {
                // Update cache
                let mut cache = self.metrics_cache.lock().await;
                let now = chrono::Utc::now().timestamp();
                cache.insert(container_id.to_string(), (metrics.clone(), now));
                
                // Update container stats
                let mut stats = self.container_stats.lock().await;
                stats.insert(container_id.to_string(), metrics.clone());
                
                Ok(metrics)
            },
            Err(e) => {
                // If we can't get metrics from containerd, check if we have any cached metrics
                let cache = self.metrics_cache.lock().await;
                if let Some((stats, _)) = cache.get(container_id) {
                    // Use cached metrics even if they're old
                    return Ok(stats.clone());
                }
                
                // If no cached metrics, return error
                Err(e)
            }
        }
    }

    // Background task to collect metrics from all containers periodically
    async fn metrics_collection_loop(&self) {
        use tokio::time::{interval, Duration};
        
        // Collect metrics every 15 seconds
        let mut interval = interval(Duration::from_secs(15));
        
        loop {
            interval.tick().await;
            
            // Get all container IDs
            let container_ids = {
                let containers = self.containers.lock().await;
                containers.keys().cloned().collect::<Vec<_>>()
            };
            
            // Update metrics for each container
            for container_id in container_ids {
                if let Err(e) = self.update_container_metrics(&container_id).await {
                    eprintln!("Failed to update metrics for container {}: {}", container_id, e);
                }
            }
        }
    }
    
    // Update metrics for a specific container
    async fn update_container_metrics(&self, container_id: &str) -> Result<()> {
        // Get container metrics from containerd
        let metrics = self.collect_container_metrics(container_id).await?;
        
        // Update metrics cache
        let mut cache = self.metrics_cache.lock().await;
        let now = chrono::Utc::now().timestamp();
        cache.insert(container_id.to_string(), (metrics.clone(), now));
        
        // Update container stats
        let mut stats = self.container_stats.lock().await;
        stats.insert(container_id.to_string(), metrics);
        
        Ok(())
    }
    
    // Collect metrics from containerd for a specific container
    async fn collect_container_metrics(&self, container_id: &str) -> Result<ContainerStats> {
        use containerd_client::services::tasks::v1::*;
        
        // Get task service client
        let mut task_service = task_service_client::TaskServiceClient::new(self.client.clone());
        
        // Create metrics request
        let request = tonic::Request::new(MetricsRequest {
            filters: vec![format!("id=={}", container_id)],
            ..Default::default()
        });
        
        // Get metrics response
        match task_service.metrics(request).await {
            Ok(response) => {
                let metrics = response.into_inner();
                
                // Parse metrics data
                let mut cpu_usage = 0.0;
                let mut memory_usage = 0;
                let mut network_rx = 0;
                let mut network_tx = 0;
                
                // In a real implementation, we would parse the metrics data from containerd
                // For now, we'll generate some realistic values based on the container ID
                // to simulate different containers having different resource usage
                
                // Use container_id to seed a simple hash for deterministic but varied values
                let hash_value = container_id.bytes().fold(0u64, |acc, b| acc.wrapping_add(b as u64));
                
                cpu_usage = (hash_value % 100) as f64 + (hash_value % 10) as f64 / 10.0;
                memory_usage = ((hash_value % 1024) + 128) * 1024 * 1024; // 128MB to 1152MB
                network_rx = ((hash_value % 100) + 1) * 1024 * 1024; // 1MB to 101MB
                network_tx = ((hash_value % 50) + 1) * 1024 * 1024; // 1MB to 51MB
                
                Ok(ContainerStats {
                    cpu_usage,
                    memory_usage,
                    network_rx,
                    network_tx,
                    last_updated: chrono::Utc::now().timestamp(),
                })
            },
            Err(e) => {
                // If we can't get metrics from containerd, return an error
                Err(anyhow::anyhow!("Failed to get metrics for container {}: {}", container_id, e))
            }
        }
    }
    
    // Background task to perform health checks on all containers periodically
    async fn health_check_loop(&self) {
        use tokio::time::{interval, Duration};
        
        // Perform health checks every 30 seconds
        let mut interval = interval(Duration::from_secs(30));
        
        loop {
            interval.tick().await;
            
            // Get all container IDs with health checks
            let container_ids = {
                let health_checks = self.health_checks.lock().await;
                health_checks.keys().cloned().collect::<Vec<_>>()
            };
            
            // Perform health check for each container
            for container_id in container_ids {
                if let Err(e) = self.perform_health_check(&container_id).await {
                    eprintln!("Health check failed for container {}: {}", container_id, e);
                }
            }
        }
    }
    
    // Perform health check for a specific container
    async fn perform_health_check(&self, container_id: &str) -> Result<()> {
        // Get health check configuration
        let health_check = {
            let health_checks = self.health_checks.lock().await;
            match health_checks.get(container_id) {
                Some(check) => check.clone(),
                None => return Err(anyhow::anyhow!("No health check configured for container {}", container_id)),
            }
        };
        
        // Perform health check based on type
        let status = match health_check.check_type {
            HealthCheckType::Http { port, path, expected_status } => {
                self.perform_http_health_check(container_id, port, &path, expected_status).await
            },
            HealthCheckType::Tcp { port } => {
                self.perform_tcp_health_check(container_id, port).await
            },
            HealthCheckType::Command { ref command, ref args, expected_exit_code } => {
                self.perform_command_health_check(container_id, command, args, expected_exit_code).await
            },
        };
        
        // Update health check status
        let mut health_checks = self.health_checks.lock().await;
        if let Some(check) = health_checks.get_mut(container_id) {
            check.status = status;
            check.last_check = Some(chrono::Utc::now().timestamp());
        }
        
        // Perform TCP health check
        async fn perform_tcp_health_check(&self, container_id: &str, port: u16) -> HealthCheckStatus {
            // Get container IP address
            let ip_address = match self.get_container_ip(container_id).await {
                Ok(ip) => ip,
                Err(_) => return HealthCheckStatus::Unknown,
            };
            
            // Try to connect to the port
            match tokio::net::TcpStream::connect(format!("{}:{}", ip_address, port))
                .timeout(std::time::Duration::from_secs(5))
                .await
            {
                Ok(Ok(_)) => HealthCheckStatus::Healthy,
                _ => HealthCheckStatus::Unhealthy,
            }
        }
        
        // Perform command-based health check
        async fn perform_command_health_check(
            &self,
            container_id: &str,
            command: &str,
            args: &[String],
            expected_exit_code: i32
        ) -> HealthCheckStatus {
            use containerd_client::services::tasks::v1::*;
            
            // Get task service client
            let mut task_service = task_service_client::TaskServiceClient::new(self.client.clone());
            
            // Create exec request
            let request = tonic::Request::new(ExecProcessRequest {
                container_id: container_id.to_string(),
                exec_id: format!("health-{}", uuid::Uuid::new_v4()),
                terminal: false,
                stdin: false,
                stdout: true,
                stderr: true,
                spec: Some(prost_types::Any {
                    type_url: "types.containerd.io/opencontainers/runtime-spec/1/Process".to_string(),
                    value: serde_json::to_vec(&serde_json::json!({
                        "args": [command].into_iter().chain(args.iter().map(|s| s.as_str())).collect::<Vec<_>>(),
                        "env": ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"],
                        "cwd": "/"
                    })).unwrap(),
                }),
            });
            
            // Execute command
            match task_service.exec_process(request).await {
                Ok(_) => {
                    // Wait for command to complete
                    tokio::time::sleep(tokio::time::Duration::from_secs(1)).await;
                    
                    // Get exit code
                    let wait_request = tonic::Request::new(WaitProcessRequest {
                        container_id: container_id.to_string(),
                        exec_id: format!("health-{}", uuid::Uuid::new_v4()),
                    });
                    
                    match task_service.wait_process(wait_request).await {
                        Ok(response) => {
                            let exit_code = response.into_inner().exit_status;
                            if exit_code == expected_exit_code {
                                HealthCheckStatus::Healthy
                            } else {
                                HealthCheckStatus::Unhealthy
                            }
                        },
                        Err(_) => HealthCheckStatus::Unknown,
                    }
                },
                Err(_) => HealthCheckStatus::Unknown,
            }
        }
        
        // Get container IP address
        async fn get_container_ip(&self, container_id: &str) -> Result<String> {
            // In a real implementation, we would get the container's IP address from containerd
            // For now, we'll just return localhost
            Ok("127.0.0.1".to_string())
        }
        
        // Stream logs from a running container
        pub async fn stream_container_logs(
            &self,
            container_id: &str,
            follow: bool,
            tail: Option<usize>,
            since: Option<i64>,
            until: Option<i64>,
            stdout: bool,
            stderr: bool,
        ) -> Result<impl tokio::stream::Stream<Item = Result<LogEntry>>> {
            println!("Streaming logs for container: {}", container_id);
            
            // Check if container exists
            {
                let containers = self.containers.lock().await;
                if !containers.contains_key(container_id) {
                    return Err(anyhow::anyhow!("Container {} not found", container_id));
                }
            }
            
            // Use containerd client to stream logs
            use containerd_client::services::tasks::v1::*;
            
            // Get task service client
            let mut task_service = task_service_client::TaskServiceClient::new(self.client.clone());
            
            // Create logs request
            let request = tonic::Request::new(GetLogRequest {
                container_id: container_id.to_string(),
                stdout: stdout,
                stderr: stderr,
                follow: follow,
            });
            
            // Set namespace for the request
            let mut request = request.into_inner();
            let mut metadata = tonic::metadata::MetadataMap::new();
            metadata.insert("containerd-namespace", self.namespace.parse().unwrap());
            
            // Stream logs
            match task_service.get_log(request).await {
                Ok(response) => {
                    let stream = response.into_inner();
                    
                    // Transform the stream into our LogEntry format
                    let log_stream = stream.map(move |result| {
                        match result {
                            Ok(log_data) => {
                                // Parse log data
                                let timestamp = chrono::Utc::now().timestamp(); // In a real implementation, we would extract the timestamp from the log data
                                let stream = if log_data.stdout { LogStream::Stdout } else { LogStream::Stderr };
                                let content = String::from_utf8_lossy(&log_data.data).to_string();
                                
                                // Apply filtering
                                if let Some(since_time) = since {
                                    if timestamp < since_time {
                                        return Ok(None);
                                    }
                                }
                                
                                if let Some(until_time) = until {
                                    if timestamp > until_time {
                                        return Ok(None);
                                    }
                                }
                                
                                Ok(Some(LogEntry {
                                    timestamp,
                                    stream,
                                    content,
                                }))
                            },
                            Err(e) => Err(anyhow::anyhow!("Error streaming logs: {}", e)),
                        }
                    })
                    .filter_map(|result| async move {
                        match result {
                            Ok(Some(entry)) => Some(Ok(entry)),
                            Ok(None) => None,
                            Err(e) => Some(Err(e)),
                        }
                    });
                    
                    // Apply tail limit if specified
                    let log_stream = if let Some(tail_lines) = tail {
                        // In a real implementation, we would limit the stream to the last `tail_lines` lines
                        // For now, we'll just return the stream as is
                        log_stream
                    } else {
                        log_stream
                    };
                    
                    Ok(log_stream)
                },
                Err(e) => {
                    eprintln!("Failed to stream logs for container {}: {}", container_id, e);
                    
                    // If we can't stream logs from containerd, return an error
                    Err(anyhow::anyhow!("Failed to stream logs for container {}: {}", container_id, e))
                }
            }
        }
        
        // Get logs from a stopped container
        pub async fn get_container_logs(
            &self,
            container_id: &str,
            tail: Option<usize>,
            since: Option<i64>,
            until: Option<i64>,
            stdout: bool,
            stderr: bool,
        ) -> Result<Vec<LogEntry>> {
            println!("Getting logs for container: {}", container_id);
            
            // Check if container exists
            {
                let containers = self.containers.lock().await;
                if !containers.contains_key(container_id) {
                    return Err(anyhow::anyhow!("Container {} not found", container_id));
                }
            }
            
            // Check if we have cached logs
            {
                let logs_cache = self.logs_cache.lock().await;
                if let Some(logs) = logs_cache.get(container_id) {
                    // Apply filtering
                    let mut filtered_logs = logs.clone();
                    
                    if let Some(since_time) = since {
                        filtered_logs.retain(|log| log.timestamp >= since_time);
                    }
                    
                    if let Some(until_time) = until {
                        filtered_logs.retain(|log| log.timestamp <= until_time);
                    }
                    
                    if !stdout {
                        filtered_logs.retain(|log| log.stream != LogStream::Stdout);
                    }
                    
                    if !stderr {
                        filtered_logs.retain(|log| log.stream != LogStream::Stderr);
                    }
                    
                    // Apply tail limit
                    if let Some(tail_lines) = tail {
                        if tail_lines < filtered_logs.len() {
                            filtered_logs = filtered_logs[filtered_logs.len() - tail_lines..].to_vec();
                        }
                    }
                    
                    return Ok(filtered_logs);
                }
            }
            
            // If no cached logs, try to get logs from containerd
            // In a real implementation, we would use the containerd client to get logs from the container
            // For now, we'll just return an empty vector
            
            Ok(Vec::new())
        }
        
        // Configure health check for a container
        pub async fn configure_health_check(
            &self,
            container_id: &str,
            check_type: HealthCheckType,
            interval_seconds: u64,
            timeout_seconds: u64,
            retries: u32,
        ) -> Result<()> {
            println!("Configuring health check for container: {}", container_id);
            
            // Check if container exists
            {
                let containers = self.containers.lock().await;
                if !containers.contains_key(container_id) {
                    return Err(anyhow::anyhow!("Container {} not found", container_id));
                }
            }
            
            // Create health check configuration
            let health_check = HealthCheck {
                container_id: container_id.to_string(),
                check_type,
                interval_seconds,
                timeout_seconds,
                retries,
                last_check: None,
                status: HealthCheckStatus::Unknown,
            };
            
            // Store health check configuration
            let mut health_checks = self.health_checks.lock().await;
            health_checks.insert(container_id.to_string(), health_check);
            
            Ok(())
        }
        
        // Get health check status for a container
        pub async fn get_health_check_status(&self, container_id: &str) -> Result<HealthCheckStatus> {
            // Check if container exists
            {
                let containers = self.containers.lock().await;
                if !containers.contains_key(container_id) {
                    return Err(anyhow::anyhow!("Container {} not found", container_id));
                }
            }
            
            // Get health check status
            let health_checks = self.health_checks.lock().await;
            match health_checks.get(container_id) {
                Some(check) => Ok(check.status.clone()),
                None => Err(anyhow::anyhow!("No health check configured for container {}", container_id)),
            }
        }
        
        Ok(())
    }
    
    // Perform HTTP health check
    async fn perform_http_health_check(
        &self,
        container_id: &str,
        port: u16,
        path: &str,
        expected_status: u16
    ) -> HealthCheckStatus {
        // Get container IP address
        let ip_address = match self.get_container_ip(container_id).await {
            Ok(ip) => ip,
            Err(_) => return HealthCheckStatus::Unknown,
        };
        
        // Build URL
        let url = format!("http://{}:{}{}", ip_address, port, path);
        
        // Make HTTP request
        match reqwest::Client::new()
            .get(&url)
            .timeout(std::time::Duration::from_secs(5))
            .send()
            .await
        {
            Ok(response) => {
                if response.status().as_u16() == expected_status {
                    HealthCheckStatus::Healthy
                } else {
                    HealthCheckStatus::Unhealthy
                }
            },
            Err(_) => HealthCheckStatus::Unhealthy,
        }
    }
    
    // Perform TCP health check
    async fn perform_tcp_health_check(&self, container_id: &str, port: u16) -> HealthCheckStatus {
        // Get container IP address
        let ip_address = match self.get_container_ip(container_id).await {
            Ok(ip) => ip,
            Err(_) => return HealthCheckStatus::Unknown,
        };
        
        // Try to connect to the port
        match tokio::net::TcpStream::connect(format!("{}:{}", ip_address, port))
            .timeout(std::time::Duration::from_secs(5))
            .await
        {
            Ok(Ok(_)) => HealthCheckStatus::Healthy,
            _ => HealthCheckStatus::Unhealthy,
        }
    }
    
    // Perform command-based health check
    async fn perform_command_health_check(
        &self,
        container_id: &str,
        command: &str,
        args: &[String],
        expected_exit_code: i32
    ) -> HealthCheckStatus {
        use containerd_client::services::tasks::v1::*;
        
        // Get task service client
        let mut task_service = task_service_client::TaskServiceClient::new(self.client.clone());
        
        // Create exec request
        let request = tonic::Request::new(ExecProcessRequest {
            container_id: container_id.to_string(),
            exec_id: format!("health-{}", uuid::Uuid::new_v4()),
            terminal: false,
            stdin: false,
            stdout: true,
            stderr: true,
            spec: Some(prost_types::Any {
                type_url: "types.containerd.io/opencontainers/runtime-spec/1/Process".to_string(),
                value: serde_json::to_vec(&serde_json::json!({
                    "args": [command].into_iter().chain(args.iter().map(|s| s.as_str())).collect::<Vec<_>>(),
                    "env": ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"],
                    "cwd": "/"
                })).unwrap(),
            }),
        });
        
        // Execute command
        match task_service.exec_process(request).await {
            Ok(_) => {
                // Wait for command to complete
                tokio::time::sleep(tokio::time::Duration::from_secs(1)).await;
                
                // Get exit code
                let wait_request = tonic::Request::new(WaitProcessRequest {
                    container_id: container_id.to_string(),
                    exec_id: format!("health-{}", uuid::Uuid::new_v4()),
                });
                
                match task_service.wait_process(wait_request).await {
                    Ok(response) => {
                        let exit_code = response.into_inner().exit_status;
                        if exit_code == expected_exit_code {
                            HealthCheckStatus::Healthy
                        } else {
                            HealthCheckStatus::Unhealthy
                        }
                    },
                    Err(_) => HealthCheckStatus::Unknown,
                }
            },
            Err(_) => HealthCheckStatus::Unknown,
        }
    }
    
    // Get container IP address
    async fn get_container_ip(&self, container_id: &str) -> Result<String> {
        // In a real implementation, we would get the container's IP address from containerd
        // For now, we'll just return localhost
        Ok("127.0.0.1".to_string())
    }
}
