# Hivemind Installation Guide

This guide provides comprehensive instructions for installing and setting up the Hivemind container orchestration platform on various environments.

## System Requirements

### Minimum Requirements
- **CPU**: 2 cores
- **RAM**: 2 GB
- **Disk**: 20 GB
- **OS**: Linux (Ubuntu 20.04+, Debian 11+, CentOS 8+, or any modern distribution)
- **Kernel**: 5.4 or newer (for optimal container networking support)

### Recommended Requirements
- **CPU**: 4+ cores
- **RAM**: 4+ GB
- **Disk**: 50+ GB
- **OS**: Ubuntu 22.04 or newer
- **Kernel**: 5.15 or newer

### Software Prerequisites
- **containerd**: 1.6.0 or newer
- **Go**: 1.18 or newer (for building from source)
- **SQLite**: 3.35.0 or newer

## Installation Methods

Hivemind can be installed using several methods:

1. [Binary Installation](#binary-installation) (Recommended for production)
2. [Package Manager Installation](#package-manager-installation)
3. [Building from Source](#building-from-source) (Recommended for development)
4. [Docker Installation](#docker-installation)

## Binary Installation

### Linux (x86_64)

1. Download the latest release:
   ```bash
   curl -LO https://github.com/ao/hivemind/releases/latest/download/hivemind-linux-amd64.tar.gz
   ```

2. Extract the archive:
   ```bash
   tar -xzf hivemind-linux-amd64.tar.gz
   ```

3. Move the binary to a directory in your PATH:
   ```bash
   sudo mv hivemind /usr/local/bin/
   ```

4. Verify the installation:
   ```bash
   hivemind --version
   ```

### Linux (ARM64)

1. Download the latest release:
   ```bash
   curl -LO https://github.com/ao/hivemind/releases/latest/download/hivemind-linux-arm64.tar.gz
   ```

2. Extract the archive:
   ```bash
   tar -xzf hivemind-linux-arm64.tar.gz
   ```

3. Move the binary to a directory in your PATH:
   ```bash
   sudo mv hivemind /usr/local/bin/
   ```

4. Verify the installation:
   ```bash
   hivemind --version
   ```

### macOS

1. Download the latest release:
   ```bash
   curl -LO https://github.com/ao/hivemind/releases/latest/download/hivemind-macos-amd64.tar.gz
   ```

2. Extract the archive:
   ```bash
   tar -xzf hivemind-macos-amd64.tar.gz
   ```

3. Move the binary to a directory in your PATH:
   ```bash
   sudo mv hivemind /usr/local/bin/
   ```

4. Verify the installation:
   ```bash
   hivemind --version
   ```

## Package Manager Installation

### Using Go Install

```bash
go install github.com/ao/hivemind@latest
```

### Using Homebrew (macOS)

```bash
brew tap ao/hivemind
brew install hivemind
```

## Building from Source

Building from source is recommended for development or if you need to customize the build.

### Prerequisites

- Go 1.18 or newer
- Git
- Build essentials (gcc, make, etc.)

### Steps

1. Clone the repository:
   ```bash
   git clone https://github.com/ao/hivemind.git
   cd hivemind
   ```

2. Build the project:
   ```bash
   make build
   ```

3. Install the binary:
   ```bash
   sudo cp bin/hivemind /usr/local/bin/
   ```

4. Verify the installation:
   ```bash
   hivemind --version
   ```

## Docker Installation

Hivemind can also be run as a Docker container:

```bash
docker run -d --name hivemind \
  --restart always \
  -p 3000:4483 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v hivemind-data:/var/lib/hivemind \
  aodev/hivemind:latest
```

## Post-Installation Setup

### Creating a Systemd Service (Linux)

1. Create a systemd service file:
   ```bash
   sudo nano /etc/systemd/system/hivemind.service
   ```

2. Add the following content:
   ```
   [Unit]
   Description=Hivemind Container Orchestration Platform
   Documentation=https://github.com/ao/hivemind
   After=network.target containerd.service

   [Service]
   ExecStart=/usr/local/bin/hivemind daemon --web-port 3000
   Restart=always
   RestartSec=5
   User=root
   Group=root
   Environment=GO_LOG=info

   [Install]
   WantedBy=multi-user.target
   ```

3. Enable and start the service:
   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable hivemind
   sudo systemctl start hivemind
   ```

4. Check the service status:
   ```bash
   sudo systemctl status hivemind
   ```

### Configuration

Hivemind uses a configuration file located at `/etc/hivemind/config.yaml` by default. You can create this file with custom settings:

```bash
sudo mkdir -p /etc/hivemind
sudo nano /etc/hivemind/config.yaml
```

Example configuration:
```yaml
# Daemon configuration
daemon:
  web_port: 3000
  data_dir: "/var/lib/hivemind"
  log_level: "info"

# Network configuration
network:
  network_cidr: "10.244.0.0/16"
  node_subnet_size: 24
  vxlan_id: 42
  vxlan_port: 4789

# Security configuration
security:
  enable_rbac: true
  enable_container_scanning: true
  enable_network_policies: true
  enable_secret_management: true
```

### Setting Up containerd

Hivemind requires containerd to be installed and configured:

1. Install containerd:
   ```bash
   # Ubuntu/Debian
   sudo apt-get update
   sudo apt-get install -y containerd

   # CentOS/RHEL
   sudo yum install -y containerd
   ```

2. Configure containerd:
   ```bash
   sudo mkdir -p /etc/containerd
   containerd config default | sudo tee /etc/containerd/config.toml
   ```

3. Enable and start containerd:
   ```bash
   sudo systemctl enable containerd
   sudo systemctl start containerd
   ```

## Setting Up a Multi-Node Cluster

### Initializing the First Node

1. Start Hivemind on the first node:
   ```bash
   hivemind daemon --web-port 3000
   ```

2. Note the node's IP address:
   ```bash
   ip addr show
   ```

### Joining Additional Nodes

1. Install Hivemind on additional nodes following the installation instructions above.

2. Join the cluster by specifying the first node's address:
   ```bash
   hivemind join --host <first-node-ip>:4483
   ```

3. Verify the node has joined the cluster:
   ```bash
   hivemind node ls
   ```

## Verifying the Installation

1. Check the Hivemind daemon status:
   ```bash
   hivemind health
   ```

2. Access the web UI:
   Open a web browser and navigate to `http://<node-ip>:4483`

3. Deploy a test application:
   ```bash
   hivemind app deploy --image nginx:latest --name test-app --service test.local
   ```

4. Verify the application is running:
   ```bash
   hivemind app containers
   ```

## Troubleshooting

### Common Issues

#### Hivemind fails to start

Check the logs:
```bash
journalctl -u hivemind
```

Ensure containerd is running:
```bash
systemctl status containerd
```

#### Node fails to join the cluster

Check network connectivity:
```bash
ping <first-node-ip>
```

Ensure the first node is accessible on port 3000:
```bash
telnet <first-node-ip> 3000
```

#### Container networking issues

Check the overlay network status:
```bash
hivemind network status
```

Ensure the VXLAN port (4789) is not blocked by firewalls:
```bash
sudo ufw allow 4789/udp
```

### Getting Help

- Check the [Troubleshooting Guide](troubleshooting_guide.md) for more detailed troubleshooting steps
- Visit the [GitHub Issues](https://github.com/ao/hivemind/issues) page to report bugs or request help
- Join the [Community Forum](https://community.hivemind.io) for community support

## Next Steps

- [User Guide](user_guide.md): Learn how to use Hivemind
- [Administration Guide](administration_guide.md): Learn how to administer Hivemind
- [Developer Guide](../developer_guide.md): Learn how to develop for Hivemind
- [Security Guide](security_guide.md): Learn about Hivemind's security features