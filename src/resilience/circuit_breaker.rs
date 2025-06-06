//! Circuit Breaker implementation for Hivemind
//! 
//! This module provides an enhanced implementation of the Circuit Breaker pattern
//! with proper state management, persistence, metrics, and configuration.

use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::sync::{Mutex, RwLock};
use tracing::{debug, error, info, warn};

/// Circuit breaker state
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub enum CircuitBreakerState {
    /// Normal operation, requests are allowed
    Closed,
    /// Circuit is open, requests are rejected
    Open,
    /// Testing if the service is healthy again
    HalfOpen,
}

impl std::fmt::Display for CircuitBreakerState {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            CircuitBreakerState::Closed => write!(f, "closed"),
            CircuitBreakerState::Open => write!(f, "open"),
            CircuitBreakerState::HalfOpen => write!(f, "half-open"),
        }
    }
}

/// Circuit breaker configuration
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct CircuitBreakerConfig {
    /// Maximum number of concurrent connections
    pub max_connections: u32,
    /// Maximum number of pending requests
    pub max_pending_requests: u32,
    /// Maximum number of requests per time window
    pub max_requests: u32,
    /// Maximum number of retries
    pub max_retries: u32,
    /// Number of consecutive errors before opening the circuit
    pub consecutive_errors_threshold: u32,
    /// Time window for error tracking in milliseconds
    pub interval_ms: u64,
    /// Time to keep the circuit open before transitioning to half-open in milliseconds
    pub open_to_half_open_timeout_ms: u64,
    /// Number of successful calls required to close the circuit from half-open state
    pub half_open_success_threshold: u32,
    /// Maximum number of calls allowed in half-open state
    pub half_open_allowed_calls: u32,
    /// Whether to enable automatic transition from open to half-open
    pub automatic_transition_from_open_to_half_open: bool,
}

impl Default for CircuitBreakerConfig {
    fn default() -> Self {
        Self {
            max_connections: 100,
            max_pending_requests: 10,
            max_requests: 1000,
            max_retries: 3,
            consecutive_errors_threshold: 5,
            interval_ms: 10000,
            open_to_half_open_timeout_ms: 30000,
            half_open_success_threshold: 5,
            half_open_allowed_calls: 5,
            automatic_transition_from_open_to_half_open: true,
        }
    }
}

/// Circuit breaker event type
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub enum CircuitBreakerEventType {
    /// State transition event
    StateTransition {
        /// Previous state
        from: CircuitBreakerState,
        /// New state
        to: CircuitBreakerState,
    },
    /// Success event
    Success,
    /// Failure event
    Failure,
    /// Rejected event (circuit is open)
    Rejected,
    /// Configuration change event
    ConfigurationChanged,
}

/// Circuit breaker event
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct CircuitBreakerEvent {
    /// Event type
    pub event_type: CircuitBreakerEventType,
    /// Timestamp of the event
    pub timestamp: u64,
    /// Service name
    pub service_name: String,
    /// Endpoint identifier (e.g., IP address)
    pub endpoint_id: Option<String>,
    /// Additional metadata
    pub metadata: Option<HashMap<String, String>>,
}

/// Circuit breaker metrics
#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct CircuitBreakerMetrics {
    /// Total number of successful requests
    pub success_count: u64,
    /// Total number of failed requests
    pub failure_count: u64,
    /// Total number of rejected requests (circuit open)
    pub rejected_count: u64,
    /// Number of state transitions
    pub state_transition_count: u64,
    /// Current state
    pub current_state: CircuitBreakerState,
    /// Time spent in each state (milliseconds)
    pub time_in_state: HashMap<CircuitBreakerState, u64>,
    /// Last state change timestamp
    pub last_state_change: u64,
    /// Success rate (0.0 - 1.0)
    pub success_rate: f64,
    /// Average response time (milliseconds)
    pub avg_response_time_ms: f64,
    /// Recent failures (timestamp -> error message)
    pub recent_failures: Vec<(u64, String)>,
    /// Recent successes (timestamp)
    pub recent_successes: Vec<u64>,
    /// Number of consecutive failures
    pub consecutive_failures: u32,
    /// Number of consecutive successes
    pub consecutive_successes: u32,
    /// Number of calls in half-open state
    pub half_open_calls: u32,
}

