use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::RwLock;
use std::time::{SystemTime, UNIX_EPOCH};

pub mod container_scanning;
pub mod network_policy;
pub mod rbac;
pub mod secret_management;

pub use container_scanning::{
    ContainerScanner, SecurityPolicy, ScanResult, Vulnerability, VulnerabilitySeverity,
    SecurityEvent, SecurityEventType, SecurityEventSeverity, RuntimeCheckResult, RuntimeViolation,
    ScanStatus
};

pub use network_policy::{
    NetworkPolicyEnforcer, EnhancedNetworkPolicy, EncryptionType, TrafficLoggingLevel,
    NetworkTrafficLog, NetworkAction, NetworkSecurityAlert, NetworkAlertType, AlertSeverity
};

pub use rbac::{
    RbacManager, User, Role, Group, Permission, PermissionScope, AuthRequest, AuthResponse,
    AuditLogEntry, AuditStatus
};

pub use secret_management::{
    SecretManager, Secret, SecretReference, SecretAccessLog, SecretAction,
    EncryptionKey, EncryptionAlgorithm
};

/// SecurityManager is the main entry point for all security features in Hivemind
pub struct SecurityManager {
    container_scanner: Arc<ContainerScanner>,
    network_policy_enforcer: Arc<NetworkPolicyEnforcer>,
    rbac_manager: Arc<RbacManager>,
    secret_manager: Arc<SecretManager>,
}

impl SecurityManager {
    pub fn new() -> Self {
        Self {
            container_scanner: Arc::new(ContainerScanner::new()),
            network_policy_enforcer: Arc::new(NetworkPolicyEnforcer::new()),
            rbac_manager: Arc::new(RbacManager::new()),
            secret_manager: Arc::new(SecretManager::new()),
        }
    }

    pub fn container_scanner(&self) -> Arc<ContainerScanner> {
        self.container_scanner.clone()
    }

    pub fn network_policy_enforcer(&self) -> Arc<NetworkPolicyEnforcer> {
        self.network_policy_enforcer.clone()
    }

    pub fn rbac_manager(&self) -> Arc<RbacManager> {
        self.rbac_manager.clone()
    }

    pub fn secret_manager(&self) -> Arc<SecretManager> {
        self.secret_manager.clone()
    }
    
    /// Initialize all security components
    pub async fn initialize(&self) -> Result<()> {
        println!("Initializing security components...");
        
        // Initialize default security policies
        self.initialize_default_policies().await?;
        
        Ok(())
    }
    
    /// Initialize default security policies
    async fn initialize_default_policies(&self) -> Result<()> {
        // Create a default container security policy
        let default_policy = SecurityPolicy {
            id: "default".to_string(),
            name: "Default Security Policy".to_string(),
            description: "Default security policy for all containers".to_string(),
            max_severity: VulnerabilitySeverity::Medium,
            block_on_severity: VulnerabilitySeverity::High,
            whitelist_cves: Vec::new(),
            blacklist_cves: Vec::new(),
            allowed_registries: vec!["docker.io".to_string(), "gcr.io".to_string(), "quay.io".to_string()],
            required_labels: HashMap::new(),
            scan_frequency: std::time::Duration::from_secs(86400), // 24 hours
            created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        };
        
        self.container_scanner.create_policy(default_policy).await?;
        
        // Create a default network policy
        let base_policy = crate::network::NetworkPolicy {
            name: "default-network-policy".to_string(),
            selector: crate::network::NetworkSelector {
                labels: HashMap::new(),
            },
            ingress_rules: Vec::new(),
            egress_rules: Vec::new(),
        };
        
        let enhanced_policy = EnhancedNetworkPolicy {
            base_policy,
            encryption_required: false,
            encryption_type: None,
            traffic_logging: true,
            traffic_logging_level: TrafficLoggingLevel::Metadata,
            intrusion_detection: false,
            created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            owner: "system".to_string(),
        };
        
        self.network_policy_enforcer.create_policy(enhanced_policy).await?;
        
        Ok(())
    }
    
    /// Scan a container image for vulnerabilities
    pub async fn scan_container_image(&self, image_name: &str, image_id: &str) -> Result<ScanResult> {
        self.container_scanner.scan_image(image_name, image_id).await
    }
    
    /// Check if a container image is compliant with security policies
    pub async fn check_image_compliance(&self, scan_result: &ScanResult) -> Result<bool> {
        self.container_scanner.check_compliance(scan_result).await
    }
    
    /// Perform a runtime security check on a container
    pub async fn check_container_runtime(&self, container_id: &str) -> Result<RuntimeCheckResult> {
        self.container_scanner.check_container_runtime(container_id).await
    }
    
    /// Check if network traffic is allowed between containers
    pub async fn is_network_traffic_allowed(
        &self,
        source_container: &str,
        destination_container: &str,
        protocol: crate::network::Protocol,
        port: u16,
    ) -> Result<bool> {
        self.network_policy_enforcer.is_traffic_allowed(
            source_container,
            destination_container,
            protocol,
            port,
        ).await
    }
    
    /// Authenticate a user
    pub async fn authenticate_user(&self, username: &str, password: &str) -> Result<AuthResponse> {
        let auth_request = AuthRequest {
            username: username.to_string(),
            password: password.to_string(),
        };
        
        self.rbac_manager.authenticate(&auth_request).await
    }
    
    /// Check if a user has permission to perform an action
    pub async fn check_permission(
        &self,
        user_id: &str,
        resource: &str,
        action: &str,
        scope: &PermissionScope,
    ) -> Result<bool> {
        self.rbac_manager.check_permission(user_id, resource, action, scope).await
    }
    
    /// Create a new secret
    pub async fn create_secret(
        &self,
        name: &str,
        description: &str,
        data: &[u8],
        created_by: &str,
        labels: HashMap<String, String>,
        metadata: HashMap<String, String>,
        expiration: Option<i64>,
        rotation_period: Option<i64>,
    ) -> Result<Secret> {
        self.secret_manager.create_secret(
            name,
            description,
            data,
            created_by,
            labels,
            metadata,
            expiration,
            rotation_period,
        ).await
    }
    
    /// Mount a secret to a container
    pub async fn mount_secret_to_container(
        &self,
        secret_id: &str,
        container_id: &str,
        mount_path: &str,
        env_var: Option<&str>,
        file_name: Option<&str>,
        file_mode: Option<u32>,
        user_id: &str,
    ) -> Result<SecretReference> {
        self.secret_manager.mount_secret_to_container(
            secret_id,
            container_id,
            mount_path,
            env_var,
            file_name,
            file_mode,
            user_id,
        ).await
    }
    
    /// Get all security events
    pub async fn get_security_events(&self) -> Vec<SecurityEvent> {
        self.container_scanner.get_security_events().await
    }
    
    /// Get all audit logs
    pub async fn get_audit_logs(&self) -> Vec<AuditLogEntry> {
        self.rbac_manager.get_audit_logs().await
    }
}