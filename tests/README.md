# Hivemind Testing Framework

This directory contains comprehensive tests for the Hivemind container orchestration system. The tests are organized into different categories to validate various aspects of the system.

## Test Categories

### 1. Unit Tests
- `unit_tests.rs` - Basic unit tests for individual components
- `scheduler_tests.rs` - Tests for the container scheduler
- `network_tests.rs` - Tests for the networking subsystem
- `membership_tests.rs` - Tests for the membership protocol
- `health_monitor_tests.rs` - Tests for the health monitoring system

### 2. Integration Tests
- `integration_tests.rs` - End-to-end integration tests for the entire system
- `service_discovery_integration_tests.rs` - Integration tests for service discovery
- `membership_integration_tests.rs` - Integration tests for membership functionality
- `node_membership_integration_tests.rs` - Integration tests for node membership

### 3. Performance Tests
- `performance_tests.rs` - Basic performance tests for various components
- `cluster_performance_tests.rs` - Performance tests for cluster operations at scale

### 4. Chaos Tests
- `chaos_tests.rs` - Basic chaos tests for system resilience
- `enhanced_chaos_tests.rs` - Advanced chaos tests for complex failure scenarios

### 5. Security Tests
- `security_tests.rs` - Tests for security features and vulnerabilities

### 6. Test Infrastructure
- `test_runner.rs` - Test runner for executing all tests
- `coverage_reporter.rs` - Tool for generating test coverage reports

## Running Tests

### Running All Tests

To run all tests with coverage reporting:

```bash
cargo run --bin test_runner
```

### Running Specific Test Categories

To run specific test categories:

```bash
# Run unit tests
cargo test --test unit_tests --test scheduler_tests --test network_tests --test membership_tests --test health_monitor_tests

# Run integration tests
cargo test --test integration_tests --test service_discovery_integration_tests --test membership_integration_tests --test node_membership_integration_tests

# Run performance tests
cargo test --test performance_tests --test cluster_performance_tests

# Run chaos tests
cargo test --test chaos_tests --test enhanced_chaos_tests

# Run security tests
cargo test --test security_tests
```

### Running Individual Tests

To run a specific test:

```bash
cargo test --test integration_tests test_full_application_deployment_workflow
```

## Coverage Reporting

To generate a test coverage report:

```bash
cargo run --bin coverage_reporter
```

This will generate coverage reports in the `test_results` directory:
- `test_results/coverage_report.txt` - Text-based coverage report
- `test_results/html/index.html` - HTML coverage report with visualizations

## Test Features

### End-to-End Integration Tests

The integration tests validate the entire system working together, including:
- Container lifecycle (create, run, network, volume mount, service discovery, health check, restart, delete)
- Cluster management (node membership, leader election, distributed state)
- Network policy application and enforcement
- Service discovery and load balancing
- Volume management and persistence

### Performance Tests

Performance tests ensure the system meets performance requirements:
- Scheduler performance with different bin packing strategies
- Service discovery performance under load
- Network operations performance
- Distributed state operations performance
- Cluster scaling performance

### Chaos Tests

Chaos tests validate system resilience under various failure conditions:
- Node failures and recovery
- Container failures and auto-healing
- Network partitions and recovery
- Leader failures and re-election
- Resource exhaustion and recovery
- Random failures and system stability

### Regression Tests

Regression tests prevent future regressions by validating core functionality:
- Container lifecycle operations
- Network policy application and enforcement
- Health monitoring and auto-healing
- Distributed state consistency

## Adding New Tests

When adding new tests:

1. Place the test in the appropriate category file
2. Ensure the test is properly documented with comments
3. Add any necessary helper functions
4. Update this README if adding a new test category
5. Run the coverage reporter to ensure adequate test coverage

## Test Coverage Goals

- Line coverage: ≥ 80%
- Branch coverage: ≥ 75%
- Function coverage: ≥ 85%

## Continuous Integration

These tests are automatically run in the CI pipeline on every pull request and merge to main. The coverage reports are published as artifacts and can be viewed in the CI dashboard.