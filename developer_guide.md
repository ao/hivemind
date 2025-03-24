# Hivemind Developer Guide

This guide provides information for developers working with the Hivemind platform, including how to deploy applications and understand the container networking system.

## Deployment Options

A developer has three main ways to deploy their Docker image to Hivemind:

1. **Command Line Interface (CLI)**
2. **Web UI**
3. **REST API**

## Prerequisites

- The Docker image is already built and pushed to a registry (Docker Hub, ECR, etc.)
- Hivemind daemon is running on the server

## Option 1: Command Line Interface (CLI)

The simplest way to deploy using the CLI:

```bash
hivemind app deploy --image your-registry/your-image:tag --name your-app-name
```

For a public-facing service with a domain:

```bash
hivemind app deploy --image your-registry/your-image:tag --name your-app-name --service your-service-domain
```

### CLI Examples

1. **Deploy a simple application**:
   ```bash
   hivemind app deploy --image myapp:latest --name myapp
   ```

2. **Deploy with a service domain** (for routing/discovery):
   ```bash
   hivemind app deploy --image myapp:latest --name myapp --service app.example.com
   ```

3. **Check deployed containers**:
   ```bash
   hivemind app containers
   ```

4. **Get detailed container info**:
   ```bash
   hivemind app container-info --container-id <container-id>
   ```

5. **Restart an application**:
   ```bash
   hivemind app restart --name myapp
   ```

6. **Scale an application**:
   ```bash
   hivemind app scale --name myapp --replicas 3
   ```

## Option 2: Web UI

Hivemind provides a web interface accessible at port 3000 by default:

1. Navigate to `http://<your-server>:3000`
2. Click on the "Deploy" link in the navigation bar
3. Fill in the form:
   - Name: Give your application a name
   - Image: Select or enter your Docker image (in format `repository/image:tag`)
   - Service Domain (optional): Enter a domain name if you want the application exposed
4. Click "Deploy" button

## Option 3: REST API

For automated or programmatic deployments, you can use the REST API:

```bash
curl -X POST http://<your-server>:3000/api/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "image": "your-registry/your-image:tag",
    "name": "your-app-name",
    "service": "optional-domain-name"
  }'
```

Response:
```json
{
  "success": true,
  "container_id": "your-app-name",
  "message": "Deployed application your-app-name with container your-app-name"
}
```

## Working with Volumes

If your application needs persistent storage, you can create and use volumes:

1. **Create a volume** (via API):
   ```bash
   curl -X POST http://<your-server>:3000/api/volumes/create \
     -H "Content-Type: application/json" \
     -d '{"name": "my-data-volume"}'
   ```

2. **Deploy with volumes**:
   Unfortunately, the CLI and API don't directly expose volume mounts, but you can use the web UI to deploy applications with volumes or modify the code to add this capability.

## How It Works Under the Hood

When you deploy an image:

1. Hivemind checks if it can connect to containerd (the container runtime)
2. If available, it uses containerd to pull and start your container
3. If not, it falls back to a mock implementation
4. It registers your container in its internal database
5. If you specified a service domain, it registers the service with the service discovery system
6. Containers are monitored for health and status changes

The connection between Hivemind and your container images happens in the `ContainerdManager` which:
- Pulls images from registries
- Creates and manages containers
- Maps ports and volumes
- Monitors container status

## Limitations and Notes

1. The current implementation has a mock `pull_image` function - in production, this would actually pull from your registry
2. The system supports common container configuration:
   - Environment variables
   - Port mappings
   - Volume mounts
3. The service discovery allows public access to your containers
4. The platform supports scaling, restarting, and monitoring containers
5. Container logs and metrics are tracked for monitoring

## Advanced Features

Hivemind also provides these additional capabilities:

1. **Scaling**: Adjust the number of container replicas
   ```bash
   hivemind app scale --name your-app-name --replicas 3
   ```

2. **Health Checks**: Monitor application health
   ```bash
   hivemind health
   ```

3. **Service Discovery**: Automatic registration and DNS for services

4. **Multi-node Clusters**: Join nodes to create a distributed cluster
   ```bash
   hivemind join --host <any-node-address>
   ```

5. **Container Networking**: Seamless communication between containers across nodes

By following these instructions, a developer can successfully deploy their Docker images to the Hivemind platform and manage them effectively.

## Container Networking

Hivemind includes a robust container networking system that enables containers to communicate across nodes in a cluster. For detailed information, see the [Container Networking Documentation](docs/container_networking.md).

### Key Features

- **Automatic IP Address Management**: Each container gets a unique IP address
- **Overlay Network**: Containers can communicate across nodes transparently
- **Network Policies**: Control traffic flow between containers for security
- **Service Discovery Integration**: Find services by name rather than IP address

### How Container Networking Works

When you deploy a container:

1. The container is assigned an IP address from the node's subnet
2. A virtual network interface is created in the container
3. The container is connected to the node's bridge
4. The bridge is connected to an overlay network that spans all nodes
5. Containers can communicate with each other using their assigned IPs

### Checking Network Health

You can check the status of the container networking system using:

```bash
hivemind health
```

This will show information about:
- Nodes in the network
- Overlay tunnels between nodes
- Network policies

### Network Troubleshooting

If containers are having trouble communicating:

1. Check that the containers have valid IP addresses
2. Verify that the overlay network is functioning
3. Check for network policies that might be blocking traffic
4. Ensure that the nodes can reach each other on the VXLAN port (default: 4789)

For more detailed troubleshooting, refer to the [Container Networking Documentation](docs/container_networking.md).
