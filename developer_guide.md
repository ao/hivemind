# Hivemind Developer Guide

This guide provides comprehensive information for developers working with the Hivemind platform, including how to set up a development environment, understand the codebase, deploy applications, and extend the platform with new features.

## Development Environment Setup

### Prerequisites

- **Rust 1.60+**: Hivemind is built in Rust and requires version 1.60 or newer
- **containerd**: For container runtime integration (optional for development with mock mode)
- **SQLite**: For persistent storage
- **Git**: For version control
- **Docker**: For building and testing container images

### Getting Started

1. **Clone the repository**:
   ```bash
   git clone https://github.com/ao/hivemind.git
   cd hivemind
   ```

2. **Build the project**:
   ```bash
   cargo build
   ```

3. **Run tests**:
   ```bash
   cargo test
   ```

4. **Run in development mode**:
   ```bash
   cargo run -- web --port 3000
   ```

### Development Modes

Hivemind supports different development modes:

1. **Mock Mode**: Uses mock implementations of container runtime and other external dependencies
   ```bash
   cargo run -- web --port 3000 --mock
   ```

2. **Full Mode**: Uses real implementations of all components
   ```bash
   cargo run -- daemon --web-port 3000
   ```

3. **Web-Only Mode**: Runs only the web interface
   ```bash
   cargo run -- web --port 3000
   ```

## Project Structure

The Hivemind codebase is organized into several key modules:

- `src/app.rs` - Application & container management
- `src/containerd_manager.rs` - Container runtime integration
- `src/service_discovery.rs` - Service discovery & DNS
- `src/storage.rs` - Persistence layer
- `src/node.rs` - Node & cluster management
- `src/membership.rs` - SWIM-based node membership protocol
- `src/network.rs` - Container networking & overlay network
- `src/scheduler.rs` - Container scheduler
- `src/health_monitor.rs` - Health monitoring system
- `src/security/` - Security features
  - `container_scanning.rs` - Container image scanning
  - `network_policy.rs` - Network policy enforcement
  - `rbac.rs` - Role-based access control
  - `secret_management.rs` - Secret management
- `src/web.rs` - Web UI & dashboard
- `src/main.rs` - CLI & entry point
- `src/deployment.rs` - Advanced deployment strategies
- `src/cicd.rs` - CI/CD pipeline integration
- `src/cloud.rs` - Cloud provider integration
- `src/helm.rs` - Helm chart support
- `src/observability.rs` - Metrics, tracing, and logging

### Key Abstractions

1. **AppManager**: Manages applications and containers
2. **ContainerdManager**: Integrates with the container runtime
3. **ServiceDiscovery**: Provides service discovery and DNS resolution
4. **NodeManager**: Manages cluster nodes
5. **MembershipProtocol**: Implements the SWIM protocol for cluster membership
6. **NetworkManager**: Manages container networking
7. **ContainerScheduler**: Schedules containers on nodes
8. **HealthMonitor**: Monitors container and node health
9. **SecurityManager**: Manages security features
10. **DeploymentManager**: Manages advanced deployment strategies
11. **CicdManager**: Manages CI/CD pipeline integration
12. **CloudManager**: Manages cloud provider integration
13. **HelmManager**: Manages Helm chart support
14. **ObservabilityManager**: Manages metrics, tracing, and logging

## Development Workflow

### Making Changes

1. **Create a feature branch**:
   ```bash
   git checkout -b feature/my-feature
   ```

2. **Make your changes**: Implement your feature or fix

3. **Write tests**: Add tests for your changes

4. **Run tests**:
   ```bash
   cargo test
   ```

5. **Format your code**:
   ```bash
   cargo fmt
   ```

6. **Check for linting issues**:
   ```bash
   cargo clippy
   ```

7. **Submit a pull request**: Push your changes and create a PR

### Testing

Hivemind has several types of tests:

1. **Unit Tests**: Test individual components in isolation
   ```bash
   cargo test --lib
   ```

2. **Integration Tests**: Test multiple components working together
   ```bash
   cargo test --test integration_tests
   ```

3. **Performance Tests**: Test performance under load
   ```bash
   cargo test --test performance_tests
   ```

