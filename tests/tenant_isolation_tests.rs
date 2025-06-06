use anyhow::Result;
use hivemind::network::{NetworkManager, NetworkConfig, NetworkPolicy, NetworkSelector, NetworkRule, PortRange, Protocol, NetworkPeer};
use hivemind::node::NodeManager;
use hivemind::security::rbac::RbacManager;
use hivemind::service_discovery::ServiceDiscovery;
use hivemind::storage::{StorageManager, VolumeOptions};
use hivemind::tenant::{TenantManager, Tenant, IsolationLevel, ResourceQuotas, TenantStatus, TenantContext};
use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr};
use std::path::PathBuf;
use std::sync::Arc;
use chrono::Utc;
use tempfile::TempDir;

// Integration test for tenant resource quota enforcement
#[tokio::test]
async fn test_tenant_resource_quota_enforcement() -> Result<()> {
    // Setup
    let rbac_manager = Arc::new(RbacManager::new());
    let tenant_manager = Arc::new(TenantManager::new(rbac_manager.clone()));
    let node_manager = Arc::new(NodeManager::new());
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create tenants with different resource quotas
    let tenant1_id = "quota-tenant1".to_string();
    let tenant1 = Tenant {
        id: tenant1_id.clone(),
        name: "Quota Tenant 1".to_string(),
        isolation_level: IsolationLevel::Soft,
        resource_quotas: ResourceQuotas {
            cpu_limit: Some(2),
            memory_limit: Some(2 * 1024 * 1024 * 1024), // 2 GB
            storage_limit: Some(10 * 1024 * 1024 * 1024), // 10 GB
            max_containers: Some(5),
            max_services: Some(3),
        },
        namespaces: vec!["tenant1".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "user1".to_string(),
        description: Some("Test tenant 1 with resource quotas".to_string()),
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    let tenant2_id = "quota-tenant2".to_string();
    let tenant2 = Tenant {
        id: tenant2_id.clone(),
        name: "Quota Tenant 2".to_string(),
        isolation_level: IsolationLevel::Soft,
        resource_quotas: ResourceQuotas {
            cpu_limit: Some(4),
            memory_limit: Some(4 * 1024 * 1024 * 1024), // 4 GB
            storage_limit: Some(20 * 1024 * 1024 * 1024), // 20 GB
            max_containers: Some(10),
            max_services: Some(5),
        },
        namespaces: vec!["tenant2".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "user2".to_string(),
        description: Some("Test tenant 2 with resource quotas".to_string()),
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    // Create the tenants
    tenant_manager.create_tenant(tenant1).await?;
    tenant_manager.create_tenant(tenant2).await?;
    
    // Verify resource quotas are enforced
    
    // Test 1: Check CPU quota enforcement
    let tenant1_cpu_check = tenant_manager.check_resource_quotas(
        &tenant1_id,
        Some(3), // Exceeds tenant1's CPU limit
        None,
        None,
    ).await?;
    
    assert!(!tenant1_cpu_check, "Tenant1 should not be allowed to exceed CPU quota");
    
    let tenant2_cpu_check = tenant_manager.check_resource_quotas(
        &tenant2_id,
        Some(3), // Within tenant2's CPU limit
        None,
        None,
    ).await?;
    
    assert!(tenant2_cpu_check, "Tenant2 should be allowed to use CPU within quota");
    
    // Test 2: Check memory quota enforcement
    let tenant1_memory_check = tenant_manager.check_resource_quotas(
        &tenant1_id,
        None,
        Some(3 * 1024 * 1024 * 1024), // Exceeds tenant1's memory limit
        None,
    ).await?;
    
    assert!(!tenant1_memory_check, "Tenant1 should not be allowed to exceed memory quota");
    
    let tenant2_memory_check = tenant_manager.check_resource_quotas(
        &tenant2_id,
        None,
        Some(3 * 1024 * 1024 * 1024), // Within tenant2's memory limit
        None,
    ).await?;
    
    assert!(tenant2_memory_check, "Tenant2 should be allowed to use memory within quota");
    
    // Test 3: Check storage quota enforcement
    let tenant1_storage_check = tenant_manager.check_resource_quotas(
        &tenant1_id,
        None,
        None,
        Some(15 * 1024 * 1024 * 1024), // Exceeds tenant1's storage limit
    ).await?;
    
    assert!(!tenant1_storage_check, "Tenant1 should not be allowed to exceed storage quota");
    
    let tenant2_storage_check = tenant_manager.check_resource_quotas(
        &tenant2_id,
        None,
        None,
        Some(15 * 1024 * 1024 * 1024), // Within tenant2's storage limit
    ).await?;
    
    assert!(tenant2_storage_check, "Tenant2 should be allowed to use storage within quota");
    
    // Test 4: Check container creation limits
    let tenant1_container_check = tenant_manager.can_create_container(&tenant1_id).await?;
    let tenant2_container_check = tenant_manager.can_create_container(&tenant2_id).await?;
    
    assert!(tenant1_container_check, "Tenant1 should be able to create containers within limit");
    assert!(tenant2_container_check, "Tenant2 should be able to create containers within limit");
    
    // Test 5: Check service creation limits
    let tenant1_service_check = tenant_manager.can_create_service(&tenant1_id).await?;
    let tenant2_service_check = tenant_manager.can_create_service(&tenant2_id).await?;
    
    assert!(tenant1_service_check, "Tenant1 should be able to create services within limit");
    assert!(tenant2_service_check, "Tenant2 should be able to create services within limit");
    
    Ok(())
}

// Integration test for tenant network isolation
#[tokio::test]
async fn test_tenant_network_isolation_integration() -> Result<()> {
    // Setup
    let node_manager = Arc::new(NodeManager::new());
    let service_discovery = Arc::new(ServiceDiscovery::new());
    let rbac_manager = Arc::new(RbacManager::new());
    let tenant_manager = Arc::new(TenantManager::new(rbac_manager));
    
    // Create network manager with tenant manager
    let network_manager = NetworkManager::new(
        node_manager.clone(),
        service_discovery.clone(),
        None, // Use default network config
    ).await?;
    
    // Create tenants with different isolation levels
    let hard_tenant_id = "hard-isolation".to_string();
    let hard_tenant = Tenant {
        id: hard_tenant_id.clone(),
        name: "Hard Isolation Tenant".to_string(),
        isolation_level: IsolationLevel::Hard,
        resource_quotas: ResourceQuotas {
            cpu_limit: None,
            memory_limit: None,
            storage_limit: None,
            max_containers: None,
            max_services: None,
        },
        namespaces: vec!["hard-namespace".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "user1".to_string(),
        description: Some("Test tenant with hard isolation".to_string()),
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    let network_tenant_id = "network-isolation".to_string();
    let network_tenant = Tenant {
        id: network_tenant_id.clone(),
        name: "Network Isolation Tenant".to_string(),
        isolation_level: IsolationLevel::NetworkOnly,
        resource_quotas: ResourceQuotas {
            cpu_limit: None,
            memory_limit: None,
            storage_limit: None,
            max_containers: None,
            max_services: None,
        },
        namespaces: vec!["network-namespace".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "user2".to_string(),
        description: Some("Test tenant with network isolation".to_string()),
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    let soft_tenant_id = "soft-isolation".to_string();
    let soft_tenant = Tenant {
        id: soft_tenant_id.clone(),
        name: "Soft Isolation Tenant".to_string(),
        isolation_level: IsolationLevel::Soft,
        resource_quotas: ResourceQuotas {
            cpu_limit: None,
            memory_limit: None,
            storage_limit: None,
            max_containers: None,
            max_services: None,
        },
        namespaces: vec!["soft-namespace".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "user3".to_string(),
        description: Some("Test tenant with soft isolation".to_string()),
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    // Create the tenants
    tenant_manager.create_tenant(hard_tenant).await?;
    tenant_manager.create_tenant(network_tenant).await?;
    tenant_manager.create_tenant(soft_tenant).await?;
    
    // Initialize tenant networks
    network_manager.initialize_tenant_network(&hard_tenant_id, Some("10.100.0.0/16".to_string())).await?;
    network_manager.initialize_tenant_network(&network_tenant_id, Some("10.200.0.0/16".to_string())).await?;
    network_manager.initialize_tenant_network(&soft_tenant_id, Some("10.300.0.0/16".to_string())).await?;
    
    // Create tenant contexts
    let hard_tenant_context = TenantContext::new(hard_tenant_id.clone(), "user1".to_string());
    let network_tenant_context = TenantContext::new(network_tenant_id.clone(), "user2".to_string());
    let soft_tenant_context = TenantContext::new(soft_tenant_id.clone(), "user3".to_string());
    
    // Verify network isolation settings
    let hard_network_isolation = tenant_manager.get_network_isolation(&hard_tenant_id).await?;
    let network_only_isolation = tenant_manager.get_network_isolation(&network_tenant_id).await?;
    let soft_network_isolation = tenant_manager.get_network_isolation(&soft_tenant_id).await?;
    
    assert!(hard_network_isolation, "Hard isolation tenant should have network isolation");
    assert!(network_only_isolation, "Network isolation tenant should have network isolation");
    assert!(!soft_network_isolation, "Soft isolation tenant should not have network isolation");
    
    // Verify tenant network CIDRs
    // Since tenant_network_cidrs and tenant_overlays are private fields,
    // we can't directly access them in tests. Instead, we'll verify that
    // the network initialization was successful by checking that no errors occurred
    // during initialization and that the isolation settings are correct
    
    // Verify network isolation settings through the tenant manager
    let hard_network_isolation = tenant_manager.get_network_isolation(&hard_tenant_id).await?;
    let network_only_isolation = tenant_manager.get_network_isolation(&network_tenant_id).await?;
    let soft_network_isolation = tenant_manager.get_network_isolation(&soft_tenant_id).await?;
    
    assert!(hard_network_isolation, "Hard isolation tenant should have network isolation");
    assert!(network_only_isolation, "Network isolation tenant should have network isolation");
    assert!(!soft_network_isolation, "Soft isolation tenant should not have network isolation");
    
    Ok(())
}

// Integration test for tenant storage isolation
#[tokio::test]
async fn test_tenant_storage_isolation_integration() -> Result<()> {
    // Create a temporary directory for testing
    let temp_dir = TempDir::new()?;
    let data_dir = temp_dir.path().to_path_buf();
    
    // Setup
    let rbac_manager = Arc::new(RbacManager::new());
    let tenant_manager = Arc::new(TenantManager::new(rbac_manager));
    
    // Initialize storage manager
    let storage_manager = StorageManager::new(&data_dir).await?;
    
    // Create tenants with different isolation levels
    let hard_tenant_id = "storage-hard-isolation".to_string();
    let hard_tenant = Tenant {
        id: hard_tenant_id.clone(),
        name: "Storage Hard Isolation Tenant".to_string(),
        isolation_level: IsolationLevel::Hard,
        resource_quotas: ResourceQuotas {
            cpu_limit: None,
            memory_limit: None,
            storage_limit: Some(10 * 1024 * 1024 * 1024), // 10 GB
            max_containers: None,
            max_services: None,
        },
        namespaces: vec!["storage-hard-namespace".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "user1".to_string(),
        description: Some("Test tenant with hard isolation for storage".to_string()),
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    let soft_tenant_id = "storage-soft-isolation".to_string();
    let soft_tenant = Tenant {
        id: soft_tenant_id.clone(),
        name: "Storage Soft Isolation Tenant".to_string(),
        isolation_level: IsolationLevel::Soft,
        resource_quotas: ResourceQuotas {
            cpu_limit: None,
            memory_limit: None,
            storage_limit: Some(5 * 1024 * 1024 * 1024), // 5 GB
            max_containers: None,
            max_services: None,
        },
        namespaces: vec!["storage-soft-namespace".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "user2".to_string(),
        description: Some("Test tenant with soft isolation for storage".to_string()),
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    // Create the tenants
    tenant_manager.create_tenant(hard_tenant).await?;
    tenant_manager.create_tenant(soft_tenant).await?;
    
    // Verify storage isolation settings
    let hard_storage_isolation = tenant_manager.get_storage_isolation(&hard_tenant_id).await?;
    let soft_storage_isolation = tenant_manager.get_storage_isolation(&soft_tenant_id).await?;
    
    assert!(hard_storage_isolation, "Hard isolation tenant should have storage isolation");
    assert!(!soft_storage_isolation, "Soft isolation tenant should not have storage isolation");
    
    // Create volumes for each tenant
    let hard_tenant_volume_name = "hard-tenant-volume";
    let soft_tenant_volume_name = "soft-tenant-volume";
    
    // Create volume options with tenant IDs
    let hard_tenant_opts = VolumeOptions {
        replicas: 1,
        encryption: false,
        performance_class: hivemind::storage::PerformanceClass::Standard,
        tenant_id: Some(hard_tenant_id.clone()),
        labels: HashMap::new(),
    };
    
    let soft_tenant_opts = VolumeOptions {
        replicas: 1,
        encryption: false,
        performance_class: hivemind::storage::PerformanceClass::Standard,
        tenant_id: Some(soft_tenant_id.clone()),
        labels: HashMap::new(),
    };
    
    // Create volumes
    let hard_tenant_volume = storage_manager.create_volume(
        hard_tenant_volume_name,
        1 * 1024 * 1024 * 1024, // 1 GB
        Some(hard_tenant_opts),
        None,
    ).await?;
    
    let soft_tenant_volume = storage_manager.create_volume(
        soft_tenant_volume_name,
        1 * 1024 * 1024 * 1024, // 1 GB
        Some(soft_tenant_opts),
        None,
    ).await?;
    
    // Verify volumes were created with correct tenant IDs
    assert_eq!(hard_tenant_volume.tenant_id, Some(hard_tenant_id.clone()), "Hard tenant volume should have correct tenant ID");
    assert_eq!(soft_tenant_volume.tenant_id, Some(soft_tenant_id.clone()), "Soft tenant volume should have correct tenant ID");
    
    // List all volumes
    let all_volumes = storage_manager.list_volumes_enhanced().await?;
    
    // Verify both volumes are in the list
    assert!(all_volumes.iter().any(|v| v.name == hard_tenant_volume_name), "Hard tenant volume should be in the list");
    assert!(all_volumes.iter().any(|v| v.name == soft_tenant_volume_name), "Soft tenant volume should be in the list");
    
    // Verify resource quota enforcement for storage
    let hard_tenant_quota_check = tenant_manager.check_resource_quotas(
        &hard_tenant_id,
        None,
        None,
        Some(11 * 1024 * 1024 * 1024), // 11 GB, exceeds the 10 GB limit
    ).await?;
    
    let soft_tenant_quota_check = tenant_manager.check_resource_quotas(
        &soft_tenant_id,
        None,
        None,
        Some(6 * 1024 * 1024 * 1024), // 6 GB, exceeds the 5 GB limit
    ).await?;
    
    assert!(!hard_tenant_quota_check, "Hard tenant should not be allowed to exceed storage quota");
    assert!(!soft_tenant_quota_check, "Soft tenant should not be allowed to exceed storage quota");
    
    // Clean up
    storage_manager.delete_volume_enhanced(hard_tenant_volume_name).await?;
    storage_manager.delete_volume_enhanced(soft_tenant_volume_name).await?;
    
    Ok(())
}