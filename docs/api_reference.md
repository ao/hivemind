# Hivemind API Reference

This document provides a comprehensive reference for the Hivemind REST API, including all available endpoints, request/response formats, and examples. It also covers the Go client library for programmatic interaction with Hivemind.

## API Overview

The Hivemind API is a RESTful API that allows you to interact with the Hivemind container orchestration platform programmatically. It provides endpoints for managing containers, services, volumes, nodes, and other resources. The API is implemented in Go and follows RESTful principles.

### Base URL

All API endpoints are relative to the base URL:

```
http://<your-server>:4483/api
```

### Authentication

Most API endpoints require authentication. You can authenticate using one of the following methods:

#### API Key Authentication

Include your API key in the `Authorization` header:

```
Authorization: Bearer <your-api-key>
```

#### Basic Authentication

Use HTTP Basic Authentication with your username and password:

```
Authorization: Basic <base64-encoded-credentials>
```

#### JWT Authentication

Include a JWT token in the `Authorization` header:

```
Authorization: Bearer <your-jwt-token>
```

To obtain a JWT token, use the `/api/auth/login` endpoint.

### Response Format

All API responses are in JSON format. Successful responses have the following structure:

```json
{
  "success": true,
  "data": { ... }
}
```

Error responses have the following structure:

```json
{
  "success": false,
  "error": {
    "code": "error_code",
    "message": "Error message"
  }
}
```

### HTTP Status Codes

The API uses standard HTTP status codes:

- `200 OK`: The request was successful
- `201 Created`: A resource was successfully created
- `400 Bad Request`: The request was invalid
- `401 Unauthorized`: Authentication failed
- `403 Forbidden`: The authenticated user doesn't have permission
- `404 Not Found`: The requested resource was not found
- `409 Conflict`: The request conflicts with the current state
- `500 Internal Server Error`: An error occurred on the server

## API Endpoints

### Containers

#### List Containers

```
GET /containers
```

Lists all containers in the cluster.

**Query Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `status` | string | Filter by container status (running, stopped, etc.) |
| `node` | string | Filter by node ID |
| `app` | string | Filter by application name |
| `limit` | integer | Maximum number of containers to return |
| `offset` | integer | Offset for pagination |

**Response:**

```json
{
  "success": true,
  "data": {
    "containers": [
      {
        "id": "container-1",
        "name": "web-server",
        "image": "nginx:latest",
        "status": "running",
        "node_id": "node-1",
        "created_at": "2025-06-01T12:00:00Z",
        "ip_address": "10.244.1.2",
        "ports": [
          {
            "container_port": 80,
            "host_port": 8080,
            "protocol": "tcp"
          }
        ]
      },
      ...
    ],
    "total": 10
  }
}
```

#### Get Container Details

```
GET /containers/{container_id}
```

Gets detailed information about a specific container.

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "container-1",
    "name": "web-server",
    "image": "nginx:latest",
    "status": "running",
    "node_id": "node-1",
    "created_at": "2025-06-01T12:00:00Z",
    "ip_address": "10.244.1.2",
    "ports": [
      {
        "container_port": 80,
        "host_port": 8080,
        "protocol": "tcp"
      }
    ],
    "environment": {
      "ENV_VAR1": "value1",
      "ENV_VAR2": "value2"
    },
    "volumes": [
      {
        "name": "data-volume",
        "mount_path": "/data"
      }
    ],
    "resources": {
      "cpu": 0.5,
      "memory": "256M"
    },
    "health": {
      "status": "healthy",
      "last_check": "2025-06-01T12:05:00Z"
    }
  }
}
```

#### Get Container Logs

```
GET /containers/{container_id}/logs
```

Gets logs from a specific container.

**Query Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `tail` | integer | Number of lines to return from the end |
| `since` | string | Return logs since this timestamp |
| `until` | string | Return logs until this timestamp |
| `follow` | boolean | Stream logs as they are generated |

**Response:**

```json
{
  "success": true,
  "data": {
    "logs": "Log line 1\nLog line 2\nLog line 3\n..."
  }
}
```

#### Get Container Stats

```
GET /containers/{container_id}/stats
```

Gets resource usage statistics for a specific container.

**Response:**

```json
{
  "success": true,
  "data": {
    "cpu_usage": 12.5,
    "memory_usage": 128000000,
    "memory_limit": 268435456,
    "network_rx": 1024,
    "network_tx": 2048,
    "block_read": 4096,
    "block_write": 8192,
    "timestamp": "2025-06-01T12:05:00Z"
  }
}
```

### Applications

#### Deploy Application

```
POST /deploy
```

Deploys a new application.

**Request Body:**

```json
{
  "image": "nginx:latest",
  "name": "web-server",
  "service": "web.local",
  "replicas": 2,
  "resources": {
    "cpu": 0.5,
    "memory": "256M",
    "cpu_limit": 1.0,
    "memory_limit": "512M"
  },
  "env": {
    "ENV_VAR1": "value1",
    "ENV_VAR2": "value2"
  },
  "volumes": [
    {
      "name": "data-volume",
      "mount_path": "/data"
    }
  ],
  "ports": [
    {
      "container_port": 80,
      "host_port": 8080,
      "protocol": "tcp"
    }
  ],
  "health_check": {
    "command": "curl -f http://localhost/",
    "interval": "30s",
    "timeout": "5s",
    "retries": 3,
    "start_period": "60s"
  },
  "node_affinity": "role=frontend",
  "service_affinity": "app=api",
  "priority_class": "high-priority"
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "app_id": "web-server",
    "container_ids": ["container-1", "container-2"],
    "service_id": "web.local",
    "message": "Application deployed successfully"
  }
}
```

**Go Client Example:**

```go
package main

