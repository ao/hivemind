// Package resilience provides resilience patterns for the Hivemind system.
package resilience

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"
)

// FallbackStrategy defines the strategy for fallback
type FallbackStrategy interface {
	// Apply applies the fallback strategy
	Apply(ctx context.Context, err error, cache *FallbackCache) ([]byte, error)
	// Type returns the type of the fallback strategy
	Type() string
}

// FallbackStrategyStaticValue is a fallback strategy that returns a static value
type FallbackStrategyStaticValue struct {
	// Value is the static value to return
	Value []byte
}

// Apply applies the fallback strategy
func (s FallbackStrategyStaticValue) Apply(ctx context.Context, err error, cache *FallbackCache) ([]byte, error) {
	return s.Value, nil
}

// Type returns the type of the fallback strategy
func (s FallbackStrategyStaticValue) Type() string {
	return "static_value"
}

// FallbackStrategyCache is a fallback strategy that returns a cached value
type FallbackStrategyCache struct {
	// MaxAgeMs is the maximum age of cached value in milliseconds
	MaxAgeMs int64
}

// Apply applies the fallback strategy
func (s FallbackStrategyCache) Apply(ctx context.Context, err error, cache *FallbackCache) ([]byte, error) {
	if cache == nil || len(cache.Value) == 0 {
		return nil, ErrNoFallbackAvailable
	}

	// Check if cache is still valid
	now := time.Now().UnixNano() / int64(time.Millisecond)
	if now-cache.Timestamp > s.MaxAgeMs {
		return nil, ErrNoFallbackAvailable
	}

	return cache.Value, nil
}

// Type returns the type of the fallback strategy
func (s FallbackStrategyCache) Type() string {
	return "cache"
}

// FallbackStrategyAlternativeService is a fallback strategy that calls an alternative service
type FallbackStrategyAlternativeService struct {
	// ServiceName is the name of the alternative service
	ServiceName string
	// ServiceFunc is the function to call the alternative service
	ServiceFunc func(ctx context.Context) ([]byte, error)
}

// Apply applies the fallback strategy
func (s FallbackStrategyAlternativeService) Apply(ctx context.Context, err error, cache *FallbackCache) ([]byte, error) {
	if s.ServiceFunc == nil {
		return nil, ErrNoFallbackAvailable
	}

	return s.ServiceFunc(ctx)
}

// Type returns the type of the fallback strategy
func (s FallbackStrategyAlternativeService) Type() string {
	return "alternative_service"
}

// FallbackStrategyError is a fallback strategy that returns an error
type FallbackStrategyError struct{}

// Apply applies the fallback strategy
func (s FallbackStrategyError) Apply(ctx context.Context, err error, cache *FallbackCache) ([]byte, error) {
	return nil, err
}

// Type returns the type of the fallback strategy
func (s FallbackStrategyError) Type() string {
	return "error"
}

// FallbackMetrics contains metrics for a fallback policy
type FallbackMetrics struct {
	// TotalFallbacks is the total number of fallbacks
	TotalFallbacks uint64
	// SuccessfulFallbacks is the number of successful fallbacks
	SuccessfulFallbacks uint64
	// FailedFallbacks is the number of failed fallbacks
	FailedFallbacks uint64
	// StaticValueFallbacks is the number of static value fallbacks
	StaticValueFallbacks uint64
	// CacheFallbacks is the number of cache fallbacks
	CacheFallbacks uint64
	// AlternativeServiceFallbacks is the number of alternative service fallbacks
	AlternativeServiceFallbacks uint64
	// ErrorFallbacks is the number of error fallbacks
	ErrorFallbacks uint64
}

// FallbackCache is a cache for fallback values
type FallbackCache struct {
	// Value is the cached value
	Value []byte
	// Timestamp is the timestamp when the value was cached (milliseconds since epoch)
	Timestamp int64
}

// FallbackPolicy implements the fallback pattern
type FallbackPolicy struct {
	// ServiceName is the name of the service
	serviceName string
	// Strategy is the fallback strategy
	strategy FallbackStrategy
	// Metrics is the fallback metrics
	metrics *FallbackMetrics
	// Cache is the cache for fallback values
	cache *FallbackCache
	// Mutex for protecting metrics and cache updates
	mu sync.Mutex
	// Logger
	logger *zap.Logger
}

// NewFallbackPolicy creates a new fallback policy
func NewFallbackPolicy(serviceName string, strategy FallbackStrategy) *FallbackPolicy {
	if strategy == nil {
		strategy = FallbackStrategyError{}
	}

	logger, _ := zap.NewProduction()

	return &FallbackPolicy{
		serviceName: serviceName,
		strategy:    strategy,
		metrics:     &FallbackMetrics{},
		logger:      logger,
	}
}

