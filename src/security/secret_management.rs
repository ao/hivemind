use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::RwLock;
use std::time::{SystemTime, UNIX_EPOCH};
use uuid::Uuid;
use base64::{Engine as _, engine::general_purpose};
use rand::RngCore;

/// Secret represents a sensitive piece of information
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Secret {
    pub id: String,
    pub name: String,
    pub description: String,
    #[serde(skip_serializing)]
    pub data: Vec<u8>,
    pub version: u32,
    pub created_at: i64,
    pub updated_at: i64,
    pub created_by: String,
    pub last_accessed: Option<i64>,
    pub labels: HashMap<String, String>,
    pub metadata: HashMap<String, String>,
    pub expiration: Option<i64>,
    pub rotation_period: Option<i64>,
    pub last_rotated: Option<i64>,
}

/// Secret reference for containers
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SecretReference {
    pub secret_id: String,
    pub container_id: String,
    pub mount_path: String,
    pub env_var: Option<String>,
    pub file_name: Option<String>,
    pub file_mode: Option<u32>,
    pub created_at: i64,
}

/// Secret access log entry
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SecretAccessLog {
    pub id: String,
    pub secret_id: String,
    pub user_id: String,
    pub timestamp: i64,
    pub action: SecretAction,
    pub client_ip: Option<String>,
    pub user_agent: Option<String>,
}

/// Secret action types
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum SecretAction {
    Create,
    Read,
    Update,
    Delete,
    Rotate,
    Mount,
    Unmount,
}

/// Encryption key for secrets
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EncryptionKey {
    pub id: String,
    pub name: String,
    pub algorithm: EncryptionAlgorithm,
    #[serde(skip_serializing)]
    pub key_data: Vec<u8>,
    pub created_at: i64,
    pub expires_at: Option<i64>,
    pub is_primary: bool,
}

/// Encryption algorithms
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum EncryptionAlgorithm {
    AES256GCM,
    ChaCha20Poly1305,
    RSA2048,
    RSA4096,
}

/// Secret Manager handles secure storage and distribution of secrets
pub struct SecretManager {
    secrets: Arc<RwLock<HashMap<String, Secret>>>,
    secret_references: Arc<RwLock<HashMap<String, Vec<SecretReference>>>>,
    access_logs: Arc<RwLock<Vec<SecretAccessLog>>>,
    encryption_keys: Arc<RwLock<HashMap<String, EncryptionKey>>>,
    primary_key_id: Arc<RwLock<Option<String>>>,
}

impl SecretManager {
    pub fn new() -> Self {
        let secret_manager = Self {
            secrets: Arc::new(RwLock::new(HashMap::new())),
            secret_references: Arc::new(RwLock::new(HashMap::new())),
            access_logs: Arc::new(RwLock::new(Vec::new())),
            encryption_keys: Arc::new(RwLock::new(HashMap::new())),
            primary_key_id: Arc::new(RwLock::new(None)),
        };
        
        // Initialize encryption keys
        tokio::spawn(async move {
            if let Err(e) = secret_manager.initialize_encryption_keys().await {
                eprintln!("Failed to initialize encryption keys: {}", e);
            }
        });
        
        secret_manager
    }
    
    /// Initialize encryption keys
    async fn initialize_encryption_keys(&self) -> Result<()> {
        // Generate a new encryption key
        let key_id = Uuid::new_v4().to_string();
        let mut key_data = vec![0u8; 32]; // 256 bits
        rand::thread_rng().fill_bytes(&mut key_data);
        
        let key = EncryptionKey {
            id: key_id.clone(),
            name: "Primary Key".to_string(),
            algorithm: EncryptionAlgorithm::AES256GCM,
            key_data,
            created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            expires_at: None,
            is_primary: true,
        };
        
        // Store the key
        let mut keys = self.encryption_keys.write().await;
        keys.insert(key_id.clone(), key);
        
        // Set as primary key
        let mut primary_key = self.primary_key_id.write().await;
        *primary_key = Some(key_id);
        
        Ok(())
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
        // Generate a new ID
        let secret_id = Uuid::new_v4().to_string();
        let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;
        
        // Encrypt the secret data
        let encrypted_data = self.encrypt_data(data).await?;
        
        let secret = Secret {
            id: secret_id.clone(),
            name: name.to_string(),
            description: description.to_string(),
            data: encrypted_data,
            version: 1,
            created_at: now,
            updated_at: now,
            created_by: created_by.to_string(),
            last_accessed: None,
            labels,
            metadata,
            expiration,
            rotation_period,
            last_rotated: Some(now),
        };
        
        // Store the secret
        let mut secrets = self.secrets.write().await;
        secrets.insert(secret_id.clone(), secret.clone());
        
        // Log the action
        self.log_access(
            &secret_id,
            created_by,
            SecretAction::Create,
            None,
            None,
        ).await?;
        
        Ok(secret)
    }
    
