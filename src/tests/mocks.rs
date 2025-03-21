#[cfg(test)]
pub mod mocks {
    use crate::app::ContainerRuntime;
    use crate::service_discovery::ServiceDiscovery;
    use crate::youki_manager::{Container, ContainerStats, ContainerStatus, EnvVar, PortMapping, Volume};
    use anyhow::Result;
    use std::collections::HashMap;
    use std::sync::Arc;
    use tokio::sync::Mutex;
    use uuid::Uuid;
    use std::time::{SystemTime, UNIX_EPOCH};

    /// MockContainerRuntime provides a mock implementation of ContainerRuntime for testing
    pub struct MockContainerRuntime {
        containers: Arc<Mutex<HashMap<String, Container>>>,
        volumes: Arc<Mutex<HashMap<String, Volume>>>,
        container_stats: Arc<Mutex<HashMap<String, ContainerStats>>>,
    }

    impl MockContainerRuntime {
        pub fn new() -> Self {
            Self {
                containers: Arc::new(Mutex::new(HashMap::new())),
                volumes: Arc::new(Mutex::new(HashMap::new())),
                container_stats: Arc::new(Mutex::new(HashMap::new())),
            }
        }
        
        /// Get current timestamp in seconds
        fn now() -> i64 {
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_secs() as i64
        }
    }

    #[async_trait::async_trait]
    impl ContainerRuntime for MockContainerRuntime {
        async fn pull_image(&self, image: &str) -> Result<()> {
            // Simulate image pull delay
            tokio::time::sleep(tokio::time::Duration::from_millis(100)).await;
            println!("[MOCK] Pulled image: {}", image);
            Ok(())
        }

        async fn create_container(
            &self,
            image: &str,
            name: &str,
            env_vars: Vec<EnvVar>,
            ports: Vec<PortMapping>,
        ) -> Result<String> {
            let container_id = format!("mock-container-{}", Uuid::new_v4());

            let container = Container {
                id: container_id.clone(),
                name: name.to_string(),
                image: image.to_string(),
                status: ContainerStatus::Running,
                node_id: "mock-node".to_string(),
                created_at: Self::now(),
                ports,
                env_vars,
                service_domain: None,
            };

            println!("[MOCK] Created container: {} ({})", name, container_id);
            
            let mut containers = self.containers.lock().await;
            containers.insert(container_id.clone(), container);
            
            // Create default stats for the container
            let mut stats = self.container_stats.lock().await;
            stats.insert(container_id.clone(), ContainerStats {
                cpu_usage: 5.0,
                memory_usage: 1024 * 1024 * 50, // 50MB
                network_rx: 1024 * 10,          // 10KB
                network_tx: 1024 * 5,           // 5KB
                last_updated: Self::now(),
            });

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
            let container_id = format!("mock-container-{}", Uuid::new_v4());

            // Ensure all volumes exist
            for (volume_name, _) in &volumes {
                let mut volumes_map = self.volumes.lock().await;
                if !volumes_map.contains_key(volume_name) {
                    volumes_map.insert(volume_name.clone(), Volume {
                        name: volume_name.clone(),
                        created_at: Self::now(),
                        size: 0,
                    });
                }
            }

            let container = Container {
                id: container_id.clone(),
                name: name.to_string(),
                image: image.to_string(),
                status: ContainerStatus::Running,
                node_id: "mock-node".to_string(),
                created_at: Self::now(),
                ports,
                env_vars,
                service_domain: None,
            };

            println!("[MOCK] Created container with volumes: {} ({})", name, container_id);
            for (vol_name, mount_path) in &volumes {
                println!("[MOCK]   Volume: {} -> {}", vol_name, mount_path);
            }
            
            let mut containers = self.containers.lock().await;
            containers.insert(container_id.clone(), container);
            
            // Create default stats for the container
            let mut stats = self.container_stats.lock().await;
            stats.insert(container_id.clone(), ContainerStats {
                cpu_usage: 5.0,
                memory_usage: 1024 * 1024 * 50, // 50MB
                network_rx: 1024 * 10,          // 10KB
                network_tx: 1024 * 5,           // 5KB
                last_updated: Self::now(),
            });

            Ok(container_id)
        }

        async fn stop_container(&self, container_id: &str) -> Result<()> {
            let mut containers = self.containers.lock().await;
            if let Some(container) = containers.get_mut(container_id) {
                container.status = ContainerStatus::Stopped;
                println!("[MOCK] Stopped container: {}", container_id);
            } else {
                println!("[MOCK] Container not found: {}", container_id);
            }
            Ok(())
        }

        async fn list_containers(&self) -> Result<Vec<Container>> {
            let containers = self.containers.lock().await;
            Ok(containers.values().cloned().collect())
        }

        async fn get_container_metrics(&self, container_id: &str) -> Result<ContainerStats> {
            let stats = self.container_stats.lock().await;
            if let Some(container_stats) = stats.get(container_id) {
                Ok(container_stats.clone())
            } else {
                // Return dummy metrics if not found
                Ok(ContainerStats {
                    cpu_usage: 5.0,
                    memory_usage: 1024 * 1024 * 50, // 50MB
                    network_rx: 1024 * 10,          // 10KB
                    network_tx: 1024 * 5,           // 5KB
                    last_updated: Self::now(),
                })
            }
        }
        
        async fn create_volume(&self, name: &str) -> Result<()> {
            let mut volumes = self.volumes.lock().await;
            volumes.insert(name.to_string(), Volume {
                name: name.to_string(),
                created_at: Self::now(),
                size: 0,
            });
            println!("[MOCK] Created volume: {}", name);
            Ok(())
        }

