use anyhow::Result;
use std::sync::Arc;
use tokio::sync::RwLock;
use std::collections::{HashMap, HashSet};
use log::{info, warn, error, debug, trace};
use chrono::Utc;

use crate::tenant::TenantManager;
use crate::app::AppManager;
use crate::containerd_manager::Container;
use crate::logging;
use crate::monitoring::MonitoringManager;

/// Represents a grace period for quota violations
#[derive(Debug, Clone)]
struct QuotaGracePeriod {
    resource_type: String,
    expiration: i64,  // Unix timestamp when grace period expires
    original_limit: u32,
    temporary_limit: u32,
}

/// TenantQuotaEnforcer handles the enforcement of tenant quotas
pub struct TenantQuotaEnforcer {
    tenant_manager: Arc<TenantManager>,
    app_manager: Arc<RwLock<AppManager>>,
    // Threshold percentages for quota alerts
    container_warning_threshold: u32,
    service_warning_threshold: u32,
    container_critical_threshold: u32,
    service_critical_threshold: u32,
    // Notification system
    notification_endpoints: Vec<String>,
    // Quota violation tracking
    quota_violations: Arc<RwLock<HashMap<String, Vec<QuotaViolation>>>>,
    // Dynamic quota adjustment
    auto_adjust_quotas: bool,
    // External monitoring integration
    monitoring_manager: Option<Arc<MonitoringManager>>,
    // Grace period management
    grace_periods: Arc<RwLock<HashMap<String, HashMap<String, QuotaGracePeriod>>>>,
    // Default grace period duration in seconds (24 hours)
    default_grace_period: i64,
}

impl TenantQuotaEnforcer {
    /// Create a new TenantQuotaEnforcer
    pub fn new(tenant_manager: Arc<TenantManager>, app_manager: Arc<RwLock<AppManager>>) -> Self {
        Self {
            tenant_manager,
            app_manager,
            container_warning_threshold: 80,
            service_warning_threshold: 80,
            container_critical_threshold: 95,
            service_critical_threshold: 95,
            notification_endpoints: Vec::new(),
            quota_violations: Arc::new(RwLock::new(HashMap::new())),
            auto_adjust_quotas: false,
            monitoring_manager: None,
            grace_periods: Arc::new(RwLock::new(HashMap::new())),
            default_grace_period: 24 * 60 * 60, // 24 hours in seconds
        }
    }

    /// Set warning threshold for container quota alerts (percentage)
    pub fn set_container_warning_threshold(&mut self, threshold: u32) {
        self.container_warning_threshold = threshold;
    }
    
    /// Set monitoring manager for metrics collection
    pub fn with_monitoring_manager(mut self, monitoring_manager: Arc<MonitoringManager>) -> Self {
        self.monitoring_manager = Some(monitoring_manager);
        self
    }
    
    /// Add notification endpoint for quota alerts
    pub fn add_notification_endpoint(&mut self, endpoint: String) {
        self.notification_endpoints.push(endpoint);
    }
    
    /// Enable or disable automatic quota adjustment
    pub fn set_auto_adjust_quotas(&mut self, enabled: bool) {
        self.auto_adjust_quotas = enabled;
    }

    /// Set warning threshold for service quota alerts (percentage)
    pub fn set_service_warning_threshold(&mut self, threshold: u32) {
        self.service_warning_threshold = threshold;
    }

    /// Set critical threshold for container quota alerts (percentage)
    pub fn set_container_critical_threshold(&mut self, threshold: u32) {
        self.container_critical_threshold = threshold;
    }

    /// Set critical threshold for service quota alerts (percentage)
    pub fn set_service_critical_threshold(&mut self, threshold: u32) {
        self.service_critical_threshold = threshold;
    }

    /// Handle container creation event
    pub async fn handle_container_created(&self, container_id: &str, tenant_id: &str) -> Result<()> {
        debug!("Handling container creation for tenant {}: {}", tenant_id, container_id);
        
        // Register container with tenant
        self.tenant_manager.register_container(tenant_id, container_id).await?;
        
        // Check if tenant is approaching container quota
        self.check_container_quota_threshold(tenant_id).await?;
        
        // Send metrics to monitoring system if available
        if let Some(monitoring) = &self.monitoring_manager {
            if let Some(tenant) = self.tenant_manager.get_tenant(tenant_id).await {
                let container_count = self.tenant_manager.get_container_count(tenant_id).await?;
                let max_containers = tenant.resource_quotas.max_containers.unwrap_or(0);
                
                // Create metrics for container count
                let mut labels = HashMap::new();
                labels.insert("tenant_id".to_string(), tenant_id.to_string());
                labels.insert("resource_type".to_string(), "container".to_string());
                
                let metrics = vec![
                    crate::monitoring::Metric {
                        name: "tenant_container_count".to_string(),
                        value: container_count as f64,
                        labels: labels.clone(),
                        timestamp: Utc::now().timestamp(),
                    },
                    crate::monitoring::Metric {
                        name: "tenant_container_quota_percent".to_string(),
                        value: if max_containers > 0 {
                            (container_count as f64 / max_containers as f64) * 100.0
                        } else {
                            0.0
                        },
                        labels,
                        timestamp: Utc::now().timestamp(),
                    },
                ];
                
                if let Err(e) = monitoring.send_metrics(metrics).await {
                    warn!("Failed to send container metrics: {}", e);
                }
            }
        }
        
        Ok(())
    }

