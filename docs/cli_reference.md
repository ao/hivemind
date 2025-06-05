# Hivemind CLI Reference

This document provides a comprehensive reference for the Hivemind Command Line Interface (CLI), including all available commands, options, and examples.

## CLI Overview

The Hivemind CLI is a powerful tool for interacting with the Hivemind container orchestration platform. It provides commands for managing containers, services, volumes, nodes, and other resources.

### Installation

The Hivemind CLI is installed as part of the Hivemind platform. See the [Installation Guide](installation_guide.md) for details.

### Configuration

The CLI can be configured using a configuration file, environment variables, or command-line flags.

#### Configuration File

The CLI looks for a configuration file in the following locations:
1. `./hivemind.yaml`
2. `$HOME/.hivemind/config.yaml`
3. `/etc/hivemind/config.yaml`

Example configuration file:

```yaml
server: http://localhost:3000
data_dir: /var/lib/hivemind
log_level: info
```

#### Environment Variables

You can also configure the CLI using environment variables:

```bash
export HIVEMIND_SERVER=http://localhost:3000
export HIVEMIND_DATA_DIR=/var/lib/hivemind
export HIVEMIND_LOG_LEVEL=info
```

#### Command-Line Flags

Global flags that apply to all commands:

```bash
hivemind --server http://localhost:3000 --data-dir /var/lib/hivemind --log-level info
```

### Authentication

The CLI authenticates with the Hivemind server using an API token. You can log in using:

```bash
hivemind login
```

This will prompt for your username and password, and store the token in `$HOME/.hivemind/token`.

You can also provide the token directly:

```bash
hivemind --token <your-token> <command>
```

## Command Reference

### Global Commands

#### `hivemind version`

Display the version of the Hivemind CLI.

```bash
hivemind version
```

Output:
```
Hivemind CLI v1.0.0
```

#### `hivemind login`

Log in to the Hivemind server.

```bash
hivemind login [--username <username>] [--password <password>] [--server <server>]
```

Options:
- `--username`: Username for authentication
- `--password`: Password for authentication
- `--server`: Server URL

If username or password are not provided, the CLI will prompt for them.

#### `hivemind logout`

Log out from the Hivemind server.

```bash
hivemind logout
```

#### `hivemind status`

Show the status of the Hivemind server.

```bash
hivemind status
```

Output:
```
Server: http://localhost:3000
Status: Running
Version: 1.0.0
Nodes: 3 (3 healthy, 0 unhealthy)
Containers: 10 (10 running, 0 stopped)
Services: 5
```

### Daemon Commands

#### `hivemind daemon`

Start the Hivemind daemon.

```bash
hivemind daemon [--web-port <port>] [--data-dir <dir>] [--log-level <level>]
```

Options:
- `--web-port`: Port for the web interface (default: 3000)
- `--data-dir`: Directory for storing data (default: /var/lib/hivemind)
- `--log-level`: Log level (default: info)

#### `hivemind web`

Start only the web interface.

```bash
hivemind web [--port <port>]
```

Options:
- `--port`: Port for the web interface (default: 3000)

### Node Commands

#### `hivemind node ls`

List all nodes in the cluster.

```bash
hivemind node ls [--output <format>] [--show-labels] [--show-taints]
```

Options:
- `--output`: Output format (table, json, yaml)
- `--show-labels`: Show node labels
- `--show-taints`: Show node taints

Output:
```
ID      ADDRESS         STATUS  ROLE    CPU     MEMORY  DISK    CONTAINERS
node-1  192.168.1.101   Ready   worker  4/2.5   8G/4G   100G/50G  5
node-2  192.168.1.102   Ready   worker  4/3.0   8G/6G   100G/70G  3
node-3  192.168.1.103   Ready   worker  4/3.5   8G/7G   100G/80G  2
```

#### `hivemind node info`

Show detailed information about a node.

```bash
hivemind node info --node <node-id> [--output <format>]
```

Options:
- `--node`: Node ID
- `--output`: Output format (table, json, yaml)

