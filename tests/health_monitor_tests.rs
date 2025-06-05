use anyhow::Result;
use hivemind::app::AppManager;
use hivemind::containerd_manager::{Container, ContainerStatus};
use hivemind::health_monitor::{
    Alert, AlertSeverity, AlertSource, AlertStatus, ContainerHealth, CustomHealthCheckResult,
    HealthCheckConfig, HealthHistoryEntry, HealthMonitor, HealthStatus, MetricDataPoint, MetricType,
    NetworkHealthStatus, NodeHealth, NodeHealthHistoryEntry,
};
use hivemind::node::NodeManager;
use hivemind::service_discovery::ServiceDiscovery;
use hivemind::storage::StorageManager;
use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tempfile::TempDir;
use tokio::time::sleep;

// Helper function to create a test container
fn create_test_container(id: &str, name: &str, status: ContainerStatus) -> Container {
    Container {
        id: id.to_string(),
        name: name.to_string(),
        image: "test-image:latest".to_string(),
        status,
        node_id: "test-node".to_string(),
        created_at: SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64,
        ports: Vec::new(),
        env_vars: Vec::new(),
        volumes: Vec::new(),
        service_domain: None,
    }
}

// Helper function to create a test container health
fn create_test_container_health(
    container_id: &str,
    status: HealthStatus,
    consecutive_failures: u32,
) -> ContainerHealth {
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs() as i64;
    
    ContainerHealth {
        container_id: container_id.to_string(),
        last_check: now,
        consecutive_failures,
        restart_count: 0,
        status,
        last_restart: None,
        health_history: vec![HealthHistoryEntry {
            timestamp: now,
            status: status.clone(),
            message: None,
        }],
        custom_check_result: None,
    }
}

// Helper function to create a test node health
fn create_test_node_health(
    node_id: &str,
    is_healthy: bool,
    consecutive_failures: u32,
) -> NodeHealth {
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs() as i64;
    
    NodeHealth {
        node_id: node_id.to_string(),
        last_seen: now,
        cpu_usage: 25.0,
        memory_usage: 40.0,
        disk_usage: 30.0,
        network_status: if is_healthy {
            NetworkHealthStatus::Connected
        } else {
            NetworkHealthStatus::Disconnected
        },
        container_count: 5,
        health_history: vec![NodeHealthHistoryEntry {
            timestamp: now,
            cpu_usage: 25.0,
            memory_usage: 40.0,
            disk_usage: 30.0,
            network_status: if is_healthy {
                NetworkHealthStatus::Connected
            } else {
                NetworkHealthStatus::Disconnected
            },
            message: None,
        }],
        consecutive_failures,
        is_healthy,
        last_failure: if is_healthy { None } else { Some(now) },
    }
}

#[tokio::test]
async fn test_health_monitor_initialization() -> Result<()> {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(NodeManager::with_storage(storage).await);
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create health monitor with default config
    let health_monitor = HealthMonitor::new(
        app_manager.clone(),
        node_manager.clone(),
        service_discovery.clone(),
    );
    
    // Verify default config values
    assert_eq!(health_monitor.get_config().check_interval_seconds, 30);
    assert_eq!(health_monitor.get_config().failure_threshold, 3);
    
    // Create health monitor with custom config
    let custom_config = HealthCheckConfig {
        check_interval_seconds: 10,
        failure_threshold: 2,
        restart_delay_seconds: 3,
        max_restart_attempts: 3,
        health_check_timeout_seconds: 5,
        custom_health_check_command: Some("curl http://localhost:8080/health".to_string()),
        node_check_interval_seconds: 30,
        node_failure_threshold: 2,
    };
    
    let health_monitor = HealthMonitor::with_config(
        app_manager,
        node_manager,
        service_discovery,
        custom_config.clone(),
    );
    
    // Verify custom config values
    assert_eq!(health_monitor.get_config().check_interval_seconds, 10);
    assert_eq!(health_monitor.get_config().failure_threshold, 2);
    assert_eq!(health_monitor.get_config().restart_delay_seconds, 3);
    assert_eq!(health_monitor.get_config().max_restart_attempts, 3);
    assert_eq!(health_monitor.get_config().health_check_timeout_seconds, 5);
    assert_eq!(
        health_monitor.get_config().custom_health_check_command,
        Some("curl http://localhost:8080/health".to_string())
    );
    
    Ok(())
}

