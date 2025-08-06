// Package resilience provides resilience patterns for the Hivemind container orchestration system.
//
// This package implements several resilience patterns to improve the reliability and fault tolerance
// of the Hivemind system:
//
// - Circuit Breaker pattern: Prevents cascading failures by stopping calls to failing services
// - Bulkhead pattern: Isolates failures by limiting concurrent requests to services
// - Retry pattern: Automatically retries failed operations with exponential backoff
// - Fallback pattern: Provides alternative responses when services fail
//
// These patterns can be used individually or combined through the ResilienceManager
// to create a comprehensive resilience strategy for distributed systems.
package resilience

import (
	"context"
	"errors"
	"sync"

	"go.uber.org/zap"
)

// Common errors used throughout the resilience package
var (
	// ErrCircuitBreakerOpen is returned when a request is rejected because the circuit breaker is open
	ErrCircuitBreakerOpen = errors.New("circuit breaker is open")

	// ErrBulkheadFull is returned when a request is rejected because the bulkhead is full
	ErrBulkheadFull = errors.New("bulkhead is full")

	// ErrMaxRetriesReached is returned when the maximum number of retries has been reached
	ErrMaxRetriesReached = errors.New("maximum retries reached")

	// ErrMaxDurationReached is returned when the maximum retry duration has been reached
	ErrMaxDurationReached = errors.New("maximum retry duration reached")

	// ErrNoFallbackAvailable is returned when no fallback is available for a failed request
	ErrNoFallbackAvailable = errors.New("no fallback available")
)

// RetryableError is an interface for errors that can be retried.
// Implementations of this interface can specify whether they are retryable
// through the IsRetryable method.
type RetryableError interface {
	error
	// IsRetryable returns true if the error can be retried, false otherwise
	IsRetryable() bool
}

// RetryableFunc is a function type that can be retried.
// It takes a context and returns a value and an error.
// This is used by the retry policy to execute functions with retry logic.
type RetryableFunc func(ctx context.Context) (interface{}, error)

// FallbackFunc is a function type that provides a fallback value.
// It takes a context and an error (the original error) and returns a value and an error.
// This is used by the fallback policy to provide alternative values when the primary function fails.
type FallbackFunc func(ctx context.Context, err error) (interface{}, error)

// ResilienceManager coordinates all resilience patterns in a centralized way.
// It manages circuit breakers, bulkheads, retry policies, and fallback policies
// for different services, and provides a unified interface to apply these patterns.
//
// The ResilienceManager uses a service name-based approach to manage resilience components,
// allowing different services to have different resilience configurations.
type ResilienceManager struct {
	circuitBreakers  sync.Map // map[string]*CircuitBreaker
	bulkheads        sync.Map // map[string]*Bulkhead
	retryPolicies    sync.Map // map[string]*RetryPolicy
	fallbackPolicies sync.Map // map[string]*FallbackPolicy
	logger           *zap.Logger
}

// NewResilienceManager creates a new resilience manager with default configurations.
// The resilience manager is the main entry point for using resilience patterns in the application.
// It initializes with a logger and empty maps for circuit breakers, bulkheads, retry policies,
// and fallback policies.
//
// Example usage:
//
//	manager := resilience.NewResilienceManager()
//	result, err := manager.Execute(ctx, "my-service", func(ctx context.Context) (string, error) {
//	    return callExternalService(ctx)
//	})
func NewResilienceManager() *ResilienceManager {
	logger, _ := zap.NewProduction()
	return &ResilienceManager{
		logger: logger,
	}
}

