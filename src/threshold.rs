use anyhow::Result;
use serde::{Serialize, Deserialize};
use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::RwLock;
use crate::tenant::ResourceUsage;
use crate::logging;

/// Threshold configuration for resource usage alerts
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ThresholdConfig {
    pub warning_percent: u32,
    pub critical_percent: u32,
    pub adjustment_factor: f64,
    pub min_threshold_percent: u32,
    pub max_threshold_percent: u32,
    pub adjustment_window_seconds: i64,
}

impl Default for ThresholdConfig {
    fn default() -> Self {
        Self {
            warning_percent: 80,
            critical_percent: 90,
            adjustment_factor: 0.1,
            min_threshold_percent: 50,
            max_threshold_percent: 95,
            adjustment_window_seconds: 3600, // 1 hour
        }
    }
}

/// Resource type for thresholds
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub enum ResourceType {
    Cpu,
    Memory,
    Storage,
    Containers,
    Services,
}

impl ResourceType {
    pub fn as_str(&self) -> &'static str {
        match self {
            ResourceType::Cpu => "cpu",
            ResourceType::Memory => "memory",
            ResourceType::Storage => "storage",
            ResourceType::Containers => "containers",
            ResourceType::Services => "services",
        }
    }
}

/// Threshold manager for dynamic threshold adjustment
pub struct ThresholdManager {
    /// Tenant-specific threshold configurations
    tenant_configs: Arc<RwLock<HashMap<String, ThresholdConfig>>>,
    
    /// Default threshold configuration
    default_config: ThresholdConfig,
    
    /// Historical usage data for threshold adjustment
    usage_history: Arc<RwLock<HashMap<String, Vec<(ResourceType, u64, i64)>>>>, // (tenant_id, [(resource_type, value, timestamp)])
}

impl ThresholdManager {
    pub fn new() -> Self {
        Self {
            tenant_configs: Arc::new(RwLock::new(HashMap::new())),
            default_config: ThresholdConfig::default(),
            usage_history: Arc::new(RwLock::new(HashMap::new())),
        }
    }
    
    /// Set a custom threshold configuration for a tenant
    pub async fn set_tenant_config(&self, tenant_id: &str, config: ThresholdConfig) {
        let mut configs = self.tenant_configs.write().await;
        configs.insert(tenant_id.to_string(), config);
    }
    
    /// Get the threshold configuration for a tenant
    pub async fn get_tenant_config(&self, tenant_id: &str) -> ThresholdConfig {
        let configs = self.tenant_configs.read().await;
        configs.get(tenant_id).cloned().unwrap_or_else(|| self.default_config.clone())
    }
    
    /// Record resource usage for a tenant
    pub async fn record_usage(&self, tenant_id: &str, usage: &ResourceUsage) {
        let timestamp = chrono::Utc::now().timestamp();
        let mut history = self.usage_history.write().await;
        
        let tenant_history = history.entry(tenant_id.to_string()).or_insert_with(Vec::new);
        
        // Add current usage to history
        tenant_history.push((ResourceType::Cpu, usage.cpu_usage as u64, timestamp));
        tenant_history.push((ResourceType::Memory, usage.memory_usage, timestamp));
        tenant_history.push((ResourceType::Storage, usage.storage_usage, timestamp));
        tenant_history.push((ResourceType::Containers, usage.container_count as u64, timestamp));
        tenant_history.push((ResourceType::Services, usage.service_count as u64, timestamp));
        
        // Prune old entries
        let config = self.get_tenant_config(tenant_id).await;
        let cutoff = timestamp - config.adjustment_window_seconds;
        tenant_history.retain(|&(_, _, ts)| ts >= cutoff);
    }
    
