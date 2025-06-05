# Container Networking Implementation in Hivemind

This document provides a detailed overview of the container networking implementation in Hivemind, focusing on the VXLAN overlay network, IP address management (IPAM), cross-node communication, and network policies.

## Architecture Overview

The Hivemind container networking stack consists of several key components:

1. **NetworkManager**: The central component that orchestrates all networking operations
2. **IpamManager**: Manages IP address allocation and subnet management
3. **OverlayNetwork**: Implements the overlay network (VXLAN, with support for other types)
4. **NetworkPolicyManager**: Enforces network policies for security and traffic control

### Component Diagram

```
┌─────────────────────────────────────────────────────────────────┐
│                       NetworkManager                            │
├─────────────┬─────────────┬────────────────┬───────────────────┤
│ IpamManager │ OverlayNet  │ NetworkPolicy  │ Container Network │
│             │             │    Manager     │     Registry      │
└─────────────┴─────────────┴────────────────┴───────────────────┘
        │             │              │                │
        ▼             ▼              ▼                ▼
┌─────────────┐ ┌──────────┐ ┌────────────┐  ┌─────────────────┐
│   Subnet    │ │  VXLAN   │ │  iptables  │  │ Container-to-IP │
│ Allocation  │ │ Tunnels  │ │   Rules    │  │    Mapping      │
└─────────────┘ └──────────┘ └────────────┘  └─────────────────┘
```

## VXLAN Overlay Network

The VXLAN (Virtual Extensible LAN) overlay network creates a virtual Layer 2 network that spans across all nodes in the cluster, allowing containers to communicate as if they were on the same local network.

### Implementation Details

- **VXLAN ID**: Default is 42, configurable
- **VXLAN Port**: Default is 4789 (standard VXLAN port)
- **MTU**: Default is 1450 (1500 - 50 for VXLAN overhead)
- **Encryption**: Optional, configurable via the `enable_encryption` flag

### Network Setup Process

1. Each node creates a VXLAN interface (`vxlan0` by default)
2. A bridge (`hivemind0`) is created on each node
3. The VXLAN interface is connected to the bridge
4. Containers are connected to the bridge via veth pairs
5. FDB (Forwarding Database) entries are added for remote nodes
6. Routes are added for remote subnets

### Cross-Node Communication

When a container on Node A communicates with a container on Node B:

1. The packet is sent to Node A's bridge
2. The bridge forwards the packet to the VXLAN interface
3. The VXLAN interface encapsulates the packet in a UDP packet
4. The UDP packet is sent to Node B's IP address
5. Node B's VXLAN interface decapsulates the packet
6. The packet is forwarded to Node B's bridge
7. The bridge delivers the packet to the destination container

## IP Address Management (IPAM)

The IPAM system manages IP address allocation for containers and subnets for nodes.

### Subnet Allocation

- The cluster uses a large CIDR block (default: `10.244.0.0/16`)
- Each node is allocated a subnet (default: `/24`)
- This allows for up to 256 nodes with 254 containers per node

### IP Address Allocation

- Each container is allocated an IP address from its node's subnet
- The first IP in the subnet is used as the gateway (bridge IP)
- IP addresses are recycled when containers are terminated
- Support for static IP assignment is available

### IP Recycling

When a container is terminated, its IP address is added to a recycling pool. When a new container is created, the IPAM system first checks if there are any recycled IPs available before allocating a new one.

## Network Policies

Network policies provide a way to control traffic flow between containers for security purposes.

### Policy Structure

```rust
NetworkPolicy {
    name: String,
    selector: NetworkSelector { labels: HashMap<String, String> },
    ingress_rules: Vec<NetworkRule>,
    egress_rules: Vec<NetworkRule>,
    priority: i32,
    namespace: Option<String>,
    labels: HashMap<String, String>,
    created_at: u64,
    updated_at: u64,
}
```

### Enhanced Policies

Enhanced policies extend the basic network policy with additional features:

```rust
EnhancedNetworkPolicy {
    base_policy: NetworkPolicy,
    policy_type: PolicyType, // Isolation, Security, QoS, Custom
    applies_to_ingress: bool,
    applies_to_egress: bool,
    action: PolicyAction, // Allow, Deny, Limit(rate), Log
    log_violations: bool,
}
```

### Policy Enforcement

Network policies are enforced using iptables rules:

1. For each policy, iptables chains are created
2. Rules are added to the chains based on the policy configuration
3. Containers are matched to policies based on their labels
4. Traffic is allowed or denied based on the policy rules

### QoS Policies

Quality of Service (QoS) policies allow for rate limiting of container traffic:

1. The `tc` (traffic control) tool is used to apply rate limits
2. Rate limits can be applied to ingress or egress traffic
3. Rate limits are specified in Kbps

## Container Network Setup

When a container is created, the following steps are performed:

1. An IP address is allocated from the node's subnet
2. A veth pair is created (one end in the host, one end in the container)
3. The host end of the veth pair is connected to the bridge
4. The container end is configured with the allocated IP
5. A default route is added in the container pointing to the bridge
6. DNS is configured in the container

## Health Monitoring

The network system includes health monitoring capabilities:

1. Node connectivity is checked periodically
2. Tunnel status is monitored
3. Metrics are collected (container count, tunnel count, policy count, etc.)
4. Node state is updated based on connectivity (Ready, Degraded, Disconnected)

## Integration with Other Components

### Service Discovery

The network system integrates with the service discovery system:

1. When a container is created, it is registered with the service discovery system
2. DNS records are created for containers
3. Services can be discovered by name rather than IP address

### Container Runtime

The network system integrates with the container runtime:

1. The container runtime provides the container ID and PID
2. The network system sets up the container's network namespace
3. The container runtime is notified when the network setup is complete

### Scheduler

The network system provides information to the scheduler:

1. Network topology information for network-aware scheduling
2. Node connectivity status for scheduling decisions
3. Network policy information for placement decisions

## Future Enhancements

1. **IPv6 Support**: Add support for IPv6 addressing
2. **Additional Overlay Types**: Implement support for WireGuard and IPsec
3. **Network Metrics**: Enhance metrics collection for better monitoring
4. **Advanced Network Policies**: Implement more sophisticated policy rules
5. **Dynamic Routing**: Implement dynamic routing protocols for better scalability