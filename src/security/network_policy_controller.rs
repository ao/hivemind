use anyhow::Result;
use std::collections::HashMap;
use std::net::IpAddr;
use std::process::Command;
use std::sync::Arc;
use tokio::sync::Mutex;
use std::time::{SystemTime, UNIX_EPOCH};

use crate::network::{
    NetworkPolicy, NetworkRule, NetworkSelector, NetworkPeer, PortRange, Protocol, PolicyAction
};

/// NetworkPolicyController translates network policies to actual network rules
pub struct NetworkPolicyController {
    // Map of policy name to iptables chain name
    policy_chains: Arc<Mutex<HashMap<String, String>>>,
    // Map of container ID to IP address
    container_ips: Arc<Mutex<HashMap<String, IpAddr>>>,
    // Map of container ID to labels
    container_labels: Arc<Mutex<HashMap<String, HashMap<String, String>>>>,
    // Temporary storage for container IPs and labels before both are available
    pending_ips: Arc<Mutex<HashMap<String, IpAddr>>>,
    pending_labels: Arc<Mutex<HashMap<String, HashMap<String, String>>>>,
    // Active policies
    policies: Arc<Mutex<HashMap<String, NetworkPolicy>>>,
    // Reconciliation settings
    reconciliation_interval: u64,
    last_reconciliation: Arc<Mutex<u64>>,
    // Violation logs
    violation_logs: Arc<Mutex<Vec<ViolationLog>>>,
    max_log_entries: usize,
}

/// Log entry for policy violations
#[derive(Debug, Clone)]
pub struct ViolationLog {
    pub id: String,
    pub timestamp: u64,
    pub source_container: String,
    pub source_ip: IpAddr,
    pub destination_container: Option<String>,
    pub destination_ip: IpAddr,
    pub protocol: Protocol,
    pub port: u16,
    pub policy_name: String,
    pub rule_id: Option<String>,
    pub action: PolicyAction,
    pub tenant_id: Option<String>,
}

impl NetworkPolicyController {
    pub fn new() -> Self {
        Self {
            policy_chains: Arc::new(Mutex::new(HashMap::new())),
            container_ips: Arc::new(Mutex::new(HashMap::new())),
            container_labels: Arc::new(Mutex::new(HashMap::new())),
            pending_ips: Arc::new(Mutex::new(HashMap::new())),
            pending_labels: Arc::new(Mutex::new(HashMap::new())),
            policies: Arc::new(Mutex::new(HashMap::new())),
            reconciliation_interval: 60, // Default to 60 seconds
            last_reconciliation: Arc::new(Mutex::new(0)),
            violation_logs: Arc::new(Mutex::new(Vec::new())),
            max_log_entries: 10000, // Store up to 10,000 log entries
        }
    }

    /// Register container labels for policy matching
    pub async fn register_container(&mut self, container_id: &str, ip: IpAddr, labels: HashMap<String, String>) -> Result<()> {
        // Store container IP
        self.container_ips.lock().await.insert(container_id.to_string(), ip);
        
        // Store container labels
        self.container_labels.lock().await.insert(container_id.to_string(), labels);
        
        // Apply existing policies to this container
        self.apply_policies_to_container(container_id).await?;
        
        Ok(())
    }
    
    /// Unregister container when it's removed
    pub async fn unregister_container(&mut self, container_id: &str) -> Result<()> {
        // Get container IP
        let ip = {
            let container_ips = self.container_ips.lock().await;
            match container_ips.get(container_id) {
                Some(ip) => *ip,
                None => return Ok(()),
            }
        };
        
        // Remove container rules
        self.remove_container_rules(container_id, ip).await?;
        
        // Remove container from maps
        self.container_ips.lock().await.remove(container_id);
        self.container_labels.lock().await.remove(container_id);
        
        Ok(())
    }
    
    /// Get container IP address
    pub async fn get_container_ip(&self, container_id: &str) -> Option<IpAddr> {
        self.container_ips.lock().await.get(container_id).copied()
    }
    
    /// Get container labels
    pub async fn get_container_labels(&self, container_id: &str) -> Option<HashMap<String, String>> {
        self.container_labels.lock().await.get(container_id).cloned()
    }
    
