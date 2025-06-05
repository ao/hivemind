# Monitoring & Observability

This document provides comprehensive information about Hivemind's monitoring and observability capabilities, which allow you to gain insights into the performance and health of your container orchestration platform.

## Overview

Hivemind provides robust monitoring and observability features that enable you to collect metrics, traces, and logs from your containers, nodes, and the Hivemind platform itself. These features help you understand the behavior of your applications, troubleshoot issues, and optimize performance.

## Key Features

- **Metrics Collection**: Collect and expose metrics from containers, nodes, and Hivemind components
- **Prometheus Integration**: Native support for Prometheus metrics format
- **Distributed Tracing**: Track requests across services with OpenTelemetry
- **Log Aggregation**: Centralize logs from all components
- **Dashboards**: Pre-built dashboards for monitoring and visualization
- **Alerting**: Configure alerts based on metrics and logs
- **Health Monitoring**: Track the health of containers, nodes, and services
- **Performance Profiling**: Identify performance bottlenecks

## Architecture

The observability system is built around the following components:

- **ObservabilityManager**: Central component that manages observability features
- **MetricCollector**: Collects metrics from various components
- **OpenTelemetryTracer**: Provides distributed tracing capabilities
- **LogAggregator**: Aggregates logs from various components
- **PrometheusExporter**: Exposes metrics in Prometheus format

## Metrics Collection

Hivemind collects metrics from various components and exposes them in Prometheus format.

### Metric Types

Hivemind supports the following metric types:

- **Counter**: A cumulative metric that represents a single monotonically increasing counter
- **Gauge**: A metric that represents a single numerical value that can arbitrarily go up and down
- **Histogram**: A metric that samples observations and counts them in configurable buckets
- **Summary**: Similar to a histogram, but also calculates quantiles over a sliding time window

### Available Metrics

#### Node Metrics

- `hivemind_node_count`: Number of nodes in the cluster
- `hivemind_node_cpu_usage`: CPU usage per node
- `hivemind_node_memory_usage`: Memory usage per node
- `hivemind_node_disk_usage`: Disk usage per node
- `hivemind_node_network_rx`: Network receive bytes per node
- `hivemind_node_network_tx`: Network transmit bytes per node

#### Container Metrics

- `hivemind_container_count`: Number of containers managed by Hivemind
- `hivemind_container_status_count`: Number of containers by status
- `hivemind_container_cpu_usage`: CPU usage per container
- `hivemind_container_memory_usage`: Memory usage per container
- `hivemind_container_network_rx`: Network receive bytes per container
- `hivemind_container_network_tx`: Network transmit bytes per container
- `hivemind_container_disk_read`: Disk read bytes per container
- `hivemind_container_disk_write`: Disk write bytes per container

#### Health Metrics

- `hivemind_health_check_success_rate`: Success rate of health checks
- `hivemind_health_check_latency`: Latency of health checks
- `hivemind_health_check_failures`: Number of health check failures

#### Network Metrics

- `hivemind_network_traffic`: Network traffic between containers
- `hivemind_network_errors`: Network errors
- `hivemind_network_latency`: Network latency between containers

#### Scheduler Metrics

- `hivemind_scheduler_latency`: Scheduling latency
- `hivemind_scheduler_errors`: Scheduling errors
- `hivemind_scheduler_resource_utilization`: Resource utilization

### Prometheus Integration

Hivemind exposes metrics in Prometheus format at the `/metrics` endpoint. You can configure the port and path:

```bash
hivemind observability configure-metrics --port 9090 --path /metrics
```

### Grafana Dashboards

Hivemind provides pre-built Grafana dashboards for visualizing metrics:

- **Hivemind Overview**: High-level overview of the cluster
- **Node Monitoring**: Detailed metrics for each node
- **Container Monitoring**: Detailed metrics for each container
- **Network Monitoring**: Network metrics between containers
- **Health Monitoring**: Health check metrics

To import the dashboards into Grafana:

1. Open Grafana
2. Go to "+" > "Import"
3. Upload the dashboard JSON file from the `dashboards/` directory

## Distributed Tracing

Hivemind supports distributed tracing using OpenTelemetry, allowing you to track requests across services.

### Configuration

To configure OpenTelemetry tracing:

```bash
hivemind observability configure-tracing --service-name hivemind --endpoint http://jaeger:14268/api/traces
```

### Trace Context Propagation

Hivemind automatically propagates trace context between services using HTTP headers:

- `traceparent`: W3C Trace Context parent ID
- `tracestate`: W3C Trace Context state

### Span Attributes

Hivemind adds the following attributes to spans:

- `service.name`: Name of the service
- `service.version`: Version of the service
- `container.id`: ID of the container
- `node.id`: ID of the node
- `operation.name`: Name of the operation
- `error`: Error message (if applicable)

### Jaeger Integration

Hivemind can send traces to Jaeger for visualization and analysis:

```bash
hivemind observability configure-tracing --service-name hivemind --endpoint http://jaeger:14268/api/traces
```

