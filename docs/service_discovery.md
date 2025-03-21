# Service Discovery in Hivemind

This document describes the service discovery mechanism in Hivemind, which enables containers and applications to find and communicate with each other across the cluster.

## Overview

Hivemind's service discovery provides a robust, scalable way for services to discover and communicate with each other. It builds on top of the container networking layer to provide a higher-level abstraction for service-to-service communication.

### Key Components

1. **Service Registry**: Maintains a registry of all services and their endpoints
2. **DNS Server**: Provides DNS resolution for service names
3. **Health Checking**: Monitors the health of service endpoints
4. **Load Balancing**: Distributes traffic across healthy endpoints
5. **Network Integration**: Works with the container networking layer for cross-node communication

### Architecture Diagram

```
┌─────────────────────────────────────────────────────────────┐
│                     Service Discovery                       │
│                                                             │
│  ┌───────────────┐  ┌───────────────┐  ┌───────────────┐    │
│  │ Service       │  │ Health        │  │ Load          │    │
│  │ Registry      │  │ Checking      │  │ Balancing     │    │
│  └───────┬───────┘  └───────┬───────┘  └───────┬───────┘    │
│          │                  │                  │            │
│          └──────────────────┼──────────────────┘            │
│                             │                               │
│  ┌───────────────┐  ┌───────┴───────┐  ┌───────────────┐    │
│  │ DNS Server    │  │ Network       │  │ Proxy Server  │    │
│  │               │◄─┤ Integration   ├─►│ (Optional)    │    │
│  └───────────────┘  └───────────────┘  └───────────────┘    │
└─────────────────────────────┬───────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                    Container Networking                     │
└─────────────────────────────────────────────────────────────┘
```

## Service Registry

The service registry maintains information about all services in the cluster:

- **Service Name**: A unique identifier for the service
- **Domain**: The DNS domain name for the service
- **Endpoints**: A list of endpoints (containers) that provide the service
- **Health Status**: The health status of each endpoint
- **Node Location**: Which node each endpoint is running on

Services are automatically registered when containers are deployed with a service domain.

## DNS Server

The DNS server provides name resolution for service domains:

- Listens on port 53 (standard DNS port)
- Resolves service domains to the IP addresses of healthy endpoints
- Supports A record queries
- Uses the service registry to find the appropriate endpoints

Example DNS query flow:

1. Container sends a DNS query for `myservice.local`
2. DNS server looks up the service in the registry
3. DNS server returns the IP address of a healthy endpoint
4. Container connects directly to the endpoint

## Health Checking

The health checking system monitors the health of service endpoints:

- **Protocols**: Supports HTTP, HTTPS, and TCP health checks
- **Configurable**: Customizable check intervals, timeouts, and thresholds
- **Automatic**: Endpoints are automatically marked as healthy or unhealthy
- **Circuit Breaking**: Unhealthy endpoints are removed from load balancing

Health check configuration options:

| Option | Description | Default |
|--------|-------------|---------|
| Protocol | HTTP, HTTPS, TCP, or Command | TCP |
| Path | URL path for HTTP/HTTPS checks | /health |
| Interval | How often to check (seconds) | 30 |
| Timeout | Check timeout (seconds) | 5 |
| Healthy Threshold | Successful checks to mark as healthy | 2 |
| Unhealthy Threshold | Failed checks to mark as unhealthy | 3 |

## Load Balancing

The load balancing system distributes traffic across healthy endpoints:

- **Strategies**: Supports Round Robin, Least Connections, and Random
- **Health-Aware**: Only routes traffic to healthy endpoints
- **Configurable**: Different strategies can be used for different services
- **Metrics**: Tracks connection statistics for better decision making

Load balancing strategies:

| Strategy | Description |
|----------|-------------|
| RoundRobin | Rotates through endpoints in sequence |
| LeastConnections | Selects endpoint with fewest active connections |
| Random | Randomly selects an endpoint |
| WeightedRoundRobin | Round robin with weights (not fully implemented) |
| IPHash | Consistent hashing based on client IP (not fully implemented) |

## Network Integration

The service discovery system integrates with the container networking layer:

- Automatically updates when nodes join or leave the cluster
- Ensures network connectivity between services
- Works with overlay networking for cross-node communication
- Handles container creation and removal events

## Usage Examples

### Deploying a Service

When deploying a container with a service domain:

```bash
hivemind app deploy --image nginx:latest --name web-frontend --service web.local
```

The service discovery system will:

1. Register the service `web-frontend` with domain `web.local`
2. Start health checking the container
3. Make the service available via DNS at `web.local`

### Accessing a Service

Containers can access the service using its domain name:

```bash
curl http://web.local/
```

The DNS server will resolve `web.local` to the IP address of a healthy endpoint, and the request will be routed directly to that endpoint.

### Scaling a Service

When scaling a service:

```bash
hivemind app scale --name web-frontend --replicas 3
```

The service discovery system will:

1. Register the new endpoints
2. Start health checking the new containers
3. Include the new endpoints in load balancing
4. Update DNS to include all healthy endpoints

## API Reference

The service discovery system provides several APIs for managing services:

### Service Registration

```rust
service_discovery.register_service(
    &service_config,
    node_id,
    ip_address,
    port,
).await?;
```

### Service Deregistration

```rust
service_discovery.deregister_service(
    service_name,
    node_id,
    ip_address,
    port,
).await?;
```

### Health Check Configuration

```rust
service_discovery.configure_health_check(
    service_name,
    HealthCheckConfig {
        protocol: HealthCheckProtocol::HTTP,
        path: Some("/health".to_string()),
        interval_secs: 10,
        timeout_secs: 2,
        healthy_threshold: 2,
        unhealthy_threshold: 3,
    },
).await?;
```

### Load Balancing Configuration

```rust
service_discovery.configure_load_balancing(
    service_name,
    LoadBalancingStrategy::LeastConnections,
).await?;
```

### Getting Service Endpoints

```rust
let endpoint = service_discovery.get_service_endpoint(service_name).await;
```

## Future Enhancements

1. **Service Mesh**: Add service mesh capabilities for advanced traffic management
2. **Metrics Collection**: Collect and expose detailed service metrics
3. **Advanced Routing**: Support for path-based routing and traffic splitting
4. **Service Versioning**: Support for blue/green deployments and canary releases
5. **External Service Integration**: Integration with external service discovery systems