/// Circuit breaker storage trait
#[async_trait::async_trait]
pub trait CircuitBreakerStorage: Send + Sync {
    /// Get circuit breaker state
    async fn get_state(&self, service_name: &str, endpoint_id: Option<&str>) -> Result<Option<CircuitBreakerState>>;
    
    /// Set circuit breaker state
    async fn set_state(&self, service_name: &str, endpoint_id: Option<&str>, state: CircuitBreakerState) -> Result<()>;
    
    /// Get circuit breaker metrics
    async fn get_metrics(&self, service_name: &str, endpoint_id: Option<&str>) -> Result<Option<CircuitBreakerMetrics>>;
    
    /// Update circuit breaker metrics
    async fn update_metrics(&self, service_name: &str, endpoint_id: Option<&str>, metrics: CircuitBreakerMetrics) -> Result<()>;
    
    /// Record circuit breaker event
    async fn record_event(&self, event: CircuitBreakerEvent) -> Result<()>;
    
    /// Get circuit breaker events
    async fn get_events(&self, service_name: &str, limit: usize) -> Result<Vec<CircuitBreakerEvent>>;
    
    /// Get circuit breaker configuration
    async fn get_config(&self, service_name: &str, endpoint_id: Option<&str>) -> Result<Option<CircuitBreakerConfig>>;
    
    /// Set circuit breaker configuration
    async fn set_config(&self, service_name: &str, endpoint_id: Option<&str>, config: CircuitBreakerConfig) -> Result<()>;
}

/// In-memory circuit breaker storage
pub struct InMemoryCircuitBreakerStorage {
    states: Arc<RwLock<HashMap<String, CircuitBreakerState>>>,
    metrics: Arc<RwLock<HashMap<String, CircuitBreakerMetrics>>>,
    events: Arc<RwLock<HashMap<String, Vec<CircuitBreakerEvent>>>>,
    configs: Arc<RwLock<HashMap<String, CircuitBreakerConfig>>>,
}

impl InMemoryCircuitBreakerStorage {
    /// Create a new in-memory circuit breaker storage
    pub fn new() -> Self {
        Self {
            states: Arc::new(RwLock::new(HashMap::new())),
            metrics: Arc::new(RwLock::new(HashMap::new())),
            events: Arc::new(RwLock::new(HashMap::new())),
            configs: Arc::new(RwLock::new(HashMap::new())),
        }
    }
    
    /// Generate a key for the storage
    fn generate_key(service_name: &str, endpoint_id: Option<&str>) -> String {
        match endpoint_id {
            Some(id) => format!("{}:{}", service_name, id),
            None => service_name.to_string(),
        }
    }
}

#[async_trait::async_trait]
impl CircuitBreakerStorage for InMemoryCircuitBreakerStorage {
    async fn get_state(&self, service_name: &str, endpoint_id: Option<&str>) -> Result<Option<CircuitBreakerState>> {
        let key = Self::generate_key(service_name, endpoint_id);
        let states = self.states.read().await;
        Ok(states.get(&key).cloned())
    }
    
    async fn set_state(&self, service_name: &str, endpoint_id: Option<&str>, state: CircuitBreakerState) -> Result<()> {
        let key = Self::generate_key(service_name, endpoint_id);
        let mut states = self.states.write().await;
        states.insert(key, state);
        Ok(())
    }
    
    async fn get_metrics(&self, service_name: &str, endpoint_id: Option<&str>) -> Result<Option<CircuitBreakerMetrics>> {
        let key = Self::generate_key(service_name, endpoint_id);
        let metrics = self.metrics.read().await;
        Ok(metrics.get(&key).cloned())
    }
    
    async fn update_metrics(&self, service_name: &str, endpoint_id: Option<&str>, metrics: CircuitBreakerMetrics) -> Result<()> {
        let key = Self::generate_key(service_name, endpoint_id);
        let mut metrics_map = self.metrics.write().await;
        metrics_map.insert(key, metrics);
        Ok(())
    }
    
    async fn record_event(&self, event: CircuitBreakerEvent) -> Result<()> {
        let service_name = event.service_name.clone();
        let mut events = self.events.write().await;
        
        events.entry(service_name)
            .or_insert_with(Vec::new)
            .push(event);
        
        Ok(())
    }
    
