// Remove the import for the non-existent container_manager module
// use crate::container_manager::ContainerManager;
use crate::containerd_manager::ContainerdManager;
use crate::service_discovery::ServiceDiscovery;
use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use tokio::sync::Mutex;
use uuid::Uuid;

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
pub struct Volume {
    pub name: String,
    pub created_at: i64,
    pub size: u64, // in bytes
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

#[derive(Clone)]
pub struct AppManager {
    containers: Arc<Mutex<Vec<Container>>>,
    images: Arc<Mutex<Vec<String>>>,
    services: Arc<Mutex<HashMap<String, ServiceConfig>>>,
    service_discovery: Option<ServiceDiscovery>,
    container_stats: Arc<Mutex<HashMap<String, ContainerStats>>>,
    storage: Option<crate::storage::StorageManager>,
    containerd: Option<Arc<ContainerdManager>>,
    volumes: Arc<Mutex<HashMap<String, Volume>>>,
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
        Ok(Self {
            containers: Arc::new(Mutex::new(Vec::new())),
            images: Arc::new(Mutex::new(Vec::new())),
            services: Arc::new(Mutex::new(HashMap::new())),
            service_discovery: None,
            container_stats: Arc::new(Mutex::new(HashMap::new())),
            storage: None,
            containerd: None,
            volumes: Arc::new(Mutex::new(HashMap::new())),
        })
    }

    // Create a volume
    pub async fn create_volume(&self, name: &str) -> Result<()> {
        println!("Creating volume {}", name);

        // Check if containerd manager is available
        if let Some(containerd) = &self.containerd {
            // Create volume using containerd manager
            containerd.create_volume(name).await?;

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
                    created_at: now,
                    size: 0, // Size will be updated during monitoring
                },
            );

