# Container Networking in Hivemind

This document describes the container networking implementation in Hivemind, which enables containers running on different nodes to communicate with each other seamlessly.

## Overview

Hivemind's container networking provides a robust, secure, and scalable way for containers to communicate across nodes in a cluster. The networking layer is designed to be transparent to applications, allowing containers to communicate as if they were on the same host, regardless of their physical location.

### Key Components

1. **IP Address Management (IPAM)**: Allocates and manages IP addresses for containers and nodes
2. **Overlay Network**: Creates a virtual network that spans across all nodes in the cluster
3. **Network Policies**: Controls traffic flow between containers for security
4. **Service Discovery Integration**: Works with DNS for service name resolution

### Architecture Diagram

```
┌─────────────────────┐     ┌─────────────────────┐
│      Node 1         │     │      Node 2         │
│  ┌───────────────┐  │     │  ┌───────────────┐  │
│  │  Container A  │  │     │  │  Container C  │  │
│  │  10.244.1.2   │  │     │  │  10.244.2.2   │  │
│  └───────┬───────┘  │     │  └───────┬───────┘  │
│          │          │     │          │          │
│  ┌───────┴───────┐  │     │  ┌───────┴───────┐  │
│  │   Bridge      │◄─┼─────┼─►│   Bridge      │  │
│  │  (hivemind0)  │  │     │  │  (hivemind0)  │  │
│  └───────┬───────┘  │     │  └───────┬───────┘  │
│          │          │     │          │          │
│  ┌───────┴───────┐  │     │  ┌───────┴───────┐  │
│  │    VXLAN      │◄─┼─────┼─►│    VXLAN      │  │
│  │   Interface   │  │     │  │   Interface   │  │
│  └───────────────┘  │     │  └───────────────┘  │
└─────────┬───────────┘     └─────────┬───────────┘
          │                           │
          └───────────────────────────┘
                  Overlay Network
```

## Network Architecture

### CIDR Allocation and Subnetting

Hivemind uses a cluster-wide CIDR block (default: `10.244.0.0/16`) and allocates a subnet to each node (default: `/24`). This allows for:

- Up to 256 nodes in the default configuration
- Up to 254 containers per node
- Automatic IP address assignment for containers

### Overlay Network with VXLAN

The overlay network uses VXLAN (Virtual Extensible LAN) to create a layer 2 network that spans across all nodes:

- VXLAN ID: 42 (default)
- VXLAN Port: 4789 (default)
- Encapsulates Ethernet frames in UDP packets
- Allows containers to communicate across nodes using their allocated IPs

### Container Network Namespaces

Each container gets its own network namespace with:

- A virtual ethernet (veth) pair connecting to the host bridge
- An assigned IP address from the node's subnet
- A default route pointing to the node's bridge
- A unique MAC address derived from the container ID

### Bridge and veth Pairs

On each node, a Linux bridge (`hivemind0`) connects all local containers:

- The bridge has an IP address (first usable IP in the node's subnet)
- Each container connects to the bridge via a veth pair
- The bridge connects to the VXLAN interface for cross-node communication

## Configuration

### Default Network Settings

| Parameter | Default Value | Description |
|-----------|---------------|-------------|
| Network CIDR | 10.244.0.0/16 | The overall network address space |
| Node Subnet Size | /24 | Subnet allocated to each node |
| Overlay Type | VXLAN | The overlay network technology |
| VXLAN ID | 42 | VXLAN network identifier |
| VXLAN Port | 4789 | UDP port for VXLAN traffic |

### Customizing Network Parameters

Network parameters can be customized when initializing the NetworkManager:

```rust
let custom_config = NetworkConfig {
    network_cidr: "10.10.0.0/16".to_string(),
    node_subnet_size: 24,
    overlay_type: OverlayNetworkType::VXLAN,
    vxlan_id: 100,
    vxlan_port: 4789,
};

let network_manager = NetworkManager::new(
    node_manager.clone(),
    service_discovery.clone(),
    Some(custom_config),
).await?;
```

## Container Communication

### Container-to-Container Communication

Containers can communicate with each other using their assigned IP addresses:

- **Same Node**: Direct communication through the bridge
- **Different Nodes**: Communication through the overlay network

### Service Discovery Integration

Hivemind integrates network information with the service discovery system:

- Services can be discovered by name rather than IP address
- The service discovery system maintains a mapping of service names to container IPs
- DNS resolution allows containers to find services by name

### Cross-Node Communication Flow

When Container A on Node 1 communicates with Container B on Node 2:

1. Container A sends a packet to Container B's IP
2. The packet reaches Node 1's bridge
3. The bridge forwards the packet to the VXLAN interface
4. The VXLAN interface encapsulates the packet and sends it to Node 2
5. Node 2's VXLAN interface decapsulates the packet
6. The packet is forwarded to Node 2's bridge
7. The bridge delivers the packet to Container B

## Network Policies

### Policy Structure

Network policies in Hivemind control traffic flow between containers:

```rust
NetworkPolicy {
    name: "web-to-db",
    selector: NetworkSelector { labels: {"app": "web"} },
    ingress_rules: [
        NetworkRule {
            ports: [PortRange { protocol: TCP, port_min: 80, port_max: 80 }],
            from: [NetworkPeer { ip_block: Some("10.244.2.0/24"), selector: None }]
        }
    ],
    egress_rules: [
        NetworkRule {
            ports: [PortRange { protocol: TCP, port_min: 5432, port_max: 5432 }],
            from: [NetworkPeer { ip_block: None, selector: Some({"app": "db"}) }]
        }
    ]
}
```

### Applying Network Policies

Network policies can be applied using the NetworkManager:

```rust
let policy = NetworkPolicy {
    name: "web-to-db".to_string(),
    selector: NetworkSelector { 
        labels: [("app", "web")].into_iter().collect() 
    },
    ingress_rules: vec![/* rules here */],
    egress_rules: vec![/* rules here */],
};

network_manager.apply_network_policy(policy).await?;
```

## Troubleshooting

### Common Issues

1. **Container can't reach the internet**
   - Check the default route in the container
   - Verify that IP forwarding is enabled on the host
   - Check that NAT is properly configured

2. **Containers can't communicate across nodes**
   - Verify that VXLAN interfaces are up on both nodes
   - Check that the nodes can reach each other on the VXLAN port
   - Verify that the FDB entries are correctly set up

3. **DNS resolution not working**
   - Check that the service discovery system is running
   - Verify that the container's DNS configuration points to the service discovery

### Diagnostic Commands

#### Check Network Health

```bash
hivemind health
```

This will show the status of the network, including:
- Nodes in the network
- Overlay tunnels
- Network policies

#### Check Container Network Configuration

```bash
# Get container's network namespace
PID=$(docker inspect --format '{{.State.Pid}}' <container_id>)

# Check IP configuration
nsenter -t $PID -n ip addr

# Check routes
nsenter -t $PID -n ip route

# Check DNS configuration
nsenter -t $PID -n cat /etc/resolv.conf
```

## Integration with Existing Features

### Node Membership Integration

The network layer integrates with the node membership system:

- When a node joins the cluster, it gets a subnet allocation
- Overlay tunnels are automatically set up to new nodes
- When a node leaves, its resources are cleaned up

### Container Lifecycle and Networking

The network is set up during container creation and torn down during container removal:

1. **Container Creation**:
   - Allocate IP address
   - Create network namespace
   - Set up veth pair
   - Configure routes

2. **Container Removal**:
   - Release IP address
   - Remove veth interfaces
   - Clean up routes

## Future Enhancements

1. **IPv6 Support**: Add support for IPv6 addressing
2. **Additional Overlay Types**: Support for other overlay technologies like Geneve
3. **Network Metrics**: Collect and expose network performance metrics
4. **Advanced Network Policies**: More sophisticated policy rules and enforcement
5. **Load Balancing**: Integrated load balancing for services
