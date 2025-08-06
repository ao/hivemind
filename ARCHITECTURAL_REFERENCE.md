# Hivemind Architectural Reference

This document provides a comprehensive architectural reference for the Hivemind container orchestration platform, focusing on the specific components identified in the debug report. It clarifies the intended design and provides guidance for implementing fixes.

## 1. Service Discovery and Health Monitoring

### Intended Design

The Service Discovery system in Hivemind is responsible for:
- Maintaining a registry of all services and their endpoints
- Providing DNS resolution for service names
- Monitoring the health of service endpoints
- Distributing traffic across healthy endpoints

### Health Monitoring Approach

Based on the code and documentation review, the health monitoring in the Service Discovery component is designed to be **handled internally** by the Service Discovery system itself. The `Discovery` class initializes the health monitoring system in its `Initialize` method:

```go
// Initialize initializes the service discovery system
func (d *Discovery) Initialize() error {
    d.logger.Info("Initializing service discovery system")

    // Start the health check system
    go d.startHealthCheckSystem()

    // Start the DNS server
    go d.startDNSServer()

    // Start the proxy server
    go d.startProxyServer()

    // If we have a network manager, set up the integration
    d.networkMutex.RLock()
    networkManager := d.networkManager
    d.networkMutex.RUnlock()

    if networkManager != nil {
        d.setupNetworkIntegration()
    }

    return nil
}
```

The health monitoring is started automatically as part of the initialization process through the `startHealthCheckSystem()` method, which runs health checks periodically:

```go
// startHealthCheckSystem starts the health check system
func (d *Discovery) startHealthCheckSystem() {
    d.logger.Info("Starting health check system")

    // Run health checks periodically
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            d.runHealthChecks()
        }
    }
}
```

**Clarification**: There should NOT be a separate `StartHealthMonitoring` method in the Service Discovery component. Health monitoring is an internal function that is automatically started during initialization.

## 2. Node Resources

### Intended Design

The Node Manager in Hivemind manages cluster coordination and node discovery, including tracking node resources.

### Resource Type Structure

Based on the code and documentation review, there are two resource-related structures in the Node Manager:

1. `Resources` - A simple structure for basic resource information:
```go
// Resources represents the resources of a node
type Resources struct {
    CPU    int
    Memory int
    Disk   int64
}
```

2. `NodeResources` - A more detailed structure that includes additional fields:
```go
// NodeResources represents the resources available on a node
type NodeResources struct {
    CPUAvailable      float64
    MemoryAvailable   uint64
    ContainersRunning uint32
    IsLeader          bool
    CPU               int
    Memory            int
    Disk              int64
}
```

**Clarification**: Both structures are valid and serve different purposes:
- `Resources` is used for basic resource information and is the parameter type for methods like `UpdateNodeResources`
- `NodeResources` is used for detailed resource tracking and is stored in the `NodeInfo` structure

The correct usage is:
- Use `Resources` when passing resource information to methods
- Use `NodeResources` when storing detailed resource information in node objects

## 3. Service Endpoints

### Intended Design

The Service Discovery system maintains a registry of services and their endpoints, allowing containers to find and communicate with each other.

### Endpoint Type Structure

Based on the code and documentation review, the correct type for service endpoints is `ServiceEndpoint`:

```go
// ServiceEndpoint represents an endpoint for a service
type ServiceEndpoint struct {
    ServiceName     string
    Domain          string
    IPAddress       string
    Port            uint16
    NodeID          string
    HealthStatus    ServiceHealth
    LastHealthCheck int64
    Version         string
    Weight          uint32
    Metadata        map[string]string
}
```

This is confirmed by:
1. The type definition in `internal/service/discovery.go`
2. Usage in the `Discovery` struct's `services` field: `services map[string][]ServiceEndpoint`
3. The API reference showing endpoints with similar fields
4. The service discovery documentation referring to endpoints with these properties

**Clarification**: The correct type is `ServiceEndpoint`, not `Endpoint`. There is no `service.Endpoint` type in the codebase.

## 4. IPAM Manager

### Intended Design

The IPAM (IP Address Management) Manager is responsible for allocating and managing IP addresses for containers and subnets for nodes.

### IP Allocation Method

Based on the code and documentation review, the correct method for allocating IP addresses to containers is `AllocateIP`:

```go
// AllocateIP allocates an IP address for a container
func (m *IPAMManager) AllocateIP(ctx context.Context, subnetCIDR, containerID string) (*IPAllocation, error) {
    // Implementation details...
}
```

This is confirmed by:
1. The method definition in `internal/security/ipam_manager.go`
2. The container networking documentation referring to IP allocation for containers
3. The container networking implementation document describing the IPAM system's role in allocating IP addresses

**Clarification**: The correct method name is `AllocateIP`, not `AllocateContainerIP`. The method takes a container ID as a parameter, making the "Container" part in the name redundant.

## 5. Component Relationships

### Service Discovery and Health Monitoring

- The Service Discovery component internally manages health monitoring
- Health checks are started automatically during initialization
- No separate `StartHealthMonitoring` method is needed

### Node Manager and Resources

- The Node Manager uses both `Resources` and `NodeResources` structures
- `Resources` is used for method parameters
- `NodeResources` is used for storing detailed resource information

### Service Discovery and Endpoints

- Services are registered with the Service Discovery system
- Each service can have multiple endpoints (containers)
- Endpoints are represented by the `ServiceEndpoint` type

### IPAM Manager and Network Manager

- The IPAM Manager is responsible for IP address allocation
- The Network Manager uses the IPAM Manager to allocate IPs for containers
- The `AllocateIP` method is used to allocate IPs to containers

## 6. Implementation Recommendations

1. **Service Discovery Health Monitoring**:
   - Do not add a separate `StartHealthMonitoring` method
   - Ensure health monitoring is started in the `Initialize` method

2. **Node Resources**:
   - Use `Resources` for method parameters
   - Use `NodeResources` for storing detailed resource information
   - Ensure consistency in field names and types

3. **Service Endpoints**:
   - Use `ServiceEndpoint` consistently throughout the codebase
   - Ensure all references to endpoints use this type

4. **IPAM Manager**:
   - Use `AllocateIP` for allocating IP addresses to containers
   - Ensure the method signature is consistent with the intended design

By following these recommendations, the codebase will be aligned with the intended architecture and design of the Hivemind platform.