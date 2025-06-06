//! Testing tools for resilience patterns
//!
//! This module provides tools for testing resilience patterns,
//! including circuit breakers, bulkheads, retries, and fallbacks.

use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::sync::{Mutex, RwLock};
use tracing::{debug, error, info, warn};

use super::circuit_breaker::{CircuitBreaker, CircuitBreakerConfig, CircuitBreakerState};
use super::bulkhead::Bulkhead;
use super::retry::RetryPolicy;
use super::fallback::FallbackPolicy;

/// Chaos testing configuration
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ChaosTestingConfig {
    /// Failure rate (0.0 - 1.0)
    pub failure_rate: f64,
    /// Latency injection in milliseconds
    pub latency_ms: u64,
    /// Whether to inject latency
    pub inject_latency: bool,
    /// Whether to inject failures
    pub inject_failures: bool,
    /// Whether to inject timeouts
    pub inject_timeouts: bool,
    /// Timeout rate (0.0 - 1.0)
    pub timeout_rate: f64,
    /// Whether to inject connection errors
    pub inject_connection_errors: bool,
    /// Connection error rate (0.0 - 1.0)
    pub connection_error_rate: f64,
}

impl Default for ChaosTestingConfig {
    fn default() -> Self {
        Self {
            failure_rate: 0.2,
            latency_ms: 100,
            inject_latency: false,
            inject_failures: false,
            inject_timeouts: false,
            timeout_rate: 0.1,
            inject_connection_errors: false,
            connection_error_rate: 0.1,
        }
    }
}

/// Circuit breaker test result
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct CircuitBreakerTestResult {
    /// Service name
    pub service_name: String,
    /// Test duration in milliseconds
    pub duration_ms: u64,
    /// Number of requests
    pub request_count: u64,
    /// Number of successful requests
    pub success_count: u64,
    /// Number of failed requests
    pub failure_count: u64,
    /// Number of rejected requests
    pub rejected_count: u64,
    /// Number of state transitions
    pub state_transitions: u64,
    /// Final state
    pub final_state: CircuitBreakerState,
    /// Time spent in each state (milliseconds)
    pub time_in_state: HashMap<CircuitBreakerState, u64>,
    /// Average response time (milliseconds)
    pub avg_response_time_ms: f64,
}

/// Circuit breaker tester
pub struct CircuitBreakerTester {
    /// Circuit breaker
    circuit_breaker: CircuitBreaker,
    /// Chaos testing configuration
    config: Arc<Mutex<ChaosTestingConfig>>,
    /// Random number generator
    rng: Arc<Mutex<rand::rngs::ThreadRng>>,
}

impl CircuitBreakerTester {
    /// Create a new circuit breaker tester
    pub fn new(circuit_breaker: CircuitBreaker, config: ChaosTestingConfig) -> Self {
        Self {
            circuit_breaker,
            config: Arc::new(Mutex::new(config)),
            rng: Arc::new(Mutex::new(rand::thread_rng())),
        }
    }
    
    /// Update chaos testing configuration
    pub async fn update_config(&self, config: ChaosTestingConfig) {
        let mut current_config = self.config.lock().await;
        *current_config = config;
    }
    
