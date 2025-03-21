#[cfg(test)]
mod tests {
    use crate::app::{AppManager, ServiceConfig};
    use crate::service_discovery::ServiceDiscovery;
    use crate::storage::StorageManager;
    use crate::app::ContainerRuntime;
    use crate::tests::mocks::mocks::{MockContainerRuntime, MockServiceDiscovery, MockStorageManager};
    use crate::youki_manager::{Container, ContainerStatus, EnvVar, PortMapping, Volume};
    use anyhow::Result;
    use std::path::PathBuf;
    use std::sync::Arc;
    use tempfile::tempdir;

    // Helper function to create a test AppManager with mocks
    async fn create_test_app_manager() -> Result<(AppManager, Arc<MockContainerRuntime>, Arc<MockServiceDiscovery>)> {
        // Create mock components
        let runtime = Arc::new(MockContainerRuntime::new());
        let service_discovery = Arc::new(MockServiceDiscovery::new());
        
        // Create app manager with mock runtime
        let app_manager = AppManager::new().await?
            .with_container_runtime(runtime.clone())
            .with_service_discovery(service_discovery.clone());
        
        Ok((app_manager, runtime, service_discovery))
    }

    #[tokio::test]
    async fn test_app_creation() -> Result<()> {
        let (app, _, _) = create_test_app_manager().await?;
        
        // A new app manager should have default images but no containers
        assert!(app.list_images().await?.len() > 0);
        assert_eq!(app.list_containers().await?.len(), 0);
        
        Ok(())
    }

    #[tokio::test]
    async fn test_deploy_app() -> Result<()> {
        let (app, runtime, _) = create_test_app_manager().await?;

        // Deploy an app
        let container_id = app
            .deploy_app("nginx:latest", "test-app", None, None, None)
            .await?;

        // Verify container was created
        let containers = runtime.list_containers().await?;
        assert_eq!(containers.len(), 1);
        
        let container = &containers[0];
        assert_eq!(container.name, "test-app");
        assert_eq!(container.image, "nginx:latest");
        assert_eq!(container.status, ContainerStatus::Running);
        
        Ok(())
    }
    
    #[tokio::test]
    async fn test_deploy_app_with_service() -> Result<()> {
        let (app, runtime, service_discovery) = create_test_app_manager().await?;

        // Deploy an app with a service domain
        let domain = "test-service.local";
        let container_id = app
            .deploy_app("nginx:latest", "test-service-app", Some(domain), None, None)
            .await?;

        // Verify container was created
        let containers = runtime.list_containers().await?;
        assert_eq!(containers.len(), 1);
        
        // Verify service was registered
        let service = app.get_service("test-service-app").await;
        assert!(service.is_some());
        
        let service = service.unwrap();
        assert_eq!(service.name, "test-service-app");
        assert_eq!(service.domain, domain);
        assert_eq!(service.container_ids.len(), 1);
        assert_eq!(service.container_ids[0], container_id);
        
        // Verify service URL is available
        let url = service_discovery.get_service_url("test-service-app").await;
        assert!(url.is_some());
        
        Ok(())
    }
    
    #[tokio::test]
    async fn test_stop_container() -> Result<()> {
        let (app, runtime, _) = create_test_app_manager().await?;

        // Deploy an app
        let container_id = app
            .deploy_app("nginx:latest", "stop-test-app", None, None, None)
            .await?;

        // Verify container is running
        let containers = runtime.list_containers().await?;
        assert_eq!(containers[0].status, ContainerStatus::Running);
        
        // Stop the container
        app.stop_container(&container_id).await?;
        
        // Verify container is stopped
        let containers = runtime.list_containers().await?;
        assert_eq!(containers[0].status, ContainerStatus::Stopped);
        
        Ok(())
    }
    
