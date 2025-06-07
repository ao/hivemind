use crate::membership::Member;
use crate::node::NodeManager;
use crate::service_discovery::ServiceDiscovery;
use crate::tenant::TenantContext;
use anyhow::{Context, Result};
use ipnetwork::IpNetwork;
use serde::{Deserialize, Serialize};
use std::collections::{HashMap, HashSet};
use std::net::{IpAddr, Ipv4Addr};
use std::process::Command;
use std::str::FromStr;
use std::sync::Arc;
use tokio::sync::Mutex;
use uuid::Uuid;

// Import the NetworkPolicyController
use crate::security::network_policy_controller::NetworkPolicyController;

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

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq)]
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
    // CIDR pool for tenant networks
    tenant_cidr_pool: Arc<Mutex<CidrPoolManager>>,
    // Network initialization retry configuration
    max_init_retries: u32,
    retry_backoff_ms: u64,
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
        
        // Create CIDR pool manager for tenant networks
        // Default tenant CIDR range is 10.100.0.0/16 to 10.200.0.0/16
        let tenant_cidr_pool = Arc::new(Mutex::new(CidrPoolManager::new(
            "10.100.0.0/16".to_string(),
            24,
            100, // Maximum number of tenant networks
        )));
        
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
            tenant_cidr_pool,
            max_init_retries: 3,
            retry_backoff_ms: 1000, // 1 second backoff
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
        println!("Setting up container network for tenant {}", tenant_context.tenant_id);
        
        // Check if tenant overlay network exists, if not create it
        self.ensure_tenant_overlay_network(&tenant_context.tenant_id, None).await?;
        
        // Create container network config with tenant context
        let config = self.create_container_network_config_with_tenant(
            container_id,
            &self.node_manager.get_node_id(),
            tenant_context,
            static_ip,
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
        self.policies.lock().await.register_container_labels(container_id, all_labels.clone()).await?;
        
        // Register container IP for policy enforcement
        self.policies.lock().await.register_container_ip(container_id, config.ip_address).await?;
        
        // Apply tenant-specific network policies
        self.apply_tenant_network_policies(&tenant_context.tenant_id, container_id, &config).await?;
        
        Ok(config)
    }
    
    // Initialize tenant network with specified CIDR
    pub async fn initialize_tenant_network(&self, tenant_id: &str, cidr: Option<String>) -> Result<()> {
        info!("Initializing network for tenant {}", tenant_id);
        
        // Validate tenant ID
        if tenant_id.is_empty() {
            return Err(anyhow::anyhow!("Tenant ID cannot be empty"));
        }
        
        // Check if tenant network already exists
        let tenant_overlays = self.tenant_overlays.lock().await;
        if tenant_overlays.contains_key(tenant_id) {
            info!("Tenant network already initialized for {}", tenant_id);
            return Ok(());
        }
        drop(tenant_overlays); // Release the lock
        
        // Allocate CIDR for tenant network
        let tenant_cidr = match cidr {
            Some(cidr_str) => {
                // Validate the provided CIDR
                let parsed_cidr = IpNetwork::from_str(&cidr_str)
                    .map_err(|e| anyhow::anyhow!("Invalid CIDR format: {}", e))?;
                
                // Check for CIDR overlap with existing tenant networks
                if !self.validate_tenant_cidr(&cidr_str, tenant_id).await? {
                    return Err(anyhow::anyhow!("CIDR {} overlaps with existing tenant networks", cidr_str));
                }
                
                cidr_str
            },
            None => {
                // Allocate a CIDR from the pool
                let mut cidr_pool = self.tenant_cidr_pool.lock().await;
                cidr_pool.allocate_cidr(tenant_id)?
            }
        };
        
        // Store the tenant CIDR
        let mut tenant_network_cidrs = self.tenant_network_cidrs.lock().await;
        tenant_network_cidrs.insert(tenant_id.to_string(), tenant_cidr.clone());
        drop(tenant_network_cidrs); // Release the lock
        
        // Initialize the tenant overlay network with retry logic
        self.initialize_tenant_overlay_with_retry(tenant_id, &tenant_cidr).await?;
        
        info!("Tenant network initialized successfully for {} with CIDR {}", tenant_id, tenant_cidr);
        Ok(())
    }
    
    // Initialize tenant overlay network with retry logic
    async fn initialize_tenant_overlay_with_retry(&self, tenant_id: &str, cidr_str: &str) -> Result<()> {
        let mut attempt = 0;
        let max_attempts = self.max_init_retries;
        let backoff_ms = self.retry_backoff_ms;
        
        loop {
            attempt += 1;
            info!("Attempt {} of {} to initialize tenant overlay network for {}", attempt, max_attempts, tenant_id);
            
            match self.ensure_tenant_overlay_network(tenant_id, Some(cidr_str.to_string())).await {
                Ok(_) => {
                    info!("Successfully initialized tenant overlay network for {} on attempt {}", tenant_id, attempt);
                    return Ok(());
                },
                Err(e) => {
                    if attempt >= max_attempts {
                        error!("Failed to initialize tenant overlay network for {} after {} attempts: {}", tenant_id, max_attempts, e);
                        return Err(anyhow::anyhow!("Failed to initialize tenant overlay network after {} attempts: {}", max_attempts, e));
                    }
                    
                    warn!("Attempt {} failed for tenant {}: {}. Retrying in {}ms...", attempt, tenant_id, e, backoff_ms);
                    tokio::time::sleep(tokio::time::Duration::from_millis(backoff_ms)).await;
                }
            }
        }
    }
    
    // Validate that a CIDR doesn't overlap with existing tenant networks
    async fn validate_tenant_cidr(&self, cidr_str: &str, exclude_tenant_id: &str) -> Result<bool> {
        let new_cidr = IpNetwork::from_str(cidr_str)?;
        let tenant_network_cidrs = self.tenant_network_cidrs.lock().await;
        
        for (tenant_id, existing_cidr_str) in tenant_network_cidrs.iter() {
            // Skip the tenant we're validating for
            if tenant_id == exclude_tenant_id {
                continue;
            }
            
            let existing_cidr = IpNetwork::from_str(existing_cidr_str)?;
            
            // Check for overlap
            if new_cidr.contains(existing_cidr.network()) ||
               existing_cidr.contains(new_cidr.network()) {
                return Ok(false);
            }
        }
        
        Ok(true)
    }
    
    // Ensure tenant overlay network exists with optional CIDR
    async fn ensure_tenant_overlay_network(&self, tenant_id: &str, cidr: Option<String>) -> Result<()> {
        let mut tenant_overlays = self.tenant_overlays.lock().await;
        
        if !tenant_overlays.contains_key(tenant_id) {
            info!("Creating overlay network for tenant {}", tenant_id);
            
            // Create a new overlay network for this tenant
            let tenant_vxlan_id = 100 + tenant_id.as_bytes().iter().fold(0u16, |acc, &b| acc.wrapping_add(b as u16));
            let tenant_overlay = OverlayNetwork::new(
                self.config.overlay_type.clone(),
                tenant_vxlan_id,
                self.config.vxlan_port,
            ).with_mtu(self.config.mtu)
             .with_tunnel_interface(format!("vxlan-{}", &tenant_id[..8]));
            
            // Initialize the tenant overlay network
            let node_id = self.node_manager.get_node_id();
            let node_ip = self.get_node_ip().await?;
            
            // Allocate a subnet for this tenant
            let mut tenant_network_cidrs = self.tenant_network_cidrs.lock().await;
            if !tenant_network_cidrs.contains_key(tenant_id) {
                if let Some(cidr_str) = cidr {
                    // Use provided CIDR
                    tenant_network_cidrs.insert(tenant_id.to_string(), cidr_str);
                } else {
                    // Allocate a new CIDR for this tenant
                    // Use a different subnet range for each tenant to ensure isolation
                    let tenant_num = tenant_id.as_bytes().iter().fold(0u8, |acc, &b| acc.wrapping_add(b));
                    let tenant_cidr = format!("10.100.{}.0/24", tenant_num);
                    tenant_network_cidrs.insert(tenant_id.to_string(), tenant_cidr);
                }
            }
            
            // Get the tenant's network CIDR
            let tenant_cidr_str = tenant_network_cidrs.get(tenant_id).unwrap().clone();
            let tenant_cidr = IpNetwork::from_str(&tenant_cidr_str)
                .map_err(|e| anyhow::anyhow!("Invalid CIDR format: {}", e))?;
            
            // Initialize the overlay network
            let mut tenant_overlay_instance = tenant_overlay;
            tenant_overlay_instance.initialize(&node_id, node_ip).await?;
            
            // Create the bridge for this tenant
            let bridge_name = format!("hivemind-{}", &tenant_id[..8]);
            let gateway_ip = Self::get_gateway_ip(tenant_cidr)?;
            
            // Create bridge if it doesn't exist
            if !Self::bridge_exists(&bridge_name)? {
                Self::create_bridge(&bridge_name)?;
                Self::set_bridge_ip(&bridge_name, gateway_ip, tenant_cidr.prefix())?;
                Self::enable_bridge(&bridge_name)?;
            }
            
            // Set up the overlay network
            tenant_overlay_instance.setup_local_network(
                &node_id,
                node_ip,
                tenant_cidr,
                &bridge_name,
            ).await?;
            
            // Store the tenant overlay network
            tenant_overlays.insert(tenant_id.to_string(), Arc::new(Mutex::new(tenant_overlay_instance)));
            
            info!("Tenant overlay network created successfully for {}", tenant_id);
        }
        
        Ok(())
    }
    
    /// Verify and repair tenant network configuration
    /// This method checks if the tenant network is properly configured and repairs any inconsistencies
    pub async fn verify_tenant_network(&self, tenant_id: &str) -> Result<bool> {
        info!("Verifying network configuration for tenant {}", tenant_id);
        
        let mut repairs_made = false;
        
        // Check if tenant CIDR exists
        let tenant_cidr = {
            let tenant_network_cidrs = self.tenant_network_cidrs.lock().await;
            match tenant_network_cidrs.get(tenant_id) {
                Some(cidr) => cidr.clone(),
                None => {
                    warn!("Tenant {} missing CIDR configuration, allocating new one", tenant_id);
                    drop(tenant_network_cidrs); // Release the lock before calling allocate_cidr
                    
                    // Allocate a new CIDR for this tenant
                    let mut cidr_pool = self.tenant_cidr_pool.lock().await;
                    let new_cidr = cidr_pool.allocate_cidr(tenant_id)?;
                    
                    // Store the tenant CIDR
                    let mut tenant_network_cidrs = self.tenant_network_cidrs.lock().await;
                    tenant_network_cidrs.insert(tenant_id.to_string(), new_cidr.clone());
                    
                    repairs_made = true;
                    new_cidr
                }
            }
        };
        
        // Check if tenant overlay network exists
        let tenant_overlay_exists = {
            let tenant_overlays = self.tenant_overlays.lock().await;
            tenant_overlays.contains_key(tenant_id)
        };
        
        if !tenant_overlay_exists {
            warn!("Tenant {} missing overlay network, initializing", tenant_id);
            
            // Initialize the tenant overlay network with retry logic
            self.initialize_tenant_overlay_with_retry(tenant_id, &tenant_cidr).await?;
            
            repairs_made = true;
        }
        
        // Check if bridge exists
        let bridge_name = format!("hivemind-{}", &tenant_id[..8]);
        if !Self::bridge_exists(&bridge_name)? {
            warn!("Tenant {} missing bridge {}, creating", tenant_id, bridge_name);
            
            // Parse the tenant CIDR
            let tenant_cidr_network = IpNetwork::from_str(&tenant_cidr)?;
            let gateway_ip = Self::get_gateway_ip(tenant_cidr_network)?;
            
            // Create and configure the bridge
            Self::create_bridge(&bridge_name)?;
            Self::set_bridge_ip(&bridge_name, gateway_ip, tenant_cidr_network.prefix())?;
            Self::enable_bridge(&bridge_name)?;
            
            repairs_made = true;
        }
        
        // Check if network policies exist
        let policies = self.policies.lock().await;
        let has_isolation_policy = policies.values().any(|p|
            p.name == format!("tenant-{}-isolation", tenant_id) &&
            p.tenant_id.as_ref().map_or(false, |id| id == tenant_id)
        );
        drop(policies);
        
        if !has_isolation_policy {
            warn!("Tenant {} missing network policies, creating defaults", tenant_id);
            
            // Create default network policies
            self.create_default_tenant_policies(tenant_id).await?;
            
            repairs_made = true;
        }
        
        if repairs_made {
            info!("Completed repairs for tenant {} network configuration", tenant_id);
        } else {
            debug!("Tenant {} network configuration verified, no repairs needed", tenant_id);
        }
        
        Ok(repairs_made)
    }
    
    /// Verify and repair all tenant networks
    /// This method checks all tenant networks and repairs any inconsistencies
    pub async fn verify_all_tenant_networks(&self) -> Result<HashMap<String, bool>> {
        info!("Verifying all tenant networks");
        
        let mut repair_results = HashMap::new();
        
        // Get all tenant IDs from various sources
        let mut tenant_ids = HashSet::new();
        
        // Add tenant IDs from network CIDRs
        {
            let tenant_network_cidrs = self.tenant_network_cidrs.lock().await;
            for tenant_id in tenant_network_cidrs.keys() {
                tenant_ids.insert(tenant_id.clone());
            }
        }
        
        // Add tenant IDs from overlay networks
        {
            let tenant_overlays = self.tenant_overlays.lock().await;
            for tenant_id in tenant_overlays.keys() {
                tenant_ids.insert(tenant_id.clone());
            }
        }
        
        // Add tenant IDs from network policies
        {
            let policies = self.policies.lock().await;
            for policy in policies.values() {
                if let Some(tenant_id) = &policy.tenant_id {
                    tenant_ids.insert(tenant_id.clone());
                }
            }
        }
        
        // Verify each tenant network
        for tenant_id in tenant_ids {
            match self.verify_tenant_network(&tenant_id).await {
                Ok(repairs_made) => {
                    repair_results.insert(tenant_id, repairs_made);
                },
                Err(e) => {
                    error!("Failed to verify tenant network for {}: {}", tenant_id, e);
                    repair_results.insert(tenant_id, false);
                }
            }
        }
        
        let repair_count = repair_results.values().filter(|&repaired| *repaired).count();
        info!("Completed verification of all tenant networks: {} tenants, {} repairs",
            repair_results.len(), repair_count);
        
        Ok(repair_results)
    }
    
    /// Run periodic maintenance tasks for network management
    /// This method should be called regularly to ensure network consistency
    pub async fn run_maintenance(&self) -> Result<()> {
        info!("Running network maintenance tasks");
        
        // Verify and repair all tenant networks
        let repair_results = self.verify_all_tenant_networks().await?;
        
        // Check for orphaned resources
        self.cleanup_orphaned_resources().await?;
        
        // Run reconciliation for network policies
        if let Some(policies) = &self.policies {
            policies.lock().await.run_reconciliation().await?;
        }
        
        info!("Network maintenance completed successfully");
        
        Ok(())
    }
    
    /// Cleanup orphaned network resources
    /// This method identifies and removes network resources that are no longer needed
    async fn cleanup_orphaned_resources(&self) -> Result<()> {
        debug!("Checking for orphaned network resources");
        
        // Find orphaned bridges (bridges without associated tenant)
        let mut orphaned_bridges = Vec::new();
        
        // Get all tenant IDs
        let tenant_ids: HashSet<String> = {
            let tenant_network_cidrs = self.tenant_network_cidrs.lock().await;
            tenant_network_cidrs.keys().cloned().collect()
        };
        
        // Check for bridges that don't match any tenant
        let output = Command::new("ip")
            .args(&["link", "show", "type", "bridge"])
            .output()?;
            
        if output.status.success() {
            let output_str = String::from_utf8_lossy(&output.stdout);
            
            for line in output_str.lines() {
                if line.contains("hivemind-") {
                    // Extract bridge name
                    if let Some(start) = line.find("hivemind-") {
                        let end = line[start..].find(':').unwrap_or(line[start..].len()) + start;
                        let bridge_name = &line[start..end];
                        
                        // Extract tenant ID from bridge name
                        if bridge_name.len() > 9 { // "hivemind-" + at least 1 char
                            let tenant_prefix = &bridge_name[9..];
                            
                            // Check if this bridge belongs to any known tenant
                            let belongs_to_tenant = tenant_ids.iter().any(|id| {
                                if id.len() >= 8 {
                                    &id[..8] == tenant_prefix
                                } else {
                                    false
                                }
                            });
                            
                            if !belongs_to_tenant {
                                orphaned_bridges.push(bridge_name.to_string());
                            }
                        }
                    }
                }
            }
        }
        
        // Remove orphaned bridges
        for bridge in orphaned_bridges {
            warn!("Removing orphaned bridge: {}", bridge);
            
            let status = Command::new("ip")
                .args(&["link", "set", &bridge, "down"])
                .status()?;
                
            if status.success() {
                let status = Command::new("ip")
                    .args(&["link", "delete", &bridge, "type", "bridge"])
                    .status()?;
                    
                if status.success() {
                    info!("Successfully removed orphaned bridge: {}", bridge);
                } else {
                    warn!("Failed to remove orphaned bridge: {}", bridge);
                }
            }
        }
        
        Ok(())
    }
    
    // Apply tenant-specific network policies
    async fn apply_tenant_network_policies(&self, tenant_id: &str, container_id: &str, config: &ContainerNetworkConfig) -> Result<()> {
        info!("Applying network policies for tenant {} container {}", tenant_id, container_id);
        
        // Create default isolation policy for tenant
        let policy = NetworkPolicy {
            name: format!("tenant-{}-isolation", tenant_id),
            selector: NetworkSelector {
                labels: {
                    let mut labels = HashMap::new();
                    labels.insert("tenant".to_string(), tenant_id.to_string());
                    labels
                },
            },
            ingress_rules: vec![
                // Allow traffic from same tenant
                NetworkRule {
                    ports: vec![],  // All ports
                    from: vec![
                        NetworkPeer {
                            ip_block: None,
                            selector: Some(NetworkSelector {
                                labels: {
                                    let mut labels = HashMap::new();
                                    labels.insert("tenant".to_string(), tenant_id.to_string());
                                    labels
                                },
                            }),
                        },
                    ],
                    action: Some(PolicyAction::Allow),
                    log: false,
                    description: Some(format!("Allow traffic from tenant {}", tenant_id)),
                    id: Some(format!("tenant-{}-ingress-allow", tenant_id)),
                },
                // Deny traffic from other tenants
                NetworkRule {
                    ports: vec![],  // All ports
                    from: vec![],   // All sources not matched by previous rules
                    action: Some(PolicyAction::Deny),
                    log: true,
                    description: Some(format!("Deny traffic from other tenants to {}", tenant_id)),
                    id: Some(format!("tenant-{}-ingress-deny", tenant_id)),
                },
            ],
            egress_rules: vec![
                // Allow all outbound traffic (can be restricted further if needed)
                NetworkRule {
                    ports: vec![],  // All ports
                    from: vec![],   // All destinations
                    action: Some(PolicyAction::Allow),
                    log: false,
                    description: Some(format!("Allow all outbound traffic from tenant {}", tenant_id)),
                    id: Some(format!("tenant-{}-egress-allow", tenant_id)),
                },
            ],
            priority: 100,
            namespace: None,
            tenant_id: Some(tenant_id.to_string()),
            labels: HashMap::new(),
            created_at: std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_secs(),
            updated_at: std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_secs(),
        };
        
        // Apply the policy
        self.policies.lock().await.apply_policy(policy).await?;
        
        Ok(())
    }
    
    // Create container network config with tenant context
    async fn create_container_network_config_with_tenant(
        &self,
        container_id: &str,
        node_id: &str,
        tenant_context: &TenantContext,
        static_ip: Option<IpAddr>,
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
            let mut tenant_overlay_instance = tenant_overlay;
            tenant_overlay_instance.initialize(node_id, node_ip).await?;
            
            // Store the tenant overlay network
            tenant_overlays.insert(tenant_id.clone(), Arc::new(Mutex::new(tenant_overlay_instance)));
        }
        
        // Get node subnet from the tenant's CIDR
        let nodes = self.nodes.lock().await;
        let node_info = nodes.get(node_id).context("Node network info not found")?;
        
        // Handle static IP assignment if provided
        if let Some(ip) = &static_ip {
            self.ipam.lock().await.assign_static_ip(container_id, *ip)?;
        }
        
        // Allocate IP for container from tenant's CIDR
        let ip = self.ipam.lock().await.allocate_container_ip(node_id, container_id)?;
        
        // Generate MAC address
        let mac_address = Self::generate_mac_address(container_id);
        
        // Get gateway IP
        let gateway = Self::get_gateway_ip(node_info.subnet)?;
        
        // Get DNS servers - use the gateway as the primary DNS server
        let mut dns_servers = vec![gateway];
        
        // In a real implementation, we would get DNS servers from service discovery
        // For now, we'll just use the gateway as the DNS server
        // No need to extend dns_servers
        
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
        
        // In a real implementation, we would get DNS servers from service discovery
        // For now, we'll just use the gateway as the DNS server
        // No need to extend dns_servers
        
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
            labels: labels.clone().unwrap_or_default(),
        };
        
        // Store container network config for future reference
        let mut container_networks = self.container_networks.lock().await;
        container_networks.insert(container_id.to_string(), config.clone());
        
        // Set up container network namespace
        self.setup_container_namespace(pid, &config).await?;
        
        // Register container in service discovery for DNS resolution
        self.register_container_dns(container_id, ip).await?;
        
        // Register container with policy manager
        if let Some(labels) = labels {
            self.policies.lock().await.register_container_labels(container_id, labels).await?;
            self.policies.lock().await.register_container_ip(container_id, ip).await?;
        }
        
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
        create_veth_pair(&host_if, &container_if, pid)?;
        
        // Connect host end to bridge
        connect_to_bridge(&host_if, "hivemind0")?;
        
        // Configure container interface
        configure_container_interface(
            pid,
            &container_if,
            config.ip_address,
            config.mac_address.as_str(),
            config.gateway,
        )?;
        
        // Enable interfaces
        enable_interface(&host_if)?;
        enable_container_interface(pid, &container_if)?;
        
        // Add default route in container
        add_container_default_route(pid, config.gateway)?;
        
        // Configure DNS resolution in container
        configure_container_dns(pid, config.gateway)?;
        
        Ok(())
    }
    
    // Generate MAC address from container ID
    fn generate_mac_address(container_id: &str) -> String {
        // Use first 6 bytes of container ID to generate MAC address
        // Format: 02:42:ac:XX:XX:XX (02:42:ac is Docker's prefix for container MACs)
        let mut mac = String::from("02:42:ac");
        
        // Use container ID bytes to generate last 3 bytes of MAC
        for i in 0..3 {
            if i < container_id.len() {
                let byte = container_id.as_bytes()[i];
                mac.push_str(&format!(":{:02x}", byte));
            } else {
                mac.push_str(":00");
            }
        }
        
        mac
    }
}

