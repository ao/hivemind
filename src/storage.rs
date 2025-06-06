use anyhow::{Context, Result};
use async_trait::async_trait;
use rusqlite::{params, Connection};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::Path;
use std::sync::Arc;
use tokio::sync::Mutex;
use uuid::Uuid;

// Storage provider types
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum StorageProviderType {
    Local,
    NFS,
    Ceph,
    GlusterFS,
    MinIO,
    AwsEbs,
    AzureDisk,
    GcpPersistentDisk,
    Custom(String),
}

// Volume represents a storage volume in the system
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Volume {
    pub name: String,
    pub size: u64,
    pub path: String,
    pub provider_type: StorageProviderType,
    pub created_at: i64,
    pub tenant_id: Option<String>,
    pub labels: HashMap<String, String>,
}

// Volume status information
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct VolumeStatus {
    pub name: String,
    pub state: VolumeState,
    pub attached_to: Option<String>,
    pub used_space: u64,
    pub available_space: u64,
    pub health: VolumeHealth,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum VolumeState {
    Available,
    InUse,
    Creating,
    Deleting,
    Error,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum VolumeHealth {
    Healthy,
    Degraded,
    Failed,
}

// Snapshot represents a point-in-time copy of a volume
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Snapshot {
    pub name: String,
    pub volume_name: String,
    pub size: u64,
    pub created_at: i64,
    pub provider_type: StorageProviderType,
}

// Performance class for volumes
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum PerformanceClass {
    Standard,
    HighPerformance,
    ArchivalStorage,
}

// Options for volume creation
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct VolumeOptions {
    pub replicas: u32,
    pub encryption: bool,
    pub performance_class: PerformanceClass,
    pub tenant_id: Option<String>,
    pub labels: HashMap<String, String>,
}

impl Default for VolumeOptions {
    fn default() -> Self {
        Self {
            replicas: 1,
            encryption: false,
            performance_class: PerformanceClass::Standard,
            tenant_id: None,
            labels: HashMap::new(),
        }
    }
}

// Storage metrics for monitoring
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct StorageMetrics {
    pub total_volumes: usize,
    pub total_snapshots: usize,
    pub total_capacity: u64,
    pub used_capacity: u64,
    pub operations_count: HashMap<String, u64>,
    pub average_latency: HashMap<String, f64>,
}

// Storage provider interface
#[async_trait]
pub trait StorageProvider: Send + Sync {
    // Provider information
    fn name(&self) -> &str;
    fn provider_type(&self) -> StorageProviderType;
    
    // Volume operations
    async fn create_volume(&self, name: &str, size: u64, opts: VolumeOptions) -> Result<Volume>;
    async fn delete_volume(&self, name: &str) -> Result<()>;
    async fn expand_volume(&self, name: &str, new_size: u64) -> Result<()>;
    async fn attach_volume(&self, name: &str, node_id: &str) -> Result<String>;
    async fn detach_volume(&self, name: &str, node_id: &str) -> Result<()>;
    
    // Snapshot operations
    async fn create_snapshot(&self, volume_name: &str, snapshot_name: &str) -> Result<Snapshot>;
    async fn delete_snapshot(&self, snapshot_name: &str) -> Result<()>;
    async fn restore_snapshot(&self, snapshot_name: &str, volume_name: &str) -> Result<()>;
    
    // Clone operations
    async fn clone_volume(&self, source_name: &str, target_name: &str) -> Result<Volume>;
    
    // Status and metrics
    async fn get_volume_status(&self, name: &str) -> Result<VolumeStatus>;
    async fn get_provider_metrics(&self) -> Result<StorageMetrics>;
    
    // List operations
    async fn list_volumes(&self) -> Result<Vec<Volume>>;
    async fn list_snapshots(&self, volume_name: Option<&str>) -> Result<Vec<Snapshot>>;
}

// Local storage provider implementation
pub struct LocalStorageProvider {
    data_dir: std::path::PathBuf,
    conn: Arc<Mutex<Connection>>,
    metrics: Arc<Mutex<StorageMetrics>>,
}

impl LocalStorageProvider {
    pub async fn new(data_dir: &Path) -> Result<Self> {
        // Ensure data directory exists
        tokio::fs::create_dir_all(data_dir).await?;

        let db_path = data_dir.join("hivemind.db");

        // Open connection in a blocking task since SQLite operations are blocking
        let conn = tokio::task::spawn_blocking(move || -> Result<Connection> {
            let conn = Connection::open(db_path)?;

            // Create tables if they don't exist
            conn.execute(
                "CREATE TABLE IF NOT EXISTS volumes (
                    name TEXT PRIMARY KEY,
                    path TEXT NOT NULL,
                    size INTEGER NOT NULL,
                    created_at INTEGER NOT NULL,
                    provider_type TEXT NOT NULL,
                    tenant_id TEXT,
                    labels TEXT
                )",
                [],
            )?;
            
            conn.execute(
                "CREATE TABLE IF NOT EXISTS snapshots (
                    name TEXT PRIMARY KEY,
                    volume_name TEXT NOT NULL,
                    size INTEGER NOT NULL,
                    created_at INTEGER NOT NULL,
                    provider_type TEXT NOT NULL
                )",
                [],
            )?;

            Ok(conn)
        })
        .await??;

        Ok(Self {
            data_dir: data_dir.to_path_buf(),
            conn: Arc::new(Mutex::new(conn)),
            metrics: Arc::new(Mutex::new(StorageMetrics::default())),
        })
    }
}