// GetOrCreateCircuitBreaker gets or creates a circuit breaker for a service
func (m *ResilienceManager) GetOrCreateCircuitBreaker(serviceName string, config *CircuitBreakerConfig) *CircuitBreaker {
	if config == nil {
		config = DefaultCircuitBreakerConfig()
	}

	if cb, ok := m.circuitBreakers.Load(serviceName); ok {
		return cb.(*CircuitBreaker)
	}

	cb := NewCircuitBreaker(serviceName, *config)
	m.circuitBreakers.Store(serviceName, cb)

	m.logger.Info("Created circuit breaker",
		zap.String("service", serviceName),
		zap.Uint32("consecutive_errors_threshold", config.ConsecutiveErrorsThreshold),
		zap.Int64("open_to_half_open_timeout_ms", config.OpenToHalfOpenTimeoutMs))

	return cb
}

// GetOrCreateBulkhead gets or creates a bulkhead for a service
func (m *ResilienceManager) GetOrCreateBulkhead(serviceName string, config *BulkheadConfig) *Bulkhead {
	if config == nil {
		config = DefaultBulkheadConfig()
	}

	if b, ok := m.bulkheads.Load(serviceName); ok {
		return b.(*Bulkhead)
	}

	b := NewBulkhead(serviceName, *config)
	m.bulkheads.Store(serviceName, b)

	m.logger.Info("Created bulkhead",
		zap.String("service", serviceName),
		zap.Int64("max_concurrent_calls", config.MaxConcurrentCalls),
		zap.Int64("max_queue_size", config.MaxQueueSize))

	return b
}

// GetOrCreateRetryPolicy gets or creates a retry policy for a service
func (m *ResilienceManager) GetOrCreateRetryPolicy(serviceName string, config *ExponentialBackoffConfig) *RetryPolicy {
	if config == nil {
		config = DefaultExponentialBackoffConfig()
	}

	if rp, ok := m.retryPolicies.Load(serviceName); ok {
		return rp.(*RetryPolicy)
	}

	rp := NewRetryPolicy(serviceName, *config)
	m.retryPolicies.Store(serviceName, rp)

	m.logger.Info("Created retry policy",
		zap.String("service", serviceName),
		zap.Int64("initial_delay_ms", config.InitialDelayMs),
		zap.Uint32("max_retries", config.MaxRetries))

	return rp
}

// GetOrCreateFallbackPolicy gets or creates a fallback policy for a service
func (m *ResilienceManager) GetOrCreateFallbackPolicy(serviceName string, strategy FallbackStrategy) *FallbackPolicy {
	if fp, ok := m.fallbackPolicies.Load(serviceName); ok {
		return fp.(*FallbackPolicy)
	}

	// Use default strategy if none provided
	if strategy == nil {
		strategy = FallbackStrategyError{}
	}

	fp := NewFallbackPolicy(serviceName, strategy)
	m.fallbackPolicies.Store(serviceName, fp)

	m.logger.Info("Created fallback policy",
		zap.String("service", serviceName),
		zap.String("strategy", strategy.Type()))

	return fp
}

