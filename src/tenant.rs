use anyhow::{Result, Context};
use serde::{Deserialize, Serialize};
use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use tokio::sync::RwLock;
use std::time::{SystemTime, UNIX_EPOCH};
use uuid::Uuid;

use crate::security::rbac::{RbacManager, Permission, PermissionScope, Role};

/// Tenant represents a logical isolation boundary in the system
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Tenant {
    pub id: String,
    pub name: String,
    pub isolation_level: IsolationLevel,
    pub resource_quotas: ResourceQuotas,
    pub namespaces: Vec<String>,
    pub created_at: chrono::DateTime<chrono::Utc>,
    pub updated_at: chrono::DateTime<chrono::Utc>,
    pub owner_id: String,
    pub description: Option<String>,
    pub labels: HashMap<String, String>,
    pub status: TenantStatus,
}

/// IsolationLevel defines how strictly tenants are isolated from each other
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum IsolationLevel {
    /// Complete isolation - dedicated nodes, network, storage
    Hard,
    /// Logical isolation with resource quotas
    Soft,
    /// Network isolation but shared compute
    NetworkOnly,
    /// Custom isolation with specific settings
    Custom(IsolationSettings),
}

/// Custom isolation settings for fine-grained control
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct IsolationSettings {
    pub network_isolation: bool,
    pub storage_isolation: bool,
    pub compute_isolation: bool,
    pub resource_guarantees: bool,
}

/// Resource quotas for a tenant
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ResourceQuotas {
    pub cpu_limit: Option<u32>,
    pub memory_limit: Option<u64>,
    pub storage_limit: Option<u64>,
    pub max_containers: Option<u32>,
    pub max_services: Option<u32>,
}

/// Status of a tenant
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum TenantStatus {
    Active,
    Suspended,
    Pending,
    Terminating,
}

/// TenantManager handles tenant lifecycle and integration with other components
pub struct TenantManager {
    tenants: Arc<RwLock<HashMap<String, Tenant>>>,
    rbac_manager: Arc<RbacManager>,
    default_tenant_id: String,
}

impl TenantManager {
    /// Create a new TenantManager
    pub fn new(rbac_manager: Arc<RbacManager>) -> Self {
        let manager = Self {
            tenants: Arc::new(RwLock::new(HashMap::new())),
            rbac_manager,
            default_tenant_id: "default".to_string(),
        };
        
        // Initialize with default tenant
        tokio::spawn({
            let manager = manager.clone();
            async move {
                if let Err(e) = manager.initialize_default_tenant().await {
                    eprintln!("Failed to initialize default tenant: {}", e);
                }
            }
        });
        
        manager
    }
    
    /// Initialize the default tenant
    async fn initialize_default_tenant(&self) -> Result<()> {
        let now = chrono::Utc::now();
        
        // Create default tenant
        let default_tenant = Tenant {
            id: self.default_tenant_id.clone(),
            name: "Default".to_string(),
            isolation_level: IsolationLevel::Soft,
            resource_quotas: ResourceQuotas {
                cpu_limit: None,
                memory_limit: None,
                storage_limit: None,
                max_containers: None,
                max_services: None,
            },
            namespaces: vec!["default".to_string()],
            created_at: now,
            updated_at: now,
            owner_id: "admin".to_string(),
            description: Some("Default tenant for backward compatibility".to_string()),
            labels: HashMap::new(),
            status: TenantStatus::Active,
        };
        
        // Add default tenant
        let mut tenants = self.tenants.write().await;
        tenants.insert(default_tenant.id.clone(), default_tenant);
        
        // Create tenant admin role
        self.create_tenant_admin_role("default").await?;
        
        Ok(())
    }
    
    /// Create a new tenant
    pub async fn create_tenant(&self, tenant: Tenant) -> Result<Tenant> {
        let mut tenants = self.tenants.write().await;
        
        // Check if tenant name already exists
        if tenants.values().any(|t| t.name == tenant.name) {
            anyhow::bail!("Tenant name already exists");
        }
        
        let tenant_id = tenant.id.clone();
        tenants.insert(tenant_id.clone(), tenant.clone());
        
        // Create tenant admin role
        self.create_tenant_admin_role(&tenant_id).await?;
        
        Ok(tenant)
    }
    