#[async_trait]
impl StorageProvider for LocalStorageProvider {
    fn name(&self) -> &str {
        "local-storage"
    }
    
    fn provider_type(&self) -> StorageProviderType {
        StorageProviderType::Local
    }
    
    async fn create_volume(&self, name: &str, size: u64, opts: VolumeOptions) -> Result<Volume> {
        // Create volume directory
        let volume_path = self.data_dir.join("volumes").join(name);
        tokio::fs::create_dir_all(&volume_path).await?;
        
        let now = chrono::Utc::now().timestamp();
        
        // Serialize labels to JSON string
        let labels_json = serde_json::to_string(&opts.labels)?;
        
        // Store volume metadata in database
        let name_clone = name.to_string();
        let path_str = volume_path.to_string_lossy().to_string();
        let provider_type = serde_json::to_string(&self.provider_type())?;
        let tenant_id = opts.tenant_id.clone();
        let conn = self.conn.clone();
        
        tokio::task::spawn_blocking(move || -> Result<()> {
            let conn = conn.blocking_lock();
            
            conn.execute(
                "INSERT OR REPLACE INTO volumes (name, path, size, created_at, provider_type, tenant_id, labels)
                 VALUES (?, ?, ?, ?, ?, ?, ?)",
                params![name_clone, path_str, size as i64, now, provider_type, tenant_id, labels_json],
            )?;
            
            Ok(())
        })
        .await??;
        
        // Update metrics
        {
            let mut metrics = self.metrics.lock().await;
            metrics.total_volumes += 1;
            metrics.total_capacity += size;
            metrics.operations_count.entry("create_volume".to_string())
                .and_modify(|count| *count += 1)
                .or_insert(1);
        }
        
        // Return volume info
        Ok(Volume {
            name: name.to_string(),
            size,
            path: volume_path.to_string_lossy().to_string(),
            provider_type: self.provider_type(),
            created_at: now,
            tenant_id: opts.tenant_id,
            labels: opts.labels,
        })
    }
    
    async fn delete_volume(&self, name: &str) -> Result<()> {
        // Check if volume exists
        let volume = self.get_volume_internal(name).await?
            .context(format!("Volume {} not found", name))?;
        
        // Delete volume directory
        let volume_path = Path::new(&volume.path);
        tokio::fs::remove_dir_all(volume_path).await?;
        
        // Delete from database
        let name_clone = name.to_string();
        let conn = self.conn.clone();
        
        tokio::task::spawn_blocking(move || -> Result<()> {
            let conn = conn.blocking_lock();
            conn.execute("DELETE FROM volumes WHERE name = ?", params![name_clone])?;
            Ok(())
        })
        .await??;
        
        // Update metrics
        {
            let mut metrics = self.metrics.lock().await;
            metrics.total_volumes -= 1;
            metrics.total_capacity -= volume.size;
            metrics.operations_count.entry("delete_volume".to_string())
                .and_modify(|count| *count += 1)
                .or_insert(1);
        }
        
        Ok(())
    }
    
    async fn expand_volume(&self, name: &str, new_size: u64) -> Result<()> {
        // Check if volume exists
        let volume = self.get_volume_internal(name).await?
            .context(format!("Volume {} not found", name))?;
        
        if new_size <= volume.size {
            return Err(anyhow::anyhow!("New size must be larger than current size"));
        }
        
        // Update size in database
        let name_clone = name.to_string();
        let conn = self.conn.clone();
        
        tokio::task::spawn_blocking(move || -> Result<()> {
            let conn = conn.blocking_lock();
            conn.execute(
                "UPDATE volumes SET size = ? WHERE name = ?",
                params![new_size as i64, name_clone],
            )?;
            Ok(())
        })
        .await??;
        
        // Update metrics
        {
            let mut metrics = self.metrics.lock().await;
            metrics.total_capacity = metrics.total_capacity - volume.size + new_size;
            metrics.operations_count.entry("expand_volume".to_string())
                .and_modify(|count| *count += 1)
                .or_insert(1);
        }
        
        Ok(())
    }
    
