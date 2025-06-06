pub mod app;
pub mod app_quota;
pub mod cicd;
pub mod cloud;
pub mod containerd_manager;
pub mod deployment;
pub mod health_monitor;
pub mod helm;
pub mod membership;
pub mod network;
pub mod node;
pub mod observability;
pub mod scheduler;
pub mod security;
pub mod service_discovery;
pub mod storage;
pub mod tenant;
pub mod tenant_quota;
pub mod web;
pub mod resilience;
pub mod service_discovery_resilience;
pub use containerd_manager::*;

// Re-export types needed by web.rs
use serde::{Deserialize, Serialize};

#[derive(Clone)]
pub struct AppState {
    pub node_manager: node::NodeManager,
    pub app_manager: app::AppManager,
    pub service_discovery: service_discovery::ServiceDiscovery,
    pub network_manager: Option<std::sync::Arc<network::NetworkManager>>,
    pub health_monitor: Option<std::sync::Arc<health_monitor::HealthMonitor>>,
    pub security_manager: Option<std::sync::Arc<security::SecurityManager>>,
    pub tenant_manager: Option<std::sync::Arc<tenant::TenantManager>>,
    pub tenant_quota_enforcer: Option<std::sync::Arc<tenant_quota::TenantQuotaEnforcer>>,
    pub cicd_manager: Option<std::sync::Arc<cicd::CicdManager>>,
    pub cloud_manager: Option<std::sync::Arc<cloud::CloudManager>>,
    pub deployment_manager: Option<std::sync::Arc<deployment::DeploymentManager>>,
    pub helm_manager: Option<std::sync::Arc<helm::HelmManager>>,
    pub observability_manager: Option<std::sync::Arc<observability::ObservabilityManager>>,
    pub resilience_manager: Option<std::sync::Arc<resilience::ResilienceManager>>,
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

