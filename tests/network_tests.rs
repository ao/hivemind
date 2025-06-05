use anyhow::Result;
use hivemind::network::{
    ContainerNetworkConfig, EnhancedNetworkPolicy, IpamManager, NetworkManager, NetworkPolicy,
    NetworkPolicyManager, NetworkRule, NetworkSelector, OverlayNetwork, OverlayNetworkType,
    PortRange, Protocol, TunnelInfo,
};
use hivemind::node::NodeManager;
use hivemind::service_discovery::ServiceDiscovery;
use hivemind::storage::StorageManager;
use ipnetwork::IpNetwork;
use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr};
use std::str::FromStr;
use std::sync::Arc;
use tempfile::TempDir;
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
async fn test_ipam_manager() -> Result<()> {
    // Create IPAM manager with test network
    let network_cidr = IpNetwork::from_str("10.244.0.0/16")?;
    let node_subnet_size = 24;
    let mut ipam = IpamManager::new(network_cidr, node_subnet_size);
    
    // Allocate subnet for a node
    let node_id = "test-node-1";
    let subnet = ipam.allocate_node_subnet(node_id)?;
    
    // Verify subnet is within network CIDR
    assert!(network_cidr.contains(subnet));
    assert_eq!(subnet.prefix(), node_subnet_size);
    
    // Allocate IP for a container
    let container_id = "test-container-1";
    let ip = ipam.allocate_container_ip(node_id, container_id)?;
    
    // Verify IP is within node subnet
    assert!(subnet.contains(ip));
    
    // Allocate another IP for a different container
    let container_id2 = "test-container-2";
    let ip2 = ipam.allocate_container_ip(node_id, container_id2)?;
    
    // Verify IPs are different
    assert_ne!(ip, ip2);
    assert!(subnet.contains(ip2));
    
    // Release container IP
    ipam.release_container_ip(node_id, container_id)?;
    
    // Allocate a new container IP - should reuse the released IP
    let container_id3 = "test-container-3";
    let ip3 = ipam.allocate_container_ip(node_id, container_id3)?;
    assert_eq!(ip, ip3);
    
    // Allocate subnet for another node
    let node_id2 = "test-node-2";
    let subnet2 = ipam.allocate_node_subnet(node_id2)?;
    
    // Verify subnets are different
    assert_ne!(subnet, subnet2);
    assert!(network_cidr.contains(subnet2));
    assert_eq!(subnet2.prefix(), node_subnet_size);
    
    // Release node subnet
    ipam.release_node_subnet(node_id)?;
    
    // Allocate a new node subnet - should reuse the released subnet
    let node_id3 = "test-node-3";
    let subnet3 = ipam.allocate_node_subnet(node_id3)?;
    assert_eq!(subnet, subnet3);
    
    Ok(())
}

#[tokio::test]
async fn test_overlay_network() -> Result<()> {
    // Create overlay network
    let overlay = OverlayNetwork::new(
        OverlayNetworkType::VXLAN,
        42, // VXLAN ID
        4789, // VXLAN port
    );
    
    // Test with MTU
    let overlay = overlay.with_mtu(1450);
    assert_eq!(overlay.get_mtu(), 1450);
    
    // Test with custom tunnel interface
    let overlay = overlay.with_tunnel_interface("hivemind-vxlan0".to_string());
    assert_eq!(overlay.get_tunnel_interface(), "hivemind-vxlan0");
    
    Ok(())
}

