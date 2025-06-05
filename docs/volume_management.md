# Hivemind Volume Management

This document provides a comprehensive overview of the Volume Management system in Hivemind, explaining how persistent storage works, how to use volumes, and the underlying implementation details.

## Overview

The Volume Management system in Hivemind provides persistent storage for stateful applications. It allows you to create, manage, and mount volumes to containers, ensuring that data persists across container restarts and rescheduling.

## Key Features

- **Persistent Storage**: Data persists across container restarts and rescheduling
- **Volume Lifecycle Management**: Create, list, and delete volumes
- **Volume Mounting**: Mount volumes to containers during deployment
- **Volume Usage Monitoring**: Track volume usage to prevent storage issues
- **Volume Types**: Support for different volume types (local, network, etc.)
- **Access Control**: Control access to volumes through RBAC

## Using Volumes

### Creating Volumes

You can create volumes using the CLI, API, or Web UI.

#### Using the CLI

```bash
hivemind volume create --name my-data-volume
```

Options:
- `--name`: Name of the volume (required)
- `--size`: Size of the volume (e.g., "10G")
- `--type`: Volume type (default: "local")
- `--labels`: Labels for the volume (e.g., "app=db,env=prod")

#### Using the API

```bash
curl -X POST http://<your-server>:3000/api/volumes/create \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-data-volume",
    "size": "10G",
    "type": "local",
    "labels": {
      "app": "db",
      "env": "prod"
    }
  }'
```

#### Using the Web UI

1. Navigate to `http://<your-server>:3000`
2. Go to the "Volumes" section
3. Click "Create Volume"
4. Fill in the form with the volume details
5. Click "Create"

### Listing Volumes

#### Using the CLI

```bash
hivemind volume ls
```

Options:
- `--labels`: Filter volumes by labels (e.g., "app=db")
- `--format`: Output format (e.g., "json", "table")

#### Using the API

```bash
curl -X GET http://<your-server>:3000/api/volumes
```

#### Using the Web UI

1. Navigate to `http://<your-server>:3000`
2. Go to the "Volumes" section

### Mounting Volumes to Containers

#### Using the CLI

```bash
hivemind app deploy --image mysql:8 --name mysql-db --volume my-data-volume:/var/lib/mysql
```

You can mount multiple volumes:

```bash
hivemind app deploy --image myapp:latest --name myapp \
  --volume config-vol:/etc/myapp/config \
  --volume data-vol:/var/lib/myapp/data
```

#### Using the API

```bash
curl -X POST http://<your-server>:3000/api/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "image": "mysql:8",
    "name": "mysql-db",
    "volumes": [
      {"name": "my-data-volume", "mountPath": "/var/lib/mysql"}
    ]
  }'
```

#### Using the Web UI

1. Navigate to `http://<your-server>:3000`
2. Go to the "Deploy" section
3. Fill in the deployment form
4. In the "Volumes" section, add your volume mounts
5. Click "Deploy"

### Deleting Volumes

#### Using the CLI

```bash
hivemind volume delete --name my-data-volume
```

Options:
- `--force`: Force deletion even if the volume is in use

#### Using the API

```bash
curl -X POST http://<your-server>:3000/api/volumes/delete \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-data-volume",
    "force": false
  }'
```

#### Using the Web UI

1. Navigate to `http://<your-server>:3000`
2. Go to the "Volumes" section
3. Find the volume you want to delete
4. Click the "Delete" button
5. Confirm the deletion

## Volume Types

Hivemind supports different volume types to meet various storage needs:

### Local Volumes

Local volumes are stored on the local node's filesystem. They provide good performance but are tied to a specific node.

```bash
hivemind volume create --name my-local-volume --type local
```

### Network Volumes

Network volumes are stored on a network storage system, allowing them to be accessed from any node in the cluster.

```bash
hivemind volume create --name my-network-volume --type network --options "server=nas.example.com,path=/exports/data"
```

### Host Path Volumes

Host path volumes map a directory from the host into the container.

```bash
hivemind volume create --name my-host-volume --type hostpath --options "path=/data/hostpath"
```

### Ephemeral Volumes

Ephemeral volumes are temporary and exist only for the lifetime of the container.

```bash
hivemind volume create --name my-ephemeral-volume --type ephemeral
```

## Volume Backup and Restore

### Backing Up Volumes

You can back up volumes to preserve their data:

```bash
hivemind volume backup --name my-data-volume --output /path/to/backup/file.tar.gz
```

### Restoring Volumes

You can restore volumes from backups:

```bash
hivemind volume restore --name my-data-volume --input /path/to/backup/file.tar.gz
```

## Volume Usage Monitoring

Hivemind monitors volume usage to prevent storage issues:

### Checking Volume Usage

```bash
hivemind volume usage --name my-data-volume
```

### Setting Usage Alerts

```bash
hivemind volume set-alert --name my-data-volume --threshold 80
```

