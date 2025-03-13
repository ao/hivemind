use anyhow::Result;
use axum::{extract::State, Json};
use std::path::PathBuf;
use serde::{Serialize, Deserialize};
use clap::{Parser, Subcommand};

mod app;
mod container_manager;
mod node;
mod scheduler;
mod storage;
mod service_discovery;

use app::{AppManager, ServiceConfig};
use container_manager::{ContainerManager, Container};
use node::NodeManager;
use storage::StorageManager;
use service_discovery::{ServiceDiscovery, ServiceEndpoint};

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
    Daemon,
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
    /// Manage containers
    Container {
        #[command(subcommand)]
        command: ContainerCommands,
    },
    /// Check system health
    Health,
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
}

#[derive(Subcommand)]
enum ContainerCommands {
    /// List all containers
    Ls,
    /// Show detailed container information
    Info,
}

#[derive(Clone)]
struct AppState {
    container_manager: ContainerManager,
    node_manager: NodeManager,
    app_manager: AppManager,
    service_discovery: ServiceDiscovery,
}

async fn hello() -> &'static str {
    "Hello from Hivemind!"
}

async fn list_containers(State(state): State<AppState>) -> Json<Vec<String>> {
    Json(
        state
            .container_manager
            .list_containers()
            .await
            .unwrap_or_default(),
    )
}

async fn list_images(State(state): State<AppState>) -> Json<Vec<String>> {
    Json(
        state
            .container_manager
            .list_images()
            .await
            .unwrap_or_default(),
    )
}

async fn list_nodes(State(state): State<AppState>) -> Json<Vec<String>> {
    Json(state.node_manager.list_nodes().await.unwrap_or_default())
}

async fn get_container_details(State(state): State<AppState>) -> Json<Vec<Container>> {
    Json(
        state
            .container_manager
            .get_container_details()
            .await
            .unwrap_or_default(),
    )
}

async fn list_services(State(state): State<AppState>) -> Json<Vec<ServiceConfig>> {
    Json(
        state
            .app_manager
            .list_services()
            .await
            .unwrap_or_default(),
    )
}

async fn list_service_endpoints(State(state): State<AppState>) -> Json<std::collections::HashMap<String, Vec<ServiceEndpoint>>> {
    Json(state.service_discovery.list_services().await)
}

async fn get_service_url(State(state): State<AppState>, Json(payload): Json<ServiceUrlRequest>) -> Json<ServiceUrlResponse> {
    match state.service_discovery.get_service_url(&payload.service_name).await {
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

#[derive(Deserialize)]
struct ServiceUrlRequest {
    service_name: String,
}

#[derive(Serialize)]
struct ServiceUrlResponse {
    success: bool,
    url: Option<String>,
    error: Option<String>,
}

async fn deploy_container(
    State(state): State<AppState>,
    Json(payload): Json<DeployRequest>,
) -> Json<DeployResponse> {
    match state
        .app_manager
        .deploy_app(&payload.image, &payload.name, payload.service.as_deref())
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

#[derive(Deserialize)]
struct DeployRequest {
    image: String,
    name: String,
    service: Option<String>,
}

#[derive(Serialize)]
struct DeployResponse {
    success: bool,
    container_id: Option<String>,
    error: Option<String>,
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();

    match cli.command {
        Commands::Daemon => {
            let storage = StorageManager::new(&cli.data_dir).await?;
            let node_manager = NodeManager::with_storage(storage.clone()).await;
            let container_manager = ContainerManager::with_storage(storage).await?;
            let service_discovery = ServiceDiscovery::new();

            // Start node discovery
            node_manager.start_discovery().await?;

            // Start service discovery
            service_discovery.start_dns_server().await?;

            // Monitor containers
            container_manager.monitor_containers().await?;

            // Keep the daemon running
            loop {
                tokio::time::sleep(tokio::time::Duration::from_secs(60)).await;
            }
        },
        Commands::Join { host } => {
            let storage = StorageManager::new(&cli.data_dir).await?;
            let node_manager = NodeManager::with_storage(storage).await;
            println!("Joining cluster at {}", host);
            node_manager.start_discovery().await
        },
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
                },
                NodeCommands::Info => {
                    let nodes = node_manager.list_nodes().await?;
                    println!("Found {} nodes", nodes.len());
                    for node in nodes {
                        println!("Node: {}", node);
                    }
                    Ok(())
                },
            }
        },
        Commands::App { command } => {
            let storage = StorageManager::new(&cli.data_dir).await?;
            let container_manager = ContainerManager::with_storage(storage).await?;
            let service_discovery = ServiceDiscovery::new();
            let app_manager = AppManager::new(container_manager)
                .with_service_discovery(service_discovery);

            match command {
                AppCommands::Ls => {
                    let services = app_manager.list_services().await?;
                    for service in services {
                        println!("{}", service.name);
                    }
                    Ok(())
                },
                AppCommands::Deploy { image, name, service } => {
                    let container_id = app_manager.deploy_app(&image, &name, service.as_deref()).await?;
                    println!("Deployed container {}", container_id);
                    Ok(())
                },
                AppCommands::Scale { name, replicas } => {
                    app_manager.scale_app(&name, replicas).await?;
                    println!("Scaled app {} to {} replicas", name, replicas);
                    Ok(())
                },
            }
        },
        Commands::Container { command } => {
            let storage = StorageManager::new(&cli.data_dir).await?;
            let container_manager = ContainerManager::with_storage(storage).await?;
            match command {
                ContainerCommands::Ls => {
                    let containers = container_manager.list_containers().await?;
                    for container in containers {
                        println!("{}", container);
                    }
                    Ok(())
                },
                ContainerCommands::Info => {
                    let details = container_manager.get_container_details().await?;
                    for container in details {
                        println!("Container ID: {}", container.id);
                        println!("  Image: {}", container.image);
                        println!("  Status: {}", container.status);
                        println!("  Created: {}", container.created_at);
                    }
                    Ok(())
                },
            }
        },
        Commands::Health => {
            let storage = StorageManager::new(&cli.data_dir).await?;
            let container_manager = ContainerManager::with_storage(storage).await?;
            let service_discovery = ServiceDiscovery::new();
            
            println!("Checking system health...");
            
            // Check container health
            match container_manager.get_container_details().await {
                Ok(containers) => {
                    println!("\nContainer Health:");
                    for container in containers {
                        println!("  {} - {}", container.id, container.status);
                    }
                },
                Err(e) => eprintln!("Failed to check container health: {}", e),
            }
            
            // Check service health
            match service_discovery.list_services().await {
                services => {
                    println!("\nService Health:");
                    for (name, endpoints) in services {
                        println!("  Service: {}", name);
                        for endpoint in endpoints {
                            println!("    {} - {}", endpoint.ip_address, endpoint.health_status);
                        }
                    }
                }
            }
            Ok(())
        },
    }
}