    /// Handle container deletion event
    pub async fn handle_container_deleted(&self, container_id: &str, tenant_id: &str) -> Result<()> {
        debug!("Handling container deletion for tenant {}: {}", tenant_id, container_id);
        
        // Unregister container from tenant
        self.tenant_manager.unregister_container(tenant_id, container_id).await?;
        
        // If auto-adjust quotas is enabled and tenant was in violation, check if they're now compliant
        if self.auto_adjust_quotas {
            self.check_quota_compliance(tenant_id, "container").await?;
        }
        
        // Send metrics to monitoring system if available
        if let Some(monitoring) = &self.monitoring_manager {
            if let Some(tenant) = self.tenant_manager.get_tenant(tenant_id).await {
                let container_count = self.tenant_manager.get_container_count(tenant_id).await?;
                let max_containers = tenant.resource_quotas.max_containers.unwrap_or(0);
                
                // Create metrics for container count
                let mut labels = HashMap::new();
                labels.insert("tenant_id".to_string(), tenant_id.to_string());
                labels.insert("resource_type".to_string(), "container".to_string());
                
                let metrics = vec![
                    crate::monitoring::Metric {
                        name: "tenant_container_count".to_string(),
                        value: container_count as f64,
                        labels: labels.clone(),
                        timestamp: Utc::now().timestamp(),
                    },
                    crate::monitoring::Metric {
                        name: "tenant_container_quota_percent".to_string(),
                        value: if max_containers > 0 {
                            (container_count as f64 / max_containers as f64) * 100.0
                        } else {
                            0.0
                        },
                        labels,
                        timestamp: Utc::now().timestamp(),
                    },
                ];
                
                if let Err(e) = monitoring.send_metrics(metrics).await {
                    warn!("Failed to send container metrics: {}", e);
                }
            }
        }
        
        Ok(())
    }

    /// Handle service creation event
    pub async fn handle_service_created(&self, service_name: &str, tenant_id: &str) -> Result<()> {
        debug!("Handling service creation for tenant {}: {}", tenant_id, service_name);
        
        // Register service with tenant
        self.tenant_manager.register_service(tenant_id, service_name).await?;
        
        // Check if tenant is approaching service quota
        self.check_service_quota_threshold(tenant_id).await?;
        
        // Send metrics to monitoring system if available
        if let Some(monitoring) = &self.monitoring_manager {
            if let Some(tenant) = self.tenant_manager.get_tenant(tenant_id).await {
                let service_count = self.tenant_manager.get_service_count(tenant_id).await?;
                let max_services = tenant.resource_quotas.max_services.unwrap_or(0);
                
                // Create metrics for service count
                let mut labels = HashMap::new();
                labels.insert("tenant_id".to_string(), tenant_id.to_string());
                labels.insert("resource_type".to_string(), "service".to_string());
                
                let metrics = vec![
                    crate::monitoring::Metric {
                        name: "tenant_service_count".to_string(),
                        value: service_count as f64,
                        labels: labels.clone(),
                        timestamp: Utc::now().timestamp(),
                    },
                    crate::monitoring::Metric {
                        name: "tenant_service_quota_percent".to_string(),
                        value: if max_services > 0 {
                            (service_count as f64 / max_services as f64) * 100.0
                        } else {
                            0.0
                        },
                        labels,
                        timestamp: Utc::now().timestamp(),
                    },
                ];
                
                if let Err(e) = monitoring.send_metrics(metrics).await {
                    warn!("Failed to send service metrics: {}", e);
                }
            }
        }
        
        Ok(())
    }

    /// Handle service deletion event
    pub async fn handle_service_deleted(&self, service_name: &str, tenant_id: &str) -> Result<()> {
        debug!("Handling service deletion for tenant {}: {}", tenant_id, service_name);
        
        // Unregister service from tenant
        self.tenant_manager.unregister_service(tenant_id, service_name).await?;
        
        // If auto-adjust quotas is enabled and tenant was in violation, check if they're now compliant
        if self.auto_adjust_quotas {
            self.check_quota_compliance(tenant_id, "service").await?;
        }
        
        // Send metrics to monitoring system if available
        if let Some(monitoring) = &self.monitoring_manager {
            if let Some(tenant) = self.tenant_manager.get_tenant(tenant_id).await {
                let service_count = self.tenant_manager.get_service_count(tenant_id).await?;
                let max_services = tenant.resource_quotas.max_services.unwrap_or(0);
                
                // Create metrics for service count
                let mut labels = HashMap::new();
                labels.insert("tenant_id".to_string(), tenant_id.to_string());
                labels.insert("resource_type".to_string(), "service".to_string());
                
                let metrics = vec![
                    crate::monitoring::Metric {
                        name: "tenant_service_count".to_string(),
                        value: service_count as f64,
                        labels: labels.clone(),
                        timestamp: Utc::now().timestamp(),
                    },
                    crate::monitoring::Metric {
                        name: "tenant_service_quota_percent".to_string(),
                        value: if max_services > 0 {
                            (service_count as f64 / max_services as f64) * 100.0
                        } else {
                            0.0
                        },
                        labels,
                        timestamp: Utc::now().timestamp(),
                    },
                ];
                
                if let Err(e) = monitoring.send_metrics(metrics).await {
                    warn!("Failed to send service metrics: {}", e);
                }
            }
        }
        
        Ok(())
    }