## Log Aggregation

Hivemind aggregates logs from all components and can send them to a centralized logging system.

### Log Levels

Hivemind supports the following log levels:

- **Error**: Error conditions
- **Warning**: Warning conditions
- **Info**: Informational messages
- **Debug**: Debug-level messages
- **Trace**: Trace-level messages

### Log Format

Logs are formatted as JSON with the following fields:

- `timestamp`: Timestamp of the log entry
- `level`: Log level
- `message`: Log message
- `service`: Service that generated the log
- `container_id`: ID of the container (if applicable)
- `node_id`: ID of the node (if applicable)
- `trace_id`: Trace ID (if applicable)
- `span_id`: Span ID (if applicable)
- `metadata`: Additional metadata

### Elasticsearch Integration

Hivemind can send logs to Elasticsearch for storage and analysis:

```bash
hivemind observability configure-logging --endpoint http://elasticsearch:9200 --index hivemind-logs
```

### Kibana Dashboards

Hivemind provides pre-built Kibana dashboards for visualizing logs:

- **Hivemind Logs**: All logs from Hivemind components
- **Container Logs**: Logs from containers
- **Error Logs**: Error-level logs
- **Audit Logs**: Audit logs for security events

## Health Monitoring

Hivemind provides comprehensive health monitoring for containers, nodes, and services.

### Health Checks

Hivemind supports the following types of health checks:

- **HTTP**: Check if an HTTP endpoint returns a 2xx status code
- **TCP**: Check if a TCP port is open
- **Command**: Execute a command and check the exit code
- **gRPC**: Check if a gRPC service is healthy

### Health Status

Health status is categorized as:

- **Healthy**: Component is functioning normally
- **Unhealthy**: Component is not functioning normally
- **Unknown**: Health status cannot be determined

### Auto-Healing

Hivemind can automatically restart unhealthy containers:

```bash
hivemind health configure-auto-healing --enabled true --max-restarts 3 --restart-interval 60
```

## Alerting

Hivemind can generate alerts based on metrics and logs.

### Alert Rules

Alert rules are defined using a simple YAML format:

```yaml
name: high_cpu_usage
description: Alert when CPU usage is high
metric: hivemind_container_cpu_usage
threshold: 90
duration: 5m
severity: warning
```

### Alert Channels

Alerts can be sent to various channels:

- **Email**: Send alerts via email
- **Slack**: Send alerts to a Slack channel
- **Webhook**: Send alerts to a webhook endpoint
- **PagerDuty**: Send alerts to PagerDuty

### Alert Management

Alerts can be managed using the Hivemind CLI:

```bash
# Create an alert rule
hivemind health create-alert-rule --name high_cpu_usage --metric hivemind_container_cpu_usage --threshold 90 --duration 5m --severity warning

# List alert rules
hivemind health list-alert-rules

# Delete an alert rule
hivemind health delete-alert-rule --name high_cpu_usage

# Configure alert channels
hivemind health configure-alert-channel --type slack --webhook-url https://hooks.slack.com/services/XXX/YYY/ZZZ
```

## Performance Profiling

Hivemind provides tools for profiling the performance of containers and the platform itself.

### CPU Profiling

To profile CPU usage:

```bash
hivemind profile cpu --container <CONTAINER_ID> --duration 30s --output cpu.prof
```

### Memory Profiling

To profile memory usage:

```bash
hivemind profile memory --container <CONTAINER_ID> --output memory.prof
```

### Network Profiling

To profile network usage:

```bash
hivemind profile network --container <CONTAINER_ID> --duration 30s --output network.prof
```

### Analyzing Profiles

Profiles can be analyzed using standard tools like `pprof`:

```bash
go tool pprof -http=:8080 cpu.prof
```

## API Reference

### REST API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/metrics` | GET | Get all metrics |
| `/api/metrics/{name}` | GET | Get a specific metric |
| `/api/traces` | GET | List all traces |
| `/api/traces/{id}` | GET | Get a specific trace |
| `/api/logs` | GET | Query logs |
| `/api/health` | GET | Get health status |
| `/api/alerts` | GET | List all alerts |
| `/api/alerts/{id}` | GET | Get a specific alert |
| `/api/dashboards` | GET | List all dashboards |
| `/api/dashboards/{id}` | GET | Get a specific dashboard |

### CLI Reference

| Command | Description |
|---------|-------------|
| `hivemind observability configure-metrics` | Configure metrics collection |
| `hivemind observability configure-tracing` | Configure distributed tracing |
| `hivemind observability configure-logging` | Configure log aggregation |
| `hivemind observability open-dashboard` | Open the observability dashboard |
| `hivemind health status` | Show health status |
| `hivemind health configure-auto-healing` | Configure auto-healing |
| `hivemind health create-alert-rule` | Create an alert rule |
| `hivemind health list-alert-rules` | List alert rules |
| `hivemind health delete-alert-rule` | Delete an alert rule |
| `hivemind health configure-alert-channel` | Configure alert channels |
| `hivemind profile cpu` | Profile CPU usage |
| `hivemind profile memory` | Profile memory usage |
| `hivemind profile network` | Profile network usage |

