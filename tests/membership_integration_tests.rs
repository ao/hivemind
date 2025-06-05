use anyhow::Result;
use hivemind::membership::{
    Member, MembershipConfig, MembershipEvent, MembershipProtocol, Message, MessageType, NodeState,
};
use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tokio::sync::mpsc;
use tokio::time::sleep;

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

// Helper function to create a test message
fn create_test_message(
    message_type: MessageType,
    sender_id: &str,
    sender_addr: &str,
    target_id: Option<&str>,
    members: Option<Vec<Member>>,
) -> Message {
    Message {
        message_type,
        sender_id: sender_id.to_string(),
        sender_addr: sender_addr.to_string(),
        target_id: target_id.map(|s| s.to_string()),
        incarnation: 1,
        members,
    }
}

#[tokio::test]
async fn test_leader_election_with_multiple_nodes() -> Result<()> {
    // Create three membership protocol instances
    let node1_id = "test-node-1";
    let node1_addr = "127.0.0.1:7960";
    
    let node2_id = "test-node-2";
    let node2_addr = "127.0.0.1:7961";
    
    let node3_id = "test-node-3";
    let node3_addr = "127.0.0.1:7962";
    
    // Use custom configs with different ports
    let mut config1 = MembershipConfig::default();
    config1.bind_port = 7960;
    config1.leader_check_interval = Duration::from_millis(100);
    
    let mut config2 = MembershipConfig::default();
    config2.bind_port = 7961;
    config2.leader_check_interval = Duration::from_millis(100);
    
    let mut config3 = MembershipConfig::default();
    config3.bind_port = 7962;
    config3.leader_check_interval = Duration::from_millis(100);
    
    let membership1 = MembershipProtocol::new(node1_id.to_string(), node1_addr.to_string(), Some(config1)).await?;
    let membership2 = MembershipProtocol::new(node2_id.to_string(), node2_addr.to_string(), Some(config2)).await?;
    let membership3 = MembershipProtocol::new(node3_id.to_string(), node3_addr.to_string(), Some(config3)).await?;
    
    // Get event receivers
    let mut event_rx1 = membership1.get_event_receiver().await;
    let mut event_rx2 = membership2.get_event_receiver().await;
    let mut event_rx3 = membership3.get_event_receiver().await;
    
    // Start all protocols
    membership1.start().await?;
    membership2.start().await?;
    membership3.start().await?;
    
    // Manually simulate nodes joining each other's clusters
    let node1_member = create_test_member(node1_id, node1_addr, NodeState::Alive);
    let node2_member = create_test_member(node2_id, node2_addr, NodeState::Alive);
    let node3_member = create_test_member(node3_id, node3_addr, NodeState::Alive);
    
    // Add members to each other
    membership1.add_member(node2_member.clone()).await?;
    membership1.add_member(node3_member.clone()).await?;
    
    membership2.add_member(node1_member.clone()).await?;
    membership2.add_member(node3_member.clone()).await?;
    
    membership3.add_member(node1_member.clone()).await?;
    membership3.add_member(node2_member.clone()).await?;
    
    // Trigger leader election on node1
    membership1.trigger_leader_election().await?;
    
    // Wait for leader election
    sleep(Duration::from_millis(500)).await;
    
    // Check if a leader was elected
    let leader1 = membership1.get_leader().await;
    let leader2 = membership2.get_leader().await;
    let leader3 = membership3.get_leader().await;
    
    // All nodes should agree on the same leader
    assert!(leader1.is_some());
    assert!(leader2.is_some());
    assert!(leader3.is_some());
    
    let leader_id = leader1.unwrap().id;
    assert_eq!(leader2.unwrap().id, leader_id);
    assert_eq!(leader3.unwrap().id, leader_id);
    
    // Verify the leader is marked as leader
    let is_leader1 = membership1.is_leader().await;
    let is_leader2 = membership2.is_leader().await;
    let is_leader3 = membership3.is_leader().await;
    
    // Only one node should be the leader
    assert_eq!(is_leader1 || is_leader2 || is_leader3, true);
    assert_eq!(is_leader1 && is_leader2, false);
    assert_eq!(is_leader1 && is_leader3, false);
    assert_eq!(is_leader2 && is_leader3, false);
    
    Ok(())
}

