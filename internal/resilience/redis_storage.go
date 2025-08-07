// Package resilience provides resilience patterns for the Hivemind system.
package resilience

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// RedisOptions defines the configuration for Redis connection
type RedisOptions struct {
	// Address is the Redis server address
	Address string
	// Password is the Redis server password
	Password string
	// DB is the Redis database number
	DB int
	// KeyPrefix is the prefix for all Redis keys
	KeyPrefix string
	// KeyExpiration is the expiration time for Redis keys
	KeyExpiration time.Duration
	// MaxRetries is the maximum number of retries for Redis operations
	MaxRetries int
	// RetryBackoff is the backoff duration between retries
	RetryBackoff time.Duration
	// DialTimeout is the timeout for establishing new connections
	DialTimeout time.Duration
	// ReadTimeout is the timeout for socket reads
	ReadTimeout time.Duration
	// WriteTimeout is the timeout for socket writes
	WriteTimeout time.Duration
	// PoolSize is the maximum number of socket connections
	PoolSize int
	// MinIdleConns is the minimum number of idle connections
	MinIdleConns int
	// IdleTimeout is the timeout for idle connections
	IdleTimeout time.Duration
	// LockTimeout is the timeout for distributed locks
	LockTimeout time.Duration
}

// DefaultRedisOptions returns the default Redis options
func DefaultRedisOptions() *RedisOptions {
	return &RedisOptions{
		Address:       "localhost:6379",
		Password:      "",
		DB:            0,
		KeyPrefix:     "circuit_breaker:",
		KeyExpiration: 24 * time.Hour,
		MaxRetries:    3,
		RetryBackoff:  100 * time.Millisecond,
		DialTimeout:   5 * time.Second,
		ReadTimeout:   3 * time.Second,
		WriteTimeout:  3 * time.Second,
		PoolSize:      10,
		MinIdleConns:  2,
		IdleTimeout:   5 * time.Minute,
		LockTimeout:   10 * time.Second,
	}
}

// RedisCircuitBreakerStorage is a Redis implementation of CircuitBreakerStorage
type RedisCircuitBreakerStorage struct {
	// client is the Redis client
	client redis.Cmdable
	// options is the Redis options
	options *RedisOptions
	// logger is the logger
	logger *zap.Logger
	// fallbackStorage is the in-memory fallback storage
	fallbackStorage *InMemoryCircuitBreakerStorage
	// instanceID is the unique identifier for this instance
	instanceID string
	// mutex for protecting operations
	mu sync.Mutex
}

// NewRedisCircuitBreakerStorage creates a new Redis circuit breaker storage
func NewRedisCircuitBreakerStorage(client redis.Cmdable, options *RedisOptions) *RedisCircuitBreakerStorage {
	logger, _ := zap.NewProduction()

	if options == nil {
		options = DefaultRedisOptions()
	}

	return &RedisCircuitBreakerStorage{
		client:          client,
		options:         options,
		logger:          logger,
		fallbackStorage: NewInMemoryCircuitBreakerStorage(),
		instanceID:      uuid.New().String(),
	}
}

// NewRedisCircuitBreakerStorageWithOptions creates a new Redis circuit breaker storage with options
func NewRedisCircuitBreakerStorageWithOptions(options *RedisOptions) *RedisCircuitBreakerStorage {
	if options == nil {
		options = DefaultRedisOptions()
	}

	client := redis.NewClient(&redis.Options{
		Addr:         options.Address,
		Password:     options.Password,
		DB:           options.DB,
		MaxRetries:   options.MaxRetries,
		DialTimeout:  options.DialTimeout,
		ReadTimeout:  options.ReadTimeout,
		WriteTimeout: options.WriteTimeout,
		PoolSize:     options.PoolSize,
		MinIdleConns: options.MinIdleConns,
		IdleTimeout:  options.IdleTimeout,
	})

	return NewRedisCircuitBreakerStorage(client, options)
}

// generateKey generates a key for the storage
func (s *RedisCircuitBreakerStorage) generateKey(serviceName string, endpointID string, suffix string) string {
	if endpointID != "" {
		return fmt.Sprintf("%s%s:%s:%s", s.options.KeyPrefix, serviceName, endpointID, suffix)
	}
	return fmt.Sprintf("%s%s:%s", s.options.KeyPrefix, serviceName, suffix)
}