// NetworkPolicyManager handles network policies
#[derive(Debug)]
pub struct NetworkPolicyManager {
    controller: Arc<Mutex<crate::security::network_policy_controller::NetworkPolicyController>>,
}

impl NetworkPolicyManager {
    pub fn new() -> Self {
        Self {
            controller: Arc::new(Mutex::new(crate::security::network_policy_controller::NetworkPolicyController::new())),
        }
    }
    
    // Register container labels for policy matching
    pub async fn register_container_labels(&mut self, container_id: &str, labels: HashMap<String, String>) -> Result<()> {
        // Store the labels in the controller
        let mut controller = self.controller.lock().await;
        
        // Check if we already have an IP for this container
        if let Some(ip) = controller.get_container_ip(container_id).await {
            // If we have both IP and labels, register the container
            controller.register_container(container_id, ip, labels).await?;
        } else {
            // Otherwise just store the labels
            controller.store_container_labels(container_id, labels).await?;
        }
        
        Ok(())
    }
    
    // Register container IP address
    pub async fn register_container_ip(&mut self, container_id: &str, ip: IpAddr) -> Result<()> {
        let mut controller = self.controller.lock().await;
        
        // Check if we already have labels for this container
        if let Some(labels) = controller.get_container_labels(container_id).await {
            // If we have both IP and labels, register the container
            controller.register_container(container_id, ip, labels).await?;
        } else {
            // Otherwise just store the IP
            controller.store_container_ip(container_id, ip).await?;
        }
        
        Ok(())
    }
    