#[tokio::test]
async fn test_container_health_tracking() -> Result<()> {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(NodeManager::with_storage(storage).await);
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create health monitor with custom config for faster testing
    let custom_config = HealthCheckConfig {
        check_interval_seconds: 1,
        failure_threshold: 2,
        restart_delay_seconds: 1,
        max_restart_attempts: 3,
        health_check_timeout_seconds: 1,
        custom_health_check_command: None,
        node_check_interval_seconds: 1,
        node_failure_threshold: 2,
    };
    
    let health_monitor = HealthMonitor::with_config(
        app_manager,
        node_manager,
        service_discovery,
        custom_config,
    );
    
    // Add container health records
    let container1_id = "container-1";
    let container2_id = "container-2";
    
    let container1_health = create_test_container_health(container1_id, HealthStatus::Healthy, 0);
    let container2_health = create_test_container_health(container2_id, HealthStatus::Unhealthy, 1);
    
    health_monitor.add_container_health(container1_health.clone()).await;
    health_monitor.add_container_health(container2_health.clone()).await;
    
    // Verify container health records
    let all_health = health_monitor.get_all_container_health().await;
    assert_eq!(all_health.len(), 2);
    
    let container1_health_record = health_monitor.get_container_health(container1_id).await;
    assert!(container1_health_record.is_some());
    assert_eq!(container1_health_record.unwrap().status, HealthStatus::Healthy);
    
    let container2_health_record = health_monitor.get_container_health(container2_id).await;
    assert!(container2_health_record.is_some());
    assert_eq!(container2_health_record.unwrap().status, HealthStatus::Unhealthy);
    assert_eq!(container2_health_record.unwrap().consecutive_failures, 1);
    
    // Update container health
    let mut updated_health = container2_health;
    updated_health.consecutive_failures = 2;
    updated_health.status = HealthStatus::Failed;
    
    health_monitor.add_container_health(updated_health).await;
    
    // Verify updated health
    let container2_health_record = health_monitor.get_container_health(container2_id).await;
    assert!(container2_health_record.is_some());
    assert_eq!(container2_health_record.unwrap().status, HealthStatus::Failed);
    assert_eq!(container2_health_record.unwrap().consecutive_failures, 2);
    
    Ok(())
}

#[tokio::test]
async fn test_node_health_tracking() -> Result<()> {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(NodeManager::with_storage(storage).await);
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create health monitor
    let health_monitor = HealthMonitor::new(
        app_manager,
        node_manager,
        service_discovery,
    );
    
    // Add node health records
    let node1_id = "node-1";
    let node2_id = "node-2";
    
    let node1_health = create_test_node_health(node1_id, true, 0);
    let node2_health = create_test_node_health(node2_id, false, 1);
    
    health_monitor.add_node_health(node1_health.clone()).await;
    health_monitor.add_node_health(node2_health.clone()).await;
    
    // Verify node health records
    let all_health = health_monitor.get_all_node_health().await;
    assert_eq!(all_health.len(), 2);
    
    let node1_health_record = health_monitor.get_node_health(node1_id).await;
    assert!(node1_health_record.is_some());
    assert!(node1_health_record.unwrap().is_healthy);
    
    let node2_health_record = health_monitor.get_node_health(node2_id).await;
    assert!(node2_health_record.is_some());
    assert!(!node2_health_record.unwrap().is_healthy);
    assert_eq!(node2_health_record.unwrap().consecutive_failures, 1);
    
    // Update node health
    let mut updated_health = node2_health;
    updated_health.consecutive_failures = 2;
    updated_health.network_status = NetworkHealthStatus::Degraded;
    
    health_monitor.add_node_health(updated_health).await;
    
    // Verify updated health
    let node2_health_record = health_monitor.get_node_health(node2_id).await;
    assert!(node2_health_record.is_some());
    assert!(!node2_health_record.unwrap().is_healthy);
    assert_eq!(node2_health_record.unwrap().consecutive_failures, 2);
    assert_eq!(node2_health_record.unwrap().network_status, NetworkHealthStatus::Degraded);
    
    Ok(())
}

