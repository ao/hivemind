# Hivemind Troubleshooting Guide

This guide provides solutions for common issues you might encounter when using the Hivemind container orchestration platform, along with diagnostic procedures and debugging techniques.

## Table of Contents

1. [Installation Issues](#installation-issues)
2. [Cluster Management Issues](#cluster-management-issues)
3. [Container Deployment Issues](#container-deployment-issues)
4. [Networking Issues](#networking-issues)
5. [Storage Issues](#storage-issues)
6. [Service Discovery Issues](#service-discovery-issues)
7. [Health Monitoring Issues](#health-monitoring-issues)
8. [Security Issues](#security-issues)
9. [Performance Issues](#performance-issues)
10. [Diagnostic Tools](#diagnostic-tools)
11. [Log Analysis](#log-analysis)

## Installation Issues

### Hivemind Fails to Install

**Symptoms:**
- Installation command fails
- Error messages during installation

**Possible Causes:**
- Missing dependencies
- Insufficient permissions
- Incompatible system

**Solutions:**

1. **Check system requirements:**
   ```bash
   # Check OS version
   cat /etc/os-release
   
   # Check kernel version
   uname -r
   ```

2. **Verify dependencies:**
   ```bash
   # Check if containerd is installed
   containerd --version
   
   # Check if SQLite is installed
   sqlite3 --version
   ```

3. **Check for permission issues:**
   ```bash
   # Try installing with sudo
   sudo cargo install hivemind
   
   # Or if using binary
   sudo mv hivemind /usr/local/bin/
   ```

4. **Check disk space:**
   ```bash
   df -h
   ```

### Hivemind Service Fails to Start

**Symptoms:**
- Service fails to start
- Error messages in logs

**Possible Causes:**
- Configuration errors
- Port conflicts
- Missing dependencies

**Solutions:**

1. **Check service status:**
   ```bash
   systemctl status hivemind
   ```

2. **Check logs for errors:**
   ```bash
   journalctl -u hivemind
   ```

3. **Verify port availability:**
   ```bash
   # Check if port 3000 is already in use
   netstat -tuln | grep 3000
   ```

4. **Verify containerd is running:**
   ```bash
   systemctl status containerd
   ```

5. **Check configuration file:**
   ```bash
   cat /etc/hivemind/config.toml
   ```

## Cluster Management Issues

### Node Fails to Join Cluster

**Symptoms:**
- Node cannot join the cluster
- Error messages when running `hivemind join`

**Possible Causes:**
- Network connectivity issues
- Firewall blocking required ports
- Incompatible versions
- Node already part of another cluster

**Solutions:**

1. **Check network connectivity:**
   ```bash
   # Ping the existing node
   ping <existing-node-ip>
   
   # Check if the port is reachable
   telnet <existing-node-ip> 3000
   ```

2. **Check firewall rules:**
   ```bash
   # For UFW
   sudo ufw status
   
   # For iptables
   sudo iptables -L
   ```

3. **Ensure required ports are open:**
   - TCP 3000 (API server)
   - TCP/UDP 7946 (Membership protocol)
   - UDP 4789 (VXLAN)

4. **Check version compatibility:**
   ```bash
   hivemind --version
   ```

5. **Reset node state and try again:**
   ```bash
   hivemind reset
   hivemind join --host <existing-node-ip>:3000
   ```

### Node Shows as Unhealthy

**Symptoms:**
- Node appears as unhealthy in `hivemind node ls`
- Services on the node may be unreachable

**Possible Causes:**
- Resource exhaustion
- Network issues
- Daemon not running properly

**Solutions:**

1. **Check node health:**
   ```bash
   hivemind node health --node <node-id>
   ```

2. **Check resource usage:**
   ```bash
   # CPU and memory usage
   top
   
   # Disk usage
   df -h
   ```

3. **Check Hivemind daemon logs:**
   ```bash
   journalctl -u hivemind
   ```

4. **Restart the Hivemind daemon:**
   ```bash
   systemctl restart hivemind
   ```

5. **Check network connectivity:**
   ```bash
   # Test connectivity to other nodes
   ping <other-node-ip>
   ```

### Cluster Split Brain

**Symptoms:**
- Different nodes show different cluster states
- Some nodes can't see others

**Possible Causes:**
- Network partition
- Membership protocol issues

**Solutions:**

1. **Check network connectivity between all nodes:**
   ```bash
   # From each node, ping all other nodes
   ping <node-ip>
   ```

2. **Check membership protocol logs:**
   ```bash
   RUST_LOG=debug journalctl -u hivemind | grep membership
   ```

3. **Force a cluster reconciliation:**
   ```bash
   hivemind cluster reconcile
   ```

4. **Restart the membership protocol on affected nodes:**
   ```bash
   hivemind membership restart
   ```

## Container Deployment Issues

### Container Fails to Deploy

**Symptoms:**
- Container deployment fails
- Error messages when running `hivemind app deploy`

**Possible Causes:**
- Image not found
- Resource constraints
- Network issues
- Configuration errors

**Solutions:**

1. **Check if the image exists:**
   ```bash
   # For Docker Hub images
   curl -s https://registry.hub.docker.com/v2/repositories/library/<image>/tags | grep name
   
   # Or try pulling the image directly
   docker pull <image>
   ```

2. **Check resource availability:**
   ```bash
   hivemind node resources
   ```

3. **Check logs for deployment errors:**
   ```bash
   hivemind app logs --name <app-name>
   ```

4. **Verify network connectivity to the registry:**
   ```bash
   ping registry-1.docker.io
   ```

5. **Try deploying with more specific options:**
   ```bash
   hivemind app deploy --image <image> --name <name> --debug
   ```

### Container Crashes or Restarts Repeatedly

**Symptoms:**
- Container keeps restarting
- Container exits shortly after starting

**Possible Causes:**
- Application errors
- Resource constraints
- Missing dependencies
- Incorrect configuration

**Solutions:**

1. **Check container logs:**
   ```bash
   hivemind app logs --name <app-name>
   ```

2. **Check container status:**
   ```bash
   hivemind app container-info --container-id <container-id>
   ```

3. **Check resource usage:**
   ```bash
   hivemind app stats --name <app-name>
   ```

4. **Try increasing resource limits:**
   ```bash
   hivemind app update --name <app-name> --cpu 1.0 --memory 512M
   ```

5. **Check for missing environment variables or configuration:**
   ```bash
   hivemind app env --name <app-name>
   ```

### Container Cannot Access External Network

**Symptoms:**
- Container cannot connect to external services
- DNS resolution fails

**Possible Causes:**
- Network configuration issues
- DNS configuration issues
- Firewall rules

**Solutions:**

1. **Check container network configuration:**
   ```bash
   hivemind app network-info --name <app-name>
   ```

2. **Check DNS resolution:**
   ```bash
   hivemind app exec --name <app-name> -- nslookup google.com
   ```

3. **Check outbound connectivity:**
   ```bash
   hivemind app exec --name <app-name> -- ping 8.8.8.8
   ```

4. **Check network policies:**
   ```bash
   hivemind security network-policy ls
   ```

5. **Check host firewall rules:**
   ```bash
   sudo iptables -L
   ```

## Networking Issues

### Containers Cannot Communicate Across Nodes

**Symptoms:**
- Containers on different nodes cannot reach each other
- Network timeouts or connection refused errors

**Possible Causes:**
- Overlay network issues
- VXLAN configuration problems
- Firewall blocking VXLAN traffic
- IP allocation conflicts

**Solutions:**

1. **Check overlay network status:**
   ```bash
   hivemind network status
   ```

2. **Verify VXLAN interfaces:**
   ```bash
   ip link show type vxlan
   ```

3. **Check VXLAN port accessibility:**
   ```bash
   # From one node to another
   nc -vz -u <other-node-ip> 4789
   ```

4. **Check IP allocation:**
   ```bash
   hivemind network ip-allocations
   ```

5. **Restart the network manager:**
   ```bash
   hivemind network restart
   ```

### Network Policy Issues

**Symptoms:**
- Containers cannot communicate despite being on the same network
- Some connections work while others don't

**Possible Causes:**
- Restrictive network policies
- Misconfigured policies
- Policy enforcement issues

**Solutions:**

1. **List all network policies:**
   ```bash
   hivemind security network-policy ls
   ```

2. **Check if traffic is being blocked by policies:**
   ```bash
   hivemind network policy-check --source <source-container> --destination <dest-container> --port <port> --protocol <protocol>
   ```

3. **Temporarily disable network policies for testing:**
   ```bash
   hivemind network policy-enforcement disable
   ```

4. **Check network policy logs:**
   ```bash
   RUST_LOG=debug journalctl -u hivemind | grep "network policy"
   ```

### DNS Resolution Issues

**Symptoms:**
- Service names cannot be resolved
- DNS queries fail

**Possible Causes:**
- Service discovery issues
- DNS server problems
- Network connectivity issues

**Solutions:**

1. **Check DNS server status:**
   ```bash
   hivemind service-discovery status
   ```

2. **Test DNS resolution:**
   ```bash
   # From inside a container
   hivemind app exec --name <app-name> -- nslookup <service-name>.local
   ```

3. **Check service registration:**
   ```bash
   hivemind service ls
   ```

4. **Restart the service discovery component:**
   ```bash
   hivemind service-discovery restart
   ```

## Storage Issues

### Volume Creation Fails

**Symptoms:**
- Volume creation fails
- Error messages when running `hivemind volume create`

**Possible Causes:**
- Insufficient disk space
- Permission issues
- Storage driver problems

**Solutions:**

1. **Check disk space:**
   ```bash
   df -h
   ```

2. **Check volume directory permissions:**
   ```bash
   ls -la /var/lib/hivemind/volumes
   ```

3. **Check storage driver status:**
   ```bash
   hivemind storage status
   ```

4. **Try creating a volume with debug output:**
   ```bash
   RUST_LOG=debug hivemind volume create --name <volume-name>
   ```

### Volume Mount Issues

**Symptoms:**
- Container cannot access mounted volume
- Permission denied errors in container logs

**Possible Causes:**
- Volume not properly mounted
- Permission issues
- Path issues

**Solutions:**

1. **Check volume mounts:**
   ```bash
   hivemind app volume-mounts --name <app-name>
   ```

2. **Check volume existence:**
   ```bash
   hivemind volume ls
   ```

3. **Check volume permissions:**
   ```bash
   ls -la /var/lib/hivemind/volumes/<volume-name>
   ```

4. **Try remounting the volume:**
   ```bash
   hivemind app update --name <app-name> --remount-volumes
   ```

### Volume Data Persistence Issues

**Symptoms:**
- Data not persisting across container restarts
- Volume appears empty after restart

**Possible Causes:**
- Volume not properly mounted
- Container writing to wrong path
- Volume path issues

**Solutions:**

1. **Verify the container is using the correct path:**
   ```bash
   hivemind app exec --name <app-name> -- ls -la <mount-path>
   ```

2. **Check if data exists in the volume:**
   ```bash
   ls -la /var/lib/hivemind/volumes/<volume-name>
   ```

3. **Check volume mount configuration:**
   ```bash
   hivemind app config --name <app-name>
   ```

4. **Try writing test data and restarting:**
   ```bash
   hivemind app exec --name <app-name> -- touch <mount-path>/test-file
   hivemind app restart --name <app-name>
   hivemind app exec --name <app-name> -- ls -la <mount-path>/test-file
   ```

## Service Discovery Issues

### Service Not Registered

**Symptoms:**
- Service does not appear in `hivemind service ls`
- Service cannot be resolved by name

**Possible Causes:**
- Service registration failed
- Service discovery component issues
- Configuration errors

**Solutions:**

1. **Check service registration:**
   ```bash
   hivemind service ls
   ```

2. **Check service discovery logs:**
   ```bash
   RUST_LOG=debug journalctl -u hivemind | grep "service discovery"
   ```

3. **Try manually registering the service:**
   ```bash
   hivemind service register --name <service-name> --domain <domain> --container <container-id>
   ```

4. **Restart the service discovery component:**
   ```bash
   hivemind service-discovery restart
   ```

### Load Balancing Issues

**Symptoms:**
- Traffic not distributed evenly
- Some containers receive no traffic

**Possible Causes:**
- Load balancing strategy issues
- Health check failures
- Service registration problems

**Solutions:**

1. **Check load balancing configuration:**
   ```bash
   hivemind service lb-config --name <service-name>
   ```

2. **Check endpoint health:**
   ```bash
   hivemind service endpoints --name <service-name>
   ```

3. **Try a different load balancing strategy:**
   ```bash
   hivemind service update --name <service-name> --lb-strategy round-robin
   ```

4. **Check service health checks:**
   ```bash
   hivemind service health-checks --name <service-name>
   ```

## Health Monitoring Issues

### Health Checks Failing

**Symptoms:**
- Containers marked as unhealthy
- Services not receiving traffic

**Possible Causes:**
- Application issues
- Misconfigured health checks
- Network issues

**Solutions:**

1. **Check health check configuration:**
   ```bash
   hivemind app health-check --name <app-name>
   ```

2. **Check health check logs:**
   ```bash
   RUST_LOG=debug journalctl -u hivemind | grep "health check"
   ```

3. **Try manually running the health check:**
   ```bash
   hivemind app exec --name <app-name> -- curl -f http://localhost:<port>/health
   ```

4. **Update health check configuration:**
   ```bash
   hivemind app update --name <app-name> --health-cmd "curl -f http://localhost:<port>/health" --health-interval 30s
   ```

### Auto-Healing Not Working

**Symptoms:**
- Failed containers not automatically restarted
- Unhealthy nodes not handled

**Possible Causes:**
- Auto-healing disabled
- Configuration issues
- Resource constraints

**Solutions:**

1. **Check auto-healing configuration:**
   ```bash
   hivemind health auto-healing-config
   ```

2. **Check auto-healing logs:**
   ```bash
   RUST_LOG=debug journalctl -u hivemind | grep "auto-healing"
   ```

3. **Enable auto-healing if disabled:**
   ```bash
   hivemind health enable-auto-healing
   ```

4. **Check resource availability for new containers:**
   ```bash
   hivemind node resources
   ```

## Security Issues

### Authentication Failures

**Symptoms:**
- Unable to log in
- Permission denied errors

**Possible Causes:**
- Incorrect credentials
- User not found
- RBAC issues

**Solutions:**

1. **Check user existence:**
   ```bash
   hivemind security user ls
   ```

2. **Reset user password:**
   ```bash
   hivemind security user reset-password --username <username>
   ```

3. **Check authentication logs:**
   ```bash
   hivemind security audit-logs --action authenticate
   ```

4. **Verify RBAC configuration:**
   ```bash
   hivemind security role ls
   hivemind security user roles --username <username>
   ```

### Container Scanning Issues

**Symptoms:**
- Container scanning fails
- Vulnerability reports not generated

**Possible Causes:**
- Scanner configuration issues
- Network connectivity problems
- Image access issues

**Solutions:**

1. **Check scanner configuration:**
   ```bash
   hivemind security scanner-config
   ```

2. **Try scanning with debug output:**
   ```bash
   RUST_LOG=debug hivemind security scan-image --image <image>
   ```

3. **Check network connectivity to vulnerability databases:**
   ```bash
   ping cve.mitre.org
   ```

4. **Update vulnerability database:**
   ```bash
   hivemind security update-vuln-db
   ```

### Secret Management Issues

**Symptoms:**
- Secrets not accessible in containers
- Permission denied errors

**Possible Causes:**
- Secret mounting issues
- Permission problems
- Configuration errors

**Solutions:**

1. **Check secret existence:**
   ```bash
   hivemind security list-secrets
   ```

2. **Verify secret mounting:**
   ```bash
   hivemind app secret-mounts --name <app-name>
   ```

3. **Check secret access permissions:**
   ```bash
   hivemind security secret-access --name <secret-name>
   ```

4. **Try remounting the secret:**
   ```bash
   hivemind app update --name <app-name> --remount-secrets
   ```

## Performance Issues

### High CPU Usage

**Symptoms:**
- System CPU usage is high
- Containers or nodes responding slowly

**Possible Causes:**
- Resource-intensive containers
- Too many containers on a node
- System processes consuming resources

**Solutions:**

1. **Identify high CPU consumers:**
   ```bash
   hivemind node top --node <node-id>
   ```

2. **Check container CPU limits:**
   ```bash
   hivemind app resource-limits --name <app-name>
   ```

3. **Consider scaling horizontally:**
   ```bash
   hivemind app scale --name <app-name> --replicas <count>
   ```

4. **Adjust container CPU limits:**
   ```bash
   hivemind app update --name <app-name> --cpu 0.5 --cpu-limit 1.0
   ```

### Memory Leaks

**Symptoms:**
- Increasing memory usage over time
- Out of memory errors

**Possible Causes:**
- Container memory leaks
- Insufficient memory limits
- System memory leaks

**Solutions:**

1. **Monitor memory usage over time:**
   ```bash
   hivemind app stats --name <app-name> --watch
   ```

2. **Check container memory limits:**
   ```bash
   hivemind app resource-limits --name <app-name>
   ```

3. **Increase memory limits if needed:**
   ```bash
   hivemind app update --name <app-name> --memory 512M --memory-limit 1G
   ```

4. **Restart leaking containers:**
   ```bash
   hivemind app restart --name <app-name>
   ```

### Slow Network Performance

**Symptoms:**
- High latency between containers
- Slow data transfer rates

**Possible Causes:**
- Network congestion
- Overlay network issues
- Hardware limitations

**Solutions:**

1. **Check network performance:**
   ```bash
   hivemind network benchmark
   ```

2. **Check overlay network status:**
   ```bash
   hivemind network status
   ```

3. **Optimize network configuration:**
   ```bash
   hivemind network optimize
   ```

4. **Consider container placement for network locality:**
   ```bash
   hivemind app deploy --name <app-name> --image <image> --node-affinity network-zone=zone1
   ```

## Diagnostic Tools

### System Diagnostics

To run a comprehensive system diagnostic:

```bash
hivemind admin diagnostics
```

This checks all components and reports any issues.

### Component-Specific Diagnostics

For more targeted diagnostics:

```bash
# Network diagnostics
hivemind network diagnostics

# Container diagnostics
hivemind app diagnostics --name <app-name>

# Node diagnostics
hivemind node diagnostics --node <node-id>

# Storage diagnostics
hivemind volume diagnostics

# Service discovery diagnostics
hivemind service-discovery diagnostics
```

### Collecting Debug Information

To collect comprehensive debug information for support:

```bash
hivemind admin collect-debug-info --output debug-info.tar.gz
```

This collects logs, configuration, and system information.

## Log Analysis

### Viewing Logs

```bash
# View Hivemind daemon logs
journalctl -u hivemind

# View container logs
hivemind app logs --name <app-name>

# View system logs with filtering
journalctl -u hivemind | grep "error"
```

### Setting Log Levels

To increase log verbosity for troubleshooting:

```bash
# Set log level for the daemon
RUST_LOG=debug hivemind daemon

# Set log level for a specific component
RUST_LOG=network=trace,membership=debug hivemind daemon
```

Available log levels (from least to most verbose):
- error
- warn
- info
- debug
- trace

### Common Log Patterns

Here are some common log patterns and what they indicate:

1. **Connection refused errors**:
   ```
   Error: Connection refused (os error 111)
   ```
   Indicates network connectivity issues.

2. **Permission denied errors**:
   ```
   Error: Permission denied (os error 13)
   ```
   Indicates file permission or authorization issues.

3. **Resource exhaustion errors**:
   ```
   Error: Cannot allocate memory (os error 12)
   ```
   Indicates the system is out of memory.

4. **Timeout errors**:
   ```
   Error: Timeout expired
   ```
   Indicates network latency or unresponsive services.

## Getting Help

If you've tried the troubleshooting steps in this guide and are still experiencing issues:

1. Check the [GitHub Issues](https://github.com/ao/hivemind/issues) page to see if others have reported similar problems
2. Join the [Community Forum](https://community.hivemind.io) for community support
3. Contact the maintainers directly for critical issues

When reporting issues, always include:
- Hivemind version (`hivemind --version`)
- OS and kernel version
- Relevant logs
- Steps to reproduce the issue
- Any error messages or symptoms