    /// Create tenant admin role with appropriate permissions
    async fn create_tenant_admin_role(&self, tenant_id: &str) -> Result<()> {
        let role_id = format!("{}-admin", tenant_id);
        let role_name = format!("{} Administrator", tenant_id);
        
        let tenant_admin_role = Role {
            id: role_id.clone(),
            name: role_name,
            description: format!("Administrator role for tenant {}", tenant_id),
            permissions: vec![
                Permission {
                    resource: "*".to_string(),
                    action: "*".to_string(),
                    scope: PermissionScope::Namespace(tenant_id.to_string()),
                },
            ],
            created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        };
        
        self.rbac_manager.create_role(tenant_admin_role).await?;
        
        Ok(())
    }
    
    /// Get a tenant by ID
    pub async fn get_tenant(&self, tenant_id: &str) -> Option<Tenant> {
        let tenants = self.tenants.read().await;
        tenants.get(tenant_id).cloned()
    }
    
    /// Get a tenant by name
    pub async fn get_tenant_by_name(&self, name: &str) -> Option<Tenant> {
        let tenants = self.tenants.read().await;
        tenants.values().find(|t| t.name == name).cloned()
    }
    
    /// Update a tenant
    pub async fn update_tenant(&self, tenant_id: &str, updated_tenant: Tenant) -> Result<Tenant> {
        let mut tenants = self.tenants.write().await;
        
        if !tenants.contains_key(tenant_id) {
            anyhow::bail!("Tenant not found");
        }
        
        // Check if tenant name is being changed and if it conflicts
        let existing_tenant = tenants.get(tenant_id).unwrap();
        if existing_tenant.name != updated_tenant.name && 
           tenants.values().any(|t| t.name == updated_tenant.name) {
            anyhow::bail!("Tenant name already exists");
        }
        
        tenants.insert(tenant_id.to_string(), updated_tenant.clone());
        
        Ok(updated_tenant)
    }
    
    /// Delete a tenant and clean up all associated resources
    pub async fn delete_tenant(&self, tenant_id: &str) -> Result<()> {
        // Don't allow deleting the default tenant
        if tenant_id == self.default_tenant_id {
            anyhow::bail!("Cannot delete the default tenant");
        }
        
        // Get tenant info before removing it
        let tenant_namespaces: Vec<String>;
        let tenant_owner: String;
        
        {
            let tenants = self.tenants.read().await;
            
            if !tenants.contains_key(tenant_id) {
                anyhow::bail!("Tenant not found");
            }
            
            let tenant = tenants.get(tenant_id).unwrap();
            tenant_namespaces = tenant.namespaces.clone();
            tenant_owner = tenant.owner_id.clone();
        }
        
        // First, update tenant status to Terminating
        {
            let mut tenants = self.tenants.write().await;
            if let Some(tenant) = tenants.get_mut(tenant_id) {
                tenant.status = TenantStatus::Terminating;
                tenant.updated_at = chrono::Utc::now();
            }
        }
        
        // Clean up tenant resources
        
        // 1. Delete tenant-specific roles
        let admin_role_id = format!("{}-admin", tenant_id);
        if let Err(e) = self.rbac_manager.delete_role(&admin_role_id).await {
            eprintln!("Failed to delete tenant admin role: {}", e);
        }
        
        // 2. Delete all roles associated with this tenant's namespaces
        for namespace in &tenant_namespaces {
            // Find and delete roles with permissions scoped to this namespace
            if let Ok(roles) = self.rbac_manager.list_roles().await {
                for role in roles {
                    let has_tenant_scope = role.permissions.iter().any(|p| {
                        if let PermissionScope::Namespace(ns) = &p.scope {
                            ns == namespace
                        } else {
                            false
                        }
                    });
                    
                    if has_tenant_scope {
                        if let Err(e) = self.rbac_manager.delete_role(&role.id).await {
                            eprintln!("Failed to delete role {}: {}", role.id, e);
                        }
                    }
                }
            }
        }
        
        // 3. Revoke user access to tenant
        if let Err(e) = self.rbac_manager.revoke_user_role(&tenant_owner, &admin_role_id).await {
            eprintln!("Failed to revoke tenant owner role: {}", e);
        }
        
        // Finally, remove the tenant
        let mut tenants = self.tenants.write().await;
        tenants.remove(tenant_id);
        
        Ok(())
    }
    
    /// List all tenants
    pub async fn list_tenants(&self) -> Vec<Tenant> {
        let tenants = self.tenants.read().await;
        tenants.values().cloned().collect()
    }
    
