//! Resilience module for Hivemind
//! 
//! This module provides resilience patterns for the Hivemind system, including:
//! - Circuit Breaker pattern
//! - Bulkhead pattern
//! - Retry with exponential backoff
//! - Fallback mechanisms

mod circuit_breaker;
mod bulkhead;
mod retry;
mod fallback;

pub use circuit_breaker::{
    CircuitBreaker, 
    CircuitBreakerConfig, 
    CircuitBreakerState, 
    CircuitBreakerMetrics,
    CircuitBreakerEvent,
    CircuitBreakerEventType,
    CircuitBreakerStorage,
    InMemoryCircuitBreakerStorage,
    PersistentCircuitBreakerStorage,
};

pub use bulkhead::{
    Bulkhead,
    BulkheadConfig,
    BulkheadMetrics,
};

pub use retry::{
    RetryPolicy,
    ExponentialBackoffConfig,
    RetryableError,
};

pub use fallback::{
    FallbackPolicy,
    FallbackStrategy,
};

/// Resilience manager that coordinates all resilience patterns
pub struct ResilienceManager {
    circuit_breakers: std::sync::Arc<tokio::sync::RwLock<std::collections::HashMap<String, CircuitBreaker>>>,
    bulkheads: std::sync::Arc<tokio::sync::RwLock<std::collections::HashMap<String, Bulkhead>>>,
    retry_policies: std::sync::Arc<tokio::sync::RwLock<std::collections::HashMap<String, RetryPolicy>>>,
    fallback_policies: std::sync::Arc<tokio::sync::RwLock<std::collections::HashMap<String, FallbackPolicy>>>,
}

impl ResilienceManager {
    /// Create a new resilience manager
    pub fn new() -> Self {
        Self {
            circuit_breakers: std::sync::Arc::new(tokio::sync::RwLock::new(std::collections::HashMap::new())),
            bulkheads: std::sync::Arc::new(tokio::sync::RwLock::new(std::collections::HashMap::new())),
            retry_policies: std::sync::Arc::new(tokio::sync::RwLock::new(std::collections::HashMap::new())),
            fallback_policies: std::sync::Arc::new(tokio::sync::RwLock::new(std::collections::HashMap::new())),
        }
    }

    /// Get or create a circuit breaker for a service
    pub async fn get_or_create_circuit_breaker(&self, service_name: &str, config: Option<CircuitBreakerConfig>) -> CircuitBreaker {
        let mut circuit_breakers = self.circuit_breakers.write().await;
        
        if !circuit_breakers.contains_key(service_name) {
            let config = config.unwrap_or_default();
            let circuit_breaker = CircuitBreaker::new(service_name.to_string(), config);
            circuit_breakers.insert(service_name.to_string(), circuit_breaker);
        }
        
        circuit_breakers.get(service_name).unwrap().clone()
    }

    /// Get or create a bulkhead for a service
    pub async fn get_or_create_bulkhead(&self, service_name: &str, config: Option<BulkheadConfig>) -> Bulkhead {
        let mut bulkheads = self.bulkheads.write().await;
        
        if !bulkheads.contains_key(service_name) {
            let config = config.unwrap_or_default();
            let bulkhead = Bulkhead::new(service_name.to_string(), config);
            bulkheads.insert(service_name.to_string(), bulkhead);
        }
        
        bulkheads.get(service_name).unwrap().clone()
    }

    /// Get or create a retry policy for a service
    pub async fn get_or_create_retry_policy(&self, service_name: &str, config: Option<ExponentialBackoffConfig>) -> RetryPolicy {
        let mut retry_policies = self.retry_policies.write().await;
        
        if !retry_policies.contains_key(service_name) {
            let config = config.unwrap_or_default();
            let retry_policy = RetryPolicy::new(service_name.to_string(), config);
            retry_policies.insert(service_name.to_string(), retry_policy);
        }
        
        retry_policies.get(service_name).unwrap().clone()
    }

    /// Get or create a fallback policy for a service
    pub async fn get_or_create_fallback_policy(&self, service_name: &str, strategy: Option<FallbackStrategy>) -> FallbackPolicy {
        let mut fallback_policies = self.fallback_policies.write().await;
        
        if !fallback_policies.contains_key(service_name) {
            let strategy = strategy.unwrap_or_default();
            let fallback_policy = FallbackPolicy::new(service_name.to_string(), strategy);
            fallback_policies.insert(service_name.to_string(), fallback_policy);
        }
        
        fallback_policies.get(service_name).unwrap().clone()
    }

    /// Execute a function with all resilience patterns applied
    pub async fn execute<F, Fut, T, E>(&self, service_name: &str, f: F) -> Result<T, E>
    where
        F: FnOnce() -> Fut,
        Fut: std::future::Future<Output = Result<T, E>>,
        E: std::error::Error + RetryableError,
        T: Clone,
    {
        let circuit_breaker = self.get_or_create_circuit_breaker(service_name, None).await;
        let bulkhead = self.get_or_create_bulkhead(service_name, None).await;
        let retry_policy = self.get_or_create_retry_policy(service_name, None).await;
        let fallback_policy = self.get_or_create_fallback_policy(service_name, None).await;
        
        // Apply circuit breaker
        if !circuit_breaker.allow_request().await {
            return Err(E::from_error("Circuit breaker open"));
        }
        
        // Apply bulkhead
        if !bulkhead.acquire_permission().await {
            return Err(E::from_error("Bulkhead full"));
        }
        
        // Execute with retry and record result
        let result = retry_policy.execute(f).await;
        
        // Release bulkhead permission
        bulkhead.release_permission().await;
        
        // Record circuit breaker result
        match &result {
            Ok(_) => circuit_breaker.record_success().await,
            Err(_) => circuit_breaker.record_failure().await,
        }
        
        // Apply fallback if needed
        if result.is_err() {
            return fallback_policy.apply(result).await;
        }
        
        result
    }
}

impl Default for ResilienceManager {
    fn default() -> Self {
        Self::new()
    }
}