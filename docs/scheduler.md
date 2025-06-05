# Hivemind Scheduler System

This document provides a comprehensive overview of the Scheduler system in Hivemind, explaining how containers are placed on nodes, how resource allocation works, and how to configure and use the scheduling features.

## Overview

The Scheduler system in Hivemind is responsible for deciding where to place containers in the cluster. It takes into account various factors such as resource availability, node health, network topology, and user-defined constraints to make optimal placement decisions.

## Key Features

- **Resource-Aware Scheduling**: Places containers based on CPU, memory, and other resource requirements
- **Network-Aware Scheduling**: Optimizes container placement based on network topology
- **Affinity/Anti-Affinity Rules**: Places related containers together or apart based on rules
- **Taints and Tolerations**: Controls which containers can be scheduled on which nodes
- **Priority-Based Scheduling**: Handles containers with different priority levels
- **Dynamic Rebalancing**: Moves containers to optimize resource usage and performance

## Scheduling Components

### Container Scheduler

The Container Scheduler is the core component responsible for container placement:

1. **Node Selection**: Selects the best node for each container
2. **Resource Allocation**: Allocates resources for containers
3. **Constraint Evaluation**: Evaluates placement constraints
4. **Scoring**: Scores nodes based on various factors
5. **Decision Making**: Makes the final placement decision

### Load Balancer

The Load Balancer component ensures even distribution of containers across nodes:

1. **Load Calculation**: Calculates the load on each node
2. **Imbalance Detection**: Detects when the cluster is imbalanced
3. **Rebalancing**: Moves containers to balance the load

### Network Topology Manager

The Network Topology Manager optimizes container placement based on network considerations:

1. **Topology Discovery**: Discovers the network topology of the cluster
2. **Distance Calculation**: Calculates network distances between nodes
3. **Placement Optimization**: Optimizes container placement for network performance

### Service Affinity Manager

The Service Affinity Manager handles affinity and anti-affinity rules:

1. **Rule Evaluation**: Evaluates affinity and anti-affinity rules
2. **Constraint Generation**: Generates placement constraints based on rules
3. **Scoring Adjustment**: Adjusts node scores based on affinity rules

## Using the Scheduler

### Basic Container Deployment

For basic container deployment, the scheduler automatically selects the best node:

```bash
hivemind app deploy --image nginx:latest --name web
```

The scheduler will place the container on the most suitable node based on resource availability and other factors.

### Resource Requirements

You can specify resource requirements for containers:

```bash
hivemind app deploy --image nginx:latest --name web \
  --cpu 0.5 --memory 256M
```

This tells the scheduler that the container needs 0.5 CPU cores and 256 MB of memory.

You can also specify resource limits:

```bash
hivemind app deploy --image nginx:latest --name web \
  --cpu 0.5 --cpu-limit 1.0 \
  --memory 256M --memory-limit 512M
```

This specifies both the requested resources and the maximum limits.

### Node Selection

You can influence node selection in several ways:

#### Specific Node

To deploy a container on a specific node:

```bash
hivemind app deploy --image nginx:latest --name web --node node1
```

#### Node Affinity

To deploy a container on nodes with specific labels:

```bash
hivemind app deploy --image nginx:latest --name web --node-affinity "role=frontend"
```

This will place the container on nodes with the label `role=frontend`.

#### Node Anti-Affinity

To avoid deploying a container on nodes with specific labels:

```bash
hivemind app deploy --image nginx:latest --name web --node-anti-affinity "role=database"
```

This will avoid placing the container on nodes with the label `role=database`.

### Service Affinity

You can define affinity between services:

```bash
hivemind app deploy --image nginx:latest --name web --service-affinity "app=api"
```

This will try to place the container on the same node as containers with the label `app=api`.

### Service Anti-Affinity

You can define anti-affinity between services:

```bash
hivemind app deploy --image nginx:latest --name web --service-anti-affinity "app=db"
```

This will try to avoid placing the container on the same node as containers with the label `app=db`.

### Taints and Tolerations

You can add taints to nodes to prevent certain containers from being scheduled on them:

```bash
hivemind node taint --node node1 --key special --value true --effect NoSchedule
```

This adds a taint to `node1` that prevents containers from being scheduled on it unless they have a matching toleration.

To add a toleration to a container:

```bash
hivemind app deploy --image nginx:latest --name web --toleration "special=true:NoSchedule"
```