    async fn get_events(&self, service_name: &str, limit: usize) -> Result<Vec<CircuitBreakerEvent>> {
        let events = self.events.read().await;
        
        if let Some(service_events) = events.get(service_name) {
            let start = if service_events.len() > limit {
                service_events.len() - limit
            } else {
                0
            };
            
            Ok(service_events[start..].to_vec())
        } else {
            Ok(Vec::new())
        }
    }
    
    async fn get_config(&self, service_name: &str, endpoint_id: Option<&str>) -> Result<Option<CircuitBreakerConfig>> {
        let key = Self::generate_key(service_name, endpoint_id);
        let configs = self.configs.read().await;
        Ok(configs.get(&key).cloned())
    }
    
    async fn set_config(&self, service_name: &str, endpoint_id: Option<&str>, config: CircuitBreakerConfig) -> Result<()> {
        let key = Self::generate_key(service_name, endpoint_id);
        let mut configs = self.configs.write().await;
        configs.insert(key, config);
        Ok(())
    }
}

/// Persistent circuit breaker storage
pub struct PersistentCircuitBreakerStorage {
    // In a real implementation, this would use a database or file system
    // For now, we'll use in-memory storage as a placeholder
    inner: InMemoryCircuitBreakerStorage,
}

impl PersistentCircuitBreakerStorage {
    /// Create a new persistent circuit breaker storage
    pub fn new() -> Self {
        Self {
            inner: InMemoryCircuitBreakerStorage::new(),
        }
    }
}

#[async_trait::async_trait]
impl CircuitBreakerStorage for PersistentCircuitBreakerStorage {
    async fn get_state(&self, service_name: &str, endpoint_id: Option<&str>) -> Result<Option<CircuitBreakerState>> {
        self.inner.get_state(service_name, endpoint_id).await
    }
    
    async fn set_state(&self, service_name: &str, endpoint_id: Option<&str>, state: CircuitBreakerState) -> Result<()> {
        self.inner.set_state(service_name, endpoint_id, state).await
    }
    
    async fn get_metrics(&self, service_name: &str, endpoint_id: Option<&str>) -> Result<Option<CircuitBreakerMetrics>> {
        self.inner.get_metrics(service_name, endpoint_id).await
    }
    
    async fn update_metrics(&self, service_name: &str, endpoint_id: Option<&str>, metrics: CircuitBreakerMetrics) -> Result<()> {
        self.inner.update_metrics(service_name, endpoint_id, metrics).await
    }
    
    async fn record_event(&self, event: CircuitBreakerEvent) -> Result<()> {
        self.inner.record_event(event).await
    }
    
    async fn get_events(&self, service_name: &str, limit: usize) -> Result<Vec<CircuitBreakerEvent>> {
        self.inner.get_events(service_name, limit).await
    }
    
    async fn get_config(&self, service_name: &str, endpoint_id: Option<&str>) -> Result<Option<CircuitBreakerConfig>> {
        self.inner.get_config(service_name, endpoint_id).await
    }
    
    async fn set_config(&self, service_name: &str, endpoint_id: Option<&str>, config: CircuitBreakerConfig) -> Result<()> {
        self.inner.set_config(service_name, endpoint_id, config).await
    }
}

/// Circuit breaker implementation
#[derive(Clone)]
pub struct CircuitBreaker {
    /// Service name
    service_name: String,
    /// Endpoint identifier (e.g., IP address)
    endpoint_id: Option<String>,
    /// Circuit breaker configuration
    config: Arc<RwLock<CircuitBreakerConfig>>,
    /// Circuit breaker state
    state: Arc<RwLock<CircuitBreakerState>>,
    /// Circuit breaker metrics
    metrics: Arc<RwLock<CircuitBreakerMetrics>>,
    /// Circuit breaker storage
    storage: Arc<dyn CircuitBreakerStorage>,
    /// Last state change timestamp
    last_state_change: Arc<RwLock<u64>>,
    /// State transition lock to prevent race conditions
    state_transition_lock: Arc<Mutex<()>>,
}