import (
    "context"
    "fmt"
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
            CPU:       0.5,
            Memory:    "256M",
            CPULimit:  1.0,
            MemLimit:  "512M",
        },
        Env: map[string]string{
            "ENV_VAR1": "value1",
            "ENV_VAR2": "value2",
        },
        Volumes: []types.Volume{
            {
                Name:      "data-volume",
                MountPath: "/data",
            },
        },
        Ports: []types.Port{
            {
                ContainerPort: 80,
                HostPort:      8080,
                Protocol:      "tcp",
            },
        },
        HealthCheck: &types.HealthCheck{
            Command:     "curl -f http://localhost/",
            Interval:    "30s",
            Timeout:     "5s",
            Retries:     3,
            StartPeriod: "60s",
        },
        NodeAffinity:    "role=frontend",
        ServiceAffinity: "app=api",
        PriorityClass:   "high-priority",
    }
    
    // Deploy the application
    result, err := c.Deploy(context.Background(), deployment)
    if err != nil {
        log.Fatalf("Deployment failed: %v", err)
    }
    
    fmt.Printf("App ID: %s\n", result.AppID)
    fmt.Printf("Container IDs: %v\n", result.ContainerIDs)
    fmt.Printf("Service ID: %s\n", result.ServiceID)
    fmt.Printf("Message: %s\n", result.Message)
}
```

#### List Applications

```
GET /apps
```

Lists all applications in the cluster.

**Response:**

```json
{
  "success": true,
  "data": {
    "apps": [
      {
        "name": "web-server",
        "image": "nginx:latest",
        "replicas": 2,
        "service": "web.local",
        "created_at": "2025-06-01T12:00:00Z"
      },
      ...
    ]
  }
}
```

#### Get Application Details

```
GET /apps/{app_name}
```

Gets detailed information about a specific application.

**Response:**

```json
{
  "success": true,
  "data": {
    "name": "web-server",
    "image": "nginx:latest",
    "replicas": 2,
    "service": "web.local",
    "created_at": "2025-06-01T12:00:00Z",
    "containers": [
      {
        "id": "container-1",
        "node_id": "node-1",
        "status": "running"
      },
      {
        "id": "container-2",
        "node_id": "node-2",
        "status": "running"
      }
    ],
    "resources": {
      "cpu": 0.5,
      "memory": "256M",
      "cpu_limit": 1.0,
      "memory_limit": "512M"
    },
    "env": {
      "ENV_VAR1": "value1",
      "ENV_VAR2": "value2"
    }
  }
}
```

#### Scale Application

```
POST /scale
```

Scales an application to a specific number of replicas.

**Request Body:**

```json
{
  "name": "web-server",
  "replicas": 5
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "name": "web-server",
    "previous_replicas": 2,
    "new_replicas": 5,
    "message": "Application scaled successfully"
  }
}
```

**Go Client Example:**

```go
package main

import (
    "context"
    "fmt"
    "log"
    
    "github.com/ao/hivemind/pkg/client"
)

func main() {
    // Create a new client
    c, err := client.NewClient("http://localhost:4483")
    if err != nil {
        log.Fatalf("Failed to create client: %v", err)
    }
    
    // Scale the application
    result, err := c.Scale(context.Background(), "web-server", 5)
    if err != nil {
        log.Fatalf("Scaling failed: %v", err)
    }
    
    fmt.Printf("Previous replicas: %d\n", result.PreviousReplicas)
    fmt.Printf("New replicas: %d\n", result.NewReplicas)
    fmt.Printf("Message: %s\n", result.Message)
}
```

#### Restart Application

```
POST /restart
```

Restarts an application.

**Request Body:**

```json
{
  "name": "web-server"
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "name": "web-server",
    "message": "Application restarted successfully"
  }
}
```

#### Delete Application

```
DELETE /apps/{app_name}
```

Deletes an application.

**Response:**

```json
{
  "success": true,
  "data": {
    "name": "web-server",
    "message": "Application deleted successfully"
  }
}
```

### Deployments

#### List Deployments

```
GET /deployments
```

Lists all deployments.

**Query Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `status` | string | Filter by deployment status |
| `app_name` | string | Filter by application name |
| `limit` | integer | Maximum number of deployments to return |
| `offset` | integer | Offset for pagination |

**Response:**

```json
{
  "success": true,
  "data": {
    "deployments": [
      {
        "id": "deployment-1",
        "app_name": "web-server",
        "image": "nginx:latest",
        "replicas": 3,
        "strategy": "blue-green",
        "status": "completed",
        "created_at": "2025-06-01T12:00:00Z",
        "completed_at": "2025-06-01T12:05:00Z"
      },
      ...
    ],
    "total": 10
  }
}
```

#### Create Deployment

```
POST /deployments
```

Creates a new deployment.

**Request Body:**

```json
{
  "app_name": "web-server",
  "image": "nginx:latest",
  "replicas": 3,
  "strategy": {
    "type": "blue-green",
    "verification_timeout": 60
  },
  "environment": {
    "ENV_VAR1": "value1",
    "ENV_VAR2": "value2"
  },
  "labels": {
    "app": "web",
    "environment": "production"
  }
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "deployment-1",
    "app_name": "web-server",
    "message": "Deployment created successfully"
  }
}
```

#### Get Deployment

```
GET /deployments/{id}
```

Gets detailed information about a specific deployment.

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "deployment-1",
    "app_name": "web-server",
    "image": "nginx:latest",
    "replicas": 3,
    "strategy": {
      "type": "blue-green",
      "verification_timeout": 60
    },
    "environment": {
      "ENV_VAR1": "value1",
      "ENV_VAR2": "value2"
    },
    "labels": {
      "app": "web",
      "environment": "production"
    },
    "status": "completed",
    "created_at": "2025-06-01T12:00:00Z",
    "completed_at": "2025-06-01T12:05:00Z"
  }
}
```