    async fn attach_volume(&self, name: &str, _node_id: &str) -> Result<String> {
        // For local storage, just return the path
        let volume = self.get_volume_internal(name).await?
            .context(format!("Volume {} not found", name))?;
        
        // Update metrics
        {
            let mut metrics = self.metrics.lock().await;
            metrics.operations_count.entry("attach_volume".to_string())
                .and_modify(|count| *count += 1)
                .or_insert(1);
        }
        
        Ok(volume.path)
    }
    
    async fn detach_volume(&self, name: &str, _node_id: &str) -> Result<()> {
        // Check if volume exists
        let _ = self.get_volume_internal(name).await?
            .context(format!("Volume {} not found", name))?;
        
        // For local storage, detaching is a no-op
        
        // Update metrics
        {
            let mut metrics = self.metrics.lock().await;
            metrics.operations_count.entry("detach_volume".to_string())
                .and_modify(|count| *count += 1)
                .or_insert(1);
        }
        
        Ok(())
    }
    
    async fn create_snapshot(&self, volume_name: &str, snapshot_name: &str) -> Result<Snapshot> {
        // Check if volume exists
        let volume = self.get_volume_internal(volume_name).await?
            .context(format!("Volume {} not found", volume_name))?;
        
        // Create snapshot directory
        let snapshots_dir = self.data_dir.join("snapshots");
        tokio::fs::create_dir_all(&snapshots_dir).await?;
        
        let snapshot_path = snapshots_dir.join(snapshot_name);
        
        // Copy volume data to snapshot (in a real implementation, this would use CoW)
        let volume_path = Path::new(&volume.path);
        tokio::process::Command::new("cp")
            .arg("-r")
            .arg(volume_path)
            .arg(&snapshot_path)
            .output()
            .await?;
        
        let now = chrono::Utc::now().timestamp();
        
        // Store snapshot metadata in database
        let snapshot_name_clone = snapshot_name.to_string();
        let volume_name_clone = volume_name.to_string();
        let provider_type = serde_json::to_string(&self.provider_type())?;
        let conn = self.conn.clone();
        
        tokio::task::spawn_blocking(move || -> Result<()> {
            let conn = conn.blocking_lock();
            
            conn.execute(
                "INSERT OR REPLACE INTO snapshots (name, volume_name, size, created_at, provider_type)
                 VALUES (?, ?, ?, ?, ?)",
                params![snapshot_name_clone, volume_name_clone, volume.size as i64, now, provider_type],
            )?;
            
            Ok(())
        })
        .await??;
        
        // Update metrics
        {
            let mut metrics = self.metrics.lock().await;
            metrics.total_snapshots += 1;
            metrics.operations_count.entry("create_snapshot".to_string())
                .and_modify(|count| *count += 1)
                .or_insert(1);
        }
        
        // Return snapshot info
        Ok(Snapshot {
            name: snapshot_name.to_string(),
            volume_name: volume_name.to_string(),
            size: volume.size,
            created_at: now,
            provider_type: self.provider_type(),
        })
    }
    
    async fn delete_snapshot(&self, snapshot_name: &str) -> Result<()> {
        // Get snapshot info
        let snapshot = self.get_snapshot_internal(snapshot_name).await?
            .context(format!("Snapshot {} not found", snapshot_name))?;
        
        // Delete snapshot directory
        let snapshot_path = self.data_dir.join("snapshots").join(snapshot_name);
        tokio::fs::remove_dir_all(snapshot_path).await?;
        
        // Delete from database
        let snapshot_name_clone = snapshot_name.to_string();
        let conn = self.conn.clone();
        
        tokio::task::spawn_blocking(move || -> Result<()> {
            let conn = conn.blocking_lock();
            conn.execute("DELETE FROM snapshots WHERE name = ?", params![snapshot_name_clone])?;
            Ok(())
        })
        .await??;
        
        // Update metrics
        {
            let mut metrics = self.metrics.lock().await;
            metrics.total_snapshots -= 1;
            metrics.operations_count.entry("delete_snapshot".to_string())
                .and_modify(|count| *count += 1)
                .or_insert(1);
        }
        
        Ok(())
    }
    
    async fn restore_snapshot(&self, snapshot_name: &str, volume_name: &str) -> Result<()> {
        // Get snapshot info
        let snapshot = self.get_snapshot_internal(snapshot_name).await?
            .context(format!("Snapshot {} not found", snapshot_name))?;
        
        // Check if volume exists
        let volume = self.get_volume_internal(volume_name).await?
            .context(format!("Volume {} not found", volume_name))?;
        
        // Copy snapshot data to volume
        let snapshot_path = self.data_dir.join("snapshots").join(snapshot_name);
        let volume_path = Path::new(&volume.path);
        
        // Remove existing volume data
        tokio::fs::remove_dir_all(volume_path).await?;
        tokio::fs::create_dir_all(volume_path).await?;
        
        // Copy snapshot data to volume
        tokio::process::Command::new("cp")
            .arg("-r")
            .arg(snapshot_path.join("*"))
            .arg(volume_path)
            .output()
            .await?;
        
        // Update metrics
        {
            let mut metrics = self.metrics.lock().await;
            metrics.operations_count.entry("restore_snapshot".to_string())
                .and_modify(|count| *count += 1)
                .or_insert(1);
        }
        
        Ok(())
    }
    
