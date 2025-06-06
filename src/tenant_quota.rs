use anyhow::Result;
use std::sync::Arc;
use tokio::sync::RwLock;
use std::collections::HashMap;

use crate::tenant::TenantManager;
use crate::app::AppManager;
use crate::containerd_manager::Container;

/// TenantQuotaEnforcer handles the enforcement of tenant quotas
pub struct TenantQuotaEnforcer {
    tenant_manager: Arc<TenantManager>,
    app_manager: Arc<RwLock<AppManager>>,
    // Threshold percentages for quota alerts
    container_warning_threshold: u32,
    service_warning_threshold: u32,
    container_critical_threshold: u32,
    service_critical_threshold: u32,
}

impl TenantQuotaEnforcer {
    /// Create a new TenantQuotaEnforcer
    pub fn new(tenant_manager: Arc<TenantManager>, app_manager: Arc<RwLock<AppManager>>) -> Self {
        Self {
            tenant_manager,
            app_manager,
            container_warning_threshold: 80,
            service_warning_threshold: 80,
            container_critical_threshold: 95,
            service_critical_threshold: 95,
        }
    }

    /// Set warning threshold for container quota alerts (percentage)
    pub fn set_container_warning_threshold(&mut self, threshold: u32) {
        self.container_warning_threshold = threshold;
    }

    /// Set warning threshold for service quota alerts (percentage)
    pub fn set_service_warning_threshold(&mut self, threshold: u32) {
        self.service_warning_threshold = threshold;
    }

    /// Set critical threshold for container quota alerts (percentage)
    pub fn set_container_critical_threshold(&mut self, threshold: u32) {
        self.container_critical_threshold = threshold;
    }

    /// Set critical threshold for service quota alerts (percentage)
    pub fn set_service_critical_threshold(&mut self, threshold: u32) {
        self.service_critical_threshold = threshold;
    }

    /// Handle container creation event
    pub async fn handle_container_created(&self, container_id: &str, tenant_id: &str) -> Result<()> {
        // Register container with tenant
        self.tenant_manager.register_container(tenant_id, container_id).await?;
        
        // Check if tenant is approaching container quota
        self.check_container_quota_threshold(tenant_id).await?;
        
        Ok(())
    }

    /// Handle container deletion event
    pub async fn handle_container_deleted(&self, container_id: &str, tenant_id: &str) -> Result<()> {
        // Unregister container from tenant
        self.tenant_manager.unregister_container(tenant_id, container_id).await?;
        
        Ok(())
    }

    /// Handle service creation event
    pub async fn handle_service_created(&self, service_name: &str, tenant_id: &str) -> Result<()> {
        // Register service with tenant
        self.tenant_manager.register_service(tenant_id, service_name).await?;
        
        // Check if tenant is approaching service quota
        self.check_service_quota_threshold(tenant_id).await?;
        
        Ok(())
    }

    /// Handle service deletion event
    pub async fn handle_service_deleted(&self, service_name: &str, tenant_id: &str) -> Result<()> {
        // Unregister service from tenant
        self.tenant_manager.unregister_service(tenant_id, service_name).await?;
        
        Ok(())
    }

    /// Check if tenant is approaching container quota threshold and send alerts if needed
    async fn check_container_quota_threshold(&self, tenant_id: &str) -> Result<()> {
        // Check if tenant is approaching critical threshold
        if self.tenant_manager.is_approaching_container_quota(tenant_id, self.container_critical_threshold).await? {
            // Send critical alert
            self.send_container_quota_alert(tenant_id, &crate::tenant::QuotaAlertLevel::Critical).await?;
        }
        // Check if tenant is approaching warning threshold
        else if self.tenant_manager.is_approaching_container_quota(tenant_id, self.container_warning_threshold).await? {
            // Send warning alert
            self.send_container_quota_alert(tenant_id, &crate::tenant::QuotaAlertLevel::Warning).await?;
        }
        
        Ok(())
    }

    /// Check if tenant is approaching service quota threshold and send alerts if needed
    async fn check_service_quota_threshold(&self, tenant_id: &str) -> Result<()> {
        // Check if tenant is approaching critical threshold
        if self.tenant_manager.is_approaching_service_quota(tenant_id, self.service_critical_threshold).await? {
            // Send critical alert
            self.send_service_quota_alert(tenant_id, &crate::tenant::QuotaAlertLevel::Critical).await?;
        }
        // Check if tenant is approaching warning threshold
        else if self.tenant_manager.is_approaching_service_quota(tenant_id, self.service_warning_threshold).await? {
            // Send warning alert
            self.send_service_quota_alert(tenant_id, &crate::tenant::QuotaAlertLevel::Warning).await?;
        }
        
        Ok(())
    }

