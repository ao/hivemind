rc/storage.rs</path>
<content lines="1590-1662">
    // Check if a volume is in use by any container
    pub async fn is_volume_in_use(&self, volume_name: &str) -> Result<bool> {
        // Try to get the volume status
        match self.get_volume_status(volume_name).await {
            Ok(status) => Ok(status.state == VolumeState::InUse),
            Err(_) => Ok(false),
        }
    }
    
    // Encrypt data for a tenant
    pub async fn encrypt_data(&self, tenant_id: Option<&str>, data: &[u8]) -> Result<Vec<u8>> {
        if let Some(encryption_manager) = &self.encryption_manager {
            encryption_manager.encrypt(tenant_id, data).await
        } else {
            // If encryption manager is not available, return the data as-is
            Ok(data.to_vec())
        }
    }
    
    // Decrypt data for a tenant
    pub async fn decrypt_data(&self, tenant_id: Option<&str>, encrypted_data: &[u8]) -> Result<Vec<u8>> {
        if let Some(encryption_manager) = &self.encryption_manager {
            encryption_manager.decrypt(tenant_id, encrypted_data).await
        } else {
            // If encryption manager is not available, return the data as-is
            Ok(encrypted_data.to_vec())
        }
    }
    
    // Get storage audit logs
    pub async fn get_storage_audit_logs(&self) -> Vec<StorageAuditLogEntry> {
        if let Some(access_control) = &self.access_control {
            access_control.get_audit_logs().await
        } else {
            Vec::new()
        }
    }
    
    // Get storage audit logs for a specific tenant
    pub async fn get_tenant_storage_audit_logs(&self, tenant_id: &str) -> Vec<StorageAuditLogEntry> {
        if let Some(access_control) = &self.access_control {
            access_control.get_tenant_audit_logs(tenant_id).await
        } else {
            Vec::new()
        }
    }
    
    // Get storage audit logs for a specific volume
    pub async fn get_volume_audit_logs(&self, volume_name: &str) -> Vec<StorageAuditLogEntry> {
        if let Some(access_control) = &self.access_control {
            access_control.get_volume_audit_logs(volume_name).await
        } else {
            Vec::new()
        }
    }
    
    // Set rate limit for a tenant
    pub async fn set_tenant_storage_rate_limit(&self, tenant_id: &str, rate_limit: u32) -> Result<()> {
        if let Some(qos_manager) = &self.qos_manager {
            qos_manager.set_tenant_rate_limit(tenant_id, rate_limit).await
        } else {
            Err(anyhow::anyhow!("QoS manager not available"))
        }
    }
    
    // Get storage QoS stats for a tenant
    pub async fn get_tenant_storage_stats(&self, tenant_id: &str) -> Option<crate::security::TenantStorageStats> {
        if let Some(qos_manager) = &self.qos_manager {
            qos_manager.get_tenant_stats(tenant_id).await
        } else {
            None
        }
    }
}