Output:
```
Node: node-1
Address: 192.168.1.101
Status: Ready
Role: worker

Resources:
  CPU: 4 cores (2.5 available)
  Memory: 8 GB (4 GB available)
  Disk: 100 GB (50 GB available)

Containers: 5
  container-1 (web-server): Running
  container-2 (api): Running
  container-3 (db): Running
  container-4 (cache): Running
  container-5 (logger): Running

Labels:
  role: frontend
  zone: zone1

Taints:
  special=true:NoSchedule

Network:
  Subnet: 10.244.1.0/24
  Overlay Status: Connected
```

#### `hivemind node maintenance`

Put a node into maintenance mode or take it out of maintenance mode.

```bash
hivemind node maintenance --node <node-id> --enable [--drain]
hivemind node maintenance --node <node-id> --disable
```

Options:
- `--node`: Node ID
- `--enable`: Enable maintenance mode
- `--disable`: Disable maintenance mode
- `--drain`: Drain containers from the node

#### `hivemind node drain`

Drain containers from a node.

```bash
hivemind node drain --node <node-id> [--force] [--timeout <seconds>]
```

Options:
- `--node`: Node ID
- `--force`: Force draining even if it would disrupt services
- `--timeout`: Timeout in seconds (default: 300)

#### `hivemind node remove`

Remove a node from the cluster.

```bash
hivemind node remove --node <node-id> [--force]
```

Options:
- `--node`: Node ID
- `--force`: Force removal even if the node is not drained

#### `hivemind node label`

Add or remove labels from a node.

```bash
hivemind node label --node <node-id> --label <key>=<value>
hivemind node label --node <node-id> --label <key>-
```

Options:
- `--node`: Node ID
- `--label`: Label to add or remove (use `key=value` to add, `key-` to remove)

#### `hivemind node taint`

Add or remove taints from a node.

```bash
hivemind node taint --node <node-id> --key <key> --value <value> --effect <effect>
hivemind node taint --node <node-id> --key <key> --remove
```

Options:
- `--node`: Node ID
- `--key`: Taint key
- `--value`: Taint value
- `--effect`: Taint effect (NoSchedule, PreferNoSchedule, NoExecute)
- `--remove`: Remove the taint

#### `hivemind node resources`

Show resource usage on a node.

```bash
hivemind node resources [--node <node-id>] [--output <format>]
```

Options:
- `--node`: Node ID (if not specified, shows all nodes)
- `--output`: Output format (table, json, yaml)

Output:
```
NODE    CPU     CPU%    MEMORY  MEMORY%  DISK    DISK%
node-1  1.5/4   37.5%   4G/8G   50.0%    50G/100G  50.0%
node-2  1.0/4   25.0%   2G/8G   25.0%    30G/100G  30.0%
node-3  0.5/4   12.5%   1G/8G   12.5%    20G/100G  20.0%
```

### Application Commands

#### `hivemind app ls`

List all applications.

```bash
hivemind app ls [--output <format>]
```

Options:
- `--output`: Output format (table, json, yaml)

Output:
```
NAME        IMAGE           REPLICAS  SERVICE     CREATED
web-server  nginx:latest    2/2       web.local   2025-06-01 12:00:00
api         myapi:1.0       3/3       api.local   2025-06-01 12:05:00
db          postgres:13     1/1       db.local    2025-06-01 12:10:00
```

#### `hivemind app deploy`

Deploy a new application.

```bash
hivemind app deploy --image <image> --name <name> [options]
```

Options:
- `--image`: Container image to deploy
- `--name`: Name for the application
- `--service`: Service domain name
- `--replicas`: Number of replicas to deploy (default: 1)
- `--cpu`: CPU request (cores)
- `--cpu-limit`: CPU limit (cores)
- `--memory`: Memory request
- `--memory-limit`: Memory limit
- `--env`: Environment variables (can be specified multiple times)
- `--volume`: Volume mounts (can be specified multiple times)
- `--port`: Port mappings (can be specified multiple times)
- `--health-cmd`: Health check command
- `--health-interval`: Health check interval
- `--health-timeout`: Health check timeout
- `--health-retries`: Health check retries
- `--health-start-period`: Health check start period
- `--node`: Specific node to deploy on
- `--node-affinity`: Node affinity constraints
- `--node-anti-affinity`: Node anti-affinity constraints
- `--service-affinity`: Service affinity constraints
- `--service-anti-affinity`: Service anti-affinity constraints
- `--toleration`: Tolerations for node taints
- `--priority-class`: Priority class for the application
- `--network-aware`: Enable network-aware scheduling

