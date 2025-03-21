use hivemind::app::AppManager;
use hivemind::storage::StorageManager;
use std::path::Path;
use tempfile::tempdir;

#[tokio::test]
async fn test_app_manager_initialization() {
    // Create a temporary directory for storage
    let temp_dir = tempdir().unwrap();
    let storage_path = temp_dir.path();
    
    // Initialize storage manager
    let storage = StorageManager::new(storage_path).await.unwrap();
    
    // Initialize app manager with storage
    let app_manager = AppManager::with_storage(storage).await.unwrap();
    
    // Verify app manager has default images
    let images = app_manager.list_images().await.unwrap();
    assert!(!images.is_empty());
    assert!(images.contains(&"nginx:latest".to_string()));
}

#[tokio::test]
async fn test_storage_manager() {
    // Create a temporary directory for storage
    let temp_dir = tempdir().unwrap();
    let storage_path = temp_dir.path();
    
    // Initialize storage manager
    let storage = StorageManager::new(storage_path).await.unwrap();
    
    // Save a container
    storage.save_container(
        "test-container-1",
        "test-nginx",
        "nginx:latest",
        "running",
        "local"
    ).await.unwrap();
    
    // Get containers
    let containers = storage.get_containers().await.unwrap();
    assert_eq!(containers.len(), 1);
    
    let (id, name, image, status, node_id) = &containers[0];
    assert_eq!(id, "test-container-1");
    assert_eq!(name, "test-nginx");
    assert_eq!(image, "nginx:latest");
    assert_eq!(status, "running");
    assert_eq!(node_id, "local");
}
