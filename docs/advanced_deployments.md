# Advanced Deployment Strategies

This document provides comprehensive information about Hivemind's advanced deployment strategies, which allow you to deploy applications with minimal downtime, reduced risk, and enhanced testing capabilities.

## Overview

Hivemind supports several advanced deployment strategies that go beyond simple container deployments. These strategies enable you to deploy applications with zero downtime, gradually roll out changes, test new versions with a subset of users, and more.

## Key Features

- **Multiple Deployment Strategies**: Choose from simple, rolling update, blue-green, canary, and A/B testing deployments
- **Zero Downtime Deployments**: Deploy applications without interrupting service
- **Gradual Rollouts**: Roll out changes incrementally to reduce risk
- **Automated Verification**: Automatically verify deployments before routing traffic
- **Traffic Splitting**: Split traffic between different versions for testing
- **Automated Rollbacks**: Automatically roll back failed deployments
- **Deployment History**: Track deployment history and changes
- **Custom Deployment Hooks**: Execute custom scripts before, during, and after deployments

## Deployment Strategies

### Simple Deployment

The simple deployment strategy deploys all containers at once. This is the most basic strategy and is suitable for non-critical applications or initial deployments.

#### Configuration

```bash
hivemind app deploy --image nginx:latest --name web-app --strategy simple
```

#### Workflow

1. Stop all existing containers (if any)
2. Deploy new containers
3. Start the new containers
4. Route traffic to the new containers

#### Pros and Cons

**Pros:**
- Simple and fast
- Minimal resource usage

**Cons:**
- Downtime during deployment
- No gradual rollout
- No automated verification

### Rolling Update

The rolling update strategy updates containers in batches, ensuring that a minimum number of containers are always available. This strategy is suitable for applications that can handle a mix of old and new versions.

#### Configuration

Standard Rolling Update:
```bash
hivemind app deploy --image nginx:latest --name web-app --strategy rolling-update --max-unavailable 1 --max-surge 1
```

Zero-Downtime Rolling Update:
```bash
hivemind app zero-downtime-deploy --image nginx:latest --name web-app --service web-service --batch-size 2 --batch-delay 30 --health-check-path /health --health-check-port 8080 --health-check-timeout 60 --drain-timeout 30
```

Parameters:
- `max-unavailable`: Maximum number of containers that can be unavailable during the update
- `max-surge`: Maximum number of extra containers that can be created during the update
- `batch-size`: Number of containers to update in each batch (zero-downtime only)
- `batch-delay`: Delay in seconds between batches (zero-downtime only)
- `health-check-path`: Path to use for health checks (zero-downtime only)
- `health-check-port`: Port to use for health checks (zero-downtime only)
- `health-check-timeout`: Timeout in seconds for health checks (zero-downtime only)
- `drain-timeout`: Time in seconds to wait for connections to drain before removing old containers (zero-downtime only)

#### Workflow

Standard Rolling Update:
1. Create new containers (up to max-surge)
2. Wait for new containers to be ready
3. Stop old containers (up to max-unavailable)
4. Repeat until all containers are updated

Zero-Downtime Rolling Update:
1. Create new containers in batches (batch-size)
2. Wait for new containers to be ready
3. Perform health checks on new containers
4. Gradually shift traffic to new containers
5. Wait for connections to drain from old containers (drain-timeout)
6. Remove old containers
7. Repeat until all containers are updated

#### Pros and Cons

**Pros:**
- Minimal or no downtime
- Controlled rollout
- Resource efficient
- Health verification (zero-downtime)
- Connection draining (zero-downtime)
- Automated rollback on failure (zero-downtime)

**Cons:**
- Both old and new versions run simultaneously
- Requires proper health checks for zero-downtime
- Potential issues with mixed versions
- More complex configuration for zero-downtime

### Blue-Green Deployment

The blue-green deployment strategy creates a complete new environment (green) alongside the existing one (blue). Once the new environment is ready and verified, traffic is switched from blue to green. This strategy is suitable for applications that require zero downtime and cannot handle mixed versions.

#### Configuration

```bash
hivemind app deploy --image nginx:latest --name web-app --strategy blue-green --verification-timeout 60
```

Parameters:
- `verification-timeout`: Time in seconds to verify the new environment before switching traffic

#### Workflow

1. Create a complete new environment (green)
2. Wait for the new environment to be ready
3. Verify the new environment
4. Switch traffic from blue to green
5. Keep the blue environment as a backup for quick rollback

#### Pros and Cons

