# Hivemind Health Monitoring System

This document provides a comprehensive overview of the Health Monitoring system in Hivemind, explaining how container and node health is monitored, how auto-healing works, and how to configure and use the health monitoring features.

## Overview

The Health Monitoring system in Hivemind continuously monitors the health of containers and nodes in the cluster. It detects issues, generates alerts, and can automatically take corrective actions to maintain system reliability and availability.

## Key Features

- **Container Health Checking**: Monitors container health with customizable checks
- **Node Health Monitoring**: Tracks node health and resource usage
- **Auto-Healing**: Automatically restarts unhealthy containers and handles node failures
- **Alerting**: Generates alerts for health issues
- **Metrics Collection**: Collects and stores health metrics over time
- **Customizable Policies**: Configure health check parameters and auto-healing policies

## Health Monitoring Components

### Container Health Monitor

The Container Health Monitor checks the health of containers using various methods:

1. **Status Checks**: Verifies that containers are in the running state
2. **Process Checks**: Ensures the main process inside the container is running
3. **HTTP/TCP Checks**: Performs HTTP or TCP checks against container endpoints
4. **Custom Commands**: Executes custom health check commands inside containers
5. **Resource Usage**: Monitors CPU, memory, and other resource usage

### Node Health Monitor

The Node Health Monitor checks the health of nodes in the cluster:

1. **Resource Monitoring**: Tracks CPU, memory, disk, and network usage
2. **Connectivity Checks**: Ensures nodes are reachable and can communicate
3. **Service Checks**: Verifies that required services are running
4. **Load Monitoring**: Tracks system load and performance metrics

### Auto-Healing System

The Auto-Healing system takes corrective actions when issues are detected:

1. **Container Restart**: Automatically restarts unhealthy containers
2. **Container Rescheduling**: Moves containers from unhealthy nodes to healthy ones
3. **Node Recovery**: Attempts to recover unhealthy nodes
4. **Service Failover**: Redirects traffic away from unhealthy services

### Alerting System

The Alerting system generates and manages alerts:

1. **Alert Generation**: Creates alerts based on health check results
2. **Alert Severity**: Categorizes alerts by severity (info, warning, error, critical)
3. **Alert Notification**: Sends notifications through configured channels
4. **Alert Management**: Tracks alert status and resolution

## Using Health Monitoring

### Checking System Health

To check the overall health of the system:

```bash
hivemind health
```

This command shows a summary of the health status of all components.

### Checking Container Health

To check the health of a specific container:

```bash
hivemind app health --name <app-name>
```

This shows detailed health information for the container, including:
- Current health status
- Health check history
- Resource usage
- Recent events

### Checking Node Health

To check the health of a specific node:

```bash
hivemind node health --node <node-id>
```

This shows detailed health information for the node, including:
- Current health status
- Resource usage
- Running containers
- Network status

### Viewing Health Metrics

To view health metrics over time:

```bash
hivemind health metrics --entity <container-id|node-id> --metric <metric-type> --duration 24h
```

Available metric types:
- `cpu`: CPU usage
- `memory`: Memory usage
- `disk`: Disk usage
- `network`: Network usage
- `health`: Health check results

### Viewing Alerts

To view active alerts:

```bash
hivemind health alerts
```

To view alert history:

```bash
hivemind health alerts --history --duration 7d
```

### Acknowledging Alerts

To acknowledge an alert:

```bash
hivemind health alert-ack --id <alert-id>
```

## Configuring Health Monitoring

### Container Health Checks

You can configure health checks when deploying a container:

```bash
hivemind app deploy --image nginx:latest --name web \
  --health-cmd "curl -f http://localhost/" \
  --health-interval 30s \
  --health-timeout 5s \
  --health-retries 3 \
  --health-start-period 60s
```

Parameters:
- `--health-cmd`: The command to run to check health
- `--health-interval`: How often to run the health check
- `--health-timeout`: How long to wait before considering the check failed
- `--health-retries`: How many consecutive failures before marking unhealthy
- `--health-start-period`: How long to wait before starting health checks

You can also update health checks for an existing container:

```bash
hivemind app update --name web \
  --health-cmd "curl -f http://localhost/health" \
  --health-interval 15s
```

### Auto-Healing Configuration

To configure auto-healing policies:

```bash
hivemind health config auto-healing \
  --container-restart true \
  --max-restart-attempts 5 \
  --restart-delay 10s \
  --node-recovery true
```