This will generate an alert when the volume usage exceeds 80%.

## Implementation Details

### Storage Backend

The Volume Management system uses a pluggable storage backend architecture, allowing different storage providers to be used:

- **Local Storage**: Uses the local filesystem
- **Network Storage**: Supports NFS, SMB, etc.
- **Cloud Storage**: Supports AWS EBS, Azure Disk, etc.

### Volume Lifecycle

1. **Creation**: When a volume is created, the storage backend allocates the necessary resources
2. **Mounting**: When a container is deployed with a volume, the volume is mounted into the container
3. **Unmounting**: When a container is stopped, the volume is unmounted
4. **Deletion**: When a volume is deleted, the storage backend releases the resources

### Data Persistence

Volumes are stored in a persistent location on the node or in a network storage system. The data persists even when containers are restarted or rescheduled.

### Volume Drivers

The Volume Management system uses volume drivers to interact with different storage backends:

- **Local Driver**: Manages local volumes
- **NFS Driver**: Manages NFS volumes
- **SMB Driver**: Manages SMB volumes
- **Cloud Drivers**: Manage cloud storage volumes

## Security Considerations

### Access Control

Access to volumes is controlled through RBAC:

```bash
hivemind security role add-permission --name operator --resource volume --action create,read,update,delete
```

### Volume Encryption

Sensitive data in volumes can be encrypted:

```bash
hivemind volume create --name secure-volume --encrypted
```

### Volume Isolation

Volumes are isolated between containers unless explicitly shared.

## Best Practices

### Naming Conventions

Use descriptive names for volumes that indicate their purpose:

- `app-name-data`: For application data
- `app-name-config`: For configuration files
- `app-name-logs`: For log files

### Volume Organization

Organize volumes by application and environment:

```bash
hivemind volume create --name mysql-prod-data --labels "app=mysql,env=prod"
hivemind volume create --name mysql-staging-data --labels "app=mysql,env=staging"
```

### Resource Management

Set appropriate size limits for volumes to prevent resource exhaustion:

```bash
hivemind volume create --name my-data-volume --size 10G
```

### Backup Strategy

Regularly back up important volumes:

```bash
# Create a backup script
cat > backup-volumes.sh << 'EOF'
#!/bin/bash
DATE=$(date +%Y%m%d)
for VOLUME in $(hivemind volume ls --format json | jq -r '.[] | select(.labels.backup=="true") | .name'); do
  hivemind volume backup --name $VOLUME --output /backups/$VOLUME-$DATE.tar.gz
done
EOF

# Make it executable
chmod +x backup-volumes.sh

# Add to crontab
(crontab -l 2>/dev/null; echo "0 2 * * * /path/to/backup-volumes.sh") | crontab -
```

### Performance Optimization

- Use local volumes for I/O-intensive workloads
- Use network volumes for shared data that needs to be accessed from multiple nodes
- Consider using SSD-backed volumes for performance-critical applications

## Troubleshooting

### Common Issues

#### Volume Creation Fails

**Symptoms:**
- Volume creation command fails
- Error messages during creation

**Possible Causes:**
- Insufficient disk space
- Permission issues
- Name conflict

**Solutions:**

1. **Check disk space:**
   ```bash
   df -h
   ```

2. **Check permissions:**
   ```bash
   ls -la /var/lib/hivemind/volumes
   ```

3. **Check for name conflicts:**
   ```bash
   hivemind volume ls
   ```

#### Volume Mount Fails

**Symptoms:**
- Container fails to start
- Error messages about volume mounting

**Possible Causes:**
- Volume doesn't exist
- Permission issues
- Path issues

**Solutions:**

1. **Verify the volume exists:**
   ```bash
   hivemind volume ls
   ```

2. **Check volume permissions:**
   ```bash
   ls -la /var/lib/hivemind/volumes/<volume-name>
   ```

3. **Check container logs:**
   ```bash
   hivemind app logs --name <app-name>
   ```

#### Data Not Persisting

**Symptoms:**
- Data disappears after container restart
- Volume appears empty

**Possible Causes:**
- Writing to wrong path
- Volume not properly mounted
- Permission issues

**Solutions:**

1. **Verify the mount path:**
   ```bash
   hivemind app exec --name <app-name> -- df -h
   ```

2. **Check if the application is writing to the correct path:**
   ```bash
   hivemind app exec --name <app-name> -- ls -la <mount-path>
   ```

3. **Check volume contents:**
   ```bash
   ls -la /var/lib/hivemind/volumes/<volume-name>
   ```

## Conclusion

The Volume Management system in Hivemind provides a robust solution for persistent storage in containerized applications. By following the guidelines in this document, you can effectively create, manage, and use volumes to ensure data persistence and reliability in your applications.

For more information, refer to the following resources:
- [User Guide](user_guide.md)
- [Administration Guide](administration_guide.md)
- [Troubleshooting Guide](troubleshooting_guide.md)