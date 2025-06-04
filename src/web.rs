use axum::{
    extract::{Path, State},
    http::StatusCode,
    response::{Html, IntoResponse, Response},
    routing::{get, post},
    Json, Router,
};
use serde::{Deserialize, Serialize};
use std::sync::Arc;
use tera::{Context, Tera};

use crate::containerd_manager::{Container, ContainerStatus};

// AppState is imported from main.rs
use crate::AppState;

// Templates directory
const TEMPLATES_DIR: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/templates/**/*");

pub struct WebServer {
    port: u16,
    templates: Arc<Tera>,
    state: Arc<AppState>,
}

impl WebServer {
    pub fn new(state: AppState, port: u16) -> Self {
        // Initialize Tera for templating
        let tera = match Tera::new(TEMPLATES_DIR) {
            Ok(t) => t,
            Err(e) => {
                eprintln!("Parsing error(s): {}", e);
                // Provide a fallback template engine with minimal templates
                let mut fallback = Tera::default();

                // Add a simple base template
                let base_html = r#"
                <!DOCTYPE html>
                <html>
                <head>
                    <meta charset="UTF-8">
                    <title>Hivemind - {{ title }}</title>
                    <style>
                        body { font-family: Arial, sans-serif; margin: 0; padding: 0; }
                        .container { width: 90%; margin: 0 auto; padding: 20px; }
                        .navbar { background-color: #2c3e50; color: white; padding: 1rem; margin-bottom: 2rem; }
                        .navbar a { color: white; text-decoration: none; margin-right: 1rem; }
                        .card { background: white; border-radius: 5px; box-shadow: 0 2px 5px rgba(0,0,0,0.1); padding: 20px; margin-bottom: 20px; }
                    </style>
                </head>
                <body>
                    <div class="navbar">
                        <div class="container">
                            <a href="/">Dashboard</a>
                            <a href="/nodes">Nodes</a>
                            <a href="/applications">Applications</a>
                            <a href="/containers">Containers</a>
                            <a href="/services">Services</a>
                            <a href="/health">Health</a>
                            <a href="/deploy">Deploy</a>
                        </div>
                    </div>
                    <div class="container">
                        {% block content %}{% endblock %}
                    </div>
                </body>
                </html>
                "#;

                // Add a simple dashboard template
                let dashboard_html = r#"
                {% extends "base.html" %}
                {% block content %}
                <h1>Hivemind Dashboard</h1>
                <div class="card">
                    <h2>System Overview</h2>
                    <p>Containers: {{ containers_count }}</p>
                    <p>Running containers: {{ running_containers }}</p>
                    <p>Nodes: {{ nodes_count }}</p>
                    <p>Services: {{ services_count }}</p>
                </div>
                {% endblock %}
                "#;

                // Add other simple templates
                let nodes_html = r#"
                {% extends "base.html" %}
                {% block content %}
                <h1>Nodes</h1>
                <div class="card">
                    <table>
                        <thead>
                            <tr>
                                <th>ID</th>
                                <th>Address</th>
                                <th>CPU Available</th>
                                <th>Memory Available</th>
                                <th>Containers Running</th>
                            </tr>
                        </thead>
                        <tbody>
                            {% for node in nodes %}
                            <tr>
                                <td>{{ node.id }}</td>
                                <td>{{ node.address }}</td>
                                <td>{{ node.cpu_available }}</td>
                                <td>{{ node.memory_available }}</td>
                                <td>{{ node.containers_running }}</td>
                            </tr>
                            {% endfor %}
                        </tbody>
                    </table>
                </div>
                {% endblock %}
                "#;

                let applications_html = r#"
                {% extends "base.html" %}
                {% block content %}
                <h1>Applications</h1>
                <div class="card">
                    <table>
                        <thead>
                            <tr>
                                <th>Name</th>
                                <th>Domain</th>
                                <th>Replicas</th>
                                <th>Actions</th>
                            </tr>
                        </thead>
                        <tbody>
                            {% for service in services %}
                            <tr>
                                <td>{{ service.name }}</td>
                                <td>{{ service.domain }}</td>
                                <td>{{ service.current_replicas }}/{{ service.desired_replicas }}</td>
                                <td>
                                    <a href="/scale/{{ service.name }}">Scale</a>
                                    <form method="post" action="/restart/{{ service.name }}" style="display: inline;">
                                        <button type="submit">Restart</button>
                                    </form>
                                </td>
                            </tr>
                            {% endfor %}
                        </tbody>
                    </table>
                </div>
                {% endblock %}
                "#;

                let containers_html = r#"
                {% extends "base.html" %}
                {% block content %}
                <h1>Containers</h1>
                <div class="card">
                    <table>
                        <thead>
                            <tr>
                                <th>ID</th>
                                <th>Name</th>
                                <th>Image</th>
                                <th>Status</th>
                                <th>Node</th>
                            </tr>
                        </thead>
                        <tbody>
                            {% for c in containers %}
                            <tr>
                                <td>{{ c.container.id }}</td>
                                <td>{{ c.container.name }}</td>
                                <td>{{ c.container.image }}</td>
                                <td>{{ c.container.status }}</td>
                                <td>{{ c.container.node_id }}</td>
                            </tr>
                            {% endfor %}
                        </tbody>
                    </table>
                </div>
                {% endblock %}
                "#;

                let services_html = r#"
                {% extends "base.html" %}
                {% block content %}
                <h1>Services</h1>
                <div class="card">
                    {% for service_name, endpoints in service_endpoints %}
                    <h2>{{ service_name }}</h2>
                    <table>
                        <thead>
                            <tr>
                                <th>Node</th>
                                <th>IP</th>
                                <th>Port</th>
                                <th>Health</th>
                            </tr>
                        </thead>
                        <tbody>
                            {% for endpoint in endpoints %}
                            <tr>
                                <td>{{ endpoint.node_id }}</td>
                                <td>{{ endpoint.ip_address }}</td>
                                <td>{{ endpoint.port }}</td>
                                <td>{{ endpoint.health_status }}</td>
                            </tr>
                            {% endfor %}
                        </tbody>
                    </table>
                    {% endfor %}
                </div>
                {% endblock %}
                "#;

                let health_html = r#"
                {% extends "base.html" %}
                {% block content %}
                <h1>System Health</h1>
                <div class="card">
                    <h2>Node Health: {{ node_health }}</h2>
                    <h2>Containers</h2>
                    <table>
                        <thead>
                            <tr>
                                <th>Name</th>
                                <th>Status</th>
                                <th>Health</th>
                            </tr>
                        </thead>
                        <tbody>
                            {% for container in containers %}
                            <tr>
                                <td>{{ container.name }}</td>
                                <td>{{ container.status }}</td>
                                <td>{% if container.status == "Running" %}Healthy{% else %}Unhealthy{% endif %}</td>
                            </tr>
                            {% endfor %}
                        </tbody>
                    </table>
                </div>
                {% endblock %}
                "#;

                let deploy_html = r#"
                {% extends "base.html" %}
                {% block content %}
                <h1>Deploy Application</h1>
                <div class="card">
                    <form method="post" action="/deploy">
                        <div class="form-group">
                            <label for="name">Name</label>
                            <input type="text" id="name" name="name" required>
                        </div>
                        <div class="form-group">
                            <label for="image">Image</label>
                            <select id="image" name="image">
                                {% for image in images %}
                                <option value="{{ image }}">{{ image }}</option>
                                {% endfor %}
                            </select>
                        </div>
                        <div class="form-group">
                            <label for="service">Service Domain (optional)</label>
                            <input type="text" id="service" name="service">
                        </div>
                        <button type="submit">Deploy</button>
                    </form>
                </div>
                {% endblock %}
                "#;

                let scale_html = r#"
                {% extends "base.html" %}
                {% block content %}
                <h1>Scale Application: {{ app_name }}</h1>
                <div class="card">
                    <form method="post" action="/scale/{{ app_name }}">
                        <div class="form-group">
                            <label for="replicas">Replicas</label>
                            <input type="number" id="replicas" name="replicas" value="{{ service.current_replicas }}" min="1" required>
                        </div>
                        <button type="submit">Scale</button>
                    </form>
                </div>
                {% endblock %}
                "#;

                let error_html = r#"
                {% extends "base.html" %}
                {% block content %}
                <h1>Error</h1>
                <div class="card">
                    <p>{{ error }}</p>
                    <a href="{{ back_url }}">Go Back</a>
                </div>
                {% endblock %}
                "#;

                // Add all templates
                let templates = [
                    ("base.html", base_html),
                    ("dashboard.html", dashboard_html),
                    ("nodes.html", nodes_html),
                    ("applications.html", applications_html),
                    ("containers.html", containers_html),
                    ("services.html", services_html),
                    ("health.html", health_html),
                    ("deploy.html", deploy_html),
                    ("scale.html", scale_html),
                    ("error.html", error_html),
                ];

                for (name, content) in templates {
                    // If adding a template fails, just log the error but continue
                    if let Err(e) = fallback.add_raw_template(name, content) {
                        eprintln!("Failed to add fallback template {}: {}", name, e);
                    }
                }

                fallback
            }
        };

        Self {
            port,
            templates: Arc::new(tera),
            state: Arc::new(state),
        }
    }

    pub async fn start(&self) -> anyhow::Result<()> {
        let templates = self.templates.clone();
        let state = self.state.clone();

        // Create router with all routes
        let app = Router::new()
            // HTML routes
            .route("/", get(dashboard_handler))
            .route("/nodes", get(nodes_handler))
            .route("/applications", get(applications_handler))
            .route("/containers", get(containers_handler))
            .route("/services", get(services_handler))
            .route("/health", get(health_handler))
            .route("/deploy", get(deploy_form_handler).post(deploy_handler))
            .route(
                "/scale/:app_name",
                get(scale_form_handler).post(scale_handler),
            )
            .route("/restart/:app_name", post(restart_handler))
            // API routes
            .route("/api/nodes", get(api_nodes_handler))
            .route("/api/containers", get(api_containers_handler))
            .route("/api/images", get(api_images_handler))
            .route("/api/services", get(api_services_handler))
            .route("/api/service-endpoints", get(api_service_endpoints_handler))
            .route("/api/health", get(api_health_handler))
            .route("/api/deploy", post(api_deploy_handler))
            .route("/api/scale", post(api_scale_handler))
            .route("/api/restart", post(api_restart_handler))
            .route("/api/service-url", post(api_service_url_handler))
            // Volume management API routes
            .route("/volumes", get(volumes_handler))
            .route("/api/volumes", get(api_list_volumes))
            .route("/api/volumes/create", post(api_create_volume))
            .route("/api/volumes/delete", post(api_delete_volume))
            // Static assets
            .route("/assets/*path", get(static_handler))
            // With shared state
            .with_state(WebServerState {
                templates,
                app_state: state,
            });

        // Start the web server
        println!("Starting web server on port {}", self.port);
        let listener = tokio::net::TcpListener::bind(format!("0.0.0.0:{}", self.port))
            .await
            .expect("Failed to bind to port");

        axum::serve(listener, app).await?;

        Ok(())
    }
}

#[derive(Serialize)]
struct VolumeResponse {
    name: String,
    created_at: i64,
    size: u64,
}

#[derive(Clone)]
struct WebServerState {
    templates: Arc<Tera>,
    app_state: Arc<AppState>,
}

async fn api_list_volumes(State(state): State<WebServerState>) -> Response {
    let app_state = &state.app_state;

    match app_state.app_manager.list_volumes().await {
        Ok(volumes) => {
            let response: Vec<VolumeResponse> = volumes
                .into_iter()
                .map(|v| VolumeResponse {
                    name: v.name,
                    created_at: v.created_at,
                    size: v.size,
                })
                .collect();

            (StatusCode::OK, Json(response)).into_response()
        }
        Err(e) => (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(serde_json::json!({
                "error": format!("Failed to list volumes: {}", e)
            })),
        )
            .into_response(),
    }
}

#[derive(Deserialize)]
struct CreateVolumeRequest {
    name: String,
}

async fn api_create_volume(
    State(state): State<WebServerState>,
    Json(payload): Json<CreateVolumeRequest>,
) -> impl IntoResponse {
    let app_state = &state.app_state;

    match app_state.app_manager.create_volume(&payload.name).await {
        Ok(_) => (
            StatusCode::CREATED,
            Json(serde_json::json!({
                "success": true,
                "message": format!("Volume {} created", payload.name)
            })),
        ),
        Err(e) => (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(serde_json::json!({
                "error": format!("Failed to create volume: {}", e)
            })),
        ),
    }
}

#[derive(Deserialize)]
struct DeleteVolumeRequest {
    name: String,
}

async fn api_delete_volume(
    State(state): State<WebServerState>,
    Json(payload): Json<DeleteVolumeRequest>,
) -> impl IntoResponse {
    let app_state = &state.app_state;

    match app_state.app_manager.delete_volume(&payload.name).await {
        Ok(_) => (
            StatusCode::OK,
            Json(serde_json::json!({
                "success": true,
                "message": format!("Volume {} deleted", payload.name)
            })),
        ),
        Err(e) => (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(serde_json::json!({
                "error": format!("Failed to delete volume: {}", e)
            })),
        ),
    }
}

async fn volumes_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;