#[tokio::test]
async fn test_alert_management() -> Result<()> {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(NodeManager::with_storage(storage).await);
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create health monitor
    let health_monitor = HealthMonitor::new(
        app_manager,
        node_manager,
        service_discovery,
    );
    
    // Create alerts
    let container_id = "container-1";
    let node_id = "node-1";
    
    health_monitor.create_alert(
        AlertSeverity::Warning,
        AlertSource::Container(container_id.to_string()),
        "Container health check failed".to_string(),
    ).await;
    
    health_monitor.create_alert(
        AlertSeverity::Critical,
        AlertSource::Node(node_id.to_string()),
        "Node disconnected".to_string(),
    ).await;
    
    health_monitor.create_alert(
        AlertSeverity::Info,
        AlertSource::System,
        "System maintenance scheduled".to_string(),
    ).await;
    
    // Verify alerts
    let active_alerts = health_monitor.get_active_alerts().await;
    assert_eq!(active_alerts.len(), 3);
    
    // Find the container alert
    let container_alert = active_alerts.iter().find(|a| {
        matches!(a.source, AlertSource::Container(ref id) if id == container_id)
    }).unwrap();
    
    assert_eq!(container_alert.severity, AlertSeverity::Warning);
    assert_eq!(container_alert.status, AlertStatus::Active);
    
    // Find the node alert
    let node_alert = active_alerts.iter().find(|a| {
        matches!(a.source, AlertSource::Node(ref id) if id == node_id)
    }).unwrap();
    
    assert_eq!(node_alert.severity, AlertSeverity::Critical);
    assert_eq!(node_alert.status, AlertStatus::Active);
    
    // Acknowledge an alert
    health_monitor.acknowledge_alert(&node_alert.id).await?;
    
    // Verify acknowledgment
    let active_alerts = health_monitor.get_active_alerts().await;
    let node_alert = active_alerts.iter().find(|a| {
        matches!(a.source, AlertSource::Node(ref id) if id == node_id)
    }).unwrap();
    
    assert_eq!(node_alert.status, AlertStatus::Acknowledged);
    
    // Resolve an alert
    health_monitor.resolve_alert(&container_alert.id).await?;
    
    // Verify resolution
    let active_alerts = health_monitor.get_active_alerts().await;
    assert_eq!(active_alerts.len(), 2); // One alert resolved
    
    let all_alerts = health_monitor.get_all_alerts().await;
    assert_eq!(all_alerts.len(), 3); // All alerts still in history
    
    let container_alert = all_alerts.iter().find(|a| {
        matches!(a.source, AlertSource::Container(ref id) if id == container_id)
    }).unwrap();
    
    assert_eq!(container_alert.status, AlertStatus::Resolved);
    assert!(container_alert.resolved_at.is_some());
    
    Ok(())
}

#[tokio::test]
async fn test_metrics_tracking() -> Result<()> {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(NodeManager::with_storage(storage).await);
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create health monitor
    let health_monitor = HealthMonitor::new(
        app_manager,
        node_manager,
        service_discovery,
    );
    
    // Add metrics for a container
    let container_id = "container-1";
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs() as i64;
    
    // Add CPU metrics
    health_monitor.add_metric(
        container_id,
        MetricType::CpuUsage,
        25.5,
        now - 60,
    ).await;
    
    health_monitor.add_metric(
        container_id,
        MetricType::CpuUsage,
        30.2,
        now - 30,
    ).await;
    
    health_monitor.add_metric(
        container_id,
        MetricType::CpuUsage,
        28.7,
        now,
    ).await;
    
    // Add memory metrics
    health_monitor.add_metric(
        container_id,
        MetricType::MemoryUsage,
        512.0,
        now - 60,
    ).await;
    
    health_monitor.add_metric(
        container_id,
        MetricType::MemoryUsage,
        550.0,
        now - 30,
    ).await;
    
    health_monitor.add_metric(
        container_id,
        MetricType::MemoryUsage,
        525.0,
        now,
    ).await;
    
    // Verify CPU metrics
    let cpu_metrics = health_monitor.get_metrics_history(container_id, MetricType::CpuUsage).await;
    assert_eq!(cpu_metrics.len(), 3);
    assert_eq!(cpu_metrics[0].value, 25.5);
    assert_eq!(cpu_metrics[1].value, 30.2);
    assert_eq!(cpu_metrics[2].value, 28.7);
    
    // Verify memory metrics
    let memory_metrics = health_monitor.get_metrics_history(container_id, MetricType::MemoryUsage).await;
    assert_eq!(memory_metrics.len(), 3);
    assert_eq!(memory_metrics[0].value, 512.0);
    assert_eq!(memory_metrics[1].value, 550.0);
    assert_eq!(memory_metrics[2].value, 525.0);
    
    // Add metrics for a node
    let node_id = "node-1";
    
    // Add disk usage metrics
    health_monitor.add_metric(
        node_id,
        MetricType::DiskUsage,
        45.0,
        now - 60,
    ).await;
    
    health_monitor.add_metric(
        node_id,
        MetricType::DiskUsage,
        46.5,
        now - 30,
    ).await;
    
    health_monitor.add_metric(
        node_id,
        MetricType::DiskUsage,
        48.0,
        now,
    ).await;
    
    // Verify disk metrics
    let disk_metrics = health_monitor.get_metrics_history(node_id, MetricType::DiskUsage).await;
    assert_eq!(disk_metrics.len(), 3);
    assert_eq!(disk_metrics[0].value, 45.0);
    assert_eq!(disk_metrics[1].value, 46.5);
    assert_eq!(disk_metrics[2].value, 48.0);
    
    Ok(())
}