    async fn clone_volume(&self, source_name: &str, target_name: &str) -> Result<Volume> {
        // Check if source volume exists
        let source_volume = self.get_volume_internal(source_name).await?
            .context(format!("Volume {} not found", source_name))?;
        
        // Create target volume
        let opts = VolumeOptions {
            tenant_id: source_volume.tenant_id.clone(),
            labels: source_volume.labels.clone(),
            ..Default::default()
        };
        
        let target_volume = self.create_volume(target_name, source_volume.size, opts).await?;
        
        // Copy data from source to target
        let source_path = Path::new(&source_volume.path);
        let target_path = Path::new(&target_volume.path);
        
        tokio::process::Command::new("cp")
            .arg("-r")
            .arg(source_path.join("*"))
            .arg(target_path)
            .output()
            .await?;
        
        // Update metrics
        {
            let mut metrics = self.metrics.lock().await;
            metrics.operations_count.entry("clone_volume".to_string())
                .and_modify(|count| *count += 1)
                .or_insert(1);
        }
        
        Ok(target_volume)
    }
    
    async fn get_volume_status(&self, name: &str) -> Result<VolumeStatus> {
        // Check if volume exists
        let volume = self.get_volume_internal(name).await?
            .context(format!("Volume {} not found", name))?;
        
        // Get volume usage statistics
        let volume_path = Path::new(&volume.path);
        
        // In a real implementation, we would get actual disk usage
        // For now, simulate with random values
        let total_space = volume.size;
        let used_space = total_space / 3; // Simulate 1/3 usage
        
        // Check if volume is in use
        let is_in_use = self.is_volume_in_use(name).await?;
        
        Ok(VolumeStatus {
            name: name.to_string(),
            state: if is_in_use { VolumeState::InUse } else { VolumeState::Available },
            attached_to: if is_in_use { Some("local-node".to_string()) } else { None },
            used_space,
            available_space: total_space - used_space,
            health: VolumeHealth::Healthy,
        })
    }
    
    async fn get_provider_metrics(&self) -> Result<StorageMetrics> {
        let metrics = self.metrics.lock().await.clone();
        Ok(metrics)
    }
    
    async fn list_volumes(&self) -> Result<Vec<Volume>> {
        let conn = self.conn.clone();
        
        let volumes = tokio::task::spawn_blocking(move || -> Result<Vec<Volume>> {
            let conn = conn.blocking_lock();
            let mut stmt = conn.prepare(
                "SELECT name, path, size, created_at, provider_type, tenant_id, labels FROM volumes"
            )?;
            
            let volume_iter = stmt.query_map([], |row| {
                let provider_type_str: String = row.get(4)?;
                let provider_type: StorageProviderType = serde_json::from_str(&provider_type_str)
                    .unwrap_or(StorageProviderType::Local);
                
                let labels_str: Option<String> = row.get(6)?;
                let labels: HashMap<String, String> = if let Some(s) = labels_str {
                    serde_json::from_str(&s).unwrap_or_default()
                } else {
                    HashMap::new()
                };
                
                Ok(Volume {
                    name: row.get(0)?,
                    path: row.get(1)?,
                    size: row.get::<_, i64>(2)? as u64,
                    created_at: row.get(3)?,
                    provider_type,
                    tenant_id: row.get(5)?,
                    labels,
                })
            })?;
            
            let mut volumes = Vec::new();
            for volume in volume_iter {
                volumes.push(volume?);
            }
            
            Ok(volumes)
        })
        .await??;
        
        Ok(volumes)
    }
    
