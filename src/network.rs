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
        static_ip: Option<IpAddr>,
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
        
        // Unregister container from service discovery
        let dns_name = format!("container-{}.hivemind", &container_id[..8]);
        self.service_discovery.deregister_service(
            &dns_name,
            &node_id,
            "", // We don't need the IP address as we're using the service name
            53,  // DNS port
        ).await?;
        
        println!("Unregistered container {} from DNS", container_id);
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
    static_ip_assignments: HashMap<String, IpAddr>,
    recycled_ips: HashMap<String, Vec<IpAddr>>,
}

impl IpamManager {
    pub fn new(network_cidr: IpNetwork, node_subnet_size: u8) -> Self {
        Self {
            network_cidr,
            node_subnet_size,
            allocated_subnets: HashMap::new(),
            allocated_ips: HashMap::new(),
            static_ip_assignments: HashMap::new(),
            recycled_ips: HashMap::new(),
        }
    }
    
    // Assign a static IP to a container
    pub fn assign_static_ip(&mut self, container_id: &str, ip: IpAddr) -> Result<()> {
        // Validate that the IP is within the network CIDR
        if !self.is_ip_in_network(ip) {
            anyhow::bail!("IP {} is not within the network CIDR {}", ip, self.network_cidr);
        }
        
        // Check if the IP is already assigned
        for (_, ips) in &self.allocated_ips {
            if ips.values().any(|&assigned_ip| assigned_ip == ip) {
                anyhow::bail!("IP {} is already assigned to another container", ip);
            }
        }
        
        // Store the static IP assignment
        self.static_ip_assignments.insert(container_id.to_string(), ip);
        
        Ok(())
    }
    