    /// Adjust thresholds based on historical usage
    pub async fn adjust_thresholds(&self, tenant_id: &str) -> Result<ThresholdConfig> {
        let history = self.usage_history.read().await;
        let tenant_history = match history.get(tenant_id) {
            Some(h) => h,
            None => return Ok(self.get_tenant_config(tenant_id).await),
        };
        
        let mut config = self.get_tenant_config(tenant_id).await;
        
        // Group by resource type
        let mut cpu_usage = Vec::new();
        let mut memory_usage = Vec::new();
        let mut storage_usage = Vec::new();
        let mut container_count = Vec::new();
        let mut service_count = Vec::new();
        
        for &(resource_type, value, _) in tenant_history {
            match resource_type {
                ResourceType::Cpu => cpu_usage.push(value),
                ResourceType::Memory => memory_usage.push(value),
                ResourceType::Storage => storage_usage.push(value),
                ResourceType::Containers => container_count.push(value),
                ResourceType::Services => service_count.push(value),
            }
        }
        
        // Calculate average usage for each resource type
        let avg_cpu = if !cpu_usage.is_empty() {
            cpu_usage.iter().sum::<u64>() as f64 / cpu_usage.len() as f64
        } else {
            0.0
        };
        
        let avg_memory = if !memory_usage.is_empty() {
            memory_usage.iter().sum::<u64>() as f64 / memory_usage.len() as f64
        } else {
            0.0
        };
        
        let avg_storage = if !storage_usage.is_empty() {
            storage_usage.iter().sum::<u64>() as f64 / storage_usage.len() as f64
        } else {
            0.0
        };
        
        let avg_containers = if !container_count.is_empty() {
            container_count.iter().sum::<u64>() as f64 / container_count.len() as f64
        } else {
            0.0
        };
        
        let avg_services = if !service_count.is_empty() {
            service_count.iter().sum::<u64>() as f64 / service_count.len() as f64
        } else {
            0.0
        };
        
        // Log the averages
        logging::debug!(
            "Average resource usage for tenant {}: CPU: {:.2}%, Memory: {:.2} bytes, Storage: {:.2} bytes, Containers: {:.2}, Services: {:.2}",
            tenant_id, avg_cpu, avg_memory, avg_storage, avg_containers, avg_services
        );
        
        // Adjust warning threshold based on usage patterns
        // If usage is consistently high, increase the threshold
        // If usage is consistently low, decrease the threshold
        let adjust_threshold = |current: u32, avg_usage: f64, limit: u64| -> u32 {
            if limit == 0 {
                return current;
            }
            
            let usage_percent = (avg_usage / limit as f64) * 100.0;
            let adjustment = config.adjustment_factor * (usage_percent - current as f64);
            
            let new_threshold = (current as f64 + adjustment).round() as u32;
            new_threshold.clamp(config.min_threshold_percent, config.max_threshold_percent)
        };
        
        // Get the tenant to check resource limits
        // For now, we'll just use placeholder values for limits
        let cpu_limit = 100; // 100% CPU
        let memory_limit = 8 * 1024 * 1024 * 1024; // 8 GB
        let storage_limit = 100 * 1024 * 1024 * 1024; // 100 GB
        let container_limit = 10;
        let service_limit = 5;
        
        // Adjust warning threshold
        let new_warning = (
            adjust_threshold(config.warning_percent, avg_cpu, cpu_limit) +
            adjust_threshold(config.warning_percent, avg_memory, memory_limit) +
            adjust_threshold(config.warning_percent, avg_storage, storage_limit) +
            adjust_threshold(config.warning_percent, avg_containers, container_limit) +
            adjust_threshold(config.warning_percent, avg_services, service_limit)
        ) / 5;
        
        // Adjust critical threshold
        let new_critical = (
            adjust_threshold(config.critical_percent, avg_cpu, cpu_limit) +
            adjust_threshold(config.critical_percent, avg_memory, memory_limit) +
            adjust_threshold(config.critical_percent, avg_storage, storage_limit) +
            adjust_threshold(config.critical_percent, avg_containers, container_limit) +
            adjust_threshold(config.critical_percent, avg_services, service_limit)
        ) / 5;
        
        // Ensure critical is always higher than warning
        let new_critical = new_critical.max(new_warning + 5);
        
        // Update config
        config.warning_percent = new_warning;
        config.critical_percent = new_critical;
        
        // Save updated config
        let mut configs = self.tenant_configs.write().await;
        configs.insert(tenant_id.to_string(), config.clone());
        
        logging::info!(
            "Adjusted thresholds for tenant {}: Warning: {}%, Critical: {}%",
            tenant_id, config.warning_percent, config.critical_percent
        );
        
        Ok(config)
    }
    
