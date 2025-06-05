use anyhow::Result;
use hivemind::app::AppManager;
use hivemind::containerd_manager::{Container, ContainerStatus};
use hivemind::health_monitor::{HealthCheckConfig, HealthMonitor};
use hivemind::membership::{Member, MembershipConfig, MembershipEvent, MembershipProtocol, NodeState};
use hivemind::network::NetworkManager;
use hivemind::node::{NodeManager, NodeResources};
use hivemind::scheduler::ContainerScheduler;
use hivemind::service_discovery::ServiceDiscovery;
use hivemind::storage::StorageManager;
use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tempfile::TempDir;
use tokio::sync::Mutex;
use tokio::time::sleep;

// Helper function to create a test container
fn create_test_container(id: &str, name: &str, node_id: &str) -> Container {
    Container {
        id: id.to_string(),
        name: name.to_string(),
        image: "test-image:latest".to_string(),
        status: ContainerStatus::Running,
        node_id: node_id.to_string(),
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

// Helper function to create a test member
fn create_test_member(id: &str, address: &str, state: NodeState) -> Member {
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs();
    
    Member {
        id: id.to_string(),
        address: address.to_string(),
        state,
        incarnation: 1,
        last_state_change: now,
        is_leader: false,
        last_leader_check: now,
        metadata: Some(HashMap::new()),
    }
}

// Mock NodeManager for chaos testing
struct MockNodeManager {
    nodes: Arc<Mutex<HashMap<String, (String, NodeResources)>>>,
    node_id: String,
}

impl MockNodeManager {
    fn new(node_id: &str) -> Self {
        let mut nodes = HashMap::new();
        
        // Add self node
        nodes.insert(
            node_id.to_string(),
            (
                "192.168.1.1".to_string(),
                NodeResources {
                    cpu_total: 8.0,
                    cpu_available: 8.0,
                    memory_total: 16 * 1024 * 1024 * 1024,
                    memory_available: 16 * 1024 * 1024 * 1024,
                    disk_total: 100 * 1024 * 1024 * 1024,
                    disk_available: 100 * 1024 * 1024 * 1024,
                },
            ),
        );
        
        Self {
            nodes: Arc::new(Mutex::new(nodes)),
            node_id: node_id.to_string(),
        }
    }
    
    async fn add_node(&self, id: &str, address: &str, resources: NodeResources) {
        let mut nodes = self.nodes.lock().await;
        nodes.insert(id.to_string(), (address.to_string(), resources));
    }
    
    async fn remove_node(&self, id: &str) {
        let mut nodes = self.nodes.lock().await;
        nodes.remove(id);
    }
    
    async fn update_node_resources(&self, id: &str, resources: NodeResources) {
        let mut nodes = self.nodes.lock().await;
        if let Some((address, _)) = nodes.get(id) {
            nodes.insert(id.to_string(), (address.clone(), resources));
        }
    }
}

#[async_trait::async_trait]
impl NodeManager for MockNodeManager {
    fn get_node_id(&self) -> String {
        self.node_id.clone()
    }

    async fn list_nodes(&self) -> Result<Vec<String>> {
        let nodes = self.nodes.lock().await;
        Ok(nodes.keys().cloned().collect())
    }

    async fn get_node_details(&self) -> Result<Vec<(String, String, NodeResources)>> {
        let nodes = self.nodes.lock().await;
        let mut result = Vec::new();
        for (id, (address, resources)) in nodes.iter() {
            result.push((id.clone(), address.clone(), resources.clone()));
        }
        Ok(result)
    }
}

#[tokio::test]
async fn test_node_failure_recovery() -> Result<()> {
    // Create components
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(MockNodeManager::new("node-1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Add additional nodes
    node_manager.add_node(
        "node-2",
        "192.168.1.2",
        NodeResources {
            cpu_total: 8.0,
            cpu_available: 8.0,
            memory_total: 16 * 1024 * 1024 * 1024,
            memory_available: 16 * 1024 * 1024 * 1024,
            disk_total: 100 * 1024 * 1024 * 1024,
            disk_available: 100 * 1024 * 1024 * 1024,
        },
    ).await;
    
    node_manager.add_node(
        "node-3",
        "192.168.1.3",
        NodeResources {
            cpu_total: 8.0,
            cpu_available: 8.0,
            memory_total: 16 * 1024 * 1024 * 1024,
            memory_available: 16 * 1024 * 1024 * 1024,
            disk_total: 100 * 1024 * 1024 * 1024,
            disk_available: 100 * 1024 * 1024 * 1024,
        },
    ).await;
    
    // Create scheduler
    let scheduler = ContainerScheduler::new(app_manager.clone())
        .with_node_manager(node_manager.clone())
        .with_service_discovery(service_discovery.clone());
    
    // Create health monitor with fast check intervals
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
    
    let health_monitor = Arc::new(HealthMonitor::with_config(
        app_manager.clone(),
        node_manager.clone(),
        service_discovery.clone(),
        custom_config,
    ));
    
    // Create membership protocol for node-1
    let mut config = MembershipConfig::default();
    config.bind_port = 7960;
    config.protocol_period = Duration::from_millis(100);
    
    let membership = MembershipProtocol::new(
        "node-1".to_string(),
        "192.168.1.1:7960".to_string(),
        Some(config),
    ).await?;
    
    // Start health monitor
    health_monitor.start().await?;
    
    // Add containers to nodes
    let container1 = create_test_container("container-1", "web-1", "node-1");
    let container2 = create_test_container("container-2", "web-2", "node-2");
    let container3 = create_test_container("container-3", "web-3", "node-3");
    
    app_manager.add_test_container(container1).await?;
    app_manager.add_test_container(container2).await?;
    app_manager.add_test_container(container3).await?;
    
    // Verify initial state
    let nodes = node_manager.list_nodes().await?;
    assert_eq!(nodes.len(), 3);
    
    let containers = app_manager.get_container_details().await?;
    assert_eq!(containers.len(), 3);
    
    // Simulate node-2 failure
    println!("Simulating node-2 failure...");
    node_manager.remove_node("node-2").await;
    
    // Wait for health monitor to detect failure
    sleep(Duration::from_millis(500)).await;
    
    // Verify node-2 is removed
    let nodes = node_manager.list_nodes().await?;
    assert_eq!(nodes.len(), 2);
    assert!(!nodes.contains(&"node-2".to_string()));
    
    // Verify container on node-2 is marked as failed
    let container2 = app_manager.get_container("container-2").await?;
    assert_eq!(container2.status, ContainerStatus::Failed);
    
    // Simulate node-2 recovery
    println!("Simulating node-2 recovery...");
    node_manager.add_node(
        "node-2",
        "192.168.1.2",
        NodeResources {
            cpu_total: 8.0,
            cpu_available: 8.0,
            memory_total: 16 * 1024 * 1024 * 1024,
            memory_available: 16 * 1024 * 1024 * 1024,
            disk_total: 100 * 1024 * 1024 * 1024,
            disk_available: 100 * 1024 * 1024 * 1024,
        },
    ).await;
    
    // Wait for recovery
    sleep(Duration::from_millis(500)).await;
    
    // Verify node-2 is back
    let nodes = node_manager.list_nodes().await?;
    assert_eq!(nodes.len(), 3);
    assert!(nodes.contains(&"node-2".to_string()));
    
    // Stop health monitor
    health_monitor.stop().await;
    
    Ok(())
}

#[tokio::test]
async fn test_container_failure_recovery() -> Result<()> {
    // Create components
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(MockNodeManager::new("node-1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create health monitor with fast check intervals
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
    
    let health_monitor = Arc::new(HealthMonitor::with_config(
        app_manager.clone(),
        node_manager.clone(),
        service_discovery.clone(),
        custom_config,
    ));
    
    // Start health monitor
    health_monitor.start().await?;
    
    // Add containers
    let container1 = create_test_container("container-1", "web-1", "node-1");
    let container2 = create_test_container("container-2", "web-2", "node-1");
    
    app_manager.add_test_container(container1).await?;
    app_manager.add_test_container(container2).await?;
    
    // Verify initial state
    let containers = app_manager.get_container_details().await?;
    assert_eq!(containers.len(), 2);
    assert_eq!(containers[0].status, ContainerStatus::Running);
    assert_eq!(containers[1].status, ContainerStatus::Running);
    
    // Simulate container-1 failure
    println!("Simulating container-1 failure...");
    app_manager.update_container_status("container-1", ContainerStatus::Failed).await?;
    
    // Wait for health monitor to detect and attempt restart
    sleep(Duration::from_millis(500)).await;
    
    // Verify container-1 is being restarted
    let container1 = app_manager.get_container("container-1").await?;
    assert!(container1.status == ContainerStatus::Restarting || container1.status == ContainerStatus::Running);
    
    // Simulate successful restart
    app_manager.update_container_status("container-1", ContainerStatus::Running).await?;
    
    // Wait for health monitor to detect recovery
    sleep(Duration::from_millis(500)).await;
    
    // Verify container-1 is running again
    let container1 = app_manager.get_container("container-1").await?;
    assert_eq!(container1.status, ContainerStatus::Running);
    
    // Stop health monitor
    health_monitor.stop().await;
    
    Ok(())
}

#[tokio::test]
async fn test_network_partition() -> Result<()> {
    // Create components
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(MockNodeManager::new("node-1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Add additional nodes
    node_manager.add_node(
        "node-2",
        "192.168.1.2",
        NodeResources {
            cpu_total: 8.0,
            cpu_available: 8.0,
            memory_total: 16 * 1024 * 1024 * 1024,
            memory_available: 16 * 1024 * 1024 * 1024,
            disk_total: 100 * 1024 * 1024 * 1024,
            disk_available: 100 * 1024 * 1024 * 1024,
        },
    ).await;
    
    node_manager.add_node(
        "node-3",
        "192.168.1.3",
        NodeResources {
            cpu_total: 8.0,
            cpu_available: 8.0,
            memory_total: 16 * 1024 * 1024 * 1024,
            memory_available: 16 * 1024 * 1024 * 1024,
            disk_total: 100 * 1024 * 1024 * 1024,
            disk_available: 100 * 1024 * 1024 * 1024,
        },
    ).await;
    
    // Create network manager
    let network_manager = NetworkManager::new(
        node_manager.clone(),
        service_discovery.clone(),
        None,
    ).await?;
    
    // Create membership protocol for node-1
    let mut config = MembershipConfig::default();
    config.bind_port = 7961;
    config.protocol_period = Duration::from_millis(100);
    config.quorum_percentage = 0.51; // >50% for quorum
    
    let membership = MembershipProtocol::new(
        "node-1".to_string(),
        "192.168.1.1:7961".to_string(),
        Some(config),
    ).await?;
    
    // Get event receiver
    let mut event_rx = membership.get_event_receiver().await;
    
    // Start membership protocol
    membership.start().await?;
    
    // Add members to membership protocol
    let member2 = create_test_member("node-2", "192.168.1.2:7961", NodeState::Alive);
    let member3 = create_test_member("node-3", "192.168.1.3:7961", NodeState::Alive);
    
    membership.add_member(member2).await?;
    membership.add_member(member3).await?;
    
    // Verify initial state
    let members = membership.get_all_members().await;
    assert_eq!(members.len(), 3);
    
    // Simulate network partition (node-1 can't see node-2 and node-3)
    println!("Simulating network partition...");
    
    // Mark node-2 and node-3 as suspected
    membership.suspect_member("node-2").await?;
    membership.suspect_member("node-3").await?;
    
    // Wait for suspicion to propagate
    sleep(Duration::from_millis(200)).await;
    
    // Verify nodes are suspected
    let members = membership.get_all_members().await;
    let node2 = members.iter().find(|m| m.id == "node-2").unwrap();
    let node3 = members.iter().find(|m| m.id == "node-3").unwrap();
    
    assert_eq!(node2.state, NodeState::Suspected);
    assert_eq!(node3.state, NodeState::Suspected);
    
    // Check for network partition event
    let mut found_partition_event = false;
    while let Ok(event) = event_rx.try_recv() {
        if let MembershipEvent::NetworkPartitionDetected = event {
            found_partition_event = true;
            break;
        }
    }
    
    assert!(found_partition_event, "Network partition event not detected");
    
    // Simulate partition healing
    println!("Simulating partition healing...");
    
    // Mark node-2 and node-3 as alive again
    let mut node2 = members.iter().find(|m| m.id == "node-2").unwrap().clone();
    let mut node3 = members.iter().find(|m| m.id == "node-3").unwrap().clone();
    
    node2.state = NodeState::Alive;
    node3.state = NodeState::Alive;
    
    membership.add_member(node2).await?;
    membership.add_member(node3).await?;
    
    // Wait for state to propagate
    sleep(Duration::from_millis(200)).await;
    
    // Verify nodes are alive
    let members = membership.get_all_members().await;
    let node2 = members.iter().find(|m| m.id == "node-2").unwrap();
    let node3 = members.iter().find(|m| m.id == "node-3").unwrap();
    
    assert_eq!(node2.state, NodeState::Alive);
    assert_eq!(node3.state, NodeState::Alive);
    
    // Check for partition resolved event
    let mut found_resolved_event = false;
    while let Ok(event) = event_rx.try_recv() {
        if let MembershipEvent::NetworkPartitionResolved = event {
            found_resolved_event = true;
            break;
        }
    }
    
    assert!(found_resolved_event, "Network partition resolved event not detected");
    
    Ok(())
}

#[tokio::test]
async fn test_resource_exhaustion() -> Result<()> {
    // Create components
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(MockNodeManager::new("node-1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create scheduler
    let scheduler = ContainerScheduler::new(app_manager.clone())
        .with_node_manager(node_manager.clone())
        .with_service_discovery(service_discovery.clone());
    
    // Create health monitor
    let health_monitor = Arc::new(HealthMonitor::new(
        app_manager.clone(),
        node_manager.clone(),
        service_discovery.clone(),
    ));
    
    // Start health monitor
    health_monitor.start().await?;
    
    // Add containers
    let container1 = create_test_container("container-1", "web-1", "node-1");
    let container2 = create_test_container("container-2", "web-2", "node-1");
    
    app_manager.add_test_container(container1).await?;
    app_manager.add_test_container(container2).await?;
    
    // Verify initial state
    let node_resources = node_manager.get_node_details().await?[0].2.clone();
    assert_eq!(node_resources.cpu_available, 8.0);
    assert_eq!(node_resources.memory_available, 16 * 1024 * 1024 * 1024);
    
    // Simulate resource exhaustion
    println!("Simulating resource exhaustion...");
    
    // Update node resources to simulate high CPU usage
    node_manager.update_node_resources(
        "node-1",
        NodeResources {
            cpu_total: 8.0,
            cpu_available: 0.5, // Almost exhausted
            memory_total: 16 * 1024 * 1024 * 1024,
            memory_available: 16 * 1024 * 1024 * 1024,
            disk_total: 100 * 1024 * 1024 * 1024,
            disk_available: 100 * 1024 * 1024 * 1024,
        },
    ).await;
    
    // Wait for health monitor to detect resource exhaustion
    sleep(Duration::from_millis(500)).await;
    
    // Verify health monitor created alerts for resource exhaustion
    let alerts = health_monitor.get_active_alerts().await;
    let resource_alerts = alerts.iter().filter(|a| {
        matches!(a.source, hivemind::health_monitor::AlertSource::Node(ref id) if id == "node-1")
            && a.message.contains("resource")
    }).collect::<Vec<_>>();
    
    assert!(!resource_alerts.is_empty(), "Resource exhaustion alert not found");
    
    // Simulate resource recovery
    println!("Simulating resource recovery...");
    
    // Update node resources back to normal
    node_manager.update_node_resources(
        "node-1",
        NodeResources {
            cpu_total: 8.0,
            cpu_available: 8.0,
            memory_total: 16 * 1024 * 1024 * 1024,
            memory_available: 16 * 1024 * 1024 * 1024,
            disk_total: 100 * 1024 * 1024 * 1024,
            disk_available: 100 * 1024 * 1024 * 1024,
        },
    ).await;
    
    // Wait for health monitor to detect recovery
    sleep(Duration::from_millis(500)).await;
    
    // Stop health monitor
    health_monitor.stop().await;
    
    Ok(())
}

#[tokio::test]
async fn test_leader_failure_recovery() -> Result<()> {
    // Create membership protocol for node-1 (leader)
    let mut config1 = MembershipConfig::default();
    config1.bind_port = 7962;
    config1.protocol_period = Duration::from_millis(100);
    config1.leader_check_interval = Duration::from_millis(100);
    
    let membership1 = MembershipProtocol::new(
        "node-1".to_string(),
        "192.168.1.1:7962".to_string(),
        Some(config1),
    ).await?;
    
    // Create membership protocol for node-2
    let mut config2 = MembershipConfig::default();
    config2.bind_port = 7963;
    config2.protocol_period = Duration::from_millis(100);
    config2.leader_check_interval = Duration::from_millis(100);
    
    let membership2 = MembershipProtocol::new(
        "node-2".to_string(),
        "192.168.1.2:7963".to_string(),
        Some(config2),
    ).await?;
    
    // Create membership protocol for node-3
    let mut config3 = MembershipConfig::default();
    config3.bind_port = 7964;
    config3.protocol_period = Duration::from_millis(100);
    config3.leader_check_interval = Duration::from_millis(100);
    
    let membership3 = MembershipProtocol::new(
        "node-3".to_string(),
        "192.168.1.3:7964".to_string(),
        Some(config3),
    ).await?;
    
    // Get event receivers
    let mut event_rx1 = membership1.get_event_receiver().await;
    let mut event_rx2 = membership2.get_event_receiver().await;
    let mut event_rx3 = membership3.get_event_receiver().await;
    
    // Start membership protocols
    membership1.start().await?;
    membership2.start().await?;
    membership3.start().await?;
    
    // Add members to each other
    let member1 = create_test_member("node-1", "192.168.1.1:7962", NodeState::Alive);
    let member2 = create_test_member("node-2", "192.168.1.2:7963", NodeState::Alive);
    let member3 = create_test_member("node-3", "192.168.1.3:7964", NodeState::Alive);
    
    membership1.add_member(member2.clone()).await?;
    membership1.add_member(member3.clone()).await?;
    
    membership2.add_member(member1.clone()).await?;
    membership2.add_member(member3.clone()).await?;
    
    membership3.add_member(member1.clone()).await?;
    membership3.add_member(member2.clone()).await?;
    
    // Make node-1 the leader
    let mut leader_member = member1.clone();
    leader_member.is_leader = true;
    
    membership1.add_member(leader_member.clone()).await?;
    membership2.add_member(leader_member.clone()).await?;
    membership3.add_member(leader_member.clone()).await?;
    
    // Wait for leader election to propagate
    sleep(Duration::from_millis(200)).await;
    
    // Verify node-1 is the leader
    let members1 = membership1.get_all_members().await;
    let members2 = membership2.get_all_members().await;
    let members3 = membership3.get_all_members().await;
    
    let node1_in_list1 = members1.iter().find(|m| m.id == "node-1").unwrap();
    let node1_in_list2 = members2.iter().find(|m| m.id == "node-1").unwrap();
    let node1_in_list3 = members3.iter().find(|m| m.id == "node-1").unwrap();
    
    assert!(node1_in_list1.is_leader);
    assert!(node1_in_list2.is_leader);
    assert!(node1_in_list3.is_leader);
    
    // Simulate leader failure
    println!("Simulating leader failure...");
    
    // Mark node-1 as dead in node-2 and node-3
    let mut dead_leader = member1.clone();
    dead_leader.state = NodeState::Dead;
    dead_leader.is_leader = false;
    
    membership2.add_member(dead_leader.clone()).await?;
    membership3.add_member(dead_leader.clone()).await?;
    
    // Wait for leader election
    sleep(Duration::from_millis(500)).await;
    
    // Verify a new leader was elected (either node-2 or node-3)
    let members2 = membership2.get_all_members().await;
    let members3 = membership3.get_all_members().await;
    
    let node2_in_list2 = members2.iter().find(|m| m.id == "node-2").unwrap();
    let node3_in_list3 = members3.iter().find(|m| m.id == "node-3").unwrap();
    
    // Either node-2 or node-3 should be the new leader
    assert!(node2_in_list2.is_leader || node3_in_list3.is_leader);
    
    // Check for leader elected event
    let mut found_leader_event = false;
    while let Ok(event) = event_rx2.try_recv() {
        if let MembershipEvent::LeaderElected(_) = event {
            found_leader_event = true;
            break;
        }
    }
    
    if !found_leader_event {
        while let Ok(event) = event_rx3.try_recv() {
            if let MembershipEvent::LeaderElected(_) = event {
                found_leader_event = true;
                break;
            }
        }
    }
    
    assert!(found_leader_event, "Leader elected event not found");
    
    Ok(())
}