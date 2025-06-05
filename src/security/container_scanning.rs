use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::RwLock;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

/// Represents a vulnerability found in a container image
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Vulnerability {
    pub id: String,
    pub name: String,
    pub description: String,
    pub severity: VulnerabilitySeverity,
    pub affected_package: String,
    pub affected_version: String,
    pub fixed_version: Option<String>,
    pub cve_id: Option<String>,
    pub discovered_at: i64,
}

/// Severity levels for vulnerabilities
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, PartialOrd, Ord)]
pub enum VulnerabilitySeverity {
    Unknown,
    Low,
    Medium,
    High,
    Critical,
}

/// Results of a container image scan
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ScanResult {
    pub image_id: String,
    pub image_name: String,
    pub scan_time: i64,
    pub vulnerabilities: Vec<Vulnerability>,
    pub compliant: bool,
    pub scan_status: ScanStatus,
    pub scan_errors: Vec<String>,
}

/// Status of a container scan
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub enum ScanStatus {
    Pending,
    InProgress,
    Completed,
    Failed,
}

/// Security policy for container images
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SecurityPolicy {
    pub id: String,
    pub name: String,
    pub description: String,
    pub max_severity: VulnerabilitySeverity,
    pub block_on_severity: VulnerabilitySeverity,
    pub whitelist_cves: Vec<String>,
    pub blacklist_cves: Vec<String>,
    pub allowed_registries: Vec<String>,
    pub required_labels: HashMap<String, String>,
    pub scan_frequency: Duration,
    pub created_at: i64,
    pub updated_at: i64,
}

/// Security event for logging and alerting
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SecurityEvent {
    pub id: String,
    pub event_type: SecurityEventType,
    pub source: String,
    pub timestamp: i64,
    pub message: String,
    pub details: HashMap<String, String>,
    pub severity: SecurityEventSeverity,
}

/// Types of security events
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum SecurityEventType {
    VulnerabilityFound,
    PolicyViolation,
    ScanCompleted,
    ScanFailed,
    RuntimeViolation,
    UnauthorizedAccess,
}

/// Severity levels for security events
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum SecurityEventSeverity {
    Info,
    Warning,
    Error,
    Critical,
}

/// Container runtime security check result
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RuntimeCheckResult {
    pub container_id: String,
    pub timestamp: i64,
    pub violations: Vec<RuntimeViolation>,
    pub compliant: bool,
}

/// Runtime security violation
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RuntimeViolation {
    pub rule_id: String,
    pub description: String,
    pub severity: SecurityEventSeverity,
    pub details: HashMap<String, String>,
    pub timestamp: i64,
}

/// Container Scanner manages image scanning and runtime security checks
pub struct ContainerScanner {
    scan_results: Arc<RwLock<HashMap<String, ScanResult>>>,
    security_policies: Arc<RwLock<HashMap<String, SecurityPolicy>>>,
    security_events: Arc<RwLock<Vec<SecurityEvent>>>,
    runtime_checks: Arc<RwLock<HashMap<String, RuntimeCheckResult>>>,
}

impl ContainerScanner {
    pub fn new() -> Self {
        Self {
            scan_results: Arc::new(RwLock::new(HashMap::new())),
            security_policies: Arc::new(RwLock::new(HashMap::new())),
            security_events: Arc::new(RwLock::new(Vec::new())),
            runtime_checks: Arc::new(RwLock::new(HashMap::new())),
        }
    }

    /// Scan a container image for vulnerabilities
    pub async fn scan_image(&self, image_name: &str, image_id: &str) -> Result<ScanResult> {
        println!("Scanning image: {}", image_name);
        
        // Create a pending scan result
        let mut scan_result = ScanResult {
            image_id: image_id.to_string(),
            image_name: image_name.to_string(),
            scan_time: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            vulnerabilities: Vec::new(),
            compliant: false,
            scan_status: ScanStatus::Pending,
            scan_errors: Vec::new(),
        };
        
        // Update status to in progress
        scan_result.scan_status = ScanStatus::InProgress;
        
        // In a real implementation, we would integrate with a vulnerability scanner like Trivy, Clair, or Anchore
        // For now, we'll simulate a scan with some sample vulnerabilities
        
        // Simulate scanning delay
        tokio::time::sleep(Duration::from_millis(500)).await;
        
        // Generate some sample vulnerabilities based on the image name
        let vulnerabilities = self.simulate_vulnerabilities(image_name);
        scan_result.vulnerabilities = vulnerabilities;
        
        // Check if the image is compliant with security policies
        scan_result.compliant = self.check_compliance(&scan_result).await?;
        
        // Update status to completed
        scan_result.scan_status = ScanStatus::Completed;
        
        // Store the scan result
        let mut results = self.scan_results.write().await;
        results.insert(image_id.to_string(), scan_result.clone());
        
        // Generate security events for any vulnerabilities found
        self.generate_vulnerability_events(&scan_result).await?;
        
        Ok(scan_result)
    }
    
