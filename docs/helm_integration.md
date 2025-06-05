# Helm Integration

This document provides comprehensive information about Hivemind's Helm integration capabilities, which allow you to deploy and manage applications using Helm charts.

## Overview

Hivemind provides built-in support for Helm, the package manager for Kubernetes. This integration allows you to create, package, and deploy Helm charts for your applications, as well as manage Helm repositories and releases.

## Key Features

- **Helm Chart Management**: Create, package, and deploy Helm charts
- **Repository Management**: Add, update, and remove Helm repositories
- **Release Management**: Install, upgrade, and uninstall Helm releases
- **Chart Creation**: Create Helm charts for Hivemind applications
- **Chart Customization**: Customize Helm charts with values
- **Chart Versioning**: Manage chart versions
- **Chart Dependencies**: Manage chart dependencies
- **Hivemind-Specific Charts**: Pre-built charts for Hivemind components

## Architecture

The Helm integration is built around the following components:

- **HelmManager**: Central component that manages Helm charts, repositories, and releases
- **HelmChart**: Represents a Helm chart
- **HelmRepository**: Represents a Helm repository
- **HelmRelease**: Represents a Helm release

## Helm Chart Management

### Creating a Chart

To create a new Helm chart:

```bash
hivemind helm create-chart --name <NAME> --description <DESCRIPTION>
```

Example:

```bash
hivemind helm create-chart --name my-app --description "My application chart"
```

This command creates a new Helm chart with the following structure:

```
my-app/
├── Chart.yaml
├── values.yaml
├── templates/
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── _helpers.tpl
│   └── NOTES.txt
```

### Packaging a Chart

To package a Helm chart:

```bash
hivemind helm package-chart --chart-dir <CHART_DIR>
```

Example:

```bash
hivemind helm package-chart --chart-dir ./my-app
```

This command creates a packaged chart file (e.g., `my-app-0.1.0.tgz`).

### Installing a Chart

To install a Helm chart:

```bash
hivemind helm install --name <NAME> --chart <CHART> --namespace <NAMESPACE> [--values <VALUES_FILE>]
```

Example:

```bash
hivemind helm install --name my-release --chart ./my-app-0.1.0.tgz --namespace default
```

### Upgrading a Release

To upgrade a Helm release:

```bash
hivemind helm upgrade --name <NAME> --chart <CHART> --namespace <NAMESPACE> [--values <VALUES_FILE>]
```

Example:

```bash
hivemind helm upgrade --name my-release --chart ./my-app-0.1.1.tgz --namespace default
```

### Uninstalling a Release

To uninstall a Helm release:

```bash
hivemind helm uninstall --name <NAME> --namespace <NAMESPACE>
```

Example:

```bash
hivemind helm uninstall --name my-release --namespace default
```

## Repository Management

### Adding a Repository

To add a Helm repository:

```bash
hivemind helm repo add --name <NAME> --url <URL> [--username <USERNAME> --password <PASSWORD>] [--oci]
```

Example:

```bash
hivemind helm repo add --name stable --url https://charts.helm.sh/stable
```

For OCI-based repositories:

```bash
hivemind helm repo add --name my-oci-repo --url oci://registry.example.com/charts --oci
```

### Updating Repositories

To update Helm repositories:

```bash
hivemind helm repo update [--name <NAME>]
```

### Removing a Repository

To remove a Helm repository:

```bash
hivemind helm repo remove --name <NAME>
```

### Listing Repositories

To list all Helm repositories:

```bash
hivemind helm repo list
```

## Release Management

### Getting Release Details

To get details about a specific release:

```bash
hivemind helm get-release --name <NAME> --namespace <NAMESPACE>
```

Example:

```bash
hivemind helm get-release --name my-release --namespace default
```

### Listing Releases

To list all releases:

```bash
hivemind helm list-releases [--namespace <NAMESPACE>]
```

### Getting Release History

To get the history of a release:

```bash
hivemind helm history --name <NAME> --namespace <NAMESPACE>
```

### Rolling Back a Release

To roll back a release to a previous revision:

```bash
hivemind helm rollback --name <NAME> --namespace <NAMESPACE> --revision <REVISION>
```

Example:

```bash
hivemind helm rollback --name my-release --namespace default --revision 1
```

## Chart Customization

### Values Files

Helm charts can be customized using values files:

```yaml
# values.yaml
replicaCount: 3
image:
  repository: nginx
  tag: latest
service:
  type: ClusterIP
  port: 80
```

To install or upgrade a chart with custom values:

```bash
hivemind helm install --name my-release --chart my-app --values values.yaml
```

### Setting Individual Values

You can also set individual values directly:

```bash
hivemind helm install --name my-release --chart my-app --set replicaCount=3 --set image.tag=latest
```

### Using Multiple Values Files

You can use multiple values files, with later files overriding earlier ones:

```bash
hivemind helm install --name my-release --chart my-app --values values.yaml --values production-values.yaml
```

## Chart Dependencies

### Adding Dependencies

Dependencies can be added to a chart by editing the `Chart.yaml` file:

```yaml
# Chart.yaml
dependencies:
  - name: mysql
    version: 8.8.8
    repository: https://charts.helm.sh/stable
```

### Updating Dependencies

To update chart dependencies:

```bash
hivemind helm update-dependencies --chart-dir <CHART_DIR>
```

## Hivemind-Specific Charts

Hivemind provides pre-built charts for its components:

### Creating Hivemind Charts

To create Hivemind-specific charts:

```bash
hivemind helm create-hivemind-charts
```

This command creates the following charts:

- `hivemind-core`: Core components of Hivemind
- `hivemind-monitoring`: Monitoring components (Prometheus, Grafana)
- `hivemind-logging`: Logging components (Elasticsearch, Kibana, Fluentd)
- `hivemind-tracing`: Tracing components (Jaeger)
- `hivemind-complete`: All Hivemind components

### Installing Hivemind Charts

To install a Hivemind chart:

```bash
hivemind helm install --name hivemind --chart hivemind-complete
```

## Integration with Hivemind

### Deploying Applications with Helm

To deploy an application using Helm:

```bash
hivemind app deploy --name <APP_NAME> --helm-chart <CHART> [--helm-values <VALUES_FILE>]
```

Example:

```bash
hivemind app deploy --name my-app --helm-chart stable/nginx --helm-values values.yaml
```

### Converting Hivemind Deployments to Helm Charts

To convert a Hivemind deployment to a Helm chart:

```bash
hivemind app export-helm --name <APP_NAME> --output <OUTPUT_DIR>
```

Example:

```bash
hivemind app export-helm --name my-app --output ./my-app-chart
```

## Implementation Details

### HelmManager

The HelmManager is responsible for managing Helm charts, repositories, and releases:

```rust
pub struct HelmManager {
    base_dir: PathBuf,
    repositories: RwLock<HashMap<String, HelmRepository>>,
    releases: RwLock<HashMap<String, HelmRelease>>,
}

impl HelmManager {
    pub fn new(base_dir: PathBuf) -> Self {
        Self {
            base_dir,
            repositories: RwLock::new(HashMap::new()),
            releases: RwLock::new(HashMap::new()),
        }
    }
    
    pub async fn initialize(&self) -> Result<()> {
        // ...
    }
    
    pub async fn add_repository(&self, repository: HelmRepository) -> Result<()> {
        // ...
    }
    
    pub async fn remove_repository(&self, name: &str) -> Result<()> {
        // ...
    }
    
    pub async fn list_repositories(&self) -> Vec<HelmRepository> {
        // ...
    }
    
    pub async fn create_chart(&self, name: &str, description: &str) -> Result<PathBuf> {
        // ...
    }
    
    pub async fn package_chart(&self, chart_dir: &PathBuf) -> Result<PathBuf> {
        // ...
    }
    
    pub async fn install_chart(
        &self,
        name: &str,
        chart: &str,
        namespace: &str,
        values: Option<&str>,
    ) -> Result<()> {
        // ...
    }
    
    pub async fn upgrade_release(
        &self,
        name: &str,
        chart: &str,
        namespace: &str,
        values: Option<&str>,
    ) -> Result<()> {
        // ...
    }
    
    pub async fn uninstall_release(&self, name: &str, namespace: &str) -> Result<()> {
        // ...
    }
    
    pub async fn get_release(&self, name: &str, namespace: &str) -> Result<HelmRelease> {
        // ...
    }
    
    pub async fn list_releases(&self, namespace: Option<&str>) -> Result<Vec<HelmRelease>> {
        // ...
    }
    
    pub async fn create_hivemind_charts(&self) -> Result<Vec<PathBuf>> {
        // ...
    }
}
```

### HelmChart