    // Apply a network policy
    pub async fn apply_policy(&mut self, policy: NetworkPolicy) -> Result<()> {
        self.controller.lock().await.apply_policy(policy).await?;
        Ok(())
    }
}

// IPAM Manager handles IP address management
pub struct IpamManager {
    network_cidr: IpNetwork,
    node_subnet_size: u8,
    node_subnets: HashMap<String, IpNetwork>,
    container_ips: HashMap<String, IpAddr>,
    allocated_ips: HashMap<IpNetwork, HashSet<IpAddr>>,
}

impl IpamManager {
    pub fn new(network_cidr: IpNetwork, node_subnet_size: u8) -> Self {
        Self {
            network_cidr,
            node_subnet_size,
            node_subnets: HashMap::new(),
            container_ips: HashMap::new(),
            allocated_ips: HashMap::new(),
        }
    }
    
    // Allocate a subnet for a node
    pub fn allocate_node_subnet(&mut self, node_id: &str) -> Result<IpNetwork> {
        // Check if node already has a subnet
        if let Some(subnet) = self.node_subnets.get(node_id) {
            return Ok(*subnet);
        }
        
        // Calculate how many subnets we can have
        let subnet_bits = self.node_subnet_size - self.network_cidr.prefix();
        let max_subnets = 2u32.pow(subnet_bits as u32);
        
        // Find an available subnet
        for i in 0..max_subnets {
            let subnet_prefix = self.network_cidr.prefix() + subnet_bits;
            
            // Calculate subnet address
            match self.network_cidr {
                IpNetwork::V4(network) => {
                    let network_u32 = u32::from(network.network());
                    let subnet_size = 2u32.pow((32 - subnet_prefix) as u32);
                    let subnet_addr = network_u32 + i * subnet_size;
                    
                    // Convert back to IP
                    let subnet_ip = Ipv4Addr::from(subnet_addr);
                    let subnet = IpNetwork::V4(ipnetwork::Ipv4Network::new(subnet_ip, subnet_prefix)?);
                    
                    // Check if subnet is already allocated
                    if !self.node_subnets.values().any(|s| *s == subnet) {
                        // Allocate subnet
                        self.node_subnets.insert(node_id.to_string(), subnet);
                        self.allocated_ips.insert(subnet, HashSet::new());
                        return Ok(subnet);
                    }
                }
                IpNetwork::V6(_) => {
                    anyhow::bail!("IPv6 not supported yet");
                }
            }
        }
        
        anyhow::bail!("No available subnets")
    }
    
