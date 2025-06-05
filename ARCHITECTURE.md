# Hivemind Architecture

This document provides a comprehensive overview of the Hivemind container orchestration platform's architecture, explaining how the various components work together to provide a robust, distributed system.

## System Overview

Hivemind is a lightweight container orchestration platform built in Rust, designed to provide Kubernetes-like features with significantly lower complexity and resource requirements. The system follows a modular architecture with clear separation of concerns between components.

![Hivemind Architecture](./assets/logo1.png)

## Core Components

### App Manager

The App Manager is responsible for application and container lifecycle management:

- **Container Deployment**: Handles the deployment of containers from images
- **Application Scaling**: Manages the scaling of applications up or down
- **Container Lifecycle**: Manages the lifecycle of containers (create, start, stop, remove)
- **Service Configuration**: Manages service configurations for applications

Key interfaces:
- `AppManager`: Main interface for application management
- `ServiceConfig`: Configuration for services

### Node Manager

The Node Manager handles cluster coordination and node discovery:

- **Node Registration**: Registers nodes in the cluster
- **Node Status**: Tracks the status of nodes
- **Resource Tracking**: Monitors node resources (CPU, memory, disk)
- **Node Coordination**: Coordinates operations across nodes

Key interfaces:
- `NodeManager`: Main interface for node management
- `NodeResources`: Represents node resources

### Node Membership Protocol

The Node Membership Protocol implements the SWIM protocol for cluster membership management:

- **Failure Detection**: Detects node failures through direct and indirect pinging
- **Dissemination**: Spreads membership information through gossip
- **Suspicion Mechanism**: Reduces false positives by marking nodes as suspected before declaring them dead
- **Leader Election**: Elects a leader node for coordination tasks

Key interfaces:
- `MembershipProtocol`: Implements the SWIM protocol
- `Member`: Represents a node in the cluster
- `NodeState`: Represents the state of a node (Alive, Suspected, Dead, Left)

### Service Discovery

The Service Discovery system enables containers and applications to find and communicate with each other:

- **Service Registry**: Maintains a registry of all services and their endpoints
- **DNS Server**: Provides DNS resolution for service names
- **Health Checking**: Monitors the health of service endpoints
- **Load Balancing**: Distributes traffic across healthy endpoints

Key interfaces:
- `ServiceDiscovery`: Main interface for service discovery
- `LoadBalancingStrategy`: Strategies for load balancing (RoundRobin, LeastConnections, etc.)

### Storage Manager

The Storage Manager handles volume and persistence management:

- **Volume Creation**: Creates persistent volumes
- **Volume Mounting**: Mounts volumes to containers
- **Volume Deletion**: Deletes volumes
- **Volume Monitoring**: Monitors volume usage

Key interfaces:
- `StorageManager`: Main interface for storage management

### Container Manager

The Container Manager integrates with the container runtime:

- **Container Runtime Integration**: Integrates with containerd
- **Container Operations**: Performs container operations (create, start, stop, remove)
- **Image Management**: Manages container images
- **Container Monitoring**: Monitors container status and metrics

Key interfaces:
- `ContainerdManager`: Integrates with containerd
- `Container`: Represents a container
- `ContainerStatus`: Represents the status of a container

### Network Manager

The Network Manager handles container networking:

- **IP Address Management**: Allocates and manages IP addresses
- **Overlay Network**: Creates a virtual network spanning all nodes
- **Network Policies**: Controls traffic flow between containers
- **Network Monitoring**: Monitors network health and performance

Key interfaces:
- `NetworkManager`: Main interface for network management
- `IpamManager`: Manages IP address allocation
- `OverlayNetwork`: Manages the overlay network
- `NetworkPolicy`: Defines network policies

### Scheduler

The Scheduler is responsible for container placement:

- **Node Selection**: Selects the best node for container placement
- **Resource Allocation**: Allocates resources for containers
- **Affinity/Anti-Affinity**: Places containers based on affinity rules
- **Network Topology Awareness**: Considers network topology for placement

Key interfaces:
- `ContainerScheduler`: Main interface for container scheduling
- `BinPackingStrategy`: Strategies for container placement

### Health Monitor

The Health Monitor tracks the health of containers and nodes:

- **Container Health Checking**: Monitors container health
- **Node Health Checking**: Monitors node health
- **Auto-Healing**: Automatically restarts failed containers
- **Alerting**: Generates alerts for health issues

Key interfaces:
- `HealthMonitor`: Main interface for health monitoring
- `HealthStatus`: Represents health status
- `Alert`: Represents a health alert

### Security Manager

The Security Manager provides comprehensive security features:

- **Container Scanning**: Scans container images for vulnerabilities
- **Network Policy Enforcement**: Enforces network security policies
- **RBAC**: Manages role-based access control
- **Secret Management**: Securely stores and distributes secrets