    // Get volumes
    let volumes = app_state
        .app_manager
        .list_volumes()
        .await
        .unwrap_or_default();

    let mut context = Context::new();
    context.insert("volumes", &volumes);

    render_template(&state.templates, "volumes.html", &context)
}

// Static file handler
async fn static_handler(Path(path): Path<String>) -> impl IntoResponse {
    // In a real implementation, we would serve files from an assets directory
    // For this example, we'll return mock CSS content
    if path.ends_with(".css") {
        Response::builder()
            .status(StatusCode::OK)
            .header("Content-Type", "text/css")
            .body(axum::body::Body::from(
                r#"
                body {
                    font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
                    line-height: 1.6;
                    color: #333;
                    margin: 0;
                    padding: 0;
                    background-color: #f5f7fa;
                }
                .container {
                    width: 90%;
                    max-width: 1200px;
                    margin: 0 auto;
                    padding: 20px;
                }
                .navbar {
                    background-color: #2c3e50;
                    color: white;
                    padding: 1rem;
                    margin-bottom: 2rem;
                }
                .navbar a {
                    color: white;
                    text-decoration: none;
                    margin-right: 1rem;
                }
                .navbar a:hover {
                    text-decoration: underline;
                }
                .card {
                    background: white;
                    border-radius: 5px;
                    box-shadow: 0 2px 5px rgba(0,0,0,0.1);
                    padding: 20px;
                    margin-bottom: 20px;
                }
                .btn {
                    display: inline-block;
                    background: #3498db;
                    color: white;
                    padding: 0.5rem 1rem;
                    border: none;
                    border-radius: 3px;
                    cursor: pointer;
                    text-decoration: none;
                }
                .btn:hover {
                    background: #2980b9;
                }
                .btn-danger {
                    background: #e74c3c;
                }
                .btn-danger:hover {
                    background: #c0392b;
                }
                .btn-success {
                    background: #2ecc71;
                }
                .btn-success:hover {
                    background: #27ae60;
                }
                table {
                    width: 100%;
                    border-collapse: collapse;
                }
                table, th, td {
                    border: 1px solid #ddd;
                }
                th, td {
                    padding: 12px;
                    text-align: left;
                }
                th {
                    background-color: #f2f2f2;
                }
                tr:nth-child(even) {
                    background-color: #f9f9f9;
                }
                .status-running {
                    color: #2ecc71;
                }
                .status-failed {
                    color: #e74c3c;
                }
                .status-pending {
                    color: #f39c12;
                }
                .form-group {
                    margin-bottom: 1rem;
                }
                label {
                    display: block;
                    margin-bottom: 0.5rem;
                }
                input, select {
                    width: 100%;
                    padding: 8px;
                    border: 1px solid #ddd;
                    border-radius: 4px;
                }
                "#,
            ))
            .unwrap()
    } else if path.ends_with(".js") {
        Response::builder()
            .status(StatusCode::OK)
            .header("Content-Type", "application/javascript")
            .body(
                r#"
                // Dashboard refresh
                function refreshData() {
                    const refreshElements = document.querySelectorAll('[data-refresh-url]');
                    refreshElements.forEach(el => {
                        fetch(el.dataset.refreshUrl)
                            .then(response => response.json())
                            .then(data => {
                                if (el.dataset.refreshTemplate === 'table') {
                                    refreshTable(el, data);
                                } else if (el.dataset.refreshTemplate === 'count') {
                                    el.textContent = data.length || 0;
                                }
                            });
                    });
                }

                function refreshTable(tableEl, data) {
                    const tbody = tableEl.querySelector('tbody');
                    const template = tableEl.dataset.refreshRowTemplate;

                    if (!tbody || !template) return;

                    let html = '';
                    data.forEach(item => {
                        let row = template;
                        Object.keys(item).forEach(key => {
                            row = row.replace(new RegExp(`\\{\\{${key}\\}\\}`, 'g'), item[key]);
                        });
                        html += row;
                    });

                    tbody.innerHTML = html;
                }

                // Set up periodic refresh
                if (document.querySelector('[data-refresh-url]')) {
                    refreshData();
                    setInterval(refreshData, 5000);
                }
                "#
                .into(),
            )
            .unwrap()
    } else if path == "logo1.png" {
        // Return a placeholder image
        Response::builder()
            .status(StatusCode::OK)
            .header("Content-Type", "image/png")
            .body(Vec::new().into()) // Empty body as a placeholder
            .unwrap()
    } else {
        Response::builder()
            .status(StatusCode::NOT_FOUND)
            .body("File not found".into())
            .unwrap()
    }
}