Parameters:
- `--container-restart`: Enable/disable automatic container restart
- `--max-restart-attempts`: Maximum number of restart attempts
- `--restart-delay`: Delay between restart attempts
- `--node-recovery`: Enable/disable automatic node recovery

### Alert Configuration

To configure alert thresholds:

```bash
hivemind health config alerts \
  --cpu-threshold 80 \
  --memory-threshold 85 \
  --disk-threshold 90 \
  --health-failure-threshold 3
```

Parameters:
- `--cpu-threshold`: CPU usage threshold (percentage)
- `--memory-threshold`: Memory usage threshold (percentage)
- `--disk-threshold`: Disk usage threshold (percentage)
- `--health-failure-threshold`: Number of health check failures before alerting

### Notification Channels

To configure notification channels for alerts:

```bash
# Configure email notifications
hivemind health config notifications email \
  --smtp-server smtp.example.com \
  --smtp-port 587 \
  --username alerter \
  --password "password" \
  --from alerts@example.com \
  --to admin@example.com

# Configure Slack notifications
hivemind health config notifications slack \
  --webhook-url https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX \
  --channel #alerts
```

## Implementation Details

### Health Check Types

#### HTTP Health Checks

HTTP health checks make an HTTP request to a specified endpoint and check the response:

```bash
hivemind app deploy --image myapp:latest --name api \
  --health-cmd "curl -f http://localhost:8080/health" \
  --health-interval 30s
```

The check succeeds if the HTTP response has a status code between 200 and 399.

#### TCP Health Checks

TCP health checks attempt to establish a TCP connection to a specified port:

```bash
hivemind app deploy --image postgres:13 --name db \
  --health-cmd "nc -z localhost 5432" \
  --health-interval 30s
```

The check succeeds if the connection can be established.

#### Command Health Checks

Command health checks execute a command inside the container:

```bash
hivemind app deploy --image redis:latest --name cache \
  --health-cmd "redis-cli ping" \
  --health-interval 30s
```

The check succeeds if the command exits with a status code of 0.

### Health Status Lifecycle

Containers and nodes go through the following health status lifecycle:

1. **Starting**: Initial state during startup
2. **Healthy**: All health checks are passing
3. **Degraded**: Some health checks are failing, but the entity is still functional
4. **Unhealthy**: Critical health checks are failing
5. **Failed**: The entity has failed and requires intervention

### Auto-Healing Process

When a container is detected as unhealthy:

1. The health monitor marks the container as unhealthy
2. If auto-healing is enabled, the container is added to the restart queue
3. The container is restarted after the configured delay
4. If the container continues to be unhealthy after the maximum restart attempts, it is marked as failed
5. An alert is generated for manual intervention

When a node is detected as unhealthy:

1. The health monitor marks the node as unhealthy
2. If auto-healing is enabled, containers are rescheduled to healthy nodes
3. The node is put into maintenance mode
4. Recovery procedures are attempted on the node
5. If recovery is successful, the node is marked as healthy and returned to service
6. If recovery fails, the node is marked as failed and an alert is generated

### Metrics Collection and Storage

Health metrics are collected at regular intervals and stored for historical analysis:

- **Short-term metrics**: High-resolution metrics stored for 24 hours
- **Medium-term metrics**: Medium-resolution metrics stored for 7 days
- **Long-term metrics**: Low-resolution metrics stored for 30 days

Metrics are stored in a time-series database for efficient querying and analysis.

## Advanced Features

### Predictive Health Monitoring

Hivemind can use historical metrics to predict potential issues before they occur:

```bash
hivemind health predict --entity <container-id|node-id> --duration 24h
```

This analyzes trends in metrics and identifies potential future issues.

### Custom Health Check Scripts

You can create custom health check scripts for complex health checking:

```bash
# Create a custom health check script
cat > custom-health-check.sh << 'EOF'
#!/bin/bash
# Check database connection
if mysql -h localhost -u user -p'password' -e "SELECT 1" &>/dev/null; then
  # Check replication status
  SLAVE_STATUS=$(mysql -h localhost -u user -p'password' -e "SHOW SLAVE STATUS\G" | grep "Slave_IO_Running: Yes")
  if [ -n "$SLAVE_STATUS" ]; then
    exit 0
  else
    exit 1
  fi
else
  exit 2
fi
EOF

# Make it executable
chmod +x custom-health-check.sh

# Use the custom script in a container
hivemind app deploy --image mysql:8 --name mysql-replica \
  --health-cmd "/custom-health-check.sh" \
  --health-interval 30s \
  --volume $(pwd)/custom-health-check.sh:/custom-health-check.sh
```

