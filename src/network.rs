use crate::membership::Member;
use crate::node::NodeManager;
use crate::service_discovery::ServiceDiscovery;
use crate::tenant::TenantContext;
use anyhow::{Context, Result};
use ipnetwork::IpNetwork;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr};
use std::process::Command;
use std::str::FromStr;
use std::sync::Arc;
use tokio::sync::Mutex;
use uuid::Uuid;

// Network configuration constants
const DEFAULT_NETWORK_CIDR: &str = "10.244.0.0/16";
const DEFAULT_NODE_SUBNET_SIZE: u8 = 24;
const DEFAULT_VXLAN_ID: u16 = 42;
const DEFAULT_VXLAN_PORT: u16 = 4789;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NetworkConfig {
    pub network_cidr: String,
    pub node_subnet_size: u8,
    pub overlay_type: OverlayNetworkType,
    pub vxlan_id: u16,
    pub vxlan_port: u16,
    pub mtu: u32,
    pub enable_ipv6: bool,
    pub enable_encryption: bool,
    pub encryption_key: Option<String>,
}

impl Default for NetworkConfig {
    fn default() -> Self {
        Self {
            network_cidr: DEFAULT_NETWORK_CIDR.to_string(),
            node_subnet_size: DEFAULT_NODE_SUBNET_SIZE,
            overlay_type: OverlayNetworkType::VXLAN,
            vxlan_id: DEFAULT_VXLAN_ID,
            vxlan_port: DEFAULT_VXLAN_PORT,
            mtu: 1450, // Default MTU for VXLAN (1500 - 50 for VXLAN overhead)
            enable_ipv6: false,
            enable_encryption: false,
            encryption_key: None,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum OverlayNetworkType {
    VXLAN,
    Geneve,
    WireGuard,
    IPsec,
    // Other overlay types could be added
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NodeNetworkInfo {
    pub node_id: String,
    pub address: String,
    pub subnet: IpNetwork,
    pub tunnel_ip: IpAddr,
    pub ipv6_subnet: Option<IpNetwork>,
    pub tunnel_ipv6: Option<IpAddr>,
    pub state: NodeNetworkState,
    pub last_heartbeat: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum NodeNetworkState {
    Initializing,
    Ready,
    Degraded,
    Disconnected,
}

impl Default for NodeNetworkState {
    fn default() -> Self {
        Self::Initializing
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContainerNetworkConfig {
    pub container_id: String,
    pub ip_address: IpAddr,
    pub mac_address: String,
    pub gateway: IpAddr,
    pub node_id: String,
    pub ipv6_address: Option<IpAddr>,
    pub ipv6_gateway: Option<IpAddr>,
    pub dns_servers: Vec<IpAddr>,
    pub search_domains: Vec<String>,
    pub labels: HashMap<String, String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TunnelInfo {
    pub node_id: String,
    pub remote_ip: IpAddr,
    pub local_ip: IpAddr,
    pub tunnel_name: String,
    pub tunnel_type: OverlayNetworkType,
    pub encrypted: bool,
    pub established: bool,
    pub last_activity: u64,
    pub tx_bytes: u64,
    pub rx_bytes: u64,
    pub mtu: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NetworkPolicy {
    pub name: String,
    pub selector: NetworkSelector,
    pub ingress_rules: Vec<NetworkRule>,
    pub egress_rules: Vec<NetworkRule>,
    pub priority: i32,
    pub namespace: Option<String>,
    pub tenant_id: Option<String>,
    pub labels: HashMap<String, String>,
    pub created_at: u64,
    pub updated_at: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EnhancedNetworkPolicy {
    pub base_policy: NetworkPolicy,
    pub policy_type: PolicyType,
    pub applies_to_ingress: bool,
    pub applies_to_egress: bool,
    pub action: PolicyAction,
    pub log_violations: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum PolicyType {
    Isolation,
    Security,
    QoS,
    Custom(String),
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum PolicyAction {
    Allow,
    Deny,
    Limit(u32), // Rate limit in Kbps
    Log,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NetworkSelector {
    pub labels: HashMap<String, String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NetworkRule {
    pub ports: Vec<PortRange>,
    pub from: Vec<NetworkPeer>,
    pub action: Option<PolicyAction>,
    pub log: bool,
    pub description: Option<String>,
    pub id: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PortRange {
    pub protocol: Protocol,
    pub port_min: u16,
    pub port_max: u16,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq)]
pub enum Protocol {
    TCP,
    UDP,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NetworkPeer {
    pub ip_block: Option<IpNetwork>,
    pub selector: Option<NetworkSelector>,
}

#[derive(Debug)]
pub struct NetworkManager {
    ipam: Arc<Mutex<IpamManager>>,
    overlay: Arc<Mutex<OverlayNetwork>>,
    nodes: Arc<Mutex<HashMap<String, NodeNetworkInfo>>>,
    policies: Arc<Mutex<NetworkPolicyManager>>,
    service_discovery: Arc<ServiceDiscovery>,
    node_manager: Arc<NodeManager>,
    config: NetworkConfig,
    container_networks: Arc<Mutex<HashMap<String, ContainerNetworkConfig>>>,
    metrics: Arc<Mutex<NetworkMetrics>>,
    health_check_interval: u64,
    last_health_check: Arc<Mutex<u64>>,
    // Tenant-specific overlay networks
    tenant_overlays: Arc<Mutex<HashMap<String, Arc<Mutex<OverlayNetwork>>>>>,
    // Tenant-specific network CIDRs
    tenant_network_cidrs: Arc<Mutex<HashMap<String, String>>>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct NetworkMetrics {
    pub total_containers: usize,
    pub total_tunnels: usize,
    pub total_policies: usize,
    pub bytes_transmitted: u64,
    pub bytes_received: u64,
    pub packets_transmitted: u64,
    pub packets_received: u64,
    pub packet_errors: u64,
    pub tunnel_failures: u64,
    pub policy_violations: u64,
}

impl NetworkManager {
    pub async fn new(
        node_manager: Arc<NodeManager>,
        service_discovery: Arc<ServiceDiscovery>,
        config: Option<NetworkConfig>,
    ) -> Result<Self> {
        let config = config.unwrap_or_default();
        
        // Parse the network CIDR
        let network_cidr = ipnetwork::IpNetwork::from_str(&config.network_cidr)
            .map_err(|e| anyhow::anyhow!("{}", e))
            .context("Failed to parse network CIDR")?;
        
        // Create IPAM manager
        let ipam = Arc::new(Mutex::new(IpamManager::new(
            network_cidr,
            config.node_subnet_size,
        )));
        
        // Create overlay network manager with MTU from config
        let mut overlay = OverlayNetwork::new(
            config.overlay_type.clone(),
            config.vxlan_id,
            config.vxlan_port,
        );
        
        // Set MTU from config
        overlay = overlay.with_mtu(config.mtu);
        
        // Enable encryption if configured
        if config.enable_encryption {
            if let Some(key) = &config.encryption_key {
                overlay = overlay.with_encryption(key.clone());
            }
        }
        
        // Create network policy manager
        let policies = Arc::new(Mutex::new(NetworkPolicyManager::new()));
        
        Ok(Self {
            ipam,
            overlay: Arc::new(Mutex::new(overlay)),
            nodes: Arc::new(Mutex::new(HashMap::new())),
            policies,
            service_discovery,
            node_manager,
            config,
            container_networks: Arc::new(Mutex::new(HashMap::new())),
            metrics: Arc::new(Mutex::new(NetworkMetrics::default())),
            health_check_interval: 60, // Default to 60 seconds
            last_health_check: Arc::new(Mutex::new(0)),
            tenant_overlays: Arc::new(Mutex::new(HashMap::new())),
            tenant_network_cidrs: Arc::new(Mutex::new(HashMap::new())),
        })
    }
    
    pub fn get_config(&self) -> &NetworkConfig {
        &self.config
    }
    
    pub async fn initialize(&self) -> Result<()> {
        println!("Initializing container networking...");
        
        // Allocate subnet for this node
        let node_id = self.node_manager.get_node_id();
        let node_ip = self.get_node_ip().await?;
        
        // Allocate subnet for this node
        let subnet = self.ipam.lock().await.allocate_node_subnet(&node_id)?;
        println!("Allocated subnet {} for node {}", subnet, node_id);
        
        // Initialize overlay network
        self.overlay.lock().await.initialize(&node_id, node_ip).await?;
        
        // Store node network info
        let mut nodes = self.nodes.lock().await;
        nodes.insert(
            node_id.clone(),
            NodeNetworkInfo {
                node_id: node_id.clone(),
                address: node_ip.to_string(),
                subnet,
                tunnel_ip: node_ip,
                ipv6_subnet: None,
                tunnel_ipv6: None,
                state: NodeNetworkState::Ready,
                last_heartbeat: std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap_or_default()
                    .as_secs(),
            },
        );
        
        // Set up local network
        self.setup_local_network(&node_id, subnet, node_ip).await?;
        
        println!("Container networking initialized successfully");
        Ok(())
    }
    
    async fn get_node_ip(&self) -> Result<IpAddr> {
        // For now, just use the node's address from node_manager
        // In a real implementation, we might need to be more selective about which IP to use
        let node_id = self.node_manager.get_node_id();
        let node_details = self.node_manager.get_node_details().await?;
        
        for (id, address, _) in node_details {
            if id == node_id {
                return Ok(address.parse()?);
            }
        }
        
        // Fallback to localhost if we can't find the node's IP
        Ok(IpAddr::V4(Ipv4Addr::new(127, 0, 0, 1)))
    }
    
    async fn setup_local_network(
        &self,
        node_id: &str,
        subnet: IpNetwork,
        node_ip: IpAddr,
    ) -> Result<()> {
        // Create the bridge for local containers
        let bridge_name = format!("hivemind0");
        let gateway_ip = Self::get_gateway_ip(subnet)?;
        
        // Create bridge if it doesn't exist
        if !Self::bridge_exists(&bridge_name)? {
            Self::create_bridge(&bridge_name)?;
            Self::set_bridge_ip(&bridge_name, gateway_ip, subnet.prefix())?;
            Self::enable_bridge(&bridge_name)?;
        }
        
        // Set up the overlay network
        self.overlay.lock().await.setup_local_network(
            node_id,
            node_ip,
            subnet,
            &bridge_name,
        ).await?;
        
        Ok(())
    }
    
    fn bridge_exists(bridge_name: &str) -> Result<bool> {
        let output = Command::new("ip")
            .args(&["link", "show", bridge_name])
            .output()?;
        
        Ok(output.status.success())
    }
    
    fn create_bridge(bridge_name: &str) -> Result<()> {
        let status = Command::new("ip")
            .args(&["link", "add", "name", bridge_name, "type", "bridge"])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to create bridge {}", bridge_name);
        }
        
        Ok(())
    }
    
    fn set_bridge_ip(bridge_name: &str, ip: IpAddr, prefix_len: u8) -> Result<()> {
        let ip_cidr = format!("{}/{}", ip, prefix_len);
        let status = Command::new("ip")
            .args(&["addr", "add", &ip_cidr, "dev", bridge_name])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to set IP {} on bridge {}", ip_cidr, bridge_name);
        }
        
        Ok(())
    }
    
    fn enable_bridge(bridge_name: &str) -> Result<()> {
        let status = Command::new("ip")
            .args(&["link", "set", bridge_name, "up"])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to enable bridge {}", bridge_name);
        }
        
        Ok(())
    }
    
    fn get_gateway_ip(subnet: IpNetwork) -> Result<IpAddr> {
        // Use the first usable IP in the subnet as the gateway
        match subnet {
            IpNetwork::V4(subnet) => {
                let mut iter = subnet.iter();
                // Skip the network address
                iter.next();
                // Use the first usable IP
                if let Some(ip) = iter.next() {
                    Ok(IpAddr::V4(ip))
                } else {
                    anyhow::bail!("Subnet too small to allocate gateway IP");
                }
            }
            IpNetwork::V6(_) => {
                anyhow::bail!("IPv6 not supported yet");
            }
        }
    }
    
    // Setup container network with tenant context
    pub async fn setup_container_network_with_tenant(
        &self,
        container_id: &str,
        pid: i32,
        static_ip: Option<IpAddr>,
        labels: Option<HashMap<String, String>>,
        tenant_context: &TenantContext,
    ) -> Result<ContainerNetworkConfig> {
        // Create container network config with tenant context
        let config = self.create_container_network_config_with_tenant(
            container_id,
            &self.node_manager.get_node_id(),
            tenant_context
        ).await?;
        
        // Set up container network namespace
        self.setup_container_namespace(pid, &config).await?;
        
        // Register container in service discovery for DNS resolution
        self.register_container_dns(container_id, config.ip_address).await?;
        
        // Add tenant ID to labels
        let mut all_labels = labels.unwrap_or_default();
        all_labels.insert("tenant".to_string(), tenant_context.tenant_id.clone());
        all_labels.insert("user".to_string(), tenant_context.user_id.clone());
        
        // Register container labels for policy matching
        self.policies.lock().await.register_container_labels(container_id, all_labels).await?;
        
        Ok(config)
    }
    
    // Create container network config with tenant context
    async fn create_container_network_config_with_tenant(
        &self,
        container_id: &str,
        node_id: &str,
        tenant_context: &TenantContext,
    ) -> Result<ContainerNetworkConfig> {
        let tenant_id = &tenant_context.tenant_id;
        
        // Check if tenant has a specific network CIDR
        let mut tenant_network_cidrs = self.tenant_network_cidrs.lock().await;
        if !tenant_network_cidrs.contains_key(tenant_id) {
            // Allocate a new CIDR for this tenant
            // Use a different subnet range for each tenant to ensure isolation
            // For example: 10.100.tenant_id.0/24
            let tenant_num = tenant_id.as_bytes().iter().fold(0u8, |acc, &b| acc.wrapping_add(b));
            let tenant_cidr = format!("10.100.{}.0/24", tenant_num);
            tenant_network_cidrs.insert(tenant_id.clone(), tenant_cidr);
        }
        
        // Get the tenant's network CIDR
        let tenant_cidr_str = tenant_network_cidrs.get(tenant_id).unwrap().clone();
        let tenant_cidr = IpNetwork::from_str(&tenant_cidr_str)?;
        
        // Check if tenant has a specific overlay network
        let mut tenant_overlays = self.tenant_overlays.lock().await;
        if !tenant_overlays.contains_key(tenant_id) {
            // Create a new overlay network for this tenant
            let tenant_vxlan_id = 100 + tenant_id.as_bytes().iter().fold(0u16, |acc, &b| acc.wrapping_add(b as u16));
            let tenant_overlay = OverlayNetwork::new(
                self.config.overlay_type.clone(),
                tenant_vxlan_id,
                self.config.vxlan_port,
            ).with_mtu(self.config.mtu)
             .with_tunnel_interface(format!("vxlan-{}", &tenant_id[..8]));
            
            // Initialize the tenant overlay network
            let node_ip = self.get_node_ip().await?;
            tenant_overlay.initialize(node_id, node_ip).await?;
            
            // Store the tenant overlay network
            tenant_overlays.insert(tenant_id.clone(), Arc::new(Mutex::new(tenant_overlay)));
        }
        
        // Get node subnet from the tenant's CIDR
        let nodes = self.nodes.lock().await;
        let node_info = nodes.get(node_id).context("Node network info not found")?;
        
        // Handle static IP assignment if provided
        if let Some(ip) = static_ip {
            self.ipam.lock().await.assign_static_ip(container_id, ip)?;
        }
        
        // Allocate IP for container from tenant's CIDR
        let ip = self.ipam.lock().await.allocate_container_ip(node_id, container_id)?;
        
        // Generate MAC address
        let mac_address = Self::generate_mac_address(container_id);
        
        // Get gateway IP
        let gateway = Self::get_gateway_ip(node_info.subnet)?;
        
        // Get DNS servers - use the gateway as the primary DNS server
        let mut dns_servers = vec![gateway];
        
        // Add any additional DNS servers from service discovery
        if let Ok(additional_dns) = self.service_discovery.get_dns_servers().await {
            dns_servers.extend(additional_dns);
        }
        
        // Create network config with tenant-specific search domains
        let config = ContainerNetworkConfig {
            container_id: container_id.to_string(),
            ip_address: ip,
            mac_address,
            gateway,
            node_id: node_id.to_string(),
            ipv6_address: None, // IPv6 not implemented yet
            ipv6_gateway: None,
            dns_servers,
            search_domains: vec![
                format!("{}.hivemind", tenant_id),
                "hivemind".to_string(),
                "cluster.local".to_string()
            ],
            labels: HashMap::new(),
        };
        
        // Store container network config for future reference
        let mut container_networks = self.container_networks.lock().await;
        container_networks.insert(container_id.to_string(), config.clone());
        
        Ok(config)
    }
    
    pub async fn setup_container_network(
        &self,
        container_id: &str,
        pid: i32,
        static_ip: Option<IpAddr>,
        labels: Option<HashMap<String, String>>,
    ) -> Result<ContainerNetworkConfig> {
        let node_id = self.node_manager.get_node_id();
        
        // Get node subnet
        let nodes = self.nodes.lock().await;
        let node_info = nodes.get(&node_id).context("Node network info not found")?;
        
        // Handle static IP assignment if provided
        if let Some(ip) = static_ip {
            self.ipam.lock().await.assign_static_ip(container_id, ip)?;
        }
        
        // Allocate IP for container
        let ip = self.ipam.lock().await.allocate_container_ip(&node_id, container_id)?;
        
        // Generate MAC address
        let mac_address = Self::generate_mac_address(container_id);
        
        // Get gateway IP
        let gateway = Self::get_gateway_ip(node_info.subnet)?;
        
        // Get DNS servers - use the gateway as the primary DNS server
        let mut dns_servers = vec![gateway];
        
        // Add any additional DNS servers from service discovery
        if let Ok(additional_dns) = self.service_discovery.get_dns_servers().await {
            dns_servers.extend(additional_dns);
        }
        
        // Create network config
        let config = ContainerNetworkConfig {
            container_id: container_id.to_string(),
            ip_address: ip,
            mac_address,
            gateway,
            node_id: node_id.clone(),
            ipv6_address: None, // IPv6 not implemented yet
            ipv6_gateway: None,
            dns_servers,
            search_domains: vec!["hivemind".to_string(), "cluster.local".to_string()],
            labels: labels.unwrap_or_default(),
        };
        
        // Store container network config for future reference
        let mut container_networks = self.container_networks.lock().await;
        container_networks.insert(container_id.to_string(), config.clone());
        
        // Set up container network namespace
        self.setup_container_namespace(pid, &config).await?;
        
        // Register container in service discovery for DNS resolution
        // Register container in service discovery for DNS resolution
        self.register_container_dns(container_id, ip).await?;
        
        Ok(config)
    }
    
    // Register container in service discovery for DNS resolution
    async fn register_container_dns(&self, container_id: &str, ip: IpAddr) -> Result<()> {
        // Create a DNS record for the container
        // Format: container-{id}.hivemind
        let dns_name = format!("container-{}.hivemind", &container_id[..8]);
        
        // Create a simple service config for DNS registration
        let service_config = crate::app::ServiceConfig {
            name: dns_name.clone(),
            domain: "hivemind".to_string(),
            container_ids: vec![container_id.to_string()],
            desired_replicas: 1,
            current_replicas: 1,
        };
        
        // Register with service discovery
        self.service_discovery.register_service(
            &service_config,
            &self.node_manager.get_node_id(),
            &ip.to_string(),
            53, // DNS port
        ).await?;
        
        println!("Registered container {} with DNS name {}", container_id, dns_name);
        Ok(())
    }
    
    async fn setup_container_namespace(
        &self,
        pid: i32,
        config: &ContainerNetworkConfig,
    ) -> Result<()> {
        // Create veth pair
        let container_if = format!("eth0");
        let host_if = format!("veth{}", &config.container_id[..8]);
        
        // Create veth pair
        Self::create_veth_pair(&host_if, &container_if, pid)?;
        
        // Connect host end to bridge
        Self::connect_to_bridge(&host_if, "hivemind0")?;
        
        // Configure container interface
        Self::configure_container_interface(
            pid,
            &container_if,
            config.ip_address,
            config.mac_address.as_str(),
            config.gateway,
        )?;
        
        // Enable interfaces
        Self::enable_interface(&host_if)?;
        Self::enable_container_interface(pid, &container_if)?;
        
        // Add default route in container
        Self::add_container_default_route(pid, config.gateway)?;
        
        // Configure DNS resolution in container
        Self::configure_container_dns(pid, config.gateway)?;
        
        Ok(())
    }
    
    // Configure DNS resolution in container
    fn configure_container_dns(pid: i32, dns_server: IpAddr) -> Result<()> {
        // Create resolv.conf content
        let resolv_conf = format!("nameserver {}\nsearch hivemind\n", dns_server);
        
        // Create a temporary file with the content
        let temp_file = std::env::temp_dir().join("resolv.conf.tmp");
        std::fs::write(&temp_file, resolv_conf)?;
        
        // Copy the file to the container's /etc/resolv.conf
        let status = Command::new("nsenter")
            .args(&[
                "-t", &pid.to_string(),
                "-m", // Mount namespace
                "cp", temp_file.to_str().unwrap(), "/etc/resolv.conf",
            ])
            .status()?;
            
        if !status.success() {
            println!("Warning: Failed to configure DNS in container");
            // Don't fail the whole setup for this
        }
        
        // Clean up the temporary file
        let _ = std::fs::remove_file(temp_file);
        
        Ok(())
    }
    
    fn create_veth_pair(host_if: &str, container_if: &str, pid: i32) -> Result<()> {
        // Create veth pair
        let status = Command::new("ip")
            .args(&[
                "link", "add", host_if, "type", "veth", "peer", "name", container_if,
                "netns", &pid.to_string(),
            ])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to create veth pair {}-{}", host_if, container_if);
        }
        
        Ok(())
    }
    
    fn connect_to_bridge(host_if: &str, bridge: &str) -> Result<()> {
        let status = Command::new("ip")
            .args(&["link", "set", host_if, "master", bridge])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to connect {} to bridge {}", host_if, bridge);
        }
        
        Ok(())
    }
    
    fn configure_container_interface(
        pid: i32,
        container_if: &str,
        ip: IpAddr,
        mac: &str,
        _gateway: IpAddr,
    ) -> Result<()> {
        // Set MAC address
        let status = Command::new("nsenter")
            .args(&[
                "-t", &pid.to_string(),
                "-n",
                "ip", "link", "set", container_if, "address", mac,
            ])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to set MAC address on {}", container_if);
        }
        
        // Set IP address
        let ip_cidr = format!("{}/32", ip);
        let status = Command::new("nsenter")
            .args(&[
                "-t", &pid.to_string(),
                "-n",
                "ip", "addr", "add", &ip_cidr, "dev", container_if,
            ])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to set IP address on {}", container_if);
        }
        
        Ok(())
    }
    
    fn enable_interface(interface: &str) -> Result<()> {
        let status = Command::new("ip")
            .args(&["link", "set", interface, "up"])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to enable interface {}", interface);
        }
        
        Ok(())
    }
    
    fn enable_container_interface(pid: i32, interface: &str) -> Result<()> {
        let status = Command::new("nsenter")
            .args(&[
                "-t", &pid.to_string(),
                "-n",
                "ip", "link", "set", interface, "up",
            ])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to enable interface {} in container", interface);
        }
        
        Ok(())
    }
    
    fn add_container_default_route(pid: i32, gateway: IpAddr) -> Result<()> {
        let status = Command::new("nsenter")
            .args(&[
                "-t", &pid.to_string(),
                "-n",
                "ip", "route", "add", "default", "via", &gateway.to_string(),
            ])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to add default route in container");
        }
        
        Ok(())
    }
