use anyhow::Result;
use std::sync::Arc;

use crate::app::AppManager;
use crate::tenant_quota::TenantQuotaEnforcer;

/// Extension trait for AppManager to add quota enforcement capabilities
pub trait AppManagerQuotaExt {
    /// Set the tenant quota enforcer
    fn with_tenant_quota_enforcer(self, enforcer: Arc<TenantQuotaEnforcer>) -> Self;
    
    /// Check if a tenant can create a container
    async fn check_container_quota(&self, tenant_id: &str) -> Result<bool>;
    
    /// Check if a tenant can create a service
    async fn check_service_quota(&self, tenant_id: &str) -> Result<bool>;
    
    /// Register a container with a tenant
    async fn register_container_with_tenant(&self, container_id: &str, tenant_id: &str) -> Result<()>;
    
    /// Register a service with a tenant
    async fn register_service_with_tenant(&self, service_name: &str, tenant_id: &str) -> Result<()>;
    
    /// Unregister a container from a tenant
    async fn unregister_container_from_tenant(&self, container_id: &str, tenant_id: &str) -> Result<()>;
    
    /// Unregister a service from a tenant
    async fn unregister_service_from_tenant(&self, service_name: &str, tenant_id: &str) -> Result<()>;
}

/// AppManagerWithQuota wraps an AppManager and adds quota enforcement capabilities
pub struct AppManagerWithQuota {
    app_manager: AppManager,
    quota_enforcer: Option<Arc<TenantQuotaEnforcer>>,
}

impl AppManagerWithQuota {
    /// Create a new AppManagerWithQuota
    pub fn new(app_manager: AppManager) -> Self {
        Self {
            app_manager,
            quota_enforcer: None,
        }
    }
    
    /// Get the inner AppManager
    pub fn inner(&self) -> &AppManager {
        &self.app_manager
    }
    
    /// Get the inner AppManager as mutable
    pub fn inner_mut(&mut self) -> &mut AppManager {
        &mut self.app_manager
    }
    
    /// Deploy a container with tenant quota enforcement
    pub async fn deploy_container_with_quota(
        &self,
        image: &str,
        name: &str,
        node_id: Option<&str>,
        service_domain: Option<&str>,
        env_vars: Option<Vec<(&str, &str)>>,
        ports: Option<Vec<(u16, u16)>>,
        tenant_id: &str,
    ) -> Result<String> {
        // Check if tenant can create a container
        if let Some(enforcer) = &self.quota_enforcer {
            if !enforcer.pre_check_container_creation(tenant_id).await? {
                return Err(anyhow::anyhow!("Tenant {} has reached container quota limit", tenant_id));
            }
        }
        
        // Deploy the container
        let container_id = self.app_manager.deploy_container(
            image,
            name,
            node_id,
            service_domain,
            env_vars,
            ports,
        ).await?;
        
        // Register the container with the tenant
        if let Some(enforcer) = &self.quota_enforcer {
            enforcer.handle_container_created(&container_id, tenant_id).await?;
        }
        
        Ok(container_id)
    }
    
    /// Deploy an app with tenant quota enforcement
    pub async fn deploy_app_with_quota(
        &self,
        image: &str,
        name: &str,
        service_domain: Option<&str>,
        env_vars: Option<Vec<(&str, &str)>>,
        ports: Option<Vec<(u16, u16)>>,
        tenant_id: &str,
    ) -> Result<String> {
        // Check if tenant can create a container
        if let Some(enforcer) = &self.quota_enforcer {
            if !enforcer.pre_check_container_creation(tenant_id).await? {
                return Err(anyhow::anyhow!("Tenant {} has reached container quota limit", tenant_id));
            }
            
            // If creating a service, check service quota as well
            if service_domain.is_some() {
                if !enforcer.pre_check_service_creation(tenant_id).await? {
                    return Err(anyhow::anyhow!("Tenant {} has reached service quota limit", tenant_id));
                }
            }
        }
        
        // Deploy the app
        let container_id = self.app_manager.deploy_app(
            image,
            name,
            service_domain,
            env_vars,
            ports,
        ).await?;
        
        // Register the container with the tenant
        if let Some(enforcer) = &self.quota_enforcer {
            enforcer.handle_container_created(&container_id, tenant_id).await?;
            
            // If a service was created, register it as well
            if service_domain.is_some() {
                enforcer.handle_service_created(name, tenant_id).await?;
            }
        }
        
        Ok(container_id)
    }
    