    // Release a node's subnet
    pub fn release_node_subnet(&mut self, node_id: &str) -> Result<()> {
        if let Some(subnet) = self.node_subnets.remove(node_id) {
            self.allocated_ips.remove(&subnet);
            
            // Remove container IPs for this node
            self.container_ips.retain(|_, ip| !subnet.contains(*ip));
        }
        
        Ok(())
    }
    
    // Allocate an IP for a container
    pub fn allocate_container_ip(&mut self, node_id: &str, container_id: &str) -> Result<IpAddr> {
        // Check if container already has an IP
        if let Some(ip) = self.container_ips.get(container_id) {
            return Ok(*ip);
        }
        
        // Get node's subnet
        let subnet = match self.node_subnets.get(node_id) {
            Some(subnet) => *subnet,
            None => anyhow::bail!("Node {} has no allocated subnet", node_id),
        };
        
        // Find an available IP in the subnet
        let allocated_ips = self.allocated_ips.get(&subnet).unwrap_or(&HashSet::new());
        
        match subnet {
            IpNetwork::V4(subnet) => {
                // Skip network address and gateway (first usable IP)
                let mut iter = subnet.iter();
                iter.next(); // Skip network address
                iter.next(); // Skip gateway
                
                // Find first unallocated IP
                for ip in iter {
                    let ip_addr = IpAddr::V4(ip);
                    if !allocated_ips.contains(&ip_addr) {
                        // Allocate IP
                        self.container_ips.insert(container_id.to_string(), ip_addr);
                        self.allocated_ips.get_mut(&subnet).unwrap().insert(ip_addr);
                        return Ok(ip_addr);
                    }
                }
                
                anyhow::bail!("No available IPs in subnet {}", subnet);
            }
            IpNetwork::V6(_) => {
                anyhow::bail!("IPv6 not supported yet");
            }
        }
    }
    
