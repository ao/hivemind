// Package resilience provides resilience patterns for the Hivemind system.
package resilience

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ChaosTestingConfig defines the configuration for chaos testing
type ChaosTestingConfig struct {
	// FailureRate is the failure rate (0.0 - 1.0)
	FailureRate float64
	// LatencyMs is the latency injection in milliseconds
	LatencyMs int64
	// InjectLatency indicates whether to inject latency
	InjectLatency bool
	// InjectFailures indicates whether to inject failures
	InjectFailures bool
	// InjectTimeouts indicates whether to inject timeouts
	InjectTimeouts bool
	// TimeoutRate is the timeout rate (0.0 - 1.0)
	TimeoutRate float64
	// InjectConnectionErrors indicates whether to inject connection errors
	InjectConnectionErrors bool
	// ConnectionErrorRate is the connection error rate (0.0 - 1.0)
	ConnectionErrorRate float64
}

// DefaultChaosTestingConfig returns the default chaos testing configuration
func DefaultChaosTestingConfig() *ChaosTestingConfig {
	return &ChaosTestingConfig{
		FailureRate:            0.2,
		LatencyMs:              100,
		InjectLatency:          false,
		InjectFailures:         false,
		InjectTimeouts:         false,
		TimeoutRate:            0.1,
		InjectConnectionErrors: false,
		ConnectionErrorRate:    0.1,
	}
}

// CircuitBreakerTestResult contains the result of a circuit breaker test
type CircuitBreakerTestResult struct {
	// ServiceName is the service name
	ServiceName string
	// DurationMs is the test duration in milliseconds
	DurationMs int64
	// RequestCount is the number of requests
	RequestCount int64
	// SuccessCount is the number of successful requests
	SuccessCount int64
	// FailureCount is the number of failed requests
	FailureCount int64
	// RejectedCount is the number of rejected requests
	RejectedCount int64
	// StateTransitions is the number of state transitions
	StateTransitions uint64
	// FinalState is the final state
	FinalState CircuitBreakerState
	// TimeInState is the time spent in each state (milliseconds)
	TimeInState map[CircuitBreakerState]int64
	// AvgResponseTimeMs is the average response time (milliseconds)
	AvgResponseTimeMs float64
}

// CircuitBreakerTester is a tester for circuit breakers
type CircuitBreakerTester struct {
	// CircuitBreaker is the circuit breaker to test
	circuitBreaker *CircuitBreaker
	// Config is the chaos testing configuration
	config *ChaosTestingConfig
	// Random number generator
	rand *rand.Rand
	// Mutex for protecting config updates
	mu sync.Mutex
	// Logger
	logger *zap.Logger
}

// NewCircuitBreakerTester creates a new circuit breaker tester
func NewCircuitBreakerTester(circuitBreaker *CircuitBreaker, config ChaosTestingConfig) *CircuitBreakerTester {
	logger, _ := zap.NewProduction()

	return &CircuitBreakerTester{
		circuitBreaker: circuitBreaker,
		config:         &config,
		rand:           rand.New(rand.NewSource(time.Now().UnixNano())),
		logger:         logger,
	}
}

// UpdateConfig updates the chaos testing configuration
func (t *CircuitBreakerTester) UpdateConfig(config ChaosTestingConfig) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.config = &config
}