#### Execute Deployment

```
POST /deployments/{id}/execute
```

Executes a deployment.

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "deployment-1",
    "message": "Deployment execution started"
  }
}
```

#### Rollback Deployment

```
POST /deployments/{id}/rollback
```

Rolls back a deployment.

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "deployment-1",
    "message": "Deployment rollback started"
  }
}
```

#### Get Deployment Status

```
GET /deployments/{id}/status
```

Gets the status of a deployment.

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "deployment-1",
    "status": "in_progress",
    "current_step": 2,
    "total_steps": 5,
    "current_replicas": 2,
    "target_replicas": 3,
    "message": "Deploying new containers"
  }
}
```

### CI/CD

#### List Pipelines

```
GET /cicd/pipelines
```

Lists all CI/CD pipelines.

**Response:**

```json
{
  "success": true,
  "data": {
    "pipelines": [
      {
        "id": "pipeline-1",
        "name": "web-pipeline",
        "repository_url": "https://github.com/user/repo",
        "branch": "main",
        "created_at": "2025-06-01T12:00:00Z",
        "updated_at": "2025-06-01T12:05:00Z"
      },
      ...
    ]
  }
}
```

#### Create Pipeline

```
POST /cicd/pipelines
```

Creates a new CI/CD pipeline.

**Request Body:**

```json
{
  "name": "web-pipeline",
  "repository_url": "https://github.com/user/repo",
  "branch": "main",
  "build_config": {
    "build_command": "npm ci && npm run build",
    "docker_image": "node:16",
    "build_args": {
      "NODE_ENV": "production"
    }
  },
  "deployment_config": {
    "strategy": {
      "type": "blue-green",
      "verification_timeout": 60
    },
    "environment": "production",
    "replicas": 3
  },
  "test_config": {
    "test_command": "npm test",
    "reports_path": "reports",
    "timeout_seconds": 300
  },
  "webhook_url": "https://example.com/webhook"
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "pipeline-1",
    "name": "web-pipeline",
    "message": "Pipeline created successfully"
  }
}
```

#### Get Pipeline

```
GET /cicd/pipelines/{id}
```

Gets detailed information about a specific pipeline.

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "pipeline-1",
    "name": "web-pipeline",
    "repository_url": "https://github.com/user/repo",
    "branch": "main",
    "build_config": {
      "build_command": "npm ci && npm run build",
      "docker_image": "node:16",
      "build_args": {
        "NODE_ENV": "production"
      }
    },
    "deployment_config": {
      "strategy": {
        "type": "blue-green",
        "verification_timeout": 60
      },
      "environment": "production",
      "replicas": 3
    },
    "test_config": {
      "test_command": "npm test",
      "reports_path": "reports",
      "timeout_seconds": 300
    },
    "webhook_url": "https://example.com/webhook",
    "created_at": "2025-06-01T12:00:00Z",
    "updated_at": "2025-06-01T12:05:00Z"
  }
}
```

#### Trigger Pipeline

```
POST /cicd/pipelines/{id}/trigger
```

Triggers a pipeline run.

**Response:**

```json
{
  "success": true,
  "data": {
    "pipeline_id": "pipeline-1",
    "run_id": "run-1",
    "message": "Pipeline triggered successfully"
  }
}
```

#### List Pipeline Runs

```
GET /cicd/pipelines/{id}/runs
```

Lists all runs for a specific pipeline.

**Response:**

```json
{
  "success": true,
  "data": {
    "runs": [
      {
        "id": "run-1",
        "pipeline_id": "pipeline-1",
        "commit_hash": "abc123",
        "commit_message": "Fix bug",
        "author": "John Doe",
        "status": "success",
        "start_time": "2025-06-01T12:00:00Z",
        "end_time": "2025-06-01T12:05:00Z",
        "duration_seconds": 300
      },
      ...
    ]
  }
}
```

