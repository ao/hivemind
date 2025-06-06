use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::sync::{Mutex, RwLock};
use tokio::time::sleep;

/// Storage QoS manager for tenant storage operations
pub struct StorageQoSManager {
    // Rate limits for tenants (operations per second)
    tenant_rate_limits: Arc<RwLock<HashMap<String, u32>>>,
    // Operation counters for tenants
    tenant_counters: Arc<Mutex<HashMap<String, OperationCounter>>>,
    // Default rate limit for tenants without specific limits
    default_rate_limit: u32,
    // Priority levels for different operation types
    operation_priorities: HashMap<StorageOperationType, u8>,
}

/// Storage operation types for QoS
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub enum StorageOperationType {
    Read,
    Write,
    Delete,
    List,
    Create,
    Snapshot,
    Restore,
}

/// Operation counter for rate limiting
#[derive(Debug, Clone)]
struct OperationCounter {
    operations: u32,
    window_start: Instant,
    last_operation: Instant,
}

/// QoS policy for a tenant
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TenantQoSPolicy {
    pub tenant_id: String,
    pub rate_limit: u32,  // Operations per second
    pub burst_limit: u32, // Maximum burst operations
    pub priority: u8,     // Priority level (1-10, 10 being highest)
}

impl StorageQoSManager {
    /// Create a new StorageQoSManager
    pub fn new(default_rate_limit: u32) -> Self {
        let mut operation_priorities = HashMap::new();
        operation_priorities.insert(StorageOperationType::Read, 8);
        operation_priorities.insert(StorageOperationType::List, 7);
        operation_priorities.insert(StorageOperationType::Write, 5);
        operation_priorities.insert(StorageOperationType::Create, 4);
        operation_priorities.insert(StorageOperationType::Delete, 3);
        operation_priorities.insert(StorageOperationType::Snapshot, 2);
        operation_priorities.insert(StorageOperationType::Restore, 1);
        
        Self {
            tenant_rate_limits: Arc::new(RwLock::new(HashMap::new())),
            tenant_counters: Arc::new(Mutex::new(HashMap::new())),
            default_rate_limit,
            operation_priorities,
        }
    }
    
    /// Set rate limit for a tenant
    pub async fn set_tenant_rate_limit(&self, tenant_id: &str, rate_limit: u32) -> Result<()> {
        let mut limits = self.tenant_rate_limits.write().await;
        limits.insert(tenant_id.to_string(), rate_limit);
        Ok(())
    }
    
    /// Get rate limit for a tenant
    pub async fn get_tenant_rate_limit(&self, tenant_id: &str) -> u32 {
        let limits = self.tenant_rate_limits.read().await;
        *limits.get(tenant_id).unwrap_or(&self.default_rate_limit)
    }
    
    /// Remove rate limit for a tenant
    pub async fn remove_tenant_rate_limit(&self, tenant_id: &str) -> Result<()> {
        let mut limits = self.tenant_rate_limits.write().await;
        limits.remove(tenant_id);
        Ok(())
    }
    
    /// Check and enforce rate limit for a tenant operation
    /// Returns the delay that should be applied before processing the operation
    pub async fn check_rate_limit(&self, tenant_id: Option<&str>, op_type: StorageOperationType) -> Duration {
        // If no tenant ID is provided, don't apply rate limiting
        let tenant_id = match tenant_id {
            Some(id) => id,
            None => return Duration::from_millis(0),
        };
        
        let rate_limit = {
            let limits = self.tenant_rate_limits.read().await;
            *limits.get(tenant_id).unwrap_or(&self.default_rate_limit)
        };
        
        // If rate limit is 0, it means no limit
        if rate_limit == 0 {
            return Duration::from_millis(0);
        }
        
        let mut counters = self.tenant_counters.lock().await;
        let counter = counters.entry(tenant_id.to_string()).or_insert_with(|| OperationCounter {
            operations: 0,
            window_start: Instant::now(),
            last_operation: Instant::now(),
        });
        
        let now = Instant::now();
        let window_duration = Duration::from_secs(1); // 1 second window
        
        // Reset counter if window has passed
        if now.duration_since(counter.window_start) >= window_duration {
            counter.operations = 0;
            counter.window_start = now;
        }
        
        // Calculate delay based on rate limit and operation count
        let delay = if counter.operations >= rate_limit {
            // Calculate how long to wait until the next window
            let time_since_window_start = now.duration_since(counter.window_start);
            if time_since_window_start < window_duration {
                window_duration - time_since_window_start
            } else {
                Duration::from_millis(0)
            }
        } else {
            Duration::from_millis(0)
        };
        
        // Update counter
        counter.operations += 1;
        counter.last_operation = now;
        
        delay
    }
    
    /// Apply rate limiting for a tenant operation
    /// This will sleep for the appropriate amount of time to enforce the rate limit
    pub async fn apply_rate_limit(&self, tenant_id: Option<&str>, op_type: StorageOperationType) -> Result<()> {
        let delay = self.check_rate_limit(tenant_id, op_type).await;
        
        if !delay.is_zero() {
            sleep(delay).await;
        }
        
        Ok(())
    }
    
    /// Get priority for an operation type
    pub fn get_operation_priority(&self, op_type: StorageOperationType) -> u8 {
        *self.operation_priorities.get(&op_type).unwrap_or(&5)
    }
    
    /// Set priority for an operation type
    pub fn set_operation_priority(&mut self, op_type: StorageOperationType, priority: u8) {
        self.operation_priorities.insert(op_type, priority);
    }
    
    /// Get operation statistics for a tenant
    pub async fn get_tenant_stats(&self, tenant_id: &str) -> Option<TenantStorageStats> {
        let counters = self.tenant_counters.lock().await;
        
        if let Some(counter) = counters.get(tenant_id) {
            let now = Instant::now();
            let rate_limit = {
                let limits = self.tenant_rate_limits.read().await;
                *limits.get(tenant_id).unwrap_or(&self.default_rate_limit)
            };
            
            Some(TenantStorageStats {
                tenant_id: tenant_id.to_string(),
                operations_in_window: counter.operations,
                rate_limit,
                time_since_last_op: now.duration_since(counter.last_operation),
                window_start: counter.window_start,
            })
        } else {
            None
        }
    }
}

/// Statistics for tenant storage operations
#[derive(Debug, Clone)]
pub struct TenantStorageStats {
    pub tenant_id: String,
    pub operations_in_window: u32,
    pub rate_limit: u32,
    pub time_since_last_op: Duration,
    pub window_start: Instant,
}

impl Default for StorageQoSManager {
    fn default() -> Self {
        Self::new(100) // Default rate limit of 100 ops/sec
    }
}