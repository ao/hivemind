use crate::container_manager::ContainerManager;
use crate::service_discovery::ServiceDiscovery;
use anyhow::Result;
use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::Mutex;
use serde::{Serialize, Deserialize};

#[derive(Clone)]
pub struct AppManager {
    container_manager: ContainerManager,
    services: Arc<Mutex<HashMap<String, ServiceConfig>>>,
    service_discovery: Option<ServiceDiscovery>,
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
    pub fn new(container_manager: ContainerManager) -> Self {
        Self {
            container_manager,
            services: Arc::new(Mutex::new(HashMap::new())),
            service_discovery: None,
        }
    }

    pub fn with_service_discovery(mut self, service_discovery: ServiceDiscovery) -> Self {
        self.service_discovery = Some(service_discovery);
        self
    }

    pub async fn deploy_app(
        &self,
        image: &str,
        name: &str,
        service_domain: Option<&str>,
    ) -> Result<String> {
        println!("Deploying app {} with image {}", name, image);

        // Set up environment variables and port mappings
        let env_vars = vec![("APP_NAME", name)];
        let ports = vec![(80, 8080)];

        // Deploy the container
        let container_id = self.container_manager.deploy_container(
            image, 
            name,
            None, // Let scheduler decide the node
            service_domain,
            Some(env_vars),
            Some(ports)
        ).await?;

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
                if let Some(container) = self.container_manager.get_container_by_id(&container_id).await {
                    // Use the container's node IP instead of hardcoded value
                    // In a real implementation, we would get the actual node IP from the node manager
                    let ip_address = match container.node_id.as_str() {
                        "local" => "127.0.0.1",
                        _ => &container.node_id, // Use node_id as IP for now
                    };
                    
                    // Find the mapped port (assuming the first port mapping is the service port)
                    if let Some(port_mapping) = container.ports.first() {
                        service_discovery.register_service(
                            &service_config,
                            &container.node_id,
                            ip_address,
                            port_mapping.host_port,
                        ).await?;
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
                    if let Some(container) = self.container_manager.get_container_by_id(container_id).await {
                        // Deploy new containers
                        for i in 0..to_add {
                            let replica_name = format!("{}-{}", name, service.container_ids.len() + i as usize);
                            let new_container_id = self.container_manager.deploy_container(
                                &container.image,
                                &replica_name,
                                None, // Let scheduler decide
                                Some(&service.domain),
                                None, // Use default env vars
                                None, // Use default ports
                            ).await?;
                            
                            // Add to service container list
                            service.container_ids.push(new_container_id.clone());
                            
                            // Register with service discovery if available
                            if let Some(service_discovery) = &self.service_discovery {
                                if let Some(new_container) = self.container_manager.get_container_by_id(&new_container_id).await {
                                    // Use the container's node IP instead of hardcoded value
                                    let ip_address = match new_container.node_id.as_str() {
                                        "local" => "127.0.0.1",
                                        _ => &new_container.node_id, // Use node_id as IP for now
                                    };
                                    
                                    if let Some(port_mapping) = new_container.ports.first() {
                                        service_discovery.register_service(
                                            service,
                                            &new_container.node_id,
                                            ip_address,
                                            port_mapping.host_port,
                                        ).await?;
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
                        let container_opt = self.container_manager.get_container_by_id(&container_id).await;
                        
                        // Stop the container
                        self.container_manager.stop_container(&container_id).await?;
                        
                        // Deregister from service discovery if available
                        if let Some(service_discovery) = &self.service_discovery {
                            if let Some(container) = container_opt {
                                let ip_address = match container.node_id.as_str() {
                                    "local" => "127.0.0.1",
                                    _ => &container.node_id, // Use node_id as IP for now
                                };
                                
                                if let Some(port_mapping) = container.ports.first() {
                                    service_discovery.deregister_service(
                                        &service.name,
                                        &container.node_id,
                                        ip_address,
                                        port_mapping.host_port,
                                    ).await?;
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
                self.container_manager.restart_container(container_id).await?;
                
                // Re-register with service discovery after restart
                if let Some(service_discovery) = &self.service_discovery {
                    // Wait a moment for the container to be fully restarted
                    tokio::time::sleep(tokio::time::Duration::from_secs(3)).await;
                    
                    // Get updated container details
                    if let Some(container) = self.container_manager.get_container_by_id(container_id).await {
                        // Use the container's node IP instead of hardcoded value
                        let ip_address = match container.node_id.as_str() {
                            "local" => "127.0.0.1",
                            _ => &container.node_id, // Use node_id as IP for now
                        };
                        
                        // Find the mapped port (assuming the first port mapping is the service port)
                        if let Some(port_mapping) = container.ports.first() {
                            service_discovery.register_service(
                                &service,
                                &container.node_id,
                                ip_address,
                                port_mapping.host_port,
                            ).await?;
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
}
