use crate::storage::StorageManager;
#[cfg(feature = "containerd")]
use crate::containerd_manager::ContainerdManager;
use crate::containerd_manager::{Container, ContainerStats, ContainerStatus};
use crate::service_discovery::ServiceDiscovery;
use anyhow::Result;
use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use tokio::sync::Mutex;
use uuid::Uuid;
use serde::{Deserialize, Serialize};

// Re-export types from containerd_manager for backward compatibility
pub use crate::containerd_manager::{EnvVar, PortMapping, Volume};

// Define the ContainerRuntime trait in the app module
#[async_trait::async_trait]
pub trait ContainerRuntime: Send + Sync + 'static {
    /// Pull an image from a registry
    async fn pull_image(&self, image: &str) -> Result<()>;
    
    /// Create and start a container
    async fn create_container(
        &self,
        image: &str,
        name: &str,
        env_vars: Vec<EnvVar>,
        ports: Vec<PortMapping>,
    ) -> Result<String>;
    
    /// Create container with volumes
    async fn create_container_with_volumes(
        &self,
        image: &str,
        name: &str,
        env_vars: Vec<EnvVar>,
        ports: Vec<PortMapping>,
        volumes: Vec<(String, String)>,
    ) -> Result<String>;
    
    /// Stop and remove a container
    async fn stop_container(&self, container_id: &str) -> Result<()>;
    
    /// List all containers
    async fn list_containers(&self) -> Result<Vec<Container>>;
    
    /// Get container metrics
    async fn get_container_metrics(&self, container_id: &str) -> Result<ContainerStats>;
    
    /// Create a volume
    async fn create_volume(&self, name: &str) -> Result<()>;
    
    /// Delete a volume
    async fn delete_volume(&self, name: &str) -> Result<()>;
    
    /// List volumes
    async fn list_volumes(&self) -> Result<Vec<Volume>>;
    
    /// Get as Any for downcasting
    fn as_any(&self) -> &dyn std::any::Any;
}

/// AppManager is responsible for managing applications and containers
#[derive(Clone)]
pub struct AppManager {
    storage: Option<StorageManager>,
    container_runtime: Option<Arc<dyn ContainerRuntime>>,
    images: Arc<Mutex<Vec<String>>>,
    services: Arc<Mutex<HashMap<String, ServiceConfig>>>,
    service_discovery: Option<ServiceDiscovery>,
    containers: Arc<Mutex<Vec<Container>>>,
    container_stats: Arc<Mutex<HashMap<String, ContainerStats>>>,
}

#[derive(Clone, Serialize, Deserialize)]
pub struct ServiceConfig {
    pub name: String,
    pub domain: String,
    pub container_ids: Vec<String>,
    pub desired_replicas: u32,
    pub current_replicas: u32,
}

impl AppManager {
    pub async fn new() -> Result<Self> {
        let manager = Self {
            images: Arc::new(Mutex::new(Vec::new())),
            services: Arc::new(Mutex::new(HashMap::new())),
            service_discovery: None,
            storage: None,
            container_runtime: None,
            containers: Arc::new(Mutex::new(Vec::new())),
            container_stats: Arc::new(Mutex::new(HashMap::new())),
        };
        
        // Initialize default images
        {
            let mut images = manager.images.lock().await;
            images.push("nginx:latest".to_string());
            images.push("redis:latest".to_string());
            images.push("postgres:latest".to_string());
            images.push("ubuntu:latest".to_string());
        }
        
        Ok(manager)
    }
    