// HTML page handlers
async fn dashboard_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;

    // Get data for dashboard
    let containers = app_state
        .app_manager
        .get_container_details()
        .await
        .unwrap_or_default();
    let nodes = app_state
        .node_manager
        .list_nodes()
        .await
        .unwrap_or_default();
    let services = app_state
        .app_manager
        .list_services()
        .await
        .unwrap_or_default();

    // Calculate statistics
    let running_containers = containers
        .iter()
        .filter(|c| c.status == ContainerStatus::Running)
        .count();

    let mut context = Context::new();
    context.insert("containers_count", &containers.len());
    context.insert("running_containers", &running_containers);
    context.insert("nodes_count", &nodes.len());
    context.insert("services_count", &services.len());
    context.insert("containers", &containers);
    context.insert(
        "recent_containers",
        &containers.iter().take(5).collect::<Vec<_>>(),
    );
    context.insert("services", &services);

    render_template(&state.templates, "dashboard.html", &context)
}

async fn nodes_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;

    // Get nodes data
    let node_details = app_state
        .node_manager
        .get_node_details()
        .await
        .unwrap_or_default();

    // Get containers per node
    let mut nodes_with_containers = Vec::new();
    for (node_id, address, resources) in &node_details {
        let containers = app_state.app_manager.get_containers_by_node(node_id).await;
        nodes_with_containers.push(NodeWithContainers {
            id: node_id.clone(),
            address: address.clone(),
            cpu_available: resources.cpu_available,
            memory_available: resources.memory_available,
            containers_running: containers
                .iter()
                .filter(|c| c.status == ContainerStatus::Running)
                .count(),
            containers: containers,
        });
    }

    let mut context = Context::new();
    context.insert("nodes", &nodes_with_containers);

    render_template(&state.templates, "nodes.html", &context)
}

