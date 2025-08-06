package resilience

import (
	"context"
)

// RetryOptions defines options for retry operations
type RetryOptions struct {
	MaxRetries     int
	InitialDelayMs int
	MaxDelayMs     int
	Multiplier     float64
	Jitter         bool
}

// CircuitBreakerOptions defines options for circuit breaker operations
type CircuitBreakerOptions struct {
	ConsecutiveErrorsThreshold int
	OpenToHalfOpenTimeoutMs    int
	HalfOpenSuccessThreshold   int
}

// BulkheadOptions defines options for bulkhead operations
type BulkheadOptions struct {
	MaxConcurrentCalls int
	MaxQueueSize       int
	AcquireTimeoutMs   int
}

// FallbackOptions defines options for fallback operations
type FallbackOptions struct {
	Strategy string
	Value    interface{}
}

// NewRetry creates a new retry policy with the given options
func NewRetry(options RetryOptions) *RetryPolicy {
	config := ExponentialBackoffConfig{
		MaxRetries:     uint32(options.MaxRetries),
		InitialDelayMs: int64(options.InitialDelayMs),
		MaxDelayMs:     int64(options.MaxDelayMs),
		Multiplier:     options.Multiplier,
		Jitter:         options.Jitter,
	}
	return NewRetryPolicy("default", config)
}

// CreateCircuitBreaker creates a new circuit breaker with the given options
func CreateCircuitBreaker(options CircuitBreakerOptions) *CircuitBreaker {
	config := CircuitBreakerConfig{
		ConsecutiveErrorsThreshold: uint32(options.ConsecutiveErrorsThreshold),
		OpenToHalfOpenTimeoutMs:    int64(options.OpenToHalfOpenTimeoutMs),
		HalfOpenSuccessThreshold:   uint32(options.HalfOpenSuccessThreshold),
	}
	return NewCircuitBreaker("default", config)
}

// CreateBulkhead creates a new bulkhead with the given options
func CreateBulkhead(options BulkheadOptions) *Bulkhead {
	config := BulkheadConfig{
		MaxConcurrentCalls: int64(options.MaxConcurrentCalls),
		MaxQueueSize:       int64(options.MaxQueueSize),
		AcquireTimeoutMs:   int64(options.AcquireTimeoutMs),
	}
	return NewBulkhead("default", config)
}

// NewFallback creates a new fallback policy with the given options
func NewFallback(options FallbackOptions) *FallbackPolicy {
	var strategy FallbackStrategy

	switch options.Strategy {
	case "static":
		strategy = FallbackStrategyStaticValue{Value: []byte(`"fallback value"`)}
	case "error":
		strategy = FallbackStrategyError{}
	default:
		strategy = FallbackStrategyError{}
	}

	return NewFallbackPolicy("default", strategy)
}

// Execute is a helper function to execute a function with resilience patterns
func Execute(ctx context.Context, f func(ctx context.Context) (interface{}, error)) (interface{}, error) {
	manager := NewResilienceManager()
	return manager.Execute(ctx, "default", f)
}
