# Hivemind User Guide

This guide provides comprehensive instructions for using the Hivemind container orchestration platform, including deploying applications, managing services, working with volumes, and more.

## Table of Contents

1. [Getting Started](#getting-started)
2. [Deploying Applications](#deploying-applications)
3. [Managing Applications](#managing-applications)
4. [Working with Services](#working-with-services)
5. [Persistent Storage with Volumes](#persistent-storage-with-volumes)
6. [Networking](#networking)
7. [Health Monitoring](#health-monitoring)
8. [Security Features](#security-features)
9. [Web Dashboard](#web-dashboard)
10. [Advanced Usage](#advanced-usage)

## Getting Started

### Prerequisites

Before using Hivemind, ensure you have:

- Hivemind installed (see [Installation Guide](installation_guide.md))
- Docker images available in a registry (Docker Hub, ECR, etc.)
- Basic understanding of containers and microservices

### Checking Hivemind Status

To verify that Hivemind is running properly:

```bash
hivemind health
```

This will show the status of the Hivemind daemon, nodes, and services.

### Accessing the Web Dashboard

Hivemind provides a web dashboard for easy management:

1. Open a web browser
2. Navigate to `http://<your-server>:3000`
3. You should see the Hivemind dashboard

## Deploying Applications

Hivemind offers multiple ways to deploy applications:

### Using the CLI

The command line interface provides a straightforward way to deploy applications:

```bash
hivemind app deploy --image nginx:latest --name web-server
```

#### Basic Deployment Options

| Option | Description | Example |
|--------|-------------|---------|
| `--image` | Container image to deploy | `--image nginx:latest` |
| `--name` | Name for the application | `--name web-server` |
| `--service` | Service domain name | `--service web.local` |
| `--replicas` | Number of replicas to deploy | `--replicas 3` |
| `--cpu` | CPU limit (cores) | `--cpu 0.5` |
| `--memory` | Memory limit | `--memory 512M` |

#### Example: Deploying a Web Server

```bash
hivemind app deploy --image nginx:latest --name web-server --service web.local --replicas 2 --cpu 0.5 --memory 256M
```

#### Example: Deploying with Environment Variables

```bash
hivemind app deploy --image postgres:13 --name db --env "POSTGRES_PASSWORD=secret" --env "POSTGRES_USER=admin"
```

#### Example: Deploying with Volume Mounts

```bash
hivemind app deploy --image mysql:8 --name mysql-db --volume mysql-data:/var/lib/mysql
```

### Using the Web UI

The web UI provides a user-friendly way to deploy applications:

1. Navigate to `http://<your-server>:3000`
2. Click on "Deploy" in the navigation menu
3. Fill in the deployment form:
   - Image: The container image to deploy
   - Name: A name for your application
   - Service Domain (optional): A domain name for service discovery
   - Replicas: Number of instances to deploy
   - Resource Limits: CPU and memory limits
   - Environment Variables: Key-value pairs for configuration
   - Volumes: Storage volumes to mount
4. Click "Deploy" to start the deployment

### Using the REST API

For automated deployments, you can use the REST API:

```bash
curl -X POST http://<your-server>:3000/api/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "image": "nginx:latest",
    "name": "web-server",
    "service": "web.local",
    "replicas": 2,
    "resources": {
      "cpu": 0.5,
      "memory": "256M"
    },
    "env": {
      "KEY1": "value1",
      "KEY2": "value2"
    },
    "volumes": [
      {"name": "data-volume", "mountPath": "/data"}
    ]
  }'
```

## Managing Applications

### Listing Applications

To list all deployed applications:

```bash
hivemind app ls
```

### Getting Application Details

To get detailed information about an application:

```bash
hivemind app info --name <app-name>
```

### Scaling Applications

To scale an application to a specific number of replicas:

```bash
hivemind app scale --name <app-name> --replicas <count>
```

Example:
```bash
hivemind app scale --name web-server --replicas 5
```

### Restarting Applications

To restart an application:

```bash
hivemind app restart --name <app-name>
```

### Updating Applications

To update an application to a new image version:

```bash
hivemind app update --name <app-name> --image <new-image>
```

Example:
```bash
hivemind app update --name web-server --image nginx:1.21
```

### Deleting Applications

To delete an application:

```bash
hivemind app delete --name <app-name>
```

### Viewing Container Logs

To view logs from a container:

```bash
hivemind app logs --name <app-name>
```

To follow logs in real-time:

```bash
hivemind app logs --name <app-name> --follow
```

## Working with Services

Hivemind provides service discovery to make it easy for applications to find and communicate with each other.

### Creating a Service

Services are automatically created when you deploy an application with the `--service` flag:

```bash
hivemind app deploy --image nginx:latest --name web-server --service web.local
```

### Listing Services

To list all services:

```bash
hivemind service ls
```

### Getting Service Details

To get detailed information about a service:

```bash
hivemind service info --name <service-name>
```

### Accessing Services

Services can be accessed using their domain name from within the cluster:

```bash
curl http://web.local
```

For external access, you can use the node's IP address and the mapped port:

```bash
curl http://<node-ip>:<mapped-port>
```

### Service Load Balancing

Hivemind automatically load balances requests across all replicas of a service. The default load balancing strategy is round-robin, but you can configure other strategies:

```bash
hivemind service config --name <service-name> --lb-strategy least-connections
```

Available load balancing strategies:
- `round-robin`: Distributes requests evenly across all replicas
- `least-connections`: Sends requests to the replica with the fewest active connections
- `random`: Randomly selects a replica for each request

## Persistent Storage with Volumes

Hivemind provides persistent storage through volumes.

### Creating Volumes

To create a volume:

```bash
hivemind volume create --name <volume-name>
```

Example:
```bash
hivemind volume create --name mysql-data
```

### Listing Volumes

To list all volumes:

```bash
hivemind volume ls
```

### Using Volumes in Deployments

To use a volume in a deployment:

```bash
hivemind app deploy --image mysql:8 --name mysql-db --volume mysql-data:/var/lib/mysql
```

You can mount multiple volumes:

```bash
hivemind app deploy --image myapp:latest --name myapp \
  --volume config-vol:/etc/myapp/config \
  --volume data-vol:/var/lib/myapp/data
```

### Deleting Volumes

To delete a volume:

```bash
hivemind volume delete --name <volume-name>
```

## Networking

Hivemind provides a robust networking system for container communication.

### Network Architecture

Each container gets a unique IP address within the cluster's overlay network. Containers can communicate with each other using these IP addresses or service names.

### Network Policies

You can control traffic between containers using network policies:

```bash
hivemind network policy create --name web-to-db \
  --selector app=web \
  --allow-egress tcp:5432 --to-selector app=db \
  --allow-ingress tcp:80 --from-cidr 10.0.0.0/8
```

### Viewing Network Status

To view the status of the network:

```bash
hivemind network status
```

## Health Monitoring

Hivemind continuously monitors the health of containers and nodes.

### Checking System Health

To check the overall health of the system:

```bash
hivemind health
```

### Container Health Checks

Hivemind performs health checks on containers to ensure they're functioning properly. You can configure custom health checks:

```bash
hivemind app deploy --image myapp:latest --name myapp \
  --health-cmd "curl -f http://localhost:8080/health" \
  --health-interval 30s \
  --health-retries 3
```

### Node Health

To check the health of nodes in the cluster:

```bash
hivemind node health
```

## Security Features

Hivemind includes several security features to protect your applications.

### Container Scanning

Scan container images for vulnerabilities:

```bash
hivemind security scan-image --image nginx:latest
```

### Network Policies

Create network policies to control traffic between containers:

```bash
hivemind security network-policy create --name restrict-db \
  --selector app=db \
  --allow-ingress tcp:5432 --from-selector app=web
```

### Secret Management

Create and manage secrets:

```bash
# Create a secret
hivemind security create-secret --name db-password --value "my-secure-password"

# Use a secret in a deployment
hivemind app deploy --image postgres:13 --name postgres \
  --secret db-password:POSTGRES_PASSWORD
```

### Role-Based Access Control (RBAC)

Hivemind includes a comprehensive RBAC system:

```bash
# Create a role
hivemind security create-role --name developer --resource container --action create,read,update,delete

# Assign a role to a user
hivemind security assign-role --user john --role developer
```

## Web Dashboard

The Hivemind web dashboard provides a user-friendly interface for managing your cluster.

### Dashboard Overview

The dashboard home page shows:
- Cluster status
- Node resources
- Running containers
- Recent events

### Applications View

The Applications view shows:
- List of deployed applications
- Application status
- Resource usage
- Scaling controls

### Containers View

The Containers view shows:
- List of all containers
- Container status
- Resource usage
- Logs access

### Nodes View

The Nodes view shows:
- List of nodes in the cluster
- Node status
- Resource usage
- Container distribution

### Services View

The Services view shows:
- List of services
- Service endpoints
- Load balancing status

### Volumes View

The Volumes view shows:
- List of volumes
- Volume usage
- Volume mounts

## Advanced Usage

### Custom Scheduling

You can influence how containers are scheduled across nodes:

```bash
# Schedule on a specific node
hivemind app deploy --image nginx:latest --name web --node node1

# Use node affinity
hivemind app deploy --image nginx:latest --name web --node-affinity role=frontend

# Use node anti-affinity
hivemind app deploy --image nginx:latest --name web --node-anti-affinity role=database
```

### Resource Quotas

Set resource quotas for applications:

```bash
hivemind app deploy --image nginx:latest --name web \
  --cpu 0.5 --cpu-limit 1.0 \
  --memory 256M --memory-limit 512M
```

### Auto-Scaling

Configure auto-scaling for applications:

```bash
hivemind app autoscale --name web \
  --min-replicas 2 \
  --max-replicas 10 \
  --cpu-percent 80
```

### Custom Health Checks

Configure custom health checks:

```bash
hivemind app deploy --image myapp:latest --name myapp \
  --health-cmd "curl -f http://localhost:8080/health" \
  --health-interval 30s \
  --health-timeout 5s \
  --health-retries 3 \
  --health-start-period 60s
```

### Environment Variables

Set environment variables for applications:

```bash
hivemind app deploy --image myapp:latest --name myapp \
  --env "DATABASE_URL=postgres://user:password@db:5432/mydb" \
  --env "API_KEY=secret" \
  --env "DEBUG=true"
```

### Port Mapping

Map container ports to host ports:

```bash
hivemind app deploy --image nginx:latest --name web \
  --port 80:8080  # Maps container port 80 to host port 8080
```

### Command and Arguments

Override the container's command and arguments:

```bash
hivemind app deploy --image ubuntu:latest --name task \
  --command "/bin/bash" \
  --args "-c,echo Hello World"
```

## Conclusion

This guide covers the basic and advanced usage of Hivemind. For more detailed information on specific topics, refer to the following resources:

- [Installation Guide](installation_guide.md)
- [Administration Guide](administration_guide.md)
- [Troubleshooting Guide](troubleshooting_guide.md)
- [API Reference](api_reference.md)
- [CLI Reference](cli_reference.md)

If you encounter any issues or have questions, please refer to the [Troubleshooting Guide](troubleshooting_guide.md) or visit the [GitHub repository](https://github.com/ao/hivemind) for support.