#[derive(Serialize)]
struct NodeWithContainers {
    id: String,
    address: String,
    cpu_available: f64,
    memory_available: u64,
    containers_running: usize,
    containers: Vec<Container>,
}

async fn applications_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;

    // Get services data
    let services = app_state
        .app_manager
        .list_services()
        .await
        .unwrap_or_default();

    // Get containers for each service
    let mut services_with_details = Vec::new();
    for service in services {
        let mut containers = Vec::new();
        for container_id in &service.container_ids {
            if let Some(container) = app_state
                .app_manager
                .get_container_by_id(container_id)
                .await
            {
                containers.push(container);
            }
        }

        services_with_details.push(ServiceWithContainers {
            name: service.name,
            domain: service.domain,
            desired_replicas: service.desired_replicas,
            current_replicas: service.current_replicas,
            containers,
        });
    }

    let mut context = Context::new();
    context.insert("services", &services_with_details);

    render_template(&state.templates, "applications.html", &context)
}

#[derive(Serialize)]
struct ServiceWithContainers {
    name: String,
    domain: String,
    desired_replicas: u32,
    current_replicas: u32,
    containers: Vec<Container>,
}

async fn containers_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;

    // Get containers data
    let containers = app_state
        .app_manager
        .get_container_details()
        .await
        .unwrap_or_default();

    // Get container stats
    let mut containers_with_stats = Vec::new();
    for container in containers {
        let stats = app_state
            .app_manager
            .get_container_stats(&container.id)
            .await;
        containers_with_stats.push(ContainerWithStats { container, stats });
    }

    let mut context = Context::new();
    context.insert("containers", &containers_with_stats);

    render_template(&state.templates, "containers.html", &context)
}

