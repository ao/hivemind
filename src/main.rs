use anyhow::Result;
use axum::{extract::State, Json};
use clap::{Parser, Subcommand};
use std::path::PathBuf;

// Use types from lib.rs
use hivemind::{AppState, DeployRequest, DeployResponse, ServiceUrlRequest, ServiceUrlResponse};
use hivemind::app::{AppManager, ServiceConfig};
use hivemind::containerd_manager::Container;
use hivemind::health_monitor::HealthMonitor;
use hivemind::node::NodeManager;
use hivemind::network::NetworkManager;
use hivemind::cicd::CicdManager;
use hivemind::cloud::CloudManager;
use hivemind::deployment::DeploymentManager;
use hivemind::helm::HelmManager;
use hivemind::observability::ObservabilityManager;
use hivemind::scheduler::ContainerScheduler;
use hivemind::security::SecurityManager;
use hivemind::service_discovery::{ServiceDiscovery, ServiceEndpoint};
use hivemind::storage::StorageManager;
use hivemind::tenant::TenantManager;
use hivemind::tenant_quota::TenantQuotaEnforcer;
use hivemind::web;
use std::path::PathBuf;
use std::sync::Arc;

#[derive(Parser)]
#[command(author, version, about, long_about = None)]
struct Cli {
    #[arg(long, default_value = "~/.hivemind")]
    data_dir: PathBuf,
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Start Hivemind in daemon mode
    Daemon {
        #[arg(long, default_value = "3000")]
        web_port: u16,
    },
    /// Join an existing Hivemind cluster
    Join {
        #[arg(long)]
        host: String,
    },
    /// List and manage nodes
    Node {
        #[command(subcommand)]
        command: NodeCommands,
    },
    /// Manage applications
    App {
        #[command(subcommand)]
        command: AppCommands,
    },
    /// Check system health
    Health,
    /// Manage volumes
    Volume {
        #[command(subcommand)]
        command: VolumeCommands,
    },
    // Start only the web interface
    Web {
        #[arg(long, default_value = "3000")]
        port: u16,
    },
}

#[derive(Subcommand)]
enum NodeCommands {
    /// List all nodes in the cluster
    Ls,
    /// Show detailed information about nodes
    Info,
}

#[derive(Subcommand)]
enum VolumeCommands {
    /// Create a new volume
    Create {
        #[arg(long)]
        name: String,
    },
    /// List all volumes
    Ls,
    /// Delete a volume
    Delete {
        #[arg(long)]
        name: String,
    },
    /// Backup a volume to a file
    Backup {
        #[arg(long)]
        name: String,
        #[arg(long)]
        output: String,
    },
    /// Restore a volume from a backup file
    Restore {
        #[arg(long)]
        name: String,
        #[arg(long)]
        input: String,
    },
}

#[derive(Subcommand)]
enum AppCommands {
    /// List all applications
    Ls,
    /// Deploy a new application
    Deploy {
        #[arg(long)]
        image: String,
        #[arg(long)]
        name: String,
        #[arg(long)]
        service: Option<String>,
        #[arg(long)]
        volume: Option<Vec<String>>,
    },
    /// Deploy a new application with zero-downtime
    ZeroDowntimeDeploy {
        #[arg(long)]
        image: String,
        #[arg(long)]
        name: String,
        #[arg(long)]
        service: Option<String>,
        #[arg(long)]
        batch_size: Option<u32>,
        #[arg(long)]
        batch_delay: Option<u64>,
        #[arg(long)]
        health_check_path: Option<String>,
        #[arg(long)]
        health_check_port: Option<u16>,
        #[arg(long)]
        health_check_timeout: Option<u64>,
        #[arg(long)]
        drain_timeout: Option<u64>,
    },
    /// Get the status of a deployment
    DeploymentStatus {
        #[arg(long)]
        id: String,
    },
    /// Rollback a deployment
    RollbackDeployment {
        #[arg(long)]
        id: String,
    },
    /// Scale an application
    Scale {
        #[arg(long)]
        name: String,
        #[arg(long)]
        replicas: u32,
    },
    /// List all containers for all applications
    Containers,
    /// Show detailed container information
    ContainerInfo {
        #[arg(long)]
        container_id: String,
    },
    /// Restart an application
    Restart {
        #[arg(long)]
        name: String,
    },
}


async fn list_images(State(state): State<AppState>) -> Json<Vec<String>> {
    Json(state.app_manager.list_images().await.unwrap_or_default())
}