    /// Send container quota alert
    async fn send_container_quota_alert(&self, tenant_id: &str, alert_level: &crate::tenant::QuotaAlertLevel) -> Result<()> {
        // Get tenant details
        let tenant = match self.tenant_manager.get_tenant(tenant_id).await {
            Some(t) => t,
            None => return Ok(()),
        };
        
        // Get current container count
        let container_count = self.tenant_manager.get_container_count(tenant_id).await?;
        
        // Get container quota limit
        let quota_limit = match tenant.resource_quotas.max_containers {
            Some(limit) => limit,
            None => return Ok(()),
        };
        
        // Calculate usage percentage
        let usage_percent = (container_count as f64 / quota_limit as f64) * 100.0;
        
        // Create alert
        let alert = crate::tenant::QuotaAlert {
            tenant_id: tenant_id.to_string(),
            resource_type: "container".to_string(),
            current_usage: container_count,
            quota_limit,
            usage_percent,
            alert_level,
            timestamp: chrono::Utc::now().timestamp(),
        };
        
        // Log alert
        let message = match alert_level {
            crate::tenant::QuotaAlertLevel::Info => {
                format!("INFO: Tenant {} is using {}/{} containers ({:.1}%)",
                    tenant_id, container_count, quota_limit, usage_percent)
            },
            crate::tenant::QuotaAlertLevel::Warning => {
                format!("WARNING: Tenant {} is approaching container quota limit: {}/{} ({:.1}%)",
                    tenant_id, container_count, quota_limit, usage_percent)
            },
            crate::tenant::QuotaAlertLevel::Critical => {
                format!("CRITICAL: Tenant {} is near container quota limit: {}/{} ({:.1}%)",
                    tenant_id, container_count, quota_limit, usage_percent)
            },
        };
        
        println!("{}", message);
        
        // In a real system, we would send this alert to a notification system
        // For now, we just log it
        
        Ok(())
    }

    /// Send service quota alert
    async fn send_service_quota_alert(&self, tenant_id: &str, alert_level: &crate::tenant::QuotaAlertLevel) -> Result<()> {
        // Get tenant details
        let tenant = match self.tenant_manager.get_tenant(tenant_id).await {
            Some(t) => t,
            None => return Ok(()),
        };
        
        // Get current service count
        let service_count = self.tenant_manager.get_service_count(tenant_id).await?;
        
        // Get service quota limit
        let quota_limit = match tenant.resource_quotas.max_services {
            Some(limit) => limit,
            None => return Ok(()),
        };
        
        // Calculate usage percentage
        let usage_percent = (service_count as f64 / quota_limit as f64) * 100.0;
        
        // Create alert
        let alert = crate::tenant::QuotaAlert {
            tenant_id: tenant_id.to_string(),
            resource_type: "service".to_string(),
            current_usage: service_count,
            quota_limit,
            usage_percent,
            alert_level,
            timestamp: chrono::Utc::now().timestamp(),
        };
        
        // Log alert
        let message = match alert_level {
            crate::tenant::QuotaAlertLevel::Info => {
                format!("INFO: Tenant {} is using {}/{} services ({:.1}%)",
                    tenant_id, service_count, quota_limit, usage_percent)
            },
            crate::tenant::QuotaAlertLevel::Warning => {
                format!("WARNING: Tenant {} is approaching service quota limit: {}/{} ({:.1}%)",
                    tenant_id, service_count, quota_limit, usage_percent)
            },
            crate::tenant::QuotaAlertLevel::Critical => {
                format!("CRITICAL: Tenant {} is near service quota limit: {}/{} ({:.1}%)",
                    tenant_id, service_count, quota_limit, usage_percent)
            },
        };
        
        println!("{}", message);
        
        // In a real system, we would send this alert to a notification system
        // For now, we just log it
        
        Ok(())
    }

    /// Pre-check if a container can be created for a tenant
    pub async fn pre_check_container_creation(&self, tenant_id: &str) -> Result<bool> {
        self.tenant_manager.pre_check_container_creation(tenant_id).await
    }

    /// Pre-check if a service can be created for a tenant
    pub async fn pre_check_service_creation(&self, tenant_id: &str) -> Result<bool> {
        self.tenant_manager.pre_check_service_creation(tenant_id).await
    }

    /// Initialize container and service counts for all tenants
    pub async fn initialize_quota_tracking(&self) -> Result<()> {
        // Get all tenants
        let tenants = self.tenant_manager.list_tenants().await;
        
        // Get all containers
        let app_manager = self.app_manager.read().await;
        let containers = match app_manager.get_container_details().await {
            Ok(containers) => containers,
            Err(_) => Vec::new(),
        };
        
        // Map containers to tenants based on namespaces
        for tenant in &tenants {
            let tenant_namespaces: Vec<String> = tenant.namespaces.clone();
            
            // Find containers belonging to this tenant
            for container in &containers {
                let belongs_to_tenant = tenant_namespaces.iter().any(|ns| container.name.contains(ns));
                
                if belongs_to_tenant {
                    // Register container with tenant
                    self.tenant_manager.register_container(&tenant.id, &container.id).await?;
                    
                    // Extract service name from container and register it
                    if let Some(service_name) = container.name.split('-').next() {
                        self.tenant_manager.register_service(&tenant.id, service_name).await?;
                    }
                }
            }
        }
        
        Ok(())
    }
}