impl CircuitBreaker {
    /// Create a new circuit breaker
    pub fn new(service_name: String, config: CircuitBreakerConfig) -> Self {
        Self::new_with_storage(
            service_name,
            None,
            config,
            Arc::new(InMemoryCircuitBreakerStorage::new()),
        )
    }
    
    /// Create a new circuit breaker with endpoint ID
    pub fn new_with_endpoint(service_name: String, endpoint_id: String, config: CircuitBreakerConfig) -> Self {
        Self::new_with_storage(
            service_name,
            Some(endpoint_id),
            config,
            Arc::new(InMemoryCircuitBreakerStorage::new()),
        )
    }
    
    /// Create a new circuit breaker with custom storage
    pub fn new_with_storage(
        service_name: String,
        endpoint_id: Option<String>,
        config: CircuitBreakerConfig,
        storage: Arc<dyn CircuitBreakerStorage>,
    ) -> Self {
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        let mut metrics = CircuitBreakerMetrics::default();
        metrics.current_state = CircuitBreakerState::Closed;
        metrics.last_state_change = now;
        
        Self {
            service_name,
            endpoint_id,
            config: Arc::new(RwLock::new(config)),
            state: Arc::new(RwLock::new(CircuitBreakerState::Closed)),
            metrics: Arc::new(RwLock::new(metrics)),
            storage,
            last_state_change: Arc::new(RwLock::new(now)),
            state_transition_lock: Arc::new(Mutex::new(())),
        }
    }
    
    /// Initialize the circuit breaker from storage
    pub async fn initialize(&self) -> Result<()> {
        // Load state from storage
        if let Some(state) = self.storage.get_state(&self.service_name, self.endpoint_id.as_deref()).await? {
            *self.state.write().await = state;
        }
        
        // Load metrics from storage
        if let Some(metrics) = self.storage.get_metrics(&self.service_name, self.endpoint_id.as_deref()).await? {
            *self.metrics.write().await = metrics;
        }
        
        // Load config from storage
        if let Some(config) = self.storage.get_config(&self.service_name, self.endpoint_id.as_deref()).await? {
            *self.config.write().await = config;
        } else {
            // Save default config to storage
            self.storage.set_config(
                &self.service_name,
                self.endpoint_id.as_deref(),
                self.config.read().await.clone(),
            ).await?;
        }
        
        Ok(())
    }
    
    /// Check if a request is allowed
    pub async fn allow_request(&self) -> bool {
        // Check if we need to transition from open to half-open
        self.check_auto_transition_to_half_open().await;
        
        // Get current state
        let state = self.state.read().await.clone();
        
        match state {
            CircuitBreakerState::Closed => true,
            CircuitBreakerState::Open => false,
            CircuitBreakerState::HalfOpen => {
                // In half-open state, allow limited requests
                let metrics = self.metrics.read().await;
                metrics.half_open_calls < self.config.read().await.half_open_allowed_calls
            }
        }
    }
    
    /// Record a successful request
    pub async fn record_success(&self) -> Result<()> {
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        // Update metrics
        let mut metrics = self.metrics.write().await;
        metrics.success_count += 1;
        metrics.consecutive_failures = 0;
        metrics.consecutive_successes += 1;
        metrics.recent_successes.push(now);
        
        // Keep only the last 100 successes
        if metrics.recent_successes.len() > 100 {
            metrics.recent_successes.remove(0);
        }
        
        // Calculate success rate
        let total = metrics.success_count + metrics.failure_count;
        if total > 0 {
            metrics.success_rate = metrics.success_count as f64 / total as f64;
        }
        
        // Update half-open calls if in half-open state
        let current_state = self.state.read().await.clone();
        if current_state == CircuitBreakerState::HalfOpen {
            metrics.half_open_calls += 1;
            
            // Check if we should transition to closed state
            if metrics.consecutive_successes >= self.config.read().await.half_open_success_threshold {
                drop(metrics); // Release the lock before transition
                self.transition_to_state(CircuitBreakerState::Closed).await?;
            }
        }
        
        // Save metrics to storage
        self.storage.update_metrics(
            &self.service_name,
            self.endpoint_id.as_deref(),
            self.metrics.read().await.clone(),
        ).await?;
        
        // Record success event
        self.storage.record_event(CircuitBreakerEvent {
            event_type: CircuitBreakerEventType::Success,
            timestamp: now,
            service_name: self.service_name.clone(),
            endpoint_id: self.endpoint_id.clone(),
            metadata: None,
        }).await?;
        
        Ok(())
    }
    
