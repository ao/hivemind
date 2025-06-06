//! Fallback pattern implementation for Hivemind
//!
//! This module provides an implementation of the Fallback pattern,
//! which provides alternative responses when a service fails.

use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};
use tokio::sync::Mutex;
use tracing::{debug, error, info, warn};

/// Fallback strategy
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub enum FallbackStrategy {
    /// Return a static value
    StaticValue,
    /// Return a cached value
    Cache {
        /// Maximum age of cached value in milliseconds
        max_age_ms: u64,
    },
    /// Call an alternative service
    AlternativeService {
        /// Name of the alternative service
        service_name: String,
    },
    /// Return an error
    Error,
}

impl Default for FallbackStrategy {
    fn default() -> Self {
        Self::Error
    }
}

/// Fallback metrics
#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct FallbackMetrics {
    /// Total number of fallbacks
    pub total_fallbacks: u64,
    /// Number of successful fallbacks
    pub successful_fallbacks: u64,
    /// Number of failed fallbacks
    pub failed_fallbacks: u64,
    /// Number of static value fallbacks
    pub static_value_fallbacks: u64,
    /// Number of cache fallbacks
    pub cache_fallbacks: u64,
    /// Number of alternative service fallbacks
    pub alternative_service_fallbacks: u64,
    /// Number of error fallbacks
    pub error_fallbacks: u64,
}

/// Fallback policy implementation
#[derive(Clone)]
pub struct FallbackPolicy {
    /// Service name
    service_name: String,
    /// Fallback strategy
    strategy: Arc<Mutex<FallbackStrategy>>,
    /// Fallback metrics
    metrics: Arc<Mutex<FallbackMetrics>>,
    /// Cache for fallback values
    cache: Arc<Mutex<Option<FallbackCache>>>,
}

/// Cache for fallback values
#[derive(Clone, Debug)]
struct FallbackCache {
    /// Cached value
    value: Vec<u8>,
    /// Timestamp when the value was cached
    timestamp: u64,
}

impl FallbackPolicy {
    /// Create a new fallback policy
    pub fn new(service_name: String, strategy: FallbackStrategy) -> Self {
        Self {
            service_name,
            strategy: Arc::new(Mutex::new(strategy)),
            metrics: Arc::new(Mutex::new(FallbackMetrics::default())),
            cache: Arc::new(Mutex::new(None)),
        }
    }
    
    /// Get current metrics
    pub async fn get_metrics(&self) -> FallbackMetrics {
        self.metrics.lock().await.clone()
    }
    
    /// Update strategy
    pub async fn update_strategy(&self, strategy: FallbackStrategy) {
        let mut current_strategy = self.strategy.lock().await;
        *current_strategy = strategy.clone();
        
        info!(
            "Fallback policy for service {} updated strategy to {:?}",
            self.service_name, strategy
        );
    }
    
    /// Reset metrics
    pub async fn reset_metrics(&self) {
        *self.metrics.lock().await = FallbackMetrics::default();
        
        info!("Fallback policy for service {} reset metrics", self.service_name);
    }
    
    /// Clear cache
    pub async fn clear_cache(&self) {
        *self.cache.lock().await = None;
        
        info!("Fallback policy for service {} cleared cache", self.service_name);
    }
    
    /// Update cache
    pub async fn update_cache<T: serde::Serialize>(&self, value: &T) -> Result<()> {
        let serialized = serde_json::to_vec(value)?;
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        let cache_entry = FallbackCache {
            value: serialized,
            timestamp: now,
        };
        
        *self.cache.lock().await = Some(cache_entry);
        
        debug!("Fallback policy for service {} updated cache", self.service_name);
        
        Ok(())
    }
    
