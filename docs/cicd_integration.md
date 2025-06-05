# CI/CD Integration

This document provides comprehensive information about Hivemind's CI/CD integration capabilities, which allow you to automate your application deployment pipeline.

## Overview

Hivemind provides built-in support for CI/CD (Continuous Integration/Continuous Deployment) pipelines, allowing you to automate the building, testing, and deployment of your applications. The CI/CD integration is designed to be flexible and extensible, supporting various CI/CD providers and deployment strategies.

## Key Features

- **Pipeline Management**: Create, update, and manage CI/CD pipelines
- **GitHub Actions Integration**: Seamless integration with GitHub Actions
- **Automated Testing**: Configure automated testing as part of your pipeline
- **Automated Deployment**: Automatically deploy applications when tests pass
- **Advanced Deployment Strategies**: Support for blue-green, canary, and A/B testing deployments
- **Rollback Support**: Automatically roll back to previous versions if deployments fail
- **Webhook Integration**: Trigger pipelines based on repository events
- **Release Management**: Manage releases with semantic versioning

## Architecture

The CI/CD integration is built around the following components:

- **CicdManager**: Central component that manages CI/CD pipelines
- **CicdProvider**: Interface for different CI/CD providers (e.g., GitHub Actions)
- **PipelineConfig**: Configuration for CI/CD pipelines
- **PipelineRun**: Represents a single run of a pipeline
- **DeploymentManager**: Manages the deployment of applications

## CI/CD Providers

Hivemind currently supports the following CI/CD providers:

### GitHub Actions

GitHub Actions is a CI/CD platform that allows you to automate your build, test, and deployment pipeline directly from your GitHub repository.

#### Configuration

To configure GitHub Actions integration:

```bash
hivemind cicd configure-provider --type github-actions --token <GITHUB_TOKEN> --owner <OWNER> --repo <REPO>
```

#### Workflow Generation

Hivemind can automatically generate GitHub Actions workflow files based on your pipeline configuration:

```bash
hivemind cicd generate-workflow --pipeline <PIPELINE_NAME> --output .github/workflows/pipeline.yml
```

## Pipeline Configuration

### Creating a Pipeline

To create a new CI/CD pipeline:

```bash
hivemind cicd create-pipeline --name <PIPELINE_NAME> --repository <REPOSITORY_URL> --branch <BRANCH>
```

### Pipeline Components

A CI/CD pipeline consists of the following components:

#### Build Configuration

```bash
hivemind cicd configure-build --pipeline <PIPELINE_NAME> --command "<BUILD_COMMAND>" --docker-image <IMAGE> --dockerfile-path <PATH>
```

#### Test Configuration

```bash
hivemind cicd configure-tests --pipeline <PIPELINE_NAME> --command "<TEST_COMMAND>" --reports-path <PATH> --timeout <SECONDS>
```

#### Deployment Configuration

```bash
hivemind cicd configure-deployment --pipeline <PIPELINE_NAME> --strategy <STRATEGY> --environment <ENV> --replicas <COUNT>
```

#### Release Configuration

```bash
hivemind cicd configure-release --pipeline <PIPELINE_NAME> --type semver --auto-increment-patch --create-github-release
```

### Deployment Strategies

Hivemind supports the following deployment strategies for CI/CD pipelines:

#### Rolling Update

```bash
hivemind cicd configure-deployment --pipeline <PIPELINE_NAME> --strategy rolling-update --max-unavailable 1 --max-surge 1
```

#### Blue-Green Deployment

```bash
hivemind cicd configure-deployment --pipeline <PIPELINE_NAME> --strategy blue-green --verification-timeout 60
```

#### Canary Deployment

```bash
hivemind cicd configure-deployment --pipeline <PIPELINE_NAME> --strategy canary --percentage 20 --steps 20,50,100 --interval 300
```

## Pipeline Management

### Triggering a Pipeline

To manually trigger a pipeline:

```bash
hivemind cicd trigger-pipeline --name <PIPELINE_NAME>
```

### Viewing Pipeline Status

To view the status of a pipeline:

```bash
hivemind cicd get-pipeline-status --name <PIPELINE_NAME>
```

### Viewing Pipeline Runs

To list all runs for a pipeline:

```bash
hivemind cicd list-pipeline-runs --name <PIPELINE_NAME>
```

### Viewing Pipeline Logs

To view the logs for a pipeline run:

```bash
hivemind cicd get-pipeline-logs --run-id <RUN_ID>
```

### Cancelling a Pipeline Run

To cancel a running pipeline:

```bash
hivemind cicd cancel-pipeline-run --run-id <RUN_ID>
```

## API Reference