    /// Run a test with a specified number of requests
    pub async fn run_test(&self, request_count: u64) -> Result<CircuitBreakerTestResult> {
        let start_time = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        let mut success_count = 0;
        let mut failure_count = 0;
        let mut rejected_count = 0;
        let mut total_response_time = 0;
        
        for _ in 0..request_count {
            let request_start_time = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis() as u64;
            
            // Check if request is allowed
            if !self.circuit_breaker.allow_request().await {
                rejected_count += 1;
                self.circuit_breaker.record_rejected().await?;
                continue;
            }
            
            // Simulate request
            let result = self.simulate_request().await;
            
            let request_end_time = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis() as u64;
            
            let response_time = request_end_time - request_start_time;
            total_response_time += response_time;
            
            match result {
                Ok(_) => {
                    success_count += 1;
                    self.circuit_breaker.record_success().await?;
                }
                Err(_) => {
                    failure_count += 1;
                    self.circuit_breaker.record_failure().await?;
                }
            }
        }
        
        let end_time = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;
        
        let duration_ms = end_time - start_time;
        
        let metrics = self.circuit_breaker.get_metrics().await;
        
        Ok(CircuitBreakerTestResult {
            service_name: "test-service".to_string(),
            duration_ms,
            request_count,
            success_count,
            failure_count,
            rejected_count,
            state_transitions: metrics.state_transition_count,
            final_state: self.circuit_breaker.get_state().await,
            time_in_state: metrics.time_in_state,
            avg_response_time_ms: if success_count + failure_count > 0 {
                total_response_time as f64 / (success_count + failure_count) as f64
            } else {
                0.0
            },
        })
    }
    
    /// Simulate a request with chaos injection
    async fn simulate_request(&self) -> Result<(), anyhow::Error> {
        let config = self.config.lock().await.clone();
        let mut rng = self.rng.lock().await;
        
        // Inject latency if enabled
        if config.inject_latency {
            let latency = config.latency_ms;
            tokio::time::sleep(Duration::from_millis(latency)).await;
        }
        
        // Inject connection errors if enabled
        if config.inject_connection_errors && rand::Rng::gen_bool(&mut *rng, config.connection_error_rate) {
            return Err(anyhow::anyhow!("Simulated connection error"));
        }
        
        // Inject timeouts if enabled
        if config.inject_timeouts && rand::Rng::gen_bool(&mut *rng, config.timeout_rate) {
            return Err(anyhow::anyhow!("Simulated timeout"));
        }
        
        // Inject failures if enabled
        if config.inject_failures && rand::Rng::gen_bool(&mut *rng, config.failure_rate) {
            return Err(anyhow::anyhow!("Simulated failure"));
        }
        
        Ok(())
    }
}

/// Resilience test suite
pub struct ResilienceTestSuite {
    /// Circuit breaker tester
    circuit_breaker_tester: Option<CircuitBreakerTester>,
    /// Chaos testing configuration
    config: Arc<Mutex<ChaosTestingConfig>>,
}

impl ResilienceTestSuite {
    /// Create a new resilience test suite
    pub fn new() -> Self {
        Self {
            circuit_breaker_tester: None,
            config: Arc::new(Mutex::new(ChaosTestingConfig::default())),
        }
    }
    
    /// Set circuit breaker tester
    pub fn with_circuit_breaker(mut self, circuit_breaker: CircuitBreaker) -> Self {
        let config = ChaosTestingConfig::default();
        self.circuit_breaker_tester = Some(CircuitBreakerTester::new(circuit_breaker, config));
        self
    }
    
    /// Update chaos testing configuration
    pub async fn update_config(&self, config: ChaosTestingConfig) {
        let mut current_config = self.config.lock().await;
        *current_config = config.clone();
        
        if let Some(tester) = &self.circuit_breaker_tester {
            tester.update_config(config).await;
        }
    }
    
    /// Run a circuit breaker test
    pub async fn run_circuit_breaker_test(&self, request_count: u64) -> Result<CircuitBreakerTestResult> {
        if let Some(tester) = &self.circuit_breaker_tester {
            tester.run_test(request_count).await
        } else {
            Err(anyhow::anyhow!("Circuit breaker tester not configured"))
        }
    }
    
