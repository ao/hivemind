use anyhow::Result;
use std::collections::HashMap;
use std::net::IpAddr;
use std::sync::Arc;
use tokio::sync::Mutex;
use log::{info, warn, error, debug, trace};
use std::time::{SystemTime, UNIX_EPOCH};

use crate::network::{NetworkPolicy, NetworkRule, NetworkSelector, Protocol, PolicyAction};
use crate::security::network_policy_controller::{NetworkPolicyController, ViolationSeverity};
use crate::monitoring::MonitoringManager;

/// NetworkPolicyManager handles network policies using the NetworkPolicyController
#[derive(Debug)]
pub struct NetworkPolicyManager {
    controller: Arc<Mutex<NetworkPolicyController>>,
    monitoring_manager: Option<Arc<MonitoringManager>>,
    notification_endpoints: Vec<String>,
    dynamic_policy_adjustment: bool,
    violation_threshold: u32,
    adjustment_cooldown_seconds: u64,
    last_adjustment: Arc<Mutex<HashMap<String, u64>>>,
}

impl NetworkPolicyManager {
    pub fn new() -> Self {
        Self {
            controller: Arc::new(Mutex::new(NetworkPolicyController::new())),
            monitoring_manager: None,
            notification_endpoints: Vec::new(),
            dynamic_policy_adjustment: false,
            violation_threshold: 5, // Default: adjust policy after 5 violations
            adjustment_cooldown_seconds: 3600, // Default: 1 hour cooldown between adjustments
            last_adjustment: Arc::new(Mutex::new(HashMap::new())),
        }
    }
    
    /// Set monitoring manager for metrics collection
    pub fn with_monitoring_manager(mut self, monitoring_manager: Arc<MonitoringManager>) -> Self {
        // Set monitoring manager for the policy manager
        self.monitoring_manager = Some(monitoring_manager.clone());
        
        // Also set it for the controller
        let controller = self.controller.clone();
        tokio::spawn(async move {
            let mut controller_lock = controller.lock().await;
            *controller_lock = controller_lock.clone().with_monitoring_manager(monitoring_manager);
        });
        
        self
    }
    
    /// Add notification endpoint for policy violation alerts
    pub async fn add_notification_endpoint(&mut self, endpoint: String) -> Result<()> {
        // Add endpoint to the policy manager
        self.notification_endpoints.push(endpoint.clone());
        
        // Also add it to the controller
        let mut controller = self.controller.lock().await;
        controller.add_notification_endpoint(endpoint);
        
        Ok(())
    }
    
    /// Enable or disable dynamic policy adjustment
    pub fn set_dynamic_policy_adjustment(&mut self, enabled: bool) {
        self.dynamic_policy_adjustment = enabled;
    }
    
    /// Set the violation threshold for dynamic policy adjustment
    pub fn set_violation_threshold(&mut self, threshold: u32) {
        self.violation_threshold = threshold;
    }
    
    /// Set the cooldown period between policy adjustments
    pub fn set_adjustment_cooldown(&mut self, seconds: u64) {
        self.adjustment_cooldown_seconds = seconds;
    }
    
