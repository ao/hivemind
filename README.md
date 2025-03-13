# Project Hivemind

<img src="./assets//logo1.png" width="100px">

A self-organizing container orchestration system written in Rust. Hivemind provides a lightweight, distributed container management platform with built-in service discovery, health monitoring, and automatic scaling capabilities.

## Features

- Pure Rust implementation for performance and safety
- Distributed node architecture with peer-to-peer discovery
- Automatic application scheduling and load balancing
- Built-in service discovery with DNS and proxy support
- Health monitoring and automatic container recovery
- Web interface for management and monitoring
- SQLite for state persistence
- Minimal resource footprint

## Setup

### Prerequisites

- Rust and Cargo (1.56 or newer)
- containerd
- SQLite

### Installation

1. Install Rust and Cargo:
```bash
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
```

2. Install containerd:
```bash
# Ubuntu/Debian
sudo apt install containerd

# macOS
brew install containerd

# CentOS/RHEL
sudo yum install containerd
```

3. Clone the repository:
```bash
git clone https://github.com/yourusername/hivemind.git
cd hivemind
```

4. Build and run:
```bash
cargo build --release
./target/release/hivemind daemon
```

## Usage

Hivemind provides both a CLI interface and a web interface for managing your container cluster.

### Command Line Interface

#### Starting the Daemon

```bash
# Start the Hivemind daemon
./hivemind daemon

# Specify a custom data directory
./hivemind --data-dir /path/to/data daemon
```

#### Node Management

```bash
# Join an existing Hivemind cluster
./hivemind join --host 192.168.1.100

# List all nodes in the cluster
./hivemind node ls

# Show detailed node information
./hivemind node info
```

#### Application Management

```bash
# List all applications
./hivemind app ls

# Deploy a new application
./hivemind app deploy --image nginx:latest --name web-app

# Deploy with service discovery enabled
./hivemind app deploy --image nginx:latest --name web-app --service webapp.local

# Scale an application
./hivemind app scale --name web-app --replicas 3

# List all containers for an application
./hivemind app containers

# Show detailed container information
./hivemind app container-info --container-id <container-id>

# Restart an application
./hivemind app restart --name web-app
```

#### Health Checking

```bash
# Check system health
./hivemind health
```

### Web Interface

Access the web interface at `http://localhost:3000` to:

1. View the dashboard with cluster overview
2. Manage nodes and applications
3. Deploy and scale applications
4. Monitor system health
5. View logs and metrics

### Service Discovery

Hivemind includes built-in service discovery and accessibility features:

1. Deploy a service with a domain name:
```bash
curl -X POST http://localhost:3000/deploy \
  -H "Content-Type: application/json" \
  -d '{"image": "nginx:latest", "name": "web-app", "service": "webapp.local"}'
```

2. List available services:
```bash
curl http://localhost:3000/services
```

3. Get service endpoints:
```bash
curl http://localhost:3000/service-endpoints
```

4. Get a service URL:
```bash
curl -X POST http://localhost:3000/service-url \
  -H "Content-Type: application/json" \
  -d '{"service_name": "web-app"}'
```

## Architecture

Hivemind is built with a modular architecture consisting of several key components:

### Core Components

- **Node Manager**: Handles node discovery, health monitoring, and peer-to-peer communication
- **App Manager**: Manages application deployment, scaling, updates, and container lifecycle
- **Scheduler**: Distributes applications across nodes based on resource availability
- **Service Discovery**: Provides DNS and proxy services for accessing applications
- **Storage Manager**: Persists state using SQLite

### Communication Flow

1. Nodes discover each other using UDP broadcasts
2. Applications are scheduled based on node resource availability
3. Service discovery maps domain names to application endpoints
4. Health monitoring ensures applications and nodes remain available
5. State is persisted to SQLite for recovery after restarts

## Troubleshooting

### Common Issues

1. **Node Discovery Fails**
   - Check network firewall settings (UDP port 8901 must be open)
   - Ensure nodes are on the same network segment
   - Verify the correct IP address is being used

2. **Application Deployment Fails**
   - Check containerd service is running
   - Verify image name and availability
   - Check disk space and resource availability

3. **Service Discovery Issues**
   - Ensure DNS port (53) is available
   - Check service domain configuration
   - Verify application health status

### Logs

Check logs for detailed error information:

```bash
# View daemon logs
tail -f ~/.hivemind/logs/daemon.log

# View application logs
./hivemind app container-info --container-id <container-id>
```

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the MIT License - see the LICENSE file for details.