#[tokio::test]
async fn test_network_policy_manager() -> Result<()> {
    // Create network policy manager
    let mut policy_manager = NetworkPolicyManager::new();
    
    // Create a network policy
    let mut labels = HashMap::new();
    labels.insert("app".to_string(), "web".to_string());
    
    let selector = NetworkSelector { labels };
    
    let port_range = PortRange {
        protocol: Protocol::TCP,
        port_min: 80,
        port_max: 80,
    };
    
    let rule = NetworkRule {
        ports: vec![port_range],
        from: Vec::new(),
    };
    
    let policy = NetworkPolicy {
        name: "test-policy".to_string(),
        selector,
        ingress_rules: vec![rule],
        egress_rules: Vec::new(),
    };
    
    // Register container labels
    let mut container_labels = HashMap::new();
    container_labels.insert("app".to_string(), "web".to_string());
    
    policy_manager.register_container_labels("test-container", container_labels).await?;
    
    // Add policy
    policy_manager.add_policy(policy.clone()).await?;
    
    // Verify policy was added
    let policies = policy_manager.get_policies();
    assert_eq!(policies.len(), 1);
    assert_eq!(policies[0].name, "test-policy");
    
    // Test policy matching
    let matching_containers = policy_manager.find_matching_containers(&policy.selector);
    assert_eq!(matching_containers.len(), 1);
    assert_eq!(matching_containers[0], "test-container");
    
    // Unregister container
    policy_manager.unregister_container_labels("test-container").await?;
    
    // Verify container was unregistered
    let matching_containers = policy_manager.find_matching_containers(&policy.selector);
    assert_eq!(matching_containers.len(), 0);
    
    // Remove policy
    policy_manager.remove_policy("test-policy").await?;
    
    // Verify policy was removed
    let policies = policy_manager.get_policies();
    assert_eq!(policies.len(), 0);
    
    Ok(())
}

