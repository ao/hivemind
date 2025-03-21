use anyhow::Result;
use axum::{extract::State, Json};
use clap::{Parser, Subcommand};
use std::path::PathBuf;

// Use types from lib.rs
use hivemind::{AppState, DeployRequest, DeployResponse, ServiceUrlRequest, ServiceUrlResponse};
use hivemind::app::{AppManager, ServiceConfig};
use hivemind::youki_manager::Container;
use hivemind::node::NodeManager;
use hivemind::service_discovery::{ServiceDiscovery, ServiceEndpoint};
use hivemind::storage::StorageManager;
use hivemind::web;

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
            // Default youki socket path
            let youki_socket = "/run/youki/youki.sock";
            let youki_namespace = "hivemind";

            let storage = StorageManager::new(&cli.data_dir).await?;
            let node_manager = NodeManager::with_storage(storage.clone()).await;
            let service_discovery = ServiceDiscovery::new();

            // Initialize AppManager with youki
            let app_manager = match AppManager::with_youki(
                storage.clone(),
                youki_socket,
                youki_namespace,
            )
            .await
            {
                Ok(manager) => {
                    println!("Successfully connected to youki");
                    manager.with_service_discovery(service_discovery.clone())
                }
                Err(e) => {
                    eprintln!("Failed to connect to youki: {}", e);
                    eprintln!("Falling back to mock implementation");
                    AppManager::with_storage(storage)
                        .await?
                        .with_service_discovery(service_discovery.clone())
                }
            };

            // Create AppState for sharing between web and API
            let app_state = AppState {
                node_manager: node_manager.clone(),
                app_manager: app_manager.clone(),
                service_discovery: service_discovery.clone(),
            };

            // Start node discovery
            node_manager.start_discovery().await?;

            // Start service discovery
            service_discovery.start_dns_server().await?;

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
            };

            // Start the web server
            let web_server = web::WebServer::new(app_state, port);
            web_server.start().await?;

            Ok(())
        }
        Commands::Join { host } => {
            let storage = StorageManager::new(&cli.data_dir).await?;
            let node_manager = NodeManager::with_storage(storage).await;
            println!("Joining cluster at {}", host);
            node_manager.start_discovery().await
        }
        Commands::Node { command } => {
            let storage = StorageManager::new(&cli.data_dir).await?;
            let node_manager = NodeManager::with_storage(storage).await;
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
                .with_service_discovery(service_discovery);

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
                } => {
                    let container_id = app_manager
                        .deploy_app(&image, &name, service.as_deref(), None, None)
                        .await?;
                    println!(
                        "Deployed application {} with container {}",
                        name, container_id
                    );
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
            Ok(())
        }
    }
}