4. **Chaos Tests**: Test resilience under failure conditions
   ```bash
   cargo test --test chaos_tests
   ```

## Deployment Options

A developer has three main ways to deploy their Docker image to Hivemind:

1. **Command Line Interface (CLI)**
2. **Web UI**
3. **REST API**

### Prerequisites

- The Docker image is already built and pushed to a registry (Docker Hub, ECR, etc.)
- Hivemind daemon is running on the server

### Option 1: Command Line Interface (CLI)

The simplest way to deploy using the CLI:

```bash
hivemind app deploy --image your-registry/your-image:tag --name your-app-name
```

For a public-facing service with a domain:

```bash
hivemind app deploy --image your-registry/your-image:tag --name your-app-name --service your-service-domain
```

#### CLI Examples

1. **Deploy a simple application**:
   ```bash
   hivemind app deploy --image myapp:latest --name myapp
   ```

2. **Deploy with a service domain** (for routing/discovery):
   ```bash
   hivemind app deploy --image myapp:latest --name myapp --service app.example.com
   ```

3. **Deploy with resource limits**:
   ```bash
   hivemind app deploy --image myapp:latest --name myapp --cpu 0.5 --memory 512M
   ```

4. **Deploy with volume mounts**:
   ```bash
   hivemind app deploy --image myapp:latest --name myapp --volume my-data:/data
   ```

5. **Deploy with environment variables**:
   ```bash
   hivemind app deploy --image myapp:latest --name myapp --env "KEY1=value1" --env "KEY2=value2"
   ```

6. **Check deployed containers**:
   ```bash
   hivemind app containers
   ```

7. **Get detailed container info**:
   ```bash
   hivemind app container-info --container-id <container-id>
   ```

8. **Restart an application**:
   ```bash
   hivemind app restart --name myapp
   ```

9. **Scale an application**:
   ```bash
   hivemind app scale --name myapp --replicas 3
   ```

### Option 2: Web UI

Hivemind provides a web interface accessible at port 3000 by default:

1. Navigate to `http://<your-server>:3000`
2. Click on the "Deploy" link in the navigation bar
3. Fill in the form:
   - Name: Give your application a name
   - Image: Select or enter your Docker image (in format `repository/image:tag`)
   - Service Domain (optional): Enter a domain name if you want the application exposed
   - Resource Limits (optional): Set CPU and memory limits
   - Volumes (optional): Add volume mounts
   - Environment Variables (optional): Add environment variables
4. Click "Deploy" button

### Option 3: REST API

For automated or programmatic deployments, you can use the REST API:

```bash
curl -X POST http://<your-server>:3000/api/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "image": "your-registry/your-image:tag",
    "name": "your-app-name",
    "service": "optional-domain-name",
    "resources": {
      "cpu": 0.5,
      "memory": "512M"
    },
    "volumes": [
      {"name": "my-data", "mountPath": "/data"}
    ],
    "env": {
      "KEY1": "value1",
      "KEY2": "value2"
    }
  }'
```

Response:
```json
{
  "success": true,
  "container_id": "your-app-name",
  "message": "Deployed application your-app-name with container your-app-name"
}
```

## Working with Volumes

If your application needs persistent storage, you can create and use volumes:

1. **Create a volume** (via API):
   ```bash
   curl -X POST http://<your-server>:3000/api/volumes/create \
     -H "Content-Type: application/json" \
     -d '{"name": "my-data-volume"}'
   ```

2. **Deploy with volumes**:
   ```bash
   hivemind app deploy --image nginx:latest --name web-frontend --volume my-data-volume:/data
   ```

## Working with Security Features

### Container Scanning

Scan container images for vulnerabilities:

```bash
hivemind security scan-image --image nginx:latest
```

### Network Policies

Create a network policy:

```bash
curl -X POST http://<your-server>:3000/api/security/network-policies \
  -H "Content-Type: application/json" \
  -d '{
    "name": "web-to-db",
    "selector": {
      "labels": {"app": "web"}
    },
    "ingress_rules": [
      {
        "ports": [{"protocol": "TCP", "port_min": 80, "port_max": 80}],
        "from": [{"ip_block": "10.244.2.0/24"}]
      }
    ],
    "egress_rules": [
      {
        "ports": [{"protocol": "TCP", "port_min": 5432, "port_max": 5432}],
        "from": [{"selector": {"labels": {"app": "db"}}}]
      }
    ]
  }'
```