    /// Simulate vulnerabilities for testing purposes
    fn simulate_vulnerabilities(&self, image_name: &str) -> Vec<Vulnerability> {
        let mut vulnerabilities = Vec::new();
        
        // Generate a deterministic number of vulnerabilities based on the image name
        let vuln_count = (image_name.bytes().map(|b| b as u32).sum::<u32>() % 10) as usize;
        
        for i in 0..vuln_count {
            let severity = match i % 5 {
                0 => VulnerabilitySeverity::Critical,
                1 => VulnerabilitySeverity::High,
                2 => VulnerabilitySeverity::Medium,
                3 => VulnerabilitySeverity::Low,
                _ => VulnerabilitySeverity::Unknown,
            };
            
            vulnerabilities.push(Vulnerability {
                id: format!("VULN-{}-{}", i, image_name.len()),
                name: format!("Sample Vulnerability {}", i),
                description: format!("This is a simulated vulnerability for testing in image {}", image_name),
                severity,
                affected_package: format!("package-{}", i),
                affected_version: format!("1.{}.0", i),
                fixed_version: Some(format!("1.{}.1", i)),
                cve_id: Some(format!("CVE-2023-{}", 1000 + i)),
                discovered_at: SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64,
            });
        }
        
        vulnerabilities
    }
    
    /// Check if an image is compliant with security policies
    pub async fn check_compliance(&self, scan_result: &ScanResult) -> Result<bool> {
        let policies = self.security_policies.read().await;
        
        // If no policies are defined, default to non-compliant
        if policies.is_empty() {
            return Ok(false);
        }
        
        // Check against each policy
        for policy in policies.values() {
            // Check for critical or high severity vulnerabilities
            if scan_result.vulnerabilities.iter().any(|v| {
                v.severity >= policy.block_on_severity && 
                !policy.whitelist_cves.contains(&v.cve_id.clone().unwrap_or_default())
            }) {
                return Ok(false);
            }
            
            // Check for blacklisted CVEs
            if scan_result.vulnerabilities.iter().any(|v| {
                v.cve_id.as_ref().map_or(false, |cve| policy.blacklist_cves.contains(cve))
            }) {
                return Ok(false);
            }
            
            // Check for allowed registries
            if !policy.allowed_registries.is_empty() {
                let registry = scan_result.image_name.split('/').next().unwrap_or("");
                if !policy.allowed_registries.iter().any(|r| registry.starts_with(r)) {
                    return Ok(false);
                }
            }
        }
        
        Ok(true)
    }
    
    /// Create a new security policy
    pub async fn create_policy(&self, policy: SecurityPolicy) -> Result<()> {
        let mut policies = self.security_policies.write().await;
        policies.insert(policy.id.clone(), policy);
        Ok(())
    }
    
    /// Get a security policy by ID
    pub async fn get_policy(&self, policy_id: &str) -> Option<SecurityPolicy> {
        let policies = self.security_policies.read().await;
        policies.get(policy_id).cloned()
    }
    
    /// List all security policies
    pub async fn list_policies(&self) -> Vec<SecurityPolicy> {
        let policies = self.security_policies.read().await;
        policies.values().cloned().collect()
    }
    
    /// Delete a security policy
    pub async fn delete_policy(&self, policy_id: &str) -> Result<()> {
        let mut policies = self.security_policies.write().await;
        policies.remove(policy_id);
        Ok(())
    }
    
