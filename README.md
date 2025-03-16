<div align="center">
<img src="./assets/logo1.png" width="100px">
<h1>Hivemind</h1>
<p><strong>A lightweight, distributed container orchestration platform built in Rust</strong></p>
</div>

## 🚀 What is Hivemind?

Hivemind is a modern, lightweight container orchestration system designed with simplicity and performance in mind. Think of it as a Kubernetes alternative that's easier to set up, understand, and operate - perfect for smaller deployments, edge computing, or when you need a container platform without the complexity.

## ✨ Key Features

- **🔄 Simple yet powerful** - Deploy containers with a clean REST API or straightforward CLI
- **⚡ Blazing fast** - Built in Rust for minimal resource usage and maximum performance
- **📦 Youki integration** - Works directly with youki for reliable container operations
- **🔍 Service Discovery** - Automatic DNS-based service discovery for your applications
- **🌐 Clustering** - Seamlessly scale from a single node to a distributed cluster
- **🔒 Volume Management** - Persistent storage for your stateful applications
- **🖥️ Clean Web UI** - Monitor and manage everything through an intuitive dashboard

## 🔧 Quick Start

### Install Hivemind

```bash
cargo install hivemind
```

### Start the daemon

```bash
hivemind daemon --web-port 3000
```

### Deploy your first application

```bash
hivemind app deploy --image nginx:latest --name my-web-app --service web.local
```

Visit `http://localhost:3000` to see your application in the Hivemind dashboard!

## 📋 Command Reference

Hivemind offers a comprehensive CLI for all operations:

### Global Options

```bash
hivemind --data-dir <PATH>  # Set the data directory (default: ~/.hivemind)
```

### Daemon Mode

```bash
# Start the Hivemind daemon
hivemind daemon --web-port <PORT>  # Start the server (default port: 3000)

# Start only the web interface (useful for development)
hivemind web --port <PORT>  # Start only the web UI (default port: 3000)
```

### Cluster Management

```bash
# Join an existing Hivemind cluster
hivemind join --host <HOST_ADDRESS>  # Connect to an existing cluster

# List all nodes in the cluster
hivemind node ls

# Show detailed node information
hivemind node info
```

### Application Management

```bash
# List all applications
hivemind app ls

# Deploy a new application
hivemind app deploy --image <IMAGE> --name <NAME> [--service <DOMAIN>]

# Scale an application to a specific number of replicas
hivemind app scale --name <NAME> --replicas <COUNT>

# List all containers
hivemind app containers

# Show detailed container information
hivemind app container-info --container-id <CONTAINER_ID>

# Restart an application
hivemind app restart --name <NAME>
```

### System Health

```bash
# Check system health
hivemind health  # Shows health status of nodes, containers, and services
```

### Volume Management

```bash
# Create a new volume
hivemind volume create --name <VOLUME_NAME>

# List all volumes
hivemind volume ls

# Delete a volume
hivemind volume delete --name <VOLUME_NAME>

# Deploy with volume mounts
hivemind app deploy --image <IMAGE> --name <NAME> --volume <VOLUME_NAME>:<CONTAINER_PATH>
```

## 🔌 API Reference

Hivemind offers a RESTful API for all operations:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/nodes` | GET | List all nodes |
| `/api/containers` | GET | List all containers |
| `/api/images` | GET | List available images |
| `/api/services` | GET | List all services |
| `/api/service-endpoints` | GET | List all service endpoints |
| `/api/health` | GET | Get system health metrics |
| `/api/deploy` | POST | Deploy a new container |
| `/api/scale` | POST | Scale an application |
| `/api/restart` | POST | Restart an application |
| `/api/service-url` | POST | Get URL for a service |
| `/api/volumes` | GET | List all volumes |
| `/api/volumes/create` | POST | Create a new volume |
| `/api/volumes/delete` | POST | Delete a volume |

## 🌟 Why Hivemind?

### For Users
- **Simple to learn** - No steep learning curve or complex YAML files
- **Resource-efficient** - Runs well even on lower-powered hardware
- **Predictable** - Designed to be stable and behave consistently
- **Self-contained** - Minimal dependencies means fewer things to break

### For Developers
- **Clean codebase** - Well-structured Rust code that's a joy to work with
- **Modular architecture** - Easy to extend with new features
- **API-first design** - Build tools and integrations with our comprehensive API
- **Fast compile-test cycle** - Quick iteration for development

## 📊 Comparison with other platforms

| Feature | Hivemind | Kubernetes | Docker Swarm |
|---------|----------|------------|---------------|
| Startup time | ⚡ Seconds | ⏱️ Minutes | ⏱️ Minutes |
| Memory usage | 🍃 ~50MB | 🏋️ ~500MB+ | 🏋️ ~200MB+ |
| Learning curve | 📘 Low | 📚 High | 📗 Medium |
| Scaling | ✅ Yes | ✅ Yes | ✅ Yes |
| Auto-healing | ✅ Yes | ✅ Yes | ✅ Yes |
| Cloud native | ✅ Yes | ✅ Yes | ⚠️ Partial |

## 🧩 Architecture

Hivemind follows a clean, modular architecture:

- **App Manager** - Application and container lifecycle management
- **Node Manager** - Cluster coordination and node discovery
- **Service Discovery** - DNS-based service discovery and routing
- **Storage Manager** - Volume and persistence handling
- **Container Manager** - Container runtime integration
- **Web UI** - Dashboard and visual management
- **Node Manager** - Cluster coordination

## 📦 Features in Detail

### Container Management

Hivemind provides comprehensive container lifecycle management:

- **Deploy containers** from various image sources
- **Scale applications** up or down with automatic load balancing
- **Restart containers** with zero downtime
- **Monitor container metrics** including CPU, memory, and network usage
- **View container logs** directly from the dashboard

### Service Discovery

Automatic DNS-based service discovery allows:

- **Service domains** for easy access to your applications
- **Automatic load balancing** across container instances
- **Health checking** to ensure traffic only goes to healthy containers
- **Built-in DNS server** for resolving service domains
- **Proxy functionality** to route external traffic

### Volume Management

Persistent storage for stateful applications:

- **Create and manage volumes** for persistent data
- **Mount volumes** to containers during deployment
- **Back up volume data** for disaster recovery
- **Monitor volume usage** to prevent storage issues

### Clustering

Distributed operation for scaling and high availability:

- **Auto-discovery** of nodes on the network
- **Seamless joining** of new nodes to the cluster
- **Resource-aware scheduling** of containers
- **Node health monitoring** for reliability
- **Distributed storage** for cluster state

## 🛠️ Development

### Prerequisites

- Rust 1.60 or newer
- Youki (for non-mock deployments)
- SQLite

### Building from source

```bash
# Clone the repository
git clone https://github.com/ao/hivemind.git
cd hivemind

# Build the project
cargo build --release
```

### Development Mode

For faster development cycles, you can run with mock implementations:

```bash
cargo run -- web --port 3000
```

### Run tests

```bash
cargo test
```

### Project Structure

- `src/app.rs` - Application & container management
- `src/youki_manager.rs` - Youki integration
- `src/service_discovery.rs` - Service discovery & DNS
- `src/storage.rs` - Persistence layer
- `src/node.rs` - Node & cluster management
- `src/scheduler.rs` - Container scheduler
- `src/web.rs` - Web UI & dashboard
- `src/main.rs` - CLI & entry point

## 📜 License

Hivemind is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
