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
- Go 1.18 or later (if building from source)

### Checking Hivemind Status

To verify that Hivemind is running properly:

```bash
hivemind health
```

This will show the status of the Hivemind daemon, nodes, and services.

### Accessing the Web Dashboard

Hivemind provides a fully functional web dashboard for easy management:

1. Start the Hivemind server:
   ```bash
   go run cmd/hivemind/main.go --runtime docker
   ```

2. Open a web browser and navigate to:
   ```
   http://localhost:4483
   ```

3. You will see the Hivemind dashboard with full navigation functionality

The web interface includes all major management features:
- **Dashboard**: Cluster overview and system status
- **Applications**: Application deployment and management
- **Containers**: Container monitoring and control
- **Nodes**: Node health and resource monitoring
- **Services**: Service discovery and load balancing
- **Volumes**: Persistent storage management
- **Health**: System health monitoring and diagnostics

All pages are fully functional with proper template rendering and navigation.

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

1. Navigate to `http://localhost:4483`
2. Click on "Applications" in the navigation menu
3. Click "Deploy New Application" or use the deployment form
4. Fill in the deployment form:
   - Image: The container image to deploy
   - Name: A name for your application
   - Service Domain (optional): A domain name for service discovery
   - Replicas: Number of instances to deploy
   - Resource Limits: CPU and memory limits
   - Environment Variables: Key-value pairs for configuration
   - Volumes: Storage volumes to mount
5. Click "Deploy" to start the deployment

The web interface now includes:
- **Real-time Updates**: Live status updates for deployments
- **Interactive Forms**: User-friendly forms for all operations
- **Navigation**: Consistent navigation across all pages
- **Error Handling**: Proper error messages and validation
- **Template Rendering**: All pages render correctly without template errors

### Using the REST API

For automated deployments, you can use the REST API or the Go client library:

```bash
# Using curl with the REST API
curl -X POST http://<your-server>:4483/api/deploy \
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

```go
// Using the Go client library
package main

import (
    "context"
    "log"
    
    "github.com/ao/hivemind/pkg/client"
    "github.com/ao/hivemind/pkg/api/types"
)

func main() {
    // Create a new client
    c, err := client.NewClient("http://localhost:4483")
    if err != nil {
        log.Fatalf("Failed to create client: %v", err)
    }
    
    // Define deployment
    deployment := &types.Deployment{
        Image:    "nginx:latest",
        Name:     "web-server",
        Service:  "web.local",
        Replicas: 2,
        Resources: types.Resources{
            CPU:    0.5,
            Memory: "256M",
        },
        Env: map[string]string{
            "KEY1": "value1",
            "KEY2": "value2",
        },
        Volumes: []types.Volume{
            {
                Name:      "data-volume",
                MountPath: "/data",
            },
        },
    }
    
    // Deploy the application
    result, err := c.Deploy(context.Background(), deployment)
    if err != nil {
        log.Fatalf("Deployment failed: %v", err)
    }
    
    log.Printf("Deployment successful: %s", result.Message)
}
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
  --health-cmd "curl -f http://localhost:4483/health" \
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

You can also manage secrets programmatically using the Go client:

```go
package main

import (
    "context"
    "log"
    
    "github.com/ao/hivemind/pkg/client"
)

func main() {
    // Create a new client
    c, err := client.NewClient("http://localhost:4483")
    if err != nil {
        log.Fatalf("Failed to create client: %v", err)
    }
    
    // Create a secret
    err = c.CreateSecret(context.Background(), "db-password", "my-secure-password")
    if err != nil {
        log.Fatalf("Failed to create secret: %v", err)
    }
    
    log.Println("Secret created successfully")
}
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

The Hivemind web dashboard provides a fully functional, user-friendly interface for managing your cluster. All template rendering issues have been resolved, and navigation works correctly across all pages.

### Dashboard Overview

The dashboard home page shows:
- **Cluster Status**: Overall health and node count
- **Resource Usage**: CPU, memory, and storage utilization
- **Active Applications**: Running applications and their status
- **Recent Activity**: Latest deployments and system events
- **Quick Actions**: Common management tasks

### Navigation Features

The web interface includes consistent navigation with:
- **Responsive Design**: Works on desktop and mobile devices
- **Real-time Updates**: Live data refresh for monitoring
- **Error Handling**: Proper error messages and recovery
- **Template Consistency**: All pages use consistent base templates
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

Using the Go client:

```go
package main

import (
    "context"
    "log"
    
    "github.com/ao/hivemind/pkg/client"
    "github.com/ao/hivemind/pkg/api/types"
)

func main() {
    // Create a new client
    c, err := client.NewClient("http://localhost:4483")
    if err != nil {
        log.Fatalf("Failed to create client: %v", err)
    }
    
    // Configure auto-scaling
    autoscale := &types.AutoscaleConfig{
        AppName:     "web",
        MinReplicas: 2,
        MaxReplicas: 10,
        CPUPercent:  80,
    }
    
    err = c.ConfigureAutoscale(context.Background(), autoscale)
    if err != nil {
        log.Fatalf("Failed to configure auto-scaling: %v", err)
    }
    
    log.Println("Auto-scaling configured successfully")
}
```

### Custom Health Checks

Configure custom health checks:

```bash
hivemind app deploy --image myapp:latest --name myapp \
  --health-cmd "curl -f http://localhost:4483/health" \
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
  --port 80:4483  # Maps container port 80 to host port 8080
