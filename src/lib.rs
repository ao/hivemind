pub mod app;
pub mod membership;
pub mod network;
pub mod node;
pub mod scheduler;
pub mod service_discovery;
pub mod storage;
pub mod web;
pub mod containerd_manager;
pub use containerd_manager::*;

// Re-export types needed by web.rs
use serde::{Deserialize, Serialize};

#[derive(Clone)]
pub struct AppState {
    pub node_manager: node::NodeManager,
    pub app_manager: app::AppManager,
    pub service_discovery: service_discovery::ServiceDiscovery,
    pub network_manager: Option<std::sync::Arc<network::NetworkManager>>,
}

#[derive(Deserialize)]
pub struct DeployRequest {
    pub image: String,
    pub name: String,
    pub service: Option<String>,
}

#[derive(Serialize)]
pub struct DeployResponse {
    pub success: bool,
    pub container_id: Option<String>,
    pub error: Option<String>,
}

#[derive(Deserialize)]
pub struct ServiceUrlRequest {
    pub service_name: String,
}

#[derive(Serialize)]
pub struct ServiceUrlResponse {
    pub success: bool,
    pub url: Option<String>,
    pub error: Option<String>,
}

