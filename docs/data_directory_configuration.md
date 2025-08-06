# Hivemind Data Directory Configuration

## Overview

Hivemind stores its data (applications, volumes, etc.) in a data directory. By default, this directory is located at `/var/lib/hivemind`. However, this location may not be accessible to all users due to permission restrictions, resulting in errors like:

```
FATA[2025-08-05T13:23:09+04:00] Failed to create server: failed to create data directory: mkdir /var/lib/hivemind: permission denied
```

This document explains how to configure a custom data directory for Hivemind.

## Configuration Options

Hivemind provides multiple ways to specify a custom data directory:

### 1. Command-line Flag

Use the `--data-dir` flag when starting Hivemind:

```bash
./bin/hivemind --data-dir /path/to/custom/directory
```

### 2. Environment Variable

Set the `HIVEMIND_DATA_DIR` environment variable:

```bash
export HIVEMIND_DATA_DIR=/path/to/custom/directory
./bin/hivemind
```

Or in a single command:

```bash
HIVEMIND_DATA_DIR=/path/to/custom/directory ./bin/hivemind
```

## Precedence Order

When multiple configuration methods are used, Hivemind follows this precedence order:

1. Command-line flag (`--data-dir`) - highest priority
2. Environment variable (`HIVEMIND_DATA_DIR`)
3. Default value (`/var/lib/hivemind`) - lowest priority

For example, if both the command-line flag and environment variable are specified, the command-line flag takes precedence.

## Examples

### Using a Local Directory

To use a directory in your current location:

```bash
./bin/hivemind --data-dir ./hivemind-data
```

### Using a Home Directory

To use a directory in your home folder:

```bash
./bin/hivemind --data-dir $HOME/hivemind-data
```

### Using Environment Variable

```bash
HIVEMIND_DATA_DIR=$HOME/hivemind-data ./bin/hivemind
```

## Testing

A test script is provided to demonstrate the different configuration options:

```bash
./test_data_dir.sh
```

This script shows examples of using both the command-line flag and environment variable.

## Troubleshooting

If you encounter permission issues even with a custom data directory, ensure that:

1. The specified directory exists or can be created by the current user
2. The current user has write permissions for the specified directory
3. There is sufficient disk space available in the specified location