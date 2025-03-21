# Network-Aware Container Scheduling in Hivemind

This document describes the network-aware container scheduling implementation in Hivemind, which optimizes container placement based on network topology and service requirements.

## Overview

Hivemind's container scheduler now incorporates network topology awareness to make intelligent placement decisions. This ensures that containers are placed on nodes that provide optimal network performance for their communication patterns, reducing latency and improving overall application performance.

### Key Features

1. **Network Topology Awareness**: Considers the network distance between nodes when placing containers
2. **Service Affinity/Anti-Affinity**: Places related services together or apart based on defined rules
3. **Load-Balanced Placement**: Distributes containers across nodes while considering network constraints
4. **Dynamic Rebalancing**: Moves containers to optimize resource usage and network performance

## Architecture

The network-aware scheduler extends the basic container scheduler with additional components:

```
┌─────────────────────────────────────────────────────────┐
│                  Container Scheduler                    │
│                                                         │
│  ┌─────────────────┐  ┌─────────────────────────────┐   │
│  │  Load Balancer  │  │  Network Topology Manager   │   │
│  └─────────────────┘  └─────────────────────────────┘   │
│                                                         │
│  ┌─────────────────┐  ┌─────────────────────────────┐   │
│  │ Service Affinity│  │ Container Placement Engine  │   │
│  │    Manager      │  │                             │   │
│  └─────────────────┘  └─────────────────────────────┘   │
│                                                         │
└─────────────────────────────────────────────────────────┘
            │                       │
            ▼                       ▼
┌─────────────────────┐  ┌─────────────────────┐
│   Node Manager      │  │  Network Manager    │
└─────────────────────┘  └─────────────────────┘
```

## Network Topology Discovery

### Automatic Distance Measurement

The scheduler automatically discovers and measures the network distance between nodes:

1. For each pair of nodes in the cluster, the network distance is measured
2. Distances are normalized to a 0-100 scale (where lower is better)
3. The topology map is periodically updated to reflect changes in network conditions

### Network Distance Calculation

Network distance is calculated using several metrics:

- **Latency**: Round-trip time between nodes
- **Bandwidth**: Available bandwidth between nodes
- **Reliability**: Packet loss and connection stability

In the current implementation, network distance is simulated based on node IDs, but in a production environment, it would use actual network measurements.

## Container Placement Algorithm

### Scoring System

The scheduler uses a sophisticated scoring system to determine the best node for each container:

1. **Base Score**: Inverse of the node's current load (CPU, memory, container count)
2. **Affinity Bonus**: Added when a node matches service affinity rules
3. **Anti-Affinity Penalty**: Subtracted when a node matches service anti-affinity rules
4. **Co-location Bonus**: Added when a node already runs containers from the same service
5. **Network Score**: Based on network distance to related containers

### Placement Decision Flow

When placing a new container:

1. Calculate base scores for all available nodes
2. Apply service affinity/anti-affinity rules
3. Consider service co-location preferences
4. Evaluate network topology for related containers
5. Select the node with the highest final score

```rust
// Pseudocode for node scoring
score = (1.0 - node_load)                   // Base score (0-1)
      + (0.3 if matches_affinity)           // Affinity bonus
      - (0.5 if matches_antiaffinity)       // Anti-affinity penalty
      + (0.2 if has_same_service)           // Co-location bonus
      + (network_proximity * related_count) // Network score
```

## Service Affinity and Anti-Affinity

### Affinity Rules

Service affinity rules specify that containers of a service should be placed on specific nodes:

```rust
scheduler.set_service_affinity("database", vec!["node1", "node2"]);
```

This gives a significant scoring bonus to the specified nodes when placing containers of the "database" service.

### Anti-Affinity Rules

Service anti-affinity rules specify that containers of a service should avoid specific nodes:

```rust
scheduler.set_service_antiaffinity("frontend", vec!["node3"]);
```

This applies a scoring penalty to the specified nodes when placing containers of the "frontend" service.

## Load Balancing and Rebalancing

### Initial Placement

During initial placement, the scheduler distributes containers across nodes based on:

1. Current node load (CPU, memory, running containers)
2. Network topology considerations
3. Service affinity/anti-affinity rules

### Dynamic Rebalancing

The scheduler periodically checks for load imbalances and rebalances containers if needed:

1. Identifies the most and least loaded nodes
2. Selects containers from the most loaded node that are suitable for moving
3. Evaluates potential target nodes based on all constraints
4. Moves containers to achieve better balance while respecting network constraints

### Rebalancing Constraints

When selecting containers to move, the scheduler considers:

1. Number of constraints on each container
2. Impact on service performance
3. Network dependencies with other containers
4. Cost of moving vs. benefit of rebalancing

## Configuration

### Scheduler Parameters

| Parameter | Default | Description |
|-----------|---------|-------------|
| `scheduling_interval` | 30s | How often the scheduler runs |
| `rebalance_threshold` | 20% | Load difference that triggers rebalancing |

### Network Topology Configuration

Network topology can be manually configured for testing or special cases:

```rust
scheduler.update_network_topology("node1", "node2", 25.0); // Distance of 25 (lower is better)
```

In production, these values are automatically discovered and updated.

## Integration with Service Discovery

The network-aware scheduler works closely with the service discovery system:

1. When containers are placed or moved, service discovery records are updated
2. Service affinity rules can be derived from service dependencies
3. Network topology information can influence DNS resolution order

## Example Scenarios

### Scenario 1: Database and Application Containers

Consider a scenario with a database service and multiple application instances:

1. Database containers have affinity to high-memory nodes
2. Application containers are placed close to their database instances
3. As load increases, new application containers are distributed across nodes
4. If a node becomes overloaded, containers are rebalanced while maintaining proximity

### Scenario 2: Microservices Communication

In a microservices architecture:

1. Services with high communication volume are placed on the same or nearby nodes
2. Services with conflicting resource needs are placed on different nodes
3. As communication patterns change, container placement is adjusted

## Monitoring and Debugging

### Logging

The scheduler logs detailed information about its decisions:

```
Node node1 load: 0.45 (CPU: 60.00%, Memory: 4096 MB, Containers: 5)
Node node2 load: 0.30 (CPU: 40.00%, Memory: 8192 MB, Containers: 3)
Node node1 score for container web-server: 0.85
Node node2 score for container web-server: 0.92
Selected node node2 for container web-server
```

### Metrics

Key metrics to monitor:

1. **Placement Success Rate**: Percentage of containers successfully placed
2. **Rebalancing Frequency**: How often rebalancing occurs
3. **Node Load Distribution**: Standard deviation of load across nodes
4. **Network Distance**: Average network distance between communicating containers

## Future Enhancements

1. **Machine Learning-Based Placement**: Use historical data to predict optimal placement
2. **Application-Aware Scheduling**: Consider application-specific communication patterns
3. **Multi-Cluster Awareness**: Extend network topology to span multiple clusters
4. **QoS-Based Scheduling**: Prioritize placement based on service quality requirements
5. **Failure Domain Awareness**: Distribute containers across failure domains