### Cloud

#### List Cloud Providers

```
GET /cloud/providers
```

Lists all configured cloud providers.

**Response:**

```json
{
  "success": true,
  "data": {
    "providers": [
      {
        "name": "AWS",
        "provider_type": "aws",
        "region": "us-west-2"
      },
      {
        "name": "Azure",
        "provider_type": "azure",
        "region": "eastus"
      },
      {
        "name": "GCP",
        "provider_type": "gcp",
        "region": "us-central1"
      }
    ]
  }
}
```

#### List Cloud Instances

```
GET /cloud/instances
```

Lists all cloud instances.

**Query Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `provider` | string | Filter by provider |
| `region` | string | Filter by region |
| `state` | string | Filter by instance state |

**Response:**

```json
{
  "success": true,
  "data": {
    "instances": [
      {
        "id": "i-12345",
        "name": "web-server",
        "provider": "aws",
        "region": "us-west-2",
        "zone": "us-west-2a",
        "instance_type": "t3.micro",
        "public_ip": "54.123.45.67",
        "private_ip": "10.0.1.2",
        "state": "running",
        "created_at": "2025-06-01T12:00:00Z"
      },
      ...
    ]
  }
}
```

#### Create Cloud Instance

```
POST /cloud/instances
```

Creates a new cloud instance.

**Request Body:**

```json
{
  "name": "web-server",
  "provider": "aws",
  "region": "us-west-2",
  "zone": "us-west-2a",
  "instance_type": "t3.micro",
  "disk_size_gb": 20,
  "tags": {
    "Environment": "Production",
    "Project": "Website"
  }
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "i-12345",
    "name": "web-server",
    "provider": "aws",
    "region": "us-west-2",
    "zone": "us-west-2a",
    "instance_type": "t3.micro",
    "public_ip": "54.123.45.67",
    "private_ip": "10.0.1.2",
    "state": "pending",
    "created_at": "2025-06-01T12:00:00Z"
  }
}
```

### Helm

#### List Helm Repositories

```
GET /helm/repositories
```

Lists all Helm repositories.

**Response:**

```json
{
  "success": true,
  "data": {
    "repositories": [
      {
        "name": "stable",
        "url": "https://charts.helm.sh/stable",
        "is_oci": false
      },
      {
        "name": "bitnami",
        "url": "https://charts.bitnami.com/bitnami",
        "is_oci": false
      }
    ]
  }
}
```

#### Add Helm Repository

```
POST /helm/repositories
```

Adds a new Helm repository.

**Request Body:**

```json
{
  "name": "bitnami",
  "url": "https://charts.bitnami.com/bitnami",
  "username": "user",
  "password": "password",
  "is_oci": false
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "name": "bitnami",
    "url": "https://charts.bitnami.com/bitnami",
    "is_oci": false,
    "message": "Repository added successfully"
  }
}
```

#### List Helm Releases

```
GET /helm/releases
```

Lists all Helm releases.

**Query Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `namespace` | string | Filter by namespace |
| `status` | string | Filter by release status |

**Response:**

```json
{
  "success": true,
  "data": {
    "releases": [
      {
        "name": "my-release",
        "namespace": "default",
        "chart": "bitnami/nginx",
        "version": "9.5.0",
        "status": "deployed",
        "revision": 1,
        "updated": "2025-06-01T12:00:00Z"
      },
      ...
    ]
  }
}
```

#### Install Helm Chart

```
POST /helm/releases
```

Installs a new Helm chart.

**Request Body:**

```json
{
  "name": "my-release",
  "chart": "bitnami/nginx",
  "namespace": "default",
  "values": {
    "replicaCount": 3,
    "service": {
      "type": "LoadBalancer"
    }
  }
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "name": "my-release",
    "namespace": "default",
    "chart": "bitnami/nginx",
    "version": "9.5.0",
    "message": "Chart installed successfully"
  }
}
```

### Observability

#### Get Metrics

```
GET /metrics
```

Gets metrics in Prometheus format.

**Response:**

```
# HELP hivemind_node_count Number of nodes in the cluster
# TYPE hivemind_node_count gauge
hivemind_node_count 3 1622548800000

# HELP hivemind_container_count Number of containers managed by Hivemind
# TYPE hivemind_container_count gauge
hivemind_container_count 10 1622548800000

# HELP hivemind_container_status_count Number of containers by status
# TYPE hivemind_container_status_count gauge
hivemind_container_status_count{status="running"} 8 1622548800000
hivemind_container_status_count{status="stopped"} 2 1622548800000
```

#### Configure Metrics

```
POST /observability/metrics/configure
```

Configures metrics collection.

**Request Body:**

```json
{
  "port": 9090,
  "path": "/metrics",
  "collectors": ["node", "container", "health", "network", "scheduler"]
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "port": 9090,
    "path": "/metrics",
    "collectors": ["node", "container", "health", "network", "scheduler"],
    "message": "Metrics configured successfully"
  }
}
```

#### Configure Tracing

```
POST /observability/tracing/configure
```

Configures distributed tracing.

**Request Body:**

