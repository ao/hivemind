use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use tokio::sync::RwLock;
use chrono::Utc;

use crate::security::rbac::{Permission, PermissionScope, AuditLogEntry, AuditStatus};

/// Access control for storage operations
pub struct StorageAccessControl {
    // Map of volume_name -> access control list
    volume_acls: Arc<RwLock<HashMap<String, VolumeAcl>>>,
    // Audit log for storage operations
    audit_logs: Arc<RwLock<Vec<StorageAuditLogEntry>>>,
}

/// Access control list for a volume
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct VolumeAcl {
    pub volume_name: String,
    pub tenant_id: Option<String>,
    pub owner_id: String,
    pub read_access: HashSet<String>,  // User IDs with read access
    pub write_access: HashSet<String>, // User IDs with write access
    pub admin_access: HashSet<String>, // User IDs with admin access
    pub created_at: i64,
    pub updated_at: i64,
}

/// Storage operation types for audit logging
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum StorageOperation {
    Create,
    Read,
    Write,
    Delete,
    Attach,
    Detach,
    Snapshot,
    Restore,
    Clone,
    Expand,
    List,
}

/// Audit log entry for storage operations
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StorageAuditLogEntry {
    pub id: String,
    pub timestamp: i64,
    pub user_id: String,
    pub tenant_id: Option<String>,
    pub operation: StorageOperation,
    pub volume_name: String,
    pub status: AuditStatus,
    pub details: HashMap<String, String>,
}

impl StorageAccessControl {
    /// Create a new StorageAccessControl
    pub fn new() -> Self {
        Self {
            volume_acls: Arc::new(RwLock::new(HashMap::new())),
            audit_logs: Arc::new(RwLock::new(Vec::new())),
        }
    }
    
    /// Create an ACL for a volume
    pub async fn create_volume_acl(&self, acl: VolumeAcl) -> Result<()> {
        let mut acls = self.volume_acls.write().await;
        acls.insert(acl.volume_name.clone(), acl);
        Ok(())
    }
    
    /// Get an ACL for a volume
    pub async fn get_volume_acl(&self, volume_name: &str) -> Option<VolumeAcl> {
        let acls = self.volume_acls.read().await;
        acls.get(volume_name).cloned()
    }
    
    /// Update an ACL for a volume
    pub async fn update_volume_acl(&self, volume_name: &str, acl: VolumeAcl) -> Result<()> {
        let mut acls = self.volume_acls.write().await;
        
        if !acls.contains_key(volume_name) {
            anyhow::bail!("Volume ACL not found");
        }
        
        acls.insert(volume_name.to_string(), acl);
        Ok(())
    }
    
    /// Delete an ACL for a volume
    pub async fn delete_volume_acl(&self, volume_name: &str) -> Result<()> {
        let mut acls = self.volume_acls.write().await;
        
        if !acls.contains_key(volume_name) {
            anyhow::bail!("Volume ACL not found");
        }
        
        acls.remove(volume_name);
        Ok(())
    }
    
    /// Check if a user has read access to a volume
    pub async fn check_read_access(&self, volume_name: &str, user_id: &str, tenant_id: Option<&str>) -> Result<bool> {
        // Admin users always have access
        if user_id == "admin" {
            return Ok(true);
        }
        
        let acls = self.volume_acls.read().await;
        
        if let Some(acl) = acls.get(volume_name) {
            // Check tenant isolation
            if let Some(acl_tenant_id) = &acl.tenant_id {
                if let Some(req_tenant_id) = tenant_id {
                    if acl_tenant_id != req_tenant_id {
                        return Ok(false);
                    }
                }
            }
            
            // Check user access
            if acl.owner_id == user_id || 
               acl.read_access.contains(user_id) || 
               acl.write_access.contains(user_id) || 
               acl.admin_access.contains(user_id) {
                return Ok(true);
            }
        }
        
        Ok(false)
    }
    
    /// Check if a user has write access to a volume
    pub async fn check_write_access(&self, volume_name: &str, user_id: &str, tenant_id: Option<&str>) -> Result<bool> {
        // Admin users always have access
        if user_id == "admin" {
            return Ok(true);
        }
        
        let acls = self.volume_acls.read().await;
        
        if let Some(acl) = acls.get(volume_name) {
            // Check tenant isolation
            if let Some(acl_tenant_id) = &acl.tenant_id {
                if let Some(req_tenant_id) = tenant_id {
                    if acl_tenant_id != req_tenant_id {
                        return Ok(false);
                    }
                }
            }
            
            // Check user access
            if acl.owner_id == user_id || 
               acl.write_access.contains(user_id) || 
               acl.admin_access.contains(user_id) {
                return Ok(true);
            }
        }
        
        Ok(false)
    }
    
    /// Check if a user has admin access to a volume
    pub async fn check_admin_access(&self, volume_name: &str, user_id: &str, tenant_id: Option<&str>) -> Result<bool> {
        // Admin users always have access
        if user_id == "admin" {
            return Ok(true);
        }
        
        let acls = self.volume_acls.read().await;
        
        if let Some(acl) = acls.get(volume_name) {
            // Check tenant isolation
            if let Some(acl_tenant_id) = &acl.tenant_id {
                if let Some(req_tenant_id) = tenant_id {
                    if acl_tenant_id != req_tenant_id {
                        return Ok(false);
                    }
                }
            }
            
            // Check user access
            if acl.owner_id == user_id || acl.admin_access.contains(user_id) {
                return Ok(true);
            }
        }
        
        Ok(false)
    }
    
    /// Log a storage operation
    pub async fn log_operation(
        &self,
        user_id: &str,
        tenant_id: Option<&str>,
        operation: StorageOperation,
        volume_name: &str,
        status: AuditStatus,
        details: HashMap<String, String>,
    ) -> Result<()> {
        let entry = StorageAuditLogEntry {
            id: uuid::Uuid::new_v4().to_string(),
            timestamp: Utc::now().timestamp(),
            user_id: user_id.to_string(),
            tenant_id: tenant_id.map(|s| s.to_string()),
            operation,
            volume_name: volume_name.to_string(),
            status,
            details,
        };
        
        let mut logs = self.audit_logs.write().await;
        logs.push(entry);
        
        Ok(())
    }
    
    /// Get all audit logs
    pub async fn get_audit_logs(&self) -> Vec<StorageAuditLogEntry> {
        let logs = self.audit_logs.read().await;
        logs.clone()
    }
    
    /// Get audit logs for a specific volume
    pub async fn get_volume_audit_logs(&self, volume_name: &str) -> Vec<StorageAuditLogEntry> {
        let logs = self.audit_logs.read().await;
        logs.iter()
            .filter(|log| log.volume_name == volume_name)
            .cloned()
            .collect()
    }
    
    /// Get audit logs for a specific tenant
    pub async fn get_tenant_audit_logs(&self, tenant_id: &str) -> Vec<StorageAuditLogEntry> {
        let logs = self.audit_logs.read().await;
        logs.iter()
            .filter(|log| log.tenant_id.as_ref().map_or(false, |id| id == tenant_id))
            .cloned()
            .collect()
    }
}

impl Default for StorageAccessControl {
    fn default() -> Self {
        Self::new()
    }
}