// GetMetrics returns the current metrics
func (p *FallbackPolicy) GetMetrics() FallbackMetrics {
	p.mu.Lock()
	defer p.mu.Unlock()

	return *p.metrics
}

// UpdateStrategy updates the fallback strategy
func (p *FallbackPolicy) UpdateStrategy(strategy FallbackStrategy) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.strategy = strategy

	p.logger.Info("Fallback policy updated strategy",
		zap.String("service", p.serviceName),
		zap.String("strategy", strategy.Type()))
}

// ResetMetrics resets the metrics
func (p *FallbackPolicy) ResetMetrics() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.metrics = &FallbackMetrics{}

	p.logger.Info("Fallback policy reset metrics", zap.String("service", p.serviceName))
}

// ClearCache clears the cache
func (p *FallbackPolicy) ClearCache() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cache = nil

	p.logger.Info("Fallback policy cleared cache", zap.String("service", p.serviceName))
}

// UpdateCache updates the cache
func (p *FallbackPolicy) UpdateCache(value interface{}) error {
	serialized, err := json.Marshal(value)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.cache = &FallbackCache{
		Value:     serialized,
		Timestamp: time.Now().UnixNano() / int64(time.Millisecond),
	}

	p.logger.Debug("Fallback policy updated cache", zap.String("service", p.serviceName))

	return nil
}

// Apply applies the fallback strategy to a function result
func (p *FallbackPolicy) Apply(ctx context.Context, f func(ctx context.Context) (interface{}, error)) (interface{}, error) {
	var zero interface{}

	// Execute the function
	result, err := f(ctx)

	// If the result is Ok, return it and optionally update cache
	if err == nil {
		// Update cache if using cache strategy
		if _, ok := p.strategy.(FallbackStrategyCache); ok {
			if err := p.UpdateCache(result); err != nil {
				p.logger.Warn("Failed to update fallback cache", zap.Error(err))
			}
		}

		return result, nil
	}

	// Update metrics
	p.mu.Lock()
	p.metrics.TotalFallbacks++

	// Apply fallback strategy based on type
	switch s := p.strategy.(type) {
	case FallbackStrategyStaticValue:
		p.metrics.StaticValueFallbacks++
		p.mu.Unlock()

		p.logger.Warn("Fallback policy using static value strategy",
			zap.String("service", p.serviceName))

		// In a real implementation, this would return a static value
		// For now, we'll just return an error
		return zero, err

	case FallbackStrategyCache:
		p.metrics.CacheFallbacks++
		cache := p.cache
		p.mu.Unlock()

		if cache != nil {
			now := time.Now().UnixNano() / int64(time.Millisecond)

			// Check if cache is still valid
			if now-cache.Timestamp <= s.MaxAgeMs {
				// Deserialize cached value
				var cachedValue interface{}
				if err := json.Unmarshal(cache.Value, &cachedValue); err != nil {
					p.logger.Warn("Fallback policy failed to deserialize cached value",
						zap.String("service", p.serviceName),
						zap.Error(err))

					p.mu.Lock()
					p.metrics.FailedFallbacks++
					p.mu.Unlock()

					return zero, err
				}

				p.logger.Debug("Fallback policy using cached value",
					zap.String("service", p.serviceName))

				p.mu.Lock()
				p.metrics.SuccessfulFallbacks++
				p.mu.Unlock()

				return cachedValue, nil
			}

			p.logger.Warn("Fallback policy cache expired",
				zap.String("service", p.serviceName),
				zap.Int64("age_ms", now-cache.Timestamp),
				zap.Int64("max_ms", s.MaxAgeMs))
		} else {
			p.logger.Warn("Fallback policy has no cached value",
				zap.String("service", p.serviceName))
		}

		p.mu.Lock()
		p.metrics.FailedFallbacks++
		p.mu.Unlock()

		return zero, err

	case FallbackStrategyAlternativeService:
		p.metrics.AlternativeServiceFallbacks++
		p.mu.Unlock()

		p.logger.Warn("Fallback policy using alternative service",
			zap.String("service", p.serviceName),
			zap.String("alternative", s.ServiceName))

		// In a real implementation, this would call an alternative service
		// For now, we'll just log and return an error
		p.mu.Lock()
		p.metrics.FailedFallbacks++
		p.mu.Unlock()

		return zero, err

	default: // FallbackStrategyError or unknown
		p.metrics.ErrorFallbacks++
		p.metrics.FailedFallbacks++
		p.mu.Unlock()

		p.logger.Warn("Fallback policy using error strategy",
			zap.String("service", p.serviceName))

		return zero, err
	}
}

// ExecuteWithFallback executes a function with fallback
func (p *FallbackPolicy) ExecuteWithFallback(ctx context.Context, f func(ctx context.Context) (interface{}, error)) (interface{}, error) {
	return p.Apply(ctx, f)
}
