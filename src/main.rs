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
use hivemind::security::SecurityManager;
use hivemind::service_discovery::{ServiceDiscovery, ServiceEndpoint};
use hivemind::storage::StorageManager;
use hivemind::web;
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

            // Create AppState for sharing between web and API
            let app_state = AppState {
                node_manager: node_manager.clone(),
                app_manager: app_manager.clone(),
                service_discovery: service_discovery.clone(),
                network_manager,
                health_monitor: Some(health_monitor.clone()),
                security_manager: Some(security_manager.clone()),
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
            
            // Create AppState for app commands
            let _app_state = AppState {
                node_manager: NodeManager::new(),
                app_manager: app_manager.clone(),
                service_discovery,
                network_manager: None,
                health_monitor: None,
                security_manager: None,
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
            }
        }
    }
}
