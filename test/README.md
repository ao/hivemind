# Hivemind Test Suite

This directory contains comprehensive tests for the Hivemind container orchestration system. The tests are organized by type and cover all aspects of the system's functionality.

## Testing Approach

Hivemind follows a hybrid testing approach:

1. **Dedicated Test Directory**: This `test/` directory contains organized test suites for different types of tests (unit, integration, performance, etc.)
2. **Standard Go Tests**: Some packages also have `*_test.go` files alongside their implementation files, following Go's standard testing approach

## Test Structure

The test suite is organized into the following directories:

- `test/fixtures`: Test fixtures and helpers used across all tests
- `test/unit`: Unit tests for individual components
- `test/integration`: Integration tests for component interactions
- `test/performance`: Performance tests for critical operations
- `test/chaos`: Chaos tests for resilience testing
- `test/network`: Network tests for network functionality
- `test/security`: Security tests for security features
- `test/tenant`: Tenant tests for multi-tenancy features

Additionally, some packages have their own tests in `*_test.go` files within their respective directories:

- `internal/web/server_test.go`: Tests for the web server
- `internal/resilience/example_test.go`: Examples and tests for the resilience package

## Running Tests

### Running All Tests

To run all tests, use the following command:

```bash
go test ./...
```

This will run both the tests in the `test/` directory and the standard Go tests in the package directories.

### Running Specific Test Types

To run specific types of tests, use the following commands:

```bash
# Run unit tests in the test directory
go test ./test/unit/...

# Run integration tests
go test ./test/integration/...

# Run performance tests
go test ./test/performance/...

# Run chaos tests
go test ./test/chaos/...

# Run network tests
go test ./test/network/...

# Run security tests
go test ./test/security/...

# Run tenant tests
go test ./test/tenant/...

# Run tests for a specific package
go test ./internal/web/...
go test ./internal/resilience/...
```

### Running Short Tests

To run only short tests (skipping long-running tests), use the `-short` flag:

```bash
go test -short ./...
```

### Running Tests with Verbose Output

To run tests with verbose output, use the `-v` flag:

```bash
go test -v ./...
```

### Running Tests with Coverage

To run tests with coverage, use the `-cover` flag:

```bash
go test -cover ./...
```

To generate a coverage report, use the following commands:

```bash
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html
```

## Test Types

### Unit Tests

Unit tests focus on testing individual components in isolation. They use mocks to simulate dependencies and test the component's behavior under various conditions.

Key unit tests include:
- `test/unit/containerd_manager_test.go`: Tests for the containerd manager
- `test/unit/node_manager_test.go`: Tests for the node manager
- `test/unit/scheduler_test.go`: Tests for the scheduler
- `test/unit/security_network_policy_test.go`: Tests for the network policy functionality

### Integration Tests

Integration tests focus on testing the interactions between components. They test end-to-end workflows and verify that components work together correctly.

Key integration tests include:
- `test/integration/integration_test.go`: Tests for component integration
- `test/integration/security_test.go`: Tests for security module integration

### Performance Tests

Performance tests focus on testing the system's performance under various conditions. They benchmark critical operations and identify performance bottlenecks.

Key performance tests include:
- `test/performance/scheduler_performance_test.go`: Tests for scheduler performance

### Chaos Tests

Chaos tests focus on testing the system's resilience under failure conditions. They simulate node failures, network partitions, and other failure scenarios to verify that the system can recover.

Key chaos tests include:
- `test/chaos/node_failure_test.go`: Tests for node failure recovery

### Network Tests

Network tests focus on testing the network functionality of the system. They test overlay network creation, network policy enforcement, and other network-related features.

Key network tests include:
- `test/network/network_test.go`: Tests for network functionality

### Security Tests

Security tests focus on testing the security features of the system. They test RBAC enforcement, network policy enforcement, secret management, and other security-related features.

Key security tests include:
- `test/unit/security_network_policy_test.go`: Tests for network policy enforcement

### Tenant Tests

Tenant tests focus on testing the multi-tenancy features of the system. They test tenant isolation, tenant resource limits, and other tenant-related features.

Key tenant tests include:
- `test/tenant/tenant_test.go`: Tests for tenant isolation and resource limits

## Test Fixtures and Helpers

The `test/fixtures` directory contains test fixtures and helpers used across all tests. These include:

- `test_helpers.go`: Common test helpers
- `mocks.go`: Mock implementations of interfaces

## Best Practices

When writing tests for Hivemind, follow these best practices:

1. **Use Table-Driven Tests**: Use table-driven tests to test multiple scenarios with the same test logic.
2. **Use Subtests**: Use subtests to organize tests and provide better test output.
3. **Use Mocks**: Use mocks to simulate dependencies and test components in isolation.
4. **Use Assertions**: Use assertions to verify test results.
5. **Use Test Helpers**: Use test helpers to reduce code duplication.
6. **Use Test Fixtures**: Use test fixtures to set up test environments.
7. **Use Test Cleanup**: Use test cleanup to clean up test environments.
8. **Use Test Timeouts**: Use test timeouts to prevent tests from hanging.
9. **Use Test Skip**: Use test skip to skip tests that are not applicable.
10. **Use Test Parallel**: Use test parallel to run tests in parallel.

## Adding New Tests

When adding new tests, follow these guidelines:

1. **Choose the Right Test Type**: Choose the right test type based on what you're testing.
2. **Follow the Naming Convention**: Name your test file `<component>_test.go`.
3. **Use the Right Test Fixtures**: Use the right test fixtures for your test.
4. **Add Test Documentation**: Add documentation to explain what your test is testing.
5. **Run Your Tests**: Run your tests to make sure they pass.
6. **Add Your Tests to CI**: Add your tests to the CI pipeline.

## Continuous Integration

The test suite is integrated with the CI pipeline. The CI pipeline runs all tests on every pull request and merge to the main branch.

## Test Coverage

The test suite aims to achieve high test coverage. The current test coverage is monitored and reported in the CI pipeline.

## Troubleshooting

If you encounter issues with the tests, try the following:

1. **Check the Test Environment**: Make sure your test environment is set up correctly.
2. **Check the Test Dependencies**: Make sure all test dependencies are installed.
3. **Check the Test Logs**: Check the test logs for error messages.
4. **Run Tests in Verbose Mode**: Run tests in verbose mode to get more information.
5. **Run Tests with Race Detection**: Run tests with race detection to find race conditions.
6. **Run Tests with Coverage**: Run tests with coverage to find untested code.
7. **Run Tests with Benchmarks**: Run tests with benchmarks to find performance issues.