Examples:

```bash
# Simple deployment
hivemind app deploy --image nginx:latest --name web-server

# Deployment with service domain
hivemind app deploy --image nginx:latest --name web-server --service web.local

# Deployment with resource limits
hivemind app deploy --image nginx:latest --name web-server --cpu 0.5 --memory 256M

# Deployment with environment variables
hivemind app deploy --image postgres:13 --name db --env "POSTGRES_PASSWORD=secret" --env "POSTGRES_USER=admin"

# Deployment with volume mounts
hivemind app deploy --image mysql:8 --name mysql-db --volume mysql-data:/var/lib/mysql

# Deployment with health checks
hivemind app deploy --image nginx:latest --name web-server --health-cmd "curl -f http://localhost/" --health-interval 30s

# Deployment with node affinity
hivemind app deploy --image nginx:latest --name web-server --node-affinity "role=frontend"
```

#### `hivemind app info`

Show detailed information about an application.

```bash
hivemind app info --name <app-name> [--output <format>]
```

Options:
- `--name`: Application name
- `--output`: Output format (table, json, yaml)

Output:
```
Application: web-server
Image: nginx:latest
Replicas: 2/2
Service: web.local
Created: 2025-06-01 12:00:00

Containers:
  container-1: Running (node-1)
  container-2: Running (node-2)

Resources:
  CPU: 0.5 (limit: 1.0)
  Memory: 256M (limit: 512M)

Environment:
  ENV_VAR1: value1
  ENV_VAR2: value2

Volumes:
  data-volume: /data

Ports:
  80/tcp -> 8080/tcp

Health Check:
  Command: curl -f http://localhost/
  Interval: 30s
  Timeout: 5s
  Retries: 3
  Start Period: 60s
```

#### `hivemind app scale`

Scale an application to a specific number of replicas.

```bash
hivemind app scale --name <app-name> --replicas <count>
```

Options:
- `--name`: Application name
- `--replicas`: Number of replicas

#### `hivemind app restart`

Restart an application.

```bash
hivemind app restart --name <app-name>
```

Options:
- `--name`: Application name

#### `hivemind app delete`

Delete an application.

```bash
hivemind app delete --name <app-name> [--force]
```

Options:
- `--name`: Application name
- `--force`: Force deletion even if it would disrupt services

#### `hivemind app logs`

View logs from a container.

```bash
hivemind app logs --name <app-name> [--follow] [--tail <lines>] [--since <time>]
```

Options:
- `--name`: Application name
- `--follow`: Follow log output
- `--tail`: Number of lines to show from the end
- `--since`: Show logs since timestamp (e.g., 2025-06-01T12:00:00Z) or relative time (e.g., 5m)

#### `hivemind app exec`

Execute a command in a container.

```bash
hivemind app exec --name <app-name> -- <command> [args...]
```

Options:
- `--name`: Application name
- `--`: Separator between hivemind options and the command to execute

Example:
```bash
hivemind app exec --name web-server -- ls -la /var/www/html
```

#### `hivemind app stats`

Show resource usage statistics for a container.

```bash
hivemind app stats --name <app-name> [--watch]
```

Options:
- `--name`: Application name
- `--watch`: Watch statistics in real-time

Output:
```
NAME        CPU%    MEM USAGE / LIMIT     MEM%    NET I/O          BLOCK I/O
web-server  0.50%   128 MB / 256 MB       50.00%  1.2 MB / 2.3 MB  4.5 MB / 2.1 MB
```

### Service Commands

#### `hivemind service ls`

List all services.

```bash
hivemind service ls [--output <format>]
```

Options:
- `--output`: Output format (table, json, yaml)

Output:
```
NAME        DOMAIN      ENDPOINTS  STRATEGY
web.local   web.local   2          round-robin
api.local   api.local   3          round-robin
db.local    db.local    1          round-robin
```

#### `hivemind service info`

Show detailed information about a service.

