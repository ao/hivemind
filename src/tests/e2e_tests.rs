#[cfg(test)]
mod e2e_tests {
    use crate::app::{AppManager, ServiceConfig};
    use crate::node::NodeManager;
    use crate::service_discovery::{ServiceDiscovery, ServiceHealth};
    use crate::storage::StorageManager;
    use crate::youki_manager::{ContainerStatus, PortMapping};
    use anyhow::Result;
    use reqwest;
    use std::path::PathBuf;
    use std::time::Duration;
    use tempfile::tempdir;
    use tokio::time::sleep;

    // Helper function to set up test environment
    async fn setup_test_environment() -> Result<(
        tempfile::TempDir,
        StorageManager,
        NodeManager,
        ServiceDiscovery,
        AppManager,
    )> {
        // Set up temporary directory for data
        let temp_dir = tempdir()?;

        // Initialize all components
        let storage = StorageManager::new(temp_dir.path()).await?;
        let node_manager = NodeManager::with_storage(storage.clone()).await;
        let service_discovery = ServiceDiscovery::new();
        let app_manager = AppManager::with_storage(storage.clone())
            .await?
            .with_service_discovery(service_discovery.clone());

        // Start node discovery
        node_manager.start_discovery().await?;

        // Start service discovery
        service_discovery.start_dns_server().await?;

        Ok((temp_dir, storage, node_manager, service_discovery, app_manager))
    }

    // Helper function to wait for service health with timeout
    async fn wait_for_service_health(
        service_discovery: &ServiceDiscovery,
        service_name: &str,
        timeout_secs: u64,
    ) -> Result<bool> {
        let start = std::time::Instant::now();
        let timeout = Duration::from_secs(timeout_secs);

        while start.elapsed() < timeout {
            // Check service health
            service_discovery.health_check_services().await?;
            
            // Get service endpoints
            if let Some(endpoints) = service_discovery.get_service_endpoints(service_name).await {
                // Check if any endpoint is healthy
                if endpoints.iter().any(|e| e.health_status == ServiceHealth::Healthy) {
                    return Ok(true);
                }
            }
            
            // Wait before checking again
            sleep(Duration::from_millis(500)).await;
        }
        
        Ok(false)
    }

    #[tokio::test]
    async fn test_full_deployment_cycle() -> Result<()> {
        // Set up test environment
        let (temp_dir, storage, node_manager, service_discovery, app_manager) = 
            setup_test_environment().await?;

        // Deploy a web app with an nginx container
        let domain = "test-e2e.service.local";
        let container_id = app_manager
            .deploy_app(
                "nginx:latest",
                "test-e2e-app",
                Some(domain),
                None,
                Some(vec![(80, 8080)]),
            )
            .await?;

        // Wait for service to be healthy with a timeout
        let is_healthy = wait_for_service_health(&service_discovery, "test-e2e-app", 10).await?;
        assert!(is_healthy, "Service did not become healthy within timeout");

        // Get service URL
        let service_url = service_discovery.get_service_url("test-e2e-app").await;
        assert!(service_url.is_some(), "Failed to get service URL");
        let url = service_url.unwrap();

        // Try to connect to the service
        let response = reqwest::get(&url).await;
        assert!(response.is_ok(), "Failed to connect to service");
        
        if let Ok(resp) = response {
            assert!(
                resp.status().is_success(),
                "Service returned non-success status: {}",
                resp.status()
            );
        }

        // Stop the container
        app_manager.stop_container(&container_id).await?;

        // Verify container is stopped
        let container = app_manager
            .get_container_by_id(&container_id)
            .await
            .expect("Container should exist");
        assert_eq!(
            container.status,
            ContainerStatus::Stopped,
            "Container should be stopped"
        );

        // Verify service is no longer available
        sleep(Duration::from_secs(1)).await; // Brief wait for service discovery to update
        let service_url = service_discovery.get_service_url("test-e2e-app").await;
        assert!(service_url.is_none(), "Service should no longer be available");

        Ok(())
    }

    #[tokio::test]
    async fn test_service_scaling() -> Result<()> {
        // Set up test environment
        let (temp_dir, storage, node_manager, service_discovery, app_manager) = 
            setup_test_environment().await?;

        // Deploy a web app
        let domain = "scaling-test.service.local";
        let container_id = app_manager
            .deploy_app(
                "nginx:latest",
                "scaling-test-app",
                Some(domain),
                None,
                Some(vec![(80, 8080)]),
            )
            .await?;

        // Wait for service to be healthy
        let is_healthy = wait_for_service_health(&service_discovery, "scaling-test-app", 10).await?;
        assert!(is_healthy, "Service did not become healthy within timeout");

        // Verify initial replica count
        let service = app_manager.get_service("scaling-test-app").await
            .expect("Service should exist");
        assert_eq!(service.current_replicas, 1, "Initial replica count should be 1");
        assert_eq!(service.container_ids.len(), 1, "Should have 1 container");

        // Scale up to 3 replicas
        app_manager.scale_app("scaling-test-app", 3).await?;

        // Wait briefly for scaling to complete
        sleep(Duration::from_secs(2)).await;

        // Verify new replica count
        let service = app_manager.get_service("scaling-test-app").await
            .expect("Service should exist after scaling");
        assert_eq!(service.current_replicas, 3, "Replica count should be 3 after scaling up");
        assert_eq!(service.container_ids.len(), 3, "Should have 3 containers after scaling up");

        // Scale down to 1 replica
        app_manager.scale_app("scaling-test-app", 1).await?;

        // Wait briefly for scaling to complete
        sleep(Duration::from_secs(2)).await;

        // Verify reduced replica count
        let service = app_manager.get_service("scaling-test-app").await
            .expect("Service should exist after scaling down");
        assert_eq!(service.current_replicas, 1, "Replica count should be 1 after scaling down");
        assert_eq!(service.container_ids.len(), 1, "Should have 1 container after scaling down");

        Ok(())
    }

    #[tokio::test]
    async fn test_app_restart() -> Result<()> {
        // Set up test environment
        let (temp_dir, storage, node_manager, service_discovery, app_manager) = 
            setup_test_environment().await?;

        // Deploy a web app
        let domain = "restart-test.service.local";
        let container_id = app_manager
            .deploy_app(
                "nginx:latest",
                "restart-test-app",
                Some(domain),
                None,
                Some(vec![(80, 8080)]),
            )
            .await?;

        // Wait for service to be healthy
        let is_healthy = wait_for_service_health(&service_discovery, "restart-test-app", 10).await?;
        assert!(is_healthy, "Service did not become healthy within timeout");

        // Get the original container ID
        let service = app_manager.get_service("restart-test-app").await
            .expect("Service should exist");
        let original_container_id = service.container_ids[0].clone();

        // Restart the app
        app_manager.restart_app("restart-test-app").await?;

        // Wait briefly for restart to complete
        sleep(Duration::from_secs(3)).await;

        // Verify service is still available
        let is_healthy = wait_for_service_health(&service_discovery, "restart-test-app", 10).await?;
        assert!(is_healthy, "Service should be healthy after restart");

        // Get the new container ID
        let service = app_manager.get_service("restart-test-app").await
            .expect("Service should exist after restart");
        let new_container_id = service.container_ids[0].clone();

        // The container ID might be the same in mock implementations, but the container
        // should at least be in running state
        let container = app_manager.get_container_by_id(&new_container_id).await
            .expect("Container should exist after restart");
        assert_eq!(
            container.status,
            ContainerStatus::Running,
            "Container should be running after restart"
        );

        Ok(())
    }
}