    /// Check if resource usage exceeds thresholds
    pub async fn check_thresholds(&self, tenant_id: &str, usage: &ResourceUsage, limits: &ResourceLimits) -> Vec<ThresholdAlert> {
        let config = self.get_tenant_config(tenant_id).await;
        let mut alerts = Vec::new();
        
        // Check CPU usage
        if let Some(cpu_limit) = limits.cpu_limit {
            let usage_percent = (usage.cpu_usage as f64 / cpu_limit as f64) * 100.0;
            if usage_percent >= config.critical_percent as f64 {
                alerts.push(ThresholdAlert {
                    tenant_id: tenant_id.to_string(),
                    resource_type: ResourceType::Cpu,
                    usage: usage.cpu_usage as u64,
                    limit: cpu_limit as u64,
                    usage_percent,
                    threshold_percent: config.critical_percent,
                    severity: AlertSeverity::Critical,
                    timestamp: chrono::Utc::now().timestamp(),
                });
            } else if usage_percent >= config.warning_percent as f64 {
                alerts.push(ThresholdAlert {
                    tenant_id: tenant_id.to_string(),
                    resource_type: ResourceType::Cpu,
                    usage: usage.cpu_usage as u64,
                    limit: cpu_limit as u64,
                    usage_percent,
                    threshold_percent: config.warning_percent,
                    severity: AlertSeverity::Warning,
                    timestamp: chrono::Utc::now().timestamp(),
                });
            }
        }
        
        // Check memory usage
        if let Some(memory_limit) = limits.memory_limit {
            let usage_percent = (usage.memory_usage as f64 / memory_limit as f64) * 100.0;
            if usage_percent >= config.critical_percent as f64 {
                alerts.push(ThresholdAlert {
                    tenant_id: tenant_id.to_string(),
                    resource_type: ResourceType::Memory,
                    usage: usage.memory_usage,
                    limit: memory_limit,
                    usage_percent,
                    threshold_percent: config.critical_percent,
                    severity: AlertSeverity::Critical,
                    timestamp: chrono::Utc::now().timestamp(),
                });
            } else if usage_percent >= config.warning_percent as f64 {
                alerts.push(ThresholdAlert {
                    tenant_id: tenant_id.to_string(),
                    resource_type: ResourceType::Memory,
                    usage: usage.memory_usage,
                    limit: memory_limit,
                    usage_percent,
                    threshold_percent: config.warning_percent,
                    severity: AlertSeverity::Warning,
                    timestamp: chrono::Utc::now().timestamp(),
                });
            }
        }
        
        // Check storage usage
        if let Some(storage_limit) = limits.storage_limit {
            let usage_percent = (usage.storage_usage as f64 / storage_limit as f64) * 100.0;
            if usage_percent >= config.critical_percent as f64 {
                alerts.push(ThresholdAlert {
                    tenant_id: tenant_id.to_string(),
                    resource_type: ResourceType::Storage,
                    usage: usage.storage_usage,
                    limit: storage_limit,
                    usage_percent,
                    threshold_percent: config.critical_percent,
                    severity: AlertSeverity::Critical,
                    timestamp: chrono::Utc::now().timestamp(),
                });
            } else if usage_percent >= config.warning_percent as f64 {
                alerts.push(ThresholdAlert {
                    tenant_id: tenant_id.to_string(),
                    resource_type: ResourceType::Storage,
                    usage: usage.storage_usage,
                    limit: storage_limit,
                    usage_percent,
                    threshold_percent: config.warning_percent,
                    severity: AlertSeverity::Warning,
                    timestamp: chrono::Utc::now().timestamp(),
                });
            }
        }
        
        // Check container count
        if let Some(container_limit) = limits.max_containers {
            let usage_percent = (usage.container_count as f64 / container_limit as f64) * 100.0;
            if usage_percent >= config.critical_percent as f64 {
                alerts.push(ThresholdAlert {
                    tenant_id: tenant_id.to_string(),
                    resource_type: ResourceType::Containers,
                    usage: usage.container_count as u64,
                    limit: container_limit as u64,
                    usage_percent,
                    threshold_percent: config.critical_percent,
                    severity: AlertSeverity::Critical,
                    timestamp: chrono::Utc::now().timestamp(),
                });
            } else if usage_percent >= config.warning_percent as f64 {
                alerts.push(ThresholdAlert {
                    tenant_id: tenant_id.to_string(),
                    resource_type: ResourceType::Containers,
                    usage: usage.container_count as u64,
                    limit: container_limit as u64,
                    usage_percent,
                    threshold_percent: config.warning_percent,
                    severity: AlertSeverity::Warning,
                    timestamp: chrono::Utc::now().timestamp(),
                });
            }
        }
        
        // Check service count
        if let Some(service_limit) = limits.max_services {
            let usage_percent = (usage.service_count as f64 / service_limit as f64) * 100.0;
            if usage_percent >= config.critical_percent as f64 {
                alerts.push(ThresholdAlert {
                    tenant_id: tenant_id.to_string(),
                    resource_type: ResourceType::Services,
                    usage: usage.service_count as u64,
                    limit: service_limit as u64,
                    usage_percent,
                    threshold_percent: config.critical_percent,
                    severity: AlertSeverity::Critical,
                    timestamp: chrono::Utc::now().timestamp(),
                });
            } else if usage_percent >= config.warning_percent as f64 {
                alerts.push(ThresholdAlert {
                    tenant_id: tenant_id.to_string(),
                    resource_type: ResourceType::Services,
                    usage: usage.service_count as u64,
                    limit: service_limit as u64,
                    usage_percent,
                    threshold_percent: config.warning_percent,
                    severity: AlertSeverity::Warning,
                    timestamp: chrono::Utc::now().timestamp(),
                });
            }
        }
        
        // Log alerts
        for alert in &alerts {
            match alert.severity {
                AlertSeverity::Warning => {
                    logging::warn!(
                        "Tenant {} is approaching {} limit: {} of {} ({:.1}%)",
                        alert.tenant_id, alert.resource_type.as_str(), alert.usage, alert.limit, alert.usage_percent
                    );
                },
                AlertSeverity::Critical => {
                    logging::error!(
                        "Tenant {} has exceeded {} limit: {} of {} ({:.1}%)",
                        alert.tenant_id, alert.resource_type.as_str(), alert.usage, alert.limit, alert.usage_percent
                    );
                },
            }
        }
        
        alerts
    }
}

/// Resource limits for threshold checking
pub struct ResourceLimits {
    pub cpu_limit: Option<u32>,
    pub memory_limit: Option<u64>,
    pub storage_limit: Option<u64>,
    pub max_containers: Option<u32>,
    pub max_services: Option<u32>,
}

/// Alert severity levels
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum AlertSeverity {
    Warning,
    Critical,
}

/// Threshold alert
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ThresholdAlert {
    pub tenant_id: String,
    pub resource_type: ResourceType,
    pub usage: u64,
    pub limit: u64,
    pub usage_percent: f64,
    pub threshold_percent: u32,
    pub severity: AlertSeverity,
    pub timestamp: i64,
}