    /// Check if tenant is approaching container quota threshold and send alerts if needed
    async fn check_container_quota_threshold(&self, tenant_id: &str) -> Result<()> {
        // Check if tenant is approaching critical threshold
        if self.tenant_manager.is_approaching_container_quota(tenant_id, self.container_critical_threshold).await? {
            // Send critical alert
            self.send_container_quota_alert(tenant_id, &crate::tenant::QuotaAlertLevel::Critical).await?;
        }
        // Check if tenant is approaching warning threshold
        else if self.tenant_manager.is_approaching_container_quota(tenant_id, self.container_warning_threshold).await? {
            // Send warning alert
            self.send_container_quota_alert(tenant_id, &crate::tenant::QuotaAlertLevel::Warning).await?;
        }
        
        Ok(())
    }

    /// Check if tenant is approaching service quota threshold and send alerts if needed
    async fn check_service_quota_threshold(&self, tenant_id: &str) -> Result<()> {
        // Check if tenant is approaching critical threshold
        if self.tenant_manager.is_approaching_service_quota(tenant_id, self.service_critical_threshold).await? {
            // Send critical alert
            self.send_service_quota_alert(tenant_id, &crate::tenant::QuotaAlertLevel::Critical).await?;
        }
        // Check if tenant is approaching warning threshold
        else if self.tenant_manager.is_approaching_service_quota(tenant_id, self.service_warning_threshold).await? {
            // Send warning alert
            self.send_service_quota_alert(tenant_id, &crate::tenant::QuotaAlertLevel::Warning).await?;
        }
        
        Ok(())
    }

    /// Send container quota alert
    async fn send_container_quota_alert(&self, tenant_id: &str, alert_level: &crate::tenant::QuotaAlertLevel) -> Result<()> {
        // Get tenant details
        let tenant = match self.tenant_manager.get_tenant(tenant_id).await {
            Some(t) => t,
            None => return Ok(()),
        };
        
        // Get current container count
        let container_count = self.tenant_manager.get_container_count(tenant_id).await?;
        
        // Get container quota limit
        let quota_limit = match tenant.resource_quotas.max_containers {
            Some(limit) => limit,
            None => return Ok(()),
        };
        
        // Calculate usage percentage
        let usage_percent = (container_count as f64 / quota_limit as f64) * 100.0;
        
        // Create alert
        let alert = crate::tenant::QuotaAlert {
            tenant_id: tenant_id.to_string(),
            resource_type: "container".to_string(),
            current_usage: container_count,
            quota_limit,
            usage_percent,
            alert_level,
            timestamp: chrono::Utc::now().timestamp(),
        };
        
        // Log alert with proper logging level
        match alert_level {
            crate::tenant::QuotaAlertLevel::Info => {
                info!("Tenant {} is using {}/{} containers ({:.1}%)",
                    tenant_id, container_count, quota_limit, usage_percent);
            },
            crate::tenant::QuotaAlertLevel::Warning => {
                warn!("Tenant {} is approaching container quota limit: {}/{} ({:.1}%)",
                    tenant_id, container_count, quota_limit, usage_percent);
            },
            crate::tenant::QuotaAlertLevel::Critical => {
                error!("Tenant {} is near container quota limit: {}/{} ({:.1}%)",
                    tenant_id, container_count, quota_limit, usage_percent);
            },
        };
        
        // Record violation if warning or critical
        if *alert_level != crate::tenant::QuotaAlertLevel::Info {
            self.record_quota_violation(
                tenant_id,
                "container",
                container_count,
                quota_limit,
                usage_percent,
                alert_level.clone()
            ).await;
        }
        
        // Send to notification endpoints if configured
        if !self.notification_endpoints.is_empty() {
            let level_str = match alert_level {
                crate::tenant::QuotaAlertLevel::Info => "INFO",
                crate::tenant::QuotaAlertLevel::Warning => "WARNING",
                crate::tenant::QuotaAlertLevel::Critical => "CRITICAL",
            };
            
            let message = format!(
                "{}: Tenant {} container quota alert: {}/{} ({:.1}%)",
                level_str, tenant_id, container_count, quota_limit, usage_percent
            );
            
            // In a real system, we would send this to each notification endpoint
            for endpoint in &self.notification_endpoints {
                debug!("Sending container quota alert to endpoint {}: {}", endpoint, message);
                // Here we would make an actual API call to the notification endpoint
            }
        }
        
        // Send metrics to monitoring system if available
        if let Some(monitoring) = &self.monitoring_manager {
            let mut labels = HashMap::new();
            labels.insert("tenant_id".to_string(), tenant_id.to_string());
            labels.insert("resource_type".to_string(), "container".to_string());
            labels.insert("alert_level".to_string(), format!("{:?}", alert_level));
            
            let metrics = vec![
                crate::monitoring::Metric {
                    name: "tenant_quota_alert".to_string(),
                    value: match alert_level {
                        crate::tenant::QuotaAlertLevel::Info => 0.0,
                        crate::tenant::QuotaAlertLevel::Warning => 1.0,
                        crate::tenant::QuotaAlertLevel::Critical => 2.0,
                    },
                    labels,
                    timestamp: Utc::now().timestamp(),
                },
            ];
            
            if let Err(e) = monitoring.send_metrics(metrics).await {
                warn!("Failed to send quota alert metrics: {}", e);
            }
        }
        
        Ok(())
    }