// RunTest runs a test with a specified number of requests
func (t *CircuitBreakerTester) RunTest(ctx context.Context, requestCount int64) (*CircuitBreakerTestResult, error) {
	startTime := time.Now()

	var successCount int64
	var failureCount int64
	var rejectedCount int64
	var totalResponseTime int64

	for i := int64(0); i < requestCount; i++ {
		requestStartTime := time.Now()

		// Check if request is allowed
		if !t.circuitBreaker.AllowRequest() {
			rejectedCount++
			t.circuitBreaker.RecordRejected()
			continue
		}

		// Simulate request
		err := t.simulateRequest(ctx)

		requestEndTime := time.Now()

		responseTime := requestEndTime.Sub(requestStartTime).Milliseconds()
		totalResponseTime += responseTime

		if err == nil {
			successCount++
			t.circuitBreaker.RecordSuccess()
		} else {
			failureCount++
			t.circuitBreaker.RecordFailure()
		}
	}

	endTime := time.Now()

	durationMs := endTime.Sub(startTime).Milliseconds()

	metrics := t.circuitBreaker.GetMetrics()

	return &CircuitBreakerTestResult{
		ServiceName:      "test-service",
		DurationMs:       durationMs,
		RequestCount:     requestCount,
		SuccessCount:     successCount,
		FailureCount:     failureCount,
		RejectedCount:    rejectedCount,
		StateTransitions: metrics.StateTransitionCount,
		FinalState:       t.circuitBreaker.GetState(),
		TimeInState:      metrics.TimeInState,
		AvgResponseTimeMs: func() float64 {
			if successCount+failureCount > 0 {
				return float64(totalResponseTime) / float64(successCount+failureCount)
			}
			return 0.0
		}(),
	}, nil
}

