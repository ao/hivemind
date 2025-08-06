// Package resilience provides resilience patterns for the Hivemind system.
package resilience

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"
)

// BulkheadConfig defines the configuration for a bulkhead
type BulkheadConfig struct {
	// MaxConcurrentCalls is the maximum number of concurrent requests
	MaxConcurrentCalls int64
	// MaxQueueSize is the maximum number of queued requests
	MaxQueueSize int64
	// AcquireTimeoutMs is the timeout for acquiring a permit (milliseconds)
	AcquireTimeoutMs int64
}

// DefaultBulkheadConfig returns the default bulkhead configuration
func DefaultBulkheadConfig() *BulkheadConfig {
	return &BulkheadConfig{
		MaxConcurrentCalls: 10,
		MaxQueueSize:       20,
		AcquireTimeoutMs:   1000,
	}
}

// BulkheadMetrics contains metrics for a bulkhead
type BulkheadMetrics struct {
	// SuccessfulCalls is the total number of successful requests
	SuccessfulCalls uint64
	// RejectedCalls is the total number of rejected requests
	RejectedCalls uint64
	// TimedOutCalls is the total number of timed out requests
	TimedOutCalls uint64
	// ActiveCalls is the current number of active requests
	ActiveCalls int64
	// QueuedCalls is the current number of queued requests
	QueuedCalls int64
	// MaxConcurrentCallsObserved is the maximum number of concurrent calls observed
	MaxConcurrentCallsObserved int64
	// MaxQueuedCallsObserved is the maximum number of queued calls observed
	MaxQueuedCallsObserved int64
	// AvgWaitTimeMs is the average wait time (milliseconds)
	AvgWaitTimeMs float64
	// AvgExecutionTimeMs is the average execution time (milliseconds)
	AvgExecutionTimeMs float64
}

// Bulkhead implements the bulkhead pattern to limit concurrent requests
type Bulkhead struct {
	// ServiceName is the name of the service
	serviceName string
	// Config is the bulkhead configuration
	config atomic.Pointer[BulkheadConfig]
	// Metrics is the bulkhead metrics
	metrics *BulkheadMetrics
	// Semaphore for limiting concurrent requests
	semaphore *semaphore.Weighted
	// Mutex for protecting metrics updates
	mu sync.Mutex
	// Logger
	logger *zap.Logger
}

// NewBulkhead creates a new bulkhead
func NewBulkhead(serviceName string, config BulkheadConfig) *Bulkhead {
	logger, _ := zap.NewProduction()

	b := &Bulkhead{
		serviceName: serviceName,
		metrics:     &BulkheadMetrics{},
		semaphore:   semaphore.NewWeighted(config.MaxConcurrentCalls),
		logger:      logger,
	}

	// Store config in atomic pointer
	b.config.Store(&config)

	return b
}

// AcquirePermission acquires permission to execute a request
func (b *Bulkhead) AcquirePermission(ctx context.Context) bool {
	startTime := time.Now()

	config := b.config.Load()

	// Create a context with timeout
	acquireCtx, cancel := context.WithTimeout(ctx, time.Duration(config.AcquireTimeoutMs)*time.Millisecond)
	defer cancel()

	// Try to acquire a permit with timeout
	err := b.semaphore.Acquire(acquireCtx, 1)

	waitTime := time.Since(startTime).Milliseconds()

	b.mu.Lock()
	defer b.mu.Unlock()

	if err == nil {
		// Successfully acquired a permit
		b.metrics.ActiveCalls++
		if b.metrics.QueuedCalls > 0 {
			b.metrics.QueuedCalls--
		}

		// Update max observed values
		if b.metrics.ActiveCalls > b.metrics.MaxConcurrentCallsObserved {
			b.metrics.MaxConcurrentCallsObserved = b.metrics.ActiveCalls
		}

		// Update average wait time
		totalCalls := b.metrics.SuccessfulCalls + b.metrics.RejectedCalls + b.metrics.TimedOutCalls
		if totalCalls > 0 {
			b.metrics.AvgWaitTimeMs = ((b.metrics.AvgWaitTimeMs * float64(totalCalls)) + float64(waitTime)) / float64(totalCalls+1)
		} else {
			b.metrics.AvgWaitTimeMs = float64(waitTime)
		}

		b.logger.Debug("Bulkhead acquired permission",
			zap.String("service", b.serviceName),
			zap.Int64("active", b.metrics.ActiveCalls),
			zap.Int64("max", config.MaxConcurrentCalls))

		return true
	} else if err == context.DeadlineExceeded {
		// Timeout waiting for a permit
		b.metrics.TimedOutCalls++

		b.logger.Warn("Bulkhead timed out waiting for permission",
			zap.String("service", b.serviceName),
			zap.Int64("timeout_ms", config.AcquireTimeoutMs))

		return false
	} else {
		// Failed to acquire a permit (context canceled or other error)
		b.metrics.RejectedCalls++

		b.logger.Warn("Bulkhead rejected request",
			zap.String("service", b.serviceName),
			zap.Error(err))

		return false
	}
}