    /// Store container IP address temporarily
    pub async fn store_container_ip(&mut self, container_id: &str, ip: IpAddr) -> Result<()> {
        // Store IP in pending map
        self.pending_ips.lock().await.insert(container_id.to_string(), ip);
        
        // Check if we have labels for this container
        if let Some(labels) = self.pending_labels.lock().await.remove(container_id) {
            // If we have both IP and labels, register the container
            self.register_container(container_id, ip, labels).await?;
        }
        
        Ok(())
    }
    
    /// Store container labels temporarily
    pub async fn store_container_labels(&mut self, container_id: &str, labels: HashMap<String, String>) -> Result<()> {
        // Store labels in pending map
        self.pending_labels.lock().await.insert(container_id.to_string(), labels.clone());
        
        // Check if we have IP for this container
        if let Some(ip) = self.pending_ips.lock().await.remove(container_id) {
            // If we have both IP and labels, register the container
            self.register_container(container_id, ip, labels).await?;
        }
        
        Ok(())
    }
    
    /// Apply a network policy
    pub async fn apply_policy(&mut self, policy: NetworkPolicy) -> Result<()> {
        println!("Applying network policy: {}", policy.name);
        
        // Store the policy
        let mut policies = self.policies.lock().await;
        policies.insert(policy.name.clone(), policy.clone());
        drop(policies);
        
        // Create iptables chain for this policy if it doesn't exist
        let chain_name = self.get_policy_chain_name(&policy.name);
        self.ensure_chain_exists(&chain_name).await?;
        self.policy_chains.lock().await.insert(policy.name.clone(), chain_name.clone());
        
        // Find containers that match the policy selector
        let matching_containers = self.find_matching_containers(&policy.selector).await;
        
        // Apply policy to matching containers
        for container_id in matching_containers {
            let container_ips = self.container_ips.lock().await;
            if let Some(ip) = container_ips.get(&container_id) {
                self.apply_policy_to_container(&policy, &container_id, *ip).await?;
            }
        }
        
        println!("Network policy {} applied successfully", policy.name);
        
        Ok(())
    }
    
    /// Find containers that match a selector
    async fn find_matching_containers(&self, selector: &NetworkSelector) -> Vec<String> {
        let mut matching_containers = Vec::new();
        let container_labels = self.container_labels.lock().await;
        
        for (container_id, labels) in container_labels.iter() {
            if self.labels_match_selector(labels, &selector.labels) {
                matching_containers.push(container_id.clone());
            }
        }
        
        matching_containers
    }
    
    /// Apply policies to a specific container
    async fn apply_policies_to_container(&self, container_id: &str) -> Result<()> {
        let container_ip = {
            let container_ips = self.container_ips.lock().await;
            match container_ips.get(container_id) {
                Some(ip) => *ip,
                None => return Ok(()), // Container IP not registered yet
            }
        };
        
        let container_labels = {
            let all_labels = self.container_labels.lock().await;
            match all_labels.get(container_id) {
                Some(labels) => labels.clone(),
                None => return Ok(()), // Container labels not registered yet
            }
        };
        
        // Apply each policy that matches this container
        let policies = self.policies.lock().await;
        for policy in policies.values() {
            if self.labels_match_selector(&container_labels, &policy.selector.labels) {
                self.apply_policy_to_container(policy, container_id, container_ip).await?;
            }
        }
        
        Ok(())
    }
    
    /// Apply a policy to a specific container
    async fn apply_policy_to_container(&self, policy: &NetworkPolicy, container_id: &str, container_ip: IpAddr) -> Result<()> {
        // Get the chain name for this policy
        let chain_name = {
            let policy_chains = self.policy_chains.lock().await;
            match policy_chains.get(&policy.name) {
                Some(chain) => chain.clone(),
                None => {
                    let chain = self.get_policy_chain_name(&policy.name);
                    self.ensure_chain_exists(&chain).await?;
                    chain
                }
            }
        };
        
        // Apply ingress rules
        for rule in &policy.ingress_rules {
            self.apply_ingress_rule(rule, policy, container_id, container_ip, &chain_name).await?;
        }
        
        // Apply egress rules
        for rule in &policy.egress_rules {
            self.apply_egress_rule(rule, policy, container_id, container_ip, &chain_name).await?;
        }
        
        Ok(())
    }
    