    /// Perform a runtime security check on a running container
    pub async fn check_container_runtime(&self, container_id: &str) -> Result<RuntimeCheckResult> {
        println!("Performing runtime security check on container: {}", container_id);
        
        // In a real implementation, we would integrate with a runtime security tool like Falco
        // For now, we'll simulate a runtime check
        
        let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;
        
        // Simulate some violations based on container ID
        let violations = self.simulate_runtime_violations(container_id);
        
        let result = RuntimeCheckResult {
            container_id: container_id.to_string(),
            timestamp: now,
            violations: violations.clone(),
            compliant: violations.is_empty(),
        };
        
        // Store the result
        let mut checks = self.runtime_checks.write().await;
        checks.insert(container_id.to_string(), result.clone());
        
        // Generate security events for any violations
        for violation in &result.violations {
            self.log_security_event(
                SecurityEventType::RuntimeViolation,
                container_id,
                &violation.description,
                violation.severity.clone(),
                HashMap::new(),
            ).await?;
        }
        
        Ok(result)
    }
    
    /// Simulate runtime violations for testing purposes
    fn simulate_runtime_violations(&self, container_id: &str) -> Vec<RuntimeViolation> {
        let mut violations = Vec::new();
        
        // Generate a deterministic number of violations based on the container ID
        let violation_count = (container_id.bytes().map(|b| b as u32).sum::<u32>() % 3) as usize;
        
        for i in 0..violation_count {
            let severity = match i % 3 {
                0 => SecurityEventSeverity::Critical,
                1 => SecurityEventSeverity::Error,
                _ => SecurityEventSeverity::Warning,
            };
            
            let mut details = HashMap::new();
            details.insert("process".to_string(), format!("/bin/suspicious{}", i));
            details.insert("user".to_string(), "root".to_string());
            
            violations.push(RuntimeViolation {
                rule_id: format!("RULE-{}", i),
                description: format!("Suspicious activity detected in container {}", container_id),
                severity,
                details,
                timestamp: SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64,
            });
        }
        
        violations
    }
    
    /// Generate security events for vulnerabilities found in a scan
    async fn generate_vulnerability_events(&self, scan_result: &ScanResult) -> Result<()> {
        for vulnerability in &scan_result.vulnerabilities {
            if vulnerability.severity >= VulnerabilitySeverity::High {
                let mut details = HashMap::new();
                details.insert("image_id".to_string(), scan_result.image_id.clone());
                details.insert("image_name".to_string(), scan_result.image_name.clone());
                details.insert("package".to_string(), vulnerability.affected_package.clone());
                details.insert("version".to_string(), vulnerability.affected_version.clone());
                
                if let Some(cve) = &vulnerability.cve_id {
                    details.insert("cve_id".to_string(), cve.clone());
                }
                
                let severity = match vulnerability.severity {
                    VulnerabilitySeverity::Critical => SecurityEventSeverity::Critical,
                    VulnerabilitySeverity::High => SecurityEventSeverity::Error,
                    _ => SecurityEventSeverity::Warning,
                };
                
                self.log_security_event(
                    SecurityEventType::VulnerabilityFound,
                    &scan_result.image_id,
                    &format!("{}: {}", vulnerability.name, vulnerability.description),
                    severity,
                    details,
                ).await?;
            }
        }
        
        Ok(())
    }
    
    /// Log a security event
    pub async fn log_security_event(
        &self,
        event_type: SecurityEventType,
        source: &str,
        message: &str,
        severity: SecurityEventSeverity,
        details: HashMap<String, String>,
    ) -> Result<()> {
        let event = SecurityEvent {
            id: format!("EVENT-{}-{}", 
                SystemTime::now().duration_since(UNIX_EPOCH)?.as_nanos() % 10000,
                source.len()),
            event_type,
            source: source.to_string(),
            timestamp: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            message: message.to_string(),
            details,
            severity,
        };
        
        let mut events = self.security_events.write().await;
        events.push(event);
        
        // In a real implementation, we would also send alerts for critical events
        
        Ok(())
    }
    
    /// Get security events
    pub async fn get_security_events(&self) -> Vec<SecurityEvent> {
        let events = self.security_events.read().await;
        events.clone()
    }
    
    /// Get scan result for an image
    pub async fn get_scan_result(&self, image_id: &str) -> Option<ScanResult> {
        let results = self.scan_results.read().await;
        results.get(image_id).cloned()
    }
    
    /// Get runtime check result for a container
    pub async fn get_runtime_check_result(&self, container_id: &str) -> Option<RuntimeCheckResult> {
        let checks = self.runtime_checks.read().await;
        checks.get(container_id).cloned()
    }
}