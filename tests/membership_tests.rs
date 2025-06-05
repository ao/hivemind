use anyhow::Result;
use hivemind::membership::{
    Member, MembershipConfig, MembershipEvent, MembershipProtocol, Message, MessageType, NodeState,
};
use std::collections::HashMap;
use std::net::SocketAddr;
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
async fn test_membership_initialization() -> Result<()> {
    // Create a membership protocol instance
    let node_id = "test-node-1";
    let addr = "127.0.0.1:7946";
    
    // Use a custom config with a different port to avoid conflicts
    let mut config = MembershipConfig::default();
    config.bind_port = 7947;
    
    let membership = MembershipProtocol::new(node_id.to_string(), addr.to_string(), Some(config)).await?;
    
    // Verify the node is initialized with itself as the only member
    let members = membership.get_all_members().await;
    assert_eq!(members.len(), 1);
    assert_eq!(members[0].id, node_id);
    assert_eq!(members[0].address, addr);
    assert_eq!(members[0].state, NodeState::Alive);
    
    Ok(())
}

#[tokio::test]
async fn test_membership_event_receiver() -> Result<()> {
    // Create a membership protocol instance
    let node_id = "test-node-1";
    let addr = "127.0.0.1:7946";
    
    // Use a custom config with a different port to avoid conflicts
    let mut config = MembershipConfig::default();
    config.bind_port = 7948;
    
    let membership = MembershipProtocol::new(node_id.to_string(), addr.to_string(), Some(config)).await?;
    
    // Get the event receiver
    let mut event_rx = membership.get_event_receiver().await;
    
    // Start the protocol
    membership.start().await?;
    
    // Wait a bit for initialization events
    sleep(Duration::from_millis(100)).await;
    
    // There should be at least one event (the node joining)
    if let Some(event) = event_rx.try_recv().ok() {
        match event {
            MembershipEvent::MemberJoined(member) => {
                assert_eq!(member.id, node_id);
                assert_eq!(member.address, addr);
            }
            _ => panic!("Expected MemberJoined event"),
        }
    }
    
    Ok(())
}

#[tokio::test]
async fn test_membership_state_storage() -> Result<()> {
    // Create a membership protocol instance
    let node_id = "test-node-1";
    let addr = "127.0.0.1:7946";
    
    // Use a custom config with a different port to avoid conflicts
    let mut config = MembershipConfig::default();
    config.bind_port = 7949;
    
    let membership = MembershipProtocol::new(node_id.to_string(), addr.to_string(), Some(config)).await?;
    
    // Store some state
    let key = "test-key";
    let value = b"test-value";
    membership.store_state(key, value).await?;
    
    // Retrieve the state
    let retrieved = membership.get_state(key).await;
    assert!(retrieved.is_some());
    assert_eq!(retrieved.unwrap(), value);
    
    // Store another state
    let key2 = "test-key-2";
    let value2 = b"test-value-2";
    membership.store_state(key2, value2).await?;
    
    // Retrieve the second state
    let retrieved2 = membership.get_state(key2).await;
    assert!(retrieved2.is_some());
    assert_eq!(retrieved2.unwrap(), value2);
    
    // Verify first state is still there
    let retrieved1 = membership.get_state(key).await;
    assert!(retrieved1.is_some());
    assert_eq!(retrieved1.unwrap(), value);
    
    Ok(())
}

#[tokio::test]
async fn test_membership_maintenance_mode() -> Result<()> {
    // Create a membership protocol instance
    let node_id = "test-node-1";
    let addr = "127.0.0.1:7946";
    
    // Use a custom config with a different port to avoid conflicts
    let mut config = MembershipConfig::default();
    config.bind_port = 7950;
    
    let membership = MembershipProtocol::new(node_id.to_string(), addr.to_string(), Some(config)).await?;
    
    // Get the event receiver
    let mut event_rx = membership.get_event_receiver().await;
    
    // Start the protocol
    membership.start().await?;
    
    // Enter maintenance mode
    membership.enter_maintenance_mode().await?;
    
    // Wait a bit for the event
    sleep(Duration::from_millis(100)).await;
    
    // There should be a maintenance event
    let mut found_maintenance_event = false;
    while let Ok(event) = event_rx.try_recv() {
        match event {
            MembershipEvent::MemberMaintenance(member) => {
                assert_eq!(member.id, node_id);
                assert_eq!(member.state, NodeState::Maintenance);
                found_maintenance_event = true;
                break;
            }
            _ => {}
        }
    }
    
    assert!(found_maintenance_event, "Maintenance event not found");
    
    // Verify the node state is Maintenance
    let members = membership.get_all_members().await;
    assert_eq!(members[0].state, NodeState::Maintenance);
    
    // Exit maintenance mode
    membership.exit_maintenance_mode().await?;
    
    // Wait a bit for the event
    sleep(Duration::from_millis(100)).await;
    
    // There should be an alive event
    let mut found_alive_event = false;
    while let Ok(event) = event_rx.try_recv() {
        match event {
            MembershipEvent::MemberAlive(member) => {
                assert_eq!(member.id, node_id);
                assert_eq!(member.state, NodeState::Alive);
                found_alive_event = true;
                break;
            }
            _ => {}
        }
    }
    
    assert!(found_alive_event, "Alive event not found");
    
    // Verify the node state is Alive
    let members = membership.get_all_members().await;
    assert_eq!(members[0].state, NodeState::Alive);
    
    Ok(())
}