```json
{
  "service_name": "hivemind",
  "endpoint": "http://jaeger:14268/api/traces",
  "sample_rate": 0.1
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "service_name": "hivemind",
    "endpoint": "http://jaeger:14268/api/traces",
    "sample_rate": 0.1,
    "message": "Tracing configured successfully"
  }
}
```

#### Configure Logging

```
POST /observability/logging/configure
```

Configures log aggregation.

**Request Body:**

```json
{
  "endpoint": "http://elasticsearch:9200",
  "index": "hivemind-logs",
  "level": "info"
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "endpoint": "http://elasticsearch:9200",
    "index": "hivemind-logs",
    "level": "info",
    "message": "Logging configured successfully"
  }
}
```

### Services

#### List Services

```
GET /services
```

Lists all services in the cluster.

**Response:**

```json
{
  "success": true,
  "data": {
    "services": [
      {
        "name": "web.local",
        "domain": "web.local",
        "endpoints": [
          {
            "container_id": "container-1",
            "ip_address": "10.244.1.2",
            "port": 80
          },
          {
            "container_id": "container-2",
            "ip_address": "10.244.2.2",
            "port": 80
          }
        ],
        "load_balancing_strategy": "round-robin"
      },
      ...
    ]
  }
}
```

#### Get Service Details

```
GET /services/{service_name}
```

Gets detailed information about a specific service.

**Response:**

```json
{
  "success": true,
  "data": {
    "name": "web.local",
    "domain": "web.local",
    "endpoints": [
      {
        "container_id": "container-1",
        "ip_address": "10.244.1.2",
        "port": 80,
        "node_id": "node-1",
        "health": "healthy"
      },
      {
        "container_id": "container-2",
        "ip_address": "10.244.2.2",
        "port": 80,
        "node_id": "node-2",
        "health": "healthy"
      }
    ],
    "load_balancing_strategy": "round-robin",
    "health_check": {
      "protocol": "http",
      "path": "/health",
      "interval": "30s",
      "timeout": "5s",
      "healthy_threshold": 2,
      "unhealthy_threshold": 3
    }
  }
}
```

#### Update Service

```
PUT /services/{service_name}
```

Updates a service configuration.

**Request Body:**

```json
{
  "load_balancing_strategy": "least-connections",
  "health_check": {
    "protocol": "http",
    "path": "/health",
    "interval": "15s",
    "timeout": "3s",
    "healthy_threshold": 2,
    "unhealthy_threshold": 3
  }
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "name": "web.local",
    "message": "Service updated successfully"
  }
}
```

### Volumes

#### List Volumes

```
GET /volumes
```

Lists all volumes in the cluster.

**Response:**

```json
{
  "success": true,
  "data": {
    "volumes": [
      {
        "name": "data-volume",
        "size": "10G",
        "type": "local",
        "created_at": "2025-06-01T12:00:00Z",
        "used_by": ["container-1"]
      },
      ...
    ]
  }
}
```

#### Create Volume

```
POST /volumes/create
```

Creates a new volume.

**Request Body:**

```json
{
  "name": "data-volume",
  "size": "10G",
  "type": "local",
  "labels": {
    "app": "db",
    "env": "prod"
  }
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "name": "data-volume",
    "message": "Volume created successfully"
  }
}
```

#### Delete Volume

```
POST /volumes/delete
```

Deletes a volume.

**Request Body:**

```json
{
  "name": "data-volume",
  "force": false
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "name": "data-volume",
    "message": "Volume deleted successfully"
  }
}
```

### Nodes

#### List Nodes

```
GET /nodes
```

Lists all nodes in the cluster.

**Response:**

```json
{
  "success": true,
  "data": {
    "nodes": [
      {
        "id": "node-1",
        "address": "192.168.1.101",
        "status": "ready",
        "role": "worker",
        "resources": {
          "cpu_total": 4,
          "cpu_available": 2.5,
          "memory_total": 8589934592,
          "memory_available": 4294967296,
          "disk_total": 107374182400,
          "disk_available": 53687091200
        },
        "containers": 5
      },
      ...
    ]
  }
}
```

#### Get Node Details

```
GET /nodes/{node_id}
```