    #[tokio::test]
    async fn test_restart_container() -> Result<()> {
        let (app, runtime, _) = create_test_app_manager().await?;

        // Deploy an app
        let container_id = app
            .deploy_app("nginx:latest", "restart-test-app", None, None, None)
            .await?;

        // Verify container is running
        let containers = runtime.list_containers().await?;
        assert_eq!(containers[0].status, ContainerStatus::Running);
        
        // Stop the container
        app.stop_container(&container_id).await?;
        
        // Verify container is stopped
        let containers = runtime.list_containers().await?;
        assert_eq!(containers[0].status, ContainerStatus::Stopped);
        
        // Restart the container
        app.restart_container(&container_id).await?;
        
        // Verify container is running again
        let containers = runtime.list_containers().await?;
        assert!(containers.iter().any(|c| c.status == ContainerStatus::Running));
        
        Ok(())
    }
    
    #[tokio::test]
    async fn test_scale_app() -> Result<()> {
        let (app, runtime, _) = create_test_app_manager().await?;

        // Deploy an app with a service domain
        let domain = "scale-test.local";
        let container_id = app
            .deploy_app("nginx:latest", "scale-test-app", Some(domain), None, None)
            .await?;

        // Verify initial state
        let service = app.get_service("scale-test-app").await.unwrap();
        assert_eq!(service.current_replicas, 1);
        
        // Scale up to 3 replicas
        app.scale_app("scale-test-app", 3).await?;
        
        // Verify scaled state
        let service = app.get_service("scale-test-app").await.unwrap();
        assert_eq!(service.current_replicas, 3);
        assert_eq!(service.container_ids.len(), 3);
        
        // Verify containers were created
        let containers = runtime.list_containers().await?;
        assert_eq!(containers.len(), 3);
        
        // Scale down to 1 replica
        app.scale_app("scale-test-app", 1).await?;
        
        // Verify scaled down state
        let service = app.get_service("scale-test-app").await.unwrap();
        assert_eq!(service.current_replicas, 1);
        assert_eq!(service.container_ids.len(), 1);
        
        Ok(())
    }
    
    #[tokio::test]
    async fn test_create_volume() -> Result<()> {
        let (app, runtime, _) = create_test_app_manager().await?;

        // Create a volume
        app.create_volume("test-volume").await?;

        // Verify volume was created
        let volumes = runtime.list_volumes().await?;
        assert_eq!(volumes.len(), 1);
        assert_eq!(volumes[0].name, "test-volume");
        
        Ok(())
    }
    
    #[tokio::test]
    async fn test_deploy_with_volumes() -> Result<()> {
        let (app, runtime, _) = create_test_app_manager().await?;

        // Create a volume
        app.create_volume("data-volume").await?;
        
        // Deploy app with volume
        let volumes = vec![("data-volume".to_string(), "/data".to_string())];
        let container_id = app
            .deploy_app_with_volumes(
                "postgres:latest",
                "db-app",
                None,
                volumes,
                Some(vec![("POSTGRES_PASSWORD", "secret")]),
                Some(vec![(5432, 5432)]),
            )
            .await?;

        // Verify container was created
        let containers = runtime.list_containers().await?;
        assert_eq!(containers.len(), 1);
        
        // Verify volume exists
        let volumes = runtime.list_volumes().await?;
        assert_eq!(volumes.len(), 1);
        assert_eq!(volumes[0].name, "data-volume");
        
        Ok(())
    }
    
    #[tokio::test]
    async fn test_monitor_containers() -> Result<()> {
        let (app, runtime, _) = create_test_app_manager().await?;

        // Deploy an app
        let container_id = app
            .deploy_app("nginx:latest", "monitor-test-app", None, None, None)
            .await?;

        // Run container monitoring
        app.monitor_containers().await?;
        
        // Verify container stats are available
        let stats = app.get_container_stats(&container_id).await;
        assert!(stats.is_some());
        
        let stats = stats.unwrap();
        assert!(stats.cpu_usage >= 0.0);
        assert!(stats.memory_usage > 0);
        
        Ok(())
    }
}