// withRetry executes a function with retry
func (s *RedisCircuitBreakerStorage) withRetry(ctx context.Context, fn func() error) error {
	var err error
	for i := 0; i <= s.options.MaxRetries; i++ {
		// Execute the function
		err = fn()
		if err == nil {
			return nil
		}

		// If this is the last retry, break
		if i == s.options.MaxRetries {
			break
		}

		// Log retry
		s.logger.Debug("Retrying Redis operation",
			zap.Int("retry", i+1),
			zap.Int("max_retries", s.options.MaxRetries),
			zap.Error(err))

		// Wait before retrying
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.options.RetryBackoff):
		}
	}

	// Log error
	s.logger.Error("Redis operation failed after retries",
		zap.Int("max_retries", s.options.MaxRetries),
		zap.Error(err))

	return err
}

// GetState gets the circuit breaker state
func (s *RedisCircuitBreakerStorage) GetState(ctx context.Context, serviceName string, endpointID string) (CircuitBreakerState, error) {
	key := s.generateKey(serviceName, endpointID, "state")
	var state CircuitBreakerState

	err := s.withRetry(ctx, func() error {
		val, err := s.client.Get(ctx, key).Result()
		if err == redis.Nil {
			// Key does not exist, return default state
			state = CircuitBreakerStateClosed
			return nil
		} else if err != nil {
			return err
		}

		// Parse state
		var stateInt int
		if _, err := fmt.Sscanf(val, "%d", &stateInt); err != nil {
			return err
		}
		state = CircuitBreakerState(stateInt)
		return nil
	})

	if err != nil {
		// Fallback to in-memory storage
		s.logger.Warn("Falling back to in-memory storage for GetState",
			zap.String("service", serviceName),
			zap.String("endpoint", endpointID),
			zap.Error(err))
		return s.fallbackStorage.GetState(ctx, serviceName, endpointID)
	}

	return state, nil
}

// SetState sets the circuit breaker state
func (s *RedisCircuitBreakerStorage) SetState(ctx context.Context, serviceName string, endpointID string, state CircuitBreakerState) error {
	key := s.generateKey(serviceName, endpointID, "state")

	err := s.withRetry(ctx, func() error {
		return s.client.Set(ctx, key, int(state), s.options.KeyExpiration).Err()
	})

	if err != nil {
		// Fallback to in-memory storage
		s.logger.Warn("Falling back to in-memory storage for SetState",
			zap.String("service", serviceName),
			zap.String("endpoint", endpointID),
			zap.Error(err))
		err = s.fallbackStorage.SetState(ctx, serviceName, endpointID, state)
		if err != nil {
			return err
		}
	}

	// Create a new context for publishing to avoid using a potentially canceled context
	publishCtx := context.Background()
	// Publish state change event synchronously for tests to catch
	err = s.publishStateChange(publishCtx, serviceName, endpointID, state)
	if err != nil {
		s.logger.Error("Failed to publish state change",
			zap.String("service", serviceName),
			zap.String("endpoint", endpointID),
			zap.Error(err))
		// Don't return the error as this is not critical
	}

	return nil
}