Gets detailed information about a specific node.

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "node-1",
    "address": "192.168.1.101",
    "status": "ready",
    "role": "worker",
    "resources": {
      "cpu_total": 4,
      "cpu_available": 2.5,
      "memory_total": 8589934592,
      "memory_available": 4294967296,
      "disk_total": 107374182400,
      "disk_available": 53687091200
    },
    "containers": [
      {
        "id": "container-1",
        "name": "web-server",
        "status": "running"
      },
      ...
    ],
    "labels": {
      "role": "frontend",
      "zone": "zone1"
    },
    "taints": [
      {
        "key": "special",
        "value": "true",
        "effect": "NoSchedule"
      }
    ],
    "network": {
      "subnet": "10.244.1.0/24",
      "overlay_status": "connected"
    }
  }
}
```

#### Put Node in Maintenance Mode

```
POST /nodes/{node_id}/maintenance
```

Puts a node into maintenance mode.

**Request Body:**

```json
{
  "enable": true,
  "drain": true
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "node-1",
    "message": "Node put into maintenance mode"
  }
}
```

### Health

#### Get System Health

```
GET /health
```

Gets the overall health of the system.

**Response:**

```json
{
  "success": true,
  "data": {
    "status": "healthy",
    "components": {
      "nodes": {
        "status": "healthy",
        "total": 3,
        "healthy": 3,
        "unhealthy": 0
      },
      "containers": {
        "status": "healthy",
        "total": 10,
        "healthy": 10,
        "unhealthy": 0
      },
      "services": {
        "status": "healthy",
        "total": 5,
        "healthy": 5,
        "unhealthy": 0
      }
    },
    "alerts": {
      "critical": 0,
      "warning": 0,
      "info": 0
    }
  }
}
```

#### Get Container Health

```
GET /health/container/{container_id}
```

Gets the health status of a specific container.

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "container-1",
    "status": "healthy",
    "last_check": "2025-06-01T12:05:00Z",
    "consecutive_failures": 0,
    "restart_count": 0,
    "health_history": [
      {
        "timestamp": "2025-06-01T12:05:00Z",
        "status": "healthy",
        "message": null
      },
      {
        "timestamp": "2025-06-01T12:04:30Z",
        "status": "healthy",
        "message": null
      },
      ...
    ]
  }
}
```

#### Get Node Health

```
GET /health/node/{node_id}
```

Gets the health status of a specific node.

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "node-1",
    "status": "healthy",
    "last_seen": "2025-06-01T12:05:00Z",
    "cpu_usage": 35.2,
    "memory_usage": 42.8,
    "disk_usage": 50.0,
    "network_status": "connected",
    "container_count": 5,
    "health_history": [
      {
        "timestamp": "2025-06-01T12:05:00Z",
        "cpu_usage": 35.2,
        "memory_usage": 42.8,
        "disk_usage": 50.0,
        "network_status": "connected",
        "message": null
      },
      ...
    ]
  }
}
```

### Security

#### Scan Container Image

```
POST /security/scan
```

Scans a container image for vulnerabilities.

**Request Body:**

```json
{
  "image": "nginx:latest"
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "image": "nginx:latest",
    "scan_time": "2025-06-01T12:05:00Z",
    "vulnerabilities": [
      {
        "id": "CVE-2023-1234",
        "name": "Sample Vulnerability",
        "description": "This is a sample vulnerability",
        "severity": "Medium",
        "affected_package": "openssl",
        "affected_version": "1.1.1",
        "fixed_version": "1.1.1g"
      },
      ...
    ],
    "compliant": true,
    "scan_status": "Completed"
  }
}
```

#### List Security Policies

```
GET /security/policies
```

Lists all security policies.

**Response:**

```json
{
  "success": true,
  "data": {
    "policies": [
      {
        "id": "default",
        "name": "Default Security Policy",
        "description": "Default security policy for all containers",
        "max_severity": "Medium",
        "block_on_severity": "High",
        "allowed_registries": ["docker.io", "gcr.io", "quay.io"]
      },
      ...
    ]
  }
}
```

#### List Network Policies

```
GET /security/network-policies
```

Lists all network policies.

**Response:**

```json
{
  "success": true,
  "data": {
    "policies": [
      {
        "name": "web-to-db",
        "selector": {
          "labels": {"app": "web"}
        },
        "ingress_rules": [
          {
            "ports": [{"protocol": "TCP", "port_min": 80, "port_max": 80}],
            "from": [{"ip_block": "10.244.2.0/24"}]
          }
        ],
        "egress_rules": [
          {
            "ports": [{"protocol": "TCP", "port_min": 5432, "port_max": 5432}],
            "from": [{"selector": {"labels": {"app": "db"}}}]
          }
        ],
        "encryption_required": false,
        "traffic_logging": true
      },
      ...
    ]
  }
}
```

#### List Secrets

```
GET /security/secrets
```

Lists all secrets (metadata only, not values).

**Response:**

```json
{
  "success": true,
  "data": {
    "secrets": [
      {
        "id": "db-password",
        "name": "db-password",
        "description": "Database password",
        "version": 1,
        "created_at": "2025-06-01T12:00:00Z",
        "created_by": "admin",
        "last_accessed": "2025-06-01T12:05:00Z"
      },
      ...
    ]
  }
}
```

#### Create Secret

```
POST /security/secrets/create
```

Creates a new secret.

**Request Body:**

```json
{
  "name": "db-password",
  "description": "Database password",
  "value": "my-secure-password",
  "labels": {
    "app": "db",
    "env": "prod"
  }
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "db-password",
    "name": "db-password",
    "message": "Secret created successfully"
  }
}
```

### Tenants

#### List Tenants

```
GET /tenants
```

Lists all tenants the authenticated user has access to.

**Query Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `status` | string | Filter by tenant status (active, suspended, etc.) |
| `limit` | integer | Maximum number of tenants to return |
| `offset` | integer | Offset for pagination |

**Response:**

```json
{
  "success": true,
  "data": {
    "tenants": [
      {
        "id": "tenant-1",
        "name": "Engineering",
        "isolation_level": "soft",
        "namespaces": ["engineering", "dev"],
        "created_at": "2025-06-01T12:00:00Z",
        "owner_id": "user-1",
        "status": "active"
      },
      {
        "id": "tenant-2",
        "name": "Marketing",
        "isolation_level": "network_only",
        "namespaces": ["marketing"],
        "created_at": "2025-06-01T14:00:00Z",
        "owner_id": "user-2",
        "status": "active"
      }
    ],
    "total": 2
  }
}
```

#### Get Tenant Details

```
GET /tenants/{tenant_id}
```

Gets detailed information about a specific tenant.

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "tenant-1",
    "name": "Engineering",
    "isolation_level": "soft",
    "resource_quotas": {
      "cpu_limit": 8,
      "memory_limit": 16000000000,
      "storage_limit": 100000000000,
      "max_containers": 20,
      "max_services": 10
    },
    "namespaces": ["engineering", "dev"],
    "created_at": "2025-06-01T12:00:00Z",
    "updated_at": "2025-06-01T15:00:00Z",
    "owner_id": "user-1",
    "description": "Engineering department tenant",
    "labels": {
      "department": "engineering",
      "cost_center": "cc-123"
    },
    "status": "active"
  }
}
```