        async fn delete_volume(&self, name: &str) -> Result<()> {
            let mut volumes = self.volumes.lock().await;
            if volumes.remove(name).is_some() {
                println!("[MOCK] Deleted volume: {}", name);
            } else {
                println!("[MOCK] Volume not found: {}", name);
            }
            Ok(())
        }

        async fn list_volumes(&self) -> Result<Vec<Volume>> {
            let volumes = self.volumes.lock().await;
            Ok(volumes.values().cloned().collect())
        }
    }
    
    /// MockServiceDiscovery provides a mock implementation of service discovery for testing
    pub struct MockServiceDiscovery {
        services: Arc<Mutex<HashMap<String, Vec<crate::service_discovery::ServiceEndpoint>>>>,
    }

    // Implement From<Arc<MockServiceDiscovery>> for Option<ServiceDiscovery>
    impl From<Arc<MockServiceDiscovery>> for Option<ServiceDiscovery> {
        fn from(_mock: Arc<MockServiceDiscovery>) -> Self {
            // In tests, we don't need to convert to a real ServiceDiscovery
            // The AppManager will use the mock directly through the trait object
            Some(ServiceDiscovery::new())
        }
    }
    
    impl MockServiceDiscovery {
        pub fn new() -> Self {
            Self {
                services: Arc::new(Mutex::new(HashMap::new())),
            }
        }
        
        pub async fn register_service(
            &self,
            service_config: &crate::app::ServiceConfig,
            node_id: &str,
            ip_address: &str,
            port: u16,
        ) -> Result<()> {
            use crate::service_discovery::{ServiceEndpoint, ServiceHealth};
            
            let endpoint = ServiceEndpoint {
                service_name: service_config.name.clone(),
                domain: service_config.domain.clone(),
                ip_address: ip_address.to_string(),
                port,
                node_id: node_id.to_string(),
                health_status: ServiceHealth::Healthy, // Always healthy in mock
                last_health_check: Self::now(),
            };
            
            let mut services = self.services.lock().await;
            
            if let Some(endpoints) = services.get_mut(&service_config.name) {
                endpoints.push(endpoint);
            } else {
                services.insert(service_config.name.clone(), vec![endpoint]);
            }
            
            println!("[MOCK] Registered service: {} at {}:{}", service_config.name, ip_address, port);
            Ok(())
        }
        
        pub async fn deregister_service(
            &self,
            service_name: &str,
            node_id: &str,
            ip_address: &str,
            port: u16,
        ) -> Result<()> {
            let mut services = self.services.lock().await;
            
            if let Some(endpoints) = services.get_mut(service_name) {
                endpoints.retain(|e| !(e.node_id == node_id && e.ip_address == ip_address && e.port == port));
                
                if endpoints.is_empty() {
                    services.remove(service_name);
                }
                
                println!("[MOCK] Deregistered service: {} at {}:{}", service_name, ip_address, port);
            }
            
            Ok(())
        }
        
        pub async fn get_service_endpoints(&self, service_name: &str) -> Option<Vec<crate::service_discovery::ServiceEndpoint>> {
            let services = self.services.lock().await;
            services.get(service_name).cloned()
        }
        
        pub async fn get_service_url(&self, service_name: &str) -> Option<String> {
            let services = self.services.lock().await;
            
            if let Some(endpoints) = services.get(service_name) {
                if let Some(endpoint) = endpoints.first() {
                    return Some(format!("http://{}:{}", endpoint.ip_address, endpoint.port));
                }
            }
            
            None
        }
        
        pub async fn health_check_services(&self) -> Result<()> {
            // All services are healthy in mock implementation
            Ok(())
        }
        
        fn now() -> i64 {
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_secs() as i64
        }
    }
    
    /// MockStorageManager provides a mock implementation of storage for testing
    pub struct MockStorageManager {
        containers: Arc<Mutex<HashMap<String, (String, String, String, String, i64)>>>,
        volumes: Arc<Mutex<HashMap<String, i64>>>,
    }
    
    impl MockStorageManager {
        pub fn new() -> Self {
            Self {
                containers: Arc::new(Mutex::new(HashMap::new())),
                volumes: Arc::new(Mutex::new(HashMap::new())),
            }
        }
        
        pub async fn save_container(
            &self,
            id: &str,
            name: &str,
            image: &str,
            status: &str,
            node_id: &str,
        ) -> Result<()> {
            let mut containers = self.containers.lock().await;
            containers.insert(
                id.to_string(),
                (
                    name.to_string(),
                    image.to_string(),
                    status.to_string(),
                    node_id.to_string(),
                    Self::now(),
                ),
            );
            Ok(())
        }
        
        pub async fn get_containers(&self) -> Result<Vec<(String, String, String, String, i64)>> {
            let containers = self.containers.lock().await;
            Ok(containers
                .iter()
                .map(|(id, (name, image, status, node_id, created_at))| {
                    (
                        id.clone(),
                        name.clone(),
                        image.clone(),
                        status.clone(),
                        *created_at,
                    )
                })
                .collect())
        }
        
        pub async fn save_volume(&self, name: &str) -> Result<()> {
            let mut volumes = self.volumes.lock().await;
            volumes.insert(name.to_string(), Self::now());
            Ok(())
        }
        
        pub async fn get_volumes(&self) -> Result<Vec<(String, i64)>> {
            let volumes = self.volumes.lock().await;
            Ok(volumes
                .iter()
                .map(|(name, created_at)| (name.clone(), *created_at))
                .collect())
        }
        
        fn now() -> i64 {
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_secs() as i64
        }
    }
}