    async fn list_snapshots(&self, volume_name: Option<&str>) -> Result<Vec<Snapshot>> {
        let conn = self.conn.clone();
        let volume_name = volume_name.map(|s| s.to_string());
        
        let snapshots = tokio::task::spawn_blocking(move || -> Result<Vec<Snapshot>> {
            let conn = conn.blocking_lock();
            
            let mut snapshots = Vec::new();
            
            if let Some(vol_name) = volume_name {
                let mut stmt = conn.prepare(
                    "SELECT name, volume_name, size, created_at, provider_type FROM snapshots WHERE volume_name = ?"
                )?;
                
                let snapshot_iter = stmt.query_map(params![vol_name], |row| {
                    let provider_type_str: String = row.get(4)?;
                    let provider_type: StorageProviderType = serde_json::from_str(&provider_type_str)
                        .unwrap_or(StorageProviderType::Local);
                    
                    Ok(Snapshot {
                        name: row.get(0)?,
                        volume_name: row.get(1)?,
                        size: row.get::<_, i64>(2)? as u64,
                        created_at: row.get(3)?,
                        provider_type,
                    })
                })?;
                
                for snapshot in snapshot_iter {
                    snapshots.push(snapshot?);
                }
            } else {
                let mut stmt = conn.prepare(
                    "SELECT name, volume_name, size, created_at, provider_type FROM snapshots"
                )?;
                
                let snapshot_iter = stmt.query_map([], |row| {
                    let provider_type_str: String = row.get(4)?;
                    let provider_type: StorageProviderType = serde_json::from_str(&provider_type_str)
                        .unwrap_or(StorageProviderType::Local);
                    
                    Ok(Snapshot {
                        name: row.get(0)?,
                        volume_name: row.get(1)?,
                        size: row.get::<_, i64>(2)? as u64,
                        created_at: row.get(3)?,
                        provider_type,
                    })
                })?;
                
                for snapshot in snapshot_iter {
                    snapshots.push(snapshot?);
                }
            }
            
            Ok(snapshots)
        })
        .await??;
        
        Ok(snapshots)
    }
}

// Helper methods for LocalStorageProvider
impl LocalStorageProvider {
    async fn get_volume_internal(&self, name: &str) -> Result<Option<Volume>> {
        let name = name.to_string();
        let conn = self.conn.clone();
        
        let result = tokio::task::spawn_blocking(move || -> Result<Option<Volume>> {
            let conn = conn.blocking_lock();
            let mut stmt = conn.prepare(
                "SELECT name, path, size, created_at, provider_type, tenant_id, labels FROM volumes WHERE name = ?"
            )?;
            
            let mut volume_iter = stmt.query_map(params![name], |row| {
                let provider_type_str: String = row.get(4)?;
                let provider_type: StorageProviderType = serde_json::from_str(&provider_type_str)
                    .unwrap_or(StorageProviderType::Local);
                
                let labels_str: Option<String> = row.get(6)?;
                let labels: HashMap<String, String> = if let Some(s) = labels_str {
                    serde_json::from_str(&s).unwrap_or_default()
                } else {
                    HashMap::new()
                };
                
                Ok(Volume {
                    name: row.get(0)?,
                    path: row.get(1)?,
                    size: row.get::<_, i64>(2)? as u64,
                    created_at: row.get(3)?,
                    provider_type,
                    tenant_id: row.get(5)?,
                    labels,
                })
            })?;
            
            if let Some(volume) = volume_iter.next() {
                Ok(Some(volume?))
            } else {
                Ok(None)
            }
        })
        .await??;
        
        Ok(result)
    }
    
    async fn get_snapshot_internal(&self, name: &str) -> Result<Option<Snapshot>> {
        let name = name.to_string();
        let conn = self.conn.clone();
        
        let result = tokio::task::spawn_blocking(move || -> Result<Option<Snapshot>> {
            let conn = conn.blocking_lock();
            let mut stmt = conn.prepare(
                "SELECT name, volume_name, size, created_at, provider_type FROM snapshots WHERE name = ?"
            )?;
            
            let mut snapshot_iter = stmt.query_map(params![name], |row| {
                let provider_type_str: String = row.get(4)?;
                let provider_type: StorageProviderType = serde_json::from_str(&provider_type_str)
                    .unwrap_or(StorageProviderType::Local);
                
                Ok(Snapshot {
                    name: row.get(0)?,
                    volume_name: row.get(1)?,
                    size: row.get::<_, i64>(2)? as u64,
                    created_at: row.get(3)?,
                    provider_type,
                })
            })?;
            
            if let Some(snapshot) = snapshot_iter.next() {
                Ok(Some(snapshot?))
            } else {
                Ok(None)
            }
        })
        .await??;
        
        Ok(result)
    }
    
    // Check if a volume is in use by any container
    async fn is_volume_in_use(&self, _volume_name: &str) -> Result<bool> {
        // This is a placeholder - in a real implementation, we would check if any container
        // is using this volume by querying a container_volumes table or similar
        // For now, we'll return false to indicate the volume is not in use
        Ok(false)
    }
}

// Enhanced StorageManager that coordinates multiple storage providers
#[derive(Clone)]
pub struct StorageManager {
    providers: Arc<Mutex<HashMap<String, Arc<dyn StorageProvider>>>>,
    default_provider: String,
    conn: Arc<Mutex<Connection>>,
}

