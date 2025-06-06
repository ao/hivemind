//! Bulkhead pattern implementation for Hivemind
//!
//! This module provides an implementation of the Bulkhead pattern,
//! which limits the number of concurrent requests to a service to prevent
//! cascading failures.

use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::sync::{Mutex, Semaphore};
use tracing::{debug, error, info, warn};

/// Bulkhead configuration
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct BulkheadConfig {
    /// Maximum number of concurrent requests
    pub max_concurrent_calls: u32,
    /// Maximum number of queued requests
    pub max_queue_size: u32,
    /// Timeout for acquiring a permit (milliseconds)
    pub acquire_timeout_ms: u64,
}

impl Default for BulkheadConfig {
    fn default() -> Self {
        Self {
            max_concurrent_calls: 10,
            max_queue_size: 20,
            acquire_timeout_ms: 1000,
        }
    }
}

/// Bulkhead metrics
#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct BulkheadMetrics {
    /// Total number of successful requests
    pub successful_calls: u64,
    /// Total number of rejected requests
    pub rejected_calls: u64,
    /// Total number of timed out requests
    pub timed_out_calls: u64,
    /// Current number of active requests
    pub active_calls: u32,
    /// Current number of queued requests
    pub queued_calls: u32,
    /// Maximum number of concurrent calls observed
    pub max_concurrent_calls_observed: u32,
    /// Maximum number of queued calls observed
    pub max_queued_calls_observed: u32,
    /// Average wait time (milliseconds)
    pub avg_wait_time_ms: f64,
    /// Average execution time (milliseconds)
    pub avg_execution_time_ms: f64,
}

/// Bulkhead implementation
#[derive(Clone)]
pub struct Bulkhead {
    /// Service name
    service_name: String,
    /// Bulkhead configuration
    config: Arc<Mutex<BulkheadConfig>>,
    /// Bulkhead metrics
    metrics: Arc<Mutex<BulkheadMetrics>>,
    /// Semaphore for limiting concurrent requests
    semaphore: Arc<Semaphore>,
}

impl Bulkhead {
    /// Create a new bulkhead
    pub fn new(service_name: String, config: BulkheadConfig) -> Self {
        let semaphore = Arc::new(Semaphore::new(config.max_concurrent_calls as usize));
        
        Self {
            service_name,
            config: Arc::new(Mutex::new(config)),
            metrics: Arc::new(Mutex::new(BulkheadMetrics::default())),
            semaphore,
        }
    }
    
    /// Acquire permission to execute a request
    pub async fn acquire_permission(&self) -> bool {
        let start_time = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        let config = self.config.lock().await.clone();
        
        // Try to acquire a permit with timeout
        let result = tokio::time::timeout(
            Duration::from_millis(config.acquire_timeout_ms),
            self.semaphore.acquire(),
        ).await;
        
        let end_time = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        let wait_time = end_time - start_time;
        
        let mut metrics = self.metrics.lock().await;
        
        match result {
            Ok(Ok(permit)) => {
                // Successfully acquired a permit
                permit.forget(); // Don't release the permit automatically
                
                metrics.active_calls += 1;
                metrics.queued_calls = metrics.queued_calls.saturating_sub(1);
                
                // Update max observed values
                if metrics.active_calls > metrics.max_concurrent_calls_observed {
                    metrics.max_concurrent_calls_observed = metrics.active_calls;
                }
                
                // Update average wait time
                let total_calls = metrics.successful_calls + metrics.rejected_calls + metrics.timed_out_calls;
                if total_calls > 0 {
                    metrics.avg_wait_time_ms = ((metrics.avg_wait_time_ms * (total_calls as f64)) + (wait_time as f64)) / ((total_calls + 1) as f64);
                } else {
                    metrics.avg_wait_time_ms = wait_time as f64;
                }
                
                debug!(
                    "Bulkhead for service {} acquired permission (active: {}, max: {})",
                    self.service_name, metrics.active_calls, config.max_concurrent_calls
                );
                
                true
            },
            Ok(Err(_)) => {
                // Failed to acquire a permit (semaphore closed)
                metrics.rejected_calls += 1;
                
                warn!(
                    "Bulkhead for service {} rejected request (semaphore closed)",
                    self.service_name
                );
                
                false
            },
            Err(_) => {
                // Timeout waiting for a permit
                metrics.timed_out_calls += 1;
                
                warn!(
                    "Bulkhead for service {} timed out waiting for permission after {}ms",
                    self.service_name, config.acquire_timeout_ms
                );
                
                false
            }
        }
    }
    