// GetMetrics gets the circuit breaker metrics
func (s *RedisCircuitBreakerStorage) GetMetrics(ctx context.Context, serviceName string, endpointID string) (*CircuitBreakerMetrics, error) {
	key := s.generateKey(serviceName, endpointID, "metrics")
	var metrics *CircuitBreakerMetrics

	err := s.withRetry(ctx, func() error {
		val, err := s.client.Get(ctx, key).Result()
		if err == redis.Nil {
			// Key does not exist, return default metrics
			metrics = &CircuitBreakerMetrics{
				TimeInState: make(map[CircuitBreakerState]int64),
			}
			return nil
		} else if err != nil {
			return err
		}

		// Parse metrics
		metrics = &CircuitBreakerMetrics{
			TimeInState: make(map[CircuitBreakerState]int64),
		}
		if err := json.Unmarshal([]byte(val), metrics); err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		// Fallback to in-memory storage
		s.logger.Warn("Falling back to in-memory storage for GetMetrics",
			zap.String("service", serviceName),
			zap.String("endpoint", endpointID),
			zap.Error(err))
		return s.fallbackStorage.GetMetrics(ctx, serviceName, endpointID)
	}

	return metrics, nil
}

// UpdateMetrics updates the circuit breaker metrics
func (s *RedisCircuitBreakerStorage) UpdateMetrics(ctx context.Context, serviceName string, endpointID string, metrics *CircuitBreakerMetrics) error {
	key := s.generateKey(serviceName, endpointID, "metrics")

	err := s.withRetry(ctx, func() error {
		// Serialize metrics
		metricsJSON, err := json.Marshal(metrics)
		if err != nil {
			return err
		}

		return s.client.Set(ctx, key, metricsJSON, s.options.KeyExpiration).Err()
	})

	if err != nil {
		// Fallback to in-memory storage
		s.logger.Warn("Falling back to in-memory storage for UpdateMetrics",
			zap.String("service", serviceName),
			zap.String("endpoint", endpointID),
			zap.Error(err))
		return s.fallbackStorage.UpdateMetrics(ctx, serviceName, endpointID, metrics)
	}

	return nil
}

// RecordEvent records a circuit breaker event
func (s *RedisCircuitBreakerStorage) RecordEvent(ctx context.Context, event *CircuitBreakerEvent) error {
	key := s.generateKey(event.ServiceName, "", "events")

	err := s.withRetry(ctx, func() error {
		// Get existing events
		var events []*CircuitBreakerEvent
		val, err := s.client.Get(ctx, key).Result()
		if err == redis.Nil {
			// Key does not exist, create new events array
			events = make([]*CircuitBreakerEvent, 0)
		} else if err != nil {
			return err
		} else {
			// Parse events
			if err := json.Unmarshal([]byte(val), &events); err != nil {
				return err
			}
		}

		// Add new event
		events = append(events, event)

		// Keep only the last 100 events
		if len(events) > 100 {
			events = events[len(events)-100:]
		}

		// Serialize events
		eventsJSON, err := json.Marshal(events)
		if err != nil {
			return err
		}

		return s.client.Set(ctx, key, eventsJSON, s.options.KeyExpiration).Err()
	})

	if err != nil {
		// Fallback to in-memory storage
		s.logger.Warn("Falling back to in-memory storage for RecordEvent",
			zap.String("service", event.ServiceName),
			zap.String("endpoint", event.EndpointID),
			zap.Error(err))
		return s.fallbackStorage.RecordEvent(ctx, event)
	}

	return nil
}

// GetEvents gets the circuit breaker events
func (s *RedisCircuitBreakerStorage) GetEvents(ctx context.Context, serviceName string, limit int) ([]*CircuitBreakerEvent, error) {
	key := s.generateKey(serviceName, "", "events")
	var events []*CircuitBreakerEvent

	err := s.withRetry(ctx, func() error {
		val, err := s.client.Get(ctx, key).Result()
		if err == redis.Nil {
			// Key does not exist, return empty events
			events = make([]*CircuitBreakerEvent, 0)
			return nil
		} else if err != nil {
			return err
		}

		// Parse events
		if err := json.Unmarshal([]byte(val), &events); err != nil {
			return err
		}

		// Limit events
		if len(events) > limit {
			events = events[len(events)-limit:]
		}

		return nil
	})

	if err != nil {
		// Fallback to in-memory storage
		s.logger.Warn("Falling back to in-memory storage for GetEvents",
			zap.String("service", serviceName),
			zap.Error(err))
		return s.fallbackStorage.GetEvents(ctx, serviceName, limit)
	}

	return events, nil
}

// GetConfig gets the circuit breaker configuration
func (s *RedisCircuitBreakerStorage) GetConfig(ctx context.Context, serviceName string, endpointID string) (*CircuitBreakerConfig, error) {
	key := s.generateKey(serviceName, endpointID, "config")
	var config *CircuitBreakerConfig

	err := s.withRetry(ctx, func() error {
		val, err := s.client.Get(ctx, key).Result()
		if err == redis.Nil {
			// Key does not exist, return nil
			config = nil
			return nil
		} else if err != nil {
			return err
		}

		// Parse config
		config = &CircuitBreakerConfig{}
		if err := json.Unmarshal([]byte(val), config); err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		// Fallback to in-memory storage
		s.logger.Warn("Falling back to in-memory storage for GetConfig",
			zap.String("service", serviceName),
			zap.String("endpoint", endpointID),
			zap.Error(err))
		return s.fallbackStorage.GetConfig(ctx, serviceName, endpointID)
	}

	return config, nil
}

// SetConfig sets the circuit breaker configuration
func (s *RedisCircuitBreakerStorage) SetConfig(ctx context.Context, serviceName string, endpointID string, config *CircuitBreakerConfig) error {
	key := s.generateKey(serviceName, endpointID, "config")

	err := s.withRetry(ctx, func() error {
		// Serialize config
		configJSON, err := json.Marshal(config)
		if err != nil {
			return err
		}

		return s.client.Set(ctx, key, configJSON, s.options.KeyExpiration).Err()
	})

	if err != nil {
		// Fallback to in-memory storage
		s.logger.Warn("Falling back to in-memory storage for SetConfig",
			zap.String("service", serviceName),
			zap.String("endpoint", endpointID),
			zap.Error(err))
		return s.fallbackStorage.SetConfig(ctx, serviceName, endpointID, config)
	}

	return nil
}

// publishStateChange publishes a state change event
func (s *RedisCircuitBreakerStorage) publishStateChange(ctx context.Context, serviceName string, endpointID string, state CircuitBreakerState) error {
	// Create state change message
	stateChangeMsg := map[string]interface{}{
		"service":   serviceName,
		"endpoint":  endpointID,
		"state":     state,
		"timestamp": time.Now().UnixNano(),
		"instance":  s.instanceID,
	}

	// Serialize message
	msgJSON, err := json.Marshal(stateChangeMsg)
	if err != nil {
		s.logger.Error("Failed to serialize state change message",
			zap.String("service", serviceName),
			zap.String("endpoint", endpointID),
			zap.Error(err))
		return err
	}

	// Publish message
	channel := s.options.KeyPrefix + "state_changes"
	err = s.withRetry(ctx, func() error {
		return s.client.Publish(ctx, channel, string(msgJSON)).Err()
	})

	if err != nil {
		s.logger.Error("Failed to publish state change",
			zap.String("service", serviceName),
			zap.String("endpoint", endpointID),
			zap.Error(err))
		return err
	}

	return nil
}

// subscribeToStateChanges subscribes to state change events
func (s *RedisCircuitBreakerStorage) subscribeToStateChanges(ctx context.Context) (<-chan map[string]interface{}, error) {
	channel := s.options.KeyPrefix + "state_changes"
	// Use type assertion to get the Redis client that supports Subscribe
	redisClient, ok := s.client.(*redis.Client)
	if !ok {
		return nil, errors.New("client does not support Subscribe")
	}

	pubsub := redisClient.Subscribe(ctx, channel)

	// Check connection
	_, err := pubsub.Receive(ctx)
	if err != nil {
		return nil, err
	}

	// Create channel for state changes
	stateChanges := make(chan map[string]interface{}, 100)

	// Start goroutine to receive messages
	go func() {
		defer pubsub.Close()
		defer close(stateChanges)

		ch := pubsub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}

				// Parse message
				var stateChange map[string]interface{}
				if err := json.Unmarshal([]byte(msg.Payload), &stateChange); err != nil {
					s.logger.Error("Failed to parse state change message",
						zap.String("payload", msg.Payload),
						zap.Error(err))
					continue
				}

				// Skip own messages
				if instance, ok := stateChange["instance"].(string); ok && instance == s.instanceID {
					continue
				}

				// Send state change
				select {
				case <-ctx.Done():
					return
				case stateChanges <- stateChange:
				}
			}
		}
	}()

	return stateChanges, nil
}