    // Release a container's IP
    pub fn release_container_ip(&mut self, node_id: &str, container_id: &str) -> Result<()> {
        if let Some(ip) = self.container_ips.remove(container_id) {
            // Find the subnet this IP belongs to
            if let Some(subnet) = self.node_subnets.get(node_id) {
                if let Some(allocated_ips) = self.allocated_ips.get_mut(subnet) {
                    allocated_ips.remove(&ip);
                }
            }
        }
        
        Ok(())
    }
    
    // Assign a static IP to a container
    pub fn assign_static_ip(&mut self, container_id: &str, ip: IpAddr) -> Result<()> {
        // Check if IP is within any of our subnets
        for (node_id, subnet) in &self.node_subnets {
            if subnet.contains(ip) {
                // Check if IP is already allocated
                if let Some(allocated_ips) = self.allocated_ips.get(subnet) {
                    if allocated_ips.contains(&ip) {
                        anyhow::bail!("IP {} is already allocated", ip);
                    }
                }
                
                // Allocate IP
                self.container_ips.insert(container_id.to_string(), ip);
                self.allocated_ips.get_mut(subnet).unwrap().insert(ip);
                return Ok(());
            }
        }
        
        anyhow::bail!("IP {} is not within any allocated subnet", ip);
    }
}

