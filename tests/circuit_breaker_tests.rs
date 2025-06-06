use anyhow::Result;
use hivemind::resilience::{
    CircuitBreaker, CircuitBreakerConfig, CircuitBreakerState, CircuitBreakerMetrics,
    ResilienceManager,
};
use std::sync::Arc;
use std::time::Duration;

#[tokio::test]
async fn test_circuit_breaker_basic_functionality() -> Result<()> {
    // Create a circuit breaker with custom configuration
    let config = CircuitBreakerConfig {
        consecutive_errors_threshold: 3,
        open_to_half_open_timeout_ms: 1000, // Short timeout for testing
        half_open_success_threshold: 2,
        ..CircuitBreakerConfig::default()
    };
    
    let circuit_breaker = CircuitBreaker::new("test-service".to_string(), config);
    
    // Initially, circuit should be closed
    assert_eq!(circuit_breaker.get_state().await, CircuitBreakerState::Closed);
    
    // Record failures to open the circuit
    for _ in 0..3 {
        circuit_breaker.record_failure().await?;
    }
    
    // Circuit should now be open
    assert_eq!(circuit_breaker.get_state().await, CircuitBreakerState::Open);
    
    // Request should be rejected
    assert!(!circuit_breaker.allow_request().await);
    
    // Wait for timeout to transition to half-open
    tokio::time::sleep(Duration::from_millis(1100)).await;
    
    // Circuit should now allow a limited number of requests
    assert!(circuit_breaker.allow_request().await);
    
    // Record successful requests to close the circuit
    for _ in 0..2 {
        circuit_breaker.record_success().await?;
    }
    
    // Circuit should now be closed
    assert_eq!(circuit_breaker.get_state().await, CircuitBreakerState::Closed);
    
    // Request should be allowed
    assert!(circuit_breaker.allow_request().await);
    
    Ok(())
}

#[tokio::test]
async fn test_circuit_breaker_metrics() -> Result<()> {
    // Create a circuit breaker
    let circuit_breaker = CircuitBreaker::new(
        "metrics-test-service".to_string(),
        CircuitBreakerConfig::default(),
    );
    
    // Record some successes and failures
    for _ in 0..5 {
        circuit_breaker.record_success().await?;
    }
    
    for _ in 0..3 {
        circuit_breaker.record_failure().await?;
    }
    
    // Get metrics
    let metrics = circuit_breaker.get_metrics().await;
    
    // Verify metrics
    assert_eq!(metrics.success_count, 5);
    assert_eq!(metrics.failure_count, 3);
    assert_eq!(metrics.consecutive_successes, 0);
    assert_eq!(metrics.consecutive_failures, 3);
    
    Ok(())
}

#[tokio::test]
async fn test_resilience_manager() -> Result<()> {
    // Create a resilience manager
    let resilience_manager = ResilienceManager::new();
    
    // Get or create circuit breakers for different services
    let service1_cb = resilience_manager.get_or_create_circuit_breaker("service1", None).await;
    let service2_cb = resilience_manager.get_or_create_circuit_breaker("service2", None).await;
    
    // Verify they are different instances
    assert_ne!(service1_cb.get_metrics().await.success_count, service2_cb.get_metrics().await.success_count);
    
    // Record success for service1
    service1_cb.record_success().await?;
    
    // Verify only service1 metrics changed
    assert_eq!(service1_cb.get_metrics().await.success_count, 1);
    assert_eq!(service2_cb.get_metrics().await.success_count, 0);
    
    // Get the same circuit breaker again
    let service1_cb_again = resilience_manager.get_or_create_circuit_breaker("service1", None).await;
    
    // Verify it's the same instance (state is preserved)
    assert_eq!(service1_cb_again.get_metrics().await.success_count, 1);
    
    Ok(())
}

#[tokio::test]
async fn test_circuit_breaker_reset() -> Result<()> {
    // Create a circuit breaker
    let circuit_breaker = CircuitBreaker::new(
        "reset-test-service".to_string(),
        CircuitBreakerConfig::default(),
    );
    
    // Record some failures to open the circuit
    for _ in 0..5 {
        circuit_breaker.record_failure().await?;
    }
    
    // Circuit should be open
    assert_eq!(circuit_breaker.get_state().await, CircuitBreakerState::Open);
    
    // Reset the circuit breaker
    circuit_breaker.reset().await?;
    
    // Circuit should be closed
    assert_eq!(circuit_breaker.get_state().await, CircuitBreakerState::Closed);
    
    // Metrics should be reset
    let metrics = circuit_breaker.get_metrics().await;
    assert_eq!(metrics.failure_count, 0);
    assert_eq!(metrics.success_count, 0);
    
    Ok(())
}

#[tokio::test]
async fn test_circuit_breaker_execute() -> Result<()> {
    // Create a circuit breaker
    let circuit_breaker = CircuitBreaker::new(
        "execute-test-service".to_string(),
        CircuitBreakerConfig::default(),
    );
    
    // Execute a successful function
    let result = circuit_breaker.execute(|| async {
        Ok::<_, anyhow::Error>(42)
    }).await;
    
    // Verify result
    assert_eq!(result.unwrap(), 42);
    
    // Verify metrics
    let metrics = circuit_breaker.get_metrics().await;
    assert_eq!(metrics.success_count, 1);
    
    // Execute a failing function
    let result = circuit_breaker.execute(|| async {
        Err::<i32, _>(anyhow::anyhow!("Test error"))
    }).await;
    
    // Verify result
    assert!(result.is_err());
    
    // Verify metrics
    let metrics = circuit_breaker.get_metrics().await;
    assert_eq!(metrics.success_count, 1);
    assert_eq!(metrics.failure_count, 1);
    
    Ok(())
}