This allows the container to be scheduled on nodes with the `special=true` taint.

### Priority Classes

You can define priority classes for containers:

```bash
hivemind priority-class create --name high-priority --value 100
hivemind priority-class create --name medium-priority --value 50
hivemind priority-class create --name low-priority --value 10
```

To deploy a container with a specific priority:

```bash
hivemind app deploy --image nginx:latest --name web --priority-class high-priority
```

Higher priority containers are scheduled before lower priority ones and can preempt lower priority containers if resources are scarce.

## Scheduler Configuration

### Global Configuration

You can configure global scheduler settings:

```bash
hivemind admin config set scheduler.rebalance_threshold 20
hivemind admin config set scheduler.scheduling_interval 30
```

Available settings:
- `scheduler.rebalance_threshold`: Percentage difference in load that triggers rebalancing
- `scheduler.scheduling_interval`: How often the scheduler runs (in seconds)
- `scheduler.bin_packing_strategy`: Strategy for bin packing (BestFit, WorstFit, FirstFit, Random)
- `scheduler.cpu_overcommit_ratio`: How much to overcommit CPU resources
- `scheduler.memory_overcommit_ratio`: How much to overcommit memory resources

### Bin Packing Strategies

The scheduler supports different bin packing strategies:

- **BestFit**: Packs containers onto as few nodes as possible
- **WorstFit**: Spreads containers across nodes evenly
- **FirstFit**: Uses the first node that fits
- **Random**: Randomly selects a node that fits

To set the bin packing strategy:

```bash
hivemind admin config set scheduler.bin_packing_strategy BestFit
```

### Resource Overcommitment

You can configure resource overcommitment to allow more efficient resource usage:

```bash
hivemind admin config set scheduler.cpu_overcommit_ratio 1.5
hivemind admin config set scheduler.memory_overcommit_ratio 1.2
```

This allows the scheduler to allocate more resources than physically available, assuming not all containers will use their maximum allocation simultaneously.

## Implementation Details

### Scheduling Algorithm

The scheduling algorithm follows these steps:

1. **Filtering**: Filter out nodes that don't meet basic requirements
2. **Scoring**: Score each remaining node based on various factors
3. **Selection**: Select the node with the highest score

#### Filtering

Nodes are filtered out if:
- They don't have enough resources
- They have taints that the container doesn't tolerate
- They don't match node affinity requirements
- They are in maintenance mode or unhealthy

#### Scoring

Nodes are scored based on:
- Resource availability (CPU, memory, etc.)
- Network proximity to related containers
- Affinity/anti-affinity rules
- Current load
- Bin packing strategy

The scoring formula is:

```
score = (1.0 - node_load) * 0.4                   // Base score (0-0.4)
      + (network_proximity * related_count) * 0.3  // Network score (0-0.3)
      + (affinity_bonus) * 0.2                    // Affinity bonus (0-0.2)
      - (anti_affinity_penalty) * 0.2             // Anti-affinity penalty (0-0.2)
      + (strategy_adjustment) * 0.1               // Strategy adjustment (0-0.1)
```

#### Selection

The node with the highest score is selected for the container. If multiple nodes have the same score, one is chosen randomly.

### Network Topology Awareness

The scheduler uses network topology information to optimize container placement:

1. **Topology Discovery**: The network topology is discovered by measuring the network distance between nodes
2. **Distance Calculation**: Network distance is calculated based on latency, bandwidth, and reliability
3. **Proximity Calculation**: Proximity between nodes is calculated as the inverse of distance
4. **Placement Optimization**: Containers that communicate frequently are placed on nodes with high proximity

### Dynamic Rebalancing

The scheduler periodically checks for load imbalances in the cluster:

1. **Load Calculation**: Calculate the load on each node
2. **Imbalance Detection**: Check if the difference between the most and least loaded nodes exceeds the threshold
3. **Container Selection**: Select containers to move from the most loaded nodes
4. **Target Selection**: Select target nodes for the containers
5. **Container Migration**: Move the containers to the target nodes

## Advanced Features

### Network-Aware Scheduling

The scheduler can optimize container placement based on network topology:

```bash
hivemind app deploy --image nginx:latest --name web --network-aware
```

This will place the container on a node that minimizes network latency to related containers.

### Service Affinity Groups

You can define affinity groups for services:

```bash
hivemind affinity-group create --name web-tier --services "frontend,api,cache"
```

This ensures that containers from these services are placed on the same or nearby nodes.