#### Create Tenant

```
POST /tenants
```

Creates a new tenant.

**Request Body:**

```json
{
  "name": "Finance",
  "isolation_level": "hard",
  "resource_quotas": {
    "cpu_limit": 4,
    "memory_limit": 8000000000,
    "storage_limit": 50000000000,
    "max_containers": 10,
    "max_services": 5
  },
  "namespaces": ["finance"],
  "description": "Finance department tenant",
  "labels": {
    "department": "finance",
    "cost_center": "cc-456"
  }
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "tenant-3",
    "name": "Finance",
    "isolation_level": "hard",
    "created_at": "2025-06-02T10:00:00Z",
    "owner_id": "user-1",
    "status": "active",
    "message": "Tenant created successfully"
  }
}
```

#### Update Tenant

```
PUT /tenants/{tenant_id}
```

Updates an existing tenant.

**Request Body:**

```json
{
  "name": "Finance Department",
  "isolation_level": "soft",
  "resource_quotas": {
    "cpu_limit": 8,
    "memory_limit": 16000000000,
    "storage_limit": 100000000000,
    "max_containers": 20,
    "max_services": 10
  },
  "description": "Updated finance department tenant",
  "labels": {
    "department": "finance",
    "cost_center": "cc-789"
  }
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "tenant-3",
    "name": "Finance Department",
    "isolation_level": "soft",
    "updated_at": "2025-06-02T11:00:00Z",
    "message": "Tenant updated successfully"
  }
}
```

#### Delete Tenant

```
DELETE /tenants/{tenant_id}
```

Deletes a tenant.

**Response:**

```json
{
  "success": true,
  "data": {
    "id": "tenant-3",
    "message": "Tenant deleted successfully"
  }
}
```

#### Add Namespace to Tenant

```
POST /tenants/{tenant_id}/namespaces
```

Adds a namespace to a tenant.

**Request Body:**

```json
{
  "namespace": "finance-dev"
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "tenant_id": "tenant-3",
    "namespace": "finance-dev",
    "message": "Namespace added successfully"
  }
}
```

#### Remove Namespace from Tenant

```
DELETE /tenants/{tenant_id}/namespaces/{namespace}
```

Removes a namespace from a tenant.

**Response:**

```json
{
  "success": true,
  "data": {
    "tenant_id": "tenant-3",
    "namespace": "finance-dev",
    "message": "Namespace removed successfully"
  }
}
```

#### Update Tenant Resource Quotas

```
PUT /tenants/{tenant_id}/quotas
```

Updates resource quotas for a tenant.

**Request Body:**

```json
{
  "cpu_limit": 16,
  "memory_limit": 32000000000,
  "storage_limit": 200000000000,
  "max_containers": 40,
  "max_services": 20
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "tenant_id": "tenant-3",
    "message": "Resource quotas updated successfully"
  }
}
```

#### Get Tenant Resource Usage

```
GET /tenants/{tenant_id}/usage
```

Gets resource usage for a tenant.

**Response:**

```json
{
  "success": true,
  "data": {
    "tenant_id": "tenant-3",
    "cpu_usage": 2.5,
    "memory_usage": 4000000000,
    "storage_usage": 20000000000,
    "container_count": 5,
    "service_count": 2,
    "timestamp": "2025-06-02T12:00:00Z"
  }
}
```

### Authentication

#### Login

```
POST /auth/login
```

Authenticates a user and returns an API token.

**Request Body:**

```json
{
  "username": "admin",
  "password": "password"
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
    "user_id": "admin",
    "username": "admin",
    "expires_at": "2025-06-02T12:00:00Z"
  }
}
```

#### Logout

```
POST /auth/logout
```

Invalidates the current API token.

**Response:**

```json
{
  "success": true,
  "data": {
    "message": "Logged out successfully"
  }
}
```

## Websocket API

In addition to the REST API, Hivemind also provides a WebSocket API for real-time updates.

