// Package resilience provides resilience patterns for the Hivemind system.
package resilience

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ExponentialBackoffConfig defines the configuration for exponential backoff
type ExponentialBackoffConfig struct {
	// InitialDelayMs is the initial delay in milliseconds
	InitialDelayMs int64
	// MaxDelayMs is the maximum delay in milliseconds
	MaxDelayMs int64
	// MaxRetries is the maximum number of retries
	MaxRetries uint32
	// Multiplier is the multiplier for each retry
	Multiplier float64
	// Jitter indicates whether to add jitter to the delay
	Jitter bool
	// MaxDurationMs is the maximum total duration for all retries in milliseconds
	MaxDurationMs *int64
}

// DefaultExponentialBackoffConfig returns the default exponential backoff configuration
func DefaultExponentialBackoffConfig() *ExponentialBackoffConfig {
	maxDuration := int64(30000)
	return &ExponentialBackoffConfig{
		InitialDelayMs: 100,
		MaxDelayMs:     10000,
		MaxRetries:     3,
		Multiplier:     2.0,
		Jitter:         true,
		MaxDurationMs:  &maxDuration,
	}
}

// RetryMetrics contains metrics for a retry policy
type RetryMetrics struct {
	// TotalRetryAttempts is the total number of retry attempts
	TotalRetryAttempts uint64
	// SuccessfulRetries is the total number of successful retries
	SuccessfulRetries uint64
	// FailedRetries is the total number of failed retries
	FailedRetries uint64
	// AvgDelayMs is the average delay between retries in milliseconds
	AvgDelayMs float64
	// MaxDelayObservedMs is the maximum delay observed in milliseconds
	MaxDelayObservedMs int64
	// AvgRetriesPerOperation is the average number of retries per operation
	AvgRetriesPerOperation float64
	// MaxRetriesObserved is the maximum number of retries observed for a single operation
	MaxRetriesObserved uint32
}

// RetryPolicy implements the retry pattern with exponential backoff
type RetryPolicy struct {
	// ServiceName is the name of the service
	serviceName string
	// Config is the retry configuration
	config *ExponentialBackoffConfig
	// Metrics is the retry metrics
	metrics *RetryMetrics
	// Mutex for protecting metrics updates
	mu sync.Mutex
	// Logger
	logger *zap.Logger
	// Random number generator for jitter
	rand *rand.Rand
}

// NewRetryPolicy creates a new retry policy
func NewRetryPolicy(serviceName string, config ExponentialBackoffConfig) *RetryPolicy {
	logger, _ := zap.NewProduction()

	return &RetryPolicy{
		serviceName: serviceName,
		config:      &config,
		metrics:     &RetryMetrics{},
		logger:      logger,
		rand:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// GetMetrics returns the current metrics
func (p *RetryPolicy) GetMetrics() RetryMetrics {
	p.mu.Lock()
	defer p.mu.Unlock()

	return *p.metrics
}

// UpdateConfig updates the retry configuration
func (p *RetryPolicy) UpdateConfig(config ExponentialBackoffConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.config = &config

	p.logger.Info("Retry policy updated configuration",
		zap.String("service", p.serviceName),
		zap.Int64("initial_delay_ms", config.InitialDelayMs),
		zap.Int64("max_delay_ms", config.MaxDelayMs),
		zap.Uint32("max_retries", config.MaxRetries))
}

// ResetMetrics resets the metrics
func (p *RetryPolicy) ResetMetrics() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.metrics = &RetryMetrics{}

	p.logger.Info("Retry policy reset metrics", zap.String("service", p.serviceName))
}

// calculateDelay calculates the delay for a retry attempt
func (p *RetryPolicy) calculateDelay(attempt uint32) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Calculate exponential delay
	baseDelayMs := float64(p.config.InitialDelayMs) * math.Pow(p.config.Multiplier, float64(attempt))

	// Cap at max delay
	cappedDelayMs := math.Min(baseDelayMs, float64(p.config.MaxDelayMs))

	// Add jitter if enabled
	finalDelayMs := cappedDelayMs
	if p.config.Jitter {
		jitterFactor := 0.8 + p.rand.Float64()*0.4 // Random value between 0.8 and 1.2
		finalDelayMs = cappedDelayMs * jitterFactor
	}

	// Update metrics
	if int64(finalDelayMs) > p.metrics.MaxDelayObservedMs {
		p.metrics.MaxDelayObservedMs = int64(finalDelayMs)
	}

	// Update average delay
	totalAttempts := p.metrics.TotalRetryAttempts
	if totalAttempts > 0 {
		p.metrics.AvgDelayMs = ((p.metrics.AvgDelayMs * float64(totalAttempts)) + finalDelayMs) / float64(totalAttempts+1)
	} else {
		p.metrics.AvgDelayMs = finalDelayMs
	}

	return time.Duration(finalDelayMs) * time.Millisecond
}

