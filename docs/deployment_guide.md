# Hivemind Deployment Guide

This guide provides comprehensive instructions for deploying Hivemind in production environments, covering installation, configuration, scaling, and maintenance.

## Table of Contents

1. [Production Requirements](#production-requirements)
2. [Installation](#installation)
3. [Configuration](#configuration)
4. [Cluster Setup](#cluster-setup)
5. [Security Considerations](#security-considerations)
6. [High Availability](#high-availability)
7. [Scaling](#scaling)
8. [Monitoring](#monitoring)
9. [Backup and Recovery](#backup-and-recovery)
10. [Upgrades](#upgrades)
11. [Troubleshooting](#troubleshooting)

## Production Requirements

Before deploying Hivemind in production, ensure your environment meets these requirements:

### Hardware Requirements

| Component | Minimum | Recommended |
|-----------|---------|-------------|
| CPU | 2 cores | 4+ cores |
| Memory | 2 GB RAM | 4+ GB RAM |
| Disk | 20 GB | 50+ GB |
| Network | 1 Gbps | 10 Gbps |

### Software Requirements

- **Operating System**: Linux (Ubuntu 20.04+, CentOS 8+, or Debian 11+)
- **Go**: 1.18 or newer
- **containerd**: 1.5 or newer
- **SQLite**: 3.35 or newer
- **iptables**: For network policy enforcement
- **VXLAN support**: For overlay networking

## Installation

### Production Installation

For production deployments, we recommend installing from source to ensure you have the latest stable version:

```bash
# Install dependencies
sudo apt update
sudo apt install -y build-essential pkg-config libsqlite3-dev containerd iptables golang-go

# Clone the repository
git clone https://github.com/ao/hivemind.git
cd hivemind

# Build and install
make build
sudo cp bin/hivemind /usr/local/bin/
```

### System Service Setup

Create a systemd service file for Hivemind:

```bash
sudo tee /etc/systemd/system/hivemind.service > /dev/null << 'EOF'
[Unit]
Description=Hivemind Container Orchestration Platform
After=network.target containerd.service
Requires=containerd.service

[Service]
ExecStart=/usr/local/bin/hivemind daemon --web-port 3000 --data-dir /var/lib/hivemind
Restart=always
RestartSec=5
User=root
Group=root
LimitNOFILE=infinity
LimitNPROC=infinity
LimitCORE=infinity

[Install]
WantedBy=multi-user.target
EOF
```

Create the data directory:

```bash
sudo mkdir -p /var/lib/hivemind
```

Enable and start the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable hivemind
sudo systemctl start hivemind
```

## Configuration

### Configuration File

Create a configuration file at `/etc/hivemind/config.yaml`:

```bash
sudo mkdir -p /etc/hivemind
sudo tee /etc/hivemind/config.yaml > /dev/null << 'EOF'
# Hivemind Configuration

# Node configuration
node:
  name: "node1"
  role: "master"
  labels:
    environment: "production"
    region: "us-east"

# Web interface configuration
web:
  port: 3000
  tls:
    enabled: true
    cert_file: "/etc/hivemind/certs/server.crt"
    key_file: "/etc/hivemind/certs/server.key"

# Storage configuration
storage:
  data_dir: "/var/lib/hivemind"
  volume_dir: "/var/lib/hivemind/volumes"
  driver: "local"  # Options: local, nfs, csi

# Network configuration
network:
  overlay:
    enabled: true
    vxlan_id: 42
    subnet: "10.244.0.0/16"
  service_cidr: "10.245.0.0/16"
  dns_domain: "cluster.local"
  cni_plugins:
    - "bridge"
    - "portmap"
    - "firewall"

# Security configuration
security:
  rbac:
    enabled: true
  network_policies:
    enabled: true
  container_scanning:
    enabled: true
  secrets:
    encryption_key: "/etc/hivemind/keys/secret.key"

# Observability configuration
observability:
  metrics:
    enabled: true
    prometheus_endpoint: "/metrics"
  tracing:
    enabled: true
    jaeger_endpoint: "http://jaeger:14268/api/traces"
  logging:
    level: "info"
    json_format: true
    output: "file"  # Options: file, stdout, syslog
    file_path: "/var/log/hivemind/hivemind.log"
EOF
```

Update the Hivemind service to use this configuration:

```bash
sudo systemctl edit hivemind
```

Add the following:

```
[Service]
ExecStart=
ExecStart=/usr/local/bin/hivemind daemon --config /etc/hivemind/config.yaml
```

Reload and restart the service:

```bash
sudo systemctl daemon-reload
sudo systemctl restart hivemind
```

## Cluster Setup

### Setting Up a Multi-Node Cluster

1. **Install Hivemind** on all nodes following the installation steps above.

2. **Configure the first node** as the master node.

3. **Join additional nodes** to the cluster:

```bash
# On worker nodes
hivemind join --host <master-node-ip> --port 7946
```

### Node Roles

Configure node roles in the configuration file:

```yaml
node:
  name: "node2"
  role: "worker"  # "master" or "worker"
  labels:
    workload: "compute"
```

## Security Considerations

### TLS Configuration

Generate self-signed certificates for development:

```bash
mkdir -p /etc/hivemind/certs
openssl req -x509 -newkey rsa:4096 -keyout /etc/hivemind/certs/server.key \
  -out /etc/hivemind/certs/server.crt -days 365 -nodes -subj "/CN=hivemind"
```

For production, use certificates from a trusted CA.

### RBAC Setup

Create roles and users:

```bash
# Create admin role
hivemind security create-role --name admin --resource "*" --action "*"

# Create developer role with limited permissions
hivemind security create-role --name developer --resource "container,service" --action "create,read,update,delete"

# Create read-only role
hivemind security create-role --name viewer --resource "*" --action "read"

# Create users and assign roles
hivemind security create-user --name admin-user --password "secure-password"
hivemind security assign-role --user admin-user --role admin

hivemind security create-user --name dev-user --password "dev-password"
hivemind security assign-role --user dev-user --role developer
```

### Network Policies

Create default network policies:

```bash
# Default deny policy
hivemind security network-policy create --name default-deny \
  --selector "*" \
  --deny-all-ingress \
  --deny-all-egress

# Allow DNS policy
hivemind security network-policy create --name allow-dns \
  --selector "*" \
  --allow-egress udp:53 --to-selector "app=kube-dns"
```

### Secret Management

Create and use secrets:

```bash
# Create a secret
hivemind security create-secret --name db-password --value "secure-password"

# Use the secret in a deployment
hivemind app deploy --image postgres:13 --name postgres \
  --secret db-password:POSTGRES_PASSWORD
```

## High Availability

### Master Node HA

For high availability of the master node:

1. **Deploy multiple master nodes** (at least 3 for quorum).

2. **Set up a load balancer** in front of the master nodes.

3. **Configure etcd** for distributed state storage:

```yaml
storage:
  type: "etcd"
  endpoints:
    - "http://etcd1:2379"
    - "http://etcd2:2379"
    - "http://etcd3:2379"
  prefix: "/hivemind/"
  tls:
    enabled: true
    cert_file: "/etc/hivemind/certs/etcd-client.crt"
    key_file: "/etc/hivemind/certs/etcd-client.key"
    ca_file: "/etc/hivemind/certs/etcd-ca.crt"
```

4. **Implement leader election** using the Go implementation's built-in leader election mechanism:

```yaml
cluster:
  leader_election:
    enabled: true
    lease_duration: "15s"
    renew_deadline: "10s"
    retry_period: "2s"
```

### Worker Node HA

For high availability of worker nodes:

1. **Deploy multiple worker nodes** across different failure domains.

2. **Use anti-affinity rules** to distribute critical workloads:

```bash
hivemind app deploy --image critical-app:latest --name critical-app \
  --node-anti-affinity "node.zone"
```

## Scaling

### Horizontal Scaling

Add more nodes to the cluster:

```bash
# On a new node
hivemind join --host <master-node-ip> --port 7946
```

### Vertical Scaling

Increase resources on existing nodes:

1. **Stop Hivemind** on the node:

```bash
sudo systemctl stop hivemind
```

2. **Add resources** to the node (CPU, memory, disk).

3. **Start Hivemind**:

```bash
sudo systemctl start hivemind
```

### Application Scaling

Scale applications based on demand:

```bash
# Manual scaling
hivemind app scale --name web-app --replicas 5

# Auto-scaling
hivemind app autoscale --name web-app --min-replicas 2 --max-replicas 10 --cpu-percent 80
```

## Monitoring

### Prometheus Integration

Enable Prometheus metrics:

```yaml
observability:
  metrics:
    enabled: true
    prometheus_endpoint: "/metrics"
    expose_process_metrics: true
    expose_go_metrics: true
```

Configure Prometheus to scrape Hivemind metrics:

```yaml
scrape_configs:
  - job_name: 'hivemind'
    scrape_interval: 15s
    static_configs:
      - targets: ['hivemind:4483']
    metrics_path: '/metrics'
    scheme: 'http'
```

Example of available metrics in the Go implementation:

```
# HELP hivemind_containers_total Total number of containers managed by Hivemind
# TYPE hivemind_containers_total gauge
hivemind_containers_total{node="node1"} 42

# HELP hivemind_container_status_count Number of containers in each status
# TYPE hivemind_container_status_count gauge
hivemind_container_status_count{node="node1",status="running"} 38
hivemind_container_status_count{node="node1",status="stopped"} 4

# HELP hivemind_node_cpu_usage_percent CPU usage percentage by node
# TYPE hivemind_node_cpu_usage_percent gauge
hivemind_node_cpu_usage_percent{node="node1"} 35.2

# HELP hivemind_node_memory_usage_bytes Memory usage in bytes by node
# TYPE hivemind_node_memory_usage_bytes gauge
hivemind_node_memory_usage_bytes{node="node1"} 4294967296
```

### Grafana Dashboards

Import the Hivemind dashboards into Grafana:

1. Navigate to Grafana
2. Go to Dashboards > Import
3. Upload the dashboard JSON files from the `dashboards/` directory

### Alerting

Configure alerts in Prometheus:

```yaml
groups:
- name: hivemind-alerts
  rules:
  - alert: NodeDown
    expr: up{job="hivemind"} == 0
    for: 5m
    labels:
      severity: critical
    annotations:
      summary: "Hivemind node is down"
      description: "Hivemind node has been down for more than 5 minutes."
```

## Backup and Recovery

### Database Backup

Backup the Hivemind database:

```bash
# Using the built-in backup command (no need to stop the service)
hivemind admin backup --output /var/backups/hivemind-$(date +%Y%m%d)

# Or manually (requires stopping the service)
sudo systemctl stop hivemind
sudo cp /var/lib/hivemind/hivemind.db /var/backups/hivemind-$(date +%Y%m%d).db
sudo systemctl start hivemind
```

You can also use the Go client to perform backups programmatically:

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"
    
    "github.com/ao/hivemind/pkg/client"
)

func main() {
    // Create a new client
    c, err := client.NewClient("http://localhost:4483")
    if err != nil {
        log.Fatalf("Failed to create client: %v", err)
    }
    
    // Create backup
    backupPath := fmt.Sprintf("/var/backups/hivemind-%s", time.Now().Format("20060102"))
    err = c.CreateBackup(context.Background(), backupPath)
    if err != nil {
        log.Fatalf("Backup failed: %v", err)
    }
    
    log.Printf("Backup created at: %s", backupPath)
}
```

### Volume Backup

Backup persistent volumes:

```bash
# Create a snapshot of volumes
sudo tar -czf /var/backups/hivemind-volumes-$(date +%Y%m%d).tar.gz /var/lib/hivemind/volumes
```

### Recovery

Restore from backup:

```bash
# Stop Hivemind
sudo systemctl stop hivemind

# Restore the database
sudo cp /var/backups/hivemind-20250101.db /var/lib/hivemind/hivemind.db

# Restore volumes if needed
sudo tar -xzf /var/backups/hivemind-volumes-20250101.tar.gz -C /

# Start Hivemind
sudo systemctl start hivemind
```

## Upgrades

### In-Place Upgrade

Perform an in-place upgrade:

```bash
# Stop Hivemind
sudo systemctl stop hivemind

# Backup the database
sudo cp /var/lib/hivemind/hivemind.db /var/backups/hivemind-pre-upgrade.db

# Update the binary
cd hivemind
git pull
cargo build --release
sudo cp target/release/hivemind /usr/local/bin/

# Start Hivemind
sudo systemctl start hivemind
```

### Rolling Upgrade

For a multi-node cluster, perform a rolling upgrade:

1. **Upgrade one node at a time**, starting with worker nodes.
2. **Verify** each node is healthy before proceeding to the next.
3. **Upgrade master nodes** one at a time, ensuring quorum is maintained.

## Troubleshooting

### Common Issues

#### Node Not Joining Cluster

Check network connectivity and firewall rules:

```bash
# Check if the required ports are open
sudo iptables -L | grep 7946

# Test connectivity to the master node
telnet <master-node-ip> 7946
```

#### Container Fails to Start

Check container logs:

```bash
hivemind app logs --name <app-name>
```

Check Hivemind logs:

```bash
journalctl -u hivemind -f
```

#### Service Discovery Issues

Check DNS resolution:

```bash
# From inside a container
nslookup <service-name>.cluster.local
```

Check service registration:

```bash
hivemind service info --name <service-name>
```

### Diagnostic Commands

Collect diagnostic information:

```bash
# Check system health
hivemind health

# Check node status
hivemind node ls

# Check network status
hivemind network status

# Generate diagnostic report
hivemind diagnostics --output /tmp/hivemind-diagnostics.tar.gz
```

## Conclusion

This deployment guide covers the essential aspects of deploying Hivemind in production. By following these guidelines, you can set up a robust, secure, and scalable container orchestration platform. For more specific scenarios or advanced configurations, refer to the component-specific documentation or reach out to the Hivemind community for support.