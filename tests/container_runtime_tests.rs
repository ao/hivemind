use anyhow::Result;
use hivemind::containerd_manager::{
    Container, ContainerManager, ContainerStats, ContainerStatus, EnvVar, PortMapping, Volume,
};
use std::sync::Arc;
use tokio::sync::Mutex;

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use tokio::time::Duration;

    // Helper function to create a test container manager
    async fn create_test_container_manager() -> Result<ContainerManager> {
        // This test assumes the containerd feature is enabled
        // and a containerd daemon is running on the default socket
        let socket_path = "/run/containerd/containerd.sock";
        let namespace = "hivemind-test";
        
        ContainerManager::new(socket_path.to_string(), namespace.to_string()).await
    }

    #[tokio::test]
    #[cfg(feature = "containerd")]
    async fn test_pull_image() -> Result<()> {
        let manager = create_test_container_manager().await?;
        
        // Pull a small test image
        let result = manager.pull_image("alpine:latest").await;
        assert!(result.is_ok(), "Failed to pull image: {:?}", result);
        
        Ok(())
    }

    #[tokio::test]
    #[cfg(feature = "containerd")]
    async fn test_container_lifecycle() -> Result<()> {
        let manager = create_test_container_manager().await?;
        
        // Pull the image first
        manager.pull_image("alpine:latest").await?;
        
        // Create a container
        let env_vars = vec![EnvVar {
            key: "TEST_VAR".to_string(),
            value: "test_value".to_string(),
        }];
        
        let ports = vec![PortMapping {
            container_port: 80,
            host_port: 8080,
            protocol: "tcp".to_string(),
        }];
        
        let container_id = manager
            .create_container("alpine:latest", "test-container", env_vars, ports)
            .await?;
        
        // Verify container exists
        let containers = manager.list_containers().await?;
        let container = containers.iter().find(|c| c.id == container_id);
        assert!(container.is_some(), "Container not found after creation");
        assert_eq!(container.unwrap().status, ContainerStatus::Running);
        
        // Get container metrics
        let metrics = manager.get_container_metrics(&container_id).await?;
        assert!(metrics.cpu_usage >= 0.0, "CPU usage should be non-negative");
        assert!(metrics.memory_usage > 0, "Memory usage should be positive");
        
        // Stream logs (just verify it doesn't error)
        let logs_stream = manager
            .stream_container_logs(&container_id, false, None, None, None, true, true)
            .await?;
        
        // Stop the container
        let stop_result = manager.stop_container(&container_id).await;
        assert!(stop_result.is_ok(), "Failed to stop container: {:?}", stop_result);
        
        // Verify container is stopped
        let containers = manager.list_containers().await?;
        let container = containers.iter().find(|c| c.id == container_id);
        assert!(container.is_some(), "Container not found after stopping");
        assert_eq!(container.unwrap().status, ContainerStatus::Stopped);
        
        Ok(())
    }

    #[tokio::test]
    #[cfg(feature = "containerd")]
    async fn test_container_with_volumes() -> Result<()> {
        let manager = create_test_container_manager().await?;
        
        // Create a volume
        let volume_name = format!("test-volume-{}", uuid::Uuid::new_v4());
        manager.create_volume(&volume_name).await?;
        
        // Verify volume exists
        let volumes = manager.list_volumes().await?;
        let volume = volumes.iter().find(|v| v.name == volume_name);
        assert!(volume.is_some(), "Volume not found after creation");
        
        // Pull the image
        manager.pull_image("alpine:latest").await?;
        
        // Create a container with the volume
        let env_vars = vec![EnvVar {
            key: "TEST_VAR".to_string(),
            value: "test_value".to_string(),
        }];
        
        let ports = vec![PortMapping {
            container_port: 80,
            host_port: 8080,
            protocol: "tcp".to_string(),
        }];
        
        let volume_mounts = vec![(volume_name.clone(), "/data".to_string())];
        
        let container_id = manager
            .create_container_with_volumes(
                "alpine:latest",
                "test-container-with-volume",
                env_vars,
                ports,
                volume_mounts,
            )
            .await?;
        
        // Verify container exists with volume
        let containers = manager.list_containers().await?;
        let container = containers.iter().find(|c| c.id == container_id);
        assert!(container.is_some(), "Container not found after creation");
        assert_eq!(container.unwrap().status, ContainerStatus::Running);
        assert!(!container.unwrap().volumes.is_empty(), "Container should have volumes");
        
        // Stop the container
        manager.stop_container(&container_id).await?;
        
        // Delete the volume
        manager.delete_volume(&volume_name).await?;
        
        // Verify volume is deleted
        let volumes = manager.list_volumes().await?;
        let volume = volumes.iter().find(|v| v.name == volume_name);
        assert!(volume.is_none(), "Volume still exists after deletion");
        
        Ok(())
    }

    #[tokio::test]
    #[cfg(feature = "containerd")]
    async fn test_health_check() -> Result<()> {
        use hivemind::containerd_manager::{HealthCheckStatus, HealthCheckType};
        
        let manager = create_test_container_manager().await?;
        
        // Pull the image
        manager.pull_image("nginx:latest").await?;
        
        // Create a container
        let env_vars = vec![];
        let ports = vec![PortMapping {
            container_port: 80,
            host_port: 8080,
            protocol: "tcp".to_string(),
        }];
        
        let container_id = manager
            .create_container("nginx:latest", "test-nginx", env_vars, ports)
            .await?;
        
        // Configure health check
        manager
            .configure_health_check(
                &container_id,
                HealthCheckType::Http {
                    port: 80,
                    path: "/".to_string(),
                    expected_status: 200,
                },
                5,
                2,
                3,
            )
            .await?;
        
        // Wait for health check to run
        tokio::time::sleep(Duration::from_secs(10)).await;
        
        // Get health check status
        let status = manager.get_health_check_status(&container_id).await?;
        
        // Stop the container
        manager.stop_container(&container_id).await?;
        
        Ok(())
    }
}