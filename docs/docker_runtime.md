# Docker Runtime Support

Hivemind now supports Docker as an alternative container runtime to containerd. This document explains how to use the Docker runtime with Hivemind.

## Overview

By default, Hivemind uses containerd as its container runtime. However, you can now choose to use Docker instead, which may be more familiar to users who are already using Docker in their environments.

## Usage

To use Docker as the container runtime, start Hivemind with the `--runtime` flag set to `docker`:

```bash
hivemind --runtime docker
```

You can also continue to use containerd (the default) by either omitting the flag or explicitly setting it:

```bash
hivemind --runtime containerd
```

## Requirements

To use the Docker runtime:

1. Docker must be installed and running on the host system
2. The user running Hivemind must have permission to access the Docker daemon

## Implementation Details

The Docker runtime adapter implements the `ContainerRuntime` interface defined in the containerd package. This allows Hivemind to use Docker as a drop-in replacement for containerd without changing the core application logic.

The Docker runtime adapter provides the following functionality:

- Pulling images from registries
- Creating and starting containers
- Stopping containers
- Listing containers
- Getting container metrics
- Managing volumes (create, delete, list)

## Limitations

The current Docker runtime implementation has the following limitations:

1. Some advanced containerd features may not be available when using Docker
2. Container metrics may be less detailed than with containerd
3. Volume size information is not available from the Docker API

## Troubleshooting

If you encounter issues with the Docker runtime:

1. Ensure Docker is running: `docker info`
2. Check Docker daemon logs: `journalctl -u docker`
3. Run Hivemind with increased log verbosity: `hivemind --runtime docker --log-level debug`

## Future Improvements

Future versions of Hivemind may include:

- Support for Docker Swarm mode
- Better integration with Docker Compose
- Enhanced metrics collection from Docker containers