    /// Set the container runtime implementation
    pub fn with_container_runtime<T: ContainerRuntime + Send + Sync + 'static>(mut self, runtime: Arc<T>) -> Self {
        self.container_runtime = Some(runtime as Arc<dyn ContainerRuntime>);
        self
    }

    // Create a volume - delegates to container runtime
    pub async fn create_volume(&self, name: &str) -> Result<()> {
        println!("Creating volume {}", name);

        // Validate volume name
        self.validate_volume_name(name)?;

        // Check if volume already exists
        if let Some(runtime) = &self.container_runtime {
            let existing_volumes = runtime.list_volumes().await?;
            if existing_volumes.iter().any(|v| v.name == name) {
                return Err(anyhow::anyhow!("Volume with name '{}' already exists", name));
            }
        }

        // Check if container runtime is available
        if let Some(runtime) = &self.container_runtime {
            // Create volume using container runtime
            runtime.create_volume(name).await?;
            
            // If we have storage, save the volume info
            if let Some(storage) = &self.storage {
                // Get the volume details from the runtime
                let volumes = runtime.list_volumes().await?;
                if let Some(volume) = volumes.iter().find(|v| v.name == name) {
                    storage.save_volume(&volume.name, &volume.path, volume.size, volume.created_at).await?;
                }
            }
            
            println!("Volume {} created", name);
            Ok(())
        } else {
            // Error if no container runtime
            println!("No container runtime available to create volume");
            Err(anyhow::anyhow!("No container runtime available"))
        }
    }

    // Validate volume name
    fn validate_volume_name(&self, name: &str) -> Result<()> {
        // Check if name is empty
        if name.is_empty() {
            return Err(anyhow::anyhow!("Volume name cannot be empty"));
        }

        // Check if name contains only alphanumeric characters, dashes, and underscores
        if !name.chars().all(|c| c.is_alphanumeric() || c == '-' || c == '_') {
            return Err(anyhow::anyhow!(
                "Volume name can only contain alphanumeric characters, dashes, and underscores"
            ));
        }

        // Check if name starts with a letter or number
        if !name.chars().next().unwrap().is_alphanumeric() {
            return Err(anyhow::anyhow!(
                "Volume name must start with an alphanumeric character"
            ));
        }

        // Check if name is too long
        if name.len() > 63 {
            return Err(anyhow::anyhow!("Volume name cannot exceed 63 characters"));
        }

        Ok(())
    }

    // Delete a volume - delegates to container runtime
    pub async fn delete_volume(&self, name: &str) -> Result<()> {
        println!("Deleting volume {}", name);

        // Validate volume name
        self.validate_volume_name(name)?;

        // Check if volume exists
        if let Some(runtime) = &self.container_runtime {
            let existing_volumes = runtime.list_volumes().await?;
            if !existing_volumes.iter().any(|v| v.name == name) {
                return Err(anyhow::anyhow!("Volume '{}' not found", name));
            }
        }

        // Check if volume is in use by any container
        let is_in_use = self.is_volume_in_use(name).await?;
        if is_in_use {
            return Err(anyhow::anyhow!(
                "Cannot delete volume '{}' because it is in use by one or more containers",
                name
            ));
        }

        // Check if container runtime is available
        if let Some(runtime) = &self.container_runtime {
            // Delete volume using container runtime
            runtime.delete_volume(name).await?;
            
            // If we have storage, delete the volume info
            if let Some(storage) = &self.storage {
                storage.delete_volume(name).await?;
            }
            
            println!("Volume {} deleted", name);
            Ok(())
        } else {
            // Error if no container runtime
            println!("No container runtime available to delete volume");
            Err(anyhow::anyhow!("No container runtime available"))
        }
    }

    // Check if a volume is in use by any container
    async fn is_volume_in_use(&self, volume_name: &str) -> Result<bool> {
        // If we have storage, check if volume is in use
        if let Some(storage) = &self.storage {
            return storage.is_volume_in_use(volume_name).await;
        }

        // Otherwise, check all containers in memory
        let containers = self.containers.lock().await;
        for container in containers.iter() {
            for volume in &container.volumes {
                if volume.name == volume_name {
                    return Ok(true);
                }
            }
        }

        // If we have a container runtime, check containers there too
        if let Some(runtime) = &self.container_runtime {
            let runtime_containers = runtime.list_containers().await?;
            for container in runtime_containers {
                for volume in container.volumes {
                    if volume.name == volume_name {
                        return Ok(true);
                    }
                }
            }
        }

        Ok(false)
    }

    // List volumes - delegates to container runtime
    pub async fn list_volumes(&self) -> Result<Vec<Volume>> {
        println!("Listing volumes");

        // Check if container runtime is available
        if let Some(runtime) = &self.container_runtime {
            // Get volumes from container runtime
            let mut volumes = runtime.list_volumes().await?;
            
            // If we have storage, update volume information from the database
            if let Some(storage) = &self.storage {
                // Get volumes from storage
                let db_volumes = storage.get_volumes().await?;
                
                // Update volume information with data from the database
                for (name, path, size, created_at) in db_volumes {
                    // Find the volume in the runtime list
                    if let Some(volume) = volumes.iter_mut().find(|v| v.name == name) {
                        // Update with database information
                        volume.path = path;
                        volume.size = size;
                        volume.created_at = created_at;
                    } else {
                        // Volume exists in database but not in runtime, add it
                        volumes.push(Volume {
                            name,
                            path,
                            size,
                            created_at,
                        });
                    }
                }
            }
            
            // Sort volumes by name for consistent output
            volumes.sort_by(|a, b| a.name.cmp(&b.name));
            
            Ok(volumes)
        } else {
            // Error if no container runtime
            println!("No container runtime available to list volumes");
            Err(anyhow::anyhow!("No container runtime available"))
        }
    }

    // Deploy container with volumes
    pub async fn deploy_app_with_volumes(
        &self,
        image: &str,
        name: &str,
        service_domain: Option<&str>,
        volumes: Vec<(String, String)>, // (volume_name, container_path)
        env_vars: Option<Vec<(&str, &str)>>,
        ports: Option<Vec<(u16, u16)>>,
    ) -> Result<String> {
        println!("Deploying app {} with image {} and volumes", name, image);

        // Convert environment variables
        let env_vars = env_vars.unwrap_or_else(|| vec![("APP_NAME", name)]);
        let ports = ports.unwrap_or_else(|| vec![(80, 8080)]);

        // Validate volume names and paths
        for (volume_name, container_path) in &volumes {
            // Validate volume name
            self.validate_volume_name(volume_name)?;
            
            // Validate container path
            if container_path.is_empty() {
                return Err(anyhow::anyhow!("Container path cannot be empty"));
            }
            
            // Container path must be absolute
            if !container_path.starts_with('/') {
                return Err(anyhow::anyhow!(
                    "Container path '{}' must be absolute (start with /)",
                    container_path
                ));
            }
        }
        
        // Check for duplicate mount paths
        let mut mount_paths = HashSet::new();
        for (_, container_path) in &volumes {
            if !mount_paths.insert(container_path) {
                return Err(anyhow::anyhow!(
                    "Duplicate container mount path: {}",
                    container_path
                ));
            }
        }
        
        // Ensure all volumes exist
        for (volume_name, _) in &volumes {
            // Check if volume exists
            if let Some(runtime) = &self.container_runtime {
                let runtime_volumes = runtime.list_volumes().await?;
                if !runtime_volumes.iter().any(|v| v.name == *volume_name) {
                    println!("Volume '{}' does not exist, creating it", volume_name);
                    // Create volume if it doesn't exist
                    self.create_volume(volume_name).await?;
                }
            }
        }

        // Deploy the container with volumes
        if let Some(runtime) = &self.container_runtime {
            // Pull the image first
            runtime.pull_image(image).await?;

            // Convert environment variables
            let env_vars = env_vars
                .into_iter()
                .map(|(k, v)| EnvVar {
                    key: k.to_string(),
                    value: v.to_string(),
                })
                .collect();

            // Convert port mappings
            let ports = ports
                .into_iter()
                .map(|(container_port, host_port)| PortMapping {
                    container_port,
                    host_port,
                    protocol: "tcp".to_string(),
                })
                .collect();

            // Deploy container with volumes
            let container_id = runtime
                .create_container_with_volumes(image, name, env_vars, ports, volumes)
                .await?;

            // Set up service discovery if requested
            if let Some(domain) = service_domain {
                // Register service
                let service_config = ServiceConfig {
                    name: name.to_string(),
                    domain: domain.to_string(),
                    container_ids: vec![container_id.clone()],
                    desired_replicas: 1,
                    current_replicas: 1,
                };
                
                let mut services = self.services.lock().await;
                services.insert(name.to_string(), service_config.clone());
                
                // Register with service discovery if available
                if let Some(service_discovery) = &self.service_discovery {
                    // Get container details to find IP and port
                    if let Some(container) = self.get_container_by_id(&container_id).await {
                        // Use the container's node IP instead of hardcoded value
                        let ip_address = match container.node_id.as_str() {
                            "local" => "127.0.0.1",
                            _ => &container.node_id, // Use node_id as IP for now
                        };

                        // Find the mapped port (assuming the first port mapping is the service port)
                        if let Some(port_mapping) = container.ports.first() {
                            service_discovery
                                .register_service(
                                    &service_config,
                                    &container.node_id,
                                    ip_address,
                                    port_mapping.host_port,
                                )
                                .await?;
                        }
                    }
                }
            }

            Ok(container_id)
        } else {
            // Error if no container runtime
            println!("No container runtime available to deploy app with volumes");
            Err(anyhow::anyhow!("No container runtime available"))
        }
    }

    pub fn with_service_discovery<T>(self, _service_discovery: T) -> Self {
        // Store service discovery instance if needed in the future
        self
    }

    pub async fn with_containerd(
        storage: StorageManager,
        _socket_path: &str,
        _namespace: &str,
    ) -> Result<Self> {
        #[cfg(feature = "containerd")]
        let runtime = {
            use anyhow::Context;
            use crate::containerd_manager::ContainerdManager;
            ContainerdManager::new(_socket_path.to_string(), _namespace.to_string())
                .await
                .context("Failed to initialize containerd manager")?
        };
            
        #[cfg(not(feature = "containerd"))]
        let runtime = {
            eprintln!("Containerd feature not enabled, using mock runtime");
            
            // Create a simple mock runtime
            struct MockRuntime {
                containers: Arc<Mutex<HashMap<String, Container>>>,
                volumes: Arc<Mutex<HashMap<String, Volume>>>,
            }
            
            #[async_trait::async_trait]
            impl ContainerRuntime for MockRuntime {
                async fn pull_image(&self, _image: &str) -> Result<()> {
                    Ok(())
                }
                
                async fn create_container(
                    &self,
                    image: &str,
                    name: &str,
                    env_vars: Vec<EnvVar>,
                    ports: Vec<PortMapping>,
                ) -> Result<String> {
                    let container_id = format!("mock-{}", uuid::Uuid::new_v4());
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
                
                async fn create_container_with_volumes(
                    &self,
                    image: &str,
                    name: &str,
                    env_vars: Vec<EnvVar>,
                    ports: Vec<PortMapping>,
                    volumes: Vec<(String, String)>,
                ) -> Result<String> {
                    let container_id = format!("mock-{}", uuid::Uuid::new_v4());
                    
                    // Convert volumes to Volume objects
                    let volume_objects = volumes.iter().map(|(name, path)| {
                        // Check if volume exists
                        Volume {
                            name: name.clone(),
                            path: path.clone(),
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
                        volumes: volume_objects,
                        service_domain: None,
                    };
                    
                    let mut containers = self.containers.lock().await;
                    containers.insert(container_id.clone(), container);
                    
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
                
                async fn get_container_metrics(&self, _container_id: &str) -> Result<ContainerStats> {
                    Ok(ContainerStats {
                        cpu_usage: 0.0,
                        memory_usage: 0,
                        network_rx: 0,
                        network_tx: 0,
                        last_updated: 0,
                    })
                }
                
                async fn create_volume(&self, name: &str) -> Result<()> {
                    let mut volumes = self.volumes.lock().await;
                    
                    // Check if volume already exists
                    if volumes.contains_key(name) {
                        return Err(anyhow::anyhow!("Volume {} already exists", name));
                    }
                    
                    volumes.insert(name.to_string(), Volume {
                        name: name.to_string(),
                        path: format!("/var/lib/hivemind/volumes/{}", name),
                        size: 1024 * 1024 * 1024, // 1GB default
                        created_at: chrono::Utc::now().timestamp(),
                    });
                    
                    Ok(())
                }
                
                async fn delete_volume(&self, name: &str) -> Result<()> {
                    let mut volumes = self.volumes.lock().await;
                    
                    // Check if volume exists
                    if !volumes.contains_key(name) {
                        return Err(anyhow::anyhow!("Volume {} not found", name));
                    }
                    
                    // Check if volume is in use
                    let containers = self.containers.lock().await;
                    for container in containers.values() {
                        for volume in &container.volumes {
                            if volume.name == name {
                                return Err(anyhow::anyhow!("Volume {} is in use by container {}", name, container.name));
                            }
                        }
                    }
                    
                    volumes.remove(name);
                    Ok(())
                }
                
                async fn list_volumes(&self) -> Result<Vec<Volume>> {
                    let volumes = self.volumes.lock().await;
                    Ok(volumes.values().cloned().collect())
                }
                
                fn as_any(&self) -> &dyn std::any::Any {
                    self
                }
            }
            
            MockRuntime {
                containers: Arc::new(Mutex::new(HashMap::new())),
                volumes: Arc::new(Mutex::new(HashMap::new())),
            }
        };
            
        Ok(Self {
            storage: Some(storage),
            container_runtime: Some(Arc::new(runtime) as Arc<dyn ContainerRuntime>),
            services: Arc::new(Mutex::new(HashMap::new())),
            images: Arc::new(Mutex::new(Vec::new())),
            service_discovery: None,
            containers: Arc::new(Mutex::new(Vec::new())),
            container_stats: Arc::new(Mutex::new(HashMap::new())),
        })
    }

    pub async fn with_storage(storage: crate::storage::StorageManager) -> Result<Self> {
        // Create an app manager with storage
        let mut manager = Self::new().await?;
        manager.storage = Some(storage);
        
        // Create a simple mock runtime for testing
        struct MockRuntime {
            containers: Arc<Mutex<HashMap<String, Container>>>,
            volumes: Arc<Mutex<HashMap<String, Volume>>>,
        }
        
        #[async_trait::async_trait]
        impl ContainerRuntime for MockRuntime {
            async fn pull_image(&self, _image: &str) -> Result<()> {
                Ok(())
            }
            
            async fn create_container(
                &self,
                image: &str,
                name: &str,
                env_vars: Vec<EnvVar>,
                ports: Vec<PortMapping>,
            ) -> Result<String> {
                let container_id = format!("mock-{}", uuid::Uuid::new_v4());
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
            
            async fn create_container_with_volumes(
                &self,
                image: &str,
                name: &str,
                env_vars: Vec<EnvVar>,
                ports: Vec<PortMapping>,
                volumes: Vec<(String, String)>,
            ) -> Result<String> {
                let container_id = format!("mock-{}", uuid::Uuid::new_v4());
                
                // Convert volumes to Volume objects
                let volume_objects = volumes.iter().map(|(name, path)| {
                    // Check if volume exists
                    Volume {
                        name: name.clone(),
                        path: path.clone(),
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
                    volumes: volume_objects,
                    service_domain: None,
                };
                
                let mut containers = self.containers.lock().await;
                containers.insert(container_id.clone(), container);
                
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
            
            async fn get_container_metrics(&self, _container_id: &str) -> Result<ContainerStats> {
                Ok(ContainerStats {
                    cpu_usage: 0.0,
                    memory_usage: 0,
                    network_rx: 0,
                    network_tx: 0,
                    last_updated: 0,
                })
            }
            
            async fn create_volume(&self, name: &str) -> Result<()> {
                let mut volumes = self.volumes.lock().await;
                
                // Check if volume already exists
                if volumes.contains_key(name) {
                    return Err(anyhow::anyhow!("Volume {} already exists", name));
                }
                
                volumes.insert(name.to_string(), Volume {
                    name: name.to_string(),
                    path: format!("/var/lib/hivemind/volumes/{}", name),
                    size: 1024 * 1024 * 1024, // 1GB default
                    created_at: chrono::Utc::now().timestamp(),
                });
                
                Ok(())
            }
            
            async fn delete_volume(&self, name: &str) -> Result<()> {
                let mut volumes = self.volumes.lock().await;
                
                // Check if volume exists
                if !volumes.contains_key(name) {
                    return Err(anyhow::anyhow!("Volume {} not found", name));
                }
                
                // Check if volume is in use
                let containers = self.containers.lock().await;
                for container in containers.values() {
                    for volume in &container.volumes {
                        if volume.name == name {
                            return Err(anyhow::anyhow!("Volume {} is in use by container {}", name, container.name));
                        }
                    }
                }
                
                volumes.remove(name);
                Ok(())
            }
            
            async fn list_volumes(&self) -> Result<Vec<Volume>> {
                let volumes = self.volumes.lock().await;
                Ok(volumes.values().cloned().collect())
            }
            
            fn as_any(&self) -> &dyn std::any::Any {
                self
            }
        }
        
        // Set up the mock runtime
        let runtime = MockRuntime {
            containers: Arc::new(Mutex::new(HashMap::new())),
            volumes: Arc::new(Mutex::new(HashMap::new())),
        };
        
        manager.container_runtime = Some(Arc::new(runtime) as Arc<dyn ContainerRuntime>);
        
        Ok(manager)
    }

    // Deploy a container using the container runtime
    pub async fn deploy_container(
        &self,
        image: &str,
        name: &str,
        node_id: Option<&str>,
        service_domain: Option<&str>,
        env_vars: Option<Vec<(&str, &str)>>,
        ports: Option<Vec<(u16, u16)>>,
    ) -> Result<String> {
        println!("Deploying container {} with image {}", name, image);

        // Convert environment variables
        let env_vars = env_vars
            .unwrap_or_default()
            .into_iter()
            .map(|(k, v)| EnvVar {
                key: k.to_string(),
                value: v.to_string(),
            })
            .collect::<Vec<_>>();

        // Convert port mappings
        let ports = ports
            .unwrap_or_default()
            .into_iter()
            .map(|(container_port, host_port)| PortMapping {
                container_port,
                host_port,
                protocol: "tcp".to_string(),
            })
            .collect::<Vec<_>>();

        // Generate a unique container ID (unused since runtime provides the real ID)
        let _container_id = format!("container-{}", Uuid::new_v4());

        // If we have a container runtime available, use it to deploy the container
        if let Some(runtime) = &self.container_runtime {
            // Pull the image first
            runtime.pull_image(image).await?;

            // Create and start container via runtime
            let real_container_id = runtime
                .create_container(image, name, env_vars.clone(), ports.clone())
                .await?;

            // Get current timestamp
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs() as i64;

            // Create container record
            let container = Container {
                id: real_container_id.clone(),
                name: name.to_string(),
                image: image.to_string(),
                status: ContainerStatus::Running,
                node_id: node_id.unwrap_or("local").to_string(),
                created_at: now,
                ports,
                env_vars,
                volumes: Vec::new(),
                service_domain: service_domain.map(|s| s.to_string()),
            };

            // Save to storage if available
            if let Some(storage) = &self.storage {
                storage
                    .save_container(
                        &container.id,
                        name,
                        image,
                        container.status.as_str(),
                        &container.node_id,
                    )
                    .await?;
            }

            // Update in-memory state
            self.containers.lock().await.push(container);

            return Ok(real_container_id);
        }

        // No container runtime available
        println!("No container runtime available to deploy container");
        Err(anyhow::anyhow!("No container runtime available"))
    }

    // Stop a container using the container runtime
    pub async fn stop_container(&self, container_id: &str) -> Result<bool> {
        if let Some(runtime) = &self.container_runtime {
            // Stop container via runtime
            runtime.stop_container(container_id).await?;

            // Update container status in memory
            let mut containers = self.containers.lock().await;
            if let Some(idx) = containers.iter().position(|c| c.id == container_id) {
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
            }

            return Ok(true);
        }

        // No container runtime available
        println!("No container runtime available to stop container");
        Err(anyhow::anyhow!("No container runtime available"))
    }

    // Restart a container using the container runtime
    pub async fn restart_container(&self, container_id: &str) -> Result<bool> {
        if let Some(runtime) = &self.container_runtime {
            // Get container info
            let container_opt = {
                let containers = self.containers.lock().await;
                containers.iter().find(|c| c.id == container_id).cloned()
            };

            if let Some(container) = container_opt {
                // Update container status to restarting
                {
                    let mut containers = self.containers.lock().await;
                    if let Some(idx) = containers.iter().position(|c| c.id == container_id) {
                        containers[idx].status = ContainerStatus::Restarting;

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
                    }
                }

                // Stop the container
                runtime.stop_container(container_id).await?;

                // Start a new container with the same parameters
                let env_vars: Vec<(&str, &str)> = container
                    .env_vars
                    .iter()
                    .map(|e| (e.key.as_str(), e.value.as_str()))
                    .collect();

                let ports: Vec<(u16, u16)> = container
                    .ports
                    .iter()
                    .map(|p| (p.container_port, p.host_port))
                    .collect();

                // Deploy new container
                let new_container_id = self
                    .deploy_container(
                        &container.image,
                        &container.name,
                        Some(&container.node_id),
                        container.service_domain.as_deref(),
                        Some(env_vars),
                        Some(ports),
                    )
                    .await?;

                println!(
                    "Container {} restarted as {}",
                    container_id, new_container_id
                );
                return Ok(true);
            }
        }

        // No container runtime available
        println!("No container runtime available to restart container");
        Err(anyhow::anyhow!("No container runtime available"))
    }

    // Monitor containers using the container runtime
    pub async fn monitor_containers(&self) -> Result<()> {
        println!("Monitoring containers...");

        if let Some(runtime) = &self.container_runtime {
            // Get all containers from runtime
            let runtime_containers = runtime.list_containers().await?;

            // Update container status in memory
            let mut containers = self.containers.lock().await;

            // Track container IDs that exist in runtime
            let mut runtime_ids = HashSet::new();

            for c in &runtime_containers {
                runtime_ids.insert(c.id.clone());

                // Find or add container in memory
                if let Some(idx) = containers.iter().position(|existing| existing.id == c.id) {
                    // Update status if changed
                    if containers[idx].status != c.status {
                        containers[idx].status = c.status.clone();

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
                    }
                } else {
                    // Add new container to memory
                    containers.push(c.clone());

                    // Add to storage if available
                    if let Some(storage) = &self.storage {
                        storage
                            .save_container(&c.id, &c.name, &c.image, c.status.as_str(), &c.node_id)
                            .await?;
                    }
                }

                // Update container stats
                self.update_container_stats(&c.id).await?;
            }

            // Handle containers that no longer exist in runtime
            for idx in (0..containers.len()).rev() {
                if !runtime_ids.contains(&containers[idx].id) {
                    // Container no longer exists in runtime
                    if containers[idx].status != ContainerStatus::Stopped {
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
                    }
                }
            }

            return Ok(());
        }

        // No container runtime available
        println!("No container runtime available to monitor containers");
        Err(anyhow::anyhow!("No container runtime available"))
    }

    // Update container stats using the container runtime
    async fn update_container_stats(&self, container_id: &str) -> Result<()> {
        if let Some(runtime) = &self.container_runtime {
            // Get container stats from runtime
            match runtime.get_container_metrics(container_id).await {
                Ok(stats) => {
                    // Update stats in memory
                    let mut container_stats = self.container_stats.lock().await;
                    container_stats.insert(container_id.to_string(), stats);
                    return Ok(());
                }
                Err(e) => {
                    println!(
                        "Failed to get metrics for container {}: {}",
                        container_id, e
                    );
                }
            }
        }

        Ok(())
    }

    pub async fn deploy_app(
        &self,
        image: &str,
        name: &str,
        service_domain: Option<&str>,
        env_vars: Option<Vec<(&str, &str)>>,
        ports: Option<Vec<(u16, u16)>>,
    ) -> Result<String> {
        println!("Deploying app {} with image {}", name, image);

        // Set default environment variables and port mappings if not provided
        let env_vars = env_vars.unwrap_or_else(|| vec![("APP_NAME", name)]);
        let ports = ports.unwrap_or_else(|| vec![(80, 8080)]);

        // Deploy the container
        let container_id = self
            .deploy_container(
                image,
                name,
                None, // Let scheduler decide the node
                service_domain,
                Some(env_vars),
                Some(ports),
            )
            .await?;

        // If a service domain is provided, set up ingress
        if let Some(domain) = service_domain {
            println!("Setting up ingress for {} -> {}", domain, name);

            // Register the service
            let mut services = self.services.lock().await;
            let service_config = ServiceConfig {
                name: name.to_string(),
                domain: domain.to_string(),
                container_ids: vec![container_id.clone()],
                desired_replicas: 1,
                current_replicas: 1,
            };

            services.insert(name.to_string(), service_config.clone());

            // Register with service discovery if available
            if let Some(service_discovery) = &self.service_discovery {
                // Get container details to find IP and port
                if let Some(container) = self.get_container_by_id(&container_id).await {
                    // Use the container's node IP instead of hardcoded value
                    // In a real implementation, we would get the actual node IP from the node manager
                    let ip_address = match container.node_id.as_str() {
                        "local" => "127.0.0.1",
                        _ => &container.node_id, // Use node_id as IP for now
                    };

                    // Find the mapped port (assuming the first port mapping is the service port)
                    if let Some(port_mapping) = container.ports.first() {
                        service_discovery
                            .register_service(
                                &service_config,
                                &container.node_id,
                                ip_address,
                                port_mapping.host_port,
                            )
                            .await?;
                    }
                }
            }
        }

        Ok(container_id)
    }

    pub async fn scale_app(&self, name: &str, replicas: u32) -> Result<()> {
        println!("Scaling app {} to {} replicas", name, replicas);

        // Get the service configuration
        let mut services = self.services.lock().await;
        let service = services.get_mut(name);

        if let Some(service) = service {
            // Update desired replicas
            service.desired_replicas = replicas;

            // Calculate how many replicas to add or remove
            let current = service.current_replicas;

            if replicas > current {
                // Need to add replicas
                let to_add = replicas - current;
                println!("Adding {} replicas for service {}", to_add, name);

                // Clone the first container ID to get image and other details
                if let Some(container_id) = service.container_ids.first() {
                    if let Some(container) = self.get_container_by_id(container_id).await {
                        // Deploy new containers
                        for i in 0..to_add {
                            let replica_name =
                                format!("{}-{}", name, service.container_ids.len() + i as usize);
                            let new_container_id = self
                                .deploy_container(
                                    &container.image,
                                    &replica_name,
                                    None, // Let scheduler decide
                                    Some(&service.domain),
                                    None, // Use default env vars
                                    None, // Use default ports
                                )
                                .await?;

                            // Add to service container list
                            service.container_ids.push(new_container_id.clone());

                            // Register with service discovery if available
                            if let Some(service_discovery) = &self.service_discovery {
                                if let Some(new_container) =
                                    self.get_container_by_id(&new_container_id).await
                                {
                                    // Use the container's node IP instead of hardcoded value
                                    let ip_address = match new_container.node_id.as_str() {
                                        "local" => "127.0.0.1",
                                        _ => &new_container.node_id, // Use node_id as IP for now
                                    };

                                    if let Some(port_mapping) = new_container.ports.first() {
                                        service_discovery
                                            .register_service(
                                                service,
                                                &new_container.node_id,
                                                ip_address,
                                                port_mapping.host_port,
                                            )
                                            .await?;
                                    }
                                }
                            }
                        }

                        // Update current replicas
                        service.current_replicas = replicas;
                    }
                }
            } else if replicas < current {
                // Need to remove replicas
                let to_remove = current - replicas;
                println!("Removing {} replicas for service {}", to_remove, name);

                // Remove containers from the end of the list
                for _ in 0..to_remove {
                    if let Some(container_id) = service.container_ids.pop() {
                        // Get container details before stopping
                        let container_opt = self.get_container_by_id(&container_id).await;

                        // Stop the container
                        self.stop_container(&container_id).await?;

                        // Deregister from service discovery if available
                        if let Some(service_discovery) = &self.service_discovery {
                            if let Some(container) = container_opt {
                                let ip_address = match container.node_id.as_str() {
                                    "local" => "127.0.0.1",
                                    _ => &container.node_id, // Use node_id as IP for now
                                };

                                if let Some(port_mapping) = container.ports.first() {
                                    service_discovery
                                        .deregister_service(
                                            &service.name,
                                            &container.node_id,
                                            ip_address,
                                            port_mapping.host_port,
                                        )
                                        .await?;
                                }
                            }
                        }
                    }
                }

                // Update current replicas
                service.current_replicas = replicas;
            } else {
                println!("Service {} already has {} replicas", name, replicas);
            }

            Ok(())
        } else {
            println!("Service {} not found", name);
            Err(anyhow::anyhow!("Service not found"))
        }
    }

    pub async fn list_services(&self) -> Result<Vec<ServiceConfig>> {
        let services = self.services.lock().await;
        Ok(services.values().cloned().collect())
    }

    pub async fn get_service(&self, name: &str) -> Option<ServiceConfig> {
        let services = self.services.lock().await;
        services.get(name).cloned()
    }

    pub async fn restart_app(&self, name: &str) -> Result<()> {
        println!("Restarting app {}", name);

        // Get the service configuration
        let services = self.services.lock().await;
        let service = services.get(name).cloned(); // Clone to avoid holding the lock
        drop(services); // Release the lock

        if let Some(service) = service {
            // Restart each container
            for container_id in &service.container_ids {
                println!("Restarting container {}", container_id);
                self.restart_container(container_id).await?;

                // Re-register with service discovery after restart
                if let Some(service_discovery) = &self.service_discovery {
                    // Wait a moment for the container to be fully restarted
                    tokio::time::sleep(tokio::time::Duration::from_secs(3)).await;

                    // Get updated container details
                    if let Some(container) = self.get_container_by_id(container_id).await {
                        // Use the container's node IP instead of hardcoded value
                        let ip_address = match container.node_id.as_str() {
                            "local" => "127.0.0.1",
                            _ => &container.node_id, // Use node_id as IP for now
                        };

                        // Find the mapped port (assuming the first port mapping is the service port)
                        if let Some(port_mapping) = container.ports.first() {
                            service_discovery
                                .register_service(
                                    &service,
                                    &container.node_id,
                                    ip_address,
                                    port_mapping.host_port,
                                )
                                .await?;
                        }
                    }
                }
            }

            Ok(())
        } else {
            println!("Service {} not found", name);
            Err(anyhow::anyhow!("Service not found"))
        }
    }

    pub async fn route_request(&self, domain: &str) -> Option<String> {
        // Find service by domain
        let services = self.services.lock().await;

        for service in services.values() {
            if service.domain == domain {
                // In a real implementation, this would use a load balancing algorithm
                // For now, just return the first container
                if let Some(container_id) = service.container_ids.first() {
                    return Some(container_id.clone());
                }
            }
        }

        None
    }

    // Container management methods - delegate to container runtime
    pub async fn list_containers(&self) -> Result<Vec<String>> {
        if let Some(runtime) = &self.container_runtime {
            // Get containers from container runtime
            let containers = runtime.list_containers().await?;
            Ok(containers.iter().map(|c| c.id.clone()).collect())
        } else if let Some(storage) = &self.storage {
            // Fall back to storage if available
            let container_data = storage.get_containers().await?;
            Ok(container_data
                .into_iter()
                .map(|(id, _, _, _, _)| id)
                .collect())
        } else {
            // No container runtime or storage available
            Ok(Vec::new())
        }
    }

    pub async fn get_container_details(&self) -> Result<Vec<Container>> {
        if let Some(runtime) = &self.container_runtime {
            // Get containers from container runtime
            runtime.list_containers().await
        } else {
            // No container runtime available
            Ok(Vec::new())
        }
    }

    pub async fn list_images(&self) -> Result<Vec<String>> {
        let images = self.images.lock().await;
        Ok(images.clone())
    }

    pub async fn get_container_by_id(&self, container_id: &str) -> Option<Container> {
        if let Some(runtime) = &self.container_runtime {
            // Try to find container in container runtime
            if let Ok(containers) = runtime.list_containers().await {
                return containers.iter().find(|c| c.id == container_id).cloned();
            }
        }
        None
    }

    pub async fn get_containers_by_node(&self, node_id: &str) -> Vec<Container> {
        if let Some(runtime) = &self.container_runtime {
            // Try to find containers in container runtime
            if let Ok(containers) = runtime.list_containers().await {
                return containers
                    .iter()
                    .filter(|c| c.node_id == node_id)
                    .cloned()
                    .collect();
            }
        }
        Vec::new()
    }

    pub async fn get_container_stats(&self, container_id: &str) -> Option<ContainerStats> {
        if let Some(runtime) = &self.container_runtime {
            // Try to get stats from container runtime
            if let Ok(stats) = runtime.get_container_metrics(container_id).await {
                return Some(stats);
            }
        }
        None
    }
    
    pub async fn get_container_health_status(&self, container_id: &str) -> Result<crate::containerd_manager::HealthCheckStatus> {
        if let Some(runtime) = &self.container_runtime {
            // Try to get health status from container runtime
            if let Some(containerd_manager) = runtime.as_any().downcast_ref::<crate::containerd_manager::ContainerdManager>() {
                return containerd_manager.get_health_check_status(container_id).await;
            }
        }
        Err(anyhow::anyhow!("Container runtime does not support health checks"))
    }
    
    pub async fn configure_container_health_check(
        &self,
        container_id: &str,
        check_type: crate::containerd_manager::HealthCheckType,
        interval_seconds: u64,
        timeout_seconds: u64,
        retries: u32,
    ) -> Result<()> {
        if let Some(runtime) = &self.container_runtime {
            // Try to configure health check using container runtime
            if let Some(containerd_manager) = runtime.as_any().downcast_ref::<crate::containerd_manager::ContainerdManager>() {
                return containerd_manager.configure_health_check(
                    container_id,
                    check_type,
                    interval_seconds,
                    timeout_seconds,
                    retries,
                ).await;
            }
        }
        Err(anyhow::anyhow!("Container runtime does not support health checks"))
    }
    
    pub async fn stream_container_logs(
        &self,
        container_id: &str,
        follow: bool,
        tail: Option<usize>,
        since: Option<i64>,
        until: Option<i64>,
        stdout: bool,
        stderr: bool,
    ) -> Result<impl tokio::stream::Stream<Item = Result<crate::containerd_manager::LogEntry>>> {
        if let Some(runtime) = &self.container_runtime {
            // Try to stream logs using container runtime
            if let Some(containerd_manager) = runtime.as_any().downcast_ref::<crate::containerd_manager::ContainerdManager>() {
                return containerd_manager.stream_container_logs(
                    container_id,
                    follow,
                    tail,
                    since,
                    until,
                    stdout,
                    stderr,
                ).await;
            }
        }
        Err(anyhow::anyhow!("Container runtime does not support log streaming"))
    }
    
    pub async fn get_container_logs(
        &self,
        container_id: &str,
        tail: Option<usize>,
        since: Option<i64>,
        until: Option<i64>,
        stdout: bool,
        stderr: bool,
    ) -> Result<Vec<crate::containerd_manager::LogEntry>> {
        if let Some(runtime) = &self.container_runtime {
            // Try to get logs using container runtime
            if let Some(containerd_manager) = runtime.as_any().downcast_ref::<crate::containerd_manager::ContainerdManager>() {
                return containerd_manager.get_container_logs(
                    container_id,
                    tail,
                    since,
                    until,
                    stdout,
                    stderr,
                ).await;
            }
        }
        Err(anyhow::anyhow!("Container runtime does not support log retrieval"))
    }
}