// Execute executes a function with all resilience patterns applied.
// This method applies circuit breaker, bulkhead, retry, and fallback patterns in sequence.
//
// The execution flow is as follows:
// 1. Check if the circuit breaker allows the request
// 2. Try to acquire permission from the bulkhead
// 3. Execute the function with retry logic
// 4. Record the result in the circuit breaker
// 5. Apply fallback if needed
//
// Parameters:
//   - ctx: The context for the operation, which can be used for cancellation
//   - serviceName: The name of the service, used to identify the resilience components
//   - f: The function to execute with resilience patterns
//
// Returns:
//   - The result of the function execution
//   - An error if any of the resilience patterns reject the request or if the function fails
//
// Example usage:
//
//	result, err := manager.Execute(ctx, "database-service", func(ctx context.Context) ([]User, error) {
//	    return db.QueryUsers(ctx)
//	})
func (m *ResilienceManager) Execute(ctx context.Context, serviceName string, f func(ctx context.Context) (interface{}, error)) (interface{}, error) {
	var zero interface{}

	// Get or create resilience components
	circuitBreaker := m.GetOrCreateCircuitBreaker(serviceName, nil)
	bulkhead := m.GetOrCreateBulkhead(serviceName, nil)
	retryPolicy := m.GetOrCreateRetryPolicy(serviceName, nil)
	fallbackPolicy := m.GetOrCreateFallbackPolicy(serviceName, FallbackStrategyError{})

	// Apply circuit breaker
	if !circuitBreaker.AllowRequest() {
		m.logger.Debug("Circuit breaker rejected request",
			zap.String("service", serviceName),
			zap.String("state", circuitBreaker.GetState().String()))

		// Record rejection
		circuitBreaker.RecordRejected()

		// Apply fallback
		return fallbackPolicy.Apply(ctx, func(ctx context.Context) (interface{}, error) {
			return zero, ErrCircuitBreakerOpen
		})
	}

	// Apply bulkhead
	if !bulkhead.AcquirePermission(ctx) {
		m.logger.Debug("Bulkhead rejected request",
			zap.String("service", serviceName))

		// Apply fallback
		return fallbackPolicy.Apply(ctx, func(ctx context.Context) (interface{}, error) {
			return zero, ErrBulkheadFull
		})
	}

	// Make sure to release bulkhead permission
	defer bulkhead.ReleasePermission()

	// Execute with retry
	result, err := retryPolicy.Execute(ctx, f)

	// Record circuit breaker result
	if err != nil {
		circuitBreaker.RecordFailure()

		m.logger.Debug("Request failed",
			zap.String("service", serviceName),
			zap.Error(err))

		// Apply fallback
		return fallbackPolicy.Apply(ctx, func(ctx context.Context) (interface{}, error) {
			return result, err
		})
	} else {
		circuitBreaker.RecordSuccess()

		m.logger.Debug("Request succeeded",
			zap.String("service", serviceName))
	}

	return result, err
}

// ExecuteWithOptions executes a function with specific resilience options.
// This method is similar to Execute, but allows specifying custom configurations
// for each resilience pattern.
//
// The execution flow is the same as Execute, but with custom configurations:
// 1. Check if the circuit breaker allows the request
// 2. Try to acquire permission from the bulkhead
// 3. Execute the function with retry logic
// 4. Record the result in the circuit breaker
// 5. Apply fallback if needed
//
// Parameters:
//   - ctx: The context for the operation, which can be used for cancellation
//   - serviceName: The name of the service, used to identify the resilience components
//   - f: The function to execute with resilience patterns
//   - circuitBreakerConfig: Custom configuration for the circuit breaker
//   - bulkheadConfig: Custom configuration for the bulkhead
//   - retryConfig: Custom configuration for the retry policy
//   - fallbackStrategy: Custom fallback strategy
//
// Returns:
//   - The result of the function execution
//   - An error if any of the resilience patterns reject the request or if the function fails
//
// Example usage:
//
//	result, err := manager.ExecuteWithOptions(
//	    ctx,
//	    "database-service",
//	    func(ctx context.Context) ([]User, error) {
//	        return db.QueryUsers(ctx)
//	    },
//	    &resilience.CircuitBreakerConfig{ConsecutiveErrorsThreshold: 10},
//	    &resilience.BulkheadConfig{MaxConcurrentCalls: 20},
//	    &resilience.ExponentialBackoffConfig{MaxRetries: 5},
//	    resilience.FallbackStrategyCache{MaxAgeMs: 60000},
//	)
func (m *ResilienceManager) ExecuteWithOptions(
	ctx context.Context,
	serviceName string,
	f func(ctx context.Context) (interface{}, error),
	circuitBreakerConfig *CircuitBreakerConfig,
	bulkheadConfig *BulkheadConfig,
	retryConfig *ExponentialBackoffConfig,
	fallbackStrategy FallbackStrategy,
) (interface{}, error) {
	var zero interface{}

	// Get or create resilience components with specific configurations
	circuitBreaker := m.GetOrCreateCircuitBreaker(serviceName, circuitBreakerConfig)
	bulkhead := m.GetOrCreateBulkhead(serviceName, bulkheadConfig)
	retryPolicy := m.GetOrCreateRetryPolicy(serviceName, retryConfig)
	fallbackPolicy := m.GetOrCreateFallbackPolicy(serviceName, fallbackStrategy)

	// Apply circuit breaker
	if !circuitBreaker.AllowRequest() {
		m.logger.Debug("Circuit breaker rejected request",
			zap.String("service", serviceName),
			zap.String("state", circuitBreaker.GetState().String()))

		// Record rejection
		circuitBreaker.RecordRejected()

		// Apply fallback
		return fallbackPolicy.Apply(ctx, func(ctx context.Context) (interface{}, error) {
			return zero, ErrCircuitBreakerOpen
		})
	}

	// Apply bulkhead
	if !bulkhead.AcquirePermission(ctx) {
		m.logger.Debug("Bulkhead rejected request",
			zap.String("service", serviceName))

		// Apply fallback
		return fallbackPolicy.Apply(ctx, func(ctx context.Context) (interface{}, error) {
			return zero, ErrBulkheadFull
		})
	}

	// Make sure to release bulkhead permission
	defer bulkhead.ReleasePermission()

	// Execute with retry
	result, err := retryPolicy.Execute(ctx, f)

	// Record circuit breaker result
	if err != nil {
		circuitBreaker.RecordFailure()

		m.logger.Debug("Request failed",
			zap.String("service", serviceName),
			zap.Error(err))

		// Apply fallback
		return fallbackPolicy.Apply(ctx, func(ctx context.Context) (interface{}, error) {
			return result, err
		})
	} else {
		circuitBreaker.RecordSuccess()

		m.logger.Debug("Request succeeded",
			zap.String("service", serviceName))
	}

	return result, err
}