// CIDR Pool Manager for tenant networks
#[derive(Debug)]
pub struct CidrPoolManager {
    base_cidr: String,
    subnet_size: u8,
    max_subnets: u32,
    allocated_cidrs: HashMap<String, String>,
}

impl CidrPoolManager {
    pub fn new(base_cidr: String, subnet_size: u8, max_subnets: u32) -> Self {
        Self {
            base_cidr,
            subnet_size,
            max_subnets,
            allocated_cidrs: HashMap::new(),
        }
    }
    
    // Allocate a CIDR for a tenant
    pub fn allocate_cidr(&mut self, tenant_id: &str) -> Result<String> {
        // Check if tenant already has a CIDR
        if let Some(cidr) = self.allocated_cidrs.get(tenant_id) {
            return Ok(cidr.clone());
        }
        
        // Parse the base CIDR
        let base_network = IpNetwork::from_str(&self.base_cidr)?;
        
        // Calculate how many subnets we can have
        let subnet_bits = self.subnet_size - base_network.prefix();
        if subnet_bits <= 0 {
            return Err(anyhow::anyhow!("Subnet size must be larger than base CIDR prefix"));
        }
        
        let max_possible_subnets = 2u32.pow(subnet_bits as u32);
        let actual_max_subnets = std::cmp::min(self.max_subnets, max_possible_subnets);
        
        // Find an available subnet
        for i in 0..actual_max_subnets {
            match base_network {
                IpNetwork::V4(base_v4) => {
                    let base_addr = u32::from(base_v4.network());
                    let subnet_size_bits = 32 - self.subnet_size;
                    let subnet_size_ips = 2u32.pow(subnet_size_bits as u32);
                    let subnet_addr = base_addr + (i * subnet_size_ips);
                    
                    // Convert to IP and create CIDR string
                    let subnet_ip = Ipv4Addr::from(subnet_addr);
                    let cidr_str = format!("{}/{}", subnet_ip, self.subnet_size);
                    
                    // Check if this CIDR is already allocated
                    if !self.allocated_cidrs.values().any(|c| c == &cidr_str) {
                        // Allocate this CIDR
                        self.allocated_cidrs.insert(tenant_id.to_string(), cidr_str.clone());
                        return Ok(cidr_str);
                    }
                },
                IpNetwork::V6(_) => {
                    return Err(anyhow::anyhow!("IPv6 not supported yet"));
                }
            }
        }
        
        Err(anyhow::anyhow!("No available CIDRs in the pool"))
    }
    