```bash
hivemind service info --name <service-name> [--output <format>]
```

Options:
- `--name`: Service name
- `--output`: Output format (table, json, yaml)

Output:
```
Service: web.local
Domain: web.local
Load Balancing Strategy: round-robin

Endpoints:
  container-1: 10.244.1.2:80 (node-1) - Healthy
  container-2: 10.244.2.2:80 (node-2) - Healthy

Health Check:
  Protocol: http
  Path: /health
  Interval: 30s
  Timeout: 5s
  Healthy Threshold: 2
  Unhealthy Threshold: 3
```

#### `hivemind service update`

Update a service configuration.

```bash
hivemind service update --name <service-name> [options]
```

Options:
- `--name`: Service name
- `--lb-strategy`: Load balancing strategy (round-robin, least-connections, random)
- `--health-protocol`: Health check protocol (http, https, tcp)
- `--health-path`: Health check path
- `--health-interval`: Health check interval
- `--health-timeout`: Health check timeout
- `--health-healthy-threshold`: Healthy threshold
- `--health-unhealthy-threshold`: Unhealthy threshold

#### `hivemind service endpoints`

List endpoints for a service.

```bash
hivemind service endpoints --name <service-name> [--output <format>]
```

Options:
- `--name`: Service name
- `--output`: Output format (table, json, yaml)

Output:
```
CONTAINER    IP ADDRESS    PORT  NODE    HEALTH
container-1  10.244.1.2    80    node-1  Healthy
container-2  10.244.2.2    80    node-2  Healthy
```

### Volume Commands

#### `hivemind volume ls`

List all volumes.

```bash
hivemind volume ls [--output <format>]
```

Options:
- `--output`: Output format (table, json, yaml)

Output:
```
NAME         SIZE    TYPE    CREATED             USED BY
data-volume  10G     local   2025-06-01 12:00:00  container-1
config-vol   1G      local   2025-06-01 12:05:00  container-2
logs-vol     5G      local   2025-06-01 12:10:00  container-3
```

#### `hivemind volume create`

Create a new volume.

```bash
hivemind volume create --name <volume-name> [options]
```

Options:
- `--name`: Volume name
- `--size`: Volume size (e.g., "10G")
- `--type`: Volume type (default: "local")
- `--labels`: Labels for the volume (e.g., "app=db,env=prod")

#### `hivemind volume delete`

Delete a volume.

```bash
hivemind volume delete --name <volume-name> [--force]
```

Options:
- `--name`: Volume name
- `--force`: Force deletion even if the volume is in use

#### `hivemind volume usage`

Show volume usage.

```bash
hivemind volume usage --name <volume-name> [--output <format>]
```

Options:
- `--name`: Volume name
- `--output`: Output format (table, json, yaml)

Output:
```
NAME         SIZE    USED    AVAILABLE  USE%
data-volume  10G     5G      5G         50%
```

#### `hivemind volume backup`

Back up a volume.

```bash
hivemind volume backup --name <volume-name> --output <file-path>
```

Options:
- `--name`: Volume name
- `--output`: Output file path

#### `hivemind volume restore`

Restore a volume from a backup.

```bash
hivemind volume restore --name <volume-name> --input <file-path>
```

Options:
- `--name`: Volume name
- `--input`: Input file path

### Health Commands

#### `hivemind health`

Show the overall health of the system.

```bash
hivemind health [--output <format>]
```

Options:
- `--output`: Output format (table, json, yaml)

Output:
```
System Health: Healthy

Components:
  Nodes: Healthy (3/3)
  Containers: Healthy (10/10)
  Services: Healthy (5/5)

Alerts:
  Critical: 0
  Warning: 0
  Info: 0
```

#### `hivemind health alerts`

Show active alerts.

```bash
hivemind health alerts [--history] [--duration <duration>] [--output <format>]
```

Options:
- `--history`: Show alert history
- `--duration`: Duration for history (e.g., "7d")
- `--output`: Output format (table, json, yaml)

Output:
```
ID                                   SEVERITY  SOURCE           MESSAGE                                  CREATED
alert-1234-5678-90ab-cdef            Warning   container-1      Container health check failed            2025-06-01 12:00:00
alert-2345-6789-01bc-defg            Critical  node-2           Node unreachable                         2025-06-01 12:05:00
```