**Pros:**
- Zero downtime
- Complete isolation between versions
- Easy and fast rollback
- Full verification before traffic switch

**Cons:**
- Resource intensive (requires double the resources)
- More complex setup
- Longer deployment time

## Monitoring and Managing Deployments

Hivemind provides tools to monitor and manage your deployments, allowing you to track progress, check status, and perform rollbacks if necessary.

### Checking Deployment Status

You can check the status of a deployment using the following command:

```bash
hivemind app deployment-status --id <deployment-id>
```

This will show you:
- Current status (pending, in progress, completed, failed)
- Progress percentage
- Details about the deployment
- Creation and last update timestamps
- Whether the deployment is completed
- Whether the deployment was successful

### Rolling Back Deployments

If a deployment fails or you need to revert to a previous version, you can roll back a deployment using:

```bash
hivemind app rollback-deployment --id <deployment-id>
```

This will:
1. Stop routing traffic to the new version
2. Restore traffic to the previous version
3. Remove the new version containers

### API Access

All deployment operations are also available through the API:

- Create a zero-downtime deployment:
  ```
  POST /api/deployments/zero-downtime
  ```

- Get deployment status:
  ```
  GET /api/deployments/status/:id
  ```

- Rollback a deployment:
  ```
  POST /api/deployments/rollback/:id
  ```

## Best Practices

### Health Checks

For zero-downtime deployments, proper health checks are critical. Configure health checks that:

1. Verify your application is truly ready to serve traffic
2. Check both basic connectivity and application functionality
3. Have appropriate timeouts and thresholds

Example health check path: `/health` or `/status`

### Connection Draining

When using zero-downtime deployments, set an appropriate drain timeout to allow existing connections to complete before removing old containers. This prevents disruption to active users.

Recommended drain timeout: 30-60 seconds for most web applications

### Batch Sizing

Choose appropriate batch sizes for rolling updates:
- Smaller batches: Lower risk, longer deployment time
- Larger batches: Higher risk, shorter deployment time

For critical applications, start with small batches (1-2 containers) and increase as you gain confidence.
4. Switch traffic from blue to green
5. Terminate the old environment (blue)

#### Pros and Cons

**Pros:**
- Zero downtime
- Clean separation between versions
- Easy rollback (switch back to blue)
- Automated verification

**Cons:**
- Resource intensive (requires double the resources)
- Longer deployment time
- Potential database migration issues

### Canary Deployment

The canary deployment strategy gradually routes traffic to the new version, starting with a small percentage and increasing over time. This strategy is suitable for applications that need to be tested with real users before full deployment.

#### Configuration

```bash
hivemind app deploy --image nginx:latest --name web-app --strategy canary --percentage 20 --steps 20,50,100 --interval 300
```

Parameters:
- `percentage`: Initial percentage of traffic to route to the new version
- `steps`: Percentage steps for incremental rollout
- `interval`: Time in seconds between steps

#### Workflow

1. Deploy the new version alongside the old version
2. Route a small percentage of traffic to the new version
3. Monitor the new version for issues
4. Gradually increase the percentage of traffic to the new version
5. Once 100% of traffic is routed to the new version, terminate the old version

#### Pros and Cons

**Pros:**
- Minimal risk
- Early detection of issues
- Gradual rollout
- Real user testing

**Cons:**
- Complex traffic routing
- Longer deployment time
- Both versions run simultaneously

### A/B Testing

The A/B testing strategy deploys multiple variants of an application and routes traffic between them based on specified criteria. This strategy is suitable for testing different versions of an application with real users to gather data for decision-making.

#### Configuration

```bash
hivemind app deploy --name web-app --strategy ab-testing --variant "name=v1,image=myapp:v1,percentage=50" --variant "name=v2,image=myapp:v2,percentage=50" --duration 86400
```

Parameters:
- `variant`: Configuration for each variant (name, image, percentage)
- `duration`: Duration of the test in seconds

#### Workflow

1. Deploy multiple variants of the application
2. Route traffic to the variants based on specified percentages
3. Collect metrics and data from each variant
4. After the specified duration, select the winning variant
5. Deploy the winning variant as the new version

#### Pros and Cons

**Pros:**
- Data-driven decision making
- Real user testing
- Multiple variants can be tested simultaneously

**Cons:**
- Complex setup and monitoring
- Resource intensive
- Requires proper metrics and analysis

## Deployment Configuration

### Creating a Deployment

To create a new deployment:

```bash
hivemind deployment create --name <NAME> --app-name <APP_NAME> --image <IMAGE> --replicas <COUNT> --strategy <STRATEGY>
```

Example:

```bash
hivemind deployment create --name web-deployment --app-name web-app --image nginx:latest --replicas 3 --strategy blue-green
```

### Listing Deployments

To list all deployments:

```bash
hivemind deployment list
```

### Getting Deployment Details

To get details about a specific deployment:

```bash
hivemind deployment get --id <DEPLOYMENT_ID>
```

### Executing a Deployment

To execute a deployment:

```bash
hivemind deployment execute --id <DEPLOYMENT_ID>
```

### Rolling Back a Deployment

To roll back a deployment:

```bash
hivemind deployment rollback --id <DEPLOYMENT_ID>
```

### Getting Deployment Status

To get the status of a deployment:

```bash
hivemind deployment status --id <DEPLOYMENT_ID>
```

## Advanced Configuration

### Deployment Hooks

Hivemind supports custom hooks that can be executed at various stages of the deployment process.

#### Pre-Deployment Hooks

Pre-deployment hooks are executed before the deployment starts:

```bash
hivemind deployment create --name web-deployment --app-name web-app --image nginx:latest --pre-hook "run-tests.sh"
```

#### Post-Deployment Hooks

Post-deployment hooks are executed after the deployment completes:

```bash
hivemind deployment create --name web-deployment --app-name web-app --image nginx:latest --post-hook "notify-team.sh"
```

#### Rollback Hooks

Rollback hooks are executed when a deployment is rolled back:

```bash
hivemind deployment create --name web-deployment --app-name web-app --image nginx:latest --rollback-hook "cleanup.sh"
```

### Health Checks

Hivemind uses health checks to verify that deployed containers are functioning correctly:

```bash
hivemind deployment create --name web-deployment --app-name web-app --image nginx:latest --health-check-path /health --health-check-port 8080
```

### Traffic Routing

For strategies that involve traffic routing (blue-green, canary, A/B testing), you can configure how traffic is routed:

```bash
hivemind deployment create --name web-deployment --app-name web-app --image nginx:latest --strategy canary --traffic-router service-mesh
```

### Resource Allocation

You can specify resource requirements for deployments:

```bash
hivemind deployment create --name web-deployment --app-name web-app --image nginx:latest --cpu 0.5 --memory 512M
```

### Environment Variables

You can specify environment variables for deployments:

```bash
hivemind deployment create --name web-deployment --app-name web-app --image nginx:latest --env "KEY1=value1" --env "KEY2=value2"
```

### Volume Mounts

You can specify volume mounts for deployments:

```bash
hivemind deployment create --name web-deployment --app-name web-app --image nginx:latest --volume data-volume:/data
```

## Integration with Other Features

### CI/CD Integration

Hivemind's advanced deployment strategies can be integrated with CI/CD pipelines:

```bash
hivemind cicd configure-deployment --pipeline my-pipeline --strategy blue-green --verification-timeout 60
```

### Cloud Integration

Hivemind's advanced deployment strategies work seamlessly with cloud providers:

```bash
hivemind deployment create --name web-deployment --app-name web-app --image nginx:latest --strategy blue-green --cloud-provider aws
```

### Observability Integration

Hivemind's advanced deployment strategies are integrated with observability features for monitoring and troubleshooting:

```bash
hivemind deployment create --name web-deployment --app-name web-app --image nginx:latest --strategy canary --metrics-endpoint /metrics
```

## Implementation Details

### Deployment Manager

The Deployment Manager is responsible for managing deployments:

```rust
pub struct DeploymentManager {
    // ...
}

impl DeploymentManager {
    pub async fn create_deployment(&self, deployment: Deployment) -> Result<String> {
        // ...
    }
    
    pub async fn execute_deployment(&self, deployment_id: &str) -> Result<()> {
        // ...
    }
    
    pub async fn rollback_deployment(&self, deployment_id: &str) -> Result<()> {
        // ...
    }
    
    // ...
}
```

### Deployment Executors

Each deployment strategy is implemented as a separate executor:

```rust
pub trait DeploymentExecutor: Send + Sync {
    async fn execute(&self, deployment: &Deployment) -> Result<()>;
    async fn rollback(&self, deployment: &Deployment) -> Result<()>;
    async fn get_status(&self, deployment_id: &str) -> Result<DeploymentStatus>;
}

pub struct SimpleDeploymentExecutor {
    // ...
}

pub struct RollingUpdateDeploymentExecutor {
    // ...
}

pub struct BlueGreenDeploymentExecutor {
    // ...
}

pub struct CanaryDeploymentExecutor {
    // ...
}

pub struct ABTestingDeploymentExecutor {
    // ...
}
```

