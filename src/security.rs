use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::RwLock;
use std::time::{SystemTime, UNIX_EPOCH};

pub mod container_scanning;
pub mod network_policy;
pub mod network_policy_controller;
pub mod network_policy_manager;
pub mod rbac;
pub mod secret_management;
pub mod storage_acl;
pub mod storage_encryption;
pub mod storage_qos;

pub use container_scanning::{
    ContainerScanner, SecurityPolicy, ScanResult, Vulnerability, VulnerabilitySeverity,
    SecurityEvent, SecurityEventType, SecurityEventSeverity, RuntimeCheckResult, RuntimeViolation,
    ScanStatus
};

pub use network_policy::{
    NetworkPolicyEnforcer, EnhancedNetworkPolicy, EncryptionType, TrafficLoggingLevel,
    NetworkTrafficLog, NetworkAction, NetworkSecurityAlert, NetworkAlertType, AlertSeverity
};

pub use network_policy_controller::{
    NetworkPolicyController, ViolationLog
};

pub use network_policy_manager::NetworkPolicyManager;

pub use rbac::{
    RbacManager, User, Role, Group, Permission, PermissionScope, AuthRequest, AuthResponse,
    AuditLogEntry, AuditStatus
};

pub use secret_management::{
    SecretManager, Secret, SecretReference, SecretAccessLog, SecretAction,
    EncryptionKey, EncryptionAlgorithm
};

pub use storage_acl::{
    StorageAccessControl, VolumeAcl, StorageOperation, StorageAuditLogEntry
};

pub use storage_encryption::{
    StorageEncryptionManager, TenantEncryptionKey
};

pub use storage_qos::{
    StorageQoSManager, StorageOperationType, TenantQoSPolicy, TenantStorageStats
};

/// SecurityManager is the main entry point for all security features in Hivemind
pub struct SecurityManager {
    container_scanner: Arc<ContainerScanner>,
    network_policy_enforcer: Arc<NetworkPolicyEnforcer>,
    network_policy_controller: Arc<RwLock<NetworkPolicyController>>,
    network_policy_manager: Arc<RwLock<NetworkPolicyManager>>,
    rbac_manager: Arc<RbacManager>,
    secret_manager: Arc<SecretManager>,
    storage_access_control: Arc<StorageAccessControl>,
    storage_encryption_manager: Arc<StorageEncryptionManager>,
    storage_qos_manager: Arc<StorageQoSManager>,
}

impl SecurityManager {
    pub fn new() -> Self {
        Self {
            container_scanner: Arc::new(ContainerScanner::new()),
            network_policy_enforcer: Arc::new(NetworkPolicyEnforcer::new()),
            network_policy_controller: Arc::new(RwLock::new(NetworkPolicyController::new())),
            network_policy_manager: Arc::new(RwLock::new(NetworkPolicyManager::new())),
            rbac_manager: Arc::new(RbacManager::new()),
            secret_manager: Arc::new(SecretManager::new()),
            storage_access_control: Arc::new(StorageAccessControl::new()),
            storage_encryption_manager: Arc::new(StorageEncryptionManager::new()),
            storage_qos_manager: Arc::new(StorageQoSManager::default()),
        }
    }

    pub fn container_scanner(&self) -> Arc<ContainerScanner> {
        self.container_scanner.clone()
    }

    pub fn network_policy_enforcer(&self) -> Arc<NetworkPolicyEnforcer> {
        self.network_policy_enforcer.clone()
    }
    
    pub fn network_policy_controller(&self) -> Arc<RwLock<NetworkPolicyController>> {
        self.network_policy_controller.clone()
    }
    
    pub fn network_policy_manager(&self) -> Arc<RwLock<NetworkPolicyManager>> {
        self.network_policy_manager.clone()
    }

    pub fn rbac_manager(&self) -> Arc<RbacManager> {
        self.rbac_manager.clone()
    }

    pub fn secret_manager(&self) -> Arc<SecretManager> {
        self.secret_manager.clone()
    }
    
    pub fn storage_access_control(&self) -> Arc<StorageAccessControl> {
        self.storage_access_control.clone()
    }
    
    pub fn storage_encryption_manager(&self) -> Arc<StorageEncryptionManager> {
        self.storage_encryption_manager.clone()
    }
    
    pub fn storage_qos_manager(&self) -> Arc<StorageQoSManager> {
        self.storage_qos_manager.clone()
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
        let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs();
        let base_policy = crate::network::NetworkPolicy {
            name: "default-network-policy".to_string(),
            selector: crate::network::NetworkSelector {
                labels: HashMap::new(),
            },
            ingress_rules: Vec::new(),
            egress_rules: Vec::new(),
            priority: 100,
            namespace: None,
            tenant_id: None,
            labels: HashMap::new(),
            created_at: now,
            updated_at: now,
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
        // First try with the new controller
        let controller = self.network_policy_controller.read().await;
        match controller.is_traffic_allowed(
            source_container,
            destination_container,
            protocol,
            port,
        ).await {
            Ok(result) => Ok(result),
            // Fall back to the old enforcer if the new controller fails
            Err(_) => {
                self.network_policy_enforcer.is_traffic_allowed(
                    source_container,
                    destination_container,
                    protocol,
                    port,
                ).await
            }
        }
    }
    
    /// Get network policy violation logs
    pub async fn get_network_policy_violations(&self) -> Vec<ViolationLog> {
        self.network_policy_controller.read().await.get_violation_logs().await
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
    
    /// Run network policy reconciliation to ensure policies are consistently applied
    pub async fn run_network_policy_reconciliation(&self) -> Result<()> {
        // Run reconciliation on the new controller
        self.network_policy_controller.write().await.run_reconciliation().await?;
        
        // Also run reconciliation on the network policy manager
        self.network_policy_manager.write().await.run_reconciliation().await?;
        
        Ok(())
    }
    
    /// Create default network policies for a new tenant
    pub async fn create_default_tenant_network_policies(&self, tenant_id: &str) -> Result<()> {
        // Create default policies using the new controller
        self.network_policy_controller.write().await.create_default_tenant_policies(tenant_id).await?;
        
        // Also create default policies using the network policy manager
        self.network_policy_manager.write().await.create_default_tenant_policies(tenant_id).await?;
        
        Ok(())
    }
}