//! Retry pattern implementation for Hivemind
//!
//! This module provides an implementation of the Retry pattern with exponential backoff,
//! which automatically retries failed operations with increasing delays between attempts.

use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::sync::Mutex;
use tracing::{debug, error, info, warn};
use rand::Rng;

/// Exponential backoff configuration
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ExponentialBackoffConfig {
    /// Initial delay in milliseconds
    pub initial_delay_ms: u64,
    /// Maximum delay in milliseconds
    pub max_delay_ms: u64,
    /// Maximum number of retries
    pub max_retries: u32,
    /// Multiplier for each retry
    pub multiplier: f64,
    /// Whether to add jitter to the delay
    pub jitter: bool,
    /// Maximum total duration for all retries in milliseconds
    pub max_duration_ms: Option<u64>,
}

impl Default for ExponentialBackoffConfig {
    fn default() -> Self {
        Self {
            initial_delay_ms: 100,
            max_delay_ms: 10000,
            max_retries: 3,
            multiplier: 2.0,
            jitter: true,
            max_duration_ms: Some(30000),
        }
    }
}

/// Retry policy metrics
#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct RetryMetrics {
    /// Total number of retry attempts
    pub total_retry_attempts: u64,
    /// Total number of successful retries
    pub successful_retries: u64,
    /// Total number of failed retries
    pub failed_retries: u64,
    /// Average delay between retries in milliseconds
    pub avg_delay_ms: f64,
    /// Maximum delay observed in milliseconds
    pub max_delay_observed_ms: u64,
    /// Average number of retries per operation
    pub avg_retries_per_operation: f64,
    /// Maximum number of retries observed for a single operation
    pub max_retries_observed: u32,
}

/// Retry policy implementation
#[derive(Clone)]
pub struct RetryPolicy {
    /// Service name
    service_name: String,
    /// Retry configuration
    config: Arc<Mutex<ExponentialBackoffConfig>>,
    /// Retry metrics
    metrics: Arc<Mutex<RetryMetrics>>,
}

/// Trait for errors that can be retried
pub trait RetryableError {
    /// Check if the error is retryable
    fn is_retryable(&self) -> bool {
        true
    }
    
    /// Create an error from a string
    fn from_error(msg: &str) -> Self;
}

impl RetryPolicy {
    /// Create a new retry policy
    pub fn new(service_name: String, config: ExponentialBackoffConfig) -> Self {
        Self {
            service_name,
            config: Arc::new(Mutex::new(config)),
            metrics: Arc::new(Mutex::new(RetryMetrics::default())),
        }
    }
    
    /// Get current metrics
    pub async fn get_metrics(&self) -> RetryMetrics {
        self.metrics.lock().await.clone()
    }
    
    /// Update configuration
    pub async fn update_config(&self, config: ExponentialBackoffConfig) {
        let mut current_config = self.config.lock().await;
        *current_config = config;
        
        info!(
            "Retry policy for service {} updated configuration (initial_delay_ms: {}, max_delay_ms: {}, max_retries: {})",
            self.service_name, 
            current_config.initial_delay_ms,
            current_config.max_delay_ms,
            current_config.max_retries
        );
    }
    
    /// Reset metrics
    pub async fn reset_metrics(&self) {
        *self.metrics.lock().await = RetryMetrics::default();
        
        info!("Retry policy for service {} reset metrics", self.service_name);
    }
    
    /// Calculate delay for a retry attempt
    async fn calculate_delay(&self, attempt: u32) -> Duration {
        let config = self.config.lock().await.clone();
        
        // Calculate exponential delay
        let base_delay_ms = (config.initial_delay_ms as f64 * config.multiplier.powi(attempt as i32)) as u64;
        
        // Cap at max delay
        let capped_delay_ms = base_delay_ms.min(config.max_delay_ms);
        
        // Add jitter if enabled
        let final_delay_ms = if config.jitter {
            let jitter_factor = rand::thread_rng().gen_range(0.8..1.2);
            (capped_delay_ms as f64 * jitter_factor) as u64
        } else {
            capped_delay_ms
        };
        
        // Update metrics
        let mut metrics = self.metrics.lock().await;
        
        // Update max delay observed
        if final_delay_ms > metrics.max_delay_observed_ms {
            metrics.max_delay_observed_ms = final_delay_ms;
        }
        
        // Update average delay
        let total_attempts = metrics.total_retry_attempts;
        if total_attempts > 0 {
            metrics.avg_delay_ms = ((metrics.avg_delay_ms * (total_attempts as f64)) + (final_delay_ms as f64)) / ((total_attempts + 1) as f64);
        } else {
            metrics.avg_delay_ms = final_delay_ms as f64;
        }
        
        Duration::from_millis(final_delay_ms)
    }
    