// GetCircuitBreaker gets a circuit breaker by service name
func (m *ResilienceManager) GetCircuitBreaker(serviceName string) *CircuitBreaker {
	if cb, ok := m.circuitBreakers.Load(serviceName); ok {
		return cb.(*CircuitBreaker)
	}
	return nil
}

// GetBulkhead gets a bulkhead by service name
func (m *ResilienceManager) GetBulkhead(serviceName string) *Bulkhead {
	if b, ok := m.bulkheads.Load(serviceName); ok {
		return b.(*Bulkhead)
	}
	return nil
}

// GetRetryPolicy gets a retry policy by service name
func (m *ResilienceManager) GetRetryPolicy(serviceName string) *RetryPolicy {
	if rp, ok := m.retryPolicies.Load(serviceName); ok {
		return rp.(*RetryPolicy)
	}
	return nil
}

// GetFallbackPolicy gets a fallback policy by service name
func (m *ResilienceManager) GetFallbackPolicy(serviceName string) *FallbackPolicy {
	if fp, ok := m.fallbackPolicies.Load(serviceName); ok {
		return fp.(*FallbackPolicy)
	}
	return nil
}

// Reset resets all resilience components for a service.
// This method resets the circuit breaker, bulkhead, retry policy, and fallback policy
// for the specified service, clearing all metrics and state.
//
// This is useful in scenarios such as:
// - After a service has been redeployed or restarted
// - When you want to clear all metrics and start fresh
// - During testing to ensure a clean state between test cases
//
// Parameters:
//   - serviceName: The name of the service to reset
//
// Example usage:
//
//	// Reset all resilience components for the database service
//	manager.Reset("database-service")
func (m *ResilienceManager) Reset(serviceName string) {
	if cb := m.GetCircuitBreaker(serviceName); cb != nil {
		cb.Reset()
	}

	if b := m.GetBulkhead(serviceName); b != nil {
		b.Reset()
	}

	if rp := m.GetRetryPolicy(serviceName); rp != nil {
		rp.ResetMetrics()
	}

	if fp := m.GetFallbackPolicy(serviceName); fp != nil {
		fp.ResetMetrics()
		fp.ClearCache()
	}

	m.logger.Info("Reset all resilience components", zap.String("service", serviceName))
}
