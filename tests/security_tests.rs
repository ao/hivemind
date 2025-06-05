use anyhow::Result;
use hivemind::security::{
    SecurityManager, SecurityPolicy, VulnerabilitySeverity, EnhancedNetworkPolicy,
    TrafficLoggingLevel, User, Role, Permission, PermissionScope, Secret
};
use hivemind::network::{NetworkPolicy, NetworkSelector};
use std::collections::HashMap;
use std::time::{SystemTime, UNIX_EPOCH};

#[tokio::test]
async fn test_container_scanner() -> Result<()> {
    // Create a security manager
    let security_manager = SecurityManager::new();
    
    // Initialize security components
    security_manager.initialize().await?;
    
    // Create a security policy
    let policy = SecurityPolicy {
        id: "test-policy".to_string(),
        name: "Test Security Policy".to_string(),
        description: "Test security policy for containers".to_string(),
        max_severity: VulnerabilitySeverity::Medium,
        block_on_severity: VulnerabilitySeverity::High,
        whitelist_cves: Vec::new(),
        blacklist_cves: Vec::new(),
        allowed_registries: vec!["docker.io".to_string()],
        required_labels: HashMap::new(),
        scan_frequency: std::time::Duration::from_secs(86400), // 24 hours
        created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
    };
    
    // Add the policy
    security_manager.container_scanner().create_policy(policy).await?;
    
    // Scan a test image
    let scan_result = security_manager.scan_container_image("nginx:latest", "test-image-id").await?;
    
    // Verify scan result
    assert_eq!(scan_result.image_name, "nginx:latest");
    assert_eq!(scan_result.image_id, "test-image-id");
    
    Ok(())
}

#[tokio::test]
async fn test_network_policy_enforcer() -> Result<()> {
    // Create a security manager
    let security_manager = SecurityManager::new();
    
    // Initialize security components
    security_manager.initialize().await?;
    
    // Create a base network policy
    let base_policy = NetworkPolicy {
        name: "test-network-policy".to_string(),
        selector: NetworkSelector {
            labels: HashMap::new(),
        },
        ingress_rules: Vec::new(),
        egress_rules: Vec::new(),
    };
    
    // Create an enhanced network policy
    let enhanced_policy = EnhancedNetworkPolicy {
        base_policy,
        encryption_required: true,
        encryption_type: None,
        traffic_logging: true,
        traffic_logging_level: TrafficLoggingLevel::Metadata,
        intrusion_detection: true,
        created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        owner: "test-user".to_string(),
    };
    
    // Add the policy
    security_manager.network_policy_enforcer().create_policy(enhanced_policy).await?;
    
    // Register container labels
    let mut container1_labels = HashMap::new();
    container1_labels.insert("app".to_string(), "web".to_string());
    
    let mut container2_labels = HashMap::new();
    container2_labels.insert("app".to_string(), "db".to_string());
    
    security_manager.network_policy_enforcer().register_container_labels("container1", container1_labels).await?;
    security_manager.network_policy_enforcer().register_container_labels("container2", container2_labels).await?;
    
    Ok(())
}

#[tokio::test]
async fn test_rbac_manager() -> Result<()> {
    // Create a security manager
    let security_manager = SecurityManager::new();
    
    // Initialize security components
    security_manager.initialize().await?;
    
    // Create a role with permissions
    let role = Role {
        id: "test-role".to_string(),
        name: "Test Role".to_string(),
        description: "Test role for RBAC".to_string(),
        permissions: vec![
            Permission {
                resource: "container".to_string(),
                action: "read".to_string(),
                scope: PermissionScope::Global,
            },
            Permission {
                resource: "container".to_string(),
                action: "create".to_string(),
                scope: PermissionScope::Global,
            },
        ],
        created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
    };
    
    // Add the role
    security_manager.rbac_manager().create_role(role).await?;
    
    // Create a user
    let user = User {
        id: "test-user".to_string(),
        username: "testuser".to_string(),
        password_hash: "dummy-hash".to_string(), // In a real test, we would use a proper hash
        email: "test@example.com".to_string(),
        full_name: "Test User".to_string(),
        roles: vec!["test-role".to_string()],
        groups: Vec::new(),
        created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        last_login: None,
        active: true,
    };
    
    // Add the user
    security_manager.rbac_manager().create_user(user).await?;
    
    // Check permissions
    let has_permission = security_manager.check_permission(
        "test-user",
        "container",
        "read",
        &PermissionScope::Global,
    ).await?;
    
    assert!(has_permission);
    
    Ok(())
}

#[tokio::test]
async fn test_secret_manager() -> Result<()> {
    // Create a security manager
    let security_manager = SecurityManager::new();
    
    // Initialize security components
    security_manager.initialize().await?;
    
    // Create a secret
    let secret_data = b"test-secret-data";
    let mut labels = HashMap::new();
    labels.insert("app".to_string(), "test".to_string());
    
    let mut metadata = HashMap::new();
    metadata.insert("description".to_string(), "Test secret".to_string());
    
    let secret = security_manager.create_secret(
        "test-secret",
        "Test Secret",
        secret_data,
        "test-user",
        labels,
        metadata,
        None,
        None,
    ).await?;
    
    // Verify secret
    assert_eq!(secret.name, "test-secret");
    assert_eq!(secret.description, "Test Secret");
    
    // Mount secret to container
    let reference = security_manager.mount_secret_to_container(
        &secret.id,
        "test-container",
        "/mnt/secrets",
        Some("TEST_SECRET"),
        Some("test-secret"),
        Some(0o600),
        "test-user",
    ).await?;
    
    // Verify reference
    assert_eq!(reference.secret_id, secret.id);
    assert_eq!(reference.container_id, "test-container");
    assert_eq!(reference.mount_path, "/mnt/secrets");
    assert_eq!(reference.env_var, Some("TEST_SECRET".to_string()));
    
    Ok(())
}