Key interfaces:
- `SecurityManager`: Main interface for security management
- `ContainerScanner`: Scans container images
- `NetworkPolicyEnforcer`: Enforces network policies
- `RbacManager`: Manages RBAC
- `SecretManager`: Manages secrets

### Web UI

The Web UI provides a dashboard for monitoring and management:

- **Dashboard**: Provides an overview of the system
- **Container Management**: Manages containers through the UI
- **Node Management**: Manages nodes through the UI
- **Monitoring**: Displays monitoring information

## Data Flow

### Container Deployment Flow

1. User submits a container deployment request via CLI or API
2. App Manager validates the request
3. Scheduler selects the best node for the container
4. Container Manager creates the container on the selected node
5. Network Manager sets up networking for the container
6. Service Discovery registers the container's service
7. Health Monitor begins monitoring the container's health

### Service Discovery Flow

1. Container needs to communicate with a service
2. Container performs a DNS lookup for the service name
3. Service Discovery resolves the service name to an endpoint
4. Service Discovery selects an endpoint based on load balancing strategy
5. Container connects directly to the selected endpoint

### Node Membership Flow

1. Node starts up and joins the cluster
2. Node Membership Protocol registers the node
3. Node begins periodic pinging of other nodes
4. If a node fails to respond, indirect pinging is attempted
5. If indirect pinging fails, the node is marked as suspected
6. If the node remains unresponsive, it is marked as dead
7. Membership changes are disseminated through gossip

### Health Monitoring Flow

1. Health Monitor periodically checks container health
2. If a container is unhealthy, it is marked for restart
3. Container Manager restarts the unhealthy container
4. Health Monitor also checks node health
5. If a node is unhealthy, containers are rescheduled to other nodes

### Security Flow

1. Container image is scanned for vulnerabilities before deployment
2. Network policies are applied to control traffic between containers
3. User authentication and authorization is performed via RBAC
4. Secrets are securely stored and distributed to containers

## Component Interactions

### App Manager and Scheduler

The App Manager works with the Scheduler to place containers on nodes:

1. App Manager receives a deployment request
2. App Manager asks the Scheduler to select a node
3. Scheduler evaluates nodes based on resources, affinity, and network topology
4. Scheduler returns the selected node
5. App Manager deploys the container to the selected node

### Network Manager and Service Discovery

The Network Manager works with Service Discovery to enable container communication:

1. Network Manager sets up networking for a container
2. Network Manager assigns an IP address to the container
3. Service Discovery registers the container's service
4. Service Discovery provides DNS resolution for the service
5. Containers can communicate using service names

### Health Monitor and Container Manager

The Health Monitor works with the Container Manager to ensure container health:

1. Health Monitor checks container health
2. If a container is unhealthy, Health Monitor notifies Container Manager
3. Container Manager restarts the unhealthy container
4. Health Monitor continues monitoring the container

### Security Manager and App Manager

The Security Manager works with the App Manager to ensure secure deployments:

1. App Manager receives a deployment request
2. Security Manager scans the container image
3. Security Manager checks compliance with security policies
4. If compliant, App Manager proceeds with deployment
5. Security Manager applies network policies to the container

## Scalability and Performance

Hivemind is designed to be highly scalable and performant:

- **Lightweight Components**: All components are designed to be lightweight and efficient
- **Rust Implementation**: Built in Rust for memory safety and performance
- **Efficient Protocols**: Uses efficient protocols like SWIM for membership management
- **Minimal Resource Usage**: Requires minimal resources compared to other orchestration platforms
- **Network-Aware Scheduling**: Optimizes container placement for network performance
- **Efficient Service Discovery**: Fast DNS-based service discovery

## Fault Tolerance

Hivemind provides robust fault tolerance:

- **Node Failure Detection**: Quickly detects node failures
- **Container Auto-Healing**: Automatically restarts failed containers
- **Leader Election**: Elects a new leader if the current leader fails
- **Distributed State**: Maintains distributed state across nodes
- **Graceful Degradation**: Continues operating with reduced functionality during failures

## Security

Hivemind includes comprehensive security features:

- **Container Scanning**: Scans container images for vulnerabilities
- **Network Policies**: Controls traffic flow between containers
- **RBAC**: Controls access to resources
- **Secret Management**: Securely stores and distributes secrets
- **Audit Logging**: Logs all security-related events

## Extensibility

Hivemind is designed to be extensible:

- **Modular Architecture**: Clear separation of concerns between components
- **Plugin System**: Supports plugins for extending functionality
- **API-First Design**: All functionality is available through APIs
- **Custom Schedulers**: Supports custom scheduling algorithms
- **Custom Health Checks**: Supports custom health checking logic

## Conclusion

Hivemind's architecture provides a robust, scalable, and secure platform for container orchestration. The modular design allows for easy extension and customization, while the lightweight implementation ensures efficient resource usage. The combination of these features makes Hivemind an excellent choice for organizations looking for a simpler alternative to Kubernetes without sacrificing essential functionality.