## Example Setup

Here's an example of setting up a complete observability stack with Hivemind:

1. **Configure metrics collection**:
   ```bash
   hivemind observability configure-metrics --port 9090 --path /metrics
   ```

2. **Configure distributed tracing**:
   ```bash
   hivemind observability configure-tracing --service-name hivemind --endpoint http://jaeger:14268/api/traces
   ```

3. **Configure log aggregation**:
   ```bash
   hivemind observability configure-logging --endpoint http://elasticsearch:9200 --index hivemind-logs
   ```

4. **Deploy Prometheus**:
   ```bash
   hivemind app deploy --image prom/prometheus:v2.30.3 --name prometheus --volume prometheus-config:/etc/prometheus --port 9090:9090
   ```

5. **Deploy Grafana**:
   ```bash
   hivemind app deploy --image grafana/grafana:8.2.2 --name grafana --volume grafana-data:/var/lib/grafana --port 3000:3000
   ```

6. **Deploy Jaeger**:
   ```bash
   hivemind app deploy --image jaegertracing/all-in-one:1.27 --name jaeger --port 16686:16686 --port 14268:14268
   ```

7. **Deploy Elasticsearch and Kibana**:
   ```bash
   hivemind app deploy --image docker.elastic.co/elasticsearch/elasticsearch:7.15.1 --name elasticsearch --volume elasticsearch-data:/usr/share/elasticsearch/data --port 9200:9200
   hivemind app deploy --image docker.elastic.co/kibana/kibana:7.15.1 --name kibana --port 5601:5601
   ```

8. **Configure Prometheus**:
   ```bash
   cat > prometheus.yml << EOF
   global:
     scrape_interval: 15s
   scrape_configs:
     - job_name: 'hivemind'
       static_configs:
         - targets: ['localhost:9090']
   EOF
   
   hivemind volume create --name prometheus-config
   hivemind volume mount --name prometheus-config --path /etc/prometheus --container prometheus
   hivemind file copy --source prometheus.yml --destination /etc/prometheus/prometheus.yml --container prometheus
   ```

9. **Import Grafana dashboards**:
   ```bash
   hivemind observability import-dashboards --target grafana
   ```

10. **Configure auto-healing**:
    ```bash
    hivemind health configure-auto-healing --enabled true --max-restarts 3 --restart-interval 60
    ```

11. **Create alert rules**:
    ```bash
    hivemind health create-alert-rule --name high_cpu_usage --metric hivemind_container_cpu_usage --threshold 90 --duration 5m --severity warning
    hivemind health create-alert-rule --name high_memory_usage --metric hivemind_container_memory_usage --threshold 90 --duration 5m --severity warning
    hivemind health create-alert-rule --name node_down --metric hivemind_node_status --threshold 0 --duration 1m --severity critical
    ```

12. **Configure alert channels**:
    ```bash
    hivemind health configure-alert-channel --type slack --webhook-url https://hooks.slack.com/services/XXX/YYY/ZZZ
    hivemind health configure-alert-channel --type email --smtp-server smtp.example.com --smtp-port 587 --smtp-user user --smtp-password password --from alerts@example.com --to admin@example.com
    ```

## Best Practices

- **Start with basic metrics** and gradually add more as needed
- **Use labels** to add context to metrics
- **Set up alerts** for critical metrics
- **Correlate metrics, traces, and logs** for effective troubleshooting
- **Use dashboards** to visualize metrics and logs
- **Regularly review** and update your observability configuration
- **Implement proper retention policies** for metrics and logs
- **Use distributed tracing** for complex microservice architectures
- **Monitor both infrastructure and application** metrics
- **Automate remediation** for common issues

## Troubleshooting

### Common Issues

1. **Metrics not showing up in Prometheus**:
   - Check Prometheus configuration
   - Verify that the metrics endpoint is accessible
   - Check for firewall or network issues

2. **Traces not showing up in Jaeger**:
   - Check Jaeger configuration
   - Verify that the trace endpoint is accessible
   - Check for firewall or network issues

3. **Logs not showing up in Elasticsearch**:
   - Check Elasticsearch configuration
   - Verify that the log endpoint is accessible
   - Check for firewall or network issues

4. **High resource usage from monitoring**:
   - Reduce scrape frequency
   - Filter unnecessary metrics
   - Implement sampling for traces
   - Aggregate logs

### Debugging

To debug observability issues:

```bash
hivemind observability diagnose --component metrics
hivemind observability diagnose --component tracing
hivemind observability diagnose --component logging
```

## Conclusion

Hivemind's monitoring and observability features provide a comprehensive solution for gaining insights into the performance and health of your container orchestration platform. By leveraging these features, you can ensure the reliability and performance of your applications.