// acquireLock acquires a distributed lock
func (s *RedisCircuitBreakerStorage) acquireLock(ctx context.Context, serviceName string, endpointID string, value string) (bool, error) {
	key := s.options.KeyPrefix + "locks:" + serviceName + ":" + endpointID

	var acquired bool
	err := s.withRetry(ctx, func() error {
		var err error
		acquired, err = s.client.SetNX(ctx, key, value, s.options.LockTimeout).Result()
		return err
	})

	if err != nil {
		return false, err
	}

	return acquired, nil
}

// releaseLock releases a distributed lock
func (s *RedisCircuitBreakerStorage) releaseLock(ctx context.Context, serviceName string, endpointID string, value string) (bool, error) {
	key := s.options.KeyPrefix + "locks:" + serviceName + ":" + endpointID

	// Use Lua script to ensure we only delete our own lock
	script := `if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) else return 0 end`

	var released bool
	err := s.withRetry(ctx, func() error {
		result, err := s.client.Eval(ctx, script, []string{key}, value).Int64()
		if err != nil {
			return err
		}
		released = result == 1
		return nil
	})

	if err != nil {
		return false, err
	}

	return released, nil
}

// becomeLeader attempts to become the leader for a service
func (s *RedisCircuitBreakerStorage) becomeLeader(ctx context.Context, serviceName string, instanceID string) (bool, error) {
	key := s.options.KeyPrefix + "leader:" + serviceName

	var isLeader bool
	err := s.withRetry(ctx, func() error {
		var err error
		isLeader, err = s.client.SetNX(ctx, key, instanceID, s.options.LockTimeout).Result()
		return err
	})

	if err != nil {
		return false, err
	}

	return isLeader, nil
}

