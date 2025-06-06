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
    
    /// Delete a tenant
    pub async fn delete_tenant(&self, tenant_id: &str) -> Result<()> {
        // Don't allow deleting the default tenant
        if tenant_id == self.default_tenant_id {
            anyhow::bail!("Cannot delete the default tenant");
        }
        
        let mut tenants = self.tenants.write().await;
        
        if !tenants.contains_key(tenant_id) {
            anyhow::bail!("Tenant not found");
        }
        
        tenants.remove(tenant_id);
        
        // TODO: Clean up tenant resources
        // - Delete tenant-specific roles
        // - Delete tenant namespaces
        // - Clean up tenant resources
        
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