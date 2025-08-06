// Package resilience provides resilience patterns for the Hivemind system.
package resilience

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// CircuitBreakerState represents the state of a circuit breaker
type CircuitBreakerState int

const (
	// CircuitBreakerStateClosed represents normal operation, requests are allowed
	CircuitBreakerStateClosed CircuitBreakerState = iota
	// CircuitBreakerStateOpen represents circuit is open, requests are rejected
	CircuitBreakerStateOpen
	// CircuitBreakerStateHalfOpen represents testing if the service is healthy again
	CircuitBreakerStateHalfOpen
)

// String returns the string representation of the circuit breaker state
func (s CircuitBreakerState) String() string {
	switch s {
	case CircuitBreakerStateClosed:
		return "closed"
	case CircuitBreakerStateOpen:
		return "open"
	case CircuitBreakerStateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// CircuitBreakerConfig defines the configuration for a circuit breaker
type CircuitBreakerConfig struct {
	// MaxConnections is the maximum number of concurrent connections
	MaxConnections uint32
	// MaxPendingRequests is the maximum number of pending requests
	MaxPendingRequests uint32
	// MaxRequests is the maximum number of requests per time window
	MaxRequests uint32
	// MaxRetries is the maximum number of retries
	MaxRetries uint32
	// ConsecutiveErrorsThreshold is the number of consecutive errors before opening the circuit
	ConsecutiveErrorsThreshold uint32
	// IntervalMs is the time window for error tracking in milliseconds
	IntervalMs int64
	// OpenToHalfOpenTimeoutMs is the time to keep the circuit open before transitioning to half-open in milliseconds
	OpenToHalfOpenTimeoutMs int64
	// HalfOpenSuccessThreshold is the number of successful calls required to close the circuit from half-open state
	HalfOpenSuccessThreshold uint32
	// HalfOpenAllowedCalls is the maximum number of calls allowed in half-open state
	HalfOpenAllowedCalls uint32
	// AutomaticTransitionFromOpenToHalfOpen indicates whether to enable automatic transition from open to half-open
	AutomaticTransitionFromOpenToHalfOpen bool
}

// DefaultCircuitBreakerConfig returns the default circuit breaker configuration
func DefaultCircuitBreakerConfig() *CircuitBreakerConfig {
	return &CircuitBreakerConfig{
		MaxConnections:                        100,
		MaxPendingRequests:                    10,
		MaxRequests:                           1000,
		MaxRetries:                            3,
		ConsecutiveErrorsThreshold:            5,
		IntervalMs:                            10000,
		OpenToHalfOpenTimeoutMs:               30000,
		HalfOpenSuccessThreshold:              5,
		HalfOpenAllowedCalls:                  5,
		AutomaticTransitionFromOpenToHalfOpen: true,
	}
}

// CircuitBreakerEventType represents the type of a circuit breaker event
type CircuitBreakerEventType int

const (
	// CircuitBreakerEventTypeStateTransition represents a state transition event
	CircuitBreakerEventTypeStateTransition CircuitBreakerEventType = iota
	// CircuitBreakerEventTypeSuccess represents a success event
	CircuitBreakerEventTypeSuccess
	// CircuitBreakerEventTypeFailure represents a failure event
	CircuitBreakerEventTypeFailure
	// CircuitBreakerEventTypeRejected represents a rejected event (circuit is open)
	CircuitBreakerEventTypeRejected
	// CircuitBreakerEventTypeConfigurationChanged represents a configuration change event
	CircuitBreakerEventTypeConfigurationChanged
)

// String returns the string representation of the circuit breaker event type
func (t CircuitBreakerEventType) String() string {
	switch t {
	case CircuitBreakerEventTypeStateTransition:
		return "state_transition"
	case CircuitBreakerEventTypeSuccess:
		return "success"
	case CircuitBreakerEventTypeFailure:
		return "failure"
	case CircuitBreakerEventTypeRejected:
		return "rejected"
	case CircuitBreakerEventTypeConfigurationChanged:
		return "configuration_changed"
	default:
		return "unknown"
	}
}

// CircuitBreakerEvent represents a circuit breaker event
type CircuitBreakerEvent struct {
	// EventType is the type of the event
	EventType CircuitBreakerEventType
	// Timestamp is the timestamp of the event
	Timestamp time.Time
	// ServiceName is the service name
	ServiceName string
	// EndpointID is the endpoint identifier (e.g., IP address)
	EndpointID string
	// Metadata is additional metadata
	Metadata map[string]string
	// FromState is the previous state (for state transition events)
	FromState CircuitBreakerState
	// ToState is the new state (for state transition events)
	ToState CircuitBreakerState
}

// CircuitBreakerMetrics contains metrics for a circuit breaker
type CircuitBreakerMetrics struct {
	// SuccessCount is the total number of successful requests
	SuccessCount uint64
	// FailureCount is the total number of failed requests
	FailureCount uint64
	// RejectedCount is the total number of rejected requests (circuit open)
	RejectedCount uint64
	// StateTransitionCount is the number of state transitions
	StateTransitionCount uint64
	// CurrentState is the current state
	CurrentState CircuitBreakerState
	// TimeInState is the time spent in each state (milliseconds)
	TimeInState map[CircuitBreakerState]int64
	// LastStateChange is the last state change timestamp
	LastStateChange time.Time
	// SuccessRate is the success rate (0.0 - 1.0)
	SuccessRate float64
	// AvgResponseTimeMs is the average response time (milliseconds)
	AvgResponseTimeMs float64
	// RecentFailures contains recent failures (timestamp -> error message)
	RecentFailures []struct {
		Timestamp time.Time
		Error     string
	}
	// RecentSuccesses contains recent successes (timestamp)
	RecentSuccesses []time.Time
	// ConsecutiveFailures is the number of consecutive failures
	ConsecutiveFailures uint32
	// ConsecutiveSuccesses is the number of consecutive successes
	ConsecutiveSuccesses uint32
	// HalfOpenCalls is the number of calls in half-open state
	HalfOpenCalls uint32
}

// CircuitBreakerStorage is an interface for circuit breaker storage
type CircuitBreakerStorage interface {
	// GetState gets the circuit breaker state
	GetState(ctx context.Context, serviceName string, endpointID string) (CircuitBreakerState, error)
	// SetState sets the circuit breaker state
	SetState(ctx context.Context, serviceName string, endpointID string, state CircuitBreakerState) error
	// GetMetrics gets the circuit breaker metrics
	GetMetrics(ctx context.Context, serviceName string, endpointID string) (*CircuitBreakerMetrics, error)
	// UpdateMetrics updates the circuit breaker metrics
	UpdateMetrics(ctx context.Context, serviceName string, endpointID string, metrics *CircuitBreakerMetrics) error
	// RecordEvent records a circuit breaker event
	RecordEvent(ctx context.Context, event *CircuitBreakerEvent) error
	// GetEvents gets the circuit breaker events
	GetEvents(ctx context.Context, serviceName string, limit int) ([]*CircuitBreakerEvent, error)
	// GetConfig gets the circuit breaker configuration
	GetConfig(ctx context.Context, serviceName string, endpointID string) (*CircuitBreakerConfig, error)
	// SetConfig sets the circuit breaker configuration
	SetConfig(ctx context.Context, serviceName string, endpointID string, config *CircuitBreakerConfig) error
}

// InMemoryCircuitBreakerStorage is an in-memory implementation of CircuitBreakerStorage
type InMemoryCircuitBreakerStorage struct {
	states  sync.Map // map[string]CircuitBreakerState
	metrics sync.Map // map[string]*CircuitBreakerMetrics
	events  sync.Map // map[string][]*CircuitBreakerEvent
	configs sync.Map // map[string]*CircuitBreakerConfig
}

// NewInMemoryCircuitBreakerStorage creates a new in-memory circuit breaker storage
func NewInMemoryCircuitBreakerStorage() *InMemoryCircuitBreakerStorage {
	return &InMemoryCircuitBreakerStorage{}
}

// generateKey generates a key for the storage
func (s *InMemoryCircuitBreakerStorage) generateKey(serviceName string, endpointID string) string {
	if endpointID != "" {
		return serviceName + ":" + endpointID
	}
	return serviceName
}

// GetState gets the circuit breaker state
func (s *InMemoryCircuitBreakerStorage) GetState(ctx context.Context, serviceName string, endpointID string) (CircuitBreakerState, error) {
	key := s.generateKey(serviceName, endpointID)
	if state, ok := s.states.Load(key); ok {
		return state.(CircuitBreakerState), nil
	}
	return CircuitBreakerStateClosed, nil
}

// SetState sets the circuit breaker state
func (s *InMemoryCircuitBreakerStorage) SetState(ctx context.Context, serviceName string, endpointID string, state CircuitBreakerState) error {
	key := s.generateKey(serviceName, endpointID)
	s.states.Store(key, state)
	return nil
}

// GetMetrics gets the circuit breaker metrics
func (s *InMemoryCircuitBreakerStorage) GetMetrics(ctx context.Context, serviceName string, endpointID string) (*CircuitBreakerMetrics, error) {
	key := s.generateKey(serviceName, endpointID)
	if metrics, ok := s.metrics.Load(key); ok {
		return metrics.(*CircuitBreakerMetrics), nil
	}
	return &CircuitBreakerMetrics{
		TimeInState: make(map[CircuitBreakerState]int64),
	}, nil
}

// UpdateMetrics updates the circuit breaker metrics
func (s *InMemoryCircuitBreakerStorage) UpdateMetrics(ctx context.Context, serviceName string, endpointID string, metrics *CircuitBreakerMetrics) error {
	key := s.generateKey(serviceName, endpointID)
	s.metrics.Store(key, metrics)
	return nil
}

// RecordEvent records a circuit breaker event
func (s *InMemoryCircuitBreakerStorage) RecordEvent(ctx context.Context, event *CircuitBreakerEvent) error {
	key := event.ServiceName
	var events []*CircuitBreakerEvent

	if eventsVal, ok := s.events.Load(key); ok {
		events = eventsVal.([]*CircuitBreakerEvent)
	}

	events = append(events, event)
	s.events.Store(key, events)
	return nil
}

// GetEvents gets the circuit breaker events
func (s *InMemoryCircuitBreakerStorage) GetEvents(ctx context.Context, serviceName string, limit int) ([]*CircuitBreakerEvent, error) {
	if eventsVal, ok := s.events.Load(serviceName); ok {
		events := eventsVal.([]*CircuitBreakerEvent)
		if len(events) <= limit {
			return events, nil
		}
		return events[len(events)-limit:], nil
	}
	return []*CircuitBreakerEvent{}, nil
}

// GetConfig gets the circuit breaker configuration
func (s *InMemoryCircuitBreakerStorage) GetConfig(ctx context.Context, serviceName string, endpointID string) (*CircuitBreakerConfig, error) {
	key := s.generateKey(serviceName, endpointID)
	if config, ok := s.configs.Load(key); ok {
		return config.(*CircuitBreakerConfig), nil
	}
	return nil, nil
}

// SetConfig sets the circuit breaker configuration
func (s *InMemoryCircuitBreakerStorage) SetConfig(ctx context.Context, serviceName string, endpointID string, config *CircuitBreakerConfig) error {
	key := s.generateKey(serviceName, endpointID)
	s.configs.Store(key, config)
	return nil
}

// CircuitBreaker implements the circuit breaker pattern
type CircuitBreaker struct {
	// ServiceName is the name of the service
	serviceName string
	// EndpointID is the endpoint identifier (e.g., IP address)
	endpointID string
	// Config is the circuit breaker configuration
	config atomic.Pointer[CircuitBreakerConfig]
	// State is the circuit breaker state
	state atomic.Int32
	// Metrics is the circuit breaker metrics
	metrics *CircuitBreakerMetrics
	// Storage is the circuit breaker storage
	storage CircuitBreakerStorage
	// LastStateChange is the last state change timestamp
	lastStateChange atomic.Int64
	// StateTransitionLock is the lock for state transitions
	stateTransitionLock sync.Mutex
	// Logger
	logger *zap.Logger
	// Mutex for protecting metrics updates
	mu sync.Mutex
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(serviceName string, config CircuitBreakerConfig) *CircuitBreaker {
	return NewCircuitBreakerWithStorage(serviceName, "", config, NewInMemoryCircuitBreakerStorage())
}

// NewCircuitBreakerWithEndpoint creates a new circuit breaker with endpoint ID
func NewCircuitBreakerWithEndpoint(serviceName string, endpointID string, config CircuitBreakerConfig) *CircuitBreaker {
	return NewCircuitBreakerWithStorage(serviceName, endpointID, config, NewInMemoryCircuitBreakerStorage())
}

// NewCircuitBreakerWithStorage creates a new circuit breaker with custom storage
func NewCircuitBreakerWithStorage(serviceName string, endpointID string, config CircuitBreakerConfig, storage CircuitBreakerStorage) *CircuitBreaker {
	logger, _ := zap.NewProduction()

	now := time.Now()

	cb := &CircuitBreaker{
		serviceName: serviceName,
		endpointID:  endpointID,
		metrics: &CircuitBreakerMetrics{
			CurrentState:    CircuitBreakerStateClosed,
			LastStateChange: now,
			TimeInState:     make(map[CircuitBreakerState]int64),
		},
		storage: storage,
		logger:  logger,
	}

	// Store config in atomic pointer
	cb.config.Store(&config)

	// Set initial state
	cb.state.Store(int32(CircuitBreakerStateClosed))

	// Set last state change
	cb.lastStateChange.Store(now.UnixNano())

	return cb
}

// Initialize initializes the circuit breaker from storage
func (cb *CircuitBreaker) Initialize(ctx context.Context) error {
	// Load state from storage
	state, err := cb.storage.GetState(ctx, cb.serviceName, cb.endpointID)
	if err == nil {
		cb.state.Store(int32(state))
	}

	// Load metrics from storage
	metrics, err := cb.storage.GetMetrics(ctx, cb.serviceName, cb.endpointID)
	if err == nil && metrics != nil {
		cb.mu.Lock()
		cb.metrics = metrics
		cb.mu.Unlock()
	}

	// Load config from storage
	config, err := cb.storage.GetConfig(ctx, cb.serviceName, cb.endpointID)
	if err == nil && config != nil {
		cb.config.Store(config)
	} else {
		// Save default config to storage
		err = cb.storage.SetConfig(ctx, cb.serviceName, cb.endpointID, cb.config.Load())
		if err != nil {
			return err
		}
	}

	return nil
}

// AllowRequest checks if a request is allowed
func (cb *CircuitBreaker) AllowRequest() bool {
	// Check if we need to transition from open to half-open
	cb.checkAutoTransitionToHalfOpen()

	// Get current state
	state := CircuitBreakerState(cb.state.Load())

	switch state {
	case CircuitBreakerStateClosed:
		return true
	case CircuitBreakerStateOpen:
		return false
	case CircuitBreakerStateHalfOpen:
		// In half-open state, allow limited requests
		cb.mu.Lock()
		defer cb.mu.Unlock()

		config := cb.config.Load()
		return cb.metrics.HalfOpenCalls < config.HalfOpenAllowedCalls
	default:
		return true
	}
}

// RecordSuccess records a successful request
func (cb *CircuitBreaker) RecordSuccess() {
	now := time.Now()

	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Update metrics
	cb.metrics.SuccessCount++
	cb.metrics.ConsecutiveFailures = 0
	cb.metrics.ConsecutiveSuccesses++
	cb.metrics.RecentSuccesses = append(cb.metrics.RecentSuccesses, now)

	// Keep only the last 100 successes
	if len(cb.metrics.RecentSuccesses) > 100 {
		cb.metrics.RecentSuccesses = cb.metrics.RecentSuccesses[1:]
	}

	// Calculate success rate
	total := cb.metrics.SuccessCount + cb.metrics.FailureCount
	if total > 0 {
		cb.metrics.SuccessRate = float64(cb.metrics.SuccessCount) / float64(total)
	}

	// Update half-open calls if in half-open state
	currentState := CircuitBreakerState(cb.state.Load())
	if currentState == CircuitBreakerStateHalfOpen {
		cb.metrics.HalfOpenCalls++

		// Check if we should transition to closed state
		config := cb.config.Load()
		if cb.metrics.ConsecutiveSuccesses >= config.HalfOpenSuccessThreshold {
			// Release the lock before transition
			cb.mu.Unlock()

			// Transition to closed state
			cb.transitionToState(CircuitBreakerStateClosed)

			// Re-acquire the lock
			cb.mu.Lock()
		}
	}

	// Save metrics to storage asynchronously
	go func() {
		ctx := context.Background()
		if err := cb.storage.UpdateMetrics(ctx, cb.serviceName, cb.endpointID, cb.metrics); err != nil {
			cb.logger.Error("Failed to update metrics", zap.Error(err))
		}

		// Record success event
		event := &CircuitBreakerEvent{
			EventType:   CircuitBreakerEventTypeSuccess,
			Timestamp:   now,
			ServiceName: cb.serviceName,
			EndpointID:  cb.endpointID,
		}

		if err := cb.storage.RecordEvent(ctx, event); err != nil {
			cb.logger.Error("Failed to record event", zap.Error(err))
		}
	}()
}

// RecordFailure records a failed request
func (cb *CircuitBreaker) RecordFailure() {
	now := time.Now()

	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Update metrics
	cb.metrics.FailureCount++
	cb.metrics.ConsecutiveFailures++
	cb.metrics.ConsecutiveSuccesses = 0
	cb.metrics.RecentFailures = append(cb.metrics.RecentFailures, struct {
		Timestamp time.Time
		Error     string
	}{
		Timestamp: now,
		Error:     "Request failed",
	})

	// Keep only the last 100 failures
	if len(cb.metrics.RecentFailures) > 100 {
		cb.metrics.RecentFailures = cb.metrics.RecentFailures[1:]
	}

	// Calculate success rate
	total := cb.metrics.SuccessCount + cb.metrics.FailureCount
	if total > 0 {
		cb.metrics.SuccessRate = float64(cb.metrics.SuccessCount) / float64(total)
	}

	// Check if we should transition to open state
	currentState := CircuitBreakerState(cb.state.Load())
	config := cb.config.Load()

	if currentState == CircuitBreakerStateClosed &&
		cb.metrics.ConsecutiveFailures >= config.ConsecutiveErrorsThreshold {
		// Release the lock before transition
		cb.mu.Unlock()

		// Transition to open state
		cb.transitionToState(CircuitBreakerStateOpen)

		// Re-acquire the lock
		cb.mu.Lock()
	} else if currentState == CircuitBreakerStateHalfOpen {
		// Any failure in half-open state should transition back to open
		// Release the lock before transition
		cb.mu.Unlock()

		// Transition to open state
		cb.transitionToState(CircuitBreakerStateOpen)

		// Re-acquire the lock
		cb.mu.Lock()
	}

	// Save metrics to storage asynchronously
	go func() {
		ctx := context.Background()
		if err := cb.storage.UpdateMetrics(ctx, cb.serviceName, cb.endpointID, cb.metrics); err != nil {
			cb.logger.Error("Failed to update metrics", zap.Error(err))
		}

		// Record failure event
		event := &CircuitBreakerEvent{
			EventType:   CircuitBreakerEventTypeFailure,
			Timestamp:   now,
			ServiceName: cb.serviceName,
			EndpointID:  cb.endpointID,
			Metadata: map[string]string{
				"error": "Request failed",
			},
		}

		if err := cb.storage.RecordEvent(ctx, event); err != nil {
			cb.logger.Error("Failed to record event", zap.Error(err))
		}
	}()
}

// RecordRejected records a rejected request (circuit is open)
func (cb *CircuitBreaker) RecordRejected() {
	now := time.Now()

	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Update metrics
	cb.metrics.RejectedCount++

	// Save metrics to storage asynchronously
	go func() {
		ctx := context.Background()
		if err := cb.storage.UpdateMetrics(ctx, cb.serviceName, cb.endpointID, cb.metrics); err != nil {
			cb.logger.Error("Failed to update metrics", zap.Error(err))
		}

		// Record rejected event
		event := &CircuitBreakerEvent{
			EventType:   CircuitBreakerEventTypeRejected,
			Timestamp:   now,
			ServiceName: cb.serviceName,
			EndpointID:  cb.endpointID,
		}

		if err := cb.storage.RecordEvent(ctx, event); err != nil {
			cb.logger.Error("Failed to record event", zap.Error(err))
		}
	}()
}

// GetState returns the current state
func (cb *CircuitBreaker) GetState() CircuitBreakerState {
	return CircuitBreakerState(cb.state.Load())
}

// GetMetrics returns the current metrics
func (cb *CircuitBreaker) GetMetrics() *CircuitBreakerMetrics {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Return a copy of the metrics
	metricsCopy := *cb.metrics
	return &metricsCopy
}

// GetConfig returns the current configuration
func (cb *CircuitBreaker) GetConfig() *CircuitBreakerConfig {
	return cb.config.Load()
}

// UpdateConfig updates the configuration
func (cb *CircuitBreaker) UpdateConfig(config CircuitBreakerConfig) {
	now := time.Now()

	// Update config
	cb.config.Store(&config)

	// Save config to storage asynchronously
	go func() {
		ctx := context.Background()
		if err := cb.storage.SetConfig(ctx, cb.serviceName, cb.endpointID, &config); err != nil {
			cb.logger.Error("Failed to update config", zap.Error(err))
		}

		// Record configuration change event
		event := &CircuitBreakerEvent{
			EventType:   CircuitBreakerEventTypeConfigurationChanged,
			Timestamp:   now,
			ServiceName: cb.serviceName,
			EndpointID:  cb.endpointID,
		}

		if err := cb.storage.RecordEvent(ctx, event); err != nil {
			cb.logger.Error("Failed to record event", zap.Error(err))
		}
	}()
}

// Reset resets the circuit breaker
func (cb *CircuitBreaker) Reset() {
	// Transition to closed state
	cb.transitionToState(CircuitBreakerStateClosed)

	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Reset metrics
	now := time.Now()
	cb.metrics = &CircuitBreakerMetrics{
		CurrentState:    CircuitBreakerStateClosed,
		LastStateChange: now,
		TimeInState:     make(map[CircuitBreakerState]int64),
	}

	// Save metrics to storage asynchronously
	go func() {
		ctx := context.Background()
		if err := cb.storage.UpdateMetrics(ctx, cb.serviceName, cb.endpointID, cb.metrics); err != nil {
			cb.logger.Error("Failed to update metrics", zap.Error(err))
		}
	}()
}

// ForceState forces transition to a specific state
func (cb *CircuitBreaker) ForceState(state CircuitBreakerState) {
	cb.transitionToState(state)
}

// GetEvents returns recent events
func (cb *CircuitBreaker) GetEvents(limit int) ([]*CircuitBreakerEvent, error) {
	ctx := context.Background()
	return cb.storage.GetEvents(ctx, cb.serviceName, limit)
}

// checkAutoTransitionToHalfOpen checks if we should automatically transition from open to half-open
func (cb *CircuitBreaker) checkAutoTransitionToHalfOpen() {
	state := CircuitBreakerState(cb.state.Load())
	config := cb.config.Load()

	if state == CircuitBreakerStateOpen && config.AutomaticTransitionFromOpenToHalfOpen {
		now := time.Now().UnixNano()
		lastChange := cb.lastStateChange.Load()

		if now-lastChange > config.OpenToHalfOpenTimeoutMs*int64(time.Millisecond) {
			// Time to try half-open
			cb.transitionToState(CircuitBreakerStateHalfOpen)
		}
	}
}

// transitionToState transitions to a new state
func (cb *CircuitBreaker) transitionToState(newState CircuitBreakerState) {
	// Acquire lock to prevent concurrent state transitions
	cb.stateTransitionLock.Lock()
	defer cb.stateTransitionLock.Unlock()

	currentState := CircuitBreakerState(cb.state.Load())

	// Skip if already in the target state
	if currentState == newState {
		return
	}

	now := time.Now()

	// Update state
	cb.state.Store(int32(newState))
	cb.lastStateChange.Store(now.UnixNano())

	cb.mu.Lock()

	// Calculate time spent in previous state
	timeSpent := now.Sub(cb.metrics.LastStateChange).Milliseconds()
	cb.metrics.TimeInState[currentState] += timeSpent

	cb.metrics.CurrentState = newState
	cb.metrics.LastStateChange = now
	cb.metrics.StateTransitionCount++

	// Reset counters for half-open state
	if newState == CircuitBreakerStateHalfOpen {
		cb.metrics.HalfOpenCalls = 0
		cb.metrics.ConsecutiveSuccesses = 0
	}

	cb.mu.Unlock()

	// Save state and metrics to storage asynchronously
	go func() {
		ctx := context.Background()
		if err := cb.storage.SetState(ctx, cb.serviceName, cb.endpointID, newState); err != nil {
			cb.logger.Error("Failed to update state", zap.Error(err))
		}

		if err := cb.storage.UpdateMetrics(ctx, cb.serviceName, cb.endpointID, cb.GetMetrics()); err != nil {
			cb.logger.Error("Failed to update metrics", zap.Error(err))
		}

		// Record state transition event
		event := &CircuitBreakerEvent{
			EventType:   CircuitBreakerEventTypeStateTransition,
			Timestamp:   now,
			ServiceName: cb.serviceName,
			EndpointID:  cb.endpointID,
			FromState:   currentState,
			ToState:     newState,
		}

		if err := cb.storage.RecordEvent(ctx, event); err != nil {
			cb.logger.Error("Failed to record event", zap.Error(err))
		}
	}()

	// Log state transition
	cb.logger.Info("Circuit breaker state transition",
		zap.String("service", cb.serviceName),
		zap.String("from", currentState.String()),
		zap.String("to", newState.String()))
}

// Execute executes a function with circuit breaker protection
func (cb *CircuitBreaker) Execute(ctx context.Context, f func(ctx context.Context) (interface{}, error)) (interface{}, error) {
	var zero interface{}

	if !cb.AllowRequest() {
		// Circuit is open, record rejection
		cb.RecordRejected()
		return zero, ErrCircuitBreakerOpen
	}

	// Execute the function
	result, err := f(ctx)

	// Record result
	if err != nil {
		cb.RecordFailure()
	} else {
		cb.RecordSuccess()
	}

	return result, err
}