            println!("Volume {} created", name);
            Ok(())
        } else {
            // Mock implementation
            let mut volumes = self.volumes.lock().await;
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs() as i64;

            volumes.insert(
                name.to_string(),
                Volume {
                    name: name.to_string(),
                    created_at: now,
                    size: 0,
                },
            );

            println!("Volume {} created (mock)", name);
            Ok(())
        }
    }

    // Delete a volume
    pub async fn delete_volume(&self, name: &str) -> Result<()> {
        println!("Deleting volume {}", name);

        // Check if containerd manager is available
        if let Some(containerd) = &self.containerd {
            // Delete volume using containerd manager
            containerd.delete_volume(name).await?;

            // Remove from in-memory state
            let mut volumes = self.volumes.lock().await;
            volumes.remove(name);

            println!("Volume {} deleted", name);
            Ok(())
        } else {
            // Mock implementation
            let mut volumes = self.volumes.lock().await;
            volumes.remove(name);

            println!("Volume {} deleted (mock)", name);
            Ok(())
        }
    }

    // List volumes
    pub async fn list_volumes(&self) -> Result<Vec<Volume>> {
        println!("Listing volumes");

        // Check if containerd manager is available
        if let Some(containerd) = &self.containerd {
            // Get volume names from containerd manager
            let volume_names = containerd.list_volumes().await?;

            // Build volume objects
            let mut volumes = self.volumes.lock().await;
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs() as i64;

            let mut result = Vec::new();

            for name in volume_names {
                // Get or create volume object
                let volume = volumes.entry(name.clone()).or_insert_with(|| Volume {
                    name: name.clone(),
                    created_at: now,
                    size: 0,
                });

                result.push(volume.clone());
            }

            Ok(result)
        } else {
            // Mock implementation
            let volumes = self.volumes.lock().await;
            Ok(volumes.values().cloned().collect())
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

        // Ensure all volumes exist
        for (volume_name, _) in &volumes {
            // Check if volume exists
            let volumes = self.volumes.lock().await;
            if !volumes.contains_key(volume_name) {
                // Create volume if it doesn't exist
                drop(volumes); // Release lock
                self.create_volume(volume_name).await?;
            }
        }

        // Deploy the container with volumes
        if let Some(containerd) = &self.containerd {
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
            let container_id = containerd
                .create_container_with_volumes(image, name, env_vars, ports, volumes)
                .await?;

            // Set up service discovery if requested
            if let Some(domain) = service_domain {
                // Register service
                // ... [service registration code from deploy_app]
            }

            Ok(container_id)
        } else {
            // Fall back to regular deployment
            self.deploy_app(image, name, service_domain, Some(env_vars), Some(ports))
                .await
        }
    }

    pub fn with_service_discovery(mut self, service_discovery: ServiceDiscovery) -> Self {
        self.service_discovery = Some(service_discovery);
        self
    }

    pub async fn with_containerd(
        storage: crate::storage::StorageManager,
        containerd_socket: &str,
        namespace: &str,
    ) -> Result<Self> {
        // Initialize containerd manager
        let containerd = ContainerdManager::new(containerd_socket, namespace).await?;

        // Create app manager with storage and containerd
        let manager = Self {
            containers: Arc::new(Mutex::new(Vec::new())),
            images: Arc::new(Mutex::new(Vec::new())),
            services: Arc::new(Mutex::new(HashMap::new())),
            service_discovery: None,
            container_stats: Arc::new(Mutex::new(HashMap::new())),
            storage: Some(storage.clone()),
            containerd: Some(Arc::new(containerd)),
            volumes: Arc::new(Mutex::new(HashMap::new())),
        };

        // Load existing containers from containerd
        if let Some(containerd) = &manager.containerd {
            let containerd_containers = containerd.list_containers().await?;
            let mut containers = manager.containers.lock().await;
            containers.extend(containerd_containers);
        }

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

    pub async fn with_storage(storage: crate::storage::StorageManager) -> Result<Self> {
        // Create an app manager with storage
        let manager = Self {
            containers: Arc::new(Mutex::new(Vec::new())),
            images: Arc::new(Mutex::new(Vec::new())),
            services: Arc::new(Mutex::new(HashMap::new())),
            service_discovery: None,
            container_stats: Arc::new(Mutex::new(HashMap::new())),
            storage: Some(storage.clone()),
            containerd: None,
            volumes: Arc::new(Mutex::new(HashMap::new())),
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

    // Update deploy_container to use containerd
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

        // Generate a unique container ID
        let container_id = format!("container-{}", Uuid::new_v4());

        // If we have containerd available, use it to deploy the container
        if let Some(containerd) = &self.containerd {
            // Create and start container via containerd
            let real_container_id = containerd
                .create_container(image, &container_id, env_vars.clone(), ports.clone())
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

        // Fallback to mock implementation if containerd is not available
        // ... [existing mock implementation code]

        Ok(container_id)
    }

    // Update stop_container to use containerd
    pub async fn stop_container(&self, container_id: &str) -> Result<bool> {
        if let Some(containerd) = &self.containerd {
            // Stop container via containerd
            containerd.stop_container(container_id).await?;

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

        // Fallback to mock implementation
        // ... [existing mock implementation code]

        Ok(false)
    }

    // Update restart_container to use containerd
    pub async fn restart_container(&self, container_id: &str) -> Result<bool> {
        if let Some(containerd) = &self.containerd {
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
                containerd.stop_container(container_id).await?;

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

        // Fallback to mock implementation
        // ... [existing mock implementation code]

        Ok(false)
    }

    // Update monitor_containers to use containerd
    pub async fn monitor_containers(&self) -> Result<()> {
        println!("Monitoring containers...");

        if let Some(containerd) = &self.containerd {
            // Get all containers from containerd
            let containerd_containers = containerd.list_containers().await?;

            // Update container status in memory
            let mut containers = self.containers.lock().await;

            // Track container IDs that exist in containerd
            let mut containerd_ids = HashSet::new();

            for c in &containerd_containers {
                containerd_ids.insert(c.id.clone());

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

            // Handle containers that no longer exist in containerd
            for idx in (0..containers.len()).rev() {
                if !containerd_ids.contains(&containers[idx].id) {
                    // Container no longer exists in containerd
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

        // Fallback to mock implementation
        // ... [existing mock implementation code]

        Ok(())
    }

    // Update update_container_stats to use containerd
    async fn update_container_stats(&self, container_id: &str) -> Result<()> {
        if let Some(containerd) = &self.containerd {
            // Get container stats from containerd
            match containerd.get_container_metrics(container_id).await {
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
                    // Continue with fallback
                }
            }
        }

        // Fallback to mock implementation
        // ... [existing mock implementation code]

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

    // Container management methods (moved from ContainerManager)
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

    pub async fn get_container_by_id(&self, container_id: &str) -> Option<Container> {
        let containers = self.containers.lock().await;
        containers.iter().find(|c| c.id == container_id).cloned()
    }

    pub async fn get_containers_by_node(&self, node_id: &str) -> Vec<Container> {
        let containers = self.containers.lock().await;
        containers
            .iter()
            .filter(|c| c.node_id == node_id)
            .cloned()
            .collect()
    }

    pub async fn get_container_stats(&self, container_id: &str) -> Option<ContainerStats> {
        let stats = self.container_stats.lock().await;
        stats.get(container_id).cloned()
    }
}