### Topology Zones

You can define topology zones for nodes:

```bash
hivemind node set-topology --node node1 --zone zone1 --region region1
hivemind node set-topology --node node2 --zone zone1 --region region1
hivemind node set-topology --node node3 --zone zone2 --region region1
```

To deploy containers with topology constraints:

```bash
hivemind app deploy --image nginx:latest --name web --topology-key "topology.zone" --topology-value "zone1"
```

This will place the container on nodes in the specified topology zone.

### Custom Scheduling Constraints

You can add custom scheduling constraints:

```bash
hivemind node add-constraint --node node1 --key "gpu" --value "true"
```

To deploy containers with custom constraints:

```bash
hivemind app deploy --image tensorflow:latest --name ml-model --constraint "gpu=true"
```

This will place the container on nodes that match the constraint.

## Best Practices

### Resource Allocation

1. **Specify resource requirements**: Always specify CPU and memory requirements for containers
2. **Be realistic**: Request resources that your application actually needs
3. **Set appropriate limits**: Set limits higher than requests but not too high
4. **Consider overcommitment**: Use resource overcommitment for better efficiency

### Affinity Rules

1. **Use service affinity for related services**: Place services that communicate frequently on the same node
2. **Use anti-affinity for high-availability**: Spread replicas across different nodes
3. **Don't overuse constraints**: Too many constraints can make scheduling difficult
4. **Balance constraints with resource efficiency**: Consider both placement constraints and resource usage

### Node Management

1. **Label nodes appropriately**: Use meaningful labels for nodes
2. **Use taints for special nodes**: Add taints to nodes with special hardware or requirements
3. **Maintain node homogeneity**: Keep nodes as similar as possible for easier scheduling
4. **Monitor node resources**: Regularly check resource usage on nodes

## Troubleshooting

### Common Issues

#### Container Pending

**Symptoms:**
- Container stays in pending state
- Container not being scheduled

**Possible Causes:**
- Insufficient resources
- No nodes match constraints
- Taints preventing scheduling

**Solutions:**

1. **Check resource availability:**
   ```bash
   hivemind node resources
   ```

2. **Check node constraints:**
   ```bash
   hivemind node ls --show-labels
   ```

3. **Check node taints:**
   ```bash
   hivemind node ls --show-taints
   ```

4. **Check scheduler logs:**
   ```bash
   RUST_LOG=debug hivemind logs | grep scheduler
   ```

#### Uneven Load Distribution

**Symptoms:**
- Some nodes are heavily loaded while others are idle
- Poor performance on overloaded nodes

**Possible Causes:**
- Rebalancing threshold too high
- Too many placement constraints
- Bin packing strategy issues

**Solutions:**

1. **Check load distribution:**
   ```bash
   hivemind node resources
   ```

2. **Lower rebalancing threshold:**
   ```bash
   hivemind admin config set scheduler.rebalance_threshold 10
   ```

3. **Change bin packing strategy:**
   ```bash
   hivemind admin config set scheduler.bin_packing_strategy WorstFit
   ```

4. **Manually trigger rebalancing:**
   ```bash
   hivemind scheduler rebalance
   ```

#### Resource Overcommitment Issues

**Symptoms:**
- Containers being killed due to OOM (Out of Memory)
- Poor performance due to CPU contention

**Possible Causes:**
- Overcommitment ratios too high
- Containers using more resources than requested
- Resource limits not set properly

**Solutions:**

1. **Check actual resource usage:**
   ```bash
   hivemind app stats --name <app-name>
   ```

2. **Adjust overcommitment ratios:**
   ```bash
   hivemind admin config set scheduler.cpu_overcommit_ratio 1.2
   hivemind admin config set scheduler.memory_overcommit_ratio 1.1
   ```

3. **Set appropriate resource requests and limits:**
   ```bash
   hivemind app update --name <app-name> --cpu 1.0 --memory 512M --cpu-limit 2.0 --memory-limit 1G
   ```

## Conclusion

The Scheduler system in Hivemind provides sophisticated container placement capabilities that optimize resource usage, network performance, and application reliability. By understanding and properly configuring the scheduler, you can ensure that your containers are placed optimally in the cluster.

For more information, refer to the following resources:
- [User Guide](user_guide.md)
- [Administration Guide](administration_guide.md)
- [Troubleshooting Guide](troubleshooting_guide.md)
- [Container Networking Scheduler](container_networking_scheduler.md)