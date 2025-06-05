# Cloud Integration

This document provides comprehensive information about Hivemind's cloud integration capabilities, which allow you to seamlessly deploy and manage your container workloads across major cloud providers.

## Overview

Hivemind provides built-in support for integrating with major cloud providers, allowing you to leverage cloud resources for your container workloads. The cloud integration is designed to be provider-agnostic, with specific implementations for AWS, Azure, and GCP.

## Key Features

- **Multi-Cloud Support**: Deploy to AWS, Azure, and GCP from a single interface
- **Instance Management**: Create, manage, and monitor cloud instances
- **Load Balancer Integration**: Automatically configure cloud load balancers
- **Storage Integration**: Manage cloud storage for persistent data
- **Network Integration**: Configure cloud networking for container communication
- **Cost Optimization**: Optimize resource usage and costs
- **Auto-Scaling**: Automatically scale resources based on demand
- **Spot/Preemptible Instances**: Utilize low-cost instance options
- **Cloud-Native Services**: Integrate with cloud-native services

## Architecture

The cloud integration is built around the following components:

- **CloudManager**: Central component that manages cloud provider integrations
- **CloudProviderInterface**: Interface for different cloud providers
- **CloudInstance**: Represents a cloud instance
- **CloudDisk**: Represents a cloud disk
- **CloudLoadBalancer**: Represents a cloud load balancer

## Supported Cloud Providers

### Amazon Web Services (AWS)

Hivemind integrates with the following AWS services:

- **EC2**: For compute instances
- **EBS**: For persistent storage
- **ELB/ALB/NLB**: For load balancing
- **VPC**: For networking
- **IAM**: For access control
- **CloudWatch**: For monitoring
- **S3**: For object storage
- **ECR**: For container registry

#### Configuration

To configure AWS integration:

```bash
hivemind cloud configure --provider aws --access-key <ACCESS_KEY> --secret-key <SECRET_KEY> --region <REGION>
```

### Microsoft Azure

Hivemind integrates with the following Azure services:

- **Virtual Machines**: For compute instances
- **Managed Disks**: For persistent storage
- **Load Balancer**: For load balancing
- **Virtual Network**: For networking
- **Azure AD**: For access control
- **Monitor**: For monitoring
- **Blob Storage**: For object storage
- **Container Registry**: For container registry

#### Configuration

To configure Azure integration:

```bash
hivemind cloud configure --provider azure --client-id <CLIENT_ID> --client-secret <CLIENT_SECRET> --tenant-id <TENANT_ID> --subscription-id <SUBSCRIPTION_ID>
```

### Google Cloud Platform (GCP)

Hivemind integrates with the following GCP services:

- **Compute Engine**: For compute instances
- **Persistent Disk**: For persistent storage
- **Load Balancing**: For load balancing
- **VPC**: For networking
- **IAM**: For access control
- **Monitoring**: For monitoring
- **Cloud Storage**: For object storage
- **Container Registry**: For container registry

#### Configuration

To configure GCP integration:

```bash
hivemind cloud configure --provider gcp --service-account-key <SERVICE_ACCOUNT_KEY_PATH>
```

## Instance Management

### Creating Instances

To create a new cloud instance:

```bash
hivemind cloud create-instance --name <NAME> --provider <PROVIDER> --region <REGION> --zone <ZONE> --instance-type <TYPE> --disk-size <SIZE>
```

Example:

```bash
hivemind cloud create-instance --name web-server --provider aws --region us-west-2 --zone us-west-2a --instance-type t3.micro --disk-size 20
```

### Listing Instances

To list all cloud instances:

```bash
hivemind cloud list-instances [--provider <PROVIDER>]
```

### Getting Instance Details

To get details about a specific instance:

```bash
hivemind cloud get-instance --id <INSTANCE_ID>
```

### Starting and Stopping Instances

To start an instance:

```bash
hivemind cloud start-instance --id <INSTANCE_ID>
```

To stop an instance:

```bash
hivemind cloud stop-instance --id <INSTANCE_ID>
```

### Terminating Instances

To terminate an instance:

```bash
hivemind cloud terminate-instance --id <INSTANCE_ID>
```

## Disk Management

### Creating Disks

To create a new cloud disk:

```bash
hivemind cloud create-disk --name <NAME> --provider <PROVIDER> --region <REGION> --zone <ZONE> --disk-type <TYPE> --size <SIZE>
```

Example:

```bash
hivemind cloud create-disk --name data-disk --provider aws --region us-west-2 --zone us-west-2a --disk-type gp2 --size 100
```

### Listing Disks