#### `hivemind health alert-ack`

Acknowledge an alert.

```bash
hivemind health alert-ack --id <alert-id>
```

Options:
- `--id`: Alert ID

#### `hivemind health config`

Configure health monitoring.

```bash
hivemind health config auto-healing [options]
hivemind health config alerts [options]
hivemind health config notifications [options]
```

Options for `auto-healing`:
- `--container-restart`: Enable/disable automatic container restart
- `--max-restart-attempts`: Maximum number of restart attempts
- `--restart-delay`: Delay between restart attempts
- `--node-recovery`: Enable/disable automatic node recovery

Options for `alerts`:
- `--cpu-threshold`: CPU usage threshold (percentage)
- `--memory-threshold`: Memory usage threshold (percentage)
- `--disk-threshold`: Disk usage threshold (percentage)
- `--health-failure-threshold`: Number of health check failures before alerting

Options for `notifications`:
- `--email`: Configure email notifications
- `--slack`: Configure Slack notifications

### Security Commands

#### `hivemind security scan-image`

Scan a container image for vulnerabilities.

```bash
hivemind security scan-image --image <image> [--output <format>]
```

Options:
- `--image`: Container image to scan
- `--output`: Output format (table, json, yaml)

Output:
```
Image: nginx:latest
Scan Time: 2025-06-01 12:00:00
Status: Completed
Compliant: Yes

Vulnerabilities:
ID             SEVERITY  PACKAGE  VERSION  FIXED IN   DESCRIPTION
CVE-2023-1234  Medium    openssl  1.1.1    1.1.1g     This is a sample vulnerability
CVE-2023-5678  Low       libc     2.31     2.31-1     Another sample vulnerability
```

#### `hivemind security list-policies`

List security policies.

```bash
hivemind security list-policies [--output <format>]
```

Options:
- `--output`: Output format (table, json, yaml)

Output:
```
ID       NAME                      MAX SEVERITY  BLOCK ON    ALLOWED REGISTRIES
default  Default Security Policy   Medium        High        docker.io,gcr.io,quay.io
```

#### `hivemind security network-policy ls`

List network policies.

```bash
hivemind security network-policy ls [--output <format>]
```

Options:
- `--output`: Output format (table, json, yaml)

Output:
```
NAME         SELECTOR      INGRESS RULES  EGRESS RULES  ENCRYPTION  LOGGING
web-to-db    app=web       1              1             No          Yes
```

#### `hivemind security network-policy create`

Create a network policy.

```bash
hivemind security network-policy create --name <name> [options]
```

Options:
- `--name`: Policy name
- `--selector`: Label selector (e.g., "app=web")
- `--allow-ingress`: Allow ingress traffic (e.g., "tcp:80")
- `--from-selector`: From selector for ingress (e.g., "app=api")
- `--from-cidr`: From CIDR for ingress (e.g., "10.0.0.0/8")
- `--allow-egress`: Allow egress traffic (e.g., "tcp:5432")
- `--to-selector`: To selector for egress (e.g., "app=db")
- `--to-cidr`: To CIDR for egress (e.g., "10.0.0.0/8")
- `--encryption`: Enable encryption
- `--logging`: Enable traffic logging

#### `hivemind security create-secret`

Create a new secret.

```bash
hivemind security create-secret --name <name> [--value <value> | --file <file-path>] [options]
```

Options:
- `--name`: Secret name
- `--value`: Secret value
- `--file`: File containing the secret value
- `--description`: Secret description
- `--labels`: Labels for the secret (e.g., "app=db,env=prod")

#### `hivemind security list-secrets`

List secrets (metadata only, not values).

```bash
hivemind security list-secrets [--output <format>]
```

Options:
- `--output`: Output format (table, json, yaml)

Output:
```
NAME         DESCRIPTION        VERSION  CREATED             LAST ACCESSED
db-password  Database password  1        2025-06-01 12:00:00  2025-06-01 12:05:00
api-key      API key            1        2025-06-01 12:10:00  2025-06-01 12:15:00
```

### Cluster Commands