    /// Execute a function with retry
    pub async fn execute<F, Fut, T, E>(&self, f: F) -> Result<T, E>
    where
        F: Fn() -> Fut,
        Fut: std::future::Future<Output = Result<T, E>>,
        E: std::error::Error + RetryableError,
    {
        let config = self.config.lock().await.clone();
        let start_time = SystemTime::now();
        let mut attempt = 0;
        let mut last_error = None;
        
        loop {
            // Check if we've exceeded max retries
            if attempt > config.max_retries {
                // Update metrics
                let mut metrics = self.metrics.lock().await;
                metrics.failed_retries += 1;
                
                warn!(
                    "Retry policy for service {} exceeded maximum retries ({}) for operation",
                    self.service_name, config.max_retries
                );
                
                return Err(last_error.unwrap());
            }
            
            // Check if we've exceeded max duration
            if let Some(max_duration_ms) = config.max_duration_ms {
                let elapsed = SystemTime::now()
                    .duration_since(start_time)
                    .unwrap_or_default()
                    .as_millis() as u64;
                
                if elapsed > max_duration_ms {
                    // Update metrics
                    let mut metrics = self.metrics.lock().await;
                    metrics.failed_retries += 1;
                    
                    warn!(
                        "Retry policy for service {} exceeded maximum duration ({}ms) for operation",
                        self.service_name, max_duration_ms
                    );
                    
                    return Err(E::from_error(&format!("Exceeded maximum retry duration of {}ms", max_duration_ms)));
                }
            }
            
            // Execute the function
            let result = f().await;
            
            match result {
                Ok(value) => {
                    // Success, update metrics and return
                    let mut metrics = self.metrics.lock().await;
                    
                    if attempt > 0 {
                        metrics.successful_retries += 1;
                        
                        // Update average retries per operation
                        let total_operations = metrics.successful_retries + metrics.failed_retries;
                        if total_operations > 0 {
                            metrics.avg_retries_per_operation = (metrics.total_retry_attempts as f64) / (total_operations as f64);
                        }
                        
                        debug!(
                            "Retry policy for service {} succeeded after {} attempts",
                            self.service_name, attempt + 1
                        );
                    }
                    
                    return Ok(value);
                }
                Err(err) => {
                    // Check if the error is retryable
                    if !err.is_retryable() {
                        // Non-retryable error, update metrics and return
                        let mut metrics = self.metrics.lock().await;
                        metrics.failed_retries += 1;
                        
                        warn!(
                            "Retry policy for service {} encountered non-retryable error: {}",
                            self.service_name, err
                        );
                        
                        return Err(err);
                    }
                    
                    // Update metrics
                    let mut metrics = self.metrics.lock().await;
                    metrics.total_retry_attempts += 1;
                    
                    // Update max retries observed
                    if attempt + 1 > metrics.max_retries_observed {
                        metrics.max_retries_observed = attempt + 1;
                    }
                    
                    // Store the error for potential return
                    last_error = Some(err);
                    
                    // Increment attempt counter
                    attempt += 1;
                    
                    // Calculate delay for next retry
                    let delay = self.calculate_delay(attempt).await;
                    
                    debug!(
                        "Retry policy for service {} failed attempt {}/{}, retrying in {}ms",
                        self.service_name, attempt, config.max_retries, delay.as_millis()
                    );
                    
                    // Wait before retrying
                    tokio::time::sleep(delay).await;
                }
            }
        }
    }
}

/// Implementation of RetryableError for std::io::Error
impl RetryableError for std::io::Error {
    fn is_retryable(&self) -> bool {
        use std::io::ErrorKind;
        
        match self.kind() {
            // Network-related errors are typically retryable
            ErrorKind::ConnectionRefused |
            ErrorKind::ConnectionReset |
            ErrorKind::ConnectionAborted |
            ErrorKind::NotConnected |
            ErrorKind::AddrInUse |
            ErrorKind::AddrNotAvailable |
            ErrorKind::TimedOut |
            ErrorKind::Interrupted |
            ErrorKind::WouldBlock => true,
            
            // Other errors are generally not retryable
            _ => false,
        }
    }
    
    fn from_error(msg: &str) -> Self {
        std::io::Error::new(std::io::ErrorKind::Other, msg)
    }
}

/// Implementation of RetryableError for anyhow::Error
impl RetryableError for anyhow::Error {
    fn from_error(msg: &str) -> Self {
        anyhow::anyhow!("{}", msg)
    }
}

/// Implementation of RetryableError for Box<dyn std::error::Error>
impl RetryableError for Box<dyn std::error::Error + Send + Sync> {
    fn from_error(msg: &str) -> Self {
        Box::new(std::io::Error::new(std::io::ErrorKind::Other, msg))
    }
}