    /// Apply fallback strategy
    pub async fn apply<T, E>(&self, result: Result<T, E>) -> Result<T, E>
    where
        T: serde::Serialize + serde::de::DeserializeOwned + Clone,
        E: std::error::Error,
    {
        // If the result is Ok, return it
        if result.is_ok() {
            // Update cache if using cache strategy
            let strategy = self.strategy.lock().await.clone();
            if let FallbackStrategy::Cache { .. } = strategy {
                if let Ok(value) = &result {
                    if let Err(e) = self.update_cache(value).await {
                        warn!("Failed to update fallback cache: {}", e);
                    }
                }
            }
            
            return result;
        }
        
        // Update metrics
        let mut metrics = self.metrics.lock().await;
        metrics.total_fallbacks += 1;
        
        // Apply fallback strategy
        let strategy = self.strategy.lock().await.clone();
        match strategy {
            FallbackStrategy::StaticValue => {
                metrics.static_value_fallbacks += 1;
                drop(metrics);
                
                // In a real implementation, this would return a static value
                // For now, we'll just return an error
                warn!(
                    "Fallback policy for service {} using static value strategy",
                    self.service_name
                );
                
                Err(result.unwrap_err())
            },
            FallbackStrategy::Cache { max_age_ms } => {
                metrics.cache_fallbacks += 1;
                drop(metrics);
                
                let cache = self.cache.lock().await.clone();
                
                if let Some(cache_entry) = cache {
                    let now = SystemTime::now()
                        .duration_since(UNIX_EPOCH)
                        .unwrap_or_default()
                        .as_millis() as u64;
                    
                    // Check if cache is still valid
                    if now - cache_entry.timestamp <= max_age_ms {
                        // Deserialize cached value
                        match serde_json::from_slice(&cache_entry.value) {
                            Ok(value) => {
                                debug!(
                                    "Fallback policy for service {} using cached value",
                                    self.service_name
                                );
                                
                                let mut metrics = self.metrics.lock().await;
                                metrics.successful_fallbacks += 1;
                                
                                return Ok(value);
                            },
                            Err(e) => {
                                warn!(
                                    "Fallback policy for service {} failed to deserialize cached value: {}",
                                    self.service_name, e
                                );
                                
                                let mut metrics = self.metrics.lock().await;
                                metrics.failed_fallbacks += 1;
                                
                                return Err(result.unwrap_err());
                            }
                        }
                    } else {
                        warn!(
                            "Fallback policy for service {} cache expired (age: {}ms, max: {}ms)",
                            self.service_name, now - cache_entry.timestamp, max_age_ms
                        );
                        
                        let mut metrics = self.metrics.lock().await;
                        metrics.failed_fallbacks += 1;
                        
                        return Err(result.unwrap_err());
                    }
                } else {
                    warn!(
                        "Fallback policy for service {} has no cached value",
                        self.service_name
                    );
                    
                    let mut metrics = self.metrics.lock().await;
                    metrics.failed_fallbacks += 1;
                    
                    return Err(result.unwrap_err());
                }
            },
            FallbackStrategy::AlternativeService { service_name } => {
                metrics.alternative_service_fallbacks += 1;
                drop(metrics);
                
                // In a real implementation, this would call an alternative service
                // For now, we'll just log and return an error
                warn!(
                    "Fallback policy for service {} using alternative service {}",
                    self.service_name, service_name
                );
                
                let mut metrics = self.metrics.lock().await;
                metrics.failed_fallbacks += 1;
                
                Err(result.unwrap_err())
            },
            FallbackStrategy::Error => {
                metrics.error_fallbacks += 1;
                metrics.failed_fallbacks += 1;
                drop(metrics);
                
                warn!(
                    "Fallback policy for service {} using error strategy",
                    self.service_name
                );
                
                Err(result.unwrap_err())
            },
        }
    }
    
    /// Execute a function with fallback
    pub async fn execute_with_fallback<F, Fut, T, E>(&self, f: F) -> Result<T, E>
    where
        F: FnOnce() -> Fut,
        Fut: std::future::Future<Output = Result<T, E>>,
        T: serde::Serialize + serde::de::DeserializeOwned + Clone,
        E: std::error::Error,
    {
        let result = f().await;
        self.apply(result).await
    }
}