    /// Get a secret by ID
    pub async fn get_secret(&self, secret_id: &str, user_id: &str) -> Result<Secret> {
        let mut secrets = self.secrets.write().await;
        
        let secret = secrets.get_mut(secret_id).cloned()
            .ok_or_else(|| anyhow::anyhow!("Secret not found"))?;
        
        // Update last accessed time
        let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;
        if let Some(s) = secrets.get_mut(secret_id) {
            s.last_accessed = Some(now);
        }
        
        // Log the access
        self.log_access(
            secret_id,
            user_id,
            SecretAction::Read,
            None,
            None,
        ).await?;
        
        Ok(secret)
    }
    
    /// Update a secret
    pub async fn update_secret(
        &self,
        secret_id: &str,
        description: Option<&str>,
        data: Option<&[u8]>,
        labels: Option<HashMap<String, String>>,
        metadata: Option<HashMap<String, String>>,
        expiration: Option<i64>,
        rotation_period: Option<i64>,
        user_id: &str,
    ) -> Result<Secret> {
        let mut secrets = self.secrets.write().await;
        
        let mut secret = secrets.get_mut(secret_id)
            .ok_or_else(|| anyhow::anyhow!("Secret not found"))?
            .clone();
        
        let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;
        
        // Update fields if provided
        if let Some(desc) = description {
            secret.description = desc.to_string();
        }
        
        if let Some(d) = data {
            secret.data = self.encrypt_data(d).await?;
            secret.version += 1;
            secret.last_rotated = Some(now);
        }
        
        if let Some(l) = labels {
            secret.labels = l;
        }
        
        if let Some(m) = metadata {
            secret.metadata = m;
        }
        
        secret.expiration = expiration;
        secret.rotation_period = rotation_period;
        secret.updated_at = now;
        
        // Store the updated secret
        secrets.insert(secret_id.to_string(), secret.clone());
        
        // Log the action
        self.log_access(
            secret_id,
            user_id,
            SecretAction::Update,
            None,
            None,
        ).await?;
        
        Ok(secret)
    }
    
    /// Delete a secret
    pub async fn delete_secret(&self, secret_id: &str, user_id: &str) -> Result<()> {
        let mut secrets = self.secrets.write().await;
        
        if !secrets.contains_key(secret_id) {
            anyhow::bail!("Secret not found");
        }
        
        // Remove the secret
        secrets.remove(secret_id);
        
        // Log the action
        self.log_access(
            secret_id,
            user_id,
            SecretAction::Delete,
            None,
            None,
        ).await?;
        
        Ok(())
    }
    
    /// List all secrets (without sensitive data)
    pub async fn list_secrets(&self) -> Vec<Secret> {
        let secrets = self.secrets.read().await;
        
        // Return secrets without the actual secret data
        secrets.values()
            .map(|s| {
                let mut secret = s.clone();
                secret.data = Vec::new(); // Clear sensitive data
                secret
            })
            .collect()
    }
    