#### `hivemind cluster status`

Show the status of the cluster.

```bash
hivemind cluster status [--output <format>]
```

Options:
- `--output`: Output format (table, json, yaml)

Output:
```
Cluster Status: Healthy

Nodes:
  Total: 3
  Healthy: 3
  Unhealthy: 0

Leader: node-1

Membership:
  Protocol: SWIM
  Gossip Interval: 1s
  Failure Detection: Enabled
```

#### `hivemind cluster join`

Join an existing cluster.

```bash
hivemind join --host <host-address> [--port <port>]
```

Options:
- `--host`: Host address of an existing node
- `--port`: Port of the existing node (default: 3000)

#### `hivemind cluster leave`

Leave the cluster.

```bash
hivemind leave [--force]
```

Options:
- `--force`: Force leaving even if it would disrupt services

#### `hivemind cluster reconcile`

Force a cluster reconciliation.

```bash
hivemind cluster reconcile
```

### Admin Commands

#### `hivemind admin config`

Configure system settings.

```bash
hivemind admin config set <key> <value>
hivemind admin config get <key>
```

Examples:
```bash
hivemind admin config set scheduler.rebalance_threshold 20
hivemind admin config get scheduler.rebalance_threshold
```

#### `hivemind admin backup`

Create a backup of Hivemind data.

```bash
hivemind admin backup --output <directory>
```

Options:
- `--output`: Output directory

#### `hivemind admin restore`

Restore from a backup.

```bash
hivemind admin restore --input <directory>
```

Options:
- `--input`: Input directory

#### `hivemind admin diagnostics`

Run system diagnostics.

```bash
hivemind admin diagnostics [--output <format>]
```

Options:
- `--output`: Output format (table, json, yaml)

Output:
```
System Diagnostics:

Components:
  API Server: Healthy
  Database: Healthy
  Container Runtime: Healthy
  Network: Healthy
  Service Discovery: Healthy

Issues:
  None found
```

#### `hivemind admin logs`

View system logs.

```bash
hivemind admin logs [--component <component>] [--level <level>] [--follow] [--tail <lines>]
```

Options:
- `--component`: Component to show logs for (e.g., "api", "scheduler")
- `--level`: Minimum log level (e.g., "info", "debug")
- `--follow`: Follow log output
- `--tail`: Number of lines to show from the end

#### `hivemind admin set-quota`

Set resource quotas.

```bash
hivemind admin set-quota [--namespace <namespace>] --cpu <cpu> --memory <memory> --storage <storage>
```

Options:
- `--namespace`: Namespace to set quota for (if not specified, sets cluster-wide quota)
- `--cpu`: CPU quota
- `--memory`: Memory quota
- `--storage`: Storage quota

## Environment Variables

The following environment variables can be used to configure the CLI:

| Variable | Description | Default |
|----------|-------------|---------|
| `HIVEMIND_SERVER` | Server URL | http://localhost:3000 |
| `HIVEMIND_DATA_DIR` | Data directory | /var/lib/hivemind |
| `HIVEMIND_LOG_LEVEL` | Log level | info |
| `HIVEMIND_TOKEN` | API token | |
| `HIVEMIND_CONFIG` | Path to config file | |

## Exit Codes

The CLI uses the following exit codes:

| Code | Description |
|------|-------------|
| 0 | Success |
| 1 | General error |
| 2 | Command line parsing error |
| 3 | Connection error |
| 4 | Authentication error |
| 5 | Permission denied |
| 6 | Resource not found |
| 7 | Resource conflict |

## Bash Completion

To enable bash completion for the Hivemind CLI:

```bash
hivemind completion bash > /etc/bash_completion.d/hivemind
```

For other shells:

```bash
hivemind completion zsh > ~/.zshrc.d/_hivemind
hivemind completion fish > ~/.config/fish/completions/hivemind.fish
```

## Conclusion

This CLI reference covers the main commands and options of the Hivemind CLI. For more detailed information on specific commands or features, refer to the following resources:

- [User Guide](user_guide.md)
- [Administration Guide](administration_guide.md)
- [Troubleshooting Guide](troubleshooting_guide.md)
- [API Reference](api_reference.md)