## API Reference

### REST API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/deployments` | GET | List all deployments |
| `/api/deployments` | POST | Create a new deployment |
| `/api/deployments/{id}` | GET | Get a deployment by ID |
| `/api/deployments/{id}/execute` | POST | Execute a deployment |
| `/api/deployments/{id}/rollback` | POST | Roll back a deployment |
| `/api/deployments/{id}/status` | GET | Get deployment status |

### CLI Reference

| Command | Description |
|---------|-------------|
| `hivemind deployment create` | Create a new deployment |
| `hivemind deployment list` | List all deployments |
| `hivemind deployment get` | Get a deployment by ID |
| `hivemind deployment execute` | Execute a deployment |
| `hivemind deployment rollback` | Roll back a deployment |
| `hivemind deployment status` | Get deployment status |

## Example Workflows

### Blue-Green Deployment of a Web Application

1. **Create a deployment**:
   ```bash
   hivemind deployment create --name web-deployment --app-name web-app --image nginx:latest --replicas 3 --strategy blue-green --verification-timeout 60
   ```

2. **Execute the deployment**:
   ```bash
   hivemind deployment execute --id deployment-12345
   ```

3. **Monitor the deployment**:
   ```bash
   hivemind deployment status --id deployment-12345
   ```

4. **If issues are detected, roll back the deployment**:
   ```bash
   hivemind deployment rollback --id deployment-12345
   ```

### Canary Deployment of an API Service

1. **Create a deployment**:
   ```bash
   hivemind deployment create --name api-deployment --app-name api-service --image api:v2 --replicas 5 --strategy canary --percentage 20 --steps 20,50,100 --interval 300
   ```

2. **Execute the deployment**:
   ```bash
   hivemind deployment execute --id deployment-67890
   ```

3. **Monitor the deployment**:
   ```bash
   hivemind deployment status --id deployment-67890
   ```

4. **Check metrics during the canary deployment**:
   ```bash
   hivemind metrics get --deployment-id deployment-67890
   ```

### A/B Testing of a Feature

1. **Create a deployment**:
   ```bash
   hivemind deployment create --name feature-test --app-name web-app --strategy ab-testing --variant "name=original,image=web:v1,percentage=50" --variant "name=new-feature,image=web:v2,percentage=50" --duration 86400
   ```

2. **Execute the deployment**:
   ```bash
   hivemind deployment execute --id deployment-abcde
   ```

3. **Monitor the deployment**:
   ```bash
   hivemind deployment status --id deployment-abcde
   ```

4. **Analyze metrics from both variants**:
   ```bash
   hivemind metrics get --deployment-id deployment-abcde --variant original
   hivemind metrics get --deployment-id deployment-abcde --variant new-feature
   ```

5. **Select the winning variant**:
   ```bash
   hivemind deployment select-winner --id deployment-abcde --variant new-feature
   ```

## Best Practices

- **Start with simple strategies** for non-critical applications
- **Use blue-green deployments** for zero-downtime requirements
- **Use canary deployments** for gradual rollouts and risk reduction
- **Use A/B testing** for feature testing and data-driven decisions
- **Implement proper health checks** to verify deployments
- **Configure appropriate timeouts** for verification and rollbacks
- **Monitor deployments** closely during execution
- **Automate rollbacks** for failed deployments
- **Test deployment strategies** in non-production environments first
- **Document deployment procedures** for each application

## Troubleshooting

### Common Issues

1. **Deployment fails to start**:
   - Check resource availability
   - Verify image exists and is accessible
   - Check for configuration errors

2. **Health checks fail**:
   - Verify application is functioning correctly
   - Check health check path and port
   - Increase health check timeout if needed

3. **Traffic routing issues**:
   - Check service discovery configuration
   - Verify load balancer configuration
   - Check network policies

4. **Rollback fails**:
   - Check if previous version is still available
   - Verify rollback configuration
   - Check for resource constraints

### Debugging

To debug deployment issues:

```bash
hivemind deployment diagnose --id <DEPLOYMENT_ID> --verbose
```

## Conclusion

Hivemind's advanced deployment strategies provide powerful tools for deploying applications with minimal downtime, reduced risk, and enhanced testing capabilities. By choosing the right strategy for each application and use case, you can ensure reliable and efficient deployments.