// Execute executes a function with retry
func (p *RetryPolicy) Execute(ctx context.Context, f func(ctx context.Context) (interface{}, error)) (interface{}, error) {
	var zero interface{}

	config := *p.config
	startTime := time.Now()
	var attempt uint32
	var lastErr error

	for {
		// Check if we've exceeded max retries
		if attempt > config.MaxRetries {
			p.mu.Lock()
			p.metrics.FailedRetries++
			p.mu.Unlock()

			p.logger.Warn("Retry policy exceeded maximum retries",
				zap.String("service", p.serviceName),
				zap.Uint32("max_retries", config.MaxRetries))

			if lastErr != nil {
				return zero, lastErr
			}
			return zero, ErrMaxRetriesReached
		}

		// Check if we've exceeded max duration
		if config.MaxDurationMs != nil {
			elapsed := time.Since(startTime).Milliseconds()
			if elapsed > *config.MaxDurationMs {
				p.mu.Lock()
				p.metrics.FailedRetries++
				p.mu.Unlock()

				p.logger.Warn("Retry policy exceeded maximum duration",
					zap.String("service", p.serviceName),
					zap.Int64("max_duration_ms", *config.MaxDurationMs))

				return zero, ErrMaxDurationReached
			}
		}

		// Execute the function
		result, err := f(ctx)

		if err == nil {
			// Success, update metrics and return
			p.mu.Lock()

			if attempt > 0 {
				p.metrics.SuccessfulRetries++

				// Update average retries per operation
				totalOperations := p.metrics.SuccessfulRetries + p.metrics.FailedRetries
				if totalOperations > 0 {
					p.metrics.AvgRetriesPerOperation = float64(p.metrics.TotalRetryAttempts) / float64(totalOperations)
				}

				p.logger.Debug("Retry policy succeeded",
					zap.String("service", p.serviceName),
					zap.Uint32("attempts", attempt+1))
			}

			p.mu.Unlock()

			return result, nil
		}

		// Check if the error is retryable
		var retryable bool
		if re, ok := err.(RetryableError); ok {
			retryable = re.IsRetryable()
		} else {
			// Default to retryable
			retryable = true
		}

		if !retryable {
			// Non-retryable error, update metrics and return
			p.mu.Lock()
			p.metrics.FailedRetries++
			p.mu.Unlock()

			p.logger.Warn("Retry policy encountered non-retryable error",
				zap.String("service", p.serviceName),
				zap.Error(err))

			return zero, err
		}

		// Update metrics
		p.mu.Lock()
		p.metrics.TotalRetryAttempts++

		// Update max retries observed
		if attempt+1 > p.metrics.MaxRetriesObserved {
			p.metrics.MaxRetriesObserved = attempt + 1
		}
		p.mu.Unlock()

		// Store the error for potential return
		lastErr = err

		// Increment attempt counter
		attempt++

		// Calculate delay for next retry
		delay := p.calculateDelay(attempt)

		p.logger.Debug("Retry policy failed attempt, retrying",
			zap.String("service", p.serviceName),
			zap.Uint32("attempt", attempt),
			zap.Uint32("max_retries", config.MaxRetries),
			zap.Int64("delay_ms", delay.Milliseconds()))

		// Create a timer for the delay
		timer := time.NewTimer(delay)

		// Wait for either the delay to expire or the context to be canceled
		select {
		case <-timer.C:
			// Continue with the next retry
		case <-ctx.Done():
			// Context was canceled
			timer.Stop()

			p.mu.Lock()
			p.metrics.FailedRetries++
			p.mu.Unlock()

			p.logger.Warn("Retry policy context canceled",
				zap.String("service", p.serviceName),
				zap.Error(ctx.Err()))

			return zero, ctx.Err()
		}
	}
}

// StandardRetryableError is a standard implementation of RetryableError
type StandardRetryableError struct {
	error
	retryable bool
}

// IsRetryable returns whether the error is retryable
func (e StandardRetryableError) IsRetryable() bool {
	return e.retryable
}

// NewRetryableError creates a new retryable error
func NewRetryableError(err error, retryable bool) RetryableError {
	return StandardRetryableError{
		error:     err,
		retryable: retryable,
	}
}

// IsNetworkError checks if an error is a network-related error
func IsNetworkError(err error) bool {
	// In a real implementation, this would check for specific network error types
	// For now, we'll just return true for demonstration purposes
	return true
}

// IsTimeoutError checks if an error is a timeout error
func IsTimeoutError(err error) bool {
	// In a real implementation, this would check for specific timeout error types
	// For now, we'll just return true for demonstration purposes
	return true
}

// IsRetryableError checks if an error is retryable
func IsRetryableError(err error) bool {
	// Check if the error implements RetryableError
	if re, ok := err.(RetryableError); ok {
		return re.IsRetryable()
	}

	// Check for common retryable errors
	return IsNetworkError(err) || IsTimeoutError(err)
}