### REST API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/cicd/pipelines` | GET | List all pipelines |
| `/api/cicd/pipelines` | POST | Create a new pipeline |
| `/api/cicd/pipelines/{id}` | GET | Get a pipeline by ID |
| `/api/cicd/pipelines/{id}` | PUT | Update a pipeline |
| `/api/cicd/pipelines/{id}` | DELETE | Delete a pipeline |
| `/api/cicd/pipelines/{id}/trigger` | POST | Trigger a pipeline |
| `/api/cicd/runs` | GET | List all pipeline runs |
| `/api/cicd/runs/{id}` | GET | Get a pipeline run by ID |
| `/api/cicd/runs/{id}/logs` | GET | Get logs for a pipeline run |
| `/api/cicd/runs/{id}/cancel` | POST | Cancel a pipeline run |

### CLI Reference

| Command | Description |
|---------|-------------|
| `hivemind cicd configure-provider` | Configure a CI/CD provider |
| `hivemind cicd create-pipeline` | Create a new pipeline |
| `hivemind cicd update-pipeline` | Update an existing pipeline |
| `hivemind cicd delete-pipeline` | Delete a pipeline |
| `hivemind cicd list-pipelines` | List all pipelines |
| `hivemind cicd get-pipeline` | Get a pipeline by name |
| `hivemind cicd trigger-pipeline` | Trigger a pipeline |
| `hivemind cicd list-pipeline-runs` | List all runs for a pipeline |
| `hivemind cicd get-pipeline-run` | Get a pipeline run by ID |
| `hivemind cicd get-pipeline-logs` | Get logs for a pipeline run |
| `hivemind cicd cancel-pipeline-run` | Cancel a pipeline run |
| `hivemind cicd generate-workflow` | Generate a workflow file |

## Example Workflow

Here's an example of a complete CI/CD workflow using Hivemind:

1. **Configure GitHub Actions provider**:
   ```bash
   hivemind cicd configure-provider --type github-actions --token $GITHUB_TOKEN --owner myorg --repo myapp
   ```

2. **Create a pipeline**:
   ```bash
   hivemind cicd create-pipeline --name myapp-pipeline --repository https://github.com/myorg/myapp --branch main
   ```

3. **Configure build**:
   ```bash
   hivemind cicd configure-build --pipeline myapp-pipeline --command "npm ci && npm run build" --cache-paths "node_modules" --cache-key "npm-${{ hashFiles('package-lock.json') }}"
   ```

4. **Configure tests**:
   ```bash
   hivemind cicd configure-tests --pipeline myapp-pipeline --command "npm test" --reports-path "reports" --timeout 300
   ```

5. **Configure deployment**:
   ```bash
   hivemind cicd configure-deployment --pipeline myapp-pipeline --strategy blue-green --environment production --replicas 3
   ```

6. **Configure release**:
   ```bash
   hivemind cicd configure-release --pipeline myapp-pipeline --type semver --auto-increment-patch --create-github-release
   ```

7. **Generate workflow file**:
   ```bash
   hivemind cicd generate-workflow --pipeline myapp-pipeline --output .github/workflows/pipeline.yml
   ```

8. **Commit and push the workflow file**:
   ```bash
   git add .github/workflows/pipeline.yml
   git commit -m "Add CI/CD pipeline"
   git push
   ```

9. **Trigger the pipeline**:
   ```bash
   hivemind cicd trigger-pipeline --name myapp-pipeline
   ```

10. **Monitor the pipeline**:
    ```bash
    hivemind cicd get-pipeline-status --name myapp-pipeline
    ```

## Best Practices

- **Use version control** for your pipeline configurations
- **Start with a simple pipeline** and gradually add complexity
- **Use environment variables** for sensitive information
- **Implement proper testing** before deployment
- **Use blue-green or canary deployments** for critical applications
- **Configure automatic rollbacks** for failed deployments
- **Monitor your pipelines** and set up alerts for failures
- **Regularly review and update** your pipeline configurations

## Troubleshooting

### Common Issues

1. **Pipeline fails to trigger**:
   - Check webhook configuration
   - Verify repository access permissions
   - Check branch name

2. **Build fails**:
   - Check build command
   - Verify dependencies are installed
   - Check for syntax errors in code

3. **Tests fail**:
   - Check test command
   - Verify test environment
   - Check for flaky tests

4. **Deployment fails**:
   - Check deployment configuration
   - Verify target environment is available
   - Check for resource constraints

### Debugging

To debug pipeline issues:

```bash
hivemind cicd get-pipeline-logs --run-id <RUN_ID> --verbose
```

## Conclusion

Hivemind's CI/CD integration provides a powerful and flexible way to automate your application deployment pipeline. By leveraging the built-in support for various CI/CD providers and deployment strategies, you can create a seamless and reliable deployment process for your applications.