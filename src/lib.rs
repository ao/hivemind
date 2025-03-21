pub mod app;
pub mod node;
pub mod scheduler;
pub mod service_discovery;
pub mod storage;
pub mod web;
pub mod youki_manager;

// Re-export types needed by web.rs
use serde::{Deserialize, Serialize};

#[derive(Clone)]
pub struct AppState {
    pub node_manager: node::NodeManager,
    pub app_manager: app::AppManager,
    pub service_discovery: service_discovery::ServiceDiscovery,
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

#[cfg(test)]
pub mod tests {
    pub mod mocks;
    pub mod app_tests;
    pub mod e2e_tests;
    pub mod property_tests;
}