    /// Send service quota alert
    async fn send_service_quota_alert(&self, tenant_id: &str, alert_level: &crate::tenant::QuotaAlertLevel) -> Result<()> {
        // Get tenant details
        let tenant = match self.tenant_manager.get_tenant(tenant_id).await {
            Some(t) => t,
            None => return Ok(()),
        };
        
        // Get current service count
        let service_count = self.tenant_manager.get_service_count(tenant_id).await?;
        
        // Get service quota limit
        let quota_limit = match tenant.resource_quotas.max_services {
            Some(limit) => limit,
            None => return Ok(()),
        };
        
        // Calculate usage percentage
        let usage_percent = (service_count as f64 / quota_limit as f64) * 100.0;
        
        // Create alert
        let alert = crate::tenant::QuotaAlert {
            tenant_id: tenant_id.to_string(),
            resource_type: "service".to_string(),
            current_usage: service_count,
            quota_limit,
            usage_percent,
            alert_level,
            timestamp: chrono::Utc::now().timestamp(),
        };
        
        // Log alert with proper logging level
        match alert_level {
            crate::tenant::QuotaAlertLevel::Info => {
                info!("Tenant {} is using {}/{} services ({:.1}%)",
                    tenant_id, service_count, quota_limit, usage_percent);
            },
            crate::tenant::QuotaAlertLevel::Warning => {
                warn!("Tenant {} is approaching service quota limit: {}/{} ({:.1}%)",
                    tenant_id, service_count, quota_limit, usage_percent);
            },
            crate::tenant::QuotaAlertLevel::Critical => {
                error!("Tenant {} is near service quota limit: {}/{} ({:.1}%)",
                    tenant_id, service_count, quota_limit, usage_percent);
            },
        };
        
        // Record violation if warning or critical
        if *alert_level != crate::tenant::QuotaAlertLevel::Info {
            self.record_quota_violation(
                tenant_id,
                "service",
                service_count,
                quota_limit,
                usage_percent,
                alert_level.clone()
            ).await;
        }
        
        /// Record a quota violation for tracking and analysis
        async fn record_quota_violation(
            &self,
            tenant_id: &str,
            resource_type: &str,
            current_usage: u32,
            quota_limit: u32,
            usage_percent: f64,
            alert_level: crate::tenant::QuotaAlertLevel
        ) {
            // Create violation record
            let violation = QuotaViolation {
                resource_type: resource_type.to_string(),
                current_usage,
                quota_limit,
                usage_percent,
                alert_level,
                timestamp: Utc::now().timestamp(),
            };
            
            // Add to tenant's violation history
            let mut violations = self.quota_violations.write().await;
            
            if let Some(tenant_violations) = violations.get_mut(tenant_id) {
                // Add to existing tenant violations
                tenant_violations.push(violation);
                
                // Keep only the most recent violations (limit to 100 per tenant)
                if tenant_violations.len() > 100 {
                    tenant_violations.sort_by(|a, b| b.timestamp.cmp(&a.timestamp));
                    tenant_violations.truncate(100);
                }
            } else {
                // Create new entry for this tenant
                violations.insert(tenant_id.to_string(), vec![violation]);
            }
            
            /// Get quota violation history for a tenant
            pub async fn get_quota_violations(&self, tenant_id: &str) -> Vec<QuotaViolation> {
                let violations = self.quota_violations.read().await;
                
                if let Some(tenant_violations) = violations.get(tenant_id) {
                    tenant_violations.clone()
                } else {
                    Vec::new()
                }
            }
            
            /// Check if a tenant is currently in violation of any quotas
            pub async fn is_in_quota_violation(&self, tenant_id: &str) -> Result<bool> {
                // Check container quota
                if let Some(tenant) = self.tenant_manager.get_tenant(tenant_id).await {
                    if let Some(max_containers) = tenant.resource_quotas.max_containers {
                        let container_count = self.tenant_manager.get_container_count(tenant_id).await?;
                        if container_count >= max_containers {
                            return Ok(true);
                        }
                    }
                    
                    // Check service quota
                    if let Some(max_services) = tenant.resource_quotas.max_services {
                        let service_count = self.tenant_manager.get_service_count(tenant_id).await?;
                        if service_count >= max_services {
                            return Ok(true);
                        }
                    }
                }
                
                Ok(false)
            }
            
            /// Automatically adjust quota based on usage patterns
            /// This method implements a simple algorithm that increases quotas for tenants
            /// that consistently approach their limits without violating them
            pub async fn auto_adjust_tenant_quota(&self, tenant_id: &str) -> Result<()> {
                // Skip if auto-adjust is disabled
                if !self.auto_adjust_quotas {
                    return Ok(());
                }
                
                // Get tenant details
                let tenant = match self.tenant_manager.get_tenant(tenant_id).await {
                    Some(t) => t,
                    None => return Ok(()),
                };
                
                // Get violation history
                let violations = self.get_quota_violations(tenant_id).await;
                
                // Get current usage
                let container_count = self.tenant_manager.get_container_count(tenant_id).await?;
                let service_count = self.tenant_manager.get_service_count(tenant_id).await?;
                
                // Check container quota for adjustment
                if let Some(max_containers) = tenant.resource_quotas.max_containers {
                    let container_usage_percent = (container_count as f64 / max_containers as f64) * 100.0;
                    
                    // If usage is consistently high but not in violation, increase quota
                    if container_usage_percent > self.container_warning_threshold as f64 &&
                       container_usage_percent < self.container_critical_threshold as f64 {
                        
                        // Count how many container violations in the last week
                        let container_violations = violations.iter()
                            .filter(|v| v.resource_type == "container")
                            .filter(|v| (Utc::now().timestamp() - v.timestamp) < 7 * 24 * 60 * 60) // 7 days
                            .count();
                        
                        // If no violations in the last week, increase quota by 10%
                        if container_violations == 0 {
                            let new_limit = (max_containers as f64 * 1.1) as u32;
                            
                            // Update tenant quota
                            self.tenant_manager.update_container_quota(tenant_id, new_limit).await?;
                            
                            info!("Auto-adjusted container quota for tenant {}: {} -> {}",
                                tenant_id, max_containers, new_limit);
                                
                            // Send metrics if monitoring is available
                            if let Some(monitoring) = &self.monitoring_manager {
                                let mut labels = HashMap::new();
                                labels.insert("tenant_id".to_string(), tenant_id.to_string());
                                labels.insert("resource_type".to_string(), "container".to_string());
                                
                                let metrics = vec![
                                    crate::monitoring::Metric {
                                        name: "tenant_quota_adjustment".to_string(),
                                        value: new_limit as f64,
                                        labels,
                                        timestamp: Utc::now().timestamp(),
                                    },
                                ];
                                
                                if let Err(e) = monitoring.send_metrics(metrics).await {
                                    warn!("Failed to send quota adjustment metrics: {}", e);
                                }
                            }
                        }
                    }
                    
                    /// Set the default grace period duration in seconds
                    pub fn set_default_grace_period(&mut self, seconds: i64) {
                        self.default_grace_period = seconds;
                    }
                    
                    /// Grant a grace period for a tenant to exceed their quota
                    pub async fn grant_quota_grace_period(
                        &self,
                        tenant_id: &str,
                        resource_type: &str,
                        temporary_limit: u32,
                        duration_seconds: Option<i64>
                    ) -> Result<()> {
                        // Get tenant details
                        let tenant = match self.tenant_manager.get_tenant(tenant_id).await {
                            Some(t) => t,
                            None => return Ok(()),
                        };
                        
                        // Get the original quota limit
                        let original_limit = match resource_type {
                            "container" => tenant.resource_quotas.max_containers.unwrap_or(0),
                            "service" => tenant.resource_quotas.max_services.unwrap_or(0),
                            _ => return Ok(()),
                        };
                        
                        // Calculate expiration time
                        let duration = duration_seconds.unwrap_or(self.default_grace_period);
                        let expiration = Utc::now().timestamp() + duration;
                        
                        // Create grace period record
                        let grace_period = QuotaGracePeriod {
                            resource_type: resource_type.to_string(),
                            expiration,
                            original_limit,
                            temporary_limit,
                        };
                        
                        // Store grace period
                        let mut grace_periods = self.grace_periods.write().await;
                        
                        if let Some(tenant_grace_periods) = grace_periods.get_mut(tenant_id) {
                            tenant_grace_periods.insert(resource_type.to_string(), grace_period.clone());
                        } else {
                            let mut tenant_grace_periods = HashMap::new();
                            tenant_grace_periods.insert(resource_type.to_string(), grace_period.clone());
                            grace_periods.insert(tenant_id.to_string(), tenant_grace_periods);
                        }
                        
                        // Apply temporary quota
                        match resource_type {
                            "container" => {
                                self.tenant_manager.update_container_quota(tenant_id, temporary_limit).await?;
                            },
                            "service" => {
                                self.tenant_manager.update_service_quota(tenant_id, temporary_limit).await?;
                            },
                            _ => {},
                        }
                        
                        // Log grace period
                        info!("Granted {} quota grace period for tenant {}: {} -> {} until {}",
                            resource_type, tenant_id, original_limit, temporary_limit,
                            chrono::DateTime::<Utc>::from_timestamp(expiration, 0)
                                .map(|dt| dt.to_rfc3339())
                                .unwrap_or_else(|| expiration.to_string()));
                        
                        // Send metrics if monitoring is available
                        if let Some(monitoring) = &self.monitoring_manager {
                            let mut labels = HashMap::new();
                            labels.insert("tenant_id".to_string(), tenant_id.to_string());
                            labels.insert("resource_type".to_string(), resource_type.to_string());
                            
                            let metrics = vec![
                                crate::monitoring::Metric {
                                    name: "tenant_quota_grace_period".to_string(),
                                    value: duration as f64,
                                    labels,
                                    timestamp: Utc::now().timestamp(),
                                },
                            ];
                            
                            if let Err(e) = monitoring.send_metrics(metrics).await {
                                warn!("Failed to send grace period metrics: {}", e);
                            }
                        }
                        
                        Ok(())
                    }
                    
                    /// Check and process expired grace periods
                    pub async fn process_expired_grace_periods(&self) -> Result<()> {
                        let now = Utc::now().timestamp();
                        let mut expired_grace_periods = Vec::new();
                        
                        // Find expired grace periods
                        {
                            let grace_periods = self.grace_periods.read().await;
                            
                            for (tenant_id, tenant_grace_periods) in grace_periods.iter() {
                                for (resource_type, grace_period) in tenant_grace_periods.iter() {
                                    if grace_period.expiration <= now {
                                        expired_grace_periods.push((
                                            tenant_id.clone(),
                                            resource_type.clone(),
                                            grace_period.clone(),
                                        ));
                                    }
                                }
                            }
                        }
                        
                        // Process expired grace periods
                        for (tenant_id, resource_type, grace_period) in expired_grace_periods {
                            // Restore original quota
                            match resource_type.as_str() {
                                "container" => {
                                    self.tenant_manager.update_container_quota(&tenant_id, grace_period.original_limit).await?;
                                },
                                "service" => {
                                    self.tenant_manager.update_service_quota(&tenant_id, grace_period.original_limit).await?;
                                },
                                _ => {},
                            }
                            
                            // Remove grace period record
                            {
                                let mut grace_periods = self.grace_periods.write().await;
                                
                                if let Some(tenant_grace_periods) = grace_periods.get_mut(&tenant_id) {
                                    tenant_grace_periods.remove(&resource_type);
                                    
                                    // Remove tenant entry if no more grace periods
                                    if tenant_grace_periods.is_empty() {
                                        grace_periods.remove(&tenant_id);
                                    }
                                }
                            }
                            
                            // Log grace period expiration
                            info!("Grace period expired for tenant {} {}: restoring quota to {}",
                                tenant_id, resource_type, grace_period.original_limit);
                                
                            // Send metrics if monitoring is available
                            if let Some(monitoring) = &self.monitoring_manager {
                                let mut labels = HashMap::new();
                                labels.insert("tenant_id".to_string(), tenant_id.clone());
                                labels.insert("resource_type".to_string(), resource_type.clone());
                                
                                let metrics = vec![
                                    crate::monitoring::Metric {
                                        name: "tenant_quota_grace_period_expired".to_string(),
                                        value: 1.0,
                                        labels,
                                        timestamp: now,
                                    },
                                ];
                                
                                if let Err(e) = monitoring.send_metrics(metrics).await {
                                    warn!("Failed to send grace period expiration metrics: {}", e);
                                }
                            }
                        }
                        
                        Ok(())
                    }
                    
                    /// Check if a tenant has an active grace period for a resource type
                    pub async fn has_active_grace_period(&self, tenant_id: &str, resource_type: &str) -> bool {
                        let grace_periods = self.grace_periods.read().await;
                        
                        if let Some(tenant_grace_periods) = grace_periods.get(tenant_id) {
                            if let Some(grace_period) = tenant_grace_periods.get(resource_type) {
                                return grace_period.expiration > Utc::now().timestamp();
                            }
                        }
                        
                        false
                    }
                }
                
                // Check service quota for adjustment
                if let Some(max_services) = tenant.resource_quotas.max_services {
                    let service_usage_percent = (service_count as f64 / max_services as f64) * 100.0;
                    
                    // If usage is consistently high but not in violation, increase quota
                    if service_usage_percent > self.service_warning_threshold as f64 &&
                       service_usage_percent < self.service_critical_threshold as f64 {
                        
                        // Count how many service violations in the last week
                        let service_violations = violations.iter()
                            .filter(|v| v.resource_type == "service")
                            .filter(|v| (Utc::now().timestamp() - v.timestamp) < 7 * 24 * 60 * 60) // 7 days
                            .count();
                        
                        // If no violations in the last week, increase quota by 10%
                        if service_violations == 0 {
                            let new_limit = (max_services as f64 * 1.1) as u32;
                            
                            // Update tenant quota
                            self.tenant_manager.update_service_quota(tenant_id, new_limit).await?;
                            
                            info!("Auto-adjusted service quota for tenant {}: {} -> {}",
                                tenant_id, max_services, new_limit);
                                
                            // Send metrics if monitoring is available
                            if let Some(monitoring) = &self.monitoring_manager {
                                let mut labels = HashMap::new();
                                labels.insert("tenant_id".to_string(), tenant_id.to_string());
                                labels.insert("resource_type".to_string(), "service".to_string());
                                
                                let metrics = vec![
                                    crate::monitoring::Metric {
                                        name: "tenant_quota_adjustment".to_string(),
                                        value: new_limit as f64,
                                        labels,
                                        timestamp: Utc::now().timestamp(),
                                    },
                                ];
                                
                                if let Err(e) = monitoring.send_metrics(metrics).await {
                                    warn!("Failed to send quota adjustment metrics: {}", e);
                                }
                            }
                        }
                    }
                }
                
                Ok(())
            }
            
            // Log the violation
            debug!("Recorded quota violation for tenant {}: {} usage at {:.1}% of limit",
                tenant_id, resource_type, usage_percent);
        }
        
        // Send to notification endpoints if configured
        if !self.notification_endpoints.is_empty() {
            let level_str = match alert_level {
                crate::tenant::QuotaAlertLevel::Info => "INFO",
                crate::tenant::QuotaAlertLevel::Warning => "WARNING",
                crate::tenant::QuotaAlertLevel::Critical => "CRITICAL",
            };
            
            let message = format!(
                "{}: Tenant {} service quota alert: {}/{} ({:.1}%)",
                level_str, tenant_id, service_count, quota_limit, usage_percent
            );
            
            // In a real system, we would send this to each notification endpoint
            for endpoint in &self.notification_endpoints {
                debug!("Sending service quota alert to endpoint {}: {}", endpoint, message);
                // Here we would make an actual API call to the notification endpoint
            }
        }
        
        // Send metrics to monitoring system if available
        if let Some(monitoring) = &self.monitoring_manager {
            let mut labels = HashMap::new();
            labels.insert("tenant_id".to_string(), tenant_id.to_string());
            labels.insert("resource_type".to_string(), "service".to_string());
            labels.insert("alert_level".to_string(), format!("{:?}", alert_level));
            
            let metrics = vec![
                crate::monitoring::Metric {
                    name: "tenant_quota_alert".to_string(),
                    value: match alert_level {
                        crate::tenant::QuotaAlertLevel::Info => 0.0,
                        crate::tenant::QuotaAlertLevel::Warning => 1.0,
                        crate::tenant::QuotaAlertLevel::Critical => 2.0,
                    },
                    labels,
                    timestamp: Utc::now().timestamp(),
                },
            ];
            
            if let Err(e) = monitoring.send_metrics(metrics).await {
                warn!("Failed to send quota alert metrics: {}", e);
            }
        }
        
        Ok(())
    }