// simulateRequest simulates a request with chaos injection
func (t *CircuitBreakerTester) simulateRequest(ctx context.Context) error {
	t.mu.Lock()
	config := *t.config
	t.mu.Unlock()

	// Inject latency if enabled
	if config.InjectLatency {
		latency := time.Duration(config.LatencyMs) * time.Millisecond

		select {
		case <-time.After(latency):
			// Latency injected
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Inject connection errors if enabled
	if config.InjectConnectionErrors && t.rand.Float64() < config.ConnectionErrorRate {
		return errors.New("simulated connection error")
	}

	// Inject timeouts if enabled
	if config.InjectTimeouts && t.rand.Float64() < config.TimeoutRate {
		return errors.New("simulated timeout")
	}

	// Inject failures if enabled
	if config.InjectFailures && t.rand.Float64() < config.FailureRate {
		return errors.New("simulated failure")
	}

	return nil
}

// ResilienceTestSuite is a test suite for resilience patterns
type ResilienceTestSuite struct {
	// CircuitBreakerTester is the circuit breaker tester
	circuitBreakerTester *CircuitBreakerTester
	// Config is the chaos testing configuration
	config *ChaosTestingConfig
	// Mutex for protecting config updates
	mu sync.Mutex
	// Logger
	logger *zap.Logger
}

// NewResilienceTestSuite creates a new resilience test suite
func NewResilienceTestSuite() *ResilienceTestSuite {
	logger, _ := zap.NewProduction()

	return &ResilienceTestSuite{
		config: DefaultChaosTestingConfig(),
		logger: logger,
	}
}

// WithCircuitBreaker adds a circuit breaker tester to the test suite
func (s *ResilienceTestSuite) WithCircuitBreaker(circuitBreaker *CircuitBreaker) *ResilienceTestSuite {
	config := DefaultChaosTestingConfig()
	s.circuitBreakerTester = NewCircuitBreakerTester(circuitBreaker, *config)
	return s
}

// UpdateConfig updates the chaos testing configuration
func (s *ResilienceTestSuite) UpdateConfig(config ChaosTestingConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.config = &config

	if s.circuitBreakerTester != nil {
		s.circuitBreakerTester.UpdateConfig(config)
	}
}

// RunCircuitBreakerTest runs a circuit breaker test
func (s *ResilienceTestSuite) RunCircuitBreakerTest(ctx context.Context, requestCount int64) (*CircuitBreakerTestResult, error) {
	if s.circuitBreakerTester == nil {
		return nil, errors.New("circuit breaker tester not configured")
	}

	return s.circuitBreakerTester.RunTest(ctx, requestCount)
}

// VerifyCircuitBreakerBehavior verifies circuit breaker behavior
func (s *ResilienceTestSuite) VerifyCircuitBreakerBehavior(ctx context.Context) (bool, error) {
	if s.circuitBreakerTester == nil {
		return false, errors.New("circuit breaker tester not configured")
	}

	// Configure chaos to inject failures
	s.circuitBreakerTester.UpdateConfig(ChaosTestingConfig{
		FailureRate:    1.0, // 100% failure rate
		InjectFailures: true,
	})

	// Run test with enough requests to trigger circuit breaker
	result, err := s.circuitBreakerTester.RunTest(ctx, 10)
	if err != nil {
		return false, err
	}

	// Verify circuit breaker opened
	if result.FinalState != CircuitBreakerStateOpen {
		return false, nil
	}

	// Configure chaos to stop injecting failures
	s.circuitBreakerTester.UpdateConfig(ChaosTestingConfig{
		FailureRate:    0.0,
		InjectFailures: false,
	})

	// Wait for circuit breaker to transition to half-open
	select {
	case <-time.After(31 * time.Second):
		// Time passed
	case <-ctx.Done():
		return false, ctx.Err()
	}

	// Run test with enough requests to close circuit breaker
	result, err = s.circuitBreakerTester.RunTest(ctx, 10)
	if err != nil {
		return false, err
	}

	// Verify circuit breaker closed
	return result.FinalState == CircuitBreakerStateClosed, nil
}

// ResilienceTestReport contains a report of a resilience test
type ResilienceTestReport struct {
	// TestName is the test name
	TestName string
	// DurationMs is the test duration in milliseconds
	DurationMs int64
	// Success indicates the test result (success/failure)
	Success bool
	// Details contains test details
	Details map[string]string
	// CircuitBreakerResults contains circuit breaker test results
	CircuitBreakerResults *CircuitBreakerTestResult
}

// ResilienceTestRunner runs resilience tests
type ResilienceTestRunner struct {
	// TestSuites contains test suites
	testSuites map[string]*ResilienceTestSuite
	// TestReports contains test reports
	testReports []*ResilienceTestReport
	// Mutex for protecting test reports
	mu sync.Mutex
	// Logger
	logger *zap.Logger
}

// NewResilienceTestRunner creates a new resilience test runner
func NewResilienceTestRunner() *ResilienceTestRunner {
	logger, _ := zap.NewProduction()

	return &ResilienceTestRunner{
		testSuites:  make(map[string]*ResilienceTestSuite),
		testReports: make([]*ResilienceTestReport, 0),
		logger:      logger,
	}
}

// AddTestSuite adds a test suite
func (r *ResilienceTestRunner) AddTestSuite(name string, suite *ResilienceTestSuite) {
	r.testSuites[name] = suite
}

// RunAllTests runs all tests
func (r *ResilienceTestRunner) RunAllTests(ctx context.Context) ([]*ResilienceTestReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	reports := make([]*ResilienceTestReport, 0, len(r.testSuites))

	for name, suite := range r.testSuites {
		startTime := time.Now()

		success, err := suite.VerifyCircuitBreakerBehavior(ctx)

		endTime := time.Now()

		durationMs := endTime.Sub(startTime).Milliseconds()

		details := make(map[string]string)
		if err != nil {
			details["error"] = err.Error()
		}

		report := &ResilienceTestReport{
			TestName:   name,
			DurationMs: durationMs,
			Success:    err == nil && success,
			Details:    details,
		}

		reports = append(reports, report)
		r.testReports = append(r.testReports, report)
	}

	return reports, nil
}

// GetTestReports returns all test reports
func (r *ResilienceTestRunner) GetTestReports() []*ResilienceTestReport {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Return a copy of the reports
	reports := make([]*ResilienceTestReport, len(r.testReports))
	copy(reports, r.testReports)

	return reports
}

// ClearTestReports clears all test reports
func (r *ResilienceTestRunner) ClearTestReports() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.testReports = make([]*ResilienceTestReport, 0)
}

// CreateTestCircuitBreaker creates a circuit breaker for testing
func CreateTestCircuitBreaker(serviceName string) *CircuitBreaker {
	config := CircuitBreakerConfig{
		ConsecutiveErrorsThreshold: 5,
		OpenToHalfOpenTimeoutMs:    30000,
		HalfOpenSuccessThreshold:   3,
	}

	return NewCircuitBreaker(serviceName, config)
}

// CreateTestSuiteForService creates a resilience test suite for a service
func CreateTestSuiteForService(serviceName string) *ResilienceTestSuite {
	circuitBreaker := CreateTestCircuitBreaker(serviceName)

	return NewResilienceTestSuite().WithCircuitBreaker(circuitBreaker)
}