#[derive(Serialize)]
struct ContainerWithStats {
    container: Container,
    stats: Option<crate::containerd_manager::ContainerStats>,
}

async fn services_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;

    // Get services data
    let service_endpoints = app_state.service_discovery.list_services().await;

    let mut context = Context::new();
    context.insert("service_endpoints", &service_endpoints);

    render_template(&state.templates, "services.html", &context)
}

async fn health_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;

    // Get health summary from health monitor if available
    let health_summary = if let Some(health_monitor) = &app_state.health_monitor {
        Some(health_monitor.get_health_summary().await)
    } else {
        None
    };

    // Get detailed container health if health monitor is available
    let container_health = if let Some(health_monitor) = &app_state.health_monitor {
        health_monitor.get_all_container_health().await
    } else {
        std::collections::HashMap::new()
    };

    // Get detailed node health if health monitor is available
    let node_health = if let Some(health_monitor) = &app_state.health_monitor {
        health_monitor.get_all_node_health().await
    } else {
        std::collections::HashMap::new()
    };

    // Fallback to basic health check
    let basic_node_health = app_state.node_manager.check_health().await.unwrap_or(false);

    // Get containers and their basic status
    let containers = app_state
        .app_manager
        .get_container_details()
        .await
        .unwrap_or_default();

    let mut context = Context::new();
    context.insert("health_summary", &health_summary);
    context.insert("container_health", &container_health);
    context.insert("node_health", &node_health);
    context.insert("basic_node_health", &basic_node_health);
    context.insert("containers", &containers);
    context.insert("has_health_monitor", &app_state.health_monitor.is_some());

    render_template(&state.templates, "health.html", &context)
}