    /// Record a failed request
    pub async fn record_failure(&self) -> Result<()> {
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        // Update metrics
        let mut metrics = self.metrics.write().await;
        metrics.failure_count += 1;
        metrics.consecutive_failures += 1;
        metrics.consecutive_successes = 0;
        metrics.recent_failures.push((now, "Request failed".to_string()));
        
        // Keep only the last 100 failures
        if metrics.recent_failures.len() > 100 {
            metrics.recent_failures.remove(0);
        }
        
        // Calculate success rate
        let total = metrics.success_count + metrics.failure_count;
        if total > 0 {
            metrics.success_rate = metrics.success_count as f64 / total as f64;
        }
        
        // Check if we should transition to open state
        let current_state = self.state.read().await.clone();
        let config = self.config.read().await.clone();
        
        if current_state == CircuitBreakerState::Closed && 
           metrics.consecutive_failures >= config.consecutive_errors_threshold {
            drop(metrics); // Release the lock before transition
            self.transition_to_state(CircuitBreakerState::Open).await?;
        } else if current_state == CircuitBreakerState::HalfOpen {
            // Any failure in half-open state should transition back to open
            drop(metrics); // Release the lock before transition
            self.transition_to_state(CircuitBreakerState::Open).await?;
        }
        
        // Save metrics to storage
        self.storage.update_metrics(
            &self.service_name,
            self.endpoint_id.as_deref(),
            self.metrics.read().await.clone(),
        ).await?;
        
        // Record failure event
        self.storage.record_event(CircuitBreakerEvent {
            event_type: CircuitBreakerEventType::Failure,
            timestamp: now,
            service_name: self.service_name.clone(),
            endpoint_id: self.endpoint_id.clone(),
            metadata: Some(HashMap::from([
                ("error".to_string(), "Request failed".to_string()),
            ])),
        }).await?;
        
        Ok(())
    }
    
    /// Record a rejected request (circuit is open)
    pub async fn record_rejected(&self) -> Result<()> {
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        // Update metrics
        let mut metrics = self.metrics.write().await;
        metrics.rejected_count += 1;
        
        // Save metrics to storage
        self.storage.update_metrics(
            &self.service_name,
            self.endpoint_id.as_deref(),
            metrics.clone(),
        ).await?;
        
        // Record rejected event
        self.storage.record_event(CircuitBreakerEvent {
            event_type: CircuitBreakerEventType::Rejected,
            timestamp: now,
            service_name: self.service_name.clone(),
            endpoint_id: self.endpoint_id.clone(),
            metadata: None,
        }).await?;
        
        Ok(())
    }
    
    /// Get current state
    pub async fn get_state(&self) -> CircuitBreakerState {
        self.state.read().await.clone()
    }
    
    /// Get current metrics
    pub async fn get_metrics(&self) -> CircuitBreakerMetrics {
        self.metrics.read().await.clone()
    }
    
    /// Get current configuration
    pub async fn get_config(&self) -> CircuitBreakerConfig {
        self.config.read().await.clone()
    }
    
    /// Update configuration
    pub async fn update_config(&self, config: CircuitBreakerConfig) -> Result<()> {
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        // Update config
        *self.config.write().await = config.clone();
        
        // Save config to storage
        self.storage.set_config(
            &self.service_name,
            self.endpoint_id.as_deref(),
            config,
        ).await?;
        
        // Record configuration change event
        self.storage.record_event(CircuitBreakerEvent {
            event_type: CircuitBreakerEventType::ConfigurationChanged,
            timestamp: now,
            service_name: self.service_name.clone(),
            endpoint_id: self.endpoint_id.clone(),
            metadata: None,
        }).await?;
        
        Ok(())
    }
    
    /// Reset the circuit breaker
    pub async fn reset(&self) -> Result<()> {
        // Transition to closed state
        self.transition_to_state(CircuitBreakerState::Closed).await?;
        
        // Reset metrics
        let mut metrics = self.metrics.write().await;
        *metrics = CircuitBreakerMetrics::default();
        metrics.current_state = CircuitBreakerState::Closed;
        metrics.last_state_change = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        // Save metrics to storage
        self.storage.update_metrics(
            &self.service_name,
            self.endpoint_id.as_deref(),
            metrics.clone(),
        ).await?;
        
        Ok(())
    }
    