async fn list_nodes(State(state): State<AppState>) -> Json<Vec<String>> {
    Json(state.node_manager.list_nodes().await.unwrap_or_default())
}

async fn get_container_details(State(state): State<AppState>) -> Json<Vec<Container>> {
    Json(
        state
            .app_manager
            .get_container_details()
            .await
            .unwrap_or_default(),
    )
}

async fn list_services(State(state): State<AppState>) -> Json<Vec<ServiceConfig>> {
    Json(state.app_manager.list_services().await.unwrap_or_default())
}

async fn list_service_endpoints(
    State(state): State<AppState>,
) -> Json<std::collections::HashMap<String, Vec<ServiceEndpoint>>> {
    Json(state.service_discovery.list_services().await)
}

async fn get_service_url(
    State(state): State<AppState>,
    Json(payload): Json<ServiceUrlRequest>,
) -> Json<ServiceUrlResponse> {
    match state
        .service_discovery
        .get_service_url(&payload.service_name)
        .await
    {
        Some(url) => Json(ServiceUrlResponse {
            success: true,
            url: Some(url),
            error: None,
        }),
        None => Json(ServiceUrlResponse {
            success: false,
            url: None,
            error: Some("Service not found or no healthy endpoints available".to_string()),
        }),
    }
}

async fn deploy_container(
    State(state): State<AppState>,
    Json(payload): Json<DeployRequest>,
) -> Json<DeployResponse> {
    match state
        .app_manager
        .deploy_app(
            &payload.image,
            &payload.name,
            payload.service.as_deref(),
            None,
            None,
        )
        .await
    {
        Ok(container_id) => Json(DeployResponse {
            success: true,
            container_id: Some(container_id),
            error: None,
        }),
        Err(e) => Json(DeployResponse {
            success: false,
            container_id: None,
            error: Some(e.to_string()),
        }),
    }
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();

    match cli.command {
        Commands::Daemon { web_port } => {
            // Default containerd socket path
            let containerd_socket = "/run/containerd/containerd.sock";
            let containerd_namespace = "hivemind";

            let storage = StorageManager::new(&cli.data_dir).await?;
            let node_manager = NodeManager::with_storage(storage.clone()).await;
            let service_discovery = ServiceDiscovery::new();

            // Initialize AppManager with containerd
            let app_manager = match AppManager::with_containerd(
                storage.clone(),
                containerd_socket,
                containerd_namespace,
            )
            .await
            {
                Ok(manager) => {
                    println!("Successfully connected to containerd");
                    manager.with_service_discovery(service_discovery.clone())
                }
                Err(e) => {
                    eprintln!("Failed to connect to containerd: {}", e);
                    eprintln!("Falling back to mock implementation");
                    AppManager::with_storage(storage)
                        .await?
                        .with_service_discovery(service_discovery.clone())
                }
            };

            // Initialize and start the membership protocol
            let mut node_manager_mut = node_manager.clone();
            if let Err(e) = node_manager_mut.init_membership_protocol().await {
                eprintln!("Failed to initialize membership protocol: {}", e);
                eprintln!("Falling back to legacy discovery");
                // Fall back to legacy discovery if membership protocol fails
                node_manager.start_discovery().await?;
            } else {
                println!("Node membership protocol initialized successfully");
            }

            // Initialize container networking
            let network_manager = match NetworkManager::new(
                Arc::new(node_manager.clone()),
                Arc::new(service_discovery.clone()),
                None, // Use default network config
            ).await {
                Ok(manager) => {
                    // Initialize the network
                    if let Err(e) = manager.initialize().await {
                        eprintln!("Failed to initialize container networking: {}", e);
                        None
                    } else {
                        println!("Container networking initialized successfully");
                        let manager_arc = Arc::new(manager);
                        
                        // Connect network manager to node manager
                        node_manager.set_network_manager(manager_arc.clone()).await;
                        
                        Some(manager_arc)
                    }
                }
                Err(e) => {
                    eprintln!("Failed to create network manager: {}", e);
                    None
                }
            };
            
            // Initialize service discovery with network manager
            let service_discovery = if let Some(network_manager) = &network_manager {
                service_discovery.with_network_manager(network_manager.clone())
            } else {
                service_discovery
            };
            
            // Start service discovery system
            if let Err(e) = service_discovery.initialize().await {
                eprintln!("Failed to initialize service discovery: {}", e);
            } else {
                println!("Service discovery initialized successfully");
            }
            
            // Initialize health monitor
            let health_monitor = Arc::new(HealthMonitor::new(
                Arc::new(app_manager.clone()),
                Arc::new(node_manager.clone()),
                Arc::new(service_discovery.clone()),
            ));

            // Start health monitoring
            if let Err(e) = health_monitor.start().await {
                eprintln!("Failed to start health monitor: {}", e);
            } else {
                println!("Health monitor started successfully");
            }

            // Initialize security manager
            let security_manager = Arc::new(SecurityManager::new());
            
            // Initialize security components
            if let Err(e) = security_manager.initialize().await {
                eprintln!("Failed to initialize security manager: {}", e);
            } else {
                println!("Security manager initialized successfully");
            }
            
            // Initialize tenant manager
            let mut tenant_manager = TenantManager::new(security_manager.rbac_manager());
            
            // Connect tenant manager with container runtime if available
            if let Some(container_runtime) = app_manager.get_container_runtime() {
                tenant_manager.set_container_runtime(container_runtime.clone());
                println!("Tenant manager connected to container runtime");
            }
            
            let tenant_manager = Arc::new(tenant_manager);
            println!("Tenant manager initialized successfully");
            
            // Initialize tenant quota enforcer
            let tenant_quota_enforcer = Arc::new(TenantQuotaEnforcer::new(
                tenant_manager.clone(),
                Arc::new(tokio::sync::RwLock::new(app_manager.clone())),
            ));
            
            // Initialize quota tracking
            if let Err(e) = tenant_quota_enforcer.initialize_quota_tracking().await {
                eprintln!("Failed to initialize quota tracking: {}", e);
            } else {
                println!("Tenant quota enforcement initialized successfully");
            }
            
            // Initialize CI/CD manager
            let cicd_base_dir = cli.data_dir.join("cicd");
            let cicd_manager = CicdManager::new(
                Arc::new(app_manager.clone()),
                security_manager.clone(),
                cicd_base_dir,
            );
            
            if let Err(e) = cicd_manager.initialize().await {
                eprintln!("Failed to initialize CI/CD manager: {}", e);
            } else {
                println!("CI/CD manager initialized successfully");
            }
            
            // Initialize cloud manager
            let cloud_base_dir = cli.data_dir.join("cloud");
            let cloud_manager = CloudManager::new(cloud_base_dir);
            
            if let Err(e) = cloud_manager.initialize().await {
                eprintln!("Failed to initialize cloud manager: {}", e);
            } else {
                println!("Cloud manager initialized successfully");
            }
            
            // Initialize container scheduler
            let mut scheduler_instance = ContainerScheduler::new(app_manager.clone())
                .with_node_manager(Arc::new(node_manager.clone()))
                .with_service_discovery(Arc::new(service_discovery.clone()));
            
            if let Some(network_manager_ref) = &network_manager {
                scheduler_instance = scheduler_instance.with_network_manager(network_manager_ref.clone());
            }
            
            if let Some(tenant_manager_ref) = &tenant_manager {
                scheduler_instance = scheduler_instance.with_tenant_manager(tenant_manager_ref.clone());
            }
            
            let scheduler = Arc::new(scheduler_instance);
            
            // Initialize deployment manager
            let mut deployment_manager = DeploymentManager::new();
            
            // Initialize the deployment manager with required dependencies
            if let Err(e) = deployment_manager.initialize(
                Arc::new(app_manager.clone()),
                scheduler.clone(),
                health_monitor.clone(),
                Arc::new(service_discovery.clone()),
            ).await {
                eprintln!("Failed to initialize deployment manager: {}", e);
            } else {
                println!("Deployment manager initialized successfully with zero-downtime support");
            }
            
            // Initialize Helm manager
            let helm_base_dir = cli.data_dir.join("helm");
            let helm_manager = HelmManager::new(helm_base_dir);
            
            if let Err(e) = helm_manager.initialize().await {
                eprintln!("Failed to initialize Helm manager: {}", e);
            } else {
                println!("Helm manager initialized successfully");
            }
            
            // Initialize observability manager
            let observability_base_dir = cli.data_dir.join("observability");
            let mut observability_manager = ObservabilityManager::new(observability_base_dir);
            
            if let Err(e) = observability_manager.initialize().await {
                eprintln!("Failed to initialize observability manager: {}", e);
            } else {
                println!("Observability manager initialized successfully");
                
                // Initialize Prometheus exporter
                if let Err(e) = observability_manager.init_prometheus_exporter(9090, "/metrics".to_string()).await {
                    eprintln!("Failed to initialize Prometheus exporter: {}", e);
                } else {
                    println!("Prometheus exporter initialized successfully");
                }
                
                // Initialize OpenTelemetry tracer
                if let Err(e) = observability_manager.init_opentelemetry_tracer(
                    "hivemind".to_string(),
                    "http://localhost:4317".to_string(),
                ).await {
                    eprintln!("Failed to initialize OpenTelemetry tracer: {}", e);
                } else {
                    println!("OpenTelemetry tracer initialized successfully");
                }
                
                // Initialize log aggregator
                if let Err(e) = observability_manager.init_log_aggregator(
                    "http://localhost:9200".to_string(),
                    "hivemind-logs".to_string(),
                ).await {
                    eprintln!("Failed to initialize log aggregator: {}", e);
                } else {
                    println!("Log aggregator initialized successfully");
                }
            }
            
            // Initialize resilience manager
            let resilience_manager = Arc::new(resilience::ResilienceManager::new());
            println!("Resilience manager initialized successfully");

            // Create AppState for sharing between web and API
            let app_state = AppState {
                node_manager: node_manager.clone(),
                app_manager: app_manager.clone(),
                service_discovery: service_discovery.clone(),
                network_manager,
                health_monitor: Some(health_monitor.clone()),
                security_manager: Some(security_manager.clone()),
                cicd_manager: Some(Arc::new(cicd_manager)),
                cloud_manager: Some(Arc::new(cloud_manager)),
                deployment_manager: Some(Arc::new(deployment_manager)),
                helm_manager: Some(Arc::new(helm_manager)),
                tenant_manager: Some(tenant_manager),
                tenant_quota_enforcer: Some(tenant_quota_enforcer),
                observability_manager: Some(Arc::new(observability_manager)),
                resilience_manager: Some(resilience_manager),
            };

            // Start the web interface in a separate task
            let web_state = app_state.clone();
            tokio::spawn(async move {
                let web_server = web::WebServer::new(web_state, web_port);
                if let Err(e) = web_server.start().await {
                    eprintln!("Web server error: {}", e);
                }
            });

            // Start container monitoring in a background task
            let monitor_app_manager = app_manager.clone();
            tokio::spawn(async move {
                loop {
                    if let Err(e) = monitor_app_manager.monitor_containers().await {
                        eprintln!("Container monitoring error: {}", e);
                    }
                    tokio::time::sleep(tokio::time::Duration::from_secs(30)).await;
                }
            });

            // Keep the daemon running
            loop {
                tokio::time::sleep(tokio::time::Duration::from_secs(60)).await;
            }
        }
        Commands::Web { port } => {
            // Start only the web interface (useful for development)
            let storage = StorageManager::new(&cli.data_dir).await?;
            let node_manager = NodeManager::with_storage(storage.clone()).await;
            let service_discovery = ServiceDiscovery::new();
            let app_manager = AppManager::with_storage(storage)
                .await?
                .with_service_discovery(service_discovery.clone());

            // Create AppState for the web interface
            let app_state = AppState {
                node_manager,
                app_manager,
                service_discovery,
                network_manager: None, // No network manager for web-only mode
                health_monitor: None, // No health monitor for web-only mode
                security_manager: None, // No security manager for web-only mode
                tenant_manager: None, // No tenant manager for web-only mode
                tenant_quota_enforcer: None, // No tenant quota enforcer for web-only mode
                cicd_manager: None, // No CI/CD manager for web-only mode
                cloud_manager: None, // No cloud manager for web-only mode
                deployment_manager: None, // No deployment manager for web-only mode
                helm_manager: None, // No Helm manager for web-only mode
                observability_manager: None, // No observability manager for web-only mode
                resilience_manager: None, // No resilience manager for web-only mode
            };

            // Start the web server
            let web_server = web::WebServer::new(app_state, port);
            web_server.start().await?;

            Ok(())
        }
        Commands::Join { host } => {
            let storage = StorageManager::new(&cli.data_dir).await?;
            let mut node_manager = NodeManager::with_storage(storage.clone()).await;
            let service_discovery = ServiceDiscovery::new();
            
            println!("Joining cluster at {}", host);
            
            // Initialize membership protocol
            if let Err(e) = node_manager.init_membership_protocol().await {
                eprintln!("Failed to initialize membership protocol: {}", e);
                eprintln!("Falling back to legacy discovery");
                // Fall back to legacy discovery
                node_manager.start_discovery().await?;
            }
            
            // Join the cluster
            let join_result = node_manager.join_cluster(&host).await;
            
            // Initialize container networking after joining
            if join_result.is_ok() {
                println!("Successfully joined cluster, initializing container networking...");
                
                // Initialize container networking
                match NetworkManager::new(
                    Arc::new(node_manager.clone()),
                    Arc::new(service_discovery.clone()),
                    None, // Use default network config
                ).await {
                    Ok(manager) => {
                        // Initialize the network
                        if let Err(e) = manager.initialize().await {
                            eprintln!("Failed to initialize container networking: {}", e);
                        } else {
                            println!("Container networking initialized successfully");
                            
                            // Connect network manager to node manager
                            let manager_arc = Arc::new(manager);
                            node_manager.set_network_manager(manager_arc.clone()).await;
                            
                            // Initialize service discovery with network manager
                            let service_discovery = service_discovery.with_network_manager(manager_arc.clone());
                            
                            // Start service discovery system
                            if let Err(e) = service_discovery.initialize().await {
                                eprintln!("Failed to initialize service discovery: {}", e);
                            } else {
                                println!("Service discovery initialized successfully");
                            }
                            
                            // Create AppState for join command
                            let _app_state = AppState {
                                node_manager: node_manager.clone(),
                                app_manager: AppManager::new().await?,
                                service_discovery,
                                network_manager: Some(manager_arc),
                                health_monitor: None,
                                security_manager: None,
                                tenant_manager: None,
                                tenant_quota_enforcer: None,
                                cicd_manager: None,
                                cloud_manager: None,
                                deployment_manager: None,
                                helm_manager: None,
                                observability_manager: None,
                                resilience_manager: None,
                            };
                        }
                    }
                    Err(e) => {
                        eprintln!("Failed to create network manager: {}", e);
                    }
                }
            }
            
            join_result
        }
        Commands::Node { command } => {
            let storage = StorageManager::new(&cli.data_dir).await?;
            let node_manager = NodeManager::with_storage(storage).await;
            
            // Create AppState for node commands
            let _app_state = AppState {
                node_manager: node_manager.clone(),
                app_manager: AppManager::new().await?,
                service_discovery: ServiceDiscovery::new(),
                network_manager: None,
                health_monitor: None,
                security_manager: None,
                tenant_manager: None,
                cicd_manager: None,
                cloud_manager: None,
                deployment_manager: None,
                helm_manager: None,
                observability_manager: None,
                tenant_quota_enforcer: None,
                resilience_manager: None,
            };
            match command {
                NodeCommands::Ls => {
                    let nodes = node_manager.list_nodes().await?;
                    for node in nodes {
                        println!("{}", node);
                    }
                    Ok(())
                }
                NodeCommands::Info => {
                    let nodes = node_manager.list_nodes().await?;
                    println!("Found {} nodes", nodes.len());
                    for node in nodes {
                        println!("Node: {}", node);
                    }
                    Ok(())
                }
            }
        }
        Commands::App { command } => {
            let storage = StorageManager::new(&cli.data_dir).await?;
            let service_discovery = ServiceDiscovery::new();
            let app_manager = AppManager::with_storage(storage)
                .await?
                .with_service_discovery(service_discovery.clone());
            
            // Initialize health monitor for zero-downtime deployments
            let health_monitor = Arc::new(HealthMonitor::new(
                Arc::new(app_manager.clone()),
                Arc::new(NodeManager::new()),
                Arc::new(service_discovery.clone()),
            ));
            
            // Initialize container scheduler
            let mut scheduler_instance = ContainerScheduler::new(app_manager.clone())
                .with_service_discovery(Arc::new(service_discovery.clone()));
                
            // Add tenant manager if available
            if let Some(tenant_manager) = &_app_state.tenant_manager {
                scheduler_instance = scheduler_instance.with_tenant_manager(tenant_manager.clone());
            }
            
            let scheduler = Arc::new(scheduler_instance);
            
            // Initialize deployment manager
            let mut deployment_manager = DeploymentManager::new();
            
            // Initialize the deployment manager with required dependencies
            if let Err(e) = deployment_manager.initialize(
                Arc::new(app_manager.clone()),
                scheduler.clone(),
                health_monitor.clone(),
                Arc::new(service_discovery.clone()),
            ).await {
                eprintln!("Failed to initialize deployment manager: {}", e);
            }
            
            // Create AppState for app commands
            let _app_state = AppState {
                node_manager: NodeManager::new(),
                app_manager: app_manager.clone(),
                service_discovery: service_discovery.clone(),
                network_manager: None,
                health_monitor: Some(health_monitor.clone()),
                security_manager: None,
                tenant_manager: None,
                cicd_manager: None,
                cloud_manager: None,
                deployment_manager: Some(Arc::new(deployment_manager)),
                helm_manager: None,
                observability_manager: None,
                tenant_quota_enforcer: None,
                resilience_manager: None,
            };

            match command {
                AppCommands::Ls => {
                    let services = app_manager.list_services().await?;
                    if services.is_empty() {
                        println!("No applications deployed");
                    } else {
                        println!("Applications:");
                        for service in services {
                            println!(
                                "  {} - {} replicas - domain: {}",
                                service.name, service.current_replicas, service.domain
                            );
                        }
                    }
                    Ok(())
                }
                AppCommands::Deploy {
                    image,
                    name,
                    service,
                    volume,
                } => {
                    if let Some(volumes) = volume {
                        // Parse volume mounts in format "volume_name:container_path"
                        let mut volume_mounts = Vec::new();
                        for vol in volumes {
                            if let Some((vol_name, container_path)) = vol.split_once(':') {
                                volume_mounts.push((vol_name.to_string(), container_path.to_string()));
                            } else {
                                eprintln!("Invalid volume format '{}'. Use 'volume_name:container_path'", vol);
                                std::process::exit(1);
                            }
                        }
                        
                        // Deploy with volumes
                        let container_id = app_manager
                            .deploy_app_with_volumes(&image, &name, service.as_deref(), volume_mounts, None, None)
                            .await?;
                        println!(
                            "Deployed application {} with container {} and volumes",
                            name, container_id
                        );
                    } else {
                        // Deploy without volumes
                        let container_id = app_manager
                            .deploy_app(&image, &name, service.as_deref(), None, None)
                            .await?;
                        println!(
                            "Deployed application {} with container {}",
                            name, container_id
                        );
                    }
                    Ok(())
                }
                AppCommands::Scale { name, replicas } => {
                    app_manager.scale_app(&name, replicas).await?;
                    println!("Scaled app {} to {} replicas", name, replicas);
                    Ok(())
                }
                AppCommands::Containers => {
                    let details = app_manager.get_container_details().await?;
                    if details.is_empty() {
                        println!("No containers found");
                    } else {
                        println!("Containers:");
                        for container in details {
                            println!(
                                "  {} - {} - {}",
                                container.name, container.image, container.status
                            );
                        }
                    }
                    Ok(())
                }
                AppCommands::ContainerInfo { container_id } => {
                    if let Some(container) = app_manager.get_container_by_id(&container_id).await {
                        println!("Container ID: {}", container.id);
                        println!("  Name: {}", container.name);
                        println!("  Image: {}", container.image);
                        println!("  Status: {}", container.status);
                        println!("  Node: {}", container.node_id);
                        println!("  Created: {}", container.created_at);

                        if !container.ports.is_empty() {
                            println!("  Ports:");
                            for port in container.ports {
                                println!(
                                    "    {}:{}/{}",
                                    port.host_port, port.container_port, port.protocol
                                );
                            }
                        }

                        if !container.env_vars.is_empty() {
                            println!("  Environment:");
                            for env in container.env_vars {
                                println!("    {}={}", env.key, env.value);
                            }
                        }

                        if let Some(stats) = app_manager.get_container_stats(&container_id).await {
                            println!("  Stats:");
                            println!("    CPU: {:.2}%", stats.cpu_usage);
                            println!("    Memory: {} MB", stats.memory_usage / (1024 * 1024));
                            println!("    Network RX: {} KB", stats.network_rx / 1024);
                            println!("    Network TX: {} KB", stats.network_tx / 1024);
                        }
                    } else {
                        println!("Container {} not found", container_id);
                    }
                    Ok(())
                }
                AppCommands::Restart { name } => {
                    app_manager.restart_app(&name).await?;
                    println!("Restarted app {}", name);
                    Ok(())
                }
                AppCommands::ZeroDowntimeDeploy {
                    image,
                    name,
                    service,
                    batch_size,
                    batch_delay,
                    health_check_path,
                    health_check_port,
                    health_check_timeout,
                    drain_timeout,
                } => {
                    // Get the deployment manager
                    let deployment_manager = _app_state.deployment_manager.as_ref()
                        .ok_or_else(|| anyhow::anyhow!("Deployment manager not available"))?;
                    
                    // Create deployment strategy
                    let strategy = deployment::DeploymentStrategy::RollingUpdate {
                        max_unavailable: 1,
                        max_surge: 1,
                        batch_size,
                        batch_delay,
                        zero_downtime: Some(true),
                        health_check_path,
                        health_check_port,
                        health_check_timeout,
                        drain_timeout,
                    };
                    
                    // Create deployment
                    let deployment_id = deployment_manager
                        .create_deployment(&name, &image, strategy, service.as_deref())
                        .await?;
                    
                    println!("Started zero-downtime deployment for {} with ID: {}", name, deployment_id);
                    println!("Use 'hivemind app deployment-status --id {}' to check status", deployment_id);
                    
                    Ok(())
                }
                AppCommands::DeploymentStatus { id } => {
                    // Get the deployment manager
                    let deployment_manager = _app_state.deployment_manager.as_ref()
                        .ok_or_else(|| anyhow::anyhow!("Deployment manager not available"))?;
                    
                    // Get deployment status
                    let status = deployment_manager.get_deployment_status(&id).await?;
                    
                    println!("Deployment ID: {}", id);
                    println!("Status: {}", status.status);
                    println!("Progress: {:.1}%", status.progress * 100.0);
                    if let Some(details) = &status.details {
                        println!("Details: {}", details);
                    }
                    println!("Created: {}", status.created_at);
                    println!("Last updated: {}", status.updated_at);
                    println!("Completed: {}", if status.completed { "Yes" } else { "No" });
                    if let Some(success) = status.success {
                        println!("Success: {}", if success { "Yes" } else { "No" });
                    }
                    
                    Ok(())
                }
                AppCommands::RollbackDeployment { id } => {
                    // Get the deployment manager
                    let deployment_manager = _app_state.deployment_manager.as_ref()
                        .ok_or_else(|| anyhow::anyhow!("Deployment manager not available"))?;
                    
                    // Rollback deployment
                    deployment_manager.rollback_deployment(&id).await?;
                    
                    println!("Rollback initiated for deployment {}", id);
                    println!("Use 'hivemind app deployment-status --id {}' to check status", id);
                    
                    Ok(())
                }
            }
        }
        Commands::Health => {
            let storage = StorageManager::new(&cli.data_dir).await?;
            let service_discovery = ServiceDiscovery::new();
            let app_manager = AppManager::with_storage(storage)
                .await?
                .with_service_discovery(service_discovery.clone());
            
            // Initialize node manager
            let node_manager = NodeManager::new();
            
            // Initialize network manager
            let network_manager = match NetworkManager::new(
                Arc::new(node_manager.clone()),
                Arc::new(service_discovery.clone()),
                None, // Use default network config
            ).await {
                Ok(manager) => Some(Arc::new(manager)),
                Err(e) => {
                    eprintln!("Failed to create network manager: {}", e);
                    None
                }
            };
            
            // Create AppState for health check
            let app_state = AppState {
                node_manager: node_manager.clone(),
                app_manager: app_manager.clone(),
                service_discovery: service_discovery.clone(),
                network_manager,
                health_monitor: None,
                security_manager: None,
                tenant_manager: None,
                cicd_manager: None,
                cloud_manager: None,
                deployment_manager: None,
                helm_manager: None,
                observability_manager: None,
                tenant_quota_enforcer: None,
                resilience_manager: None,
            };

            println!("Checking system health...");

            // Check container health
            match app_manager.get_container_details().await {
                Ok(containers) => {
                    println!("\nContainer Health:");
                    if containers.is_empty() {
                        println!("  No containers found");
                    } else {
                        for container in containers {
                            println!(
                                "  {} ({}) - {}",
                                container.name, container.id, container.status
                            );
                        }
                    }
                }
                Err(e) => eprintln!("Failed to check container health: {}", e),
            }

            // Check service health
            match service_discovery.list_services().await {
                services => {
                    println!("\nService Health:");
                    if services.is_empty() {
                        println!("  No services found");
                    } else {
                        for (name, endpoints) in services {
                            println!("  Service: {}", name);
                            for endpoint in endpoints {
                                println!(
                                    "    {} - {}",
                                    endpoint.ip_address, endpoint.health_status
                                );
                            }
                        }
                    }
                }
            }
            
            // Check network health
            if let Some(network_manager) = &app_state.network_manager {
                println!("\nNetwork Health:");
                
                // Get node network information
                let nodes = network_manager.get_nodes().await;
                if nodes.is_empty() {
                    println!("  No nodes in network");
                } else {
                    println!("  Nodes in network: {}", nodes.len());
                    for (node_id, info) in nodes.iter() {
                        println!("  Node: {}", node_id);
                        println!("    Address: {}", info.address);
                        println!("    Subnet: {}", info.subnet);
                    }
                }
                
                // Get overlay tunnels
                let tunnels = network_manager.get_tunnels().await;
                if tunnels.is_empty() {
                    println!("  No overlay tunnels");
                } else {
                    println!("  Overlay tunnels: {}", tunnels.len());
                    for (node_id, info) in tunnels.iter() {
                        println!("    Tunnel to {}: {} -> {}", 
                            node_id, info.local_ip, info.remote_ip);
                    }
                }
                
                // Get network policies
                let policies = network_manager.get_policies().await;
                if policies.is_empty() {
                    println!("  No network policies");
                } else {
                    println!("  Network policies: {}", policies.len());
                    for policy in policies {
                        println!("    Policy: {}", policy.name);
                        println!("      Ingress rules: {}", policy.ingress_rules.len());
                        println!("      Egress rules: {}", policy.egress_rules.len());
                    }
                }
            } else {
                println!("\nNetwork: Not available");
            }
            Ok(())
        }
        Commands::Volume { command } => {
            let storage = StorageManager::new(&cli.data_dir).await?;
            let service_discovery = ServiceDiscovery::new();
            let app_manager = AppManager::with_storage(storage)
                .await?
                .with_service_discovery(service_discovery.clone());

            match command {
                VolumeCommands::Create { name } => {
                    match app_manager.create_volume(&name).await {
                        Ok(_) => {
                            println!("Volume '{}' created successfully", name);
                        }
                        Err(e) => {
                            eprintln!("Failed to create volume '{}': {}", name, e);
                            std::process::exit(1);
                        }
                    }
                    Ok(())
                }
                VolumeCommands::Ls => {
                    match app_manager.list_volumes().await {
                        Ok(volumes) => {
                            if volumes.is_empty() {
                                println!("No volumes found");
                            } else {
                                println!("Volumes:");
                                for volume in volumes {
                                    let size_mb = volume.size / (1024 * 1024);
                                    let created = chrono::DateTime::from_timestamp(volume.created_at, 0)
                                        .map(|dt| dt.format("%Y-%m-%d %H:%M:%S").to_string())
                                        .unwrap_or_else(|| "Unknown".to_string());
                                    println!("  {} ({}MB) - Created: {}", volume.name, size_mb, created);
                                }
                            }
                        }
                        Err(e) => {
                            eprintln!("Failed to list volumes: {}", e);
                            std::process::exit(1);
                        }
                    }
                    Ok(())
                }
                VolumeCommands::Delete { name } => {
                    match app_manager.delete_volume(&name).await {
                        Ok(_) => {
                            println!("Volume '{}' deleted successfully", name);
                        }
                        Err(e) => {
                            eprintln!("Failed to delete volume '{}': {}", name, e);
                            std::process::exit(1);
                        }
                    }
                    Ok(())
                }
                VolumeCommands::Backup { name, output } => {
                    match app_manager.backup_volume(&name, &output).await {
                        Ok(_) => {
                            println!("Volume '{}' backed up to '{}' successfully", name, output);
                        }
                        Err(e) => {
                            eprintln!("Failed to backup volume '{}': {}", name, e);
                            std::process::exit(1);
                        }
                    }
                    Ok(())
                }
                VolumeCommands::Restore { name, input } => {
                    match app_manager.restore_volume(&name, &input).await {
                        Ok(_) => {
                            println!("Volume '{}' restored from '{}' successfully", name, input);
                        }
                        Err(e) => {
                            eprintln!("Failed to restore volume '{}': {}", name, e);
                            std::process::exit(1);
                        }
                    }
                    Ok(())
                }
            }
        }
    }
}