async fn deploy_form_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;

    // Get available images
    let images = app_state
        .app_manager
        .list_images()
        .await
        .unwrap_or_default();

    // Get available volumes
    let volumes = app_state
        .app_manager
        .list_volumes()
        .await
        .unwrap_or_default();

    let mut context = Context::new();
    context.insert("images", &images);
    context.insert("volumes", &volumes);

    render_template(&state.templates, "deploy.html", &context)
}

async fn deploy_handler(
    State(state): State<WebServerState>,
    axum::Form(params): axum::Form<DeployFormParams>,
) -> impl IntoResponse {
    let app_state = &state.app_state;

    // Deploy the application
    let result = app_state
        .app_manager
        .deploy_app(
            &params.image,
            &params.name,
            params.service.as_deref(),
            None,
            None,
        )
        .await;

    match result {
        Ok(container_id) => {
            // Redirect to applications page with success message
            let url = format!(
                "/applications?success=Deployed application {} with container {}",
                params.name, container_id
            );
            axum::response::Redirect::to(&url).into_response()
        }
        Err(e) => {
            // Show error page
            let mut context = Context::new();
            context.insert("error", &format!("Failed to deploy application: {}", e));
            context.insert("back_url", &"/deploy");
            render_template(&state.templates, "error.html", &context).into_response()
        }
    }
}

