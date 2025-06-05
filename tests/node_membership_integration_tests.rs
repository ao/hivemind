use anyhow::Result;
use hivemind::membership::{MembershipEvent, NodeState};
use hivemind::node::NodeManager;
use hivemind::network::NetworkManager;
use hivemind::storage::StorageManager;
use std::sync::Arc;
use std::time::Duration;
use tempfile::TempDir;
use tokio::time::sleep;

#[tokio::test]
async fn test_node_membership_integration() -> Result<()> {
    // Create temporary directories for storage
    let temp_dir1 = TempDir::new().unwrap();
    let temp_dir2 = TempDir::new().unwrap();
    
    // Create storage managers
    let storage1 = StorageManager::new(temp_dir1.path()).await?;
    let storage2 = StorageManager::new(temp_dir2.path()).await?;
    
    // Create node managers
    let mut node1 = NodeManager::with_storage(storage1.clone()).await;
    let mut node2 = NodeManager::with_storage(storage2.clone()).await;
    
    // Initialize membership protocols
    node1.init_membership_protocol().await?;
    node2.init_membership_protocol().await?;
    
    // Get node IDs and addresses
    let node1_id = node1.get_node_id();
    let node2_id = node2.get_node_id();
    
    // Get node details
    let node1_details = node1.get_node_details().await?;
    let node2_details = node2.get_node_details().await?;
    
    let node1_addr = node1_details.iter().find(|(id, _, _)| id == &node1_id).unwrap().1.clone();
    let node2_addr = node2_details.iter().find(|(id, _, _)| id == &node2_id).unwrap().1.clone();
    
    println!("Node 1: {} at {}", node1_id, node1_addr);
    println!("Node 2: {} at {}", node2_id, node2_addr);
    
    // Node 2 joins Node 1's cluster
    node2.join_cluster(&node1_addr).await?;
    
    // Wait for cluster to stabilize
    sleep(Duration::from_millis(500)).await;
    
    // Verify both nodes see each other
    let node1_peers = node1.list_nodes().await?;
    let node2_peers = node2.list_nodes().await?;
    
    assert!(node1_peers.contains(&node2_id));
    assert!(node2_peers.contains(&node1_id));
    
    // Check leader election
    sleep(Duration::from_millis(500)).await;
    
    let node1_is_leader = node1.is_cluster_leader().await;
    let node2_is_leader = node2.is_cluster_leader().await;
    
    // Only one node should be the leader
    assert_ne!(node1_is_leader, node2_is_leader);
    
    // Get the leader from both nodes' perspective
    let leader1 = node1.get_cluster_leader().await;
    let leader2 = node2.get_cluster_leader().await;
    
    // Both nodes should agree on the leader
    assert!(leader1.is_some());
    assert!(leader2.is_some());
    assert_eq!(leader1.unwrap().id, leader2.unwrap().id);
    
    // Test distributed state management
    let key = "test-key";
    let value = b"test-value";
    
    // Store state through the leader
    if node1_is_leader {
        node1.store_distributed_state(key, value).await?;
    } else {
        node2.store_distributed_state(key, value).await?;
    }
    
    // Wait for state to propagate
    sleep(Duration::from_millis(500)).await;
    
    // Both nodes should have the state
    let state1 = node1.get_distributed_state(key).await;
    let state2 = node2.get_distributed_state(key).await;
    
    assert!(state1.is_some());
    assert!(state2.is_some());
    assert_eq!(state1.unwrap(), value);
    assert_eq!(state2.unwrap(), value);
    
    // Test graceful leaving
    node2.leave_cluster().await?;
    
    // Wait for leave to propagate
    sleep(Duration::from_millis(500)).await;
    
    // Node 1 should no longer see Node 2
    let node1_peers = node1.list_nodes().await?;
    assert!(!node1_peers.contains(&node2_id));
    
    Ok(())
}