    /// Apply an ingress rule to a container
    async fn apply_ingress_rule(
        &self, 
        rule: &NetworkRule, 
        policy: &NetworkPolicy,
        container_id: &str, 
        container_ip: IpAddr,
        chain_name: &str
    ) -> Result<()> {
        // For each port range in the rule
        for port_range in &rule.ports {
            // For each source in the rule
            if rule.from.is_empty() {
                // If no sources specified, apply to all traffic
                self.add_ingress_rule_for_all(
                    port_range, 
                    rule, 
                    policy,
                    container_id, 
                    container_ip, 
                    chain_name
                ).await?;
            } else {
                // Apply to specific sources
                for peer in &rule.from {
                    self.add_ingress_rule_for_peer(
                        port_range, 
                        rule, 
                        peer,
                        policy,
                        container_id, 
                        container_ip, 
                        chain_name
                    ).await?;
                }
            }
        }
        
        Ok(())
    }
    
    /// Apply an egress rule to a container
    async fn apply_egress_rule(
        &self, 
        rule: &NetworkRule, 
        policy: &NetworkPolicy,
        container_id: &str, 
        container_ip: IpAddr,
        chain_name: &str
    ) -> Result<()> {
        // For each port range in the rule
        for port_range in &rule.ports {
            // For each destination in the rule
            if rule.from.is_empty() {
                // If no destinations specified, apply to all traffic
                self.add_egress_rule_for_all(
                    port_range, 
                    rule, 
                    policy,
                    container_id, 
                    container_ip, 
                    chain_name
                ).await?;
            } else {
                // Apply to specific destinations
                for peer in &rule.from {
                    self.add_egress_rule_for_peer(
                        port_range, 
                        rule, 
                        peer,
                        policy,
                        container_id, 
                        container_ip, 
                        chain_name
                    ).await?;
                }
            }
        }
        
        Ok(())
    }
    
    /// Add ingress rule for all sources
    async fn add_ingress_rule_for_all(
        &self,
        port_range: &PortRange,
        rule: &NetworkRule,
        policy: &NetworkPolicy,
        container_id: &str,
        container_ip: IpAddr,
        chain_name: &str
    ) -> Result<()> {
        let protocol = match port_range.protocol {
            Protocol::TCP => "tcp",
            Protocol::UDP => "udp",
        };
        
        let action = match rule.action.as_ref().unwrap_or(&PolicyAction::Allow) {
            PolicyAction::Allow => "ACCEPT",
            PolicyAction::Deny => "DROP",
            PolicyAction::Limit(rate) => "ACCEPT", // Rate limiting would require additional setup
            PolicyAction::Log => "LOG",
        };
        
        // Create iptables rule
        let port_spec = if port_range.port_min == port_range.port_max {
            format!("--dport {}", port_range.port_min)
        } else {
            format!("--dport {}:{}", port_range.port_min, port_range.port_max)
        };
        
        let rule_spec = format!(
            "-A {} -p {} {} -d {} -j {}",
            chain_name, protocol, port_spec, container_ip, action
        );
        
        // Execute iptables command
        let status = Command::new("iptables")
            .args(&["-C", &rule_spec])
            .status();
            
        // If rule doesn't exist, add it
        if status.is_err() || !status.unwrap().success() {
            let status = Command::new("iptables")
                .args(&["-A", chain_name, "-p", protocol, &port_spec, "-d", &container_ip.to_string(), "-j", action])
                .status()?;
                
            if !status.success() {
                anyhow::bail!("Failed to add iptables rule for container {}", container_id);
            }
        }
        
        // If logging is enabled, add a logging rule
        if rule.log {
            let log_rule_spec = format!(
                "-A {} -p {} {} -d {} -j LOG --log-prefix \"HIVEMIND-POLICY-{}: \"",
                chain_name, protocol, port_spec, container_ip, policy.name
            );
            
            // Execute iptables command for logging
            let status = Command::new("iptables")
                .args(&["-C", &log_rule_spec])
                .status();
                
            // If rule doesn't exist, add it
            if status.is_err() || !status.unwrap().success() {
                let status = Command::new("iptables")
                    .args(&[
                        "-A", chain_name, 
                        "-p", protocol, 
                        &port_spec, 
                        "-d", &container_ip.to_string(), 
                        "-j", "LOG", 
                        "--log-prefix", &format!("HIVEMIND-POLICY-{}: ", policy.name)
                    ])
                    .status()?;
                    
                if !status.success() {
                    anyhow::bail!("Failed to add iptables logging rule for container {}", container_id);
                }
            }
        }
        
        Ok(())
    }
    
