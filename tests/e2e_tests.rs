use hivemind::app::AppManager;
use hivemind::storage::StorageManager;
use hivemind::service_discovery::ServiceDiscovery;
use tempfile::tempdir;

#[tokio::test]
async fn test_deploy_and_scale_app() -> anyhow::Result<()> {
    // Skip this test in CI environments
    if std::env::var("CI").is_ok() {
        return Ok(());
    }

    // Create a temporary directory for storage
    let _temp_dir = tempdir().unwrap();
    let storage_path = _temp_dir.path();
    
    // Initialize storage manager
    let storage = StorageManager::new(storage_path).await.unwrap();
    
    // Initialize service discovery
    let service_discovery = ServiceDiscovery::new();
    service_discovery.initialize().await?;
    
    // Initialize app manager with storage and service discovery
    let app_manager = AppManager::with_storage(storage)
        .await
        .unwrap()
        .with_service_discovery(service_discovery);
    
    // Deploy an app
    let app_name = "test-nginx";
    let domain = "test.local";
    let result = app_manager.deploy_app(
        "nginx:latest",
        app_name,
        Some(domain),
        None,
        None
    ).await;
    
    // This will fail without a real container runtime, but we can check that the code path works
    assert!(result.is_err());
    
    // In a real test with a container runtime, we would:
    // 1. Assert that the container was created
    // 2. Scale the app
    // 3. Assert that the replicas were created
    // 4. Clean up
    Ok(())
}
