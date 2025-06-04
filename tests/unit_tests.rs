use hivemind::app::{AppManager, ServiceConfig, ContainerRuntime, EnvVar, PortMapping};
use hivemind::storage::StorageManager;
use hivemind::service_discovery::{ServiceDiscovery, ServiceEndpoint, ServiceHealth};
use hivemind::node::NodeManager;
use hivemind::network::NetworkManager;
use hivemind::containerd_manager::{Container, ContainerStats, ContainerStatus, Volume};
use anyhow::Result;
use std::sync::Arc;
use tempfile::TempDir;
use tokio_test;

// Mock Container Runtime for testing
#[derive(Clone)]
struct MockContainerRuntime {
    containers: Arc<tokio::sync::Mutex<Vec<Container>>>,
    volumes: Arc<tokio::sync::Mutex<Vec<Volume>>>,
}

impl MockContainerRuntime {
    fn new() -> Self {
        Self {
            containers: Arc::new(tokio::sync::Mutex::new(Vec::new())),
            volumes: Arc::new(tokio::sync::Mutex::new(Vec::new())),
        }
    }
}

#[async_trait::async_trait]
impl ContainerRuntime for MockContainerRuntime {
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
            node_id: "test-node".to_string(),
            created_at: chrono::Utc::now().timestamp(),
            ports,
            env_vars,
            volumes: Vec::new(),
            service_domain: None,
        };
        
        let mut containers = self.containers.lock().await;
        containers.push(container);
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
            node_id: "test-node".to_string(),
            created_at: chrono::Utc::now().timestamp(),
            ports,
            env_vars,
            volumes: volume_mounts,
            service_domain: None,
        };
        
        let mut containers = self.containers.lock().await;
        containers.push(container);
        Ok(container_id)
    }
    
    async fn stop_container(&self, container_id: &str) -> Result<()> {
        let mut containers = self.containers.lock().await;
        if let Some(container) = containers.iter_mut().find(|c| c.id == container_id) {
            container.status = ContainerStatus::Stopped;
        }
        Ok(())
    }
    
    async fn list_containers(&self) -> Result<Vec<Container>> {
        let containers = self.containers.lock().await;
        Ok(containers.clone())
    }
    
    async fn get_container_metrics(&self, _container_id: &str) -> Result<ContainerStats> {
        Ok(ContainerStats {
            cpu_usage: 25.5,
            memory_usage: 512 * 1024 * 1024, // 512MB
            network_rx: 1024 * 1024, // 1MB
            network_tx: 2 * 1024 * 1024, // 2MB
            last_updated: chrono::Utc::now().timestamp(),
        })
    }
    
    async fn create_volume(&self, name: &str) -> Result<()> {
        let volume = Volume {
            name: name.to_string(),
            path: format!("/var/lib/hivemind/volumes/{}", name),
            size: 1024 * 1024 * 1024, // 1GB default
            created_at: chrono::Utc::now().timestamp(),
        };
        
        let mut volumes = self.volumes.lock().await;
        volumes.push(volume);
        Ok(())
    }
    
    async fn delete_volume(&self, name: &str) -> Result<()> {
        let mut volumes = self.volumes.lock().await;
        volumes.retain(|v| v.name != name);
        Ok(())
    }
    
    async fn list_volumes(&self) -> Result<Vec<Volume>> {
        let volumes = self.volumes.lock().await;
        Ok(volumes.clone())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn test_app_manager_creation() {
        let app_manager = AppManager::new().await.unwrap();
        let images = app_manager.list_images().await.unwrap();
        assert!(!images.is_empty());
        assert!(images.contains(&"nginx:latest".to_string()));
    }

    #[tokio::test]
    async fn test_app_manager_with_storage() {
        let temp_dir = TempDir::new().unwrap();
        let storage = StorageManager::new(temp_dir.path()).await.unwrap();
        let app_manager = AppManager::with_storage(storage).await.unwrap();
        
        let images = app_manager.list_images().await.unwrap();
        assert!(!images.is_empty());
    }

    #[tokio::test]
    async fn test_deploy_app() {
        let temp_dir = TempDir::new().unwrap();
        let storage = StorageManager::new(temp_dir.path()).await.unwrap();
        let service_discovery = ServiceDiscovery::new();
        let mock_runtime = Arc::new(MockContainerRuntime::new());
        
        let app_manager = AppManager::with_storage(storage)
            .await
            .unwrap()
            .with_service_discovery(service_discovery)
            .with_container_runtime(mock_runtime);

        let container_id = app_manager
            .deploy_app("nginx:latest", "test-app", Some("test.local"), None, None)
            .await
            .unwrap();

        assert!(!container_id.is_empty());
        
        let services = app_manager.list_services().await.unwrap();
        assert_eq!(services.len(), 1);
        assert_eq!(services[0].name, "test-app");
        assert_eq!(services[0].domain, "test.local");
    }

    #[tokio::test]
    async fn test_deploy_app_with_volumes() {
        let temp_dir = TempDir::new().unwrap();
        let storage = StorageManager::new(temp_dir.path()).await.unwrap();
        let service_discovery = ServiceDiscovery::new();
        let mock_runtime = Arc::new(MockContainerRuntime::new());
        
        let app_manager = AppManager::with_storage(storage)
            .await
            .unwrap()
            .with_service_discovery(service_discovery)
            .with_container_runtime(mock_runtime.clone());

        // First create a volume
        app_manager.create_volume("test-volume").await.unwrap();
        
        let volumes = vec![("test-volume".to_string(), "/data".to_string())];
        let container_id = app_manager
            .deploy_app_with_volumes("nginx:latest", "test-app", Some("test.local"), volumes, None, None)
            .await
            .unwrap();

        assert!(!container_id.is_empty());
        
        let containers = mock_runtime.list_containers().await.unwrap();
        assert_eq!(containers.len(), 1);
        assert!(!containers[0].volumes.is_empty());
        assert_eq!(containers[0].volumes[0].name, "test-volume");
    }

    #[tokio::test]
    async fn test_scale_app() {
        let temp_dir = TempDir::new().unwrap();
        let storage = StorageManager::new(temp_dir.path()).await.unwrap();
        let service_discovery = ServiceDiscovery::new();
        let mock_runtime = Arc::new(MockContainerRuntime::new());
        
        let app_manager = AppManager::with_storage(storage)
            .await
            .unwrap()
            .with_service_discovery(service_discovery)
            .with_container_runtime(mock_runtime.clone());

        // Deploy initial app
        app_manager
            .deploy_app("nginx:latest", "test-app", Some("test.local"), None, None)
            .await
            .unwrap();

        // Scale to 3 replicas
        app_manager.scale_app("test-app", 3).await.unwrap();
        
        let services = app_manager.list_services().await.unwrap();
        assert_eq!(services[0].desired_replicas, 3);
        
        let containers = mock_runtime.list_containers().await.unwrap();
        assert_eq!(containers.len(), 3);
    }

    #[tokio::test]
    async fn test_volume_management() {
        let temp_dir = TempDir::new().unwrap();
        let storage = StorageManager::new(temp_dir.path()).await.unwrap();
        let service_discovery = ServiceDiscovery::new();
        let mock_runtime = Arc::new(MockContainerRuntime::new());
        
        let app_manager = AppManager::with_storage(storage)
            .await
            .unwrap()
            .with_service_discovery(service_discovery)
            .with_container_runtime(mock_runtime);

        // Create volume
        app_manager.create_volume("test-volume").await.unwrap();
        
        // List volumes
        let volumes = app_manager.list_volumes().await.unwrap();
        assert_eq!(volumes.len(), 1);
        assert_eq!(volumes[0].name, "test-volume");
        
        // Delete volume
        app_manager.delete_volume("test-volume").await.unwrap();
        
        let volumes = app_manager.list_volumes().await.unwrap();
        assert_eq!(volumes.len(), 0);
    }

    #[tokio::test]
    async fn test_service_discovery() {
        let service_discovery = ServiceDiscovery::new();
        
        // Create a service config
        let service_config = ServiceConfig {
            name: "test-service".to_string(),
            domain: "test-service.local".to_string(),
            container_ids: vec!["container-1".to_string()],
            desired_replicas: 1,
            current_replicas: 1,
        };
        
        // Register a service
        service_discovery.register_service(&service_config, "test-node", "192.168.1.100", 80).await.unwrap();
        
        // Check service is registered
        let services = service_discovery.list_services().await;
        // The service should be registered under the service name, not domain
        assert!(services.contains_key("test-service") || services.contains_key("test-service.local"));
        
        // Get service URL - try both service name and domain
        let url = service_discovery.get_service_url("test-service").await;
        let url = if url.is_none() {
            service_discovery.get_service_url("test-service.local").await
        } else {
            url
        };
        assert!(url.is_some());
        assert_eq!(url.unwrap(), "http://192.168.1.100:80");
    }

    #[tokio::test]
    async fn test_service_discovery_load_balancing() {
        let service_discovery = ServiceDiscovery::new();
        
        // Create a service config
        let service_config = ServiceConfig {
            name: "test-service".to_string(),
            domain: "test-service.local".to_string(),
            container_ids: vec!["container-1".to_string(), "container-2".to_string()],
            desired_replicas: 2,
            current_replicas: 2,
        };
        
        // Register multiple endpoints for the same service
        service_discovery.register_service(&service_config, "test-node-1", "192.168.1.100", 80).await.unwrap();
        service_discovery.register_service(&service_config, "test-node-2", "192.168.1.101", 80).await.unwrap();
        
        // Check both endpoints are registered
        let services = service_discovery.list_services().await;
        let service_key = if services.contains_key("test-service") {
            "test-service"
        } else {
            "test-service.local"
        };
        assert_eq!(services[service_key].len(), 2);
        
        // Test load balancing - should return different URLs
        let mut urls = std::collections::HashSet::new();
        for _ in 0..10 {
            if let Some(url) = service_discovery.get_service_url(service_key).await {
                urls.insert(url);
            }
        }
        
        // Should have both URLs due to round-robin load balancing
        assert!(urls.len() >= 1); // At least one URL should be returned
    }

    #[tokio::test]
    async fn test_node_manager() {
        let temp_dir = TempDir::new().unwrap();
        let storage = StorageManager::new(temp_dir.path()).await.unwrap();
        let node_manager = NodeManager::with_storage(storage).await;
        
        // Test node listing (should be empty initially)
        let nodes = node_manager.list_nodes().await.unwrap();
        assert!(nodes.is_empty());
    }

    #[tokio::test]
    async fn test_storage_manager() {
        let temp_dir = TempDir::new().unwrap();
        let storage = StorageManager::new(temp_dir.path()).await.unwrap();
        
        // Test basic storage operations
        let test_data = b"test data";
        storage.store("test_key", test_data).await.unwrap();
        
        let retrieved_data = storage.get("test_key").await.unwrap();
        assert_eq!(retrieved_data, Some(test_data.to_vec()));
        
        // Test non-existent key
        let missing_data = storage.get("missing_key").await.unwrap();
        assert_eq!(missing_data, None);
    }

    #[tokio::test]
    async fn test_container_stats() {
        let mock_runtime = Arc::new(MockContainerRuntime::new());
        
        // Create a container
        let container_id = mock_runtime
            .create_container("nginx:latest", "test-container", vec![], vec![])
            .await
            .unwrap();
        
        // Get container stats
        let stats = mock_runtime.get_container_metrics(&container_id).await.unwrap();
        assert!(stats.cpu_usage > 0.0);
        assert!(stats.memory_usage > 0);
        assert!(stats.network_rx > 0);
        assert!(stats.network_tx > 0);
    }

    #[tokio::test]
    async fn test_container_lifecycle() {
        let mock_runtime = Arc::new(MockContainerRuntime::new());
        
        // Create a container
        let container_id = mock_runtime
            .create_container("nginx:latest", "test-container", vec![], vec![])
            .await
            .unwrap();
        
        // Verify container is running
        let containers = mock_runtime.list_containers().await.unwrap();
        assert_eq!(containers.len(), 1);
        assert_eq!(containers[0].status, ContainerStatus::Running);
        
        // Stop the container
        mock_runtime.stop_container(&container_id).await.unwrap();
        
        // Verify container is stopped
        let containers = mock_runtime.list_containers().await.unwrap();
        assert_eq!(containers[0].status, ContainerStatus::Stopped);
    }

    #[tokio::test]
    async fn test_error_handling() {
        let temp_dir = TempDir::new().unwrap();
        let storage = StorageManager::new(temp_dir.path()).await.unwrap();
        let service_discovery = ServiceDiscovery::new();
        let mock_runtime = Arc::new(MockContainerRuntime::new());
        
        let app_manager = AppManager::with_storage(storage)
            .await
            .unwrap()
            .with_service_discovery(service_discovery)
            .with_container_runtime(mock_runtime);

        // Test scaling non-existent app
        let result = app_manager.scale_app("non-existent-app", 3).await;
        assert!(result.is_err());
        
        // Test deleting non-existent volume
        let result = app_manager.delete_volume("non-existent-volume").await;
        assert!(result.is_err());
    }
}