// isLeader checks if the instance is the leader for a service
func (s *RedisCircuitBreakerStorage) isLeader(ctx context.Context, serviceName string, instanceID string) (bool, error) {
	key := s.options.KeyPrefix + "leader:" + serviceName

	var leader string
	err := s.withRetry(ctx, func() error {
		var err error
		leader, err = s.client.Get(ctx, key).Result()
		if err == redis.Nil {
			leader = ""
			return nil
		}
		return err
	})

	if err != nil {
		return false, err
	}

	return leader == instanceID, nil
}

// ExtendedCircuitBreakerStorage is an extension of CircuitBreakerStorage for distributed coordination
type ExtendedCircuitBreakerStorage interface {
	CircuitBreakerStorage

	// SubscribeToStateChanges subscribes to state change events
	SubscribeToStateChanges(ctx context.Context) (<-chan map[string]interface{}, error)

	// AcquireLock acquires a distributed lock
	AcquireLock(ctx context.Context, serviceName string, endpointID string, value string) (bool, error)

	// ReleaseLock releases a distributed lock
	ReleaseLock(ctx context.Context, serviceName string, endpointID string, value string) (bool, error)

	// BecomeLeader attempts to become the leader for a service
	BecomeLeader(ctx context.Context, serviceName string, instanceID string) (bool, error)

	// IsLeader checks if the instance is the leader for a service
	IsLeader(ctx context.Context, serviceName string, instanceID string) (bool, error)
}

// Ensure RedisCircuitBreakerStorage implements ExtendedCircuitBreakerStorage
var _ ExtendedCircuitBreakerStorage = (*RedisCircuitBreakerStorage)(nil)

// SubscribeToStateChanges subscribes to state change events
func (s *RedisCircuitBreakerStorage) SubscribeToStateChanges(ctx context.Context) (<-chan map[string]interface{}, error) {
	return s.subscribeToStateChanges(ctx)
}

// AcquireLock acquires a distributed lock
func (s *RedisCircuitBreakerStorage) AcquireLock(ctx context.Context, serviceName string, endpointID string, value string) (bool, error) {
	return s.acquireLock(ctx, serviceName, endpointID, value)
}

// ReleaseLock releases a distributed lock
func (s *RedisCircuitBreakerStorage) ReleaseLock(ctx context.Context, serviceName string, endpointID string, value string) (bool, error) {
	return s.releaseLock(ctx, serviceName, endpointID, value)
}

// BecomeLeader attempts to become the leader for a service
func (s *RedisCircuitBreakerStorage) BecomeLeader(ctx context.Context, serviceName string, instanceID string) (bool, error) {
	return s.becomeLeader(ctx, serviceName, instanceID)
}

// IsLeader checks if the instance is the leader for a service
func (s *RedisCircuitBreakerStorage) IsLeader(ctx context.Context, serviceName string, instanceID string) (bool, error) {
	return s.isLeader(ctx, serviceName, instanceID)
}

// ErrRedisUnavailable is returned when Redis is unavailable
var ErrRedisUnavailable = errors.New("redis unavailable")
