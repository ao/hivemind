use anyhow::Result;
use hivemind::network::{
    ContainerNetworkConfig, EnhancedNetworkPolicy, NetworkManager, NetworkPolicy,
    NetworkRule, NetworkSelector, OverlayNetworkType, PolicyAction, PolicyType, PortRange, Protocol,
};
use hivemind::node::NodeManager;
use hivemind::service_discovery::ServiceDiscovery;
use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr};
use std::sync::Arc;
use tokio::sync::Mutex;

// Mock NodeManager for testing network
struct MockNodeManager {
    node_id: String,
    nodes: Arc<Mutex<HashMap<String, String>>>,
}

impl MockNodeManager {
    fn new(node_id: &str) -> Self {
        let mut nodes = HashMap::new();
        nodes.insert(node_id.to_string(), "192.168.1.1".to_string());
        
        Self {
            node_id: node_id.to_string(),
            nodes: Arc::new(Mutex::new(nodes)),
        }
    }
    
    async fn add_node(&self, id: &str, address: &str) {
        let mut nodes = self.nodes.lock().await;
        nodes.insert(id.to_string(), address.to_string());
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

    async fn get_node_details(&self) -> Result<Vec<(String, String, hivemind::node::NodeResources)>> {
        let nodes = self.nodes.lock().await;
        let mut result = Vec::new();
        for (id, address) in nodes.iter() {
            // Create dummy resources for testing
            let resources = hivemind::node::NodeResources {
                cpu_total: 8.0,
                cpu_available: 8.0,
                memory_total: 16 * 1024 * 1024 * 1024,
                memory_available: 16 * 1024 * 1024 * 1024,
                disk_total: 100 * 1024 * 1024 * 1024,
                disk_available: 100 * 1024 * 1024 * 1024,
            };
            result.push((id.clone(), address.clone(), resources));
        }
        Ok(result)
    }
}

#[tokio::test]
async fn test_enhanced_network_policy() -> Result<()> {
    let node_manager = Arc::new(MockNodeManager::new("test-node"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create network manager with custom config
    let network_config = hivemind::network::NetworkConfig {
        network_cidr: "10.244.0.0/16".to_string(),
        node_subnet_size: 24,
        overlay_type: OverlayNetworkType::VXLAN,
        vxlan_id: 42,
        vxlan_port: 4789,
        mtu: 1450,
        enable_ipv6: false,
        enable_encryption: true,
        encryption_key: Some("test-key".to_string()),
    };
    
    let network_manager = NetworkManager::new(
        node_manager.clone(),
        service_discovery.clone(),
        Some(network_config),
    ).await?;
    
    // Create container labels
    let mut web_labels = HashMap::new();
    web_labels.insert("app".to_string(), "web".to_string());
    web_labels.insert("tier".to_string(), "frontend".to_string());
    
    // Register container with labels
    network_manager.register_container_labels("web-container", web_labels.clone()).await?;
    
    // Create a base network policy
    let port_range = PortRange {
        protocol: Protocol::TCP,
        port_min: 80,
        port_max: 80,
    };
    
    let rule = NetworkRule {
        ports: vec![port_range],
        from: Vec::new(),
        action: Some(PolicyAction::Allow),
        log: true,
        description: Some("Allow HTTP traffic".to_string()),
        id: Some("http-rule".to_string()),
    };
    
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();
    
    let base_policy = NetworkPolicy {
        name: "web-policy".to_string(),
        selector: NetworkSelector { labels: web_labels },
        ingress_rules: vec![rule],
        egress_rules: Vec::new(),
        priority: 100,
        namespace: Some("default".to_string()),
        labels: HashMap::new(),
        created_at: now,
        updated_at: now,
    };
    
    // Create an enhanced QoS policy
    let enhanced_policy = EnhancedNetworkPolicy {
        base_policy: base_policy.clone(),
        policy_type: PolicyType::QoS,
        applies_to_ingress: true,
        applies_to_egress: false,
        action: PolicyAction::Limit(1000), // 1000 Kbps
        log_violations: true,
    };
    
    // Apply the enhanced policy
    network_manager.apply_enhanced_policy(enhanced_policy).await?;
    
    // Verify policy was added
    let policies = network_manager.get_policies().await;
    assert_eq!(policies.len(), 1);
    assert_eq!(policies[0].name, "web-policy");
    
    // Get metrics
    let metrics = network_manager.get_metrics().await;
    assert_eq!(metrics.total_policies, 1);
    
    // Run health check
    network_manager.run_health_check().await?;
    
    // Remove policy
    network_manager.remove_network_policy("web-policy").await?;
    
    // Verify policy was removed
    let policies = network_manager.get_policies().await;
    assert_eq!(policies.len(), 0);
    
    Ok(())
}

#[tokio::test]
async fn test_vxlan_overlay_network() -> Result<()> {
    let node_manager = Arc::new(MockNodeManager::new("node1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Add another node
    node_manager.add_node("node2", "192.168.1.2").await;
    
    // Create network manager with custom config
    let network_config = hivemind::network::NetworkConfig {
        network_cidr: "10.244.0.0/16".to_string(),
        node_subnet_size: 24,
        overlay_type: OverlayNetworkType::VXLAN,
        vxlan_id: 42,
        vxlan_port: 4789,
        mtu: 1450,
        enable_ipv6: false,
        enable_encryption: true,
        encryption_key: Some("test-key".to_string()),
    };
    
    let network_manager = NetworkManager::new(
        node_manager.clone(),
        service_discovery.clone(),
        Some(network_config),
    ).await?;
    
    // Get nodes
    let nodes = network_manager.get_nodes().await;
    assert_eq!(nodes.len(), 2);
    assert!(nodes.contains_key("node1"));
    assert!(nodes.contains_key("node2"));
    
    // Simulate node join
    let member = hivemind::membership::Member {
        id: "node3".to_string(),
        address: "192.168.1.3".to_string(),
        state: hivemind::membership::NodeState::Alive,
        incarnation: 1,
        last_state_change: 0,
        is_leader: false,
        last_leader_check: 0,
        metadata: None,
    };
    
    // Handle node join
    network_manager.handle_node_joined(&member).await?;
    
    // Verify node was added
    let nodes = network_manager.get_nodes().await;
    assert_eq!(nodes.len(), 3);
    assert!(nodes.contains_key("node3"));
    
    // Get tunnels
    let tunnels = network_manager.get_tunnels().await;
    assert!(tunnels.contains_key("node2") || tunnels.contains_key("node3"));
    
    // Verify tunnel properties
    for (_, tunnel) in tunnels.iter() {
        assert_eq!(tunnel.tunnel_type, OverlayNetworkType::VXLAN);
        assert_eq!(tunnel.mtu, 1450);
    }
    
    // Simulate node leave
    network_manager.handle_node_left(&member).await?;
    
    // Verify node was removed
    let nodes = network_manager.get_nodes().await;
    assert_eq!(nodes.len(), 2);
    assert!(!nodes.contains_key("node3"));
    
    Ok(())
}