    /// Register container labels for policy matching
    pub async fn register_container_labels(&mut self, container_id: &str, labels: HashMap<String, String>) -> Result<()> {
        // We'll store the labels but wait for the IP to be registered before applying policies
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
    
    /// Register container IP address
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
    
    /// Unregister container when it's removed
    pub async fn unregister_container(&mut self, container_id: &str) -> Result<()> {
        self.controller.lock().await.unregister_container(container_id).await?;
        Ok(())
    }
    
    /// Apply a network policy
    pub async fn apply_policy(&mut self, policy: NetworkPolicy) -> Result<()> {
        debug!("Applying network policy: {}", policy.name);
        
        // Apply the policy using the controller
        self.controller.lock().await.apply_policy(policy.clone()).await?;
        
        // Send metrics if monitoring is available
        if let Some(monitoring) = &self.monitoring_manager {
            let mut labels = HashMap::new();
            labels.insert("policy_name".to_string(), policy.name.clone());
            if let Some(tenant_id) = &policy.tenant_id {
                labels.insert("tenant_id".to_string(), tenant_id.clone());
            }
            
            let metrics = vec![
                crate::monitoring::Metric {
                    name: "network_policy_applied".to_string(),
                    value: 1.0,
                    labels,
                    timestamp: SystemTime::now()
                        .duration_since(UNIX_EPOCH)
                        .unwrap_or_default()
                        .as_secs() as i64,
                },
            ];
            
            if let Err(e) = monitoring.send_metrics(metrics).await {
                warn!("Failed to send network policy metrics: {}", e);
            }
            
            /// Dynamically adjust a network policy based on violation patterns
            pub async fn adjust_policy_for_violations(&mut self, policy_name: &str) -> Result<bool> {
                // Skip if dynamic adjustment is disabled
                if !self.dynamic_policy_adjustment {
                    return Ok(false);
                }
                
                // Get current time
                let now = SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .unwrap_or_default()
                    .as_secs();
                    
                // Check cooldown period
                let mut last_adjustment = self.last_adjustment.lock().await;
                if let Some(last_time) = last_adjustment.get(policy_name) {
                    if now - *last_time < self.adjustment_cooldown_seconds {
                        debug!("Skipping policy adjustment for {} due to cooldown period", policy_name);
                        return Ok(false);
                    }
                }
                
                // Get violation logs for this policy
                let controller = self.controller.lock().await;
                let violations = controller.get_violation_logs().await?;
                
                // Count recent violations for this policy
                let recent_violations = violations.iter()
                    .filter(|v| v.policy_name == policy_name)
                    .filter(|v| (now - v.timestamp) < 24 * 60 * 60) // Last 24 hours
                    .count();
                    
                // If violations exceed threshold, adjust the policy
                if recent_violations >= self.violation_threshold as usize {
                    info!("Detected {} violations for policy {}, adjusting policy",
                        recent_violations, policy_name);
                        
                    // Get the current policy
                    let policies = controller.get_policies().await?;
                    if let Some(mut policy) = policies.get(policy_name).cloned() {
                        // Adjust the policy based on violation patterns
                        self.strengthen_policy(&mut policy, &violations).await?;
                        
                        // Apply the updated policy
                        drop(controller); // Release the lock before calling apply_policy
                        self.apply_policy(policy.clone()).await?;
                        
                        // Update last adjustment time
                        last_adjustment.insert(policy_name.to_string(), now);
                        
                        // Log the adjustment
                        info!("Policy {} adjusted due to {} recent violations", policy_name, recent_violations);
                        
                        // Send metrics if monitoring is available
                        if let Some(monitoring) = &self.monitoring_manager {
                            let mut labels = HashMap::new();
                            labels.insert("policy_name".to_string(), policy_name.to_string());
                            labels.insert("violation_count".to_string(), recent_violations.to_string());
                            
                            let metrics = vec![
                                crate::monitoring::Metric {
                                    name: "network_policy_adjusted".to_string(),
                                    value: 1.0,
                                    labels,
                                    timestamp: now as i64,
                                },
                            ];
                            
                            if let Err(e) = monitoring.send_metrics(metrics).await {
                                warn!("Failed to send policy adjustment metrics: {}", e);
                            }
                        }
                        
                        return Ok(true);
                    }
                }
                
                Ok(false)
            }
            
            /// Strengthen a policy based on violation patterns
            async fn strengthen_policy(&self, policy: &mut NetworkPolicy, violations: &[crate::security::network_policy_controller::ViolationLog]) -> Result<()> {
                // Group violations by source/destination patterns
                let mut violation_patterns: HashMap<(String, String), Vec<&crate::security::network_policy_controller::ViolationLog>> = HashMap::new();
                
                for violation in violations.iter().filter(|v| v.policy_name == policy.name) {
                    let src_pattern = violation.source_container.clone();
                    let dst_pattern = violation.destination_container.clone().unwrap_or_else(|| "unknown".to_string());
                    
                    violation_patterns
                        .entry((src_pattern, dst_pattern))
                        .or_insert_with(Vec::new)
                        .push(violation);
                }
                
                // Find the most common violation patterns
                let mut pattern_counts: Vec<((String, String), usize)> = violation_patterns
                    .iter()
                    .map(|(pattern, violations)| (pattern.clone(), violations.len()))
                    .collect();
                    
                pattern_counts.sort_by(|a, b| b.1.cmp(&a.1));
                
                // Add or strengthen rules for the top violation patterns
                for ((src_pattern, dst_pattern), count) in pattern_counts.iter().take(3) {
                    if *count >= 3 {
                        debug!("Adding stricter rule for pattern: {} -> {} ({} violations)",
                            src_pattern, dst_pattern, count);
                        
                        // Create a new rule or strengthen existing rules
                        let mut rule_added = false;
                        
                        for rule in &mut policy.ingress_rules {
                            // If we find a matching rule, make it stricter
                            if rule.from.iter().any(|peer| {
                                if let Some(selector) = &peer.selector {
                                    selector.labels.values().any(|v| v.contains(src_pattern))
                                } else {
                                    false
                                }
                            }) {
                                // Make the rule stricter
                                rule.action = Some(PolicyAction::Deny);
                                rule.log = true;
                                rule_added = true;
                                break;
                            }
                        }
                        
                        // If no matching rule found, add a new one
                        if !rule_added {
                            // Create a new selector for the source pattern
                            let mut selector_labels = HashMap::new();
                            
                            // Try to extract meaningful labels from the pattern
                            if src_pattern.contains("frontend") {
                                selector_labels.insert("app".to_string(), "frontend".to_string());
                            } else if src_pattern.contains("backend") {
                                selector_labels.insert("app".to_string(), "backend".to_string());
                            } else {
                                // Use a generic label based on the pattern
                                selector_labels.insert("name".to_string(), src_pattern.clone());
                            }
                            
                            // Create a new rule
                            let new_rule = NetworkRule {
                                ports: Vec::new(), // Empty means all ports
                                from: vec![
                                    crate::network::NetworkPeer {
                                        ip_block: None,
                                        selector: Some(crate::network::NetworkSelector {
                                            labels: selector_labels,
                                        }),
                                    },
                                ],
                                action: Some(PolicyAction::Deny),
                                log: true,
                                description: Some(format!("Auto-generated rule based on {} violation patterns", count)),
                                id: Some(format!("auto-{}", uuid::Uuid::new_v4())),
                            };
                            
                            // Add the new rule
                            policy.ingress_rules.push(new_rule);
                        }
                    }
                }
                
                // Increase policy priority to ensure it takes precedence
                policy.priority += 10;
                
                Ok(())
            }
            
            /// Run maintenance tasks for network policies
            pub async fn run_maintenance(&mut self) -> Result<()> {
                debug!("Running network policy maintenance tasks");
                
                // Run reconciliation to ensure policies are consistently applied
                self.run_reconciliation().await?;
                
                // Check for policy violations
                let controller = self.controller.lock().await;
                let violations = controller.get_violation_logs().await?;
                drop(controller);
                
                // Count violations by policy
                let mut policy_violations: HashMap<String, usize> = HashMap::new();
                
                for violation in &violations {
                    *policy_violations.entry(violation.policy_name.clone()).or_insert(0) += 1;
                }
                
                // Adjust policies with high violation counts
                for (policy_name, count) in policy_violations {
                    if count >= self.violation_threshold as usize {
                        self.adjust_policy_for_violations(&policy_name).await?;
                    }
                }
                
                // Clean up old violation logs (older than 30 days)
                let controller = self.controller.lock().await;
                let removed = controller.cleanup_old_violations(30).await?;
                if removed > 0 {
                    debug!("Cleaned up {} old violation logs during maintenance", removed);
                }
                
                info!("Network policy maintenance completed");
                
                Ok(())
            }
        }
        
        Ok(())
    }
    
    /// Run reconciliation loop to ensure policies are consistently applied
    pub async fn run_reconciliation(&mut self) -> Result<()> {
        self.controller.lock().await.run_reconciliation().await?;
        Ok(())
    }
    
    /// Create default network policies for a new tenant
    pub async fn create_default_tenant_policies(&mut self, tenant_id: &str) -> Result<()> {
        self.controller.lock().await.create_default_tenant_policies(tenant_id).await?;
        Ok(())
    }
    
    /// Check if traffic is allowed between containers
    pub async fn is_traffic_allowed(&self, src_container_id: &str, dst_container_id: &str, protocol: Protocol, port: u16) -> Result<bool> {
        Ok(self.controller.lock().await.is_traffic_allowed(src_container_id, dst_container_id, protocol, port).await?)
    }
    
    /// Get policy violation logs
    pub async fn get_violation_logs(&self) -> Result<Vec<crate::security::network_policy_controller::ViolationLog>> {
        Ok(self.controller.lock().await.get_violation_logs().await)
    }
}