    // Check if an IP is within the network CIDR
    fn is_ip_in_network(&self, ip: IpAddr) -> bool {
        match (ip, self.network_cidr) {
            (IpAddr::V4(ipv4), IpNetwork::V4(network)) => network.contains(ipv4),
            (IpAddr::V6(ipv6), IpNetwork::V6(network)) => network.contains(ipv6),
            _ => false,
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
        // Check if this container has a static IP assignment
        if let Some(&ip) = self.static_ip_assignments.get(container_id) {
            // Validate that the static IP is within the node's subnet
            let subnet = self.allocated_subnets.get(node_id)
                .context("Node subnet not found")?;
                
            if !self.is_ip_in_subnet(ip, *subnet) {
                anyhow::bail!("Static IP {} is not within node {}'s subnet {}", ip, node_id, subnet);
            }
            
            // Get or create the node's IP allocations
            let node_ips = self.allocated_ips.entry(node_id.to_string())
                .or_insert_with(HashMap::new);
                
            // Assign the static IP
            node_ips.insert(container_id.to_string(), ip);
            
            // Remove from static assignments as it's now allocated
            self.static_ip_assignments.remove(container_id);
            
            return Ok(ip);
        }
        
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
        
        // Check if there are any recycled IPs for this node
        if let Some(recycled) = self.recycled_ips.get_mut(node_id) {
            if let Some(ip) = recycled.pop() {
                // Reuse a recycled IP
                node_ips.insert(container_id.to_string(), ip);
                
                // If the recycled list is now empty, remove it
                if recycled.is_empty() {
                    self.recycled_ips.remove(node_id);
                }
                
                println!("Reusing recycled IP {} for container {}", ip, container_id);
                return Ok(ip);
            }
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
                        println!("Allocated new IP {} for container {}", ip_addr, container_id);
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
    
    // Check if an IP is within a subnet
    fn is_ip_in_subnet(&self, ip: IpAddr, subnet: IpNetwork) -> bool {
        match (ip, subnet) {
            (IpAddr::V4(ipv4), IpNetwork::V4(network)) => network.contains(ipv4),
            (IpAddr::V6(ipv6), IpNetwork::V6(network)) => network.contains(ipv6),
            _ => false,
        }
    }
    
    pub fn release_container_ip(&mut self, node_id: &str, container_id: &str) -> Result<()> {
        // Get the node's IP allocations
        if let Some(node_ips) = self.allocated_ips.get_mut(node_id) {
            // Get the container's IP allocation
            if let Some(ip) = node_ips.remove(container_id) {
                // Add the IP to the recycled list for this node
                let recycled = self.recycled_ips.entry(node_id.to_string())
                    .or_insert_with(Vec::new);
                    
                recycled.push(ip);
                println!("Recycled IP {} from container {}", ip, container_id);
            }
        }
        
        Ok(())
    }
    
    // Get all allocated IPs for a node
    pub fn get_node_allocated_ips(&self, node_id: &str) -> Vec<(String, IpAddr)> {
        if let Some(node_ips) = self.allocated_ips.get(node_id) {
            node_ips.iter()
                .map(|(container_id, ip)| (container_id.clone(), *ip))
                .collect()
        } else {
            Vec::new()
        }
    }
    
    // Get all recycled IPs for a node
    pub fn get_node_recycled_ips(&self, node_id: &str) -> Vec<IpAddr> {
        if let Some(recycled) = self.recycled_ips.get(node_id) {
            recycled.clone()
        } else {
            Vec::new()
        }
    }
}

#[derive(Debug)]
pub struct OverlayNetwork {
    network_type: OverlayNetworkType,
    vxlan_id: u16,
    vxlan_port: u16,
    mtu: u32,
    tunnels: Arc<Mutex<HashMap<String, TunnelInfo>>>,
    local_node_id: Arc<Mutex<Option<String>>>,
    local_ip: Arc<Mutex<Option<IpAddr>>>,
    tunnel_interface: String,
}

impl OverlayNetwork {
    pub fn new(
        network_type: OverlayNetworkType,
        vxlan_id: u16,
        vxlan_port: u16,
    ) -> Self {
        // Default MTU is 1450 to accommodate VXLAN overhead (1500 - 50)
        Self {
            network_type,
            vxlan_id,
            vxlan_port,
            mtu: 1450,
            tunnels: Arc::new(Mutex::new(HashMap::new())),
            local_node_id: Arc::new(Mutex::new(None)),
            local_ip: Arc::new(Mutex::new(None)),
            tunnel_interface: "vxlan0".to_string(),
        }
    }
    
    // Set custom MTU for the overlay network
    pub fn with_mtu(mut self, mtu: u32) -> Self {
        self.mtu = mtu;
        self
    }
    
    // Set custom tunnel interface name
    pub fn with_tunnel_interface(mut self, interface_name: String) -> Self {
        self.tunnel_interface = interface_name;
        self
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
        node_id: &str,
        node_ip: IpAddr,
        subnet: IpNetwork,
        bridge_name: &str,
    ) -> Result<()> {
        // Create VXLAN interface
        let vxlan_name = &self.tunnel_interface;
        
        println!("Setting up VXLAN overlay for node {} with subnet {}", node_id, subnet);
        
        // Check if VXLAN interface already exists
        if !Self::interface_exists(vxlan_name)? {
            // Create VXLAN interface
            Self::create_vxlan_interface(
                vxlan_name,
                self.vxlan_id,
                self.vxlan_port,
                node_ip,
            )?;
            
            // Set MTU on VXLAN interface
            Self::set_interface_mtu(vxlan_name, self.mtu)?;
            
            // Connect VXLAN to bridge
            Self::connect_to_bridge(vxlan_name, bridge_name)?;
            
            // Enable VXLAN interface
            Self::enable_interface(vxlan_name)?;
            
            // Enable IP forwarding for cross-node routing
            Self::enable_ip_forwarding()?;
        }
        
        // Set up ARP proxy for container subnet
        Self::setup_arp_proxy(vxlan_name, subnet)?;
        
        // Set up masquerading for external traffic
        Self::setup_masquerading(subnet)?;
        
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
        // Get the main interface name
        let main_interface = Self::get_main_interface()?;
        
        let status = Command::new("ip")
            .args(&[
                "link", "add", name, "type", "vxlan",
                "id", &vxlan_id.to_string(),
                "dstport", &port.to_string(),
                "local", &local_ip.to_string(),
                "dev", &main_interface,
                "nolearning", // Disable MAC learning, we'll manage FDB entries manually
                "ttl", "64", // Set TTL for VXLAN packets
            ])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to create VXLAN interface {}", name);
        }
        
        Ok(())
    }
    
    // Get the main network interface
    fn get_main_interface() -> Result<String> {
        // Try to get the default route interface
        let output = Command::new("ip")
            .args(&["route", "get", "8.8.8.8"])
            .output()?;
            
        if output.status.success() {
            let output_str = String::from_utf8_lossy(&output.stdout);
            // Parse the output to find the interface name
            for part in output_str.split_whitespace() {
                if part == "dev" {
                    if let Some(interface) = output_str.split_whitespace().skip_while(|&p| p != "dev").nth(1) {
                        return Ok(interface.to_string());
                    }
                }
            }
        }
        
        // Fallback to eth0 if we can't determine the interface
        Ok("eth0".to_string())
    }
    
    // Set MTU on an interface
    fn set_interface_mtu(interface: &str, mtu: u32) -> Result<()> {
        let status = Command::new("ip")
            .args(&["link", "set", "dev", interface, "mtu", &mtu.to_string()])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to set MTU on interface {}", interface);
        }
        
        Ok(())
    }
    
    // Enable IP forwarding
    fn enable_ip_forwarding() -> Result<()> {
        // Enable IPv4 forwarding
        let status = Command::new("sysctl")
            .args(&["-w", "net.ipv4.ip_forward=1"])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to enable IP forwarding");
        }
        
        Ok(())
    }
    