#[derive(Deserialize)]
struct DeployFormParams {
    image: String,
    name: String,
    service: Option<String>,
}

async fn scale_form_handler(
    State(state): State<WebServerState>,
    Path(app_name): Path<String>,
) -> impl IntoResponse {
    let app_state = &state.app_state;

    // Get the service
    let service = app_state.app_manager.get_service(&app_name).await;

    let mut context = Context::new();
    context.insert("service", &service);
    context.insert("app_name", &app_name);

    render_template(&state.templates, "scale.html", &context)
}

async fn scale_handler(
    State(state): State<WebServerState>,
    Path(app_name): Path<String>,
    axum::Form(params): axum::Form<ScaleFormParams>,
) -> impl IntoResponse {
    let app_state = &state.app_state;

    // Scale the application
    let result = app_state
        .app_manager
        .scale_app(&app_name, params.replicas)
        .await;

    match result {
        Ok(_) => {
            // Redirect to applications page with success message
            let url = format!(
                "/applications?success=Scaled application {} to {} replicas",
                app_name, params.replicas
            );
            axum::response::Redirect::to(&url).into_response()
        }
        Err(e) => {
            // Show error page
            let mut context = Context::new();
            context.insert("error", &format!("Failed to scale application: {}", e));
            context.insert("back_url", &"/applications");
            render_template(&state.templates, "error.html", &context).into_response()
        }
    }
}

#[derive(Deserialize)]
struct ScaleFormParams {
    replicas: u32,
}

async fn restart_handler(
    State(state): State<WebServerState>,
    Path(app_name): Path<String>,
) -> impl IntoResponse {
    let app_state = &state.app_state;

    // Restart the application
    let result = app_state.app_manager.restart_app(&app_name).await;

    match result {
        Ok(_) => {
            // Redirect to applications page with success message
            let url = format!("/applications?success=Restarted application {}", app_name);
            axum::response::Redirect::to(&url).into_response()
        }
        Err(e) => {
            // Show error page
            let mut context = Context::new();
            context.insert("error", &format!("Failed to restart application: {}", e));
            context.insert("back_url", &"/applications");
            render_template(&state.templates, "error.html", &context).into_response()
        }
    }
}

// API handlers (JSON responses)
async fn api_nodes_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;
    let nodes = app_state
        .node_manager
        .get_node_details()
        .await
        .unwrap_or_default();

    let nodes_json: Vec<serde_json::Value> = nodes
        .into_iter()
        .map(|(id, address, resources)| {
            serde_json::json!({
                "id": id,
                "address": address,
                "cpu_available": resources.cpu_available,
                "memory_available": resources.memory_available,
                "containers_running": resources.containers_running
            })
        })
        .collect();

    Json(nodes_json)
}

async fn api_containers_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;
    let containers = app_state
        .app_manager
        .get_container_details()
        .await
        .unwrap_or_default();
    Json(containers)
}

async fn api_images_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;
    let images = app_state
        .app_manager
        .list_images()
        .await
        .unwrap_or_default();
    Json(images)
}

async fn api_services_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;
    let services = app_state
        .app_manager
        .list_services()
        .await
        .unwrap_or_default();
    Json(services)
}

async fn api_service_endpoints_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;
    let endpoints = app_state.service_discovery.list_services().await;
    Json(endpoints)
}

