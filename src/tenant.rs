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
    container_runtime: Option<Arc<dyn crate::app::ContainerRuntime>>,
    // Cache for resource usage to avoid excessive monitoring overhead
    resource_usage_cache: Arc<RwLock<HashMap<String, (ResourceUsage, i64)>>>, // (usage, timestamp)
    // Cache TTL in seconds
    cache_ttl: i64,
    // Track containers per tenant
    tenant_containers: Arc<RwLock<HashMap<String, HashSet<String>>>>, // tenant_id -> set of container_ids
    // Track services per tenant
    tenant_services: Arc<RwLock<HashMap<String, HashSet<String>>>>, // tenant_id -> set of service_names
    // Grace periods for quota violations (in seconds)
    container_quota_grace_period: i64,
    service_quota_grace_period: i64,
    // Track temporary quota violations with expiration timestamps
    container_quota_violations: Arc<RwLock<HashMap<String, i64>>>, // tenant_id -> expiration timestamp
    service_quota_violations: Arc<RwLock<HashMap<String, i64>>>, // tenant_id -> expiration timestamp
}

impl TenantManager {
    /// Create a new TenantManager
    pub fn new(rbac_manager: Arc<RbacManager>) -> Self {
        let manager = Self {
            tenants: Arc::new(RwLock::new(HashMap::new())),
            rbac_manager,
            default_tenant_id: "default".to_string(),
            container_runtime: None,
            resource_usage_cache: Arc::new(RwLock::new(HashMap::new())),
            cache_ttl: 60, // Default 60 seconds TTL for cache
            tenant_containers: Arc::new(RwLock::new(HashMap::new())),
            tenant_services: Arc::new(RwLock::new(HashMap::new())),
            container_quota_grace_period: 3600, // Default 1 hour grace period
            service_quota_grace_period: 3600, // Default 1 hour grace period
            container_quota_violations: Arc::new(RwLock::new(HashMap::new())),
            service_quota_violations: Arc::new(RwLock::new(HashMap::new())),
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
            let roles = self.rbac_manager.list_roles().await;
            for role in roles {
                let has_tenant_scope = role.permissions.iter().any(|p| {
                    if let PermissionScope::Namespace(ns) = &p.scope {
                        ns == &namespace.to_string()
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
        
        // 3. Remove tenant admin role from users
        // Since there's no direct revoke_user_role method, we'd need to update each user
        // This is a simplified approach - in a real implementation, we would need to
        // find all users with this role and update them
        eprintln!("Note: Users with tenant admin role should be updated separately");
        
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
            container_runtime: self.container_runtime.clone(),
            resource_usage_cache: self.resource_usage_cache.clone(),
            cache_ttl: self.cache_ttl,
            tenant_containers: self.tenant_containers.clone(),
            tenant_services: self.tenant_services.clone(),
            container_quota_grace_period: self.container_quota_grace_period,
            service_quota_grace_period: self.service_quota_grace_period,
            container_quota_violations: self.container_quota_violations.clone(),
            service_quota_violations: self.service_quota_violations.clone(),
        }
    }
    
    /// Set the container runtime
    pub fn set_container_runtime(&mut self, runtime: Arc<dyn crate::app::ContainerRuntime>) {
        self.container_runtime = Some(runtime);
    }
    
    /// Set the cache TTL in seconds
    pub fn set_cache_ttl(&mut self, ttl: i64) {
        self.cache_ttl = ttl;
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
        
        // Check if tenant has a grace period for container quota violations
        {
            let violations = self.container_quota_violations.read().await;
            if let Some(expiration) = violations.get(tenant_id) {
                let current_time = chrono::Utc::now().timestamp();
                if current_time < *expiration {
                    // Grace period is still active, allow container creation
                    return Ok(true);
                }
            }
        }
        
        // Count current containers for this tenant
        let container_count = {
            let containers = self.tenant_containers.read().await;
            containers.get(tenant_id).map_or(0, |set| set.len() as u32)
        };
        
        // Check if container count is below the limit
        let max_containers = tenant.resource_quotas.max_containers.unwrap();
        Ok(container_count < max_containers)
    }
    
    /// Check if a tenant can create more services
    pub async fn can_create_service(&self, tenant_id: &str) -> Result<bool> {
        let tenants = self.tenants.read().await;
        let tenant = tenants.get(tenant_id).context("Tenant not found")?;
        
        // If no limit is set, allow service creation
        if tenant.resource_quotas.max_services.is_none() {
            return Ok(true);
        }
        
        // Check if tenant has a grace period for service quota violations
        {
            let violations = self.service_quota_violations.read().await;
            if let Some(expiration) = violations.get(tenant_id) {
                let current_time = chrono::Utc::now().timestamp();
                if current_time < *expiration {
                    // Grace period is still active, allow service creation
                    return Ok(true);
                }
            }
        }
        
        // Count current services for this tenant
        let service_count = {
            let services = self.tenant_services.read().await;
            services.get(tenant_id).map_or(0, |set| set.len() as u32)
        };
        
        // Check if service count is below the limit
        let max_services = tenant.resource_quotas.max_services.unwrap();
        Ok(service_count < max_services)
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
        // Check if tenant exists
        let tenants = self.tenants.read().await;
        if !tenants.contains_key(tenant_id) {
            anyhow::bail!("Tenant not found");
        }
        
        // Check cache first
        {
            let cache = self.resource_usage_cache.read().await;
            if let Some((usage, timestamp)) = cache.get(tenant_id) {
                let current_time = chrono::Utc::now().timestamp();
                // If cache is still valid, return cached value
                if current_time - timestamp < self.cache_ttl {
                    return Ok(usage.clone());
                }
            }
        }
        
        // If no container runtime is available, return zeros
        if self.container_runtime.is_none() {
            return Ok(ResourceUsage {
                cpu_usage: 0,
                memory_usage: 0,
                storage_usage: 0,
                container_count: 0,
                service_count: 0,
            });
        }
        
        let container_runtime = self.container_runtime.as_ref().unwrap();
        
        // Get all containers
        let containers = match container_runtime.list_containers().await {
            Ok(containers) => containers,
            Err(_) => {
                // If we can't get containers, return zeros
                return Ok(ResourceUsage {
                    cpu_usage: 0,
                    memory_usage: 0,
                    storage_usage: 0,
                    container_count: 0,
                    service_count: 0,
                });
            }
        };
        
        // Filter containers by tenant ID
        // We'll use the tenant's namespaces to identify containers belonging to this tenant
        let tenant = tenants.get(tenant_id).unwrap();
        let tenant_namespaces: HashSet<String> = tenant.namespaces.iter().cloned().collect();
        
        let mut cpu_usage: u32 = 0;
        let mut memory_usage: u64 = 0;
        let mut storage_usage: u64 = 0;
        let mut container_count: u32 = 0;
        
        // Map to track unique services
        let mut services = HashSet::new();
        
        // Collect metrics for each container belonging to this tenant
        for container in containers {
            // Check if container belongs to this tenant
            // We'll use a simple heuristic: check if the container name contains any of the tenant's namespaces
            let belongs_to_tenant = tenant_namespaces.iter().any(|ns| container.name.contains(ns));
            
            if belongs_to_tenant {
                container_count += 1;
                
                // Extract service name from container (assuming format: service-name-replica-id)
                if let Some(service_name) = container.name.split('-').next() {
                    services.insert(service_name.to_string());
                }
                
                // Get container metrics
                if let Ok(stats) = container_runtime.get_container_metrics(&container.id).await {
                    cpu_usage = cpu_usage.saturating_add(stats.cpu_usage as u32);
                    memory_usage = memory_usage.saturating_add(stats.memory_usage);
                    
                    // For storage, we'll use the sum of volumes attached to the container
                    for volume in &container.volumes {
                        storage_usage = storage_usage.saturating_add(volume.size);
                    }
                }
            }
        }
        
        // Create resource usage object
        let usage = ResourceUsage {
            cpu_usage,
            memory_usage,
            storage_usage,
            container_count,
            service_count: services.len() as u32,
        };
        
        // Update cache
        {
            let mut cache = self.resource_usage_cache.write().await;
            cache.insert(tenant_id.to_string(), (usage.clone(), chrono::Utc::now().timestamp()));
        }
        
        Ok(usage)
    }

    /// Register a container with a tenant
    pub async fn register_container(&self, tenant_id: &str, container_id: &str) -> Result<()> {
        // Verify tenant exists
        let tenants = self.tenants.read().await;
        if !tenants.contains_key(tenant_id) {
            return Err(anyhow::anyhow!("Tenant not found"));
        }
        drop(tenants);

        // Add container to tenant's container set
        let mut containers = self.tenant_containers.write().await;
        let tenant_containers = containers.entry(tenant_id.to_string()).or_insert_with(HashSet::new);
        tenant_containers.insert(container_id.to_string());

        // Invalidate resource usage cache for this tenant
        let mut cache = self.resource_usage_cache.write().await;
        cache.remove(tenant_id);

        Ok(())
    }

    /// Unregister a container from a tenant
    pub async fn unregister_container(&self, tenant_id: &str, container_id: &str) -> Result<()> {
        // Verify tenant exists
        let tenants = self.tenants.read().await;
        if !tenants.contains_key(tenant_id) {
            return Err(anyhow::anyhow!("Tenant not found"));
        }
        drop(tenants);

        // Remove container from tenant's container set
        let mut containers = self.tenant_containers.write().await;
        if let Some(tenant_containers) = containers.get_mut(tenant_id) {
            tenant_containers.remove(container_id);
        }

        // Invalidate resource usage cache for this tenant
        let mut cache = self.resource_usage_cache.write().await;
        cache.remove(tenant_id);

        Ok(())
    }

    /// Register a service with a tenant
    pub async fn register_service(&self, tenant_id: &str, service_name: &str) -> Result<()> {
        // Verify tenant exists
        let tenants = self.tenants.read().await;
        if !tenants.contains_key(tenant_id) {
            return Err(anyhow::anyhow!("Tenant not found"));
        }
        drop(tenants);

        // Add service to tenant's service set
        let mut services = self.tenant_services.write().await;
        let tenant_services = services.entry(tenant_id.to_string()).or_insert_with(HashSet::new);
        tenant_services.insert(service_name.to_string());

        // Invalidate resource usage cache for this tenant
        let mut cache = self.resource_usage_cache.write().await;
        cache.remove(tenant_id);

        Ok(())
    }

    /// Unregister a service from a tenant
    pub async fn unregister_service(&self, tenant_id: &str, service_name: &str) -> Result<()> {
        // Verify tenant exists
        let tenants = self.tenants.read().await;
        if !tenants.contains_key(tenant_id) {
            return Err(anyhow::anyhow!("Tenant not found"));
        }
        drop(tenants);

        // Remove service from tenant's service set
        let mut services = self.tenant_services.write().await;
        if let Some(tenant_services) = services.get_mut(tenant_id) {
            tenant_services.remove(service_name);
        }

        // Invalidate resource usage cache for this tenant
        let mut cache = self.resource_usage_cache.write().await;
        cache.remove(tenant_id);

        Ok(())
    }

    /// Get the current container count for a tenant
    pub async fn get_container_count(&self, tenant_id: &str) -> Result<u32> {
        // Verify tenant exists
        let tenants = self.tenants.read().await;
        if !tenants.contains_key(tenant_id) {
            return Err(anyhow::anyhow!("Tenant not found"));
        }
        drop(tenants);

        // Get container count
        let containers = self.tenant_containers.read().await;
        let count = containers.get(tenant_id).map_or(0, |set| set.len() as u32);
        
        Ok(count)
    }

    /// Get the current service count for a tenant
    pub async fn get_service_count(&self, tenant_id: &str) -> Result<u32> {
        // Verify tenant exists
        let tenants = self.tenants.read().await;
        if !tenants.contains_key(tenant_id) {
            return Err(anyhow::anyhow!("Tenant not found"));
        }
        drop(tenants);

        // Get service count
        let services = self.tenant_services.read().await;
        let count = services.get(tenant_id).map_or(0, |set| set.len() as u32);
        
        Ok(count)
    }

    /// Set grace period for container quota violations
    pub async fn set_container_quota_grace_period(&mut self, seconds: i64) {
        self.container_quota_grace_period = seconds;
    }

    /// Set grace period for service quota violations
    pub async fn set_service_quota_grace_period(&mut self, seconds: i64) {
        self.service_quota_grace_period = seconds;
    }

    /// Grant temporary container quota violation grace period for a tenant
    pub async fn grant_container_quota_grace(&self, tenant_id: &str) -> Result<()> {
        // Verify tenant exists
        let tenants = self.tenants.read().await;
        if !tenants.contains_key(tenant_id) {
            return Err(anyhow::anyhow!("Tenant not found"));
        }
        drop(tenants);

        // Set expiration timestamp
        let expiration = chrono::Utc::now().timestamp() + self.container_quota_grace_period;
        let mut violations = self.container_quota_violations.write().await;
        violations.insert(tenant_id.to_string(), expiration);

        Ok(())
    }

    /// Grant temporary service quota violation grace period for a tenant
    pub async fn grant_service_quota_grace(&self, tenant_id: &str) -> Result<()> {
        // Verify tenant exists
        let tenants = self.tenants.read().await;
        if !tenants.contains_key(tenant_id) {
            return Err(anyhow::anyhow!("Tenant not found"));
        }
        drop(tenants);

        // Set expiration timestamp
        let expiration = chrono::Utc::now().timestamp() + self.service_quota_grace_period;
        let mut violations = self.service_quota_violations.write().await;
        violations.insert(tenant_id.to_string(), expiration);

        Ok(())
    }

    /// Revoke container quota violation grace period for a tenant
    pub async fn revoke_container_quota_grace(&self, tenant_id: &str) -> Result<()> {
        // Verify tenant exists
        let tenants = self.tenants.read().await;
        if !tenants.contains_key(tenant_id) {
            return Err(anyhow::anyhow!("Tenant not found"));
        }
        drop(tenants);

        // Remove grace period
        let mut violations = self.container_quota_violations.write().await;
        violations.remove(tenant_id);

        Ok(())
    }

    /// Revoke service quota violation grace period for a tenant
    pub async fn revoke_service_quota_grace(&self, tenant_id: &str) -> Result<()> {
        // Verify tenant exists
        let tenants = self.tenants.read().await;
        if !tenants.contains_key(tenant_id) {
            return Err(anyhow::anyhow!("Tenant not found"));
        }
        drop(tenants);

        // Remove grace period
        let mut violations = self.service_quota_violations.write().await;
        violations.remove(tenant_id);

        Ok(())
    }

    /// Check if a tenant is approaching its container quota limit
    /// Returns true if the tenant is using more than the specified percentage of its quota
    pub async fn is_approaching_container_quota(&self, tenant_id: &str, threshold_percent: u32) -> Result<bool> {
        // Verify tenant exists and has a container quota
        let tenants = self.tenants.read().await;
        let tenant = tenants.get(tenant_id).context("Tenant not found")?;
        
        if let Some(max_containers) = tenant.resource_quotas.max_containers {
            // Get current container count
            let container_count = self.get_container_count(tenant_id).await?;
            
            // Calculate percentage used
            let percent_used = (container_count as f64 / max_containers as f64) * 100.0;
            
            return Ok(percent_used >= threshold_percent as f64);
        }
        
        // No quota set, so not approaching limit
        Ok(false)
    }

    /// Check if a tenant is approaching its service quota limit
    /// Returns true if the tenant is using more than the specified percentage of its quota
    pub async fn is_approaching_service_quota(&self, tenant_id: &str, threshold_percent: u32) -> Result<bool> {
        // Verify tenant exists and has a service quota
        let tenants = self.tenants.read().await;
        let tenant = tenants.get(tenant_id).context("Tenant not found")?;
        
        if let Some(max_services) = tenant.resource_quotas.max_services {
            // Get current service count
            let service_count = self.get_service_count(tenant_id).await?;
            
            // Calculate percentage used
            let percent_used = (service_count as f64 / max_services as f64) * 100.0;
            
            return Ok(percent_used >= threshold_percent as f64);
        }
        
        // No quota set, so not approaching limit
        Ok(false)
    }

    /// Pre-check if a container can be created for a tenant
    /// This should be called before actually creating the container
    pub async fn pre_check_container_creation(&self, tenant_id: &str) -> Result<bool> {
        // Check if tenant can create more containers
        let can_create = self.can_create_container(tenant_id).await?;
        
        if !can_create {
            // Check if tenant is approaching quota limit
            let approaching_limit = self.is_approaching_container_quota(tenant_id, 90).await?;
            
            if approaching_limit {
                // Log warning
                eprintln!("WARNING: Tenant {} is approaching container quota limit", tenant_id);
            }
        }
        
        Ok(can_create)
    }

    /// Pre-check if a service can be created for a tenant
    /// This should be called before actually creating the service
    pub async fn pre_check_service_creation(&self, tenant_id: &str) -> Result<bool> {
        // Check if tenant can create more services
        let can_create = self.can_create_service(tenant_id).await?;
        
        if !can_create {
            // Check if tenant is approaching quota limit
            let approaching_limit = self.is_approaching_service_quota(tenant_id, 90).await?;
            
            if approaching_limit {
                // Log warning
                eprintln!("WARNING: Tenant {} is approaching service quota limit", tenant_id);
            }
        }
        
        Ok(can_create)
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

/// Quota alert level
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum QuotaAlertLevel {
    Info,
    Warning,
    Critical,
}

/// Quota alert notification
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct QuotaAlert {
    pub tenant_id: String,
    pub resource_type: String,
    pub current_usage: u32,
    pub quota_limit: u32,
    pub usage_percent: f64,
    pub alert_level: QuotaAlertLevel,
    pub timestamp: i64,
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