#[tokio::test]
async fn test_membership_message_handling() -> Result<()> {
    // Create a membership protocol instance
    let node_id = "test-node-1";
    let addr = "127.0.0.1:7946";
    
    // Use a custom config with a different port to avoid conflicts
    let mut config = MembershipConfig::default();
    config.bind_port = 7951;
    
    let membership = MembershipProtocol::new(node_id.to_string(), addr.to_string(), Some(config)).await?;
    
    // Start the protocol
    membership.start().await?;
    
    // Create a test member
    let remote_id = "test-node-2";
    let remote_addr = "127.0.0.1:7952";
    let remote_member = create_test_member(remote_id, remote_addr, NodeState::Alive);
    
    // Create a join message
    let join_msg = create_test_message(
        MessageType::Join,
        remote_id,
        remote_addr,
        Some(node_id),
        Some(vec![remote_member.clone()]),
    );
    
    // Manually process the join message
    membership.process_message(join_msg).await?;
    
    // Verify the remote node was added
    let members = membership.get_all_members().await;
    assert_eq!(members.len(), 2);
    
    let remote_member_in_list = members.iter().find(|m| m.id == remote_id).unwrap();
    assert_eq!(remote_member_in_list.address, remote_addr);
    assert_eq!(remote_member_in_list.state, NodeState::Alive);
    
    // Create a leave message
    let leave_msg = create_test_message(
        MessageType::Leave,
        remote_id,
        remote_addr,
        Some(node_id),
        None,
    );
    
    // Manually process the leave message
    membership.process_message(leave_msg).await?;
    
    // Verify the remote node is marked as Left
    let members = membership.get_all_members().await;
    let remote_member_in_list = members.iter().find(|m| m.id == remote_id).unwrap();
    assert_eq!(remote_member_in_list.state, NodeState::Left);
    
    Ok(())
}

#[tokio::test]
async fn test_membership_leader_election() -> Result<()> {
    // Create a membership protocol instance
    let node_id = "test-node-1";
    let addr = "127.0.0.1:7946";
    
    // Use a custom config with a different port to avoid conflicts
    let mut config = MembershipConfig::default();
    config.bind_port = 7953;
    config.leader_check_interval = Duration::from_millis(100);
    
    let membership = MembershipProtocol::new(node_id.to_string(), addr.to_string(), Some(config)).await?;
    
    // Get the event receiver
    let mut event_rx = membership.get_event_receiver().await;
    
    // Start the protocol
    membership.start().await?;
    
    // Wait for leader election
    sleep(Duration::from_millis(200)).await;
    
    // Since this is the only node, it should elect itself as leader
    let mut found_leader_event = false;
    while let Ok(event) = event_rx.try_recv() {
        match event {
            MembershipEvent::LeaderElected(member) => {
                assert_eq!(member.id, node_id);
                assert!(member.is_leader);
                found_leader_event = true;
                break;
            }
            _ => {}
        }
    }
    
    assert!(found_leader_event, "Leader election event not found");
    
    // Verify the node is marked as leader
    let members = membership.get_all_members().await;
    assert!(members[0].is_leader);
    
    Ok(())
}

#[tokio::test]
async fn test_membership_multi_node_simulation() -> Result<()> {
    // Create two membership protocol instances
    let node1_id = "test-node-1";
    let node1_addr = "127.0.0.1:7954";
    
    let node2_id = "test-node-2";
    let node2_addr = "127.0.0.1:7955";
    
    // Use custom configs with different ports
    let mut config1 = MembershipConfig::default();
    config1.bind_port = 7954;
    
    let mut config2 = MembershipConfig::default();
    config2.bind_port = 7955;
    
    let membership1 = MembershipProtocol::new(node1_id.to_string(), node1_addr.to_string(), Some(config1)).await?;
    let membership2 = MembershipProtocol::new(node2_id.to_string(), node2_addr.to_string(), Some(config2)).await?;
    
    // Start both protocols
    membership1.start().await?;
    membership2.start().await?;
    
    // Manually simulate node2 joining node1's cluster
    let node2_member = create_test_member(node2_id, node2_addr, NodeState::Alive);
    
    // Create a join message from node2 to node1
    let join_msg = create_test_message(
        MessageType::Join,
        node2_id,
        node2_addr,
        Some(node1_id),
        Some(vec![node2_member.clone()]),
    );
    
    // Process the join message on node1
    membership1.process_message(join_msg).await?;
    
    // Wait a bit for processing
    sleep(Duration::from_millis(100)).await;
    
    // Verify node1 knows about node2
    let members1 = membership1.get_all_members().await;
    assert_eq!(members1.len(), 2);
    
    let node2_in_list = members1.iter().find(|m| m.id == node2_id).unwrap();
    assert_eq!(node2_in_list.address, node2_addr);
    assert_eq!(node2_in_list.state, NodeState::Alive);
    
    // Create a sync message from node1 to node2
    let node1_member = create_test_member(node1_id, node1_addr, NodeState::Alive);
    
    let sync_msg = create_test_message(
        MessageType::Sync,
        node1_id,
        node1_addr,
        Some(node2_id),
        Some(vec![node1_member.clone()]),
    );
    
    // Process the sync message on node2
    membership2.process_message(sync_msg).await?;
    
    // Wait a bit for processing
    sleep(Duration::from_millis(100)).await;
    
    // Verify node2 knows about node1
    let members2 = membership2.get_all_members().await;
    assert_eq!(members2.len(), 2);
    
    let node1_in_list = members2.iter().find(|m| m.id == node1_id).unwrap();
    assert_eq!(node1_in_list.address, node1_addr);
    assert_eq!(node1_in_list.state, NodeState::Alive);
    
    Ok(())
}