    /// Add a namespace to a tenant
    pub async fn add_namespace(&self, tenant_id: &str, namespace: &str) -> Result<()> {
        let mut tenants = self.tenants.write().await;
        
        let tenant = tenants.get_mut(tenant_id).context("Tenant not found")?;
        
        if !tenant.namespaces.contains(&namespace.to_string()) {
            tenant.namespaces.push(namespace.to_string());
            tenant.updated_at = chrono::Utc::now();
        }
        
        Ok(())
    }
    
    /// Remove a namespace from a tenant
    pub async fn remove_namespace(&self, tenant_id: &str, namespace: &str) -> Result<()> {
        let mut tenants = self.tenants.write().await;
        
        let tenant = tenants.get_mut(tenant_id).context("Tenant not found")?;
        
        tenant.namespaces.retain(|n| n != namespace);
        tenant.updated_at = chrono::Utc::now();
        
        Ok(())
    }
    
    /// Update tenant resource quotas
    pub async fn update_resource_quotas(&self, tenant_id: &str, quotas: ResourceQuotas) -> Result<()> {
        let mut tenants = self.tenants.write().await;
        
        let tenant = tenants.get_mut(tenant_id).context("Tenant not found")?;
        
        tenant.resource_quotas = quotas;
        tenant.updated_at = chrono::Utc::now();
        
        Ok(())
    }
    
    /// Update tenant isolation level
    pub async fn update_isolation_level(&self, tenant_id: &str, isolation_level: IsolationLevel) -> Result<()> {
        let mut tenants = self.tenants.write().await;
        
        let tenant = tenants.get_mut(tenant_id).context("Tenant not found")?;
        
        tenant.isolation_level = isolation_level;
        tenant.updated_at = chrono::Utc::now();
        
        Ok(())
    }
    
    /// Check if a user has access to a tenant
    pub async fn check_tenant_access(&self, user_id: &str, tenant_id: &str) -> Result<bool> {
        // Admin always has access
        if user_id == "admin" {
            return Ok(true);
        }
        
        // Check if tenant exists
        let tenants = self.tenants.read().await;
        if !tenants.contains_key(tenant_id) {
            anyhow::bail!("Tenant not found");
        }
        
        // Check if user has any permissions in the tenant's namespace
        let tenant = tenants.get(tenant_id).unwrap();
        
        for namespace in &tenant.namespaces {
            let has_access = self.rbac_manager.check_permission(
                user_id,
                "*",
                "get",
                &PermissionScope::Namespace(namespace.clone()),
            ).await?;
            
            if has_access {
                return Ok(true);
            }
        }
        
        // Check if user is the tenant owner
        let tenant = tenants.get(tenant_id).unwrap();
        if tenant.owner_id == user_id {
            return Ok(true);
        }
        
        Ok(false)
    }
    
    /// Get the default tenant ID
    pub fn default_tenant_id(&self) -> &str {
        &self.default_tenant_id
    }
    
    /// Get tenant context for a user
    pub async fn get_tenant_context(&self, user_id: &str) -> Result<Vec<String>> {
        let tenants = self.tenants.read().await;
        let mut accessible_tenants = Vec::new();
        
        // Admin has access to all tenants
        if user_id == "admin" {
            return Ok(tenants.keys().cloned().collect());
        }
        
        for (tenant_id, tenant) in tenants.iter() {
            // Check if user is the tenant owner
            if tenant.owner_id == user_id {
                accessible_tenants.push(tenant_id.clone());
                continue;
            }
            
            // Check if user has any permissions in the tenant's namespace
            for namespace in &tenant.namespaces {
                let has_access = self.rbac_manager.check_permission(
                    user_id,
                    "*",
                    "get",
                    &PermissionScope::Namespace(namespace.clone()),
                ).await?;
                
                if has_access {
                    accessible_tenants.push(tenant_id.clone());
                    break;
                }
            }
        }
        
        Ok(accessible_tenants)
    }
    