The HelmChart represents a Helm chart:

```rust
pub struct HelmChart {
    pub name: String,
    pub version: String,
    pub description: String,
    pub app_version: String,
    pub keywords: Vec<String>,
    pub home: Option<String>,
    pub sources: Vec<String>,
    pub maintainers: Vec<HelmMaintainer>,
    pub icon: Option<String>,
    pub api_version: String,
    pub chart_type: Option<String>,
    pub dependencies: Vec<HelmDependency>,
    pub values: serde_yaml::Value,
}
```

### HelmRepository

The HelmRepository represents a Helm repository:

```rust
pub struct HelmRepository {
    pub name: String,
    pub url: String,
    pub username: Option<String>,
    pub password_encrypted: Option<String>,
    pub is_oci: bool,
}
```

### HelmRelease

The HelmRelease represents a Helm release:

```rust
pub struct HelmRelease {
    pub name: String,
    pub namespace: String,
    pub chart: String,
    pub version: String,
    pub values: serde_yaml::Value,
    pub status: HelmReleaseStatus,
    pub revision: u32,
    pub updated: chrono::DateTime<chrono::Utc>,
}

pub enum HelmReleaseStatus {
    Deployed,
    Pending,
    Failed,
    Superseded,
    Uninstalled,
}
```

## API Reference

### REST API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/helm/charts` | GET | List all charts |
| `/api/helm/charts` | POST | Create a new chart |
| `/api/helm/charts/{name}` | GET | Get a chart by name |
| `/api/helm/charts/{name}/package` | POST | Package a chart |
| `/api/helm/repositories` | GET | List all repositories |
| `/api/helm/repositories` | POST | Add a repository |
| `/api/helm/repositories/{name}` | DELETE | Remove a repository |
| `/api/helm/repositories/update` | POST | Update repositories |
| `/api/helm/releases` | GET | List all releases |
| `/api/helm/releases` | POST | Install a chart |
| `/api/helm/releases/{name}` | GET | Get a release by name |
| `/api/helm/releases/{name}` | PUT | Upgrade a release |
| `/api/helm/releases/{name}` | DELETE | Uninstall a release |
| `/api/helm/releases/{name}/history` | GET | Get release history |
| `/api/helm/releases/{name}/rollback` | POST | Roll back a release |

### CLI Reference

| Command | Description |
|---------|-------------|
| `hivemind helm create-chart` | Create a new chart |
| `hivemind helm package-chart` | Package a chart |
| `hivemind helm install` | Install a chart |
| `hivemind helm upgrade` | Upgrade a release |
| `hivemind helm uninstall` | Uninstall a release |
| `hivemind helm repo add` | Add a repository |
| `hivemind helm repo update` | Update repositories |
| `hivemind helm repo remove` | Remove a repository |
| `hivemind helm repo list` | List repositories |
| `hivemind helm get-release` | Get release details |
| `hivemind helm list-releases` | List releases |
| `hivemind helm history` | Get release history |
| `hivemind helm rollback` | Roll back a release |
| `hivemind helm update-dependencies` | Update chart dependencies |
| `hivemind helm create-hivemind-charts` | Create Hivemind charts |

## Example Workflows

### Deploying a Web Application with Helm

1. **Add the Bitnami repository**:
   ```bash
   hivemind helm repo add --name bitnami --url https://charts.bitnami.com/bitnami
   ```

2. **Update repositories**:
   ```bash
   hivemind helm repo update
   ```

3. **Create a values file**:
   ```bash
   cat > nginx-values.yaml << EOF
   replicaCount: 3
   service:
     type: LoadBalancer
   resources:
     limits:
       cpu: 100m
       memory: 128Mi
     requests:
       cpu: 50m
       memory: 64Mi
   EOF
   ```

4. **Install the Nginx chart**:
   ```bash
   hivemind helm install --name my-nginx --chart bitnami/nginx --namespace default --values nginx-values.yaml
   ```

5. **Check the release status**:
   ```bash
   hivemind helm get-release --name my-nginx --namespace default
   ```

6. **Upgrade the release**:
   ```bash
   cat > nginx-values-v2.yaml << EOF
   replicaCount: 5
   service:
     type: LoadBalancer
   resources:
     limits:
       cpu: 200m
       memory: 256Mi
     requests:
       cpu: 100m
       memory: 128Mi
   EOF
   
   hivemind helm upgrade --name my-nginx --chart bitnami/nginx --namespace default --values nginx-values-v2.yaml
   ```

