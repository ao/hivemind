use log::{info, warn, error, debug, trace};
use std::sync::Once;
use env_logger::Builder;
use std::io::Write;
use chrono::Local;

static INIT: Once = Once::new();

/// Initialize the logging system
pub fn init_logging() {
    INIT.call_once(|| {
        let mut builder = Builder::from_default_env();
        builder.format(|buf, record| {
            writeln!(
                buf,
                "{} [{}] - {}: {}",
                Local::now().format("%Y-%m-%d %H:%M:%S"),
                record.level(),
                record.target(),
                record.args()
            )
        });
        builder.init();
    });
}

/// Log resource usage information
pub fn log_resource_usage(tenant_id: &str, cpu: u32, memory: u64, storage: u64, containers: u32, services: u32) {
    info!(
        "Resource usage for tenant {}: CPU: {}%, Memory: {} bytes, Storage: {} bytes, Containers: {}, Services: {}",
        tenant_id, cpu, memory, storage, containers, services
    );
}

/// Log resource quota violation
pub fn log_quota_violation(tenant_id: &str, resource_type: &str, current: u64, limit: u64) {
    warn!(
        "Quota violation for tenant {}: {} usage {} exceeds limit {}",
        tenant_id, resource_type, current, limit
    );
}

/// Log resource approaching quota
pub fn log_approaching_quota(tenant_id: &str, resource_type: &str, current: u64, limit: u64, percent: f64) {
    warn!(
        "Tenant {} is approaching {} quota limit: {} of {} ({:.1}%)",
        tenant_id, resource_type, current, limit, percent
    );
}

/// Log container runtime error
pub fn log_container_runtime_error(tenant_id: &str, error: &str) {
    error!(
        "Container runtime error for tenant {}: {}",
        tenant_id, error
    );
}

/// Log cache hit
pub fn log_cache_hit(tenant_id: &str, age_seconds: i64) {
    debug!(
        "Cache hit for tenant {} resource usage (age: {} seconds)",
        tenant_id, age_seconds
    );
}

/// Log cache update
pub fn log_cache_update(tenant_id: &str) {
    debug!(
        "Updated resource usage cache for tenant {}",
        tenant_id
    );
}

/// Log external monitoring system integration
pub fn log_external_monitoring(tenant_id: &str, system_name: &str, success: bool) {
    if success {
        info!(
            "Successfully sent metrics for tenant {} to external monitoring system {}",
            tenant_id, system_name
        );
    } else {
        error!(
            "Failed to send metrics for tenant {} to external monitoring system {}",
            tenant_id, system_name
        );
    }
}