#[tokio::test]
async fn test_distributed_state_management() -> Result<()> {
    // Create two membership protocol instances
    let node1_id = "test-node-1";
    let node1_addr = "127.0.0.1:7963";
    
    let node2_id = "test-node-2";
    let node2_addr = "127.0.0.1:7964";
    
    // Use custom configs with different ports
    let mut config1 = MembershipConfig::default();
    config1.bind_port = 7963;
    config1.leader_check_interval = Duration::from_millis(100);
    
    let mut config2 = MembershipConfig::default();
    config2.bind_port = 7964;
    config2.leader_check_interval = Duration::from_millis(100);
    
    let membership1 = MembershipProtocol::new(node1_id.to_string(), node1_addr.to_string(), Some(config1)).await?;
    let membership2 = MembershipProtocol::new(node2_id.to_string(), node2_addr.to_string(), Some(config2)).await?;
    
    // Start both protocols
    membership1.start().await?;
    membership2.start().await?;
    
    // Manually simulate nodes joining each other's clusters
    let node1_member = create_test_member(node1_id, node1_addr, NodeState::Alive);
    let node2_member = create_test_member(node2_id, node2_addr, NodeState::Alive);
    
    // Add members to each other
    membership1.add_member(node2_member.clone()).await?;
    membership2.add_member(node1_member.clone()).await?;
    
    // Trigger leader election on node1
    membership1.trigger_leader_election().await?;
    
    // Wait for leader election
    sleep(Duration::from_millis(500)).await;
    
    // Store state on the leader
    let key = "test-key";
    let value = b"test-value";
    
    if membership1.is_leader().await {
        membership1.store_state(key, value).await?;
    } else {
        membership2.store_state(key, value).await?;
    }
    
    // Wait for state to propagate
    sleep(Duration::from_millis(500)).await;
    
    // Both nodes should have the state
    let state1 = membership1.get_state(key).await;
    let state2 = membership2.get_state(key).await;
    
    assert!(state1.is_some());
    assert!(state2.is_some());
    assert_eq!(state1.unwrap(), value);
    assert_eq!(state2.unwrap(), value);
    
    Ok(())
}

#[tokio::test]
async fn test_node_failure_detection() -> Result<()> {
    // Create two membership protocol instances
    let node1_id = "test-node-1";
    let node1_addr = "127.0.0.1:7965";
    
    let node2_id = "test-node-2";
    let node2_addr = "127.0.0.1:7966";
    
    // Use custom configs with different ports and shorter timeouts
    let mut config1 = MembershipConfig::default();
    config1.bind_port = 7965;
    config1.protocol_period = Duration::from_millis(100);
    config1.ping_timeout = Duration::from_millis(50);
    config1.suspicion_mult = 2;
    
    let mut config2 = MembershipConfig::default();
    config2.bind_port = 7966;
    config2.protocol_period = Duration::from_millis(100);
    config2.ping_timeout = Duration::from_millis(50);
    config2.suspicion_mult = 2;
    
    let membership1 = MembershipProtocol::new(node1_id.to_string(), node1_addr.to_string(), Some(config1)).await?;
    let membership2 = MembershipProtocol::new(node2_id.to_string(), node2_addr.to_string(), Some(config2)).await?;
    
    // Get event receiver for node1
    let mut event_rx1 = membership1.get_event_receiver().await;
    
    // Start both protocols
    membership1.start().await?;
    membership2.start().await?;
    
    // Manually simulate nodes joining each other's clusters
    let node1_member = create_test_member(node1_id, node1_addr, NodeState::Alive);
    let node2_member = create_test_member(node2_id, node2_addr, NodeState::Alive);
    
    // Add members to each other
    membership1.add_member(node2_member.clone()).await?;
    membership2.add_member(node1_member.clone()).await?;
    
    // Wait for a bit to establish connection
    sleep(Duration::from_millis(200)).await;
    
    // Manually mark node2 as suspected
    membership1.suspect_member(node2_id).await?;
    
    // Wait for suspicion timeout
    sleep(Duration::from_millis(500)).await;
    
    // Check if node2 is marked as dead
    let members = membership1.get_all_members().await;
    let node2_in_list = members.iter().find(|m| m.id == node2_id).unwrap();
    assert_eq!(node2_in_list.state, NodeState::Dead);
    
    // Check for events
    let mut found_suspected_event = false;
    let mut found_dead_event = false;
    
    while let Ok(event) = event_rx1.try_recv() {
        match event {
            MembershipEvent::MemberSuspected(member) => {
                if member.id == node2_id {
                    found_suspected_event = true;
                }
            }
            MembershipEvent::MemberDead(member) => {
                if member.id == node2_id {
                    found_dead_event = true;
                }
            }
            _ => {}
        }
    }
    
    assert!(found_suspected_event, "Suspected event not found");
    assert!(found_dead_event, "Dead event not found");
    
    Ok(())
}