    /// Pre-check if a container can be created for a tenant
    pub async fn pre_check_container_creation(&self, tenant_id: &str) -> Result<bool> {
        self.tenant_manager.pre_check_container_creation(tenant_id).await
    }

    /// Pre-check if a service can be created for a tenant
    pub async fn pre_check_service_creation(&self, tenant_id: &str) -> Result<bool> {
        self.tenant_manager.pre_check_service_creation(tenant_id).await
    }
    
    /// Represents a quota violation event
    #[derive(Debug, Clone)]
    struct QuotaViolation {
        resource_type: String,
        current_usage: u32,
        quota_limit: u32,
        usage_percent: f64,
        alert_level: crate::tenant::QuotaAlertLevel,
        timestamp: i64,
    }

    /// Initialize container and service counts for all tenants
    pub async fn initialize_quota_tracking(&self) -> Result<()> {
        // Get all tenants
        let tenants = self.tenant_manager.list_tenants().await;
        
        // Get all containers
        let app_manager = self.app_manager.read().await;
        let containers = match app_manager.get_container_details().await {
            Ok(containers) => containers,
            Err(_) => Vec::new(),
        };
        
        // Map containers to tenants based on namespaces
        for tenant in &tenants {
            let tenant_namespaces: Vec<String> = tenant.namespaces.clone();
            
            // Find containers belonging to this tenant
            for container in &containers {
                let belongs_to_tenant = tenant_namespaces.iter().any(|ns| container.name.contains(ns));
                
                if belongs_to_tenant {
                    // Register container with tenant
                    self.tenant_manager.register_container(&tenant.id, &container.id).await?;
                    
                    // Extract service name from container and register it
                    if let Some(service_name) = container.name.split('-').next() {
                        self.tenant_manager.register_service(&tenant.id, service_name).await?;
                    }
                    
                    /// Check if a tenant is now compliant with their quota after a resource has been deleted
                    /// This is used when auto_adjust_quotas is enabled to detect when a tenant returns to compliance
                    async fn check_quota_compliance(&self, tenant_id: &str, resource_type: &str) -> Result<()> {
                        // Get tenant details
                        let tenant = match self.tenant_manager.get_tenant(tenant_id).await {
                            Some(t) => t,
                            None => return Ok(()),
                        };
                        
                        // Check if this tenant has any recorded violations
                        let has_violations = {
                            let violations = self.quota_violations.read().await;
                            violations.contains_key(tenant_id)
                        };
                        
                        if !has_violations {
                            return Ok(());
                        }
                        
                        // Check current usage against quota
                        match resource_type {
                            "container" => {
                                let container_count = self.tenant_manager.get_container_count(tenant_id).await?;
                                if let Some(max_containers) = tenant.resource_quotas.max_containers {
                                    let usage_percent = (container_count as f64 / max_containers as f64) * 100.0;
                                    
                                    // If usage is now below warning threshold, tenant is compliant
                                    if usage_percent < self.container_warning_threshold as f64 {
                                        // Remove violation records for this resource type
                                        self.remove_quota_violation(tenant_id, "container").await;
                                        
                                        // Log compliance
                                        info!("Tenant {} is now compliant with container quota: {}/{} ({:.1}%)",
                                            tenant_id, container_count, max_containers, usage_percent);
                                            
                                        // Send notification if endpoints are configured
                                        self.send_compliance_notification(
                                            tenant_id,
                                            "container",
                                            container_count,
                                            max_containers,
                                            usage_percent
                                        ).await?;
                                    }
                                }
                            },
                            "service" => {
                                let service_count = self.tenant_manager.get_service_count(tenant_id).await?;
                                if let Some(max_services) = tenant.resource_quotas.max_services {
                                    let usage_percent = (service_count as f64 / max_services as f64) * 100.0;
                                    
                                    // If usage is now below warning threshold, tenant is compliant
                                    if usage_percent < self.service_warning_threshold as f64 {
                                        // Remove violation records for this resource type
                                        self.remove_quota_violation(tenant_id, "service").await;
                                        
                                        // Log compliance
                                        info!("Tenant {} is now compliant with service quota: {}/{} ({:.1}%)",
                                            tenant_id, service_count, max_services, usage_percent);
                                            
                                        // Send notification if endpoints are configured
                                        self.send_compliance_notification(
                                            tenant_id,
                                            "service",
                                            service_count,
                                            max_services,
                                            usage_percent
                                        ).await?;
                                    }
                                }
                            },
                            _ => warn!("Unknown resource type: {}", resource_type),
                        }
                        
                        Ok(())
                    }
                    
                    /// Remove a quota violation record for a tenant and resource type
                    async fn remove_quota_violation(&self, tenant_id: &str, resource_type: &str) {
                        let mut violations = self.quota_violations.write().await;
                        
                        if let Some(tenant_violations) = violations.get_mut(tenant_id) {
                            // Remove violations of this resource type
                            tenant_violations.retain(|v| v.resource_type != resource_type);
                            
                            // If no violations remain, remove the tenant entry
                            if tenant_violations.is_empty() {
                                violations.remove(tenant_id);
                            }
                        }
                    }
                    
                    /// Send notification that a tenant is now compliant with their quota
                    async fn send_compliance_notification(
                        &self,
                        tenant_id: &str,
                        resource_type: &str,
                        current_usage: u32,
                        quota_limit: u32,
                        usage_percent: f64
                    ) -> Result<()> {
                        // Skip if no notification endpoints are configured
                        if self.notification_endpoints.is_empty() {
                            return Ok(());
                        }
                        
                        let message = format!(
                            "COMPLIANCE: Tenant {} is now within {} quota limits: {}/{} ({:.1}%)",
                            tenant_id, resource_type, current_usage, quota_limit, usage_percent
                        );
                        
                        // In a real system, we would send this to notification endpoints
                        // For now, we just log it
                        info!("{}", message);
                        
                        // If monitoring is available, send a compliance event
                        if let Some(monitoring) = &self.monitoring_manager {
                            let mut labels = HashMap::new();
                            labels.insert("tenant_id".to_string(), tenant_id.to_string());
                            labels.insert("resource_type".to_string(), resource_type.to_string());
                            
                            let metrics = vec![
                                crate::monitoring::Metric {
                                    name: "tenant_quota_compliance".to_string(),
                                    value: 1.0, // 1 = compliant
                                    labels,
                                    timestamp: Utc::now().timestamp(),
                                },
                            ];
                            
                            if let Err(e) = monitoring.send_metrics(metrics).await {
                                warn!("Failed to send compliance metrics: {}", e);
                            }
                        }
                        
                        Ok(())
                    }
                }
            }
        }
        
        Ok(())
        /// Run periodic maintenance tasks
        /// This should be called regularly (e.g., every hour) to perform maintenance tasks
        pub async fn run_maintenance(&self) -> Result<()> {
            debug!("Running TenantQuotaEnforcer maintenance tasks");
            
            // Process expired grace periods
            self.process_expired_grace_periods().await?;
            
            // Auto-adjust quotas for all tenants if enabled
            if self.auto_adjust_quotas {
                let tenants = self.tenant_manager.list_tenants().await;
                
                for tenant in tenants {
                    if let Err(e) = self.auto_adjust_tenant_quota(&tenant.id).await {
                        warn!("Failed to auto-adjust quota for tenant {}: {}", tenant.id, e);
                    }
                }
            }
            
            // Clean up old quota violation records
            self.cleanup_old_violations().await;
            
            info!("TenantQuotaEnforcer maintenance completed");
            
            Ok(())
        }
        
        /// Clean up old quota violation records
        /// This removes violation records older than 30 days
        async fn cleanup_old_violations(&self) {
            let now = Utc::now().timestamp();
            let thirty_days_ago = now - (30 * 24 * 60 * 60); // 30 days in seconds
            let mut tenants_to_remove = Vec::new();
            
            // Find old violations to remove
            let mut violations = self.quota_violations.write().await;
            
            for (tenant_id, tenant_violations) in violations.iter_mut() {
                // Remove old violations
                let original_count = tenant_violations.len();
                tenant_violations.retain(|v| v.timestamp > thirty_days_ago);
                let removed_count = original_count - tenant_violations.len();
                
                if removed_count > 0 {
                    debug!("Removed {} old quota violations for tenant {}", removed_count, tenant_id);
                }
                
                // Mark empty tenant entries for removal
                if tenant_violations.is_empty() {
                    tenants_to_remove.push(tenant_id.clone());
                }
            }
            
            // Remove empty tenant entries
            for tenant_id in tenants_to_remove {
                violations.remove(&tenant_id);
            }
        }
    }
}