#[tokio::test]
async fn test_health_summary() -> Result<()> {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(NodeManager::with_storage(storage).await);
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create health monitor
    let health_monitor = HealthMonitor::new(
        app_manager,
        node_manager,
        service_discovery,
    );
    
    // Add container health records
    let container1_id = "container-1";
    let container2_id = "container-2";
    let container3_id = "container-3";
    
    health_monitor.add_container_health(
        create_test_container_health(container1_id, HealthStatus::Healthy, 0)
    ).await;
    
    health_monitor.add_container_health(
        create_test_container_health(container2_id, HealthStatus::Unhealthy, 1)
    ).await;
    
    health_monitor.add_container_health(
        create_test_container_health(container3_id, HealthStatus::Failed, 3)
    ).await;
    
    // Add node health records
    let node1_id = "node-1";
    let node2_id = "node-2";
    
    health_monitor.add_node_health(
        create_test_node_health(node1_id, true, 0)
    ).await;
    
    health_monitor.add_node_health(
        create_test_node_health(node2_id, false, 2)
    ).await;
    
    // Create alerts
    health_monitor.create_alert(
        AlertSeverity::Warning,
        AlertSource::Container(container2_id.to_string()),
        "Container health check failed".to_string(),
    ).await;
    
    health_monitor.create_alert(
        AlertSeverity::Critical,
        AlertSource::Container(container3_id.to_string()),
        "Container failed".to_string(),
    ).await;
    
    health_monitor.create_alert(
        AlertSeverity::Critical,
        AlertSource::Node(node2_id.to_string()),
        "Node disconnected".to_string(),
    ).await;
    
    // Get health summary
    let summary = health_monitor.get_health_summary().await;
    
    // Verify summary
    assert_eq!(summary.total_containers, 3);
    assert_eq!(summary.healthy_containers, 1);
    assert_eq!(summary.unhealthy_containers, 1);
    assert_eq!(summary.failed_containers, 1);
    
    assert_eq!(summary.total_nodes, 2);
    assert_eq!(summary.healthy_nodes, 1);
    assert_eq!(summary.unhealthy_nodes, 1);
    
    assert_eq!(summary.critical_alerts, 2);
    assert_eq!(summary.warning_alerts, 1);
    assert_eq!(summary.info_alerts, 0);
    
    Ok(())
}

#[tokio::test]
async fn test_custom_health_checks() -> Result<()> {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(NodeManager::with_storage(storage).await);
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create health monitor with custom health check command
    let custom_config = HealthCheckConfig {
        check_interval_seconds: 30,
        failure_threshold: 3,
        restart_delay_seconds: 5,
        max_restart_attempts: 5,
        health_check_timeout_seconds: 10,
        custom_health_check_command: Some("curl http://localhost:8080/health".to_string()),
        node_check_interval_seconds: 60,
        node_failure_threshold: 3,
    };
    
    let health_monitor = HealthMonitor::with_config(
        app_manager,
        node_manager,
        service_discovery,
        custom_config,
    );
    
    // Create a test container
    let container_id = "test-container";
    let container = create_test_container(container_id, "test", ContainerStatus::Running);
    
    // Simulate a custom health check result
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs() as i64;
    
    let custom_result = CustomHealthCheckResult {
        exit_code: 0,
        output: "OK".to_string(),
        timestamp: now,
        success: true,
    };
    
    // Add container health with custom check result
    let mut container_health = create_test_container_health(container_id, HealthStatus::Healthy, 0);
    container_health.custom_check_result = Some(custom_result.clone());
    
    health_monitor.add_container_health(container_health).await;
    
    // Verify custom health check result
    let health_record = health_monitor.get_container_health(container_id).await;
    assert!(health_record.is_some());
    
    let custom_check = health_record.unwrap().custom_check_result;
    assert!(custom_check.is_some());
    
    let check_result = custom_check.unwrap();
    assert_eq!(check_result.exit_code, 0);
    assert_eq!(check_result.output, "OK");
    assert!(check_result.success);
    
    Ok(())
}

#[tokio::test]
async fn test_container_restart() -> Result<()> {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(NodeManager::with_storage(storage).await);
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create health monitor
    let health_monitor = HealthMonitor::new(
        app_manager.clone(),
        node_manager,
        service_discovery,
    );
    
    // Create a test container
    let container_id = "test-container";
    let container = create_test_container(container_id, "test", ContainerStatus::Running);
    
    // Add container to app manager's mock state
    app_manager.add_test_container(container).await?;
    
    // Add failed container health
    let mut container_health = create_test_container_health(container_id, HealthStatus::Failed, 3);
    container_health.restart_count = 1; // Already restarted once
    
    health_monitor.add_container_health(container_health).await;
    
    // Force restart the container
    health_monitor.force_restart_container(container_id).await?;
    
    // Verify container health was updated
    let health_record = health_monitor.get_container_health(container_id).await;
    assert!(health_record.is_some());
    
    let updated_health = health_record.unwrap();
    assert_eq!(updated_health.status, HealthStatus::Restarting);
    assert_eq!(updated_health.restart_count, 2); // Incremented
    assert!(updated_health.last_restart.is_some());
    
    Ok(())
}