#[tokio::test]
async fn test_node_membership_with_network_manager() -> Result<()> {
    // Create temporary directories for storage
    let temp_dir1 = TempDir::new().unwrap();
    let temp_dir2 = TempDir::new().unwrap();
    
    // Create storage managers
    let storage1 = StorageManager::new(temp_dir1.path()).await?;
    let storage2 = StorageManager::new(temp_dir2.path()).await?;
    
    // Create node managers
    let mut node1 = NodeManager::with_storage(storage1.clone()).await;
    let mut node2 = NodeManager::with_storage(storage2.clone()).await;
    
    // Initialize membership protocols
    node1.init_membership_protocol().await?;
    node2.init_membership_protocol().await?;
    
    // Create network managers
    let network_manager1 = NetworkManager::new(
        Arc::new(node1.clone()),
        Arc::new(hivemind::service_discovery::ServiceDiscovery::new()),
        None,
    ).await?;
    
    let network_manager2 = NetworkManager::new(
        Arc::new(node2.clone()),
        Arc::new(hivemind::service_discovery::ServiceDiscovery::new()),
        None,
    ).await?;
    
    // Set network managers
    node1.set_network_manager(Arc::new(network_manager1.clone())).await;
    node2.set_network_manager(Arc::new(network_manager2.clone())).await;
    
    // Get node details
    let node1_details = node1.get_node_details().await?;
    let node2_details = node2.get_node_details().await?;
    
    let node1_id = node1.get_node_id();
    let node2_id = node2.get_node_id();
    
    let node1_addr = node1_details.iter().find(|(id, _, _)| id == &node1_id).unwrap().1.clone();
    let node2_addr = node2_details.iter().find(|(id, _, _)| id == &node2_id).unwrap().1.clone();
    
    // Node 2 joins Node 1's cluster
    node2.join_cluster(&node1_addr).await?;
    
    // Wait for cluster to stabilize
    sleep(Duration::from_millis(500)).await;
    
    // Verify both nodes see each other
    let node1_peers = node1.list_nodes().await?;
    let node2_peers = node2.list_nodes().await?;
    
    assert!(node1_peers.contains(&node2_id));
    assert!(node2_peers.contains(&node1_id));
    
    // Verify network managers are updated
    let network1_nodes = network_manager1.get_nodes().await;
    let network2_nodes = network_manager2.get_nodes().await;
    
    assert!(network1_nodes.contains(&node2_id));
    assert!(network2_nodes.contains(&node1_id));
    
    // Test node leaving
    node2.leave_cluster().await?;
    
    // Wait for leave to propagate
    sleep(Duration::from_millis(500)).await;
    
    // Node 1 should no longer see Node 2
    let node1_peers = node1.list_nodes().await?;
    assert!(!node1_peers.contains(&node2_id));
    
    // Network manager should be updated
    let network1_nodes = network_manager1.get_nodes().await;
    assert!(!network1_nodes.contains(&node2_id));
    
    Ok(())
}

#[tokio::test]
async fn test_node_health_with_membership() -> Result<()> {
    // Create temporary directory for storage
    let temp_dir = TempDir::new().unwrap();
    
    // Create storage manager
    let storage = StorageManager::new(temp_dir.path()).await?;
    
    // Create node manager
    let mut node = NodeManager::with_storage(storage.clone()).await;
    
    // Initialize membership protocol
    node.init_membership_protocol().await?;
    
    // Check node health
    let is_healthy = node.check_health().await?;
    assert!(is_healthy);
    
    // Get node details
    let node_details = node.get_node_details().await?;
    
    // Verify resources are updated
    let node_id = node.get_node_id();
    let node_info = node_details.iter().find(|(id, _, _)| id == &node_id).unwrap();
    
    // CPU and memory should be available
    assert!(node_info.2.cpu_available > 0.0);
    assert!(node_info.2.memory_available > 0);
    
    Ok(())
}