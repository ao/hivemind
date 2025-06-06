use anyhow::Result;
use hivemind::security::{
    StorageAccessControl, StorageEncryptionManager, StorageQoSManager,
    StorageOperation, AuditStatus
};
use hivemind::storage::{StorageManager, VolumeOptions, StorageProviderType};
use hivemind::tenant::{TenantManager, Tenant, IsolationLevel, ResourceQuotas, TenantStatus};
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;
use chrono::Utc;
use tempfile::TempDir;

#[tokio::test]
async fn test_tenant_storage_isolation() -> Result<()> {
    // Create a temporary directory for testing
    let temp_dir = TempDir::new()?;
    let data_dir = temp_dir.path().to_path_buf();
    
    // Initialize security components
    let access_control = Arc::new(StorageAccessControl::new());
    let encryption_manager = Arc::new(StorageEncryptionManager::new());
    let qos_manager = Arc::new(StorageQoSManager::default());
    
    // Initialize RBAC manager for tenant manager
    let rbac_manager = Arc::new(hivemind::security::rbac::RbacManager::new());
    let tenant_manager = Arc::new(TenantManager::new(rbac_manager));
    
    // Initialize storage manager with security components
    let storage_manager = StorageManager::new(&data_dir).await?
        .with_security_components(
            access_control.clone(),
            encryption_manager.clone(),
            qos_manager.clone()
        )
        .with_tenant_manager(tenant_manager.clone());
    
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
    
    // Test 1: Create volumes for each tenant
    // Hard tenant should have encrypted volume with namespace
    let hard_tenant_volume_name = "hard-tenant-volume";
    let hard_tenant_opts = VolumeOptions {
        replicas: 1,
        encryption: true,
        performance_class: hivemind::storage::PerformanceClass::Standard,
        tenant_id: Some(hard_tenant_id.clone()),
        namespace: Some(format!("tenant-{}", hard_tenant_id)),
        owner_id: Some("user1".to_string()),
        labels: HashMap::new(),
        encryption_key_rotation: false,
    };
    
    let hard_tenant_volume = storage_manager.create_volume(
        hard_tenant_volume_name,
        1 * 1024 * 1024 * 1024, // 1 GB
        Some(hard_tenant_opts),
        None,
        Some("user1"),
    ).await?;
    
    // Soft tenant should have unencrypted volume without namespace
    let soft_tenant_volume_name = "soft-tenant-volume";
    let soft_tenant_opts = VolumeOptions {
        replicas: 1,
        encryption: false,
        performance_class: hivemind::storage::PerformanceClass::Standard,
        tenant_id: Some(soft_tenant_id.clone()),
        namespace: None,
        owner_id: Some("user2".to_string()),
        labels: HashMap::new(),
        encryption_key_rotation: false,
    };
    
    let soft_tenant_volume = storage_manager.create_volume(
        soft_tenant_volume_name,
        1 * 1024 * 1024 * 1024, // 1 GB
        Some(soft_tenant_opts),
        None,
        Some("user2"),
    ).await?;
    
    // Test 2: Verify volume properties
    assert_eq!(hard_tenant_volume.tenant_id, Some(hard_tenant_id.clone()), "Hard tenant volume should have correct tenant ID");
    assert_eq!(soft_tenant_volume.tenant_id, Some(soft_tenant_id.clone()), "Soft tenant volume should have correct tenant ID");
    
    assert!(hard_tenant_volume.encrypted, "Hard tenant volume should be encrypted");
    assert!(!soft_tenant_volume.encrypted, "Soft tenant volume should not be encrypted");
    
    assert_eq!(hard_tenant_volume.namespace, Some(format!("tenant-{}", hard_tenant_id)), "Hard tenant volume should have namespace");
    assert!(soft_tenant_volume.namespace.is_none(), "Soft tenant volume should not have namespace");
    
    // Test 3: Test access control
    // User1 should be able to access hard tenant volume but not soft tenant volume
    let hard_volume = storage_manager.get_volume_enhanced(hard_tenant_volume_name, Some("user1")).await?;
    assert_eq!(hard_volume.name, hard_tenant_volume_name);
    
    // This should fail with access denied
    let soft_volume_result = storage_manager.get_volume_enhanced(soft_tenant_volume_name, Some("user1")).await;
    assert!(soft_volume_result.is_err(), "User1 should not be able to access soft tenant volume");
    
    // Test 4: Test encryption
    let test_data = b"This is a test of tenant-specific encryption";
    let encrypted_data = storage_manager.encrypt_data(Some(&hard_tenant_id), test_data).await?;
    let decrypted_data = storage_manager.decrypt_data(Some(&hard_tenant_id), &encrypted_data).await?;
    
    assert_eq!(decrypted_data, test_data, "Encryption/decryption should preserve data");
    
    // Test 5: Test QoS rate limiting
    // Set a low rate limit for the soft tenant
    storage_manager.set_tenant_storage_rate_limit(&soft_tenant_id, 2).await?;
    
    // Get QoS stats
    let stats = storage_manager.get_tenant_storage_stats(&soft_tenant_id).await;
    assert!(stats.is_some(), "QoS stats should be available");
    
    // Test 6: Test audit logging
    let audit_logs = storage_manager.get_storage_audit_logs().await;
    assert!(!audit_logs.is_empty(), "Audit logs should not be empty");
    
    let tenant_logs = storage_manager.get_tenant_storage_audit_logs(&hard_tenant_id).await;
    assert!(!tenant_logs.is_empty(), "Tenant audit logs should not be empty");
    
    let volume_logs = storage_manager.get_volume_audit_logs(hard_tenant_volume_name).await;
    assert!(!volume_logs.is_empty(), "Volume audit logs should not be empty");
    
    // Clean up
    storage_manager.delete_volume_enhanced(hard_tenant_volume_name, Some("user1")).await?;
    storage_manager.delete_volume_enhanced(soft_tenant_volume_name, Some("user2")).await?;
    
    Ok(())
}