impl StorageManager {
    pub async fn new(data_dir: &Path) -> Result<Self> {
        // Ensure data directory exists
        tokio::fs::create_dir_all(data_dir).await?;

        let db_path = data_dir.join("hivemind.db");

        // Open connection in a blocking task since SQLite operations are blocking
        let conn = tokio::task::spawn_blocking(move || -> Result<Connection> {
            let conn = Connection::open(db_path)?;

            // Create tables if they don't exist
            conn.execute(
                "CREATE TABLE IF NOT EXISTS nodes (
                    id TEXT PRIMARY KEY,
                    address TEXT NOT NULL,
                    last_seen INTEGER NOT NULL
                )",
                [],
            )?;

            conn.execute(
                "CREATE TABLE IF NOT EXISTS containers (
                    id TEXT PRIMARY KEY,
                    name TEXT NOT NULL,
                    image TEXT NOT NULL,
                    status TEXT NOT NULL,
                    node_id TEXT NOT NULL
                )",
                [],
            )?;

            conn.execute(
                "CREATE TABLE IF NOT EXISTS volumes (
                    name TEXT PRIMARY KEY,
                    path TEXT NOT NULL,
                    size INTEGER NOT NULL,
                    created_at INTEGER NOT NULL
                )",
                [],
            )?;