```

### Command and Arguments

Override the container's command and arguments:

```bash
hivemind app deploy --image ubuntu:latest --name task \
  --command "/bin/bash" \
  --args "-c,echo Hello World"
```

### Advanced Deployment Strategies

Hivemind supports several advanced deployment strategies for zero-downtime updates and testing:

#### Blue-Green Deployment

Deploy a new version alongside the old one and switch traffic when ready:

```bash
hivemind app deploy --image myapp:v2 --name myapp \
  --strategy blue-green \
  --verification-timeout 60
```

#### Canary Deployment

Gradually roll out a new version to a subset of users:

```bash
hivemind app deploy --image myapp:v2 --name myapp \
  --strategy canary \
  --percentage 20 \
  --steps 20,50,100 \
  --interval 300
```

#### A/B Testing

Deploy multiple variants for testing:

```bash
hivemind app deploy --name myapp \
  --strategy ab-testing \
  --variant "name=v1,image=myapp:v1,percentage=50" \
  --variant "name=v2,image=myapp:v2,percentage=50" \
  --duration 86400
```

### CI/CD Integration

Hivemind integrates with CI/CD pipelines for automated deployments:

#### GitHub Actions Integration

```yaml
name: Deploy to Hivemind

on:
  push:
    branches: [ main ]

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
    - uses: actions/checkout@v2
    - name: Build and push Docker image
      uses: docker/build-push-action@v2
      with:
        push: true
        tags: myapp:latest
    - name: Deploy to Hivemind
      run: |
        curl -X POST http://hivemind-server:4483/api/deploy \
          -H "Content-Type: application/json" \
          -d '{
            "image": "myapp:latest",
            "name": "myapp",
            "service": "myapp.local"
          }'
```

#### Pipeline Management

Create and manage CI/CD pipelines:

```bash
# Create a pipeline
hivemind cicd create-pipeline --name build-deploy \
  --source github \
  --repository myuser/myapp \
  --branch main \
  --trigger push \
  --action "build,test,deploy"

# Trigger a pipeline
hivemind cicd trigger-pipeline --name build-deploy
```

### Cloud Provider Integration

Hivemind integrates with major cloud providers:

#### AWS Integration

```bash
# Configure AWS provider
hivemind cloud configure --provider aws \
  --region us-east-1 \
  --access-key $AWS_ACCESS_KEY \
  --secret-key $AWS_SECRET_KEY

# Create an AWS instance
hivemind cloud create-instance --provider aws \
  --type t3.medium \
  --name hivemind-node-1 \
  --join-cluster
```

#### Azure Integration

```bash
# Configure Azure provider
hivemind cloud configure --provider azure \
  --subscription-id $AZURE_SUBSCRIPTION_ID \
  --tenant-id $AZURE_TENANT_ID \
  --client-id $AZURE_CLIENT_ID \
  --client-secret $AZURE_CLIENT_SECRET

# Create an Azure instance
hivemind cloud create-instance --provider azure \
  --size Standard_D2s_v3 \
  --name hivemind-node-1 \
  --join-cluster
```

#### GCP Integration

```bash
# Configure GCP provider
hivemind cloud configure --provider gcp \
  --project-id $GCP_PROJECT_ID \
  --credentials-file $GCP_CREDENTIALS_FILE

# Create a GCP instance
hivemind cloud create-instance --provider gcp \
  --machine-type n1-standard-2 \
  --name hivemind-node-1 \
  --join-cluster
```

### Helm Chart Support

Hivemind supports deploying applications using Helm charts:

#### Adding Helm Repositories

```bash
hivemind helm repo add --name stable --url https://charts.helm.sh/stable
```

#### Listing Available Charts

```bash
hivemind helm search --repo stable
```

#### Installing Charts

```bash
hivemind helm install --name my-release \
  --chart stable/nginx \
  --set service.type=ClusterIP
```

#### Managing Releases

```bash
# List releases
hivemind helm list

# Upgrade a release
hivemind helm upgrade --name my-release \
  --chart stable/nginx \
  --set replicaCount=3

# Rollback a release
hivemind helm rollback --name my-release --revision 1
```

### Observability Features

Hivemind includes comprehensive observability features:

#### Metrics

View system metrics:

```bash
hivemind metrics
```

Configure Prometheus integration:

```bash
hivemind observability configure-metrics \
  --prometheus-endpoint /metrics \
  --scrape-interval 15s
```

#### Distributed Tracing

Configure OpenTelemetry tracing:

```bash
hivemind observability configure-tracing \
  --provider jaeger \
  --endpoint http://jaeger:14268/api/traces
```

#### Log Aggregation

Configure log aggregation:

```bash
hivemind observability configure-logging \
  --provider elasticsearch \
  --endpoint http://elasticsearch:9200
```

## Conclusion

This guide covers the basic and advanced usage of Hivemind. For more detailed information on specific topics, refer to the following resources:

- [Installation Guide](installation_guide.md)
- [Administration Guide](administration_guide.md)
- [Troubleshooting Guide](troubleshooting_guide.md)
- [API Reference](api_reference.md)
- [CLI Reference](cli_reference.md)
- [Deployment Guide](deployment_guide.md)

If you encounter any issues or have questions, please refer to the [Troubleshooting Guide](troubleshooting_guide.md) or visit the [GitHub repository](https://github.com/ao/hivemind) for support.