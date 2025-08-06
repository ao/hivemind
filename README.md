<div align="center">
<img src="./assets/logo1.png" width="100px">
<h1>Hivemind</h1>
<p><strong>A lightweight, distributed container orchestration platform</strong></p>
</div>

<!-- Build Status Badges -->
<div align="center">

![Tests](https://github.com/ao/hivemind/workflows/Test/badge.svg)
![Release](https://github.com/ao/hivemind/workflows/Release/badge.svg)
[![codecov](https://codecov.io/gh/ao/hivemind/branch/main/graph/badge.svg)](https://codecov.io/gh/ao/hivemind)
[![Go Report Card](https://goreportcard.com/badge/github.com/ao/hivemind)](https://goreportcard.com/report/github.com/ao/hivemind)
[![Security Rating](https://sonarcloud.io/api/project_badges/measure?project=ao_hivemind&metric=security_rating)](https://sonarcloud.io/summary/new_code?id=ao_hivemind)

[![Latest Release](https://img.shields.io/github/v/release/ao/hivemind)](https://github.com/ao/hivemind/releases/latest)
[![Go Version](https://img.shields.io/badge/Go-1.21%20|%201.22%20|%201.23-blue)](https://golang.org/)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](https://www.gnu.org/licenses/gpl-3.0)

</div>

# Hivemind Container Orchestration System

Hivemind is a Go-based container orchestration system designed for distributed environments with a focus on resilience and scalability. It provides a comprehensive set of features for managing containers, nodes, storage, and applications in a distributed environment.

## Architecture

Hivemind is built with a modular Go architecture, consisting of several key components:

### Container Manager

The Container Manager is responsible for managing container lifecycle, including:
- Creating, starting, stopping, and removing containers
- Managing container images
- Handling container volumes
- Executing commands in containers
- Retrieving container logs

### Node Manager

The Node Manager is responsible for managing nodes in the cluster, including:
- Node discovery and registration
- Health checking
- Resource tracking
- Node status updates

### Membership Protocol

The Membership Protocol is responsible for maintaining cluster membership, including:
- SWIM protocol for failure detection
- Leader election
- Cluster state synchronization
- Gossip-based communication

### Scheduler

The Scheduler is responsible for scheduling tasks on nodes, including:
- Bin packing strategies
- Resource allocation
- Taints and tolerations
- Priority-based scheduling
- Resource overcommitment

### Service Discovery

The Service Discovery component is responsible for service registration and discovery, including:
- Service registration
- Service discovery
- Health checking
- Load balancing

### Storage Manager

The Storage Manager is responsible for managing storage resources, including:
- Volume creation, mounting, and deletion
- Volume health monitoring
- Backup and restore
- Encryption

### App Manager

The App Manager is responsible for managing applications, including:
- Application lifecycle management
- Application scaling
- Application updates
- Application logs

## Getting Started

### Prerequisites

- Go 1.18 or later
- Docker or containerd runtime
- SQLite

### Installation

1. Clone the repository:
```bash
git clone https://github.com/ao/hivemind.git
cd hivemind
```

2. Build the project:
```bash
make build
```

3. Run the server:
```bash
./bin/hivemind --runtime docker
```

Or with containerd:
```bash
./bin/hivemind --runtime containerd
```

### Web Interface

Hivemind includes a fully functional web GUI that provides an intuitive interface for managing your container orchestration platform. After starting the server, you can access the web interface at:

```
http://localhost:4483
```

The web interface includes:
- **Dashboard**: Overview of cluster status, nodes, and applications
- **Applications**: Deploy, manage, and scale applications
- **Containers**: Monitor and manage individual containers
- **Nodes**: View cluster nodes and their health status
- **Services**: Manage service discovery and load balancing
- **Volumes**: Create and manage persistent storage
- **Health**: System health monitoring and diagnostics

All web interface pages are fully functional with proper navigation and template rendering.

## Configuration

Hivemind can be configured using command-line flags:

```bash
./bin/hivemind --data-dir=/path/to/data --runtime docker
```

You can also use environment variables:

```bash
HIVEMIND_DATA_DIR=/tmp/hivemind go run cmd/hivemind/main.go --runtime docker
```

### Runtime Support

Hivemind supports multiple container runtimes:
- **Docker**: Use `--runtime docker` (recommended for development)
- **containerd**: Use `--runtime containerd` (recommended for production)

## Development

### Running Tests

```bash
make test
```

### Running Integration Tests

```bash
make integration-test
```

### Building Documentation

```bash
make docs
```

## Contributing

Contributions are welcome! Please see our [Contributing Guide](CONTRIBUTING.md) for more details.

## License

This project is licensed under the MIT License - see the LICENSE file for details.