To list all cloud disks:

```bash
hivemind cloud list-disks [--provider <PROVIDER>]
```

### Getting Disk Details

To get details about a specific disk:

```bash
hivemind cloud get-disk --id <DISK_ID>
```

### Attaching and Detaching Disks

To attach a disk to an instance:

```bash
hivemind cloud attach-disk --disk-id <DISK_ID> --instance-id <INSTANCE_ID> --device-name <DEVICE_NAME>
```

To detach a disk from an instance:

```bash
hivemind cloud detach-disk --disk-id <DISK_ID>
```

### Deleting Disks

To delete a disk:

```bash
hivemind cloud delete-disk --id <DISK_ID>
```

## Load Balancer Management

### Creating Load Balancers

To create a new cloud load balancer:

```bash
hivemind cloud create-load-balancer --name <NAME> --provider <PROVIDER> --region <REGION> --type <TYPE> --scheme <SCHEME>
```

Example:

```bash
hivemind cloud create-load-balancer --name web-lb --provider aws --region us-west-2 --type application --scheme internet-facing
```

### Listing Load Balancers

To list all cloud load balancers:

```bash
hivemind cloud list-load-balancers [--provider <PROVIDER>]
```

### Getting Load Balancer Details

To get details about a specific load balancer:

```bash
hivemind cloud get-load-balancer --id <LOAD_BALANCER_ID>
```

### Registering and Deregistering Instances

To register instances with a load balancer:

```bash
hivemind cloud register-instances --lb-id <LOAD_BALANCER_ID> --instance-ids <INSTANCE_ID1>,<INSTANCE_ID2>,...
```

To deregister instances from a load balancer:

```bash
hivemind cloud deregister-instances --lb-id <LOAD_BALANCER_ID> --instance-ids <INSTANCE_ID1>,<INSTANCE_ID2>,...
```

### Deleting Load Balancers

To delete a load balancer:

```bash
hivemind cloud delete-load-balancer --id <LOAD_BALANCER_ID>
```

## Network Management

### Creating VPCs

To create a new VPC:

```bash
hivemind cloud create-vpc --name <NAME> --provider <PROVIDER> --region <REGION> --cidr <CIDR>
```

Example:

```bash
hivemind cloud create-vpc --name main-vpc --provider aws --region us-west-2 --cidr 10.0.0.0/16
```

### Creating Subnets

To create a new subnet:

```bash
hivemind cloud create-subnet --name <NAME> --vpc-id <VPC_ID> --cidr <CIDR> --zone <ZONE>
```

Example:

```bash
hivemind cloud create-subnet --name web-subnet --vpc-id vpc-12345 --cidr 10.0.1.0/24 --zone us-west-2a
```

### Creating Security Groups

To create a new security group:

```bash
hivemind cloud create-security-group --name <NAME> --vpc-id <VPC_ID> --description <DESCRIPTION>
```

Example:

```bash
hivemind cloud create-security-group --name web-sg --vpc-id vpc-12345 --description "Web server security group"
```

### Adding Security Group Rules

To add a rule to a security group:

```bash
hivemind cloud add-security-group-rule --group-id <GROUP_ID> --direction <DIRECTION> --protocol <PROTOCOL> --port-range <PORT_RANGE> --cidr <CIDR>
```

Example:

```bash
hivemind cloud add-security-group-rule --group-id sg-12345 --direction ingress --protocol tcp --port-range 80-80 --cidr 0.0.0.0/0
```

## Cost Optimization

### Using Spot/Preemptible Instances

To create a spot/preemptible instance:

```bash
hivemind cloud create-instance --name <NAME> --provider <PROVIDER> --region <REGION> --zone <ZONE> --instance-type <TYPE> --spot
```

Example:

```bash
hivemind cloud create-instance --name worker --provider aws --region us-west-2 --zone us-west-2a --instance-type c5.large --spot
```

### Auto-Scaling

To configure auto-scaling:

```bash
hivemind cloud configure-auto-scaling --group <GROUP_NAME> --min <MIN> --max <MAX> --desired <DESIRED> --metric <METRIC> --target <TARGET>
```

Example:

```bash
hivemind cloud configure-auto-scaling --group web-asg --min 2 --max 10 --desired 2 --metric cpu --target 70
```

### Resource Tagging

To add tags to resources:

```bash
hivemind cloud add-tags --resource-id <RESOURCE_ID> --tags <KEY1>=<VALUE1>,<KEY2>=<VALUE2>,...
```

Example:

```bash
hivemind cloud add-tags --resource-id i-12345 --tags Environment=Production,Project=Website
```