    /// Add ingress rule for specific peer
    async fn add_ingress_rule_for_peer(
        &self,
        port_range: &PortRange,
        rule: &NetworkRule,
        peer: &NetworkPeer,
        policy: &NetworkPolicy,
        container_id: &str,
        container_ip: IpAddr,
        chain_name: &str
    ) -> Result<()> {
        let protocol = match port_range.protocol {
            Protocol::TCP => "tcp",
            Protocol::UDP => "udp",
        };
        
        let action = match rule.action.as_ref().unwrap_or(&PolicyAction::Allow) {
            PolicyAction::Allow => "ACCEPT",
            PolicyAction::Deny => "DROP",
            PolicyAction::Limit(rate) => "ACCEPT", // Rate limiting would require additional setup
            PolicyAction::Log => "LOG",
        };
        
        // Create port specification
        let port_spec = if port_range.port_min == port_range.port_max {
            format!("--dport {}", port_range.port_min)
        } else {
            format!("--dport {}:{}", port_range.port_min, port_range.port_max)
        };
        
        // Handle IP block peer
        if let Some(ip_block) = &peer.ip_block {
            let rule_spec = format!(
                "-A {} -p {} {} -s {} -d {} -j {}",
                chain_name, protocol, port_spec, ip_block, container_ip, action
            );
            
            // Execute iptables command
            let status = Command::new("iptables")
                .args(&["-C", &rule_spec])
                .status();
                
            // If rule doesn't exist, add it
            if status.is_err() || !status.unwrap().success() {
                let status = Command::new("iptables")
                    .args(&[
                        "-A", chain_name, 
                        "-p", protocol, 
                        &port_spec, 
                        "-s", &ip_block.to_string(), 
                        "-d", &container_ip.to_string(), 
                        "-j", action
                    ])
                    .status()?;
                    
                if !status.success() {
                    anyhow::bail!("Failed to add iptables rule for container {}", container_id);
                }
            }
        }
        
        // Handle selector peer
        if let Some(selector) = &peer.selector {
            // Find containers that match the selector
            let matching_containers = self.find_matching_containers(selector).await;
            
            // Add rule for each matching container
            let container_ips = self.container_ips.lock().await;
            for src_container_id in matching_containers {
                if let Some(src_ip) = container_ips.get(&src_container_id) {
                    let rule_spec = format!(
                        "-A {} -p {} {} -s {} -d {} -j {}",
                        chain_name, protocol, port_spec, src_ip, container_ip, action
                    );
                    
                    // Execute iptables command
                    let status = Command::new("iptables")
                        .args(&["-C", &rule_spec])
                        .status();
                        
                    // If rule doesn't exist, add it
                    if status.is_err() || !status.unwrap().success() {
                        let status = Command::new("iptables")
                            .args(&[
                                "-A", chain_name, 
                                "-p", protocol, 
                                &port_spec, 
                                "-s", &src_ip.to_string(), 
                                "-d", &container_ip.to_string(), 
                                "-j", action
                            ])
                            .status()?;
                            
                        if !status.success() {
                            anyhow::bail!("Failed to add iptables rule for container {}", container_id);
                        }
                    }
                }
            }
        }
        
        Ok(())
    }
    