    /// Force transition to a specific state
    pub async fn force_state(&self, state: CircuitBreakerState) -> Result<()> {
        self.transition_to_state(state).await
    }
    
    /// Get recent events
    pub async fn get_events(&self, limit: usize) -> Result<Vec<CircuitBreakerEvent>> {
        self.storage.get_events(&self.service_name, limit).await
    }
    
    /// Check if we should automatically transition from open to half-open
    async fn check_auto_transition_to_half_open(&self) {
        let state = self.state.read().await.clone();
        let config = self.config.read().await.clone();
        
        if state == CircuitBreakerState::Open && config.automatic_transition_from_open_to_half_open {
            let now = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis() as u64;
            
            let last_change = *self.last_state_change.read().await;
            
            if now - last_change > config.open_to_half_open_timeout_ms {
                // Time to try half-open
                if let Err(e) = self.transition_to_state(CircuitBreakerState::HalfOpen).await {
                    error!("Failed to transition to half-open state: {}", e);
                }
            }
        }
    }
    
    /// Transition to a new state
    async fn transition_to_state(&self, new_state: CircuitBreakerState) -> Result<()> {
        // Acquire lock to prevent concurrent state transitions
        let _lock = self.state_transition_lock.lock().await;
        
        let current_state = self.state.read().await.clone();
        
        // Skip if already in the target state
        if current_state == new_state {
            return Ok(());
        }
        
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        // Update state
        *self.state.write().await = new_state.clone();
        *self.last_state_change.write().await = now;
        
        // Update metrics
        let mut metrics = self.metrics.write().await;
        
        // Calculate time spent in previous state
        let time_spent = now - metrics.last_state_change;
        *metrics.time_in_state.entry(current_state.clone()).or_insert(0) += time_spent;
        
        metrics.current_state = new_state.clone();
        metrics.last_state_change = now;
        metrics.state_transition_count += 1;
        
        // Reset counters for half-open state
        if new_state == CircuitBreakerState::HalfOpen {
            metrics.half_open_calls = 0;
            metrics.consecutive_successes = 0;
        }
        
        // Save state and metrics to storage
        self.storage.set_state(
            &self.service_name,
            self.endpoint_id.as_deref(),
            new_state.clone(),
        ).await?;
        
        self.storage.update_metrics(
            &self.service_name,
            self.endpoint_id.as_deref(),
            metrics.clone(),
        ).await?;
        
        // Record state transition event
        self.storage.record_event(CircuitBreakerEvent {
            event_type: CircuitBreakerEventType::StateTransition {
                from: current_state.clone(),
                to: new_state.clone(),
            },
            timestamp: now,
            service_name: self.service_name.clone(),
            endpoint_id: self.endpoint_id.clone(),
            metadata: None,
        }).await?;
        
        // Log state transition
        info!(
            "Circuit breaker for service {} transitioned from {} to {}",
            self.service_name, current_state, new_state
        );
        
        Ok(())
    }
    
    /// Execute a function with circuit breaker protection
    pub async fn execute<F, Fut, T, E>(&self, f: F) -> Result<T, E>
    where
        F: FnOnce() -> Fut,
        Fut: std::future::Future<Output = Result<T, E>>,
        E: std::error::Error,
    {
        if !self.allow_request().await {
            // Circuit is open, record rejection
            if let Err(e) = self.record_rejected().await {
                error!("Failed to record rejected request: {}", e);
            }
            
            return Err(E::from_error("Circuit breaker is open"));
        }
        
        // Execute the function
        let result = f().await;
        
        // Record result
        match &result {
            Ok(_) => {
                if let Err(e) = self.record_success().await {
                    error!("Failed to record successful request: {}", e);
                }
            }
            Err(_) => {
                if let Err(e) = self.record_failure().await {
                    error!("Failed to record failed request: {}", e);
                }
            }
        }
        
        result
    }
}

/// Extension trait for error types
pub trait RetryableError {
    /// Create an error from a string
    fn from_error(msg: &str) -> Self;
}