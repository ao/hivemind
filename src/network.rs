use crate::membership::Member;
use crate::node::NodeManager;
use crate::service_discovery::ServiceDiscovery;
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
}

impl Default for NetworkConfig {
    fn default() -> Self {
        Self {
            network_cidr: DEFAULT_NETWORK_CIDR.to_string(),
            node_subnet_size: DEFAULT_NODE_SUBNET_SIZE,
            overlay_type: OverlayNetworkType::VXLAN,
            vxlan_id: DEFAULT_VXLAN_ID,
            vxlan_port: DEFAULT_VXLAN_PORT,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum OverlayNetworkType {
    VXLAN,
    Geneve,
    // Other overlay types could be added
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NodeNetworkInfo {
    pub node_id: String,
    pub address: String,
    pub subnet: IpNetwork,
    pub tunnel_ip: IpAddr,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ContainerNetworkConfig {
    pub container_id: String,
    pub ip_address: IpAddr,
    pub mac_address: String,
    pub gateway: IpAddr,
    pub node_id: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TunnelInfo {
    pub node_id: String,
    pub remote_ip: IpAddr,
    pub local_ip: IpAddr,
    pub tunnel_name: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NetworkPolicy {
    pub name: String,
    pub selector: NetworkSelector,
    pub ingress_rules: Vec<NetworkRule>,
    pub egress_rules: Vec<NetworkRule>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NetworkSelector {
    pub labels: HashMap<String, String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NetworkRule {
    pub ports: Vec<PortRange>,
    pub from: Vec<NetworkPeer>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PortRange {
    pub protocol: Protocol,
    pub port_min: u16,
    pub port_max: u16,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
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
    #[allow(dead_code)]
    service_discovery: Arc<ServiceDiscovery>,
    node_manager: Arc<NodeManager>,
    #[allow(dead_code)]
    config: NetworkConfig,
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
        
        // Create overlay network manager
        let overlay = Arc::new(Mutex::new(OverlayNetwork::new(
            config.overlay_type.clone(),
            config.vxlan_id,
            config.vxlan_port,
        )));
        
        // Create network policy manager
        let policies = Arc::new(Mutex::new(NetworkPolicyManager::new()));
        
        Ok(Self {
            ipam,
            overlay,
            nodes: Arc::new(Mutex::new(HashMap::new())),
            policies,
            service_discovery,
            node_manager,
            config,
        })
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
    
    pub async fn setup_container_network(
        &self,
        container_id: &str,
        pid: i32,
    ) -> Result<ContainerNetworkConfig> {
        let node_id = self.node_manager.get_node_id();
        
        // Get node subnet
        let nodes = self.nodes.lock().await;
        let node_info = nodes.get(&node_id).context("Node network info not found")?;
        
        // Allocate IP for container
        let ip = self.ipam.lock().await.allocate_container_ip(&node_id, container_id)?;
        
        // Generate MAC address
        let mac_address = Self::generate_mac_address(container_id);
        
        // Get gateway IP
        let gateway = Self::get_gateway_ip(node_info.subnet)?;
        
        // Create network config
        let config = ContainerNetworkConfig {
            container_id: container_id.to_string(),
            ip_address: ip,
            mac_address,
            gateway,
            node_id: node_id.clone(),
        };
        
        // Set up container network namespace
        self.setup_container_namespace(pid, &config).await?;
        
        Ok(config)
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
    
    fn generate_mac_address(container_id: &str) -> String {
        // Generate a deterministic MAC address based on container ID
        // Use 02: as the prefix to indicate a locally administered address
        let mut mac = String::from("02:");
        
        // Use first 5 bytes of container ID (or UUID if needed)
        let id_bytes = if container_id.len() >= 10 {
            container_id[..10].to_string()
        } else {
            let uuid = Uuid::new_v4().to_string();
            uuid[..10].to_string()
        };
        
        for i in 0..5 {
            let start = i * 2;
            let end = start + 2;
            if start < id_bytes.len() {
                let byte_str = if end <= id_bytes.len() {
                    &id_bytes[start..end]
                } else {
                    &id_bytes[start..]
                };
                mac.push_str(byte_str);
            } else {
                mac.push_str("00");
            }
            
            if i < 4 {
                mac.push(':');
            }
        }
        
        mac
    }
    
    pub async fn teardown_container_network(&self, container_id: &str) -> Result<()> {
        let node_id = self.node_manager.get_node_id();
        
        // Release IP address
        self.ipam.lock().await.release_container_ip(&node_id, container_id)?;
        
        // Remove veth interface from host
        let host_if = format!("veth{}", &container_id[..8]);
        if Self::interface_exists(&host_if)? {
            Self::delete_interface(&host_if)?;
        }
        
        Ok(())
    }
    
    fn interface_exists(interface: &str) -> Result<bool> {
        let output = Command::new("ip")
            .args(&["link", "show", interface])
            .output()?;
        
        Ok(output.status.success())
    }
    
    fn delete_interface(interface: &str) -> Result<()> {
        let status = Command::new("ip")
            .args(&["link", "delete", interface])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to delete interface {}", interface);
        }
        
        Ok(())
    }
    
    pub async fn handle_node_joined(&self, member: &Member) -> Result<()> {
        println!("Setting up networking for new node: {}", member.id);
        
        // Allocate subnet for the new node
        let subnet = self.ipam.lock().await.allocate_node_subnet(&member.id)?;
        
        // Parse the node's IP address
        let node_ip: IpAddr = member.address.parse()?;
        
        // Store node network info
        let mut nodes = self.nodes.lock().await;
        nodes.insert(
            member.id.clone(),
            NodeNetworkInfo {
                node_id: member.id.clone(),
                address: member.address.clone(),
                subnet,
                tunnel_ip: node_ip,
            },
        );
        
        // Set up tunnel to the new node
        self.overlay.lock().await.add_tunnel(
            &member.id,
            node_ip,
        ).await?;
        
        Ok(())
    }
    
    pub async fn handle_node_left(&self, member: &Member) -> Result<()> {
        println!("Tearing down networking for node: {}", member.id);
        
        // Remove tunnel to the node
        self.overlay.lock().await.remove_tunnel(&member.id).await?;
        
        // Release the node's subnet
        self.ipam.lock().await.release_node_subnet(&member.id)?;
        
        // Remove node network info
        let mut nodes = self.nodes.lock().await;
        nodes.remove(&member.id);
        
        Ok(())
    }
    
    pub async fn apply_network_policy(&self, policy: NetworkPolicy) -> Result<()> {
        self.policies.lock().await.add_policy(policy).await
    }
    
    pub async fn remove_network_policy(&self, name: &str) -> Result<()> {
        self.policies.lock().await.remove_policy(name).await
    }
    
    // Accessor methods for health checks
    
    pub async fn get_nodes(&self) -> HashMap<String, NodeNetworkInfo> {
        self.nodes.lock().await.clone()
    }
    
    pub async fn get_tunnels(&self) -> HashMap<String, TunnelInfo> {
        self.overlay.lock().await.get_tunnels().await
    }
    
    pub async fn get_policies(&self) -> Vec<NetworkPolicy> {
        self.policies.lock().await.list_policies().await
    }
}

#[derive(Debug)]
pub struct IpamManager {
    network_cidr: IpNetwork,
    node_subnet_size: u8,
    allocated_subnets: HashMap<String, IpNetwork>,
    allocated_ips: HashMap<String, HashMap<String, IpAddr>>,
}

impl IpamManager {
    pub fn new(network_cidr: IpNetwork, node_subnet_size: u8) -> Self {
        Self {
            network_cidr,
            node_subnet_size,
            allocated_subnets: HashMap::new(),
            allocated_ips: HashMap::new(),
        }
    }
    
    pub fn allocate_node_subnet(&mut self, node_id: &str) -> Result<IpNetwork> {
        // Check if this node already has a subnet
        if let Some(subnet) = self.allocated_subnets.get(node_id) {
            return Ok(*subnet);
        }
        
        // Only support IPv4 for now
        if let IpNetwork::V4(network) = self.network_cidr {
            // Calculate how many subnets we can have
            let network_bits = network.prefix();
            let subnet_bits = self.node_subnet_size;
            
            if subnet_bits <= network_bits {
                anyhow::bail!("Subnet size must be larger than network size");
            }
            
            let num_subnets = 2u32.pow((subnet_bits - network_bits) as u32);
            
            // Find an available subnet
            for i in 0..num_subnets {
                let subnet_addr = u32::from(network.network()) + (i << (32 - subnet_bits));
                let subnet_ip = Ipv4Addr::from(subnet_addr);
                
                if let Ok(subnet) = IpNetwork::new(
                    IpAddr::V4(subnet_ip),
                    subnet_bits,
                ) {
                    // Check if this subnet is already allocated
                    if !self.allocated_subnets.values().any(|s| *s == subnet) {
                        // Allocate this subnet
                        self.allocated_subnets.insert(node_id.to_string(), subnet);
                        
                        // Initialize container IP allocation for this node
                        self.allocated_ips.insert(node_id.to_string(), HashMap::new());
                        
                        return Ok(subnet);
                    }
                }
            }
            
            anyhow::bail!("No available subnets");
        } else {
            anyhow::bail!("IPv6 not supported yet");
        }
    }
    
    pub fn release_node_subnet(&mut self, node_id: &str) -> Result<()> {
        // Remove the subnet allocation
        self.allocated_subnets.remove(node_id);
        
        // Remove container IP allocations for this node
        self.allocated_ips.remove(node_id);
        
        Ok(())
    }
    
    pub fn allocate_container_ip(&mut self, node_id: &str, container_id: &str) -> Result<IpAddr> {
        // Get the node's subnet
        let subnet = self.allocated_subnets.get(node_id)
            .context("Node subnet not found")?;
        
        // Get or create the node's IP allocations
        let node_ips = self.allocated_ips.entry(node_id.to_string())
            .or_insert_with(HashMap::new);
        
        // Check if this container already has an IP
        if let Some(ip) = node_ips.get(container_id) {
            return Ok(*ip);
        }
        
        // Allocate a new IP from the subnet
        match subnet {
            IpNetwork::V4(subnet) => {
                let mut iter = subnet.iter();
                
                // Skip the network address and gateway
                iter.next(); // Network address
                iter.next(); // Gateway
                
                // Find an available IP
                for ip in iter {
                    let ip_addr = IpAddr::V4(ip);
                    
                    // Check if this IP is already allocated
                    if !node_ips.values().any(|i| *i == ip_addr) {
                        // Allocate this IP
                        node_ips.insert(container_id.to_string(), ip_addr);
                        return Ok(ip_addr);
                    }
                }
                
                anyhow::bail!("No available IPs in subnet");
            }
            IpNetwork::V6(_) => {
                anyhow::bail!("IPv6 not supported yet");
            }
        }
    }
    
    pub fn release_container_ip(&mut self, node_id: &str, container_id: &str) -> Result<()> {
        // Get the node's IP allocations
        if let Some(node_ips) = self.allocated_ips.get_mut(node_id) {
            // Remove the container's IP allocation
            node_ips.remove(container_id);
        }
        
        Ok(())
    }
}

#[derive(Debug)]
pub struct OverlayNetwork {
    network_type: OverlayNetworkType,
    vxlan_id: u16,
    vxlan_port: u16,
    tunnels: Arc<Mutex<HashMap<String, TunnelInfo>>>,
    local_node_id: Arc<Mutex<Option<String>>>,
    local_ip: Arc<Mutex<Option<IpAddr>>>,
}

impl OverlayNetwork {
    pub fn new(
        network_type: OverlayNetworkType,
        vxlan_id: u16,
        vxlan_port: u16,
    ) -> Self {
        Self {
            network_type,
            vxlan_id,
            vxlan_port,
            tunnels: Arc::new(Mutex::new(HashMap::new())),
            local_node_id: Arc::new(Mutex::new(None)),
            local_ip: Arc::new(Mutex::new(None)),
        }
    }
    
    pub async fn initialize(&self, node_id: &str, node_ip: IpAddr) -> Result<()> {
        // Store local node info
        *self.local_node_id.lock().await = Some(node_id.to_string());
        *self.local_ip.lock().await = Some(node_ip);
        
        Ok(())
    }
    
    pub async fn setup_local_network(
        &self,
        node_id: &str,
        node_ip: IpAddr,
        subnet: IpNetwork,
        bridge_name: &str,
    ) -> Result<()> {
        match self.network_type {
            OverlayNetworkType::VXLAN => {
                self.setup_vxlan(node_id, node_ip, subnet, bridge_name).await?;
            }
            OverlayNetworkType::Geneve => {
                // Not implemented yet
                anyhow::bail!("Geneve overlay not implemented yet");
            }
        }
        
        Ok(())
    }
    
    async fn setup_vxlan(
        &self,
        _node_id: &str,
        node_ip: IpAddr,
        _subnet: IpNetwork,
        bridge_name: &str,
    ) -> Result<()> {
        // Create VXLAN interface
        let vxlan_name = "vxlan0";
        
        // Check if VXLAN interface already exists
        if !Self::interface_exists(vxlan_name)? {
            // Create VXLAN interface
            Self::create_vxlan_interface(
                vxlan_name,
                self.vxlan_id,
                self.vxlan_port,
                node_ip,
            )?;
            
            // Connect VXLAN to bridge
            Self::connect_to_bridge(vxlan_name, bridge_name)?;
            
            // Enable VXLAN interface
            Self::enable_interface(vxlan_name)?;
        }
        
        Ok(())
    }
    
    fn interface_exists(interface: &str) -> Result<bool> {
        let output = Command::new("ip")
            .args(&["link", "show", interface])
            .output()?;
        
        Ok(output.status.success())
    }
    
    fn create_vxlan_interface(
        name: &str,
        vxlan_id: u16,
        port: u16,
        local_ip: IpAddr,
    ) -> Result<()> {
        let status = Command::new("ip")
            .args(&[
                "link", "add", name, "type", "vxlan",
                "id", &vxlan_id.to_string(),
                "dstport", &port.to_string(),
                "local", &local_ip.to_string(),
                "dev", "eth0",
            ])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to create VXLAN interface {}", name);
        }
        
        Ok(())
    }
    
    fn connect_to_bridge(interface: &str, bridge: &str) -> Result<()> {
        let status = Command::new("ip")
            .args(&["link", "set", interface, "master", bridge])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to connect {} to bridge {}", interface, bridge);
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
    
    pub async fn add_tunnel(&self, node_id: &str, remote_ip: IpAddr) -> Result<()> {
        let local_node_id = self.local_node_id.lock().await.clone()
            .context("Local node ID not set")?;
        let local_ip = self.local_ip.lock().await.clone()
            .context("Local IP not set")?;
        
        // Skip if this is the local node
        if node_id == local_node_id {
            return Ok(());
        }
        
        // Check if tunnel already exists
        let mut tunnels = self.tunnels.lock().await;
        if tunnels.contains_key(node_id) {
            return Ok(());
        }
        
        // Create tunnel
        match self.network_type {
            OverlayNetworkType::VXLAN => {
                // For VXLAN, we just need to add the remote node to the FDB
                Self::add_vxlan_fdb_entry("vxlan0", remote_ip)?;
            }
            OverlayNetworkType::Geneve => {
                // Not implemented yet
                anyhow::bail!("Geneve overlay not implemented yet");
            }
        }
        
        // Store tunnel info
        tunnels.insert(
            node_id.to_string(),
            TunnelInfo {
                node_id: node_id.to_string(),
                remote_ip,
                local_ip,
                tunnel_name: "vxlan0".to_string(),
            },
        );
        
        println!("Added tunnel to node {}", node_id);
        Ok(())
    }
    
    fn add_vxlan_fdb_entry(vxlan_if: &str, remote_ip: IpAddr) -> Result<()> {
        // Add remote node to VXLAN FDB
        let status = Command::new("bridge")
            .args(&[
                "fdb", "append", "00:00:00:00:00:00", "dev", vxlan_if,
                "dst", &remote_ip.to_string(),
            ])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to add FDB entry for {}", remote_ip);
        }
        
        Ok(())
    }
    
    pub async fn remove_tunnel(&self, node_id: &str) -> Result<()> {
        // Get tunnel info
        let mut tunnels = self.tunnels.lock().await;
        let tunnel = tunnels.remove(node_id);
        
        if let Some(tunnel) = tunnel {
            // Remove tunnel
            match self.network_type {
                OverlayNetworkType::VXLAN => {
                    // For VXLAN, we just need to remove the remote node from the FDB
                    Self::remove_vxlan_fdb_entry(&tunnel.tunnel_name, tunnel.remote_ip)?;
                }
                OverlayNetworkType::Geneve => {
                    // Not implemented yet
                    anyhow::bail!("Geneve overlay not implemented yet");
                }
            }
            
            println!("Removed tunnel to node {}", node_id);
        }
        
        Ok(())
    }
    
    fn remove_vxlan_fdb_entry(vxlan_if: &str, remote_ip: IpAddr) -> Result<()> {
        // Remove remote node from VXLAN FDB
        let status = Command::new("bridge")
            .args(&[
                "fdb", "del", "00:00:00:00:00:00", "dev", vxlan_if,
                "dst", &remote_ip.to_string(),
            ])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to remove FDB entry for {}", remote_ip);
        }
        
        Ok(())
    }
    
    // Accessor method for health checks
    pub async fn get_tunnels(&self) -> HashMap<String, TunnelInfo> {
        self.tunnels.lock().await.clone()
    }
}

#[derive(Debug)]
pub struct NetworkPolicyManager {
    policies: HashMap<String, NetworkPolicy>,
}

impl NetworkPolicyManager {
    pub fn new() -> Self {
        Self {
            policies: HashMap::new(),
        }
    }
    
    pub async fn add_policy(&mut self, policy: NetworkPolicy) -> Result<()> {
        // Store policy
        self.policies.insert(policy.name.clone(), policy.clone());
        
        // Apply policy rules
        self.apply_policy_rules(&policy).await?;
        
        Ok(())
    }
    
    pub async fn remove_policy(&mut self, name: &str) -> Result<()> {
        // Get policy
        let policy = self.policies.remove(name);
        
        if let Some(policy) = policy {
            // Remove policy rules
            self.remove_policy_rules(&policy).await?;
        }
        
        Ok(())
    }
    
    async fn apply_policy_rules(&self, policy: &NetworkPolicy) -> Result<()> {
        // In a real implementation, this would apply iptables/nftables rules
        // For now, just log the policy
        println!("Applying network policy: {}", policy.name);
        
        Ok(())
    }
    
    async fn remove_policy_rules(&self, policy: &NetworkPolicy) -> Result<()> {
        // In a real implementation, this would remove iptables/nftables rules
        // For now, just log the policy
        println!("Removing network policy: {}", policy.name);
        
        Ok(())
    }
    
    pub async fn get_policy(&self, name: &str) -> Option<NetworkPolicy> {
        self.policies.get(name).cloned()
    }
    
    pub async fn list_policies(&self) -> Vec<NetworkPolicy> {
        self.policies.values().cloned().collect()
    }
}

// End of file
