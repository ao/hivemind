use anyhow::{Context, Result};
use rand::Rng;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::RwLock;
use aes_gcm::{
    aead::{Aead, KeyInit},
    Aes256Gcm, Nonce,
};

/// Manages encryption keys for tenant storage
pub struct StorageEncryptionManager {
    // Map of tenant_id -> encryption key
    tenant_keys: Arc<RwLock<HashMap<String, TenantEncryptionKey>>>,
    // Default key for non-tenant-specific data
    default_key: [u8; 32],
}

/// Tenant-specific encryption key
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TenantEncryptionKey {
    pub tenant_id: String,
    pub key: [u8; 32],
    pub created_at: i64,
    pub rotation_count: u32,
}

impl StorageEncryptionManager {
    /// Create a new StorageEncryptionManager
    pub fn new() -> Self {
        let mut rng = rand::thread_rng();
        let mut default_key = [0u8; 32];
        rng.fill(&mut default_key);
        
        Self {
            tenant_keys: Arc::new(RwLock::new(HashMap::new())),
            default_key,
        }
    }
    
    /// Get or create an encryption key for a tenant
    pub async fn get_tenant_key(&self, tenant_id: &str) -> Result<TenantEncryptionKey> {
        let mut keys = self.tenant_keys.write().await;
        
        if !keys.contains_key(tenant_id) {
            // Create a new key for this tenant
            let mut key_data = [0u8; 32];
            rand::thread_rng().fill(&mut key_data);
            
            let key = TenantEncryptionKey {
                tenant_id: tenant_id.to_string(),
                key: key_data,
                created_at: chrono::Utc::now().timestamp(),
                rotation_count: 0,
            };
            
            keys.insert(tenant_id.to_string(), key.clone());
            return Ok(key);
        }
        
        Ok(keys.get(tenant_id).unwrap().clone())
    }
    
    /// Rotate a tenant's encryption key
    pub async fn rotate_tenant_key(&self, tenant_id: &str) -> Result<TenantEncryptionKey> {
        let mut keys = self.tenant_keys.write().await;
        
        let mut new_key = if let Some(old_key) = keys.get(tenant_id) {
            let mut key_data = [0u8; 32];
            rand::thread_rng().fill(&mut key_data);
            
            TenantEncryptionKey {
                tenant_id: tenant_id.to_string(),
                key: key_data,
                created_at: chrono::Utc::now().timestamp(),
                rotation_count: old_key.rotation_count + 1,
            }
        } else {
            // Create a new key if one doesn't exist
            let mut key_data = [0u8; 32];
            rand::thread_rng().fill(&mut key_data);
            
            TenantEncryptionKey {
                tenant_id: tenant_id.to_string(),
                key: key_data,
                created_at: chrono::Utc::now().timestamp(),
                rotation_count: 0,
            }
        };
        
        keys.insert(tenant_id.to_string(), new_key.clone());
        Ok(new_key)
    }
    
    /// Encrypt data using a tenant's key
    pub async fn encrypt(&self, tenant_id: Option<&str>, data: &[u8]) -> Result<Vec<u8>> {
        let key = match tenant_id {
            Some(id) => self.get_tenant_key(id).await?.key,
            None => self.default_key,
        };
        
        let cipher = Aes256Gcm::new_from_slice(&key)
            .context("Failed to create cipher from key")?;
        
        // Create a random nonce
        let mut nonce_bytes = [0u8; 12];
        rand::thread_rng().fill(&mut nonce_bytes);
        let nonce = Nonce::from_slice(&nonce_bytes);
        
        // Encrypt the data
        let ciphertext = cipher.encrypt(nonce, data)
            .map_err(|e| anyhow::anyhow!("Encryption failed: {}", e))?;
        
        // Combine nonce and ciphertext for storage
        let mut result = Vec::with_capacity(nonce_bytes.len() + ciphertext.len());
        result.extend_from_slice(&nonce_bytes);
        result.extend_from_slice(&ciphertext);
        
        Ok(result)
    }
    
    /// Decrypt data using a tenant's key
    pub async fn decrypt(&self, tenant_id: Option<&str>, encrypted_data: &[u8]) -> Result<Vec<u8>> {
        if encrypted_data.len() < 12 {
            return Err(anyhow::anyhow!("Encrypted data too short"));
        }
        
        let key = match tenant_id {
            Some(id) => self.get_tenant_key(id).await?.key,
            None => self.default_key,
        };
        
        let cipher = Aes256Gcm::new_from_slice(&key)
            .context("Failed to create cipher from key")?;
        
        // Split the data into nonce and ciphertext
        let nonce = Nonce::from_slice(&encrypted_data[..12]);
        let ciphertext = &encrypted_data[12..];
        
        // Decrypt the data
        let plaintext = cipher.decrypt(nonce, ciphertext)
            .map_err(|e| anyhow::anyhow!("Decryption failed: {}", e))?;
        
        Ok(plaintext)
    }
}

impl Default for StorageEncryptionManager {
    fn default() -> Self {
        Self::new()
    }
}