    /// Release permission after request completion
    pub async fn release_permission(&self) {
        let mut metrics = self.metrics.lock().await;
        
        // Add a permit back to the semaphore
        self.semaphore.add_permits(1);
        
        // Update metrics
        metrics.active_calls = metrics.active_calls.saturating_sub(1);
        metrics.successful_calls += 1;
        
        debug!(
            "Bulkhead for service {} released permission (active: {})",
            self.service_name, metrics.active_calls
        );
    }
    
    /// Get current metrics
    pub async fn get_metrics(&self) -> BulkheadMetrics {
        self.metrics.lock().await.clone()
    }
    
    /// Update configuration
    pub async fn update_config(&self, config: BulkheadConfig) {
        let mut current_config = self.config.lock().await;
        
        // Update the semaphore if max_concurrent_calls changed
        if config.max_concurrent_calls != current_config.max_concurrent_calls {
            let current_permits = self.semaphore.available_permits();
            let diff = config.max_concurrent_calls as isize - current_config.max_concurrent_calls as isize;
            
            if diff > 0 {
                // Add permits
                self.semaphore.add_permits(diff as usize);
            } else if diff < 0 {
                // Remove permits (up to available)
                let to_remove = (-diff as usize).min(current_permits);
                for _ in 0..to_remove {
                    if let Ok(permit) = self.semaphore.try_acquire() {
                        permit.forget();
                    }
                }
            }
        }
        
        // Update config
        *current_config = config;
        
        info!(
            "Bulkhead for service {} updated configuration (max_concurrent_calls: {}, max_queue_size: {}, acquire_timeout_ms: {})",
            self.service_name, 
            current_config.max_concurrent_calls,
            current_config.max_queue_size,
            current_config.acquire_timeout_ms
        );
    }
    
    /// Reset the bulkhead
    pub async fn reset(&self) {
        // Reset metrics
        *self.metrics.lock().await = BulkheadMetrics::default();
        
        // Reset semaphore
        let config = self.config.lock().await;
        let current_permits = self.semaphore.available_permits();
        let max_permits = config.max_concurrent_calls as usize;
        
        if current_permits < max_permits {
            // Add missing permits
            self.semaphore.add_permits(max_permits - current_permits);
        } else if current_permits > max_permits {
            // Remove excess permits
            let to_remove = current_permits - max_permits;
            for _ in 0..to_remove {
                if let Ok(permit) = self.semaphore.try_acquire() {
                    permit.forget();
                }
            }
        }
        
        info!("Bulkhead for service {} reset", self.service_name);
    }
    
    /// Execute a function with bulkhead protection
    pub async fn execute<F, Fut, T, E>(&self, f: F) -> Result<T, E>
    where
        F: FnOnce() -> Fut,
        Fut: std::future::Future<Output = Result<T, E>>,
        E: std::error::Error,
    {
        // Try to acquire permission
        if !self.acquire_permission().await {
            return Err(E::from_error("Bulkhead rejected request"));
        }
        
        let start_time = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        // Execute the function
        let result = f().await;
        
        let end_time = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        let execution_time = end_time - start_time;
        
        // Update metrics
        let mut metrics = self.metrics.lock().await;
        let total_calls = metrics.successful_calls + metrics.rejected_calls + metrics.timed_out_calls;
        if total_calls > 0 {
            metrics.avg_execution_time_ms = ((metrics.avg_execution_time_ms * (total_calls as f64)) + (execution_time as f64)) / ((total_calls + 1) as f64);
        } else {
            metrics.avg_execution_time_ms = execution_time as f64;
        }
        drop(metrics);
        
        // Release permission
        self.release_permission().await;
        
        result
    }
}

/// Extension trait for error types
impl<E: std::error::Error + 'static> crate::resilience::RetryableError for E {
    fn from_error(msg: &str) -> Self {
        std::io::Error::new(std::io::ErrorKind::Other, msg).into()
    }
}