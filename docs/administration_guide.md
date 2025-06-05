# Hivemind Administration Guide

This guide provides comprehensive instructions for administering and managing a Hivemind container orchestration platform deployment, including cluster management, security administration, monitoring, and maintenance tasks.

## Table of Contents

1. [Cluster Management](#cluster-management)
2. [Node Administration](#node-administration)
3. [Security Administration](#security-administration)
4. [Resource Management](#resource-management)
5. [Monitoring and Logging](#monitoring-and-logging)
6. [Backup and Recovery](#backup-and-recovery)
7. [Upgrades and Maintenance](#upgrades-and-maintenance)
8. [Performance Tuning](#performance-tuning)
9. [Troubleshooting](#troubleshooting)

## Cluster Management

### Cluster Initialization

To initialize a new Hivemind cluster:

```bash
hivemind daemon --web-port 3000 --data-dir /var/lib/hivemind
```

This command starts the Hivemind daemon and initializes a new cluster with the current node as the first member.

### Adding Nodes to the Cluster

To add a new node to an existing cluster:

1. Install Hivemind on the new node
2. Join the cluster by specifying an existing node's address:

```bash
hivemind join --host <existing-node-ip>:3000
```

### Listing Cluster Nodes

To list all nodes in the cluster:

```bash
hivemind node ls
```

### Viewing Cluster Status

To view the overall status of the cluster:

```bash
hivemind cluster status
```

This shows information about all nodes, their roles, and health status.

### Cluster Configuration

The cluster configuration is stored in `/etc/hivemind/config.toml` by default. You can modify this file to change cluster-wide settings:

```toml
[cluster]
name = "production-cluster"
join_timeout = 60
gossip_interval = 1
failure_threshold = 3
```

After modifying the configuration, restart the Hivemind daemon:

```bash
sudo systemctl restart hivemind
```

## Node Administration

### Node Information

To get detailed information about a specific node:

```bash
hivemind node info --node <node-id>
```

### Node Maintenance Mode

To put a node into maintenance mode (prevents new containers from being scheduled):

```bash
hivemind node maintenance --node <node-id> --enable
```

To take a node out of maintenance mode:

```bash
hivemind node maintenance --node <node-id> --disable
```

### Draining a Node

To drain a node (move all containers to other nodes):

```bash
hivemind node drain --node <node-id>
```

### Removing a Node

To remove a node from the cluster:

```bash
hivemind node remove --node <node-id>
```

Before removing a node, ensure it's drained of all containers.

### Node Labels

You can add labels to nodes for scheduling purposes:

```bash
hivemind node label --node <node-id> --label role=frontend
```

To remove a label:

```bash
hivemind node label --node <node-id> --label role-
```

### Node Resources

To view resource usage on a node:

```bash
hivemind node resources --node <node-id>
```

## Security Administration

### User Management

#### Creating Users

To create a new user:

```bash
hivemind security user create --username john --full-name "John Doe" --email john@example.com
```

This will prompt for a password or you can provide it with `--password`.

#### Listing Users

To list all users:

```bash
hivemind security user ls
```

#### Modifying Users

To modify a user:

```bash
hivemind security user update --username john --full-name "John Smith"
```

#### Deleting Users

To delete a user:

```bash
hivemind security user delete --username john
```

### Role-Based Access Control (RBAC)

#### Creating Roles

To create a new role:

```bash
hivemind security role create --name developer --description "Developer role"
```

#### Adding Permissions to Roles

To add permissions to a role:

```bash
hivemind security role add-permission --name developer --resource container --action create,read,update,delete
```

#### Assigning Roles to Users

To assign a role to a user:

```bash
hivemind security role assign --name developer --username john
```

#### Listing Roles

To list all roles:

```bash
hivemind security role ls
```

### Network Policies

#### Creating Network Policies

To create a network policy:

```bash
hivemind security network-policy create --name web-to-db \
  --selector app=web \
  --allow-egress tcp:5432 --to-selector app=db \
  --allow-ingress tcp:80 --from-cidr 10.0.0.0/8
```

#### Listing Network Policies

To list all network policies:

```bash
hivemind security network-policy ls
```

#### Deleting Network Policies

To delete a network policy:

```bash
hivemind security network-policy delete --name web-to-db
```

### Secret Management

#### Creating Secrets

To create a new secret:

```bash
hivemind security create-secret --name db-password --value "my-secure-password"
```

Or from a file:

```bash
hivemind security create-secret --name tls-cert --file /path/to/cert.pem
```

#### Listing Secrets

To list all secrets (metadata only, not values):

```bash
hivemind security list-secrets
```

#### Updating Secrets

To update a secret:

```bash
hivemind security update-secret --name db-password --value "new-secure-password"
```

#### Deleting Secrets

To delete a secret:

```bash
hivemind security delete-secret --name db-password
```

### Audit Logging

To view security audit logs:

```bash
hivemind security audit-logs
```

You can filter by user, action, or time range:

```bash
hivemind security audit-logs --user john --action create --since 24h
```

## Resource Management

### Resource Quotas

#### Setting Cluster-wide Resource Limits

To set cluster-wide resource limits:

```bash
hivemind admin set-quota --cpu 100 --memory 200G --storage 1T
```

#### Setting Namespace Resource Limits

To set resource limits for a namespace:

```bash
hivemind admin set-quota --namespace production --cpu 50 --memory 100G --storage 500G
```

### Resource Allocation

To view resource allocation across the cluster:

```bash
hivemind admin resources
```

This shows CPU, memory, and storage allocation across all nodes.

### Container Resource Management

To set resource limits for a container:

```bash
hivemind app deploy --image nginx:latest --name web \
  --cpu 0.5 --cpu-limit 1.0 \
  --memory 256M --memory-limit 512M
```

To update resource limits for an existing container:

```bash
hivemind app update --name web \
  --cpu 1.0 --cpu-limit 2.0 \
  --memory 512M --memory-limit 1G
```

## Monitoring and Logging

### System Health Monitoring

To check the overall health of the system:

```bash
hivemind health
```

### Container Monitoring

To monitor a specific container:

```bash
hivemind app monitor --name <app-name>
```

This shows real-time resource usage and health status.

### Node Monitoring

To monitor a specific node:

```bash
hivemind node monitor --node <node-id>
```

This shows real-time resource usage and health status.

### Viewing Logs

#### Container Logs

To view logs from a container:

```bash
hivemind app logs --name <app-name>
```

To follow logs in real-time:

```bash
hivemind app logs --name <app-name> --follow
```

#### System Logs

To view Hivemind system logs:

```bash
hivemind admin logs
```

Or using journalctl (if using systemd):

```bash
journalctl -u hivemind
```

### Alerts and Notifications

#### Configuring Alert Channels

To configure email alerts:

```bash
hivemind admin alerts configure-email \
  --smtp-server smtp.example.com \
  --smtp-port 587 \
  --username alerter \
  --password "password" \
  --from alerts@example.com \
  --to admin@example.com
```

To configure Slack alerts:

```bash
hivemind admin alerts configure-slack \
  --webhook-url https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX \
  --channel #alerts
```

#### Viewing Active Alerts

To view active alerts:

```bash
hivemind admin alerts
```

#### Acknowledging Alerts

To acknowledge an alert:

```bash
hivemind admin alerts acknowledge --id <alert-id>
```

## Backup and Recovery

### Backing Up Hivemind Data

To create a backup of Hivemind data:

```bash
hivemind admin backup --output /path/to/backup/directory
```

This creates a backup of the Hivemind database, configuration, and other essential data.

### Restoring from Backup

To restore from a backup:

```bash
hivemind admin restore --input /path/to/backup/directory
```

### Backing Up Application Data

To back up application data stored in volumes:

```bash
hivemind volume backup --name <volume-name> --output /path/to/backup/file.tar.gz
```

### Restoring Application Data

To restore application data:

```bash
hivemind volume restore --name <volume-name> --input /path/to/backup/file.tar.gz
```

## Upgrades and Maintenance

### Upgrading Hivemind

To upgrade Hivemind to the latest version:

1. Download the new version:
   ```bash
   curl -LO https://github.com/ao/hivemind/releases/latest/download/hivemind-linux-amd64.tar.gz
   ```

2. Extract the archive:
   ```bash
   tar -xzf hivemind-linux-amd64.tar.gz
   ```

3. Stop the Hivemind service:
   ```bash
   sudo systemctl stop hivemind
   ```

4. Replace the binary:
   ```bash
   sudo mv hivemind /usr/local/bin/
   ```

5. Start the Hivemind service:
   ```bash
   sudo systemctl start hivemind
   ```

6. Verify the upgrade:
   ```bash
   hivemind --version
   ```

### Rolling Upgrades

For a multi-node cluster, perform a rolling upgrade to minimize downtime:

1. Put the node into maintenance mode:
   ```bash
   hivemind node maintenance --node <node-id> --enable
   ```

2. Drain the node:
   ```bash
   hivemind node drain --node <node-id>
   ```

3. Upgrade Hivemind on the node (as described above)

4. Take the node out of maintenance mode:
   ```bash
   hivemind node maintenance --node <node-id> --disable
   ```

5. Repeat for each node in the cluster

### Scheduled Maintenance

To schedule maintenance for a node:

```bash
hivemind node schedule-maintenance --node <node-id> --start "2025-06-10T22:00:00Z" --duration 2h
```

This will automatically put the node into maintenance mode at the specified time and take it out of maintenance mode after the specified duration.

## Performance Tuning

### Container Scheduler Tuning

To adjust the container scheduler parameters:

```bash
hivemind admin config set scheduler.rebalance_threshold 20
hivemind admin config set scheduler.scheduling_interval 30
```

### Network Tuning

To adjust network parameters:

```bash
hivemind admin config set network.vxlan_mtu 1450
hivemind admin config set network.max_transmit_unit 1500
```

### Memory Management

To adjust memory management parameters:

```bash
hivemind admin config set memory.overcommit_ratio 1.2
```

### Disk I/O Tuning

To adjust disk I/O parameters:

```bash
hivemind admin config set storage.io_weight 100
```

## Troubleshooting

### Common Issues

#### Node Not Joining Cluster

If a node fails to join the cluster:

1. Check network connectivity:
   ```bash
   ping <existing-node-ip>
   ```

2. Verify the Hivemind daemon is running on both nodes:
   ```bash
   systemctl status hivemind
   ```

3. Check firewall rules:
   ```bash
   sudo ufw status
   ```

4. Check logs for errors:
   ```bash
   journalctl -u hivemind
   ```

#### Container Fails to Start

If a container fails to start:

1. Check container logs:
   ```bash
   hivemind app logs --name <app-name>
   ```

2. Check system logs:
   ```bash
   journalctl -u hivemind
   ```

3. Verify the image exists:
   ```bash
   hivemind app image-exists --image <image-name>
   ```

4. Check resource constraints:
   ```bash
   hivemind node resources
   ```

#### Network Connectivity Issues

If containers can't communicate:

1. Check network status:
   ```bash
   hivemind network status
   ```

2. Verify network policies:
   ```bash
   hivemind security network-policy ls
   ```

3. Check overlay network:
   ```bash
   hivemind network overlay-status
   ```

### Diagnostic Tools

#### System Diagnostics

To run a full system diagnostic:

```bash
hivemind admin diagnostics
```

This checks all components and reports any issues.

#### Network Diagnostics

To diagnose network issues:

```bash
hivemind network diagnostics
```

#### Container Diagnostics

To diagnose container issues:

```bash
hivemind app diagnostics --name <app-name>
```

### Getting Support

If you encounter issues that you can't resolve:

1. Check the [Troubleshooting Guide](troubleshooting_guide.md)
2. Visit the [GitHub Issues](https://github.com/ao/hivemind/issues) page
3. Join the [Community Forum](https://community.hivemind.io)

## Conclusion

This administration guide covers the essential tasks for managing a Hivemind cluster. For more detailed information on specific topics, refer to the following resources:

- [Installation Guide](installation_guide.md)
- [User Guide](user_guide.md)
- [Troubleshooting Guide](troubleshooting_guide.md)
- [API Reference](api_reference.md)
- [CLI Reference](cli_reference.md)