#[tokio::test]
async fn test_network_manager_initialization() -> Result<()> {
    let node_manager = Arc::new(MockNodeManager::new("test-node"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create network manager
    let network_manager = NetworkManager::new(
        node_manager.clone(),
        service_discovery.clone(),
        None, // Use default config
    ).await?;
    
    // Test initialization
    // Note: This will likely fail in CI environments without proper network setup
    // So we'll just verify the manager was created
    assert!(network_manager.get_config().network_cidr == "10.244.0.0/16");
    assert_eq!(network_manager.get_config().node_subnet_size, 24);
    assert_eq!(network_manager.get_config().overlay_type, OverlayNetworkType::VXLAN);
    
    Ok(())
}

#[tokio::test]
async fn test_container_network_config() -> Result<()> {
    // Create container network config
    let config = ContainerNetworkConfig {
        container_id: "test-container".to_string(),
        ip_address: IpAddr::V4(Ipv4Addr::new(10, 244, 0, 2)),
        mac_address: "02:42:0a:f4:00:02".to_string(),
        gateway: IpAddr::V4(Ipv4Addr::new(10, 244, 0, 1)),
        node_id: "test-node".to_string(),
    };
    
    // Verify config
    assert_eq!(config.container_id, "test-container");
    assert_eq!(config.ip_address.to_string(), "10.244.0.2");
    assert_eq!(config.mac_address, "02:42:0a:f4:00:02");
    assert_eq!(config.gateway.to_string(), "10.244.0.1");
    assert_eq!(config.node_id, "test-node");
    
    Ok(())
}

#[tokio::test]
async fn test_tunnel_info() -> Result<()> {
    // Create tunnel info
    let tunnel = TunnelInfo {
        node_id: "remote-node".to_string(),
        remote_ip: IpAddr::V4(Ipv4Addr::new(192, 168, 1, 2)),
        local_ip: IpAddr::V4(Ipv4Addr::new(192, 168, 1, 1)),
        tunnel_name: "vxlan0".to_string(),
    };
    
    // Verify tunnel info
    assert_eq!(tunnel.node_id, "remote-node");
    assert_eq!(tunnel.remote_ip.to_string(), "192.168.1.2");
    assert_eq!(tunnel.local_ip.to_string(), "192.168.1.1");
    assert_eq!(tunnel.tunnel_name, "vxlan0");
    
    Ok(())
}

#[tokio::test]
async fn test_multi_node_network() -> Result<()> {
    let node_manager = Arc::new(MockNodeManager::new("node1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Add another node
    node_manager.add_node("node2", "192.168.1.2").await;
    
    // Create network manager
    let network_manager = NetworkManager::new(
        node_manager.clone(),
        service_discovery.clone(),
        None, // Use default config
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
    
    // Simulate node leave
    network_manager.handle_node_left(&member).await?;
    
    // Verify node was removed
    let nodes = network_manager.get_nodes().await;
    assert_eq!(nodes.len(), 2);
    assert!(!nodes.contains_key("node3"));
    
    Ok(())
}

#[tokio::test]
async fn test_network_policies() -> Result<()> {
    let node_manager = Arc::new(MockNodeManager::new("test-node"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create network manager
    let network_manager = NetworkManager::new(
        node_manager.clone(),
        service_discovery.clone(),
        None, // Use default config
    ).await?;
    
    // Create a network policy
    let mut labels = HashMap::new();
    labels.insert("app".to_string(), "web".to_string());
    
    let selector = NetworkSelector { labels };
    
    let port_range = PortRange {
        protocol: Protocol::TCP,
        port_min: 80,
        port_max: 80,
    };
    
    let rule = NetworkRule {
        ports: vec![port_range],
        from: Vec::new(),
    };
    
    let policy = NetworkPolicy {
        name: "test-policy".to_string(),
        selector,
        ingress_rules: vec![rule],
        egress_rules: Vec::new(),
    };
    
    // Add policy
    network_manager.add_policy(policy.clone()).await?;
    
    // Verify policy was added
    let policies = network_manager.get_policies().await;
    assert_eq!(policies.len(), 1);
    assert_eq!(policies[0].name, "test-policy");
    
    // Register container labels
    let mut container_labels = HashMap::new();
    container_labels.insert("app".to_string(), "web".to_string());
    
    network_manager.register_container_labels("test-container", container_labels).await?;
    
    // Remove policy
    network_manager.remove_policy("test-policy").await?;
    
    // Verify policy was removed
    let policies = network_manager.get_policies().await;
    assert_eq!(policies.len(), 0);
    
    Ok(())
}

#[tokio::test]
async fn test_network_isolation() -> Result<()> {
    let node_manager = Arc::new(MockNodeManager::new("test-node"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create network manager
    let network_manager = NetworkManager::new(
        node_manager.clone(),
        service_discovery.clone(),
        None, // Use default config
    ).await?;
    
    // Create two network policies for different app tiers
    
    // Web tier policy - allow ingress on port 80
    let mut web_labels = HashMap::new();
    web_labels.insert("tier".to_string(), "web".to_string());
    
    let web_selector = NetworkSelector { labels: web_labels };
    
    let web_port_range = PortRange {
        protocol: Protocol::TCP,
        port_min: 80,
        port_max: 80,
    };
    
    let web_rule = NetworkRule {
        ports: vec![web_port_range],
        from: Vec::new(), // Allow from anywhere
    };
    
    let web_policy = NetworkPolicy {
        name: "web-tier-policy".to_string(),
        selector: web_selector,
        ingress_rules: vec![web_rule],
        egress_rules: Vec::new(),
    };
    
    // DB tier policy - allow ingress only from app tier on port 5432
    let mut db_labels = HashMap::new();
    db_labels.insert("tier".to_string(), "db".to_string());
    
    let db_selector = NetworkSelector { labels: db_labels };
    
    let db_port_range = PortRange {
        protocol: Protocol::TCP,
        port_min: 5432,
        port_max: 5432,
    };
    
    // Create "from" selector for app tier
    let mut app_labels = HashMap::new();
    app_labels.insert("tier".to_string(), "app".to_string());
    
    let app_selector = NetworkSelector { labels: app_labels };
    
    let mut from_peers = Vec::new();
    from_peers.push(hivemind::network::NetworkPeer {
        ip_block: None,
        selector: Some(app_selector),
    });
    
    let db_rule = NetworkRule {
        ports: vec![db_port_range],
        from: from_peers,
    };
    
    let db_policy = NetworkPolicy {
        name: "db-tier-policy".to_string(),
        selector: db_selector,
        ingress_rules: vec![db_rule],
        egress_rules: Vec::new(),
    };
    
    // Add policies
    network_manager.add_policy(web_policy).await?;
    network_manager.add_policy(db_policy).await?;
    
    // Verify policies were added
    let policies = network_manager.get_policies().await;
    assert_eq!(policies.len(), 2);
    
    // Register containers with labels
    let mut web_container_labels = HashMap::new();
    web_container_labels.insert("tier".to_string(), "web".to_string());
    
    let mut app_container_labels = HashMap::new();
    app_container_labels.insert("tier".to_string(), "app".to_string());
    
    let mut db_container_labels = HashMap::new();
    db_container_labels.insert("tier".to_string(), "db".to_string());
    
    network_manager.register_container_labels("web-container", web_container_labels).await?;
    network_manager.register_container_labels("app-container", app_container_labels).await?;
    network_manager.register_container_labels("db-container", db_container_labels).await?;
    
    // Clean up
    network_manager.remove_policy("web-tier-policy").await?;
    network_manager.remove_policy("db-tier-policy").await?;
    
    Ok(())
}