async fn api_health_handler(State(state): State<WebServerState>) -> impl IntoResponse {
    let app_state = &state.app_state;

    // Get comprehensive health data from health monitor if available
    if let Some(health_monitor) = &app_state.health_monitor {
        let health_summary = health_monitor.get_health_summary().await;
        let container_health = health_monitor.get_all_container_health().await;
        let node_health = health_monitor.get_all_node_health().await;

        Json(serde_json::json!({
            "status": "comprehensive",
            "summary": health_summary,
            "container_health": container_health,
            "node_health": node_health,
            "overall_health": health_summary.healthy_containers as f64 / health_summary.total_containers.max(1) as f64 > 0.8
        }))
    } else {
        // Fallback to basic health check
        let node_health = app_state.node_manager.check_health().await.unwrap_or(false);
        
        let containers = app_state
            .app_manager
            .get_container_details()
            .await
            .unwrap_or_default();
        let container_health = containers
            .iter()
            .filter(|c| c.status == ContainerStatus::Running)
            .count() as f64
            / containers.len().max(1) as f64;

        Json(serde_json::json!({
            "status": "basic",
            "node_health": node_health,
            "container_health": container_health,
            "overall_health": node_health && container_health > 0.8
        }))
    }
}

// Use DeployRequest from main.rs
async fn api_deploy_handler(
    State(state): State<WebServerState>,
    Json(payload): Json<crate::DeployRequest>,
) -> impl IntoResponse {
    let app_state = &state.app_state;
    let result = app_state
        .app_manager
        .deploy_app(
            &payload.image,
            &payload.name,
            payload.service.as_deref(),
            None,
            None,
        )
        .await;

    match result {
        Ok(container_id) => (
            StatusCode::OK,
            Json(serde_json::json!({
                "success": true,
                "container_id": container_id,
                "message": format!("Deployed application {} with container {}", payload.name, container_id)
            })),
        ),
        Err(e) => (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(serde_json::json!({
                "success": false,
                "error": e.to_string()
            })),
        ),
    }
}

#[derive(Deserialize)]
struct ScaleRequest {
    name: String,
    replicas: u32,
}

async fn api_scale_handler(
    State(state): State<WebServerState>,
    Json(payload): Json<ScaleRequest>,
) -> impl IntoResponse {
    let app_state = &state.app_state;
    let result = app_state
        .app_manager
        .scale_app(&payload.name, payload.replicas)
        .await;

    match result {
        Ok(_) => (
            StatusCode::OK,
            Json(serde_json::json!({
                "success": true,
                "message": format!("Scaled application {} to {} replicas", payload.name, payload.replicas)
            })),
        ),
        Err(e) => (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(serde_json::json!({
                "success": false,
                "error": e.to_string()
            })),
        ),
    }
}

#[derive(Deserialize)]
struct RestartRequest {
    name: String,
}

async fn api_restart_handler(
    State(state): State<WebServerState>,
    Json(payload): Json<RestartRequest>,
) -> impl IntoResponse {
    let app_state = &state.app_state;
    let result = app_state.app_manager.restart_app(&payload.name).await;

    match result {
        Ok(_) => (
            StatusCode::OK,
            Json(serde_json::json!({
                "success": true,
                "message": format!("Restarted application {}", payload.name)
            })),
        ),
        Err(e) => (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(serde_json::json!({
                "success": false,
                "error": e.to_string()
            })),
        ),
    }
}

async fn api_service_url_handler(
    State(state): State<WebServerState>,
    Json(payload): Json<crate::ServiceUrlRequest>,
) -> impl IntoResponse {
    let app_state = &state.app_state;
    let url = app_state
        .service_discovery
        .get_service_url(&payload.service_name)
        .await;

    match url {
        Some(url) => (
            StatusCode::OK,
            Json(serde_json::json!({
                "success": true,
                "url": url
            })),
        ),
        None => (
            StatusCode::NOT_FOUND,
            Json(serde_json::json!({
                "success": false,
                "error": "Service not found or no healthy endpoints available"
            })),
        ),
    }
}

// Helper function to render a template
fn render_template(tera: &Tera, template_name: &str, context: &Context) -> Html<String> {
    match tera.render(template_name, context) {
        Ok(html) => Html(html),
        Err(e) => {
            eprintln!("Template error: {}", e);
            Html(format!(
                "<html><body><h1>Template Error</h1><p>{}</p></body></html>",
                e
            ))
        }
    }
}
