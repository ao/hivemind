# Hivemind Testing Framework

This directory contains comprehensive tests for the Hivemind container orchestration platform. The tests are designed to verify the functionality, resilience, and performance of all components.

## Test Categories

### Unit Tests
- **unit_tests.rs**: Tests for individual components and basic functionality
- **scheduler_tests.rs**: Tests for the container scheduler component
- **network_tests.rs**: Tests for the network management component
- **membership_tests.rs**: Tests for the cluster membership component
- **health_monitor_tests.rs**: Tests for the health monitoring component

### Integration Tests
- **integration_tests.rs**: Tests for component interactions and workflows

### Security Tests
- **security_tests.rs**: Tests for security features including RBAC, network policies, and secret management

### Chaos Tests
- **chaos_tests.rs**: Tests for failure scenarios and recovery mechanisms

### Performance Tests
- **performance_tests.rs**: Tests for system performance under load

## Running Tests

### Running All Tests
```bash
cargo test
```

### Running Specific Test Categories
```bash
# Run unit tests
cargo test --test unit_tests --test scheduler_tests --test network_tests --test membership_tests --test health_monitor_tests

# Run integration tests
cargo test --test integration_tests

# Run security tests
cargo test --test security_tests

# Run chaos tests
cargo test --test chaos_tests

# Run performance tests
cargo test --test performance_tests
```

### Running the Test Runner
The test runner executes all tests and generates a coverage report:

```bash
cargo run --bin test_runner
```

## Test Coverage

The testing framework aims to achieve at least 80% code coverage across all components. Coverage reports are generated using cargo-tarpaulin and can be viewed after running the test runner.

### Installing cargo-tarpaulin
```bash
cargo install cargo-tarpaulin
```

### Generating Coverage Reports Manually
```bash
cargo tarpaulin --out Html
```

## Test Structure

Each test file follows a consistent structure:

1. **Setup**: Create necessary components and mock dependencies
2. **Execution**: Run the functionality being tested
3. **Verification**: Assert that the results match expected outcomes
4. **Cleanup**: Clean up any resources created during the test

## Mock Components

Several mock components are provided to facilitate testing:

- **MockNodeManager**: Simulates a cluster of nodes
- **MockContainerRuntime**: Simulates container operations
- **MockNetworkManager**: Simulates network operations

## Test Scenarios

### Unit Test Scenarios
- Component initialization
- Configuration validation
- Basic operations
- Edge cases and error handling

### Integration Test Scenarios
- End-to-end workflows
- Multi-component interactions
- State persistence

### Chaos Test Scenarios
- Node failures and recovery
- Container failures and recovery
- Network partitions
- Resource exhaustion
- Leader failures

### Performance Test Scenarios
- Large cluster operations
- Concurrent operations
- Resource utilization under load
- Scaling performance

## Adding New Tests

When adding new tests, follow these guidelines:

1. Place the test in the appropriate category file
2. Follow the existing test structure
3. Use descriptive test names
4. Include comments explaining the test purpose
5. Ensure tests are isolated and don't interfere with each other
6. Clean up resources after tests complete

## Continuous Integration

These tests are integrated into the CI pipeline and run automatically on each pull request. Tests must pass before code can be merged.