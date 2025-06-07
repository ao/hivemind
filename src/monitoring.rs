use anyhow::Result;
use async_trait::async_trait;
use serde::{Serialize, Deserialize};
use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::RwLock;
use crate::tenant::ResourceUsage;
use crate::logging;

/// Represents a metric to be sent to external monitoring systems
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Metric {
    pub name: String,
    pub value: f64,
    pub labels: HashMap<String, String>,
    pub timestamp: i64,
}

/// Trait for external monitoring system integrations
#[async_trait]
pub trait MonitoringSystem: Send + Sync + 'static {
    /// Send metrics to the monitoring system
    async fn send_metrics(&self, metrics: Vec<Metric>) -> Result<()>;
    
    /// Get the name of the monitoring system
    fn name(&self) -> &str;
    
    /// Check if the monitoring system is available
    async fn is_available(&self) -> bool;
}

/// Prometheus monitoring system implementation
pub struct PrometheusMonitoring {
    endpoint: String,
    available: Arc<RwLock<bool>>,
}

impl PrometheusMonitoring {
    pub fn new(endpoint: String) -> Self {
        Self {
            endpoint,
            available: Arc::new(RwLock::new(true)),
        }
    }
}

#[async_trait]
impl MonitoringSystem for PrometheusMonitoring {
    async fn send_metrics(&self, metrics: Vec<Metric>) -> Result<()> {
        // In a real implementation, this would send metrics to Prometheus
        // For now, we'll just log the metrics
        for metric in &metrics {
            logging::debug!(
                "Sending metric to Prometheus: {} = {} (labels: {:?})",
                metric.name,
                metric.value,
                metric.labels
            );
        }
        
        Ok(())
    }
    
    fn name(&self) -> &str {
        "Prometheus"
    }
    
    async fn is_available(&self) -> bool {
        *self.available.read().await
    }
}

/// Monitoring manager that handles multiple monitoring systems
pub struct MonitoringManager {
    systems: Vec<Arc<dyn MonitoringSystem>>,
}

impl MonitoringManager {
    pub fn new() -> Self {
        Self {
            systems: Vec::new(),
        }
    }
    
    pub fn add_system<T: MonitoringSystem>(&mut self, system: T) {
        self.systems.push(Arc::new(system));
    }
    
    pub async fn send_tenant_metrics(&self, tenant_id: &str, usage: &ResourceUsage) -> Result<()> {
        let timestamp = chrono::Utc::now().timestamp();
        
        let metrics = vec![
            Metric {
                name: "tenant_cpu_usage".to_string(),
                value: usage.cpu_usage as f64,
                labels: HashMap::from([
                    ("tenant_id".to_string(), tenant_id.to_string()),
                ]),
                timestamp,
            },
            Metric {
                name: "tenant_memory_usage".to_string(),
                value: usage.memory_usage as f64,
                labels: HashMap::from([
                    ("tenant_id".to_string(), tenant_id.to_string()),
                ]),
                timestamp,
            },
            Metric {
                name: "tenant_storage_usage".to_string(),
                value: usage.storage_usage as f64,
                labels: HashMap::from([
                    ("tenant_id".to_string(), tenant_id.to_string()),
                ]),
                timestamp,
            },
            Metric {
                name: "tenant_container_count".to_string(),
                value: usage.container_count as f64,
                labels: HashMap::from([
                    ("tenant_id".to_string(), tenant_id.to_string()),
                ]),
                timestamp,
            },
            Metric {
                name: "tenant_service_count".to_string(),
                value: usage.service_count as f64,
                labels: HashMap::from([
                    ("tenant_id".to_string(), tenant_id.to_string()),
                ]),
                timestamp,
            },
        ];
        
        for system in &self.systems {
            if system.is_available().await {
                match system.send_metrics(metrics.clone()).await {
                    Ok(_) => {
                        logging::log_external_monitoring(tenant_id, system.name(), true);
                    },
                    Err(e) => {
                        logging::log_external_monitoring(tenant_id, system.name(), false);
                        logging::error!("Failed to send metrics to {}: {}", system.name(), e);
                    }
                }
            }
        }
        
        Ok(())
    }
}

/// Default implementation that creates a monitoring manager with Prometheus
pub fn create_default_monitoring() -> MonitoringManager {
    let mut manager = MonitoringManager::new();
    manager.add_system(PrometheusMonitoring::new("http://localhost:9090".to_string()));
    manager
}