// ReleasePermission releases permission after request completion
func (b *Bulkhead) ReleasePermission() {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Release a permit
	b.semaphore.Release(1)

	// Update metrics
	if b.metrics.ActiveCalls > 0 {
		b.metrics.ActiveCalls--
	}
	b.metrics.SuccessfulCalls++

	b.logger.Debug("Bulkhead released permission",
		zap.String("service", b.serviceName),
		zap.Int64("active", b.metrics.ActiveCalls))
}

// GetMetrics returns the current metrics
func (b *Bulkhead) GetMetrics() BulkheadMetrics {
	b.mu.Lock()
	defer b.mu.Unlock()

	return *b.metrics
}

// UpdateConfig updates the bulkhead configuration
func (b *Bulkhead) UpdateConfig(config BulkheadConfig) {
	oldConfig := b.config.Load()

	// Update the semaphore if max_concurrent_calls changed
	if config.MaxConcurrentCalls != oldConfig.MaxConcurrentCalls {
		diff := config.MaxConcurrentCalls - oldConfig.MaxConcurrentCalls

		if diff > 0 {
			// Add permits
			b.semaphore.Release(diff)
		} else if diff < 0 {
			// Try to remove permits (up to available)
			ctx := context.Background()
			if err := b.semaphore.Acquire(ctx, -diff); err == nil {
				// Successfully acquired permits to remove
				// We don't release them, effectively reducing the semaphore size
			}
		}
	}

	// Update config
	b.config.Store(&config)

	b.logger.Info("Bulkhead updated configuration",
		zap.String("service", b.serviceName),
		zap.Int64("max_concurrent_calls", config.MaxConcurrentCalls),
		zap.Int64("max_queue_size", config.MaxQueueSize),
		zap.Int64("acquire_timeout_ms", config.AcquireTimeoutMs))
}

// Reset resets the bulkhead
func (b *Bulkhead) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Reset metrics
	b.metrics = &BulkheadMetrics{}

	// Reset semaphore
	config := b.config.Load()

	// Create a new semaphore with the configured size
	b.semaphore = semaphore.NewWeighted(config.MaxConcurrentCalls)

	b.logger.Info("Bulkhead reset", zap.String("service", b.serviceName))
}

// Execute executes a function with bulkhead protection
func (b *Bulkhead) Execute(ctx context.Context, f func(ctx context.Context) (interface{}, error)) (interface{}, error) {
	var zero interface{}

	// Try to acquire permission
	if !b.AcquirePermission(ctx) {
		return zero, ErrBulkheadFull
	}

	startTime := time.Now()

	// Execute the function
	result, err := f(ctx)

	executionTime := time.Since(startTime).Milliseconds()

	// Update metrics
	b.mu.Lock()
	totalCalls := b.metrics.SuccessfulCalls + b.metrics.RejectedCalls + b.metrics.TimedOutCalls
	if totalCalls > 0 {
		b.metrics.AvgExecutionTimeMs = ((b.metrics.AvgExecutionTimeMs * float64(totalCalls)) + float64(executionTime)) / float64(totalCalls+1)
	} else {
		b.metrics.AvgExecutionTimeMs = float64(executionTime)
	}
	b.mu.Unlock()

	// Release permission
	b.ReleasePermission()

	return result, err
}