    // Release a tenant's CIDR
    pub fn release_cidr(&mut self, tenant_id: &str) -> Result<()> {
        self.allocated_cidrs.remove(tenant_id);
        Ok(())
    }
    
    // Get all allocated CIDRs
    pub fn get_allocated_cidrs(&self) -> &HashMap<String, String> {
        &self.allocated_cidrs
    }
    
    // Check if a CIDR overlaps with any allocated CIDRs
    pub fn check_cidr_overlap(&self, cidr_str: &str) -> Result<bool> {
        let cidr = IpNetwork::from_str(cidr_str)?;
        
        for allocated_cidr_str in self.allocated_cidrs.values() {
            let allocated_cidr = IpNetwork::from_str(allocated_cidr_str)?;
            
            // Check for overlap
            if cidr.contains(allocated_cidr.network()) ||
               allocated_cidr.contains(cidr.network()) {
                return Ok(true);
            }
        }
        
        Ok(false)
    }
}

// OverlayNetwork manages the overlay network
pub struct OverlayNetwork {
    overlay_type: OverlayNetworkType,
    vxlan_id: u16,
    vxlan_port: u16,
    mtu: u32,
    tunnel_interface: String,
    encryption_enabled: bool,
    encryption_key: Option<String>,
    tunnels: HashMap<String, TunnelInfo>,
}

impl OverlayNetwork {
    pub fn new(overlay_type: OverlayNetworkType, vxlan_id: u16, vxlan_port: u16) -> Self {
        Self {
            overlay_type,
            vxlan_id,
            vxlan_port,
            mtu: 1450, // Default MTU for VXLAN
            tunnel_interface: "vxlan0".to_string(),
            encryption_enabled: false,
            encryption_key: None,
            tunnels: HashMap::new(),
        }
    }
    
    // Set MTU
    pub fn with_mtu(mut self, mtu: u32) -> Self {
        self.mtu = mtu;
        self
    }
    
    // Set tunnel interface name
    pub fn with_tunnel_interface(mut self, interface: String) -> Self {
        self.tunnel_interface = interface;
        self
    }
    
    // Enable encryption
    pub fn with_encryption(mut self, key: String) -> Self {
        self.encryption_enabled = true;
        self.encryption_key = Some(key);
        self
    }
    
    // Get MTU
    pub fn get_mtu(&self) -> u32 {
        self.mtu
    }
    
    // Get tunnel interface name
    pub fn get_tunnel_interface(&self) -> &str {
        &self.tunnel_interface
    }
    
