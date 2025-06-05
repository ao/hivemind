# Node Membership Protocol Implementation

This document describes the implementation details of the Node Membership Protocol in Hivemind.

## Overview

The Node Membership Protocol is based on the SWIM protocol (Scalable Weakly-consistent Infection-style Process Group Membership Protocol) and provides the following features:

1. **Node Discovery**: Automatic discovery of nodes in the cluster
2. **Failure Detection**: Reliable detection of node failures
3. **Leader Election**: Deterministic leader election for coordinating cluster activities
4. **Distributed State Management**: Consistent state sharing across the cluster
5. **Quorum Detection**: Detection of network partitions and quorum loss

## Implementation Details

### Core Components

#### MembershipProtocol

The `MembershipProtocol` struct in `src/membership.rs` is the main implementation of the SWIM protocol. It manages:

- Membership list with node states (Alive, Suspected, Dead, Left, Maintenance)
- Failure detection through direct and indirect pinging
- Gossip-based dissemination of membership information
- Leader election and state synchronization

#### NodeManager Integration

The `NodeManager` in `src/node.rs` integrates with the membership protocol to:

- Initialize the protocol with appropriate configuration
- Handle membership events (joins, leaves, failures)
- Provide a consistent view of the cluster to other components
- Manage distributed state through the membership protocol

### Protocol Operation

#### Node States

Nodes in the cluster can be in one of the following states:

- **Alive**: The node is functioning normally
- **Suspected**: The node has failed to respond to pings but is not yet considered dead
- **Dead**: The node is considered to have failed
- **Left**: The node has gracefully left the cluster
- **Maintenance**: The node is temporarily unavailable for planned maintenance

#### Failure Detection

1. **Direct Pinging**:
   - Each node periodically selects a random member and pings it
   - If the ping succeeds, the member is marked as alive
   - If the ping fails, indirect pinging is attempted

2. **Indirect Pinging**:
   - When a direct ping fails, the node asks k other nodes to ping the target
   - If any of these indirect pings succeed, the target is considered alive
   - If all indirect pings fail, the target is marked as suspected

3. **Suspicion Mechanism**:
   - A suspected node is given a grace period before being marked as dead
   - This reduces false positives due to temporary network issues
   - The suspicion period is configurable

#### Leader Election

The leader election mechanism is deterministic and based on the following rules:

1. If no leader exists, the node with the lowest incarnation number becomes the leader
2. If multiple nodes have the same incarnation number, the node with the lexicographically smallest ID becomes the leader
3. Leader announcements are propagated through the cluster via gossip
4. All nodes maintain a consistent view of who the leader is

#### Distributed State Management

The distributed state management provides:

1. **Leader-Based Writes**: Only the leader can write state to ensure consistency
2. **State Versioning**: Each state update has a version number to resolve conflicts
3. **State Replication**: State is replicated to all nodes through gossip
4. **Conflict Resolution**: Conflicts are resolved based on version numbers

#### Quorum Detection

The protocol detects network partitions and quorum loss:

1. Each node tracks the number of alive nodes in the cluster
2. If the number of alive nodes falls below the quorum threshold (>50%), a quorum loss event is generated
3. When the number of alive nodes exceeds the quorum threshold again, a quorum restored event is generated

### Message Types

The protocol uses the following message types:

- **Ping**: Direct ping to check if a node is alive
- **PingReq**: Request to another node to ping a target (indirect ping)
- **Ack**: Acknowledgment of a ping
- **Sync**: Synchronization of membership information
- **Join**: Request to join the cluster
- **Leave**: Notification of leaving the cluster
- **LeaderElection**: Initiate leader election
- **LeaderAnnounce**: Announce a new leader
- **Maintenance**: Enter maintenance mode
- **Reintegrate**: Request to reintegrate after failure
- **StateSync**: Synchronize distributed state

## Integration with Other Components

### Network Manager

The membership protocol integrates with the network manager to:

- Update network configuration when nodes join or leave
- Establish secure connections between nodes
- Handle network partitions

### Storage Manager

The membership protocol integrates with the storage manager to:

- Persist node information
- Recover membership state after restarts
- Store distributed state

### Health Monitor

The membership protocol integrates with the health monitor to:

- Provide node health information
- Detect and handle node failures
- Coordinate health checks across the cluster

## Configuration

The membership protocol can be configured with the following parameters:

- `bind_addr`: The address to bind to for protocol communication
- `bind_port`: The port to use for protocol communication
- `protocol_period`: The interval between protocol periods
- `ping_timeout`: The timeout for direct pings
- `suspicion_mult`: The multiplier for the suspicion period
- `indirect_checks`: The number of nodes to use for indirect pinging
- `gossip_factor`: The number of nodes to gossip to per round
- `max_gossip_updates`: Maximum number of updates to include in gossip
- `reintegration_time`: Time before allowing a failed node to rejoin
- `quorum_percentage`: Percentage of nodes needed for quorum
- `leader_check_interval`: How often to check leader status

## Testing

The membership protocol is tested with:

1. **Unit Tests**: Test individual components in isolation
2. **Integration Tests**: Test the interaction between components
3. **Chaos Tests**: Test the protocol under network failures and partitions

## Future Improvements

Potential future improvements to the membership protocol include:

1. **Adaptive Timeouts**: Adjust timeouts based on network conditions
2. **Prioritized Suspicion**: Prioritize checking suspected nodes
3. **Bandwidth-Aware Dissemination**: Limit gossip based on available bandwidth
4. **Security Enhancements**: Add authentication and encryption to protocol messages
5. **Dynamic Quorum**: Adjust quorum threshold based on cluster size
6. **Hierarchical Clustering**: Support for large-scale deployments with hierarchical structure