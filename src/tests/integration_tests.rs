#[cfg(test)]
mod integration_tests {
    use crate::app::AppManager;
    use crate::service_discovery::ServiceDiscovery;
    use crate::storage::StorageManager;
    use std::path::PathBuf;
    use tempfile::tempdir;

    #[tokio::test]
    async fn test_app_with_service_discovery() {
        let temp_dir = tempdir().unwrap();
        let storage = StorageManager::new(temp_dir.path()).await.unwrap();
        let service_discovery = ServiceDiscovery::new();
        let app = AppManager::with_storage(storage)
            .await
            .unwrap()
            .with_service_discovery(service_discovery.clone());

        // Deploy app with service domain
        let domain = "test.service.local";
        let container_id = app
            .deploy_app("nginx:latest", "test-service", Some(domain), None, None)
            .await
            .unwrap();

        // Verify service was registered
        let services = service_discovery.list_services().await;
        assert_eq!(services.len(), 1);

        // Get service URL
        let service_url = service_discovery.get_service_url("test-service").await;
        assert!(service_url.is_some());
    }

    #[tokio::test]
    async fn test_app_scale_up_down() {
        let temp_dir = tempdir().unwrap();
        let storage = StorageManager::new(temp_dir.path()).await.unwrap();
        let app = AppManager::with_storage(storage).await.unwrap();

        // Deploy app with service domain
        let domain = "scale-test.service.local";
        app.deploy_app("nginx:latest", "scale-test", Some(domain), None, None)
            .await
            .unwrap();

        // Scale up to 3 replicas
        app.scale_app("scale-test", 3).await.unwrap();

        // Verify container count
        let service = app.get_service("scale-test").await.unwrap();
        assert_eq!(service.current_replicas, 3);
        assert_eq!(service.container_ids.len(), 3);

        // Scale down to 1 replica
        app.scale_app("scale-test", 1).await.unwrap();

        // Verify container count
        let service = app.get_service("scale-test").await.unwrap();
        assert_eq!(service.current_replicas, 1);
        assert_eq!(service.container_ids.len(), 1);
    }
}