    /// Stop a container with tenant quota enforcement
    pub async fn stop_container_with_quota(
        &self,
        container_id: &str,
        tenant_id: &str,
    ) -> Result<bool> {
        // Stop the container
        let result = self.app_manager.stop_container(container_id).await?;
        
        // Unregister the container from the tenant
        if let Some(enforcer) = &self.quota_enforcer {
            enforcer.handle_container_deleted(container_id, tenant_id).await?;
        }
        
        Ok(result)
    }
    
    /// Scale an app with tenant quota enforcement
    pub async fn scale_app_with_quota(
        &self,
        name: &str,
        replicas: u32,
        tenant_id: &str,
    ) -> Result<()> {
        // Get current replicas
        let current_replicas = match self.app_manager.get_service(name).await {
            Some(service) => service.current_replicas,
            None => 0,
        };
        
        // If scaling up, check if tenant can create more containers
        if replicas > current_replicas {
            let additional = replicas - current_replicas;
            
            if let Some(enforcer) = &self.quota_enforcer {
                // Check if tenant can create the additional containers
                for _ in 0..additional {
                    if !enforcer.pre_check_container_creation(tenant_id).await? {
                        return Err(anyhow::anyhow!("Tenant {} has reached container quota limit", tenant_id));
                    }
                }
            }
        }
        
        // Scale the app
        self.app_manager.scale_app(name, replicas).await?;
        
        // Register or unregister containers with the tenant
        if let Some(enforcer) = &self.quota_enforcer {
            if replicas > current_replicas {
                // Get the new containers
                if let Some(service) = self.app_manager.get_service(name).await {
                    // Register the new containers
                    if service.container_ids.len() > current_replicas as usize {
                        let new_containers = &service.container_ids[(current_replicas as usize)..];
                        for container_id in new_containers {
                            enforcer.handle_container_created(container_id, tenant_id).await?;
                        }
                    }
                }
            } else if replicas < current_replicas {
                // Get the removed containers
                if let Some(service) = self.app_manager.get_service(name).await {
                    // Unregister the removed containers
                    if service.container_ids.len() >= current_replicas as usize &&
                       replicas as usize < current_replicas as usize {
                        let removed_containers = &service.container_ids[(replicas as usize)..(current_replicas as usize)];
                        for container_id in removed_containers {
                            enforcer.handle_container_deleted(container_id, tenant_id).await?;
                        }
                    }
                }
            }
        }
        
        Ok(())
    }
}

impl AppManagerQuotaExt for AppManagerWithQuota {
    fn with_tenant_quota_enforcer(mut self, enforcer: Arc<TenantQuotaEnforcer>) -> Self {
        self.quota_enforcer = Some(enforcer);
        self
    }
    
    async fn check_container_quota(&self, tenant_id: &str) -> Result<bool> {
        if let Some(enforcer) = &self.quota_enforcer {
            enforcer.pre_check_container_creation(tenant_id).await
        } else {
            Ok(true)
        }
    }
    
    async fn check_service_quota(&self, tenant_id: &str) -> Result<bool> {
        if let Some(enforcer) = &self.quota_enforcer {
            enforcer.pre_check_service_creation(tenant_id).await
        } else {
            Ok(true)
        }
    }
    
    async fn register_container_with_tenant(&self, container_id: &str, tenant_id: &str) -> Result<()> {
        if let Some(enforcer) = &self.quota_enforcer {
            enforcer.handle_container_created(container_id, tenant_id).await
        } else {
            Ok(())
        }
    }
    
    async fn register_service_with_tenant(&self, service_name: &str, tenant_id: &str) -> Result<()> {
        if let Some(enforcer) = &self.quota_enforcer {
            enforcer.handle_service_created(service_name, tenant_id).await
        } else {
            Ok(())
        }
    }
    
    async fn unregister_container_from_tenant(&self, container_id: &str, tenant_id: &str) -> Result<()> {
        if let Some(enforcer) = &self.quota_enforcer {
            enforcer.handle_container_deleted(container_id, tenant_id).await
        } else {
            Ok(())
        }
    }
    
    async fn unregister_service_from_tenant(&self, service_name: &str, tenant_id: &str) -> Result<()> {
        if let Some(enforcer) = &self.quota_enforcer {
            enforcer.handle_service_deleted(service_name, tenant_id).await
        } else {
            Ok(())
        }
    }
}

// Implement conversion from AppManager to AppManagerWithQuota
impl From<AppManager> for AppManagerWithQuota {
    fn from(app_manager: AppManager) -> Self {
        Self::new(app_manager)
    }
}

// Implement conversion from AppManagerWithQuota to AppManager
impl From<AppManagerWithQuota> for AppManager {
    fn from(app_manager_with_quota: AppManagerWithQuota) -> Self {
        app_manager_with_quota.app_manager
    }
}