use anyhow::Result;
use hivemind::app::AppManager;
use hivemind::containerd_manager::{Container, ContainerStatus};
use hivemind::health_monitor::{HealthCheckConfig, HealthMonitor, ResourceThresholds, RestartPolicy};
use hivemind::membership::{Member, MembershipConfig, MembershipEvent, MembershipProtocol, NodeState};
use hivemind::network::{NetworkManager, NetworkPolicy, NetworkRule, NetworkSelector, PortRange, Protocol, NetworkPeer, PolicyAction};
use hivemind::node::{NodeManager, NodeResources};
use hivemind::scheduler::{BinPackingStrategy, ContainerScheduler};
use hivemind::service_discovery::ServiceDiscovery;
use hivemind::storage::StorageManager;
use ipnetwork::IpNetwork;
use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr};
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tempfile::TempDir;
use tokio::sync::Mutex;
use tokio::time::sleep;
use rand::{thread_rng, Rng};
use rand::seq::SliceRandom;

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
    
    async fn add_many_nodes(&self, count: usize, base_resources: &NodeResources) {
        let mut nodes = self.nodes.lock().await;
        for i in 0..count {
            let node_id = format!("node-{}", i + 2); // Start from node-2
            let address = format!("192.168.1.{}", i + 2);
            nodes.insert(node_id, (address, base_resources.clone()));
        }
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

// Enhanced chaos test for cascading node failures
#[tokio::test]
async fn test_cascading_node_failures() -> Result<()> {
    println!("Starting cascading node failures test");
    
    // Create components
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(MockNodeManager::new("node-1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Add multiple nodes (10 nodes total)
    let base_resources = NodeResources {
        cpu_total: 8.0,
        cpu_available: 8.0,
        memory_total: 16 * 1024 * 1024 * 1024,
        memory_available: 16 * 1024 * 1024 * 1024,
        disk_total: 100 * 1024 * 1024 * 1024,
        disk_available: 100 * 1024 * 1024 * 1024,
    };
    
    node_manager.add_many_nodes(9, &base_resources).await;
    
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
        restart_policy: RestartPolicy::OnFailure,
        resource_thresholds: ResourceThresholds::default(),
    };
    
    let health_monitor = Arc::new(HealthMonitor::with_config(
        app_manager.clone(),
        node_manager.clone(),
        service_discovery.clone(),
        custom_config,
    ));
    
    // Start health monitor
    health_monitor.start().await?;
    
    // Deploy containers across all nodes (3 containers per node)
    println!("Deploying containers across all nodes");
    let mut container_ids = Vec::new();
    
    for i in 0..30 {
        let node_id = format!("node-{}", (i % 10) + 1);
        let container = create_test_container(
            &format!("container-{}", i),
            &format!("app-{}", i),
            &node_id,
        );
        
        app_manager.add_test_container(container).await?;
        container_ids.push(format!("container-{}", i));
    }
    
    // Verify initial state
    let nodes = node_manager.list_nodes().await?;
    assert_eq!(nodes.len(), 10);
    
    let containers = app_manager.get_container_details().await?;
    assert_eq!(containers.len(), 30);
    
    // Simulate cascading node failures (fail 5 nodes in sequence)
    println!("Simulating cascading node failures");
    
    for i in 2..7 {
        println!("Failing node-{}", i);
        node_manager.remove_node(&format!("node-{}", i)).await;
        
        // Wait for health monitor to detect failure
        sleep(Duration::from_millis(500)).await;
        
        // Verify node is removed
        let nodes = node_manager.list_nodes().await?;
        assert!(!nodes.contains(&format!("node-{}", i)));
        
        // Verify containers on failed node are marked as failed
        for j in 0..30 {
            if j % 10 + 1 == i {
                let container = app_manager.get_container(&format!("container-{}", j)).await?;
                assert_eq!(container.status, ContainerStatus::Failed);
            }
        }
    }
    
    // Verify system is still operational
    let remaining_nodes = node_manager.list_nodes().await?;
    assert_eq!(remaining_nodes.len(), 5);
    
    // Verify containers on remaining nodes are still running
    for j in 0..30 {
        let node_id = j % 10 + 1;
        if node_id != 2 && node_id != 3 && node_id != 4 && node_id != 5 && node_id != 6 {
            let container = app_manager.get_container(&format!("container-{}", j)).await?;
            assert_eq!(container.status, ContainerStatus::Running);
        }
    }
    
    // Simulate node recovery (bring back 3 nodes)
    println!("Simulating node recovery");
    
    for i in 2..5 {
        println!("Recovering node-{}", i);
        node_manager.add_node(
            &format!("node-{}", i),
            &format!("192.168.1.{}", i),
            base_resources.clone(),
        ).await;
        
        // Wait for recovery
        sleep(Duration::from_millis(500)).await;
    }
    
    // Verify recovered nodes
    let recovered_nodes = node_manager.list_nodes().await?;
    assert_eq!(recovered_nodes.len(), 8);
    assert!(recovered_nodes.contains(&"node-2".to_string()));
    assert!(recovered_nodes.contains(&"node-3".to_string()));
    assert!(recovered_nodes.contains(&"node-4".to_string()));
    
    // Stop health monitor
    health_monitor.stop().await;
    
    println!("Cascading node failures test completed successfully");
    Ok(())
}

// Enhanced chaos test for network partitions with recovery
#[tokio::test]
async fn test_network_partition_with_recovery() -> Result<()> {
    println!("Starting network partition with recovery test");
    
    // Create components for a 3-node cluster
    let temp_dir1 = TempDir::new().unwrap();
    let temp_dir2 = TempDir::new().unwrap();
    let temp_dir3 = TempDir::new().unwrap();
    
    let storage1 = StorageManager::new(temp_dir1.path()).await?;
    let storage2 = StorageManager::new(temp_dir2.path()).await?;
    let storage3 = StorageManager::new(temp_dir3.path()).await?;
    
    let mut node1 = NodeManager::with_storage(storage1.clone()).await;
    let mut node2 = NodeManager::with_storage(storage2.clone()).await;
    let mut node3 = NodeManager::with_storage(storage3.clone()).await;
    
    // Initialize membership protocols
    let mut config1 = MembershipConfig::default();
    config1.bind_port = 7971;
    config1.protocol_period = Duration::from_millis(100);
    config1.quorum_percentage = 0.51; // >50% for quorum
    
    let mut config2 = config1.clone();
    config2.bind_port = 7972;
    
    let mut config3 = config1.clone();
    config3.bind_port = 7973;
    
    node1.init_membership_protocol_with_config(Some(config1)).await?;
    node2.init_membership_protocol_with_config(Some(config2)).await?;
    node3.init_membership_protocol_with_config(Some(config3)).await?;
    
    // Get node IDs and addresses
    let node1_id = node1.get_node_id();
    let node2_id = node2.get_node_id();
    let node3_id = node3.get_node_id();
    
    // Get node details
    let node1_details = node1.get_node_details().await?;
    let node2_details = node2.get_node_details().await?;
    let node3_details = node3.get_node_details().await?;
    
    let node1_addr = node1_details.iter().find(|(id, _, _)| id == &node1_id).unwrap().1.clone();
    
    println!("Node 1: {} at {}", node1_id, node1_addr);
    println!("Node 2: {} at {}", node2_id, node2_details.iter().find(|(id, _, _)| id == &node2_id).unwrap().1);
    println!("Node 3: {} at {}", node3_id, node3_details.iter().find(|(id, _, _)| id == &node3_id).unwrap().1);
    
    // Node 2 and 3 join Node 1's cluster
    node2.join_cluster(&node1_addr).await?;
    node3.join_cluster(&node1_addr).await?;
    
    // Wait for cluster to stabilize
    sleep(Duration::from_millis(500)).await;
    
    // Verify all nodes see each other
    let node1_peers = node1.list_nodes().await?;
    let node2_peers = node2.list_nodes().await?;
    let node3_peers = node3.list_nodes().await?;
    
    assert!(node1_peers.contains(&node2_id));
    assert!(node1_peers.contains(&node3_id));
    assert!(node2_peers.contains(&node1_id));
    assert!(node2_peers.contains(&node3_id));
    assert!(node3_peers.contains(&node1_id));
    assert!(node3_peers.contains(&node2_id));
    
    // Get event receivers
    let mut event_rx1 = node1.get_membership_events().await;
    let mut event_rx2 = node2.get_membership_events().await;
    let mut event_rx3 = node3.get_membership_events().await;
    
    // Simulate network partition: Node 1 and 2 can't see Node 3
    println!("Simulating network partition: Node 1 and 2 can't see Node 3");
    
    // Mark Node 3 as suspected from Node 1 and 2's perspective
    node1.suspect_node(&node3_id).await?;
    node2.suspect_node(&node3_id).await?;
    
    // Wait for suspicion to propagate
    sleep(Duration::from_millis(300)).await;
    
    // Verify Node 3 is suspected
    let node1_members = node1.get_all_members().await;
    let node2_members = node2.get_all_members().await;
    
    let node3_from_node1 = node1_members.iter().find(|m| m.id == node3_id).unwrap();
    let node3_from_node2 = node2_members.iter().find(|m| m.id == node3_id).unwrap();
    
    assert_eq!(node3_from_node1.state, NodeState::Suspected);
    assert_eq!(node3_from_node2.state, NodeState::Suspected);
    
    // Check for network partition event
    let mut found_partition_event = false;
    while let Ok(event) = event_rx1.try_recv() {
        if let MembershipEvent::NetworkPartitionDetected = event {
            found_partition_event = true;
            break;
        }
    }
    
    assert!(found_partition_event, "Network partition event not detected");
    
    // Simulate partition healing
    println!("Simulating partition healing");
    
    // Mark Node 3 as alive again
    let mut node3_member = node1_members.iter().find(|m| m.id == node3_id).unwrap().clone();
    node3_member.state = NodeState::Alive;
    
    node1.add_member(node3_member.clone()).await?;
    node2.add_member(node3_member).await?;
    
    // Wait for state to propagate
    sleep(Duration::from_millis(300)).await;
    
    // Verify Node 3 is alive
    let node1_members = node1.get_all_members().await;
    let node2_members = node2.get_all_members().await;
    
    let node3_from_node1 = node1_members.iter().find(|m| m.id == node3_id).unwrap();
    let node3_from_node2 = node2_members.iter().find(|m| m.id == node3_id).unwrap();
    
    assert_eq!(node3_from_node1.state, NodeState::Alive);
    assert_eq!(node3_from_node2.state, NodeState::Alive);
    
    // Check for partition resolved event
    let mut found_resolved_event = false;
    while let Ok(event) = event_rx1.try_recv() {
        if let MembershipEvent::NetworkPartitionResolved = event {
            found_resolved_event = true;
            break;
        }
    }
    
    assert!(found_resolved_event, "Network partition resolved event not detected");
    
    // Test distributed state consistency after partition
    println!("Testing distributed state consistency after partition");
    
    // Store state through Node 1
    let key = "post-partition-key";
    let value = b"post-partition-value";
    node1.store_distributed_state(key, value).await?;
    
    // Wait for state to propagate
    sleep(Duration::from_millis(300)).await;
    
    // All nodes should have the state
    let state1 = node1.get_distributed_state(key).await;
    let state2 = node2.get_distributed_state(key).await;
    let state3 = node3.get_distributed_state(key).await;
    
    assert!(state1.is_some());
    assert!(state2.is_some());
    assert!(state3.is_some());
    assert_eq!(state1.unwrap(), value);
    assert_eq!(state2.unwrap(), value);
    assert_eq!(state3.unwrap(), value);
    
    println!("Network partition with recovery test completed successfully");
    Ok(())
}

// Enhanced chaos test for leader failure and re-election
#[tokio::test]
async fn test_leader_failure_and_reelection() -> Result<()> {
    println!("Starting leader failure and re-election test");
    
    // Create components for a 5-node cluster
    let temp_dirs: Vec<TempDir> = (0..5).map(|_| TempDir::new().unwrap()).collect();
    let storages: Vec<StorageManager> = Vec::new();
    let mut nodes: Vec<NodeManager> = Vec::new();
    
    // Initialize storage and node managers
    for (i, temp_dir) in temp_dirs.iter().enumerate() {
        let storage = StorageManager::new(temp_dir.path()).await?;
        let mut node = NodeManager::with_storage(storage.clone()).await;
        
        // Initialize membership protocol
        let mut config = MembershipConfig::default();
        config.bind_port = 7980 + i as u16;
        config.protocol_period = Duration::from_millis(100);
        config.quorum_percentage = 0.51; // >50% for quorum
        
        node.init_membership_protocol_with_config(Some(config)).await?;
        nodes.push(node);
    }
    
    // Get node IDs and addresses
    let node_ids: Vec<String> = nodes.iter().map(|n| n.get_node_id()).collect();
    let node_details: Vec<Vec<(String, String, NodeResources)>> = Vec::new();
    
    for node in &nodes {
        node_details.push(node.get_node_details().await?);
    }
    
    // Get the first node's address
    let node0_addr = node_details[0].iter().find(|(id, _, _)| id == &node_ids[0]).unwrap().1.clone();
    
    // All other nodes join the first node's cluster
    for i in 1..5 {
        nodes[i].join_cluster(&node0_addr).await?;
    }
    
    // Wait for cluster to stabilize
    sleep(Duration::from_millis(500)).await;
    
    // Verify all nodes see each other
    for (i, node) in nodes.iter().enumerate() {
        let peers = node.list_nodes().await?;
        assert_eq!(peers.len(), 5);
        for j in 0..5 {
            if i != j {
                assert!(peers.contains(&node_ids[j]));
            }
        }
    }
    
    // Find the current leader
    let mut leader_idx = None;
    for (i, node) in nodes.iter().enumerate() {
        if node.is_cluster_leader().await {
            leader_idx = Some(i);
            break;
        }
    }
    
    assert!(leader_idx.is_some(), "No leader found");
    let leader_idx = leader_idx.unwrap();
    println!("Current leader is node {}", leader_idx);
    
    // Store some state through the leader
    let key = "leader-test-key";
    let value = b"leader-test-value";
    nodes[leader_idx].store_distributed_state(key, value).await?;
    
    // Wait for state to propagate
    sleep(Duration::from_millis(300)).await;
    
    // All nodes should have the state
    for (i, node) in nodes.iter().enumerate() {
        let state = node.get_distributed_state(key).await;
        assert!(state.is_some(), "Node {} missing state", i);
        assert_eq!(state.unwrap(), value, "Node {} has incorrect state", i);
    }
    
    // Simulate leader failure
    println!("Simulating leader failure");
    
    // Mark leader as failed from all other nodes' perspective
    for i in 0..5 {
        if i != leader_idx {
            nodes[i].suspect_node(&node_ids[leader_idx]).await?;
        }
    }
    
    // Wait for leader re-election
    sleep(Duration::from_millis(500)).await;
    
    // Find the new leader
    let mut new_leader_idx = None;
    for (i, node) in nodes.iter().enumerate() {
        if i != leader_idx && node.is_cluster_leader().await {
            new_leader_idx = Some(i);
            break;
        }
    }
    
    assert!(new_leader_idx.is_some(), "No new leader elected");
    let new_leader_idx = new_leader_idx.unwrap();
    println!("New leader is node {}", new_leader_idx);
    assert_ne!(leader_idx, new_leader_idx, "New leader is the same as the failed leader");
    
    // Store new state through the new leader
    let new_key = "new-leader-test-key";
    let new_value = b"new-leader-test-value";
    nodes[new_leader_idx].store_distributed_state(new_key, new_value).await?;
    
    // Wait for state to propagate
    sleep(Duration::from_millis(300)).await;
    
    // All non-failed nodes should have the new state
    for (i, node) in nodes.iter().enumerate() {
        if i != leader_idx {
            let state = node.get_distributed_state(new_key).await;
            assert!(state.is_some(), "Node {} missing new state", i);
            assert_eq!(state.unwrap(), new_value, "Node {} has incorrect new state", i);
        }
    }
    
    // Simulate leader recovery
    println!("Simulating leader recovery");
    
    // Mark old leader as alive again
    let mut leader_member = nodes[new_leader_idx].get_all_members().await
        .iter()
        .find(|m| m.id == node_ids[leader_idx])
        .unwrap()
        .clone();
    
    leader_member.state = NodeState::Alive;
    
    for i in 0..5 {
        if i != leader_idx {
            nodes[i].add_member(leader_member.clone()).await?;
        }
    }
    
    // Wait for state to propagate
    sleep(Duration::from_millis(500)).await;
    
    // Verify recovered leader is not the leader anymore
    assert!(!nodes[leader_idx].is_cluster_leader().await, "Recovered node should not be the leader");
    
    // Verify recovered leader has the new state
    let recovered_state = nodes[leader_idx].get_distributed_state(new_key).await;
    assert!(recovered_state.is_some(), "Recovered leader missing new state");
    assert_eq!(recovered_state.unwrap(), new_value, "Recovered leader has incorrect new state");
    
    println!("Leader failure and re-election test completed successfully");
    Ok(())
}

// Enhanced chaos test for random container failures
#[tokio::test]
async fn test_random_container_failures() -> Result<()> {
    println!("Starting random container failures test");
    
    // Create components
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(MockNodeManager::new("node-1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Add additional nodes
    let base_resources = NodeResources {
        cpu_total: 8.0,
        cpu_available: 8.0,
        memory_total: 16 * 1024 * 1024 * 1024,
        memory_available: 16 * 1024 * 1024 * 1024,
        disk_total: 100 * 1024 * 1024 * 1024,
        disk_available: 100 * 1024 * 1024 * 1024,
    };
    
    node_manager.add_many_nodes(4, &base_resources).await;
    
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
        restart_policy: RestartPolicy::OnFailure,
        resource_thresholds: ResourceThresholds::default(),
    };
    
    let health_monitor = Arc::new(HealthMonitor::with_config(
        app_manager.clone(),
        node_manager.clone(),
        service_discovery.clone(),
        custom_config,
    ));
    
    // Start health monitor
    health_monitor.start().await?;
    
    // Deploy containers across all nodes
    println!("Deploying containers across all nodes");
    let mut container_ids = Vec::new();
    
    for i in 0..50 {
        let node_id = format!("node-{}", (i % 5) + 1);
        let container = create_test_container(
            &format!("container-{}", i),
            &format!("app-{}", i),
            &node_id,
        );
        
        app_manager.add_test_container(container).await?;
        container_ids.push(format!("container-{}", i));
    }
    
    // Verify initial state
    let containers = app_manager.get_container_details().await?;
    assert_eq!(containers.len(), 50);
    assert!(containers.iter().all(|c| c.status == ContainerStatus::Running));
    
    // Simulate random container failures
    println!("Simulating random container failures");
    let mut rng = thread_rng();
    let mut failed_containers = Vec::new();
    
    // Fail 20% of containers randomly
    let failure_count = 10;
    let mut indices: Vec<usize> = (0..50).collect();
    indices.shuffle(&mut rng);
    
    for i in 0..failure_count {
        let idx = indices[i];
        let container_id = &container_ids[idx];
        println!("Failing container {}", container_id);
        app_manager.update_container_status(container_id, ContainerStatus::Failed).await?;
        failed_containers.push(container_id.clone());
    }
    
    // Wait for health monitor to detect and attempt restart
    sleep(Duration::from_millis(500)).await;
    
    // Verify containers are being restarted
    for container_id in &failed_containers {
        let container = app_manager.get_container(container_id).await?;
        assert!(container.status == ContainerStatus::Restarting || container.status == ContainerStatus::Running);
    }
    
    // Simulate successful restart for half of the failed containers
    for i in 0..failure_count/2 {
        let container_id = &failed_containers[i];
        app_manager.update_container_status(container_id, ContainerStatus::Running).await?;
    }
    
    // Wait for health monitor to detect recovery
    sleep(Duration::from_millis(500)).await;
    
    // Verify half of the containers are running again
    for i in 0..failure_count/2 {
        let container_id = &failed_containers[i];
        let container = app_manager.get_container(container_id).await?;
        assert_eq!(container.status, ContainerStatus::Running);
    }
    
    // Verify the other half are still being restarted
    for i in failure_count/2..failure_count {
        let container_id = &failed_containers[i];
        let container = app_manager.get_container(container_id).await?;
        assert!(container.status == ContainerStatus::Restarting || container.status == ContainerStatus::Failed);
    }
    
    // Stop health monitor
    health_monitor.stop().await;
    
    println!("Random container failures test completed successfully");
    Ok(())
}

// Enhanced chaos test for resource exhaustion
#[tokio::test]
async fn test_resource_exhaustion_recovery() -> Result<()> {
    println!("Starting resource exhaustion recovery test");
    
    // Create components
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(MockNodeManager::new("node-1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Add additional nodes
    let base_resources = NodeResources {
        cpu_total: 8.0,
        cpu_available: 8.0,
        memory_total: 16 * 1024 * 1024 * 1024,
        memory_available: 16 * 1024 * 1024 * 1024,
        disk_total: 100 * 1024 * 1024 * 1024,
        disk_available: 100 * 1024 * 1024 * 1024,
    };
    
    node_manager.add_many_nodes(2, &base_resources).await;
    
    // Create scheduler
    let scheduler = ContainerScheduler::new(app_manager.clone())
        .with_node_manager(node_manager.clone())
        .with_service_discovery(service_discovery.clone());
    
    // Create health monitor with resource thresholds
    let custom_config = HealthCheckConfig {
        check_interval_seconds: 1,
        failure_threshold: 2,
        restart_delay_seconds: 1,
        max_restart_attempts: 3,
        health_check_timeout_seconds: 1,
        custom_health_check_command: None,
        node_check_interval_seconds: 1,
        node_failure_threshold: 2,
        restart_policy: RestartPolicy::OnFailure,
        resource_thresholds: ResourceThresholds {
            cpu_warning_percent: 80.0,
            cpu_critical_percent: 90.0,
            memory_warning_percent: 80.0,
            memory_critical_percent: 90.0,
            disk_warning_percent: 85.0,
            disk_critical_percent: 95.0,
            network_warning_mbps: 800.0,
            network_critical_mbps: 950.0,
        },
    };
    
    let health_monitor = Arc::new(HealthMonitor::with_config(
        app_manager.clone(),
        node_manager.clone(),
        service_discovery.clone(),
        custom_config,
    ));
    
    // Start health monitor
    health_monitor.start().await?;
    
    // Deploy containers
    println!("Deploying containers");
    let mut container_ids = Vec::new();
    
    for i in 0..10 {
        let node_id = format!("node-{}", (i % 3) + 1);
        let container = create_test_container(
            &format!("container-{}", i),
            &format!("app-{}", i),
            &node_id,
        );
        
        app_manager.add_test_container(container).await?;
        container_ids.push(format!("container-{}", i));
    }
    
    // Verify initial state
    let containers = app_manager.get_container_details().await?;
    assert_eq!(containers.len(), 10);
    
    // Simulate resource exhaustion on node-2
    println!("Simulating resource exhaustion on node-2");
    let exhausted_resources = NodeResources {
        cpu_total: 8.0,
        cpu_available: 0.2, // 97.5% CPU usage
        memory_total: 16 * 1024 * 1024 * 1024,
        memory_available: 1 * 1024 * 1024 * 1024, // 93.75% memory usage
        disk_total: 100 * 1024 * 1024 * 1024,
        disk_available: 4 * 1024 * 1024 * 1024, // 96% disk usage
    };
    
    node_manager.update_node_resources("node-2", exhausted_resources).await;
    
    // Wait for health monitor to detect resource exhaustion
    sleep(Duration::from_millis(500)).await;
    
    // Get health summary
    let health_summary = health_monitor.get_health_summary().await;
    
    // Verify node-2 is marked as unhealthy
    let node2_health = health_summary.node_health.iter().find(|h| h.node_id == "node-2");
    assert!(node2_health.is_some());
    assert!(!node2_health.unwrap().is_healthy);
    
    // Verify scheduler avoids placing new containers on node-2
    println!("Verifying scheduler avoids exhausted node");
    let new_container = create_test_container(
        "new-container",
        "new-app",
        "pending", // Not assigned to a node yet
    );
    
    let scheduled_node = scheduler.schedule_container(&new_container).await?;
    assert_ne!(scheduled_node, "node-2", "Scheduler should avoid exhausted node");
    
    // Simulate resource recovery on node-2
    println!("Simulating resource recovery on node-2");
    node_manager.update_node_resources("node-2", base_resources.clone()).await;
    
    // Wait for health monitor to detect recovery
    sleep(Duration::from_millis(500)).await;
    
    // Get updated health summary
    let health_summary = health_monitor.get_health_summary().await;
    
    // Verify node-2 is marked as healthy again
    let node2_health = health_summary.node_health.iter().find(|h| h.node_id == "node-2");
    assert!(node2_health.is_some());
    assert!(node2_health.unwrap().is_healthy);
    
    // Verify scheduler can place containers on node-2 again
    let another_container = create_test_container(
        "another-container",
        "another-app",
        "pending", // Not assigned to a node yet
    );
    
    // Force scheduler to use node-2 by making it the only viable option
    node_manager.update_node_resources("node-1", exhausted_resources.clone()).await;
    node_manager.update_node_resources("node-3", exhausted_resources.clone()).await;
    node_manager.update_node_resources("node-2", base_resources.clone()).await;
    
    let scheduled_node = scheduler.schedule_container(&another_container).await?;
    assert_eq!(scheduled_node, "node-2", "Scheduler should use recovered node");
    
    // Stop health monitor
    health_monitor.stop().await;
    
    println!("Resource exhaustion recovery test completed successfully");
    Ok(())
}