            conn.execute(
                "CREATE TABLE IF NOT EXISTS key_value (
                    key TEXT PRIMARY KEY,
                    value BLOB NOT NULL
                )",
                [],
            )?;

            Ok(conn)
        })
        .await??;

        // Create local storage provider
        let local_provider = LocalStorageProvider::new(data_dir).await?;
        
        let mut providers = HashMap::new();
        providers.insert("local".to_string(), Arc::new(local_provider) as Arc<dyn StorageProvider>);

        Ok(Self {
            providers: Arc::new(Mutex::new(providers)),
            default_provider: "local".to_string(),
            conn: Arc::new(Mutex::new(conn)),
        })
    }

    // Register a new storage provider
    pub async fn register_provider(&self, provider: Arc<dyn StorageProvider>) -> Result<()> {
        let provider_name = provider.name().to_string();
        let mut providers = self.providers.lock().await;
        providers.insert(provider_name, provider);
        Ok(())
    }

    // Set the default provider
    pub async fn set_default_provider(&self, provider_name: &str) -> Result<()> {
        let providers = self.providers.lock().await;
        if !providers.contains_key(provider_name) {
            return Err(anyhow::anyhow!("Provider {} not found", provider_name));
        }
        
        // Release the lock before modifying default_provider
        drop(providers);
        
        // This is safe because we're only modifying the string, not the Arc<Mutex<>>
        let this = self.clone();
        let provider_name = provider_name.to_string();
        tokio::task::spawn_blocking(move || {
            // SAFETY: This is safe because we're only modifying the string, not the Arc<Mutex<>>
            let this_mut = unsafe { &mut *((&this.default_provider) as *const String as *mut String) };
            *this_mut = provider_name;
        })
        .await?;
        
        Ok(())
    }

    // Get a provider by name
    pub async fn get_provider(&self, provider_name: &str) -> Result<Arc<dyn StorageProvider>> {
        let providers = self.providers.lock().await;
        providers
            .get(provider_name)
            .cloned()
            .ok_or_else(|| anyhow::anyhow!("Provider {} not found", provider_name))
    }

    // Get the default provider
    pub async fn get_default_provider(&self) -> Result<Arc<dyn StorageProvider>> {
        self.get_provider(&self.default_provider).await
    }

    // List all registered providers
    pub async fn list_providers(&self) -> Result<Vec<(String, StorageProviderType)>> {
        let providers = self.providers.lock().await;
        let mut result = Vec::new();
        
        for (name, provider) in providers.iter() {
            result.push((name.clone(), provider.provider_type()));
        }
        
        Ok(result)
    }

    // Create a volume using the specified provider or default provider
    pub async fn create_volume(
        &self,
        name: &str,
        size: u64,
        opts: Option<VolumeOptions>,
        provider_name: Option<&str>,
    ) -> Result<Volume> {
        let provider = match provider_name {
            Some(name) => self.get_provider(name).await?,
            None => self.get_default_provider().await?,
        };
        
        let options = opts.unwrap_or_default();
        provider.create_volume(name, size, options).await
    }

    // Delete a volume
    pub async fn delete_volume_enhanced(&self, name: &str) -> Result<()> {
        // First, find which provider owns this volume
        let volume = self.get_volume_enhanced(name).await?;
        let provider_type = volume.provider_type;
        
        // Find a provider of this type
        let providers = self.providers.lock().await;
        for provider in providers.values() {
            if provider.provider_type() == provider_type {
                return provider.delete_volume(name).await;
            }
        }
        
        Err(anyhow::anyhow!("No provider found for volume {}", name))
    }

    // Get a volume by name
    pub async fn get_volume_enhanced(&self, name: &str) -> Result<Volume> {
        // Try each provider until we find the volume
        let providers = self.providers.lock().await;
        
        for provider in providers.values() {
            let volumes = provider.list_volumes().await?;
            for volume in volumes {
                if volume.name == name {
                    return Ok(volume);
                }
            }
        }
        
        Err(anyhow::anyhow!("Volume {} not found", name))
    }

    // List all volumes across all providers
    pub async fn list_volumes_enhanced(&self) -> Result<Vec<Volume>> {
        let providers = self.providers.lock().await;
        let mut all_volumes = Vec::new();
        
        for provider in providers.values() {
            let volumes = provider.list_volumes().await?;
            all_volumes.extend(volumes);
        }
        
        Ok(all_volumes)
    }

    // Create a snapshot
    pub async fn create_snapshot(&self, volume_name: &str, snapshot_name: &str) -> Result<Snapshot> {
        // Find which provider owns this volume
        let volume = self.get_volume_enhanced(volume_name).await?;
        let provider_type = volume.provider_type;
        
        // Find a provider of this type
        let providers = self.providers.lock().await;
        for provider in providers.values() {
            if provider.provider_type() == provider_type {
                return provider.create_snapshot(volume_name, snapshot_name).await;
            }
        }
        
        Err(anyhow::anyhow!("No provider found for volume {}", volume_name))
    }

    // Delete a snapshot
    pub async fn delete_snapshot(&self, snapshot_name: &str) -> Result<()> {
        // Try each provider until we find the snapshot
        let providers = self.providers.lock().await;
        
        for provider in providers.values() {
            let snapshots = provider.list_snapshots(None).await?;
            for snapshot in snapshots {
                if snapshot.name == snapshot_name {
                    return provider.delete_snapshot(snapshot_name).await;
                }
            }
        }
        
        Err(anyhow::anyhow!("Snapshot {} not found", snapshot_name))
    }

    // List all snapshots
    pub async fn list_snapshots(&self, volume_name: Option<&str>) -> Result<Vec<Snapshot>> {
        let providers = self.providers.lock().await;
        let mut all_snapshots = Vec::new();
        
        for provider in providers.values() {
            let snapshots = provider.list_snapshots(volume_name).await?;
            all_snapshots.extend(snapshots);
        }
        
        Ok(all_snapshots)
    }

    // Get volume status
    pub async fn get_volume_status(&self, name: &str) -> Result<VolumeStatus> {
        // Find which provider owns this volume
        let volume = self.get_volume_enhanced(name).await?;
        let provider_type = volume.provider_type;
        
        // Find a provider of this type
        let providers = self.providers.lock().await;
        for provider in providers.values() {
            if provider.provider_type() == provider_type {
                return provider.get_volume_status(name).await;
            }
        }
        
        Err(anyhow::anyhow!("No provider found for volume {}", name))
    }

    // Get metrics for all providers
    pub async fn get_storage_metrics(&self) -> Result<HashMap<String, StorageMetrics>> {
        let providers = self.providers.lock().await;
        let mut all_metrics = HashMap::new();
        
        for (name, provider) in providers.iter() {
            let metrics = provider.get_provider_metrics().await?;
            all_metrics.insert(name.clone(), metrics);
        }
        
        Ok(all_metrics)
    }

    pub async fn save_container(
        &self,
        id: &str,
        name: &str,
        image: &str,
        status: &str,
        node_id: &str,
    ) -> Result<()> {
        let id = id.to_string();
        let name = name.to_string();
        let image = image.to_string();
        let status = status.to_string();
        let node_id = node_id.to_string();

        let conn = self.conn.clone();

        tokio::task::spawn_blocking(move || -> Result<()> {
            let conn = conn.blocking_lock();

            conn.execute(
                "INSERT OR REPLACE INTO containers (id, name, image, status, node_id) VALUES (?, ?, ?, ?, ?)",
                params![id, name, image, status, node_id],
            )?;

            Ok(())
        })
        .await??;

        Ok(())
    }

    pub async fn get_containers(&self) -> Result<Vec<(String, String, String, String, String)>> {
        let conn = self.conn.clone();

        let results = tokio::task::spawn_blocking(
            move || -> Result<Vec<(String, String, String, String, String)>> {
                let conn = conn.blocking_lock();
                let mut stmt =
                    conn.prepare("SELECT id, name, image, status, node_id FROM containers")?;
                let container_iter = stmt.query_map([], |row| {
                    Ok((
                        row.get(0)?,
                        row.get(1)?,
                        row.get(2)?,
                        row.get(3)?,
                        row.get(4)?,
                    ))
                })?;

                let mut containers = Vec::new();
                for container in container_iter {
                    containers.push(container?);
                }
                Ok(containers)
            },
        )
        .await??;

        Ok(results)
    }

    // Add methods to retrieve stored data
    pub async fn get_nodes(&self) -> Result<Vec<(String, String, i64)>> {
        let conn = self.conn.clone();

        let results = tokio::task::spawn_blocking(move || -> Result<Vec<(String, String, i64)>> {
            let conn = conn.blocking_lock();
            let mut stmt = conn.prepare("SELECT id, address, last_seen FROM nodes")?;
            let node_iter =
                stmt.query_map([], |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)))?;

            let mut nodes = Vec::new();
            for node in node_iter {
                nodes.push(node?);
            }
            Ok(nodes)
        })
        .await??;

        Ok(results)
    }

    pub async fn save_node(&self, id: &str, address: &str, last_seen: i64) -> Result<()> {
        let id = id.to_string();
        let address = address.to_string();
        let conn = self.conn.clone();

        tokio::task::spawn_blocking(move || -> Result<()> {
            let conn = conn.blocking_lock();

            conn.execute(
                "INSERT OR REPLACE INTO nodes (id, address, last_seen) VALUES (?, ?, ?)",
                params![id, address, last_seen],
            )?;

            Ok(())
        })
        .await??;

        Ok(())
    }

    // Generic key-value storage methods for testing
    pub async fn store(&self, key: &str, data: &[u8]) -> Result<()> {
        let key = key.to_string();
        let data = data.to_vec();
        let conn = self.conn.clone();

        tokio::task::spawn_blocking(move || -> Result<()> {
            let conn = conn.blocking_lock();

            // Create key-value table if it doesn't exist
            conn.execute(
                "CREATE TABLE IF NOT EXISTS key_value (
                    key TEXT PRIMARY KEY,
                    value BLOB NOT NULL
                )",
                [],
            )?;

            conn.execute(
                "INSERT OR REPLACE INTO key_value (key, value) VALUES (?, ?)",
                params![key, data],
            )?;

            Ok(())
        })
        .await??;

        Ok(())
    }

    pub async fn get(&self, key: &str) -> Result<Option<Vec<u8>>> {
        let key = key.to_string();
        let conn = self.conn.clone();

        let result = tokio::task::spawn_blocking(move || -> Result<Option<Vec<u8>>> {
            let conn = conn.blocking_lock();

            // Create key-value table if it doesn't exist
            conn.execute(
                "CREATE TABLE IF NOT EXISTS key_value (
                    key TEXT PRIMARY KEY,
                    value BLOB NOT NULL
                )",
                [],
            )?;

            let mut stmt = conn.prepare("SELECT value FROM key_value WHERE key = ?")?;
            let mut rows = stmt.query_map(params![key], |row| {
                let data: Vec<u8> = row.get(0)?;
                Ok(data)
            })?;

            if let Some(row) = rows.next() {
                Ok(Some(row?))
            } else {
                Ok(None)
            }
        })
        .await??;

        Ok(result)
    }

    // Save volume information to the database
    pub async fn save_volume(
        &self,
        name: &str,
        path: &str,
        size: u64,
        created_at: i64,
    ) -> Result<()> {
        // Create a volume using the default provider
        let options = VolumeOptions {
            ..Default::default()
        };
        
        // Check if volume already exists
        let volume_exists = match self.get_volume_enhanced(name).await {
            Ok(_) => true,
            Err(_) => false,
        };
        
        if !volume_exists {
            let provider = self.get_default_provider().await?;
            provider.create_volume(name, size, options).await?;
        }
        
        Ok(())
    }

    // Get all volumes from all providers
    pub async fn get_volumes(&self) -> Result<Vec<(String, String, u64, i64)>> {
        // Convert the new Volume struct to the old tuple format
        let volumes = self.list_volumes_enhanced().await?;
        let mut result = Vec::new();
        
        for volume in volumes {
            result.push((
                volume.name,
                volume.path,
                volume.size,
                volume.created_at,
            ));
        }
        
        Ok(result)
    }

    // Get a specific volume by name
    pub async fn get_volume(&self, name: &str) -> Result<Option<(String, String, u64, i64)>> {
        let result = self.get_volume_enhanced(name).await;
        match result {
            Ok(volume) => Ok(Some((
                volume.name,
                volume.path,
                volume.size,
                volume.created_at,
            ))),
            Err(_) => Ok(None),
        }
    }

    // Delete a volume
    pub async fn delete_volume(&self, name: &str) -> Result<bool> {
        match self.delete_volume_enhanced(name).await {
            Ok(_) => Ok(true),
            Err(_) => Ok(false),
        }
    }

    // Check if a volume is in use by any container
    pub async fn is_volume_in_use(&self, volume_name: &str) -> Result<bool> {
        // Try to get the volume status
        match self.get_volume_status(volume_name).await {
            Ok(status) => Ok(status.state == VolumeState::InUse),
            Err(_) => Ok(false),
        }
    }
}