    /// Rotate a secret
    pub async fn rotate_secret(&self, secret_id: &str, new_data: &[u8], user_id: &str) -> Result<Secret> {
        let mut secrets = self.secrets.write().await;
        
        let mut secret = secrets.get_mut(secret_id)
            .ok_or_else(|| anyhow::anyhow!("Secret not found"))?
            .clone();
        
        let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;
        
        // Update the secret data
        secret.data = self.encrypt_data(new_data).await?;
        secret.version += 1;
        secret.updated_at = now;
        secret.last_rotated = Some(now);
        
        // Store the updated secret
        secrets.insert(secret_id.to_string(), secret.clone());
        
        // Log the action
        self.log_access(
            secret_id,
            user_id,
            SecretAction::Rotate,
            None,
            None,
        ).await?;
        
        Ok(secret)
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
        // Check if the secret exists
        let secrets = self.secrets.read().await;
        if !secrets.contains_key(secret_id) {
            anyhow::bail!("Secret not found");
        }
        
        let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;
        
        // Create the secret reference
        let reference = SecretReference {
            secret_id: secret_id.to_string(),
            container_id: container_id.to_string(),
            mount_path: mount_path.to_string(),
            env_var: env_var.map(|s| s.to_string()),
            file_name: file_name.map(|s| s.to_string()),
            file_mode,
            created_at: now,
        };
        
        // Store the reference
        let mut references = self.secret_references.write().await;
        let container_refs = references.entry(container_id.to_string())
            .or_insert_with(Vec::new);
        container_refs.push(reference.clone());
        
        // Log the action
        self.log_access(
            secret_id,
            user_id,
            SecretAction::Mount,
            None,
            None,
        ).await?;
        
        Ok(reference)
    }
    
    /// Unmount a secret from a container
    pub async fn unmount_secret_from_container(
        &self,
        secret_id: &str,
        container_id: &str,
        user_id: &str,
    ) -> Result<()> {
        let mut references = self.secret_references.write().await;
        
        if let Some(container_refs) = references.get_mut(container_id) {
            // Remove the reference
            container_refs.retain(|r| r.secret_id != secret_id);
            
            // If no more references, remove the container entry
            if container_refs.is_empty() {
                references.remove(container_id);
            }
            
            // Log the action
            self.log_access(
                secret_id,
                user_id,
                SecretAction::Unmount,
                None,
                None,
            ).await?;
            
            Ok(())
        } else {
            anyhow::bail!("No secret references found for container");
        }
    }
    
    /// Get all secrets mounted to a container
    pub async fn get_container_secrets(&self, container_id: &str) -> Vec<SecretReference> {
        let references = self.secret_references.read().await;
        
        if let Some(container_refs) = references.get(container_id) {
            container_refs.clone()
        } else {
            Vec::new()
        }
    }
    
    /// Check for secrets that need rotation
    pub async fn check_rotation_needed(&self) -> Vec<Secret> {
        let secrets = self.secrets.read().await;
        let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64;
        
        let mut needs_rotation = Vec::new();
        
        for secret in secrets.values() {
            // Check if rotation period is set and exceeded
            if let Some(rotation_period) = secret.rotation_period {
                if let Some(last_rotated) = secret.last_rotated {
                    if now - last_rotated > rotation_period {
                        needs_rotation.push(secret.clone());
                    }
                }
            }
            
            // Check if secret is expired
            if let Some(expiration) = secret.expiration {
                if now > expiration {
                    needs_rotation.push(secret.clone());
                }
            }
        }
        
        needs_rotation
    }
    
    /// Encrypt data using the primary key
    async fn encrypt_data(&self, data: &[u8]) -> Result<Vec<u8>> {
        // In a real implementation, we would use proper encryption
        // For now, we'll just do a simple base64 encoding
        
        // Get the primary key
        let primary_key_id = self.primary_key_id.read().await;
        let key_id = primary_key_id.as_ref()
            .ok_or_else(|| anyhow::anyhow!("No primary encryption key found"))?;
        
        let keys = self.encryption_keys.read().await;
        let _key = keys.get(key_id)
            .ok_or_else(|| anyhow::anyhow!("Primary encryption key not found"))?;
        
        // In a real implementation, we would use the key to encrypt the data
        // For now, just base64 encode it
        let encoded = general_purpose::STANDARD.encode(data);
        
        Ok(encoded.into_bytes())
    }
    
    /// Decrypt data using the appropriate key
    async fn decrypt_data(&self, data: &[u8]) -> Result<Vec<u8>> {
        // In a real implementation, we would use proper decryption
        // For now, we'll just do a simple base64 decoding
        
        let data_str = String::from_utf8_lossy(data);
        let decoded = general_purpose::STANDARD.decode(data_str.as_bytes())?;
        
        Ok(decoded)
    }
    