#[tokio::test]
async fn test_membership_failure_detection() -> Result<()> {
    // Create a membership protocol instance
    let node_id = "test-node-1";
    let addr = "127.0.0.1:7946";
    
    // Use a custom config with a different port to avoid conflicts
    let mut config = MembershipConfig::default();
    config.bind_port = 7956;
    config.suspicion_mult = 1; // Make suspicion timeout shorter for testing
    
    let membership = MembershipProtocol::new(node_id.to_string(), addr.to_string(), Some(config)).await?;
    
    // Get the event receiver
    let mut event_rx = membership.get_event_receiver().await;
    
    // Start the protocol
    membership.start().await?;
    
    // Create a test member
    let remote_id = "test-node-2";
    let remote_addr = "127.0.0.1:7957";
    let remote_member = create_test_member(remote_id, remote_addr, NodeState::Alive);
    
    // Add the remote member
    membership.add_member(remote_member.clone()).await?;
    
    // Verify the remote node was added
    let members = membership.get_all_members().await;
    assert_eq!(members.len(), 2);
    
    // Manually mark the remote node as suspected
    membership.suspect_member(remote_id).await?;
    
    // Wait a bit for the suspicion event
    sleep(Duration::from_millis(100)).await;
    
    // There should be a suspicion event
    let mut found_suspicion_event = false;
    while let Ok(event) = event_rx.try_recv() {
        match event {
            MembershipEvent::MemberSuspected(member) => {
                assert_eq!(member.id, remote_id);
                assert_eq!(member.state, NodeState::Suspected);
                found_suspicion_event = true;
                break;
            }
            _ => {}
        }
    }
    
    assert!(found_suspicion_event, "Suspicion event not found");
    
    // Verify the remote node is marked as Suspected
    let members = membership.get_all_members().await;
    let remote_member_in_list = members.iter().find(|m| m.id == remote_id).unwrap();
    assert_eq!(remote_member_in_list.state, NodeState::Suspected);
    
    // Wait for the suspicion timeout to expire
    sleep(Duration::from_secs(2)).await;
    
    // The node should be marked as Dead
    let members = membership.get_all_members().await;
    let remote_member_in_list = members.iter().find(|m| m.id == remote_id).unwrap();
    assert_eq!(remote_member_in_list.state, NodeState::Dead);
    
    Ok(())
}

#[tokio::test]
async fn test_membership_reintegration() -> Result<()> {
    // Create a membership protocol instance
    let node_id = "test-node-1";
    let addr = "127.0.0.1:7946";
    
    // Use a custom config with a different port to avoid conflicts
    let mut config = MembershipConfig::default();
    config.bind_port = 7958;
    config.reintegration_time = Duration::from_millis(500); // Short reintegration time for testing
    
    let membership = MembershipProtocol::new(node_id.to_string(), addr.to_string(), Some(config)).await?;
    
    // Start the protocol
    membership.start().await?;
    
    // Create a test member that is Dead
    let remote_id = "test-node-2";
    let remote_addr = "127.0.0.1:7959";
    let mut remote_member = create_test_member(remote_id, remote_addr, NodeState::Dead);
    remote_member.last_state_change = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs() - 1000; // Set last state change to a while ago
    
    // Add the dead member
    membership.add_member(remote_member.clone()).await?;
    
    // Verify the remote node was added as Dead
    let members = membership.get_all_members().await;
    let remote_member_in_list = members.iter().find(|m| m.id == remote_id).unwrap();
    assert_eq!(remote_member_in_list.state, NodeState::Dead);
    
    // Create a reintegration message
    let reintegrate_msg = create_test_message(
        MessageType::Reintegrate,
        remote_id,
        remote_addr,
        Some(node_id),
        None,
    );
    
    // Wait for reintegration time to pass
    sleep(Duration::from_millis(600)).await;
    
    // Process the reintegration message
    membership.process_message(reintegrate_msg).await?;
    
    // Verify the remote node is now Alive
    let members = membership.get_all_members().await;
    let remote_member_in_list = members.iter().find(|m| m.id == remote_id).unwrap();
    assert_eq!(remote_member_in_list.state, NodeState::Alive);
    
    Ok(())
}