### Connect to WebSocket

```
ws://<your-server>:4483/api/ws
```

Include your API token in the query string:

```
ws://<your-server>:4483/api/ws?token=<your-api-token>
```

### Event Types

The WebSocket API sends events in JSON format:

```json
{
  "type": "event_type",
  "data": { ... }
}
```

Available event types:

- `container_created`: A new container was created
- `container_started`: A container was started
- `container_stopped`: A container was stopped
- `container_deleted`: A container was deleted
- `container_health_changed`: A container's health status changed
- `node_joined`: A new node joined the cluster
- `node_left`: A node left the cluster
- `node_health_changed`: A node's health status changed
- `service_updated`: A service was updated
- `alert_created`: A new alert was created
- `alert_resolved`: An alert was resolved

### Example Event

```json
{
  "type": "container_health_changed",
  "data": {
    "container_id": "container-1",
    "previous_status": "healthy",
    "new_status": "unhealthy",
    "timestamp": "2025-06-01T12:05:00Z"
  }
}
```

## Error Codes

| Code | Description |
|------|-------------|
| `invalid_request` | The request was invalid |
| `authentication_failed` | Authentication failed |
| `permission_denied` | The authenticated user doesn't have permission |
| `resource_not_found` | The requested resource was not found |
| `resource_conflict` | The request conflicts with the current state |
| `internal_error` | An internal server error occurred |

## Rate Limiting

The API is rate limited to prevent abuse. The rate limits are:

- 100 requests per minute per IP address
- 1000 requests per hour per user

When rate limited, the API returns a `429 Too Many Requests` status code with the following response:

```json
{
  "success": false,
  "error": {
    "code": "rate_limit_exceeded",
    "message": "Rate limit exceeded. Try again in X seconds."
  }
}
```

The response includes `X-RateLimit-Limit`, `X-RateLimit-Remaining`, and `X-RateLimit-Reset` headers.

## Pagination

List endpoints support pagination using the `limit` and `offset` query parameters:

```
GET /containers?limit=10&offset=20
```

The response includes pagination metadata:

```json
{
  "success": true,
  "data": {
    "containers": [ ... ],
    "pagination": {
      "total": 100,
      "limit": 10,
      "offset": 20,
      "next_offset": 30,
      "prev_offset": 10
    }
  }
}
```

## Filtering

List endpoints support filtering using query parameters:

```
GET /containers?status=running&node=node-1
```

## Sorting

List endpoints support sorting using the `sort` and `order` query parameters:

```
GET /containers?sort=created_at&order=desc
```

## Field Selection

You can specify which fields to include in the response using the `fields` query parameter:

```
GET /containers?fields=id,name,status
```

## Versioning

The API is versioned using the `Accept` header:

```
Accept: application/json; version=1.0
```

If no version is specified, the latest version is used.

## Advanced API Features

### Batch Operations

Hivemind supports batch operations for improved efficiency:

```
POST /batch
```

**Request Body:**

```json
{
  "operations": [
    {
      "method": "POST",
      "path": "/deploy",
      "body": {
        "image": "nginx:latest",
        "name": "web-server"
      }
    },
    {
      "method": "POST",
      "path": "/volumes/create",
      "body": {
        "name": "data-volume"
      }
    }
  ]
}
```

**Response:**

```json
{
  "success": true,
  "data": {
    "results": [
      {
        "success": true,
        "data": {
          "app_id": "web-server",
          "container_ids": ["container-1"],
          "message": "Application deployed successfully"
        }
      },
      {
        "success": true,
        "data": {
          "name": "data-volume",
          "message": "Volume created successfully"
        }
      }
    ]
  }
}
```

### Streaming API

For real-time updates, Hivemind provides a streaming API using Server-Sent Events (SSE):

```
GET /stream/events
```

**Query Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `types` | string | Comma-separated list of event types to subscribe to |
| `resources` | string | Comma-separated list of resources to monitor |

**Response:**

```
event: container.created
data: {"id":"container-1","name":"web-server","status":"created"}

event: container.started
data: {"id":"container-1","name":"web-server","status":"running"}
```

### GraphQL API

Hivemind provides a GraphQL API for flexible querying:

```
POST /graphql
```

**Request Body:**

```json
{
  "query": "query { containers(status: \"running\") { id name image status node { id name } metrics { cpu_usage memory_usage } } }"
}
```

**Response:**

```json
{
  "data": {
    "containers": [
      {
        "id": "container-1",
        "name": "web-server",
        "image": "nginx:latest",
        "status": "running",
        "node": {
          "id": "node-1",
          "name": "worker-1"
        },
        "metrics": {
          "cpu_usage": 0.5,
          "memory_usage": 128000000
        }
      }
    ]
  }
}
```

## Conclusion

This API reference covers the main endpoints and features of the Hivemind API. For more detailed information on specific endpoints or features, refer to the following resources:

- [User Guide](user_guide.md)
- [Administration Guide](administration_guide.md)
- [Troubleshooting Guide](troubleshooting_guide.md)
- [Developer Guide](../developer_guide.md)
- [Deployment Guide](deployment_guide.md)