### Secret Management

Create and use secrets:

```bash
# Create a secret
hivemind security create-secret --name db-password --file password.txt

# Use a secret in a deployment
hivemind app deploy --image myapp:latest --name myapp --secret db-password:/etc/secrets/db-password
```

## Extending Hivemind

### Adding a New Command

1. Add a new subcommand to the CLI in `src/main.rs`
2. Implement the command handler
3. Add tests for the new command

### Adding a New API Endpoint

1. Add a new route in `src/web.rs`
2. Implement the handler function
3. Add tests for the new endpoint

### Adding a New Feature

1. Create a new module in `src/`
2. Implement the feature
3. Integrate with existing components
4. Add tests for the new feature

## How It Works Under the Hood

When you deploy an image:

1. Hivemind checks if it can connect to containerd (the container runtime)
2. If available, it uses containerd to pull and start your container
3. If not, it falls back to a mock implementation
4. It registers your container in its internal database
5. If you specified a service domain, it registers the service with the service discovery system
6. Containers are monitored for health and status changes

The connection between Hivemind and your container images happens in the `ContainerdManager` which:
- Pulls images from registries
- Creates and manages containers
- Maps ports and volumes
- Monitors container status

## Container Networking

Hivemind includes a robust container networking system that enables containers to communicate across nodes in a cluster. For detailed information, see the [Container Networking Documentation](docs/container_networking.md).

### Key Features

- **Automatic IP Address Management**: Each container gets a unique IP address
- **Overlay Network**: Containers can communicate across nodes transparently
- **Network Policies**: Control traffic flow between containers for security
- **Service Discovery Integration**: Find services by name rather than IP address

### How Container Networking Works

When you deploy a container:

1. The container is assigned an IP address from the node's subnet
2. A virtual network interface is created in the container
3. The container is connected to the node's bridge
4. The bridge is connected to an overlay network that spans all nodes
5. Containers can communicate with each other using their assigned IPs

### Checking Network Health

You can check the status of the container networking system using:

```bash
hivemind health
```

This will show information about:
- Nodes in the network
- Overlay tunnels between nodes
- Network policies

### Network Troubleshooting

If containers are having trouble communicating:

1. Check that the containers have valid IP addresses
2. Verify that the overlay network is functioning
3. Check for network policies that might be blocking traffic
4. Ensure that the nodes can reach each other on the VXLAN port (default: 4789)

For more detailed troubleshooting, refer to the [Container Networking Documentation](docs/container_networking.md).

## Debugging Tips

### Logging

Hivemind uses the standard Rust logging framework. You can control the log level using the `RUST_LOG` environment variable:

```bash
RUST_LOG=debug cargo run -- daemon
```

Log levels:
- `error`: Error conditions
- `warn`: Warning conditions
- `info`: Informational messages
- `debug`: Debug-level messages
- `trace`: Trace-level messages

### Common Issues

1. **Container fails to start**:
   - Check the container logs: `hivemind app logs --name <app-name>`
   - Check the Hivemind logs: `RUST_LOG=debug hivemind daemon`
   - Verify the image exists: `docker pull <image>`

2. **Service discovery not working**:
   - Check the service is registered: `hivemind app services`
   - Verify DNS resolution: `nslookup <service-name>.local`
   - Check network policies: `hivemind security list-network-policies`

3. **Node not joining cluster**:
   - Check network connectivity between nodes
   - Verify the node is running: `hivemind node ls`
   - Check the membership protocol logs: `RUST_LOG=debug hivemind daemon`

## Contributing

We welcome contributions to Hivemind! Please follow these guidelines:

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Write tests for your changes
5. Format your code with `cargo fmt`
6. Check for linting issues with `cargo clippy`
7. Submit a pull request

## Advanced Features

### Working with Deployment Strategies

Hivemind supports several advanced deployment strategies:

1. **Simple Deployment**: Deploy all containers at once
   ```bash
   hivemind app deploy --image myapp:latest --name myapp --strategy simple
   ```