    /// Add egress rule for all destinations
    async fn add_egress_rule_for_all(
        &self,
        port_range: &PortRange,
        rule: &NetworkRule,
        policy: &NetworkPolicy,
        container_id: &str,
        container_ip: IpAddr,
        chain_name: &str
    ) -> Result<()> {
        let protocol = match port_range.protocol {
            Protocol::TCP => "tcp",
            Protocol::UDP => "udp",
        };
        
        let action = match rule.action.as_ref().unwrap_or(&PolicyAction::Allow) {
            PolicyAction::Allow => "ACCEPT",
            PolicyAction::Deny => "DROP",
            PolicyAction::Limit(rate) => "ACCEPT", // Rate limiting would require additional setup
            PolicyAction::Log => "LOG",
        };
        
        // Create iptables rule
        let port_spec = if port_range.port_min == port_range.port_max {
            format!("--dport {}", port_range.port_min)
        } else {
            format!("--dport {}:{}", port_range.port_min, port_range.port_max)
        };
        
        let rule_spec = format!(
            "-A {} -p {} {} -s {} -j {}",
            chain_name, protocol, port_spec, container_ip, action
        );
        
        // Execute iptables command
        let status = Command::new("iptables")
            .args(&["-C", &rule_spec])
            .status();
            
        // If rule doesn't exist, add it
        if status.is_err() || !status.unwrap().success() {
            let status = Command::new("iptables")
                .args(&["-A", chain_name, "-p", protocol, &port_spec, "-s", &container_ip.to_string(), "-j", action])
                .status()?;
                
            if !status.success() {
                anyhow::bail!("Failed to add iptables rule for container {}", container_id);
            }
        }
        
        // If logging is enabled, add a logging rule
        if rule.log {
            let log_rule_spec = format!(
                "-A {} -p {} {} -s {} -j LOG --log-prefix \"HIVEMIND-POLICY-{}: \"",
                chain_name, protocol, port_spec, container_ip, policy.name
            );
            
            // Execute iptables command for logging
            let status = Command::new("iptables")
                .args(&["-C", &log_rule_spec])
                .status();
                
            // If rule doesn't exist, add it
            if status.is_err() || !status.unwrap().success() {
                let status = Command::new("iptables")
                    .args(&[
                        "-A", chain_name, 
                        "-p", protocol, 
                        &port_spec, 
                        "-s", &container_ip.to_string(), 
                        "-j", "LOG", 
                        "--log-prefix", &format!("HIVEMIND-POLICY-{}: ", policy.name)
                    ])
                    .status()?;
                    
                if !status.success() {
                    anyhow::bail!("Failed to add iptables logging rule for container {}", container_id);
                }
            }
        }
        
        Ok(())
    }
    
    /// Add egress rule for specific peer
    async fn add_egress_rule_for_peer(
        &self,
        port_range: &PortRange,
        rule: &NetworkRule,
        peer: &NetworkPeer,
        policy: &NetworkPolicy,
        container_id: &str,
        container_ip: IpAddr,
        chain_name: &str
    ) -> Result<()> {
        let protocol = match port_range.protocol {
            Protocol::TCP => "tcp",
            Protocol::UDP => "udp",
        };
        
        let action = match rule.action.as_ref().unwrap_or(&PolicyAction::Allow) {
            PolicyAction::Allow => "ACCEPT",
            PolicyAction::Deny => "DROP",
            PolicyAction::Limit(rate) => "ACCEPT", // Rate limiting would require additional setup
            PolicyAction::Log => "LOG",
        };
        
        // Create port specification
        let port_spec = if port_range.port_min == port_range.port_max {
            format!("--dport {}", port_range.port_min)
        } else {
            format!("--dport {}:{}", port_range.port_min, port_range.port_max)
        };
        
        // Handle IP block peer
        if let Some(ip_block) = &peer.ip_block {
            let rule_spec = format!(
                "-A {} -p {} {} -s {} -d {} -j {}",
                chain_name, protocol, port_spec, container_ip, ip_block, action
            );
            
            // Execute iptables command
            let status = Command::new("iptables")
                .args(&["-C", &rule_spec])
                .status();
                
            // If rule doesn't exist, add it
            if status.is_err() || !status.unwrap().success() {
                let status = Command::new("iptables")
                    .args(&[
                        "-A", chain_name, 
                        "-p", protocol, 
                        &port_spec, 
                        "-s", &container_ip.to_string(), 
                        "-d", &ip_block.to_string(), 
                        "-j", action
                    ])
                    .status()?;
                    
                if !status.success() {
                    anyhow::bail!("Failed to add iptables rule for container {}", container_id);
                }
            }
        }
        
        // Handle selector peer
        if let Some(selector) = &peer.selector {
            // Find containers that match the selector
            let matching_containers = self.find_matching_containers(selector).await;
            
            // Add rule for each matching container
            let container_ips = self.container_ips.lock().await;
            for dst_container_id in matching_containers {
                if let Some(dst_ip) = container_ips.get(&dst_container_id) {
                    let rule_spec = format!(
                        "-A {} -p {} {} -s {} -d {} -j {}",
                        chain_name, protocol, port_spec, container_ip, dst_ip, action
                    );
                    
                    // Execute iptables command
                    let status = Command::new("iptables")
                        .args(&["-C", &rule_spec])
                        .status();
                        
                    // If rule doesn't exist, add it
                    if status.is_err() || !status.unwrap().success() {
                        let status = Command::new("iptables")
                            .args(&[
                                "-A", chain_name, 
                                "-p", protocol, 
                                &port_spec, 
                                "-s", &container_ip.to_string(), 
                                "-d", &dst_ip.to_string(), 
                                "-j", action
                            ])
                            .status()?;
                            
                        if !status.success() {
                            anyhow::bail!("Failed to add iptables rule for container {}", container_id);
                        }
                    }
                }
            }
        }
        
        Ok(())
    }
    