7. **Roll back if needed**:
   ```bash
   hivemind helm rollback --name my-nginx --namespace default --revision 1
   ```

8. **Uninstall the release**:
   ```bash
   hivemind helm uninstall --name my-nginx --namespace default
   ```

### Creating and Deploying a Custom Chart

1. **Create a new chart**:
   ```bash
   hivemind helm create-chart --name my-app --description "My custom application"
   ```

2. **Customize the chart**:
   ```bash
   # Edit Chart.yaml
   cat > my-app/Chart.yaml << EOF
   apiVersion: v2
   name: my-app
   description: My custom application
   version: 0.1.0
   appVersion: "1.0.0"
   maintainers:
     - name: John Doe
       email: john@example.com
   EOF
   
   # Edit values.yaml
   cat > my-app/values.yaml << EOF
   replicaCount: 1
   
   image:
     repository: my-registry/my-app
     tag: latest
     pullPolicy: IfNotPresent
   
   service:
     type: ClusterIP
     port: 80
   
   resources:
     limits:
       cpu: 100m
       memory: 128Mi
     requests:
       cpu: 50m
       memory: 64Mi
   EOF
   
   # Edit deployment.yaml
   cat > my-app/templates/deployment.yaml << EOF
   apiVersion: apps/v1
   kind: Deployment
   metadata:
     name: {{ include "my-app.fullname" . }}
     labels:
       {{- include "my-app.labels" . | nindent 4 }}
   spec:
     replicas: {{ .Values.replicaCount }}
     selector:
       matchLabels:
         {{- include "my-app.selectorLabels" . | nindent 6 }}
     template:
       metadata:
         labels:
           {{- include "my-app.selectorLabels" . | nindent 8 }}
       spec:
         containers:
           - name: {{ .Chart.Name }}
             image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
             imagePullPolicy: {{ .Values.image.pullPolicy }}
             ports:
               - name: http
                 containerPort: 80
                 protocol: TCP
             resources:
               {{- toYaml .Values.resources | nindent 12 }}
   EOF
   ```

3. **Package the chart**:
   ```bash
   hivemind helm package-chart --chart-dir ./my-app
   ```

4. **Install the chart**:
   ```bash
   hivemind helm install --name my-release --chart ./my-app-0.1.0.tgz --namespace default
   ```

### Deploying Hivemind Components with Helm

1. **Create Hivemind charts**:
   ```bash
   hivemind helm create-hivemind-charts
   ```

2. **Install the Hivemind monitoring chart**:
   ```bash
   hivemind helm install --name hivemind-monitoring --chart hivemind-monitoring --namespace monitoring
   ```

3. **Customize the installation**:
   ```bash
   cat > monitoring-values.yaml << EOF
   prometheus:
     retention: 15d
     resources:
       limits:
         cpu: 1
         memory: 2Gi
   
   grafana:
     adminPassword: secure-password
     persistence:
       enabled: true
       size: 10Gi
   EOF
   
   hivemind helm install --name hivemind-monitoring --chart hivemind-monitoring --namespace monitoring --values monitoring-values.yaml
   ```

## Best Practices

- **Use version control** for your Helm charts
- **Parameterize your charts** with values files
- **Document your charts** with README files
- **Use semantic versioning** for chart versions
- **Test charts** before deployment
- **Use dependencies** for common components
- **Keep charts simple** and focused
- **Use templates** for repetitive resources
- **Validate charts** before deployment
- **Use hooks** for complex deployments

## Troubleshooting

### Common Issues

1. **Chart not found**:
   - Check repository configuration
   - Update repositories
   - Verify chart name and version

2. **Installation fails**:
   - Check values configuration
   - Verify resource availability
   - Check for syntax errors in templates

3. **Upgrade fails**:
   - Check for breaking changes
   - Verify compatibility with previous version
   - Check for resource conflicts

4. **Repository issues**:
   - Check network connectivity
   - Verify authentication credentials
   - Check for repository availability

### Debugging

To debug Helm issues:

```bash
hivemind helm debug --release <RELEASE_NAME> --namespace <NAMESPACE>
```

## Conclusion

Hivemind's Helm integration provides a powerful and flexible way to deploy and manage applications using Helm charts. By leveraging Helm's package management capabilities, Hivemind makes it easy to deploy complex applications with consistent configurations across different environments.