2. **Rolling Update**: Update containers in batches
   ```bash
   hivemind app deploy --image myapp:latest --name myapp --strategy rolling-update --max-unavailable 1 --max-surge 1
   ```

3. **Blue-Green Deployment**: Deploy a new version alongside the old one and switch traffic
   ```bash
   hivemind app deploy --image myapp:latest --name myapp --strategy blue-green --verification-timeout 60
   ```

4. **Canary Deployment**: Deploy a new version to a subset of users
   ```bash
   hivemind app deploy --image myapp:latest --name myapp --strategy canary --percentage 20 --steps 20,50,100 --interval 300
   ```

5. **A/B Testing**: Deploy multiple variants for testing
   ```bash
   hivemind app deploy --name myapp --strategy ab-testing --variant "name=v1,image=myapp:v1,percentage=50" --variant "name=v2,image=myapp:v2,percentage=50" --duration 86400
   ```

For more details on implementing custom deployment strategies, see the [Deployment Manager](#deployment-manager) section.

### Deployment Manager

The Deployment Manager handles advanced deployment strategies:

```rust
// Example implementation of a custom deployment strategy
pub struct MyCustomStrategy {
    app_name: String,
    image: String,
    // Custom parameters
}

impl DeploymentStrategy for MyCustomStrategy {
    fn execute(&self, app_manager: &dyn AppManager) -> Result<(), Error> {
        // Implementation of the strategy
    }
    
    fn rollback(&self, app_manager: &dyn AppManager) -> Result<(), Error> {
        // Rollback implementation
    }
}
```

To register a custom strategy:

```rust
let mut deployment_manager = DeploymentManager::new(app_manager);
deployment_manager.register_strategy("my-custom", Box::new(MyCustomStrategyFactory {}));
```

For more details on implementing custom deployment strategies, see the [Deployment Manager](#deployment-manager) section.

### Deployment Manager

The Deployment Manager handles advanced deployment strategies:

```rust
// Example implementation of a custom deployment strategy
pub struct MyCustomStrategy {
    app_name: String,
    image: String,
    // Custom parameters
}

impl DeploymentStrategy for MyCustomStrategy {
    fn execute(&self, app_manager: &dyn AppManager) -> Result<(), Error> {
        // Implementation of the strategy
    }
    
    fn rollback(&self, app_manager: &dyn AppManager) -> Result<(), Error> {
        // Rollback implementation
    }
}
```

To register a custom strategy:

```rust
let mut deployment_manager = DeploymentManager::new(app_manager);
deployment_manager.register_strategy("my-custom", Box::new(MyCustomStrategyFactory {}));
```

### Working with CI/CD Integration

Hivemind provides built-in support for CI/CD pipelines:

1. **Create a pipeline**:
   ```bash
   hivemind cicd create-pipeline --name my-pipeline --repository https://github.com/user/repo --branch main
   ```

2. **Configure a pipeline**:
   ```bash
   hivemind cicd configure-pipeline --name my-pipeline --build-command "npm run build" --test-command "npm test"
   ```

3. **Set deployment strategy**:
   ```bash
   hivemind cicd set-deployment-strategy --name my-pipeline --strategy blue-green --verification-timeout 60
   ```

4. **Trigger a pipeline**:
   ```bash
   hivemind cicd trigger-pipeline --name my-pipeline
   ```

5. **View pipeline status**:
   ```bash
   hivemind cicd get-pipeline-status --name my-pipeline
   ```

### Working with Cloud Integration

Hivemind can integrate with major cloud providers:

1. **Configure a cloud provider**:
   ```bash
   hivemind cloud configure --provider aws --access-key <ACCESS_KEY> --secret-key <SECRET_KEY> --region us-west-2
   ```

2. **Create a cloud instance**:
   ```bash
   hivemind cloud create-instance --name my-instance --provider aws --region us-west-2 --zone us-west-2a --instance-type t3.micro
   ```

3. **Create a cloud load balancer**:
   ```bash
   hivemind cloud create-load-balancer --name my-lb --provider aws --region us-west-2 --type application --scheme internet-facing
   ```

4. **Register instances with a load balancer**:
   ```bash
   hivemind cloud register-instances --lb-name my-lb --instance-ids my-instance-1,my-instance-2
   ```

### Working with Helm Charts

Hivemind provides support for Helm charts:

1. **Add a Helm repository**:
   ```bash
   hivemind helm repo add --name stable --url https://charts.helm.sh/stable
   ```

2. **Create a Helm chart**:
   ```bash
   hivemind helm create-chart --name my-chart --description "My application chart"
   ```

3. **Install a Helm chart**:
   ```bash
   hivemind helm install --name my-release --chart stable/nginx --namespace default
   ```

4. **Upgrade a Helm release**:
   ```bash
   hivemind helm upgrade --name my-release --chart stable/nginx --namespace default --version 1.2.3
   ```

5. **Uninstall a Helm release**:
   ```bash
   hivemind helm uninstall --name my-release --namespace default
   ```

### Working with Observability

Hivemind provides comprehensive observability features:

1. **Configure Prometheus metrics**:
   ```bash
   hivemind observability configure-metrics --port 9090 --path /metrics
   ```

2. **Configure OpenTelemetry tracing**:
   ```bash
   hivemind observability configure-tracing --service-name hivemind --endpoint http://jaeger:14268/api/traces
   ```

3. **Configure log aggregation**:
   ```bash
   hivemind observability configure-logging --endpoint http://elasticsearch:9200 --index hivemind-logs
   ```

4. **View metrics dashboard**:
   ```bash
   hivemind observability open-dashboard
   ```

## Architecture Deep Dive

### Component Interactions

The Hivemind architecture is built around clear component interactions:

1. **App Manager and Scheduler**:
   - App Manager receives deployment requests
   - Scheduler selects optimal nodes based on resources and constraints
   - App Manager deploys containers to selected nodes

2. **Network Manager and Service Discovery**:
   - Network Manager assigns IP addresses to containers
   - Service Discovery registers services and provides DNS resolution
   - Containers communicate using service names

3. **Health Monitor and Container Manager**:
   - Health Monitor continuously checks container health
   - Container Manager restarts unhealthy containers
   - Health status is reported to the dashboard

4. **Security Manager and App Manager**:
   - Security Manager scans images before deployment
   - Network policies control traffic between containers
   - RBAC controls access to resources

### Data Flow

Understanding the data flow in Hivemind is crucial for development:

1. **Container Deployment Flow**:
   ```
   User Request → App Manager → Scheduler → Container Manager → Network Manager → Service Discovery → Health Monitor
   ```

2. **Service Discovery Flow**:
   ```
   Container DNS Query → Service Discovery → Endpoint Selection → Direct Connection
   ```

3. **Node Membership Flow**:
   ```
   Node Join → Membership Protocol → Periodic Health Checks → Gossip Dissemination
   ```

4. **Health Monitoring Flow**:
   ```
   Health Check → Status Evaluation → Auto-healing Actions → Metrics Collection
   ```

### Extension Points

Hivemind provides several extension points for customization:

1. **Custom Schedulers**:
   ```rust
   pub trait SchedulingStrategy {
       fn select_node(&self, container: &Container, nodes: &[Node]) -> Option<Node>;
   }
   ```

2. **Custom Health Checks**:
   ```rust
   pub trait HealthCheck {
       fn check(&self, target: &HealthCheckTarget) -> HealthStatus;
   }
   ```

3. **Custom Network Policies**:
   ```rust
   pub trait PolicyEvaluator {
       fn evaluate(&self, source: &NetworkEndpoint, target: &NetworkEndpoint) -> bool;
   }
   ```

4. **Custom Service Discovery Backends**:
   ```rust
   pub trait ServiceRegistry {
       fn register(&self, service: &Service) -> Result<(), Error>;
       fn lookup(&self, name: &str) -> Result<Vec<Endpoint>, Error>;
   }
   ```

## Performance Optimization

### Resource Efficiency

Hivemind is designed to be resource-efficient:

1. **Memory Usage**:
   - Use Rust's ownership model to minimize memory overhead
   - Implement custom allocators for performance-critical components
   - Use arena allocation for frequently allocated/deallocated objects

2. **CPU Optimization**:
   - Use async/await for I/O-bound operations
   - Implement work-stealing thread pools for CPU-bound tasks
   - Profile and optimize hot paths

3. **Network Efficiency**:
   - Implement connection pooling
   - Use binary protocols for internal communication
   - Batch operations when possible

### Benchmarking

Benchmark your changes to ensure they don't negatively impact performance:

```bash
# Run performance benchmarks
cargo bench

# Run specific benchmark
cargo bench --bench scheduler_bench
```

## Testing Strategies

### Unit Testing

Write comprehensive unit tests for all components:

```rust
#[cfg(test)]
mod tests {
    use super::*;
    
    #[test]
    fn test_container_scheduling() {
        let scheduler = ContainerScheduler::new();
        let nodes = vec![
            Node::new("node1", NodeResources::new(4, 8192)),
            Node::new("node2", NodeResources::new(8, 16384)),
        ];
        let container = Container::new("test", "nginx:latest", ResourceRequirements::new(2, 4096));
        
        let selected = scheduler.schedule(&container, &nodes).unwrap();
        assert_eq!(selected.name(), "node2");
    }
}
```

### Integration Testing

Test components working together:

```rust
#[test]
fn test_deployment_flow() {
    let mut app_manager = AppManager::new();
    let mut scheduler = ContainerScheduler::new();
    let mut container_manager = ContainerdManager::new();
    
    // Set up test environment
    
    // Test the deployment flow
    let result = app_manager.deploy("nginx:latest", "web", &scheduler, &container_manager);
    assert!(result.is_ok());
    
    // Verify the container is running
    let containers = app_manager.list_containers().unwrap();
    assert_eq!(containers.len(), 1);
    assert_eq!(containers[0].status(), ContainerStatus::Running);
}
```

### Chaos Testing

Test resilience under failure conditions:

```bash
# Run chaos tests
cargo test --test chaos_tests

# Run specific chaos test
cargo test --test chaos_tests -- test_node_failure_recovery
```

## Documentation Standards

### Code Documentation

Follow these standards for code documentation:

1. **Module Documentation**:
   ```rust
   //! # Network Manager
   //!
   //! The Network Manager handles container networking, including IP address
   //! allocation, overlay networking, and network policy enforcement.
   ```

2. **Function Documentation**:
   ```rust
   /// Allocates an IP address for a container
   ///
   /// # Arguments
   ///
   /// * `container_id` - The ID of the container
   /// * `subnet` - The subnet to allocate from
   ///
   /// # Returns
   ///
   /// The allocated IP address or an error
   ///
   /// # Errors
   ///
   /// Returns an error if no IP addresses are available
   pub fn allocate_ip(&self, container_id: &str, subnet: &Subnet) -> Result<IpAddr, Error> {
       // Implementation
   }
   ```

3. **Example Usage**:
   ```rust
   /// # Examples
   ///
   /// ```
   /// let network_manager = NetworkManager::new();
   /// let subnet = Subnet::new("10.244.0.0/24");
   /// let ip = network_manager.allocate_ip("container-1", &subnet).unwrap();
   /// assert!(subnet.contains(ip));
   /// ```
   ```

### Component Documentation

Create comprehensive documentation for each component:

1. **Overview**: High-level description of the component
2. **Architecture**: How the component is structured
3. **Interfaces**: Public APIs and their usage
4. **Implementation Details**: Key algorithms and data structures
5. **Configuration**: Available configuration options
6. **Examples**: Example usage scenarios

## Additional Resources

- [API Documentation](docs/api_reference.md)
- [CLI Reference](docs/cli_reference.md)
- [Architecture Guide](ARCHITECTURE.md)
- [Container Networking](docs/container_networking.md)
- [Service Discovery](docs/service_discovery.md)
- [Security Features](docs/security_features.md)
- [Node Membership Protocol](docs/node_membership_protocol.md)
- [CI/CD Integration](docs/cicd_integration.md)
- [Monitoring & Observability](docs/monitoring_observability.md)
- [Deployment Guide](docs/deployment_guide.md)
- [Cloud Integration](docs/cloud_integration.md)
- [Advanced Deployments](docs/advanced_deployments.md)
- [Helm Integration](docs/helm_integration.md)