### Health Check Dependencies

You can define dependencies between health checks:

```bash
hivemind app deploy --image myapp:latest --name api \
  --health-cmd "curl -f http://localhost:8080/health" \
  --health-interval 30s \
  --health-depends-on db
```

This ensures that the health check for `api` is only considered if `db` is healthy.

### Health Monitoring API

Hivemind provides a REST API for health monitoring:

```bash
# Get system health
curl -X GET http://<your-server>:3000/api/health

# Get container health
curl -X GET http://<your-server>:3000/api/health/container/<container-id>

# Get node health
curl -X GET http://<your-server>:3000/api/health/node/<node-id>

# Get alerts
curl -X GET http://<your-server>:3000/api/health/alerts
```

## Best Practices

### Effective Health Checks

1. **Keep health checks lightweight**: Health checks should be fast and use minimal resources
2. **Check critical functionality**: Health checks should verify that the application is functioning correctly
3. **Use appropriate intervals**: Balance between quick detection and resource usage
4. **Set appropriate thresholds**: Avoid false positives by setting reasonable thresholds
5. **Include startup delay**: Allow applications time to initialize before checking health

### Auto-Healing Configuration

1. **Start conservative**: Begin with longer intervals and higher thresholds
2. **Limit restart attempts**: Prevent endless restart loops
3. **Use exponential backoff**: Increase delay between restart attempts
4. **Monitor auto-healing actions**: Regularly review auto-healing logs
5. **Have fallback procedures**: Define manual procedures for when auto-healing fails

### Alert Management

1. **Prioritize alerts**: Set appropriate severity levels
2. **Avoid alert fatigue**: Only alert on actionable issues
3. **Include context**: Provide enough information to understand and address the issue
4. **Define escalation paths**: Know who should respond to different types of alerts
5. **Close the loop**: Ensure alerts are acknowledged and resolved

## Troubleshooting

### Common Issues

#### Health Checks Failing

**Symptoms:**
- Container marked as unhealthy
- Health check failures in logs

**Possible Causes:**
- Application not responding
- Incorrect health check configuration
- Resource constraints

**Solutions:**

1. **Check application logs:**
   ```bash
   hivemind app logs --name <app-name>
   ```

2. **Verify health check configuration:**
   ```bash
   hivemind app health-check --name <app-name>
   ```

3. **Test health check manually:**
   ```bash
   hivemind app exec --name <app-name> -- <health-check-command>
   ```

4. **Check resource usage:**
   ```bash
   hivemind app stats --name <app-name>
   ```

#### Auto-Healing Not Working

**Symptoms:**
- Unhealthy containers not being restarted
- Failed nodes not being handled

**Possible Causes:**
- Auto-healing disabled
- Configuration issues
- Resource constraints

**Solutions:**

1. **Check auto-healing configuration:**
   ```bash
   hivemind health config auto-healing
   ```

2. **Check auto-healing logs:**
   ```bash
   hivemind health logs --component auto-healing
   ```

3. **Verify resource availability:**
   ```bash
   hivemind node resources
   ```

4. **Manually trigger healing:**
   ```bash
   hivemind health heal --entity <container-id|node-id>
   ```

#### False Positive Alerts

**Symptoms:**
- Excessive alerts for healthy components
- Alerts for transient issues

**Possible Causes:**
- Thresholds too sensitive
- Flaky health checks
- Transient network issues

**Solutions:**

1. **Adjust alert thresholds:**
   ```bash
   hivemind health config alerts --cpu-threshold 90 --memory-threshold 95
   ```

2. **Increase health check failure threshold:**
   ```bash
   hivemind app update --name <app-name> --health-retries 5
   ```

3. **Add delay between health checks:**
   ```bash
   hivemind app update --name <app-name> --health-interval 60s
   ```

## Conclusion

The Health Monitoring system in Hivemind provides comprehensive monitoring, alerting, and auto-healing capabilities to ensure the reliability and availability of your containerized applications. By following the guidelines in this document, you can effectively configure and use health monitoring to maintain a healthy and resilient cluster.

For more information, refer to the following resources:
- [User Guide](user_guide.md)
- [Administration Guide](administration_guide.md)
- [Troubleshooting Guide](troubleshooting_guide.md)