## Integration with Hivemind

### Deploying Applications to Cloud Instances

To deploy an application to cloud instances:

```bash
hivemind app deploy --image <IMAGE> --name <NAME> --cloud-provider <PROVIDER> --cloud-region <REGION> --instance-type <TYPE> --replicas <COUNT>
```

Example:

```bash
hivemind app deploy --image nginx:latest --name web-app --cloud-provider aws --cloud-region us-west-2 --instance-type t3.micro --replicas 3
```

### Using Cloud Load Balancers

To expose an application using a cloud load balancer:

```bash
hivemind app expose --name <APP_NAME> --port <PORT> --cloud-lb-type <TYPE>
```

Example:

```bash
hivemind app expose --name web-app --port 80 --cloud-lb-type application
```

### Using Cloud Storage for Volumes

To create a volume backed by cloud storage:

```bash
hivemind volume create --name <NAME> --cloud-provider <PROVIDER> --cloud-disk-type <TYPE> --size <SIZE>
```

Example:

```bash
hivemind volume create --name data-volume --cloud-provider aws --cloud-disk-type gp2 --size 100
```

## API Reference

### REST API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/cloud/providers` | GET | List all configured cloud providers |
| `/api/cloud/providers/{provider}` | GET | Get details about a specific provider |
| `/api/cloud/instances` | GET | List all cloud instances |
| `/api/cloud/instances` | POST | Create a new cloud instance |
| `/api/cloud/instances/{id}` | GET | Get details about a specific instance |
| `/api/cloud/instances/{id}/start` | POST | Start an instance |
| `/api/cloud/instances/{id}/stop` | POST | Stop an instance |
| `/api/cloud/instances/{id}/terminate` | POST | Terminate an instance |
| `/api/cloud/disks` | GET | List all cloud disks |
| `/api/cloud/disks` | POST | Create a new cloud disk |
| `/api/cloud/disks/{id}` | GET | Get details about a specific disk |
| `/api/cloud/disks/{id}/attach` | POST | Attach a disk to an instance |
| `/api/cloud/disks/{id}/detach` | POST | Detach a disk from an instance |
| `/api/cloud/disks/{id}` | DELETE | Delete a disk |
| `/api/cloud/load-balancers` | GET | List all cloud load balancers |
| `/api/cloud/load-balancers` | POST | Create a new cloud load balancer |
| `/api/cloud/load-balancers/{id}` | GET | Get details about a specific load balancer |
| `/api/cloud/load-balancers/{id}/register` | POST | Register instances with a load balancer |
| `/api/cloud/load-balancers/{id}/deregister` | POST | Deregister instances from a load balancer |
| `/api/cloud/load-balancers/{id}` | DELETE | Delete a load balancer |

### CLI Reference

| Command | Description |
|---------|-------------|
| `hivemind cloud configure` | Configure a cloud provider |
| `hivemind cloud list-providers` | List all configured cloud providers |
| `hivemind cloud create-instance` | Create a new cloud instance |
| `hivemind cloud list-instances` | List all cloud instances |
| `hivemind cloud get-instance` | Get details about a specific instance |
| `hivemind cloud start-instance` | Start an instance |
| `hivemind cloud stop-instance` | Stop an instance |
| `hivemind cloud terminate-instance` | Terminate an instance |
| `hivemind cloud create-disk` | Create a new cloud disk |
| `hivemind cloud list-disks` | List all cloud disks |
| `hivemind cloud get-disk` | Get details about a specific disk |
| `hivemind cloud attach-disk` | Attach a disk to an instance |
| `hivemind cloud detach-disk` | Detach a disk from an instance |
| `hivemind cloud delete-disk` | Delete a disk |
| `hivemind cloud create-load-balancer` | Create a new cloud load balancer |
| `hivemind cloud list-load-balancers` | List all cloud load balancers |
| `hivemind cloud get-load-balancer` | Get details about a specific load balancer |
| `hivemind cloud register-instances` | Register instances with a load balancer |
| `hivemind cloud deregister-instances` | Deregister instances from a load balancer |
| `hivemind cloud delete-load-balancer` | Delete a load balancer |
| `hivemind cloud create-vpc` | Create a new VPC |
| `hivemind cloud create-subnet` | Create a new subnet |
| `hivemind cloud create-security-group` | Create a new security group |
| `hivemind cloud add-security-group-rule` | Add a rule to a security group |
| `hivemind cloud configure-auto-scaling` | Configure auto-scaling |
| `hivemind cloud add-tags` | Add tags to resources |

## Example Workflows

### Deploying a Web Application on AWS