    /// Clone the TenantManager
    pub fn clone(&self) -> Self {
        Self {
            tenants: self.tenants.clone(),
            rbac_manager: self.rbac_manager.clone(),
            default_tenant_id: self.default_tenant_id.clone(),
        }
    }
    /// Check if a tenant has exceeded its resource quotas
    pub async fn check_resource_quotas(&self, tenant_id: &str,
                                      cpu_request: Option<u32>,
                                      memory_request: Option<u64>,
                                      storage_request: Option<u64>) -> Result<bool> {
        let tenants = self.tenants.read().await;
        let tenant = tenants.get(tenant_id).context("Tenant not found")?;
        
        // Check CPU quota
        if let (Some(cpu_limit), Some(cpu_req)) = (tenant.resource_quotas.cpu_limit, cpu_request) {
            if cpu_req > cpu_limit {
                return Ok(false);
            }
        }
        
        // Check memory quota
        if let (Some(memory_limit), Some(memory_req)) = (tenant.resource_quotas.memory_limit, memory_request) {
            if memory_req > memory_limit {
                return Ok(false);
            }
        }
        
        // Check storage quota
        if let (Some(storage_limit), Some(storage_req)) = (tenant.resource_quotas.storage_limit, storage_request) {
            if storage_req > storage_limit {
                return Ok(false);
            }
        }
        
        Ok(true)
    }
    
    /// Check if a tenant can create more containers
    pub async fn can_create_container(&self, tenant_id: &str) -> Result<bool> {
        let tenants = self.tenants.read().await;
        let tenant = tenants.get(tenant_id).context("Tenant not found")?;
        
        // If no limit is set, allow container creation
        if tenant.resource_quotas.max_containers.is_none() {
            return Ok(true);
        }
        
        // TODO: Count current containers for this tenant
        // For now, we'll just return true
        Ok(true)
    }
    
    /// Check if a tenant can create more services
    pub async fn can_create_service(&self, tenant_id: &str) -> Result<bool> {
        let tenants = self.tenants.read().await;
        let tenant = tenants.get(tenant_id).context("Tenant not found")?;
        
        // If no limit is set, allow service creation
        if tenant.resource_quotas.max_services.is_none() {
            return Ok(true);
        }
        
        // TODO: Count current services for this tenant
        // For now, we'll just return true
        Ok(true)
    }
    
    /// Get network isolation settings for a tenant
    pub async fn get_network_isolation(&self, tenant_id: &str) -> Result<bool> {
        let tenants = self.tenants.read().await;
        let tenant = tenants.get(tenant_id).context("Tenant not found")?;
        
        match &tenant.isolation_level {
            IsolationLevel::Hard => Ok(true),
            IsolationLevel::NetworkOnly => Ok(true),
            IsolationLevel::Soft => Ok(false),
            IsolationLevel::Custom(settings) => Ok(settings.network_isolation),
        }
    }
    
    /// Get storage isolation settings for a tenant
    pub async fn get_storage_isolation(&self, tenant_id: &str) -> Result<bool> {
        let tenants = self.tenants.read().await;
        let tenant = tenants.get(tenant_id).context("Tenant not found")?;
        
        match &tenant.isolation_level {
            IsolationLevel::Hard => Ok(true),
            IsolationLevel::NetworkOnly => Ok(false),
            IsolationLevel::Soft => Ok(false),
            IsolationLevel::Custom(settings) => Ok(settings.storage_isolation),
        }
    }
    
    /// Get compute isolation settings for a tenant
    pub async fn get_compute_isolation(&self, tenant_id: &str) -> Result<bool> {
        let tenants = self.tenants.read().await;
        let tenant = tenants.get(tenant_id).context("Tenant not found")?;
        
        match &tenant.isolation_level {
            IsolationLevel::Hard => Ok(true),
            IsolationLevel::NetworkOnly => Ok(false),
            IsolationLevel::Soft => Ok(false),
            IsolationLevel::Custom(settings) => Ok(settings.compute_isolation),
        }
    }
    
    /// Get resource usage for a tenant
    pub async fn get_resource_usage(&self, tenant_id: &str) -> Result<ResourceUsage> {
        // TODO: Implement actual resource usage tracking
        // For now, return placeholder values
        Ok(ResourceUsage {
            cpu_usage: 0,
            memory_usage: 0,
            storage_usage: 0,
            container_count: 0,
            service_count: 0,
        })
    }
}

/// Resource usage statistics for a tenant
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ResourceUsage {
    pub cpu_usage: u32,
    pub memory_usage: u64,
    pub storage_usage: u64,
    pub container_count: u32,
    pub service_count: u32,
}

/// TenantContext provides tenant-specific context for operations
#[derive(Debug, Clone)]
pub struct TenantContext {
    pub tenant_id: String,
    pub user_id: String,
}

impl TenantContext {
    pub fn new(tenant_id: String, user_id: String) -> Self {
        Self {
            tenant_id,
            user_id,
        }
    }
    
    pub fn default(user_id: String) -> Self {
        Self {
            tenant_id: "default".to_string(),
            user_id,
        }
    }
}