    /// Log a secret access
    async fn log_access(
        &self,
        secret_id: &str,
        user_id: &str,
        action: SecretAction,
        client_ip: Option<&str>,
        user_agent: Option<&str>,
    ) -> Result<()> {
        let log_entry = SecretAccessLog {
            id: Uuid::new_v4().to_string(),
            secret_id: secret_id.to_string(),
            user_id: user_id.to_string(),
            timestamp: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            action,
            client_ip: client_ip.map(|s| s.to_string()),
            user_agent: user_agent.map(|s| s.to_string()),
        };
        
        let mut logs = self.access_logs.write().await;
        logs.push(log_entry);
        
        // In a real implementation, we would also write to a persistent store
        
        Ok(())
    }
    
    /// Get access logs for a secret
    pub async fn get_secret_access_logs(&self, secret_id: &str) -> Vec<SecretAccessLog> {
        let logs = self.access_logs.read().await;
        
        logs.iter()
            .filter(|log| log.secret_id == secret_id)
            .cloned()
            .collect()
    }
    
    /// Create a new encryption key
    pub async fn create_encryption_key(
        &self,
        name: &str,
        algorithm: EncryptionAlgorithm,
        expires_at: Option<i64>,
        make_primary: bool,
    ) -> Result<EncryptionKey> {
        // Generate a new key ID
        let key_id = Uuid::new_v4().to_string();
        
        // Generate key data based on algorithm
        let key_size = match algorithm {
            EncryptionAlgorithm::AES256GCM => 32, // 256 bits
            EncryptionAlgorithm::ChaCha20Poly1305 => 32, // 256 bits
            EncryptionAlgorithm::RSA2048 => 256, // 2048 bits
            EncryptionAlgorithm::RSA4096 => 512, // 4096 bits
        };
        
        let mut key_data = vec![0u8; key_size];
        rand::thread_rng().fill_bytes(&mut key_data);
        
        let key = EncryptionKey {
            id: key_id.clone(),
            name: name.to_string(),
            algorithm,
            key_data,
            created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            expires_at,
            is_primary: make_primary,
        };
        
        // Store the key
        let mut keys = self.encryption_keys.write().await;
        
        // If this is the new primary key, update all other keys
        if make_primary {
            for existing_key in keys.values_mut() {
                existing_key.is_primary = false;
            }
            
            // Update the primary key ID
            let mut primary_key = self.primary_key_id.write().await;
            *primary_key = Some(key_id.clone());
        }
        
        keys.insert(key_id.clone(), key.clone());
        
        Ok(key)
    }
    
    /// List all encryption keys (without sensitive data)
    pub async fn list_encryption_keys(&self) -> Vec<EncryptionKey> {
        let keys = self.encryption_keys.read().await;
        
        // Return keys without the actual key data
        keys.values()
            .map(|k| {
                let mut key = k.clone();
                key.key_data = Vec::new(); // Clear sensitive data
                key
            })
            .collect()
    }
    
    /// Rotate all secrets using a new encryption key
    pub async fn rotate_encryption_key(&self, user_id: &str) -> Result<()> {
        // Create a new encryption key
        let new_key = self.create_encryption_key(
            "Rotated Primary Key",
            EncryptionAlgorithm::AES256GCM,
            None,
            true,
        ).await?;
        
        // Re-encrypt all secrets with the new key
        let mut secrets = self.secrets.write().await;
        
        for (secret_id, secret) in secrets.iter_mut() {
            // Decrypt with old key
            let decrypted = self.decrypt_data(&secret.data).await?;
            
            // Encrypt with new key
            let encrypted = self.encrypt_data(&decrypted).await?;
            
            // Update the secret
            secret.data = encrypted;
            secret.version += 1;
            secret.updated_at = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;
            
            // Log the rotation
            self.log_access(
                secret_id,
                user_id,
                SecretAction::Rotate,
                None,
                None,
            ).await?;
        }
        
        println!("Rotated encryption key and re-encrypted {} secrets", secrets.len());
        
        Ok(())
    }
}