    // Set up ARP proxy for a subnet
    fn setup_arp_proxy(interface: &str, subnet: IpNetwork) -> Result<()> {
        // Enable proxy ARP on the interface
        let status = Command::new("sysctl")
            .args(&["-w", &format!("net.ipv4.conf.{}.proxy_arp=1", interface)])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to enable proxy ARP on {}", interface);
        }
        
        // Add proxy ARP entry for the subnet
        let status = Command::new("ip")
            .args(&["neigh", "add", "proxy", &subnet.to_string(), "dev", interface])
            .status()?;
            
        // Ignore errors as this might fail if the entry already exists
        
        Ok(())
    }
    
    // Set up masquerading for external traffic
    fn setup_masquerading(subnet: IpNetwork) -> Result<()> {
        // Check if iptables is available
        let iptables_check = Command::new("which")
            .arg("iptables")
            .output()?;
            
        if !iptables_check.status.success() {
            println!("Warning: iptables not found, skipping masquerading setup");
            return Ok(());
        }
        
        // Add masquerading rule
        let status = Command::new("iptables")
            .args(&["-t", "nat", "-A", "POSTROUTING", "-s", &subnet.to_string(),
                   "-j", "MASQUERADE"])
            .status()?;
            
        if !status.success() {
            println!("Warning: Failed to set up masquerading for {}", subnet);
            // Don't fail the whole setup for this
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
        
        println!("Creating tunnel to node {} at {}", node_id, remote_ip);
        
        // Create tunnel
        match self.network_type {
            OverlayNetworkType::VXLAN => {
                // For VXLAN, we need to add the remote node to the FDB
                Self::add_vxlan_fdb_entry(&self.tunnel_interface, remote_ip)?;
                
                // Add route to remote subnet via VXLAN
                if let Some(remote_subnet) = self.get_remote_subnet(node_id).await {
                    Self::add_route_to_subnet(remote_subnet, remote_ip, &self.tunnel_interface)?;
                }
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
                tunnel_name: self.tunnel_interface.clone(),
            },
        );
        
        println!("Added tunnel to node {}", node_id);
        Ok(())
    }
    
    // Get the subnet allocated to a remote node
    async fn get_remote_subnet(&self, node_id: &str) -> Option<IpNetwork> {
        // This would typically come from a distributed store
        // For now, we'll use a simple heuristic based on node ID
        
        // Parse the node ID as a number if possible
        if let Ok(node_num) = node_id.parse::<u8>() {
            // Create a subnet based on node number
            if let Ok(network) = IpNetwork::from_str(&format!("10.244.{}.0/24", node_num)) {
                return Some(network);
            }
        }
        
        // If we can't determine the subnet, return None
        None
    }
    
    // Add a route to a remote subnet
    fn add_route_to_subnet(subnet: IpNetwork, via: IpAddr, dev: &str) -> Result<()> {
        let status = Command::new("ip")
            .args(&["route", "replace", &subnet.to_string(), "via", &via.to_string(), "dev", dev])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to add route to subnet {}", subnet);
        }
        
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
        
        // Also add a permanent ARP entry for the remote node
        // This helps with faster packet forwarding
        let status = Command::new("ip")
            .args(&[
                "neigh", "add", &remote_ip.to_string(), "lladdr", "00:00:00:00:00:00",
                "dev", vxlan_if, "nud", "permanent",
            ])
            .status()?;
        
        // Ignore errors for the ARP entry as it's not critical
        
        Ok(())
    }
    
    pub async fn remove_tunnel(&self, node_id: &str) -> Result<()> {
        // Get tunnel info
        let mut tunnels = self.tunnels.lock().await;
        let tunnel = tunnels.remove(node_id);
        
        if let Some(tunnel) = tunnel {
            println!("Removing tunnel to node {} at {}", node_id, tunnel.remote_ip);
            
            // Remove tunnel
            match self.network_type {
                OverlayNetworkType::VXLAN => {
                    // For VXLAN, we need to remove the remote node from the FDB
                    Self::remove_vxlan_fdb_entry(&tunnel.tunnel_name, tunnel.remote_ip)?;
                    
                    // Remove route to remote subnet
                    if let Some(remote_subnet) = self.get_remote_subnet(node_id).await {
                        Self::remove_route_to_subnet(remote_subnet)?;
                    }
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
    
    // Remove a route to a remote subnet
    fn remove_route_to_subnet(subnet: IpNetwork) -> Result<()> {
        let status = Command::new("ip")
            .args(&["route", "del", &subnet.to_string()])
            .status()?;
            
        if !status.success() {
            // Don't fail if the route doesn't exist
            println!("Warning: Failed to remove route to subnet {}", subnet);
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
    container_labels: HashMap<String, HashMap<String, String>>,
    chain_prefix: String,
}

impl NetworkPolicyManager {
    pub fn new() -> Self {
        Self {
            policies: HashMap::new(),
            container_labels: HashMap::new(),
            chain_prefix: "HIVEMIND".to_string(),
        }
    }
    
    // Set custom chain prefix for iptables rules
    pub fn with_chain_prefix(mut self, prefix: String) -> Self {
        self.chain_prefix = prefix;
        self
    }
    
    // Register container labels for policy matching
    pub async fn register_container_labels(&mut self, container_id: &str, labels: HashMap<String, String>) -> Result<()> {
        self.container_labels.insert(container_id.to_string(), labels);
        
        // Apply all existing policies to this container
        for policy in self.policies.values() {
            self.apply_policy_to_container(policy, container_id).await?;
        }
        
        Ok(())
    }
    
    // Unregister container labels when container is removed
    pub async fn unregister_container_labels(&mut self, container_id: &str) -> Result<()> {
        self.container_labels.remove(container_id);
        Ok(())
    }
    
    pub async fn add_policy(&mut self, policy: NetworkPolicy) -> Result<()> {
        println!("Adding network policy: {}", policy.name);
        
        // Store policy
        self.policies.insert(policy.name.clone(), policy.clone());
        
        // Apply policy rules to all matching containers
        self.apply_policy_rules(&policy).await?;
        
        Ok(())
    }
    
    pub async fn remove_policy(&mut self, name: &str) -> Result<()> {
        // Get policy
        let policy = self.policies.remove(name);
        
        if let Some(policy) = policy {
            println!("Removing network policy: {}", policy.name);
            
            // Remove policy rules
            self.remove_policy_rules(&policy).await?;
        }
        
        Ok(())
    }
    
    async fn apply_policy_rules(&self, policy: &NetworkPolicy) -> Result<()> {
        // Find all containers that match the policy selector
        let matching_containers = self.find_matching_containers(&policy.selector);
        
        println!("Applying policy {} to {} matching containers", policy.name, matching_containers.len());
        
        // Apply policy to each matching container
        for container_id in matching_containers {
            self.apply_policy_to_container(policy, &container_id).await?;
        }
        
        Ok(())
    }
    
    async fn apply_policy_to_container(&self, policy: &NetworkPolicy, container_id: &str) -> Result<()> {
        // Create iptables chains for this policy and container
        self.create_policy_chains(policy, container_id)?;
        
        // Apply ingress rules
        for rule in &policy.ingress_rules {
            self.apply_ingress_rule(policy, container_id, rule)?;
        }
        
        // Apply egress rules
        for rule in &policy.egress_rules {
            self.apply_egress_rule(policy, container_id, rule)?;
        }
        
        Ok(())
    }
    
    fn create_policy_chains(&self, policy: &NetworkPolicy, container_id: &str) -> Result<()> {
        // Create chain names
        let ingress_chain = self.get_ingress_chain_name(policy, container_id);
        let egress_chain = self.get_egress_chain_name(policy, container_id);
        
        // Create ingress chain
        let status = Command::new("iptables")
            .args(&["-N", &ingress_chain])
            .status();
            
        // Ignore errors if chain already exists
        
        // Create egress chain
        let status = Command::new("iptables")
            .args(&["-N", &egress_chain])
            .status();
            
        // Ignore errors if chain already exists
        
        // Add jumps to the chains
        // For ingress: FORWARD -> HIVEMIND-IN-<policy>-<container>
        let status = Command::new("iptables")
            .args(&[
                "-I", "FORWARD", "1",
                "-m", "comment", "--comment", &format!("Hivemind policy: {}", policy.name),
                "-j", &ingress_chain
            ])
            .status();
            
        // Ignore errors
        
        // For egress: FORWARD -> HIVEMIND-OUT-<policy>-<container>
        let status = Command::new("iptables")
            .args(&[
                "-I", "FORWARD", "1",
                "-m", "comment", "--comment", &format!("Hivemind policy: {}", policy.name),
                "-j", &egress_chain
            ])
            .status();
            
        // Ignore errors
        
        Ok(())
    }
    
    fn apply_ingress_rule(&self, policy: &NetworkPolicy, container_id: &str, rule: &NetworkRule) -> Result<()> {
        let chain = self.get_ingress_chain_name(policy, container_id);
        
        // Get container's interface
        let container_if = format!("veth{}", &container_id[..8]);
        
        // Apply rules for each port range
        for port_range in &rule.ports {
            // Apply rules for each peer
            for peer in &rule.from {
                // Handle IP block peers
                if let Some(ip_block) = &peer.ip_block {
                    self.add_cidr_rule(&chain, &container_if, ip_block, port_range, true)?;
                }
                
                // Handle selector peers
                if let Some(selector) = &peer.selector {
                    // Find containers matching the selector
                    let peer_containers = self.find_matching_containers(selector);
                    
                    // Add rule for each matching container
                    for peer_id in peer_containers {
                        // Skip if this is the same container
                        if peer_id == container_id {
                            continue;
                        }
                        
                        // Get peer container's interface
                        let peer_if = format!("veth{}", &peer_id[..8]);
                        
                        // Add rule
                        self.add_interface_rule(&chain, &container_if, &peer_if, port_range, true)?;
                    }
                }
            }
        }
        
        Ok(())
    }
    
    fn apply_egress_rule(&self, policy: &NetworkPolicy, container_id: &str, rule: &NetworkRule) -> Result<()> {
        let chain = self.get_egress_chain_name(policy, container_id);
        
        // Get container's interface
        let container_if = format!("veth{}", &container_id[..8]);
        
        // Apply rules for each port range
        for port_range in &rule.ports {
            // Apply rules for each peer
            for peer in &rule.from {
                // Handle IP block peers
                if let Some(ip_block) = &peer.ip_block {
                    self.add_cidr_rule(&chain, &container_if, ip_block, port_range, false)?;
                }
                
                // Handle selector peers
                if let Some(selector) = &peer.selector {
                    // Find containers matching the selector
                    let peer_containers = self.find_matching_containers(selector);
                    
                    // Add rule for each matching container
                    for peer_id in peer_containers {
                        // Skip if this is the same container
                        if peer_id == container_id {
                            continue;
                        }
                        
                        // Get peer container's interface
                        let peer_if = format!("veth{}", &peer_id[..8]);
                        
                        // Add rule
                        self.add_interface_rule(&chain, &container_if, &peer_if, port_range, false)?;
                    }
                }
            }
        }
        
        Ok(())
    }
    
    fn add_cidr_rule(&self, chain: &str, interface: &str, cidr: &IpNetwork, port_range: &PortRange, ingress: bool) -> Result<()> {
        // Determine source and destination for the rule
        let (src, dst) = if ingress {
            (format!("{}", cidr), interface)
        } else {
            (interface, format!("{}", cidr))
        };
        
        // Add rule
        let status = Command::new("iptables")
            .args(&[
                "-A", chain,
                "-p", &format!("{:?}", port_range.protocol).to_lowercase(),
                "-s", &src,
                "-d", &dst,
                "--dport", &format!("{}:{}", port_range.port_min, port_range.port_max),
                "-j", "ACCEPT"
            ])
            .status();
            
        // Ignore errors
        
        Ok(())
    }
    
    fn add_interface_rule(&self, chain: &str, interface1: &str, interface2: &str, port_range: &PortRange, ingress: bool) -> Result<()> {
        // Determine source and destination interfaces
        let (src_if, dst_if) = if ingress {
            (interface2, interface1)
        } else {
            (interface1, interface2)
        };
        
        // Add rule
        let status = Command::new("iptables")
            .args(&[
                "-A", chain,
                "-p", &format!("{:?}", port_range.protocol).to_lowercase(),
                "-i", src_if,
                "-o", dst_if,
                "--dport", &format!("{}:{}", port_range.port_min, port_range.port_max),
                "-j", "ACCEPT"
            ])
            .status();
            
        // Ignore errors
        
        Ok(())
    }
    
    async fn remove_policy_rules(&self, policy: &NetworkPolicy) -> Result<()> {
        // Find all containers that match the policy selector
        let matching_containers = self.find_matching_containers(&policy.selector);
        
        // Remove policy from each matching container
        for container_id in matching_containers {
            self.remove_policy_from_container(policy, &container_id)?;
        }
        
        Ok(())
    }
    
    fn remove_policy_from_container(&self, policy: &NetworkPolicy, container_id: &str) -> Result<()> {
        // Get chain names
        let ingress_chain = self.get_ingress_chain_name(policy, container_id);
        let egress_chain = self.get_egress_chain_name(policy, container_id);
        
        // Remove jumps to the chains
        let status = Command::new("iptables")
            .args(&[
                "-D", "FORWARD",
                "-m", "comment", "--comment", &format!("Hivemind policy: {}", policy.name),
                "-j", &ingress_chain
            ])
            .status();
            
        // Ignore errors
        
        let status = Command::new("iptables")
            .args(&[
                "-D", "FORWARD",
                "-m", "comment", "--comment", &format!("Hivemind policy: {}", policy.name),
                "-j", &egress_chain
            ])
            .status();
            
        // Ignore errors
        
        // Flush and delete chains
        let status = Command::new("iptables")
            .args(&["-F", &ingress_chain])
            .status();
            
        // Ignore errors
        
        let status = Command::new("iptables")
            .args(&["-X", &ingress_chain])
            .status();
            
        // Ignore errors
        
        let status = Command::new("iptables")
            .args(&["-F", &egress_chain])
            .status();
            
        // Ignore errors
        
        let status = Command::new("iptables")
            .args(&["-X", &egress_chain])
            .status();
            
        // Ignore errors
        
        Ok(())
    }
    
    // Find containers that match a selector
    fn find_matching_containers(&self, selector: &NetworkSelector) -> Vec<String> {
        let mut matching = Vec::new();
        
        // Check each container's labels against the selector
        for (container_id, labels) in &self.container_labels {
            if self.labels_match_selector(labels, &selector.labels) {
                matching.push(container_id.clone());
            }
        }
        
        matching
    }
    
    // Check if container labels match a selector
    fn labels_match_selector(&self, container_labels: &HashMap<String, String>, selector_labels: &HashMap<String, String>) -> bool {
        // All selector labels must be present in the container labels with matching values
        for (key, value) in selector_labels {
            if !container_labels.contains_key(key) || container_labels.get(key) != Some(value) {
                return false;
            }
        }
        
        true
    }
    
    // Generate ingress chain name for a policy and container
    fn get_ingress_chain_name(&self, policy: &NetworkPolicy, container_id: &str) -> String {
        format!("{}-IN-{}-{}", self.chain_prefix, policy.name, &container_id[..8])
    }
    
    // Generate egress chain name for a policy and container
    fn get_egress_chain_name(&self, policy: &NetworkPolicy, container_id: &str) -> String {
        format!("{}-OUT-{}-{}", self.chain_prefix, policy.name, &container_id[..8])
    }
    
    pub async fn get_policy(&self, name: &str) -> Option<NetworkPolicy> {
        self.policies.get(name).cloned()
    }
    
    pub async fn list_policies(&self) -> Vec<NetworkPolicy> {
        self.policies.values().cloned().collect()
    }
}

// End of file
