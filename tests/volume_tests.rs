use anyhow::Result;
use hivemind::app::AppManager;
use hivemind::containerd_manager::{ContainerStatus, Volume};
use hivemind::storage::StorageManager;
use std::path::PathBuf;
use tokio::fs;

#[tokio::test]
async fn test_volume_lifecycle() -> Result<()> {
    // Create a temporary directory for testing
    let temp_dir = tempfile::tempdir()?;
    let data_dir = temp_dir.path().to_path_buf();
    
    // Initialize storage manager
    let storage = StorageManager::new(&data_dir).await?;
    
    // Initialize app manager with storage
    let app_manager = AppManager::with_storage(storage.clone()).await?;
    
    // Test volume creation
    let volume_name = "test-volume";
    app_manager.create_volume(volume_name).await?;
    
    // Verify volume exists
    let volumes = app_manager.list_volumes().await?;
    assert!(volumes.iter().any(|v| v.name == volume_name), "Volume not found after creation");
    
    // Test deploying a container with the volume
    let container_name = "test-container";
    let image = "alpine:latest";
    let mount_path = "/data";
    let volume_mounts = vec![(volume_name.to_string(), mount_path.to_string())];
    
    let container_id = app_manager
        .deploy_app_with_volumes(
            image,
            container_name,
            None,
            volume_mounts,
            None,
            None,
        )
        .await?;
    
    // Verify container exists with volume
    let containers = app_manager.get_container_details().await?;
    let container = containers.iter().find(|c| c.id == container_id);
    assert!(container.is_some(), "Container not found after creation");
    
    let container = container.unwrap();
    assert_eq!(container.status, ContainerStatus::Running);
    assert!(!container.volumes.is_empty(), "Container should have volumes");
    assert_eq!(container.volumes[0].name, volume_name);
    
    // Test volume backup
    let backup_path = data_dir.join("volume-backup.tar.gz");
    app_manager.backup_volume(volume_name, backup_path.to_str().unwrap()).await?;
    
    // Verify backup file exists
    assert!(backup_path.exists(), "Backup file not created");
    
    // Delete the volume
    app_manager.delete_volume(volume_name).await.expect_err("Should fail to delete volume in use");
    
    // Stop the container first
    app_manager.stop_container(&container_id).await?;
    
    // Now delete the volume
    app_manager.delete_volume(volume_name).await?;
    
    // Verify volume is gone
    let volumes = app_manager.list_volumes().await?;
    assert!(!volumes.iter().any(|v| v.name == volume_name), "Volume still exists after deletion");
    
    // Test volume restore
    let restore_volume_name = "restored-volume";
    app_manager.restore_volume(restore_volume_name, backup_path.to_str().unwrap()).await?;
    
    // Verify restored volume exists
    let volumes = app_manager.list_volumes().await?;
    assert!(volumes.iter().any(|v| v.name == restore_volume_name), "Restored volume not found");
    
    // Clean up
    app_manager.delete_volume(restore_volume_name).await?;
    fs::remove_file(backup_path).await?;
    
    Ok(())
}

#[tokio::test]
async fn test_volume_integration_with_deployment() -> Result<()> {
    use hivemind::deployment::{Deployment, DeploymentStrategy};
    
    // Create a temporary directory for testing
    let temp_dir = tempfile::tempdir()?;
    let data_dir = temp_dir.path().to_path_buf();
    
    // Initialize storage manager
    let storage = StorageManager::new(&data_dir).await?;
    
    // Initialize app manager with storage
    let app_manager = AppManager::with_storage(storage.clone()).await?;
    
    // Create a volume
    let volume_name = "deployment-test-volume";
    app_manager.create_volume(volume_name).await?;
    
    // Create a deployment with the volume
    let deployment_id = uuid::Uuid::new_v4().to_string();
    let app_name = "test-app";
    let image = "nginx:latest";
    let mount_path = "/usr/share/nginx/html";
    
    let mut deployment = Deployment::new(
        deployment_id.clone(),
        app_name.to_string(),
        image.to_string(),
        DeploymentStrategy::Simple,
        None,
    );
    
    // Add volume to deployment
    deployment = deployment.with_volume(volume_name.to_string(), mount_path.to_string());
    
    // In a real test, we would use the DeploymentManager to execute the deployment
    // For this test, we'll just verify the volume was added to the deployment
    assert_eq!(deployment.volumes.len(), 1);
    assert_eq!(deployment.volumes[0].0, volume_name);
    assert_eq!(deployment.volumes[0].1, mount_path);
    
    // Clean up
    app_manager.delete_volume(volume_name).await?;
    
    Ok(())
}