    // Initialize overlay network
    pub async fn initialize(&mut self, node_id: &str, node_ip: IpAddr) -> Result<()> {
        println!("Initializing overlay network with type {:?}", self.overlay_type);
        
        // Create VXLAN interface if it doesn't exist
        if !self.interface_exists(&self.tunnel_interface)? {
            self.create_vxlan_interface(&self.tunnel_interface, self.vxlan_id, self.vxlan_port, node_ip)?;
            self.set_interface_mtu(&self.tunnel_interface, self.mtu)?;
            self.enable_interface(&self.tunnel_interface)?;
        }
        
        Ok(())
    }
    
    // Setup local network
    pub async fn setup_local_network(
        &mut self,
        node_id: &str,
        node_ip: IpAddr,
        subnet: IpNetwork,
        bridge_name: &str,
    ) -> Result<()> {
        // Connect VXLAN interface to bridge
        self.connect_to_bridge(&self.tunnel_interface, bridge_name)?;
        
        Ok(())
    }
    
    // Check if interface exists
    fn interface_exists(&self, interface: &str) -> Result<bool> {
        let output = Command::new("ip")
            .args(&["link", "show", interface])
            .output()?;
        
        Ok(output.status.success())
    }
    
    // Create VXLAN interface
    fn create_vxlan_interface(&self, interface: &str, vxlan_id: u16, vxlan_port: u16, local_ip: IpAddr) -> Result<()> {
        let status = Command::new("ip")
            .args(&[
                "link", "add", interface, "type", "vxlan",
                "id", &vxlan_id.to_string(),
                "dstport", &vxlan_port.to_string(),
                "local", &local_ip.to_string(),
                "dev", "eth0", // Assume eth0 is the main interface
            ])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to create VXLAN interface {}", interface);
        }
        
        Ok(())
    }
    
    // Set interface MTU
    fn set_interface_mtu(&self, interface: &str, mtu: u32) -> Result<()> {
        let status = Command::new("ip")
            .args(&["link", "set", interface, "mtu", &mtu.to_string()])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to set MTU on interface {}", interface);
        }
        
        Ok(())
    }
    
    // Enable interface
    fn enable_interface(&self, interface: &str) -> Result<()> {
        let status = Command::new("ip")
            .args(&["link", "set", interface, "up"])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to enable interface {}", interface);
        }
        
        Ok(())
    }
    
    // Connect interface to bridge
    fn connect_to_bridge(&self, interface: &str, bridge: &str) -> Result<()> {
        let status = Command::new("ip")
            .args(&["link", "set", interface, "master", bridge])
            .status()?;
            
        if !status.success() {
            anyhow::bail!("Failed to connect {} to bridge {}", interface, bridge);
        }
        
        Ok(())
    }
}

// Helper functions for container network setup
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

// Network validation tools
pub struct NetworkValidator {
    network_manager: Arc<NetworkManager>,
}

impl NetworkValidator {
    pub fn new(network_manager: Arc<NetworkManager>) -> Self {
        Self {
            network_manager,
        }
    }
    
    // Verify network isolation between tenants
    pub async fn verify_tenant_isolation(&self, tenant1_id: &str, tenant2_id: &str) -> Result<bool> {
        // Get tenant CIDRs
        let tenant_cidrs = self.network_manager.tenant_network_cidrs.lock().await;
        
        let tenant1_cidr = match tenant_cidrs.get(tenant1_id) {
            Some(cidr) => cidr.clone(),
            None => return Err(anyhow::anyhow!("Tenant {} has no allocated CIDR", tenant1_id)),
        };
        
        let tenant2_cidr = match tenant_cidrs.get(tenant2_id) {
            Some(cidr) => cidr.clone(),
            None => return Err(anyhow::anyhow!("Tenant {} has no allocated CIDR", tenant2_id)),
        };
        
        // Parse CIDRs
        let cidr1 = IpNetwork::from_str(&tenant1_cidr)?;
        let cidr2 = IpNetwork::from_str(&tenant2_cidr)?;
        
        // Check for overlap
        let isolated = !cidr1.contains(cidr2.network()) && !cidr2.contains(cidr1.network());
        
        Ok(isolated)
    }
    
    // Test connectivity between tenant networks
    pub async fn test_connectivity(&self, source_tenant: &str, dest_tenant: &str) -> Result<bool> {
        // This would normally involve creating test containers in each tenant network
        // and attempting to ping between them, but for now we'll just check isolation
        
        // If tenants are different, they should not be able to communicate
        if source_tenant != dest_tenant {
            return self.verify_tenant_isolation(source_tenant, dest_tenant).await;
        }
        
        // Same tenant should be able to communicate
        Ok(true)
    }
    
    // Validate network configuration
    pub async fn validate_network_config(&self, tenant_id: &str) -> Result<bool> {
        // Check if tenant has an overlay network
        let tenant_overlays = self.network_manager.tenant_overlays.lock().await;
        if !tenant_overlays.contains_key(tenant_id) {
            return Ok(false);
        }
        
        // Check if tenant has a CIDR
        let tenant_cidrs = self.network_manager.tenant_network_cidrs.lock().await;
        if !tenant_cidrs.contains_key(tenant_id) {
            return Ok(false);
        }
        
        // Check if the CIDR is valid
        let cidr_str = tenant_cidrs.get(tenant_id).unwrap();
        if IpNetwork::from_str(cidr_str).is_err() {
            return Ok(false);
        }
        
        Ok(true)
    }
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