1. **Configure AWS provider**:
   ```bash
   hivemind cloud configure --provider aws --access-key $AWS_ACCESS_KEY --secret-key $AWS_SECRET_KEY --region us-west-2
   ```

2. **Create a VPC**:
   ```bash
   hivemind cloud create-vpc --name web-vpc --provider aws --region us-west-2 --cidr 10.0.0.0/16
   ```

3. **Create subnets**:
   ```bash
   hivemind cloud create-subnet --name web-subnet-1 --vpc-id vpc-12345 --cidr 10.0.1.0/24 --zone us-west-2a
   hivemind cloud create-subnet --name web-subnet-2 --vpc-id vpc-12345 --cidr 10.0.2.0/24 --zone us-west-2b
   ```

4. **Create a security group**:
   ```bash
   hivemind cloud create-security-group --name web-sg --vpc-id vpc-12345 --description "Web server security group"
   ```

5. **Add security group rules**:
   ```bash
   hivemind cloud add-security-group-rule --group-id sg-12345 --direction ingress --protocol tcp --port-range 80-80 --cidr 0.0.0.0/0
   hivemind cloud add-security-group-rule --group-id sg-12345 --direction ingress --protocol tcp --port-range 443-443 --cidr 0.0.0.0/0
   ```

6. **Create instances**:
   ```bash
   hivemind cloud create-instance --name web-1 --provider aws --region us-west-2 --zone us-west-2a --instance-type t3.micro --disk-size 20 --subnet-id subnet-12345 --security-group-id sg-12345
   hivemind cloud create-instance --name web-2 --provider aws --region us-west-2 --zone us-west-2b --instance-type t3.micro --disk-size 20 --subnet-id subnet-67890 --security-group-id sg-12345
   ```

7. **Create a load balancer**:
   ```bash
   hivemind cloud create-load-balancer --name web-lb --provider aws --region us-west-2 --type application --scheme internet-facing
   ```

8. **Register instances with the load balancer**:
   ```bash
   hivemind cloud register-instances --lb-id lb-12345 --instance-ids i-12345,i-67890
   ```

9. **Deploy the application**:
   ```bash
   hivemind app deploy --image nginx:latest --name web-app --replicas 2 --instance-ids i-12345,i-67890
   ```

### Deploying a Database with Persistent Storage on GCP

1. **Configure GCP provider**:
   ```bash
   hivemind cloud configure --provider gcp --service-account-key service-account.json
   ```

2. **Create a disk**:
   ```bash
   hivemind cloud create-disk --name db-disk --provider gcp --region us-central1 --zone us-central1-a --disk-type pd-ssd --size 100
   ```

3. **Create an instance**:
   ```bash
   hivemind cloud create-instance --name db-server --provider gcp --region us-central1 --zone us-central1-a --instance-type n2-standard-2 --disk-size 20
   ```

4. **Attach the disk to the instance**:
   ```bash
   hivemind cloud attach-disk --disk-id disk-12345 --instance-id instance-12345 --device-name /dev/sdb
   ```

5. **Deploy the database**:
   ```bash
   hivemind app deploy --image postgres:13 --name db --instance-id instance-12345 --volume /dev/sdb:/var/lib/postgresql/data
   ```

## Best Practices

- **Use Infrastructure as Code** to manage cloud resources
- **Implement proper tagging** for cost allocation and resource management
- **Use spot/preemptible instances** for non-critical workloads
- **Implement auto-scaling** to handle variable workloads
- **Use load balancers** for high availability
- **Use cloud-native storage** for persistent data
- **Implement proper security** with security groups and network ACLs
- **Monitor cloud resources** for performance and cost
- **Implement proper backup** and disaster recovery
- **Use multi-region deployments** for high availability

## Troubleshooting

### Common Issues

1. **Authentication failures**:
   - Check credentials
   - Verify permissions
   - Check for expired tokens

2. **Resource creation failures**:
   - Check quota limits
   - Verify region/zone availability
   - Check for name conflicts

3. **Network connectivity issues**:
   - Check security group rules
   - Verify network configuration
   - Check for routing issues

4. **Performance issues**:
   - Check instance type
   - Verify disk type
   - Check for network bottlenecks

### Debugging

To debug cloud integration issues:

```bash
hivemind cloud diagnose --provider <PROVIDER> --resource-type <TYPE> --resource-id <ID>
```

## Conclusion

Hivemind's cloud integration provides a powerful and flexible way to leverage cloud resources for your container workloads. By supporting multiple cloud providers and offering a consistent interface, Hivemind makes it easy to deploy and manage applications across different cloud environments.