    /// Remove iptables rules for a container
    async fn remove_container_rules(&self, container_id: &str, container_ip: IpAddr) -> Result<()> {
        // For each policy chain
        let policy_chains = self.policy_chains.lock().await;
        for (policy_name, chain_name) in policy_chains.iter() {
            // Find and remove rules for this container
            let output = Command::new("iptables")
                .args(&["-S", chain_name])
                .output()?;
                
            if output.status.success() {
                let rules = String::from_utf8_lossy(&output.stdout);
                
                for line in rules.lines() {
                    if line.contains(&container_ip.to_string()) {
                        // Extract the rule without the -A prefix
                        let rule_parts: Vec<&str> = line.split_whitespace().collect();
                        if rule_parts.len() >= 3 {
                            // Replace -A with -D to delete the rule
                            let mut delete_args = vec!["-D"];
                            delete_args.extend_from_slice(&rule_parts[1..]);
                            
                            let status = Command::new("iptables")
                                .args(&delete_args)
                                .status()?;
                                
                            if !status.success() {
                                println!("Warning: Failed to remove iptables rule for container {}", container_id);
                            }
                        }
                    }
                }
            }
        }
        
        Ok(())
    }
    
    /// Ensure an iptables chain exists
    async fn ensure_chain_exists(&self, chain_name: &str) -> Result<()> {
        // Check if chain exists
        let status = Command::new("iptables")
            .args(&["-L", chain_name])
            .status();
            
        // If chain doesn't exist, create it
        if status.is_err() || !status.unwrap().success() {
            // Create the chain
            let status = Command::new("iptables")
                .args(&["-N", chain_name])
                .status()?;
                
            if !status.success() {
                anyhow::bail!("Failed to create iptables chain {}", chain_name);
            }
            
            // Add the chain to the FORWARD chain
            let status = Command::new("iptables")
                .args(&["-A", "FORWARD", "-j", chain_name])
                .status()?;
                
            if !status.success() {
                anyhow::bail!("Failed to add chain {} to FORWARD chain", chain_name);
            }
        }
        
        Ok(())
    }
    
    /// Generate a chain name for a policy
    fn get_policy_chain_name(&self, policy_name: &str) -> String {
        // Create a valid iptables chain name (alphanumeric and underscore only, max 28 chars)
        let sanitized = policy_name.chars()
            .filter(|c| c.is_alphanumeric() || *c == '_')
            .collect::<String>();
            
        let prefix = "HVM_POL_";
        let max_len = 28 - prefix.len();
        
        if sanitized.len() <= max_len {
            format!("{}{}", prefix, sanitized)
        } else {
            // If too long, use a prefix and a hash
            let hash = sanitized.as_bytes().iter().fold(0u32, |acc, &b| acc.wrapping_add(b as u32));
            format!("{}{:x}", prefix, hash)
        }
    }
    
    /// Run reconciliation loop to ensure policies are consistently applied
    pub async fn run_reconciliation(&mut self) -> Result<()> {
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_secs();
            
        // Only run reconciliation if enough time has passed since last run
        let mut last_reconciliation = self.last_reconciliation.lock().await;
        if now - *last_reconciliation < self.reconciliation_interval {
            return Ok(());
        }
        
        println!("Running network policy reconciliation...");
        
        // Update last reconciliation time
        *last_reconciliation = now;
        drop(last_reconciliation);
        
        // Ensure all policy chains exist
        let policy_chains = self.policy_chains.lock().await.clone();
        for (policy_name, chain_name) in policy_chains {
            self.ensure_chain_exists(&chain_name).await?;
        }
        
        // For each container, apply all matching policies
        let container_ids: Vec<String> = self.container_labels.lock().await.keys().cloned().collect();
        
        for container_id in container_ids {
            self.apply_policies_to_container(&container_id).await?;
        }
        
        // Check for policy violations in system logs
        self.check_policy_violations().await?;
        
        println!("Network policy reconciliation completed successfully");
        
        Ok(())
    }
    