    /// Run a test scenario that verifies circuit breaker behavior
    pub async fn verify_circuit_breaker_behavior(&self) -> Result<bool> {
        if let Some(tester) = &self.circuit_breaker_tester {
            // Configure chaos to inject failures
            tester.update_config(ChaosTestingConfig {
                failure_rate: 1.0, // 100% failure rate
                inject_failures: true,
                ..ChaosTestingConfig::default()
            }).await;
            
            // Run test with enough requests to trigger circuit breaker
            let result = tester.run_test(10).await?;
            
            // Verify circuit breaker opened
            if result.final_state != CircuitBreakerState::Open {
                return Ok(false);
            }
            
            // Configure chaos to stop injecting failures
            tester.update_config(ChaosTestingConfig {
                failure_rate: 0.0,
                inject_failures: false,
                ..ChaosTestingConfig::default()
            }).await;
            
            // Wait for circuit breaker to transition to half-open
            tokio::time::sleep(Duration::from_millis(31000)).await;
            
            // Run test with enough requests to close circuit breaker
            let result = tester.run_test(10).await?;
            
            // Verify circuit breaker closed
            Ok(result.final_state == CircuitBreakerState::Closed)
        } else {
            Err(anyhow::anyhow!("Circuit breaker tester not configured"))
        }
    }
}

/// Resilience test report
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ResilienceTestReport {
    /// Test name
    pub test_name: String,
    /// Test duration in milliseconds
    pub duration_ms: u64,
    /// Test result (success/failure)
    pub success: bool,
    /// Test details
    pub details: HashMap<String, String>,
    /// Circuit breaker test results
    pub circuit_breaker_results: Option<CircuitBreakerTestResult>,
}

/// Resilience test runner
pub struct ResilienceTestRunner {
    /// Test suites
    test_suites: HashMap<String, ResilienceTestSuite>,
    /// Test reports
    test_reports: Arc<RwLock<Vec<ResilienceTestReport>>>,
}

impl ResilienceTestRunner {
    /// Create a new resilience test runner
    pub fn new() -> Self {
        Self {
            test_suites: HashMap::new(),
            test_reports: Arc::new(RwLock::new(Vec::new())),
        }
    }
    
    /// Add a test suite
    pub fn add_test_suite(&mut self, name: &str, suite: ResilienceTestSuite) {
        self.test_suites.insert(name.to_string(), suite);
    }
    
    /// Run all tests
    pub async fn run_all_tests(&self) -> Result<Vec<ResilienceTestReport>> {
        let mut reports = Vec::new();
        
        for (name, suite) in &self.test_suites {
            let start_time = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis() as u64;
            
            let result = suite.verify_circuit_breaker_behavior().await;
            
            let end_time = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis() as u64;
            
            let duration_ms = end_time - start_time;
            
            let success = result.is_ok() && result.unwrap();
            
            let mut details = HashMap::new();
            if let Err(e) = &result {
                details.insert("error".to_string(), e.to_string());
            }
            
            let report = ResilienceTestReport {
                test_name: name.clone(),
                duration_ms,
                success,
                details,
                circuit_breaker_results: None,
            };
            
            reports.push(report.clone());
            
            // Store report
            self.test_reports.write().await.push(report);
        }
        
        Ok(reports)
    }
    
    /// Get all test reports
    pub async fn get_test_reports(&self) -> Vec<ResilienceTestReport> {
        self.test_reports.read().await.clone()
    }
    
    /// Clear test reports
    pub async fn clear_test_reports(&self) {
        self.test_reports.write().await.clear();
    }
}

/// Create a circuit breaker for testing
pub fn create_test_circuit_breaker(service_name: &str) -> CircuitBreaker {
    let config = CircuitBreakerConfig {
        consecutive_errors_threshold: 5,
        open_to_half_open_timeout_ms: 30000,
        half_open_success_threshold: 3,
        ..CircuitBreakerConfig::default()
    };
    
    CircuitBreaker::new(service_name.to_string(), config)
}

/// Create a resilience test suite for a service
pub fn create_test_suite_for_service(service_name: &str) -> ResilienceTestSuite {
    let circuit_breaker = create_test_circuit_breaker(service_name);
    
    ResilienceTestSuite::new()
        .with_circuit_breaker(circuit_breaker)
}