#[tokio::test]
async fn test_quorum_detection() -> Result<()> {
    // Create three membership protocol instances
    let node1_id = "test-node-1";
    let node1_addr = "127.0.0.1:7967";
    
    let node2_id = "test-node-2";
    let node2_addr = "127.0.0.1:7968";
    
    let node3_id = "test-node-3";
    let node3_addr = "127.0.0.1:7969";
    
    // Use custom configs with different ports
    let mut config1 = MembershipConfig::default();
    config1.bind_port = 7967;
    config1.protocol_period = Duration::from_millis(100);
    
    let mut config2 = MembershipConfig::default();
    config2.bind_port = 7968;
    config2.protocol_period = Duration::from_millis(100);
    
    let mut config3 = MembershipConfig::default();
    config3.bind_port = 7969;
    config3.protocol_period = Duration::from_millis(100);
    
    let membership1 = MembershipProtocol::new(node1_id.to_string(), node1_addr.to_string(), Some(config1)).await?;
    let membership2 = MembershipProtocol::new(node2_id.to_string(), node2_addr.to_string(), Some(config2)).await?;
    let membership3 = MembershipProtocol::new(node3_id.to_string(), node3_addr.to_string(), Some(config3)).await?;
    
    // Get event receiver for node1
    let mut event_rx1 = membership1.get_event_receiver().await;
    
    // Start all protocols
    membership1.start().await?;
    membership2.start().await?;
    membership3.start().await?;
    
    // Manually simulate nodes joining each other's clusters
    let node1_member = create_test_member(node1_id, node1_addr, NodeState::Alive);
    let node2_member = create_test_member(node2_id, node2_addr, NodeState::Alive);
    let node3_member = create_test_member(node3_id, node3_addr, NodeState::Alive);
    
    // Add members to each other
    membership1.add_member(node2_member.clone()).await?;
    membership1.add_member(node3_member.clone()).await?;
    
    membership2.add_member(node1_member.clone()).await?;
    membership2.add_member(node3_member.clone()).await?;
    
    membership3.add_member(node1_member.clone()).await?;
    membership3.add_member(node2_member.clone()).await?;
    
    // Wait for a bit to establish connection
    sleep(Duration::from_millis(200)).await;
    
    // Mark both node2 and node3 as dead
    membership1.suspect_member(node2_id).await?;
    membership1.suspect_member(node3_id).await?;
    
    // Wait for suspicion timeout and quorum check
    sleep(Duration::from_millis(500)).await;
    
    // Check for quorum lost event
    let mut found_quorum_lost_event = false;
    let mut found_partition_event = false;
    
    while let Ok(event) = event_rx1.try_recv() {
        match event {
            MembershipEvent::QuorumLost => {
                found_quorum_lost_event = true;
            }
            MembershipEvent::NetworkPartitionDetected => {
                found_partition_event = true;
            }
            _ => {}
        }
    }
    
    assert!(found_quorum_lost_event, "Quorum lost event not found");
    assert!(found_partition_event, "Network partition event not found");
    
    Ok(())
}