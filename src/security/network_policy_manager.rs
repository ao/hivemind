use anyhow::Result;
use std::collections::HashMap;
use std::net::IpAddr;
use std::sync::Arc;
use tokio::sync::Mutex;

use crate::network::{NetworkPolicy, NetworkRule, NetworkSelector, Protocol, PolicyAction};
use crate::security::network_policy_controller::NetworkPolicyController;

/// NetworkPolicyManager handles network policies using the NetworkPolicyController
#[derive(Debug)]
pub struct NetworkPolicyManager {
    controller: Arc<Mutex<NetworkPolicyController>>,
}

impl NetworkPolicyManager {
    pub fn new() -> Self {
        Self {
            controller: Arc::new(Mutex::new(NetworkPolicyController::new())),
        }
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
        self.controller.lock().await.apply_policy(policy).await?;
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