    /// Check for policy violations in system logs
    async fn check_policy_violations(&mut self) -> Result<()> {
        // In a real implementation, we would parse /var/log/kern.log or similar
        // to find iptables LOG messages for policy violations
        
        // For now, we'll just simulate finding some violations
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_secs();
            
        // Get all container IPs
        let container_ips = self.container_ips.lock().await;
        let ip_to_container: HashMap<IpAddr, String> = container_ips
            .iter()
            .map(|(id, ip)| (*ip, id.clone()))
            .collect();
        drop(container_ips);
            
        // Try to read the kernel log for iptables messages
        if let Ok(output) = Command::new("grep")
            .args(&[
                "-E", 
                "HIVEMIND-POLICY-.*: ", 
                "/var/log/kern.log"
            ])
            .output() {
                
            if output.status.success() {
                let log_content = String::from_utf8_lossy(&output.stdout);
                
                for line in log_content.lines() {
                    // Parse the log line to extract policy violation information
                    if let Some(policy_name) = self.extract_policy_name(line) {
                        if let Some((src_ip, dst_ip, protocol, port)) = self.extract_connection_info(line) {
                            // Find container IDs for the IPs
                            let src_container = ip_to_container.get(&src_ip).cloned();
                            let dst_container = ip_to_container.get(&dst_ip).cloned();
                            
                            if let Some(src_container_id) = src_container {
                                // Create a violation log entry
                                let mut violation_logs = self.violation_logs.lock().await;
                                let violation = ViolationLog {
                                    id: format!("VIOLATION-{}-{}", now, src_container_id),
                                    timestamp: now,
                                    source_container: src_container_id,
                                    source_ip: src_ip,
                                    destination_container: dst_container,
                                    destination_ip: dst_ip,
                                    protocol,
                                    port,
                                    policy_name,
                                    rule_id: None, // We don't have this information from the log
                                    action: PolicyAction::Deny,
                                    tenant_id: None, // We don't have this information from the log
                                };
                                
                                // Add to violation logs
                                violation_logs.push(violation);
                                
                                // Limit the number of logs
                                if violation_logs.len() > self.max_log_entries {
                                    violation_logs.remove(0);
                                }
                            }
                        }
                    }
                }
            }
        }
        
        Ok(())
    }
    
    /// Check if traffic is allowed between containers
    pub async fn is_traffic_allowed(&self, src_container_id: &str, dst_container_id: &str, protocol: Protocol, port: u16) -> Result<bool> {
        // Get container labels
        let container_labels = self.container_labels.lock().await;
        let src_labels = match container_labels.get(src_container_id) {
            Some(labels) => labels,
            None => return Ok(true), // If we don't have labels, allow by default
        };
        
        let dst_labels = match container_labels.get(dst_container_id) {
            Some(labels) => labels,
            None => return Ok(true), // If we don't have labels, allow by default
        };
        
        // Check if containers are in the same tenant
        let src_tenant = src_labels.get("tenant");
        let dst_tenant = dst_labels.get("tenant");
        
        if let (Some(src_tenant_id), Some(dst_tenant_id)) = (src_tenant, dst_tenant) {
            // If different tenants, check policies
            if src_tenant_id != dst_tenant_id {
                // Find policies that apply to the destination container
                let policies = self.policies.lock().await;
                for policy in policies.values() {
                    // Check if policy applies to destination container
                    if self.labels_match_selector(dst_labels, &policy.selector.labels) {
                        // Check ingress rules
                        for rule in &policy.ingress_rules {
                            // Check if rule matches source container
                            let matches_source = rule.from.iter().any(|peer| {
                                if let Some(selector) = &peer.selector {
                                    self.labels_match_selector(src_labels, &selector.labels)
                                } else {
                                    false
                                }
                            });
                            
                            // Check if rule matches port and protocol
                            let matches_port = rule.ports.is_empty() || rule.ports.iter().any(|port_range| {
                                port_range.protocol == protocol &&
                                port >= port_range.port_min &&
                                port <= port_range.port_max
                            });
                            
                            if matches_source && matches_port {
                                // Return based on action
                                return Ok(match rule.action.as_ref().unwrap_or(&PolicyAction::Allow) {
                                    PolicyAction::Allow => true,
                                    PolicyAction::Deny => false,
                                    _ => true, // Default to allow
                                });
                            }
                        }
                    }
                }
                
                // If no matching policy found, default to deny for cross-tenant traffic
                return Ok(false);
            }
        }
        
        // Default to allow for same tenant or if tenant info is missing
        Ok(true)
    }
    
    /// Get policy violation logs
    pub async fn get_violation_logs(&self) -> Vec<ViolationLog> {
        self.violation_logs.lock().await.clone()
    }
    
    /// Extract policy name from log line
    fn extract_policy_name(&self, log_line: &str) -> Option<String> {
        // Example log format: "HIVEMIND-POLICY-web-policy: ..."
        if let Some(start_idx) = log_line.find("HIVEMIND-POLICY-") {
            if let Some(end_idx) = log_line[start_idx..].find(": ") {
                let policy_with_prefix = &log_line[start_idx..start_idx + end_idx];
                return Some(policy_with_prefix.replace("HIVEMIND-POLICY-", ""));
            }
        }
        None
    }
    
    /// Extract connection information from log line
    fn extract_connection_info(&self, log_line: &str) -> Option<(IpAddr, IpAddr, Protocol, u16)> {
        // This is a simplified implementation
        // In a real system, we would need to parse the actual iptables log format
        
        // Try to extract source IP
        if let Some(src_start) = log_line.find("SRC=") {
            let src_end = log_line[src_start + 4..].find(' ').unwrap_or(15) + src_start + 4;
            let src_ip_str = &log_line[src_start + 4..src_end];
            
            // Try to extract destination IP
            if let Some(dst_start) = log_line.find("DST=") {
                let dst_end = log_line[dst_start + 4..].find(' ').unwrap_or(15) + dst_start + 4;
                let dst_ip_str = &log_line[dst_start + 4..dst_end];
                
                // Try to extract protocol
                if let Some(proto_start) = log_line.find("PROTO=") {
                    let proto_end = log_line[proto_start + 6..].find(' ').unwrap_or(3) + proto_start + 6;
                    let proto_str = &log_line[proto_start + 6..proto_end];
                    
                    let protocol = match proto_str {
                        "TCP" => Protocol::TCP,
                        "UDP" => Protocol::UDP,
                        _ => return None,
                    };
                    
                    // Try to extract port
                    let port = if let Some(dpt_start) = log_line.find("DPT=") {
                        let dpt_end = log_line[dpt_start + 4..].find(' ').unwrap_or(5) + dpt_start + 4;
                        let dpt_str = &log_line[dpt_start + 4..dpt_end];
                        dpt_str.parse::<u16>().unwrap_or(0)
                    } else {
                        0
                    };
                    
                    // Parse IPs
                    if let (Ok(src_ip), Ok(dst_ip)) = (src_ip_str.parse::<IpAddr>(), dst_ip_str.parse::<IpAddr>()) {
                        return Some((src_ip, dst_ip, protocol, port));
                    }
                }
            }
        }
        
        None
    }
    
    /// Get policy violation logs
    pub async fn get_violation_logs(&self) -> Vec<ViolationLog> {
        self.violation_logs.lock().await.clone()
    }
    
    /// Create default network policies for a new tenant
    pub async fn create_default_tenant_policies(&mut self, tenant_id: &str) -> Result<()> {
        println!("Creating default network policies for tenant {}", tenant_id);
        
        // Create default isolation policy
        let isolation_policy = NetworkPolicy {
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
            created_at: SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_secs(),
            updated_at: SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_secs(),
        };
        
        // Apply the policy
        self.apply_policy(isolation_policy).await?;
        
        println!("Default network policies created for tenant {}", tenant_id);
        
        Ok(())
    }
    
    /// Check if labels match a selector
    fn labels_match_selector(&self, labels: &HashMap<String, String>, selector_labels: &HashMap<String, String>) -> bool {
        for (key, value) in selector_labels {
            if !labels.contains_key(key) || labels.get(key) != Some(value) {
                return false;
            }
        }
        true
    }
}