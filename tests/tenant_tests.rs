use anyhow::Result;
use hivemind::security::rbac::{RbacManager, User, Role, Permission, PermissionScope};
use hivemind::tenant::{TenantManager, Tenant, IsolationLevel, ResourceQuotas, TenantStatus};
use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};
use chrono::Utc;
use std::collections::HashMap;

#[tokio::test]
async fn test_tenant_creation() -> Result<()> {
    // Setup
    let rbac_manager = Arc::new(RbacManager::new());
    let tenant_manager = TenantManager::new(rbac_manager.clone());
    
    // Create a test tenant
    let tenant_id = "test-tenant".to_string();
    let tenant = Tenant {
        id: tenant_id.clone(),
        name: "Test Tenant".to_string(),
        isolation_level: IsolationLevel::Soft,
        resource_quotas: ResourceQuotas {
            cpu_limit: Some(4),
            memory_limit: Some(8 * 1024 * 1024 * 1024), // 8 GB
            storage_limit: Some(100 * 1024 * 1024 * 1024), // 100 GB
            max_containers: Some(10),
            max_services: Some(5),
        },
        namespaces: vec!["test-namespace".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "test-user".to_string(),
        description: Some("Test tenant for unit tests".to_string()),
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    // Create the tenant
    let created_tenant = tenant_manager.create_tenant(tenant.clone()).await?;
    
    // Verify tenant was created correctly
    assert_eq!(created_tenant.id, tenant.id);
    assert_eq!(created_tenant.name, tenant.name);
    
    // Verify tenant can be retrieved
    let retrieved_tenant = tenant_manager.get_tenant(&tenant_id).await.expect("Tenant should exist");
    assert_eq!(retrieved_tenant.id, tenant.id);
    assert_eq!(retrieved_tenant.name, tenant.name);
    
    // Verify tenant admin role was created
    let role_id = format!("{}-admin", tenant_id);
    let role = rbac_manager.get_role(&role_id).await.expect("Role should exist");
    assert_eq!(role.name, format!("{} Administrator", tenant_id));
    
    // Verify role has correct permissions
    assert_eq!(role.permissions.len(), 1);
    assert_eq!(role.permissions[0].resource, "*");
    assert_eq!(role.permissions[0].action, "*");
    match &role.permissions[0].scope {
        PermissionScope::Namespace(ns) => assert_eq!(ns, &tenant_id),
        _ => panic!("Expected Namespace scope"),
    }
    
    Ok(())
}

#[tokio::test]
async fn test_tenant_update() -> Result<()> {
    // Setup
    let rbac_manager = Arc::new(RbacManager::new());
    let tenant_manager = TenantManager::new(rbac_manager);
    
    // Create a test tenant
    let tenant_id = "update-tenant".to_string();
    let tenant = Tenant {
        id: tenant_id.clone(),
        name: "Update Tenant".to_string(),
        isolation_level: IsolationLevel::Soft,
        resource_quotas: ResourceQuotas {
            cpu_limit: Some(2),
            memory_limit: Some(4 * 1024 * 1024 * 1024), // 4 GB
            storage_limit: Some(50 * 1024 * 1024 * 1024), // 50 GB
            max_containers: Some(5),
            max_services: Some(3),
        },
        namespaces: vec!["update-namespace".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "test-user".to_string(),
        description: Some("Tenant for update test".to_string()),
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    // Create the tenant
    tenant_manager.create_tenant(tenant.clone()).await?;
    
    // Create updated tenant
    let mut updated_tenant = tenant.clone();
    updated_tenant.name = "Updated Tenant Name".to_string();
    updated_tenant.description = Some("Updated description".to_string());
    updated_tenant.isolation_level = IsolationLevel::NetworkOnly;
    updated_tenant.resource_quotas.cpu_limit = Some(4);
    updated_tenant.resource_quotas.memory_limit = Some(8 * 1024 * 1024 * 1024); // 8 GB
    
    // Update the tenant
    let result = tenant_manager.update_tenant(&tenant_id, updated_tenant.clone()).await?;
    
    // Verify tenant was updated correctly
    assert_eq!(result.name, "Updated Tenant Name");
    assert_eq!(result.description, Some("Updated description".to_string()));
    match result.isolation_level {
        IsolationLevel::NetworkOnly => (),
        _ => panic!("Expected NetworkOnly isolation level"),
    }
    assert_eq!(result.resource_quotas.cpu_limit, Some(4));
    
    // Verify tenant can be retrieved with updates
    let retrieved_tenant = tenant_manager.get_tenant(&tenant_id).await.expect("Tenant should exist");
    assert_eq!(retrieved_tenant.name, "Updated Tenant Name");
    
    Ok(())
}

#[tokio::test]
async fn test_tenant_namespace_operations() -> Result<()> {
    // Setup
    let rbac_manager = Arc::new(RbacManager::new());
    let tenant_manager = TenantManager::new(rbac_manager);
    
    // Create a test tenant
    let tenant_id = "namespace-tenant".to_string();
    let tenant = Tenant {
        id: tenant_id.clone(),
        name: "Namespace Tenant".to_string(),
        isolation_level: IsolationLevel::Soft,
        resource_quotas: ResourceQuotas {
            cpu_limit: None,
            memory_limit: None,
            storage_limit: None,
            max_containers: None,
            max_services: None,
        },
        namespaces: vec!["initial-namespace".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "test-user".to_string(),
        description: None,
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    // Create the tenant
    tenant_manager.create_tenant(tenant).await?;
    
    // Add a namespace
    tenant_manager.add_namespace(&tenant_id, "new-namespace").await?;
    
    // Verify namespace was added
    let tenant = tenant_manager.get_tenant(&tenant_id).await.expect("Tenant should exist");
    assert_eq!(tenant.namespaces.len(), 2);
    assert!(tenant.namespaces.contains(&"initial-namespace".to_string()));
    assert!(tenant.namespaces.contains(&"new-namespace".to_string()));
    
    // Remove a namespace
    tenant_manager.remove_namespace(&tenant_id, "initial-namespace").await?;
    
    // Verify namespace was removed
    let tenant = tenant_manager.get_tenant(&tenant_id).await.expect("Tenant should exist");
    assert_eq!(tenant.namespaces.len(), 1);
    assert!(!tenant.namespaces.contains(&"initial-namespace".to_string()));
    assert!(tenant.namespaces.contains(&"new-namespace".to_string()));
    
    Ok(())
}

#[tokio::test]
async fn test_tenant_access_control() -> Result<()> {
    // Setup
    let rbac_manager = Arc::new(RbacManager::new());
    let tenant_manager = TenantManager::new(rbac_manager.clone());
    
    // Create a test tenant
    let tenant_id = "access-tenant".to_string();
    let tenant = Tenant {
        id: tenant_id.clone(),
        name: "Access Control Tenant".to_string(),
        isolation_level: IsolationLevel::Soft,
        resource_quotas: ResourceQuotas {
            cpu_limit: None,
            memory_limit: None,
            storage_limit: None,
            max_containers: None,
            max_services: None,
        },
        namespaces: vec!["access-namespace".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "owner-user".to_string(),
        description: None,
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    // Create the tenant
    tenant_manager.create_tenant(tenant).await?;
    
    // Create a test user with access to the tenant
    let user_with_access = User {
        id: "user-with-access".to_string(),
        username: "access-user".to_string(),
        password_hash: "hash".to_string(),
        email: "access@example.com".to_string(),
        full_name: "Access User".to_string(),
        roles: vec![format!("{}-admin", tenant_id)],
        groups: Vec::new(),
        created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        last_login: None,
        active: true,
    };
    
    // Create a test user without access to the tenant
    let user_without_access = User {
        id: "user-without-access".to_string(),
        username: "no-access-user".to_string(),
        password_hash: "hash".to_string(),
        email: "noaccess@example.com".to_string(),
        full_name: "No Access User".to_string(),
        roles: vec!["viewer".to_string()], // Some other role
        groups: Vec::new(),
        created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        last_login: None,
        active: true,
    };
    
    // Add users to RBAC
    rbac_manager.create_user(user_with_access).await?;
    rbac_manager.create_user(user_without_access).await?;
    
    // Check access for owner
    let owner_has_access = tenant_manager.check_tenant_access("owner-user", &tenant_id).await?;
    assert!(owner_has_access, "Owner should have access to tenant");
    
    // Check access for user with access
    let user_has_access = tenant_manager.check_tenant_access("user-with-access", &tenant_id).await?;
    assert!(user_has_access, "User with tenant role should have access");
    
    // Check access for user without access
    let user_no_access = tenant_manager.check_tenant_access("user-without-access", &tenant_id).await?;
    assert!(!user_no_access, "User without tenant role should not have access");
    
    // Admin should always have access
    let admin_has_access = tenant_manager.check_tenant_access("admin", &tenant_id).await?;
    assert!(admin_has_access, "Admin should always have access");
    
    Ok(())
}

#[tokio::test]
async fn test_default_tenant() -> Result<()> {
    // Setup
    let rbac_manager = Arc::new(RbacManager::new());
    let tenant_manager = TenantManager::new(rbac_manager);
    
    // Verify default tenant exists
    let default_tenant_id = tenant_manager.default_tenant_id();
    let default_tenant = tenant_manager.get_tenant(default_tenant_id).await.expect("Default tenant should exist");
    
    assert_eq!(default_tenant.id, "default");
    assert_eq!(default_tenant.name, "Default");
    assert_eq!(default_tenant.owner_id, "admin");
    
    // Verify default tenant has at least one namespace
    assert!(!default_tenant.namespaces.is_empty());
    assert!(default_tenant.namespaces.contains(&"default".to_string()));
    
    // Verify we can't delete the default tenant
    let result = tenant_manager.delete_tenant(default_tenant_id).await;
    assert!(result.is_err(), "Should not be able to delete default tenant");
    
    Ok(())
}

#[tokio::test]
async fn test_tenant_deletion() -> Result<()> {
    // Setup
    let rbac_manager = Arc::new(RbacManager::new());
    let tenant_manager = TenantManager::new(rbac_manager);
    
    // Create a test tenant
    let tenant_id = "delete-tenant".to_string();
    let tenant = Tenant {
        id: tenant_id.clone(),
        name: "Delete Tenant".to_string(),
        isolation_level: IsolationLevel::Soft,
        resource_quotas: ResourceQuotas {
            cpu_limit: None,
            memory_limit: None,
            storage_limit: None,
            max_containers: None,
            max_services: None,
        },
        namespaces: vec!["delete-namespace".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "test-user".to_string(),
        description: None,
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    // Create the tenant
    tenant_manager.create_tenant(tenant).await?;
    
    // Verify tenant exists
    let tenant_exists = tenant_manager.get_tenant(&tenant_id).await.is_some();
    assert!(tenant_exists, "Tenant should exist before deletion");
    
    // Delete the tenant
    tenant_manager.delete_tenant(&tenant_id).await?;
    
    // Verify tenant no longer exists
    let tenant_exists = tenant_manager.get_tenant(&tenant_id).await.is_some();
    assert!(!tenant_exists, "Tenant should not exist after deletion");
    
    Ok(())
}

#[tokio::test]
async fn test_tenant_context() -> Result<()> {
    // Setup
    let rbac_manager = Arc::new(RbacManager::new());
    let tenant_manager = TenantManager::new(rbac_manager.clone());
    
    // Create test tenants
    let tenant1_id = "context-tenant1".to_string();
    let tenant1 = Tenant {
        id: tenant1_id.clone(),
        name: "Context Tenant 1".to_string(),
        isolation_level: IsolationLevel::Soft,
        resource_quotas: ResourceQuotas {
            cpu_limit: None,
            memory_limit: None,
            storage_limit: None,
            max_containers: None,
            max_services: None,
        },
        namespaces: vec!["context-ns1".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "context-user".to_string(),
        description: None,
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    let tenant2_id = "context-tenant2".to_string();
    let tenant2 = Tenant {
        id: tenant2_id.clone(),
        name: "Context Tenant 2".to_string(),
        isolation_level: IsolationLevel::Soft,
        resource_quotas: ResourceQuotas {
            cpu_limit: None,
            memory_limit: None,
            storage_limit: None,
            max_containers: None,
            max_services: None,
        },
        namespaces: vec!["context-ns2".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "other-user".to_string(),
        description: None,
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    // Create the tenants
    tenant_manager.create_tenant(tenant1).await?;
    tenant_manager.create_tenant(tenant2).await?;
    
    // Create a test user with access to tenant1
    let user = User {
        id: "context-user".to_string(),
        username: "context-user".to_string(),
        password_hash: "hash".to_string(),
        email: "context@example.com".to_string(),
        full_name: "Context User".to_string(),
        roles: vec![format!("{}-admin", tenant1_id)],
        groups: Vec::new(),
        created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        last_login: None,
        active: true,
    };
    
    // Add user to RBAC
    rbac_manager.create_user(user).await?;
    
    // Get tenant context for user
    let tenant_context = tenant_manager.get_tenant_context("context-user").await?;
    
    // User should have access to tenant1 (as owner) and default tenant
    assert!(tenant_context.contains(&tenant1_id), "User should have access to tenant1");
    assert!(tenant_context.contains(&"default".to_string()), "User should have access to default tenant");
    assert!(!tenant_context.contains(&tenant2_id), "User should not have access to tenant2");
    
    // Admin should have access to all tenants
    let admin_context = tenant_manager.get_tenant_context("admin").await?;
    assert!(admin_context.contains(&tenant1_id), "Admin should have access to tenant1");
    assert!(admin_context.contains(&tenant2_id), "Admin should have access to tenant2");
    assert!(admin_context.contains(&"default".to_string()), "Admin should have access to default tenant");
    
    Ok(())
}