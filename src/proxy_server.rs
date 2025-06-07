//! Proxy Server for Service Discovery
//!
//! This module provides an enhanced proxy server implementation for the service discovery system.
//! It includes features like circuit breaking, monitoring integration, request/response metrics,
//! and proper logging.

use crate::logging;
use crate::resilience::{
    CircuitBreaker, CircuitBreakerConfig, CircuitBreakerState,
    Bulkhead, BulkheadConfig,
};
use crate::service_discovery::{
    ServiceDiscovery, ServiceEndpoint, ServiceHealth, 
    LoadBalancingStrategy
};
use anyhow::{Result, anyhow};
use hyper::{Body, Client, Request, Response, Server, StatusCode};
use hyper::service::{make_service_fn, service_fn};
use hyper::client::HttpConnector;
use hyper_tls::HttpsConnector;
use log::{debug, error, info, warn};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::convert::Infallible;
use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tokio::sync::{Mutex, RwLock};
use tokio::time::timeout;
use uuid::Uuid;

/// Proxy server configuration
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ProxyServerConfig {
    /// Port to listen on
    pub port: u16,
    /// Maximum number of concurrent connections
    pub max_connections: u32,
    /// Connection timeout in milliseconds
    pub connection_timeout_ms: u64,
    /// Request timeout in milliseconds
    pub request_timeout_ms: u64,
    /// Enable HTTPS
    pub enable_https: bool,
    /// Enable request/response logging
    pub enable_request_logging: bool,
    /// Enable metrics collection
    pub enable_metrics: bool,
    /// Enable circuit breaking
    pub enable_circuit_breaking: bool,
    /// Metrics reporting interval in seconds
    pub metrics_reporting_interval_seconds: u64,
}

impl Default for ProxyServerConfig {
    fn default() -> Self {
        Self {
            port: 8080,
            max_connections: 1000,
            connection_timeout_ms: 30000,
            request_timeout_ms: 30000,
            enable_https: false,
            enable_request_logging: true,
            enable_metrics: true,
            enable_circuit_breaking: true,
            metrics_reporting_interval_seconds: 60,
        }
    }
}

/// Request metrics
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct RequestMetrics {
    /// Total requests
    pub total_requests: u64,
    /// Successful requests
    pub successful_requests: u64,
    /// Failed requests
    pub failed_requests: u64,
    /// Rejected requests (circuit breaker, etc.)
    pub rejected_requests: u64,
    /// Average response time in milliseconds
    pub avg_response_time_ms: f64,
    /// Status code counts
    pub status_codes: HashMap<u16, u64>,
    /// Circuit breaker open count
    pub circuit_breaker_open_count: u64,
    /// Last updated timestamp
    pub last_updated: u64,
}

/// Proxy server implementation
pub struct ProxyServer {
    /// Service discovery instance
    service_discovery: Arc<ServiceDiscovery>,
    /// Configuration
    config: RwLock<ProxyServerConfig>,
    /// HTTP client
    client: Client<HttpsConnector<HttpConnector>>,
    /// Request metrics
    metrics: Arc<RwLock<HashMap<String, RequestMetrics>>>,
    /// Circuit breakers by service
    circuit_breakers: Arc<RwLock<HashMap<String, Arc<CircuitBreaker>>>>,
    /// Active connections counter
    active_connections: Arc<Mutex<u32>>,
}

impl ProxyServer {
    /// Create a new proxy server
    pub fn new(service_discovery: Arc<ServiceDiscovery>) -> Self {
        // Create HTTPS connector
        let https = HttpsConnector::new();
        let client = Client::builder().build::<_, Body>(https);
        
        Self {
            service_discovery,
            config: RwLock::new(ProxyServerConfig::default()),
            client,
            metrics: Arc::new(RwLock::new(HashMap::new())),
            circuit_breakers: Arc::new(RwLock::new(HashMap::new())),
            active_connections: Arc::new(Mutex::new(0)),
        }
    }
    
    /// Configure the proxy server
    pub async fn configure(&self, config: ProxyServerConfig) {
        *self.config.write().await = config;
    }
    
    /// Start the proxy server
    pub async fn start(&self) -> Result<()> {
        let config = self.config.read().await.clone();
        
        info!("Starting proxy server on port {}", config.port);
        
        // Clone necessary data for the proxy server task
        let service_discovery = self.service_discovery.clone();
        let metrics = self.metrics.clone();
        let circuit_breakers = self.circuit_breakers.clone();
        let active_connections = self.active_connections.clone();
        let client = self.client.clone();
        let config_clone = config.clone();
        
        // Start metrics reporting task
        self.start_metrics_reporting().await;
        
        // Create the server
        let addr = SocketAddr::from(([0, 0, 0, 0], config.port));
        
        // Create a service function that will handle incoming requests
        let make_svc = make_service_fn(move |conn| {
            let remote_addr = conn.remote_addr();
            let service_discovery = service_discovery.clone();
            let metrics = metrics.clone();
            let circuit_breakers = circuit_breakers.clone();
            let active_connections = active_connections.clone();
            let client = client.clone();
            let config = config_clone.clone();
            
            async move {
                Ok::<_, Infallible>(service_fn(move |req: Request<Body>| {
                    let remote_addr = remote_addr;
                    let service_discovery = service_discovery.clone();
                    let metrics = metrics.clone();
                    let circuit_breakers = circuit_breakers.clone();
                    let active_connections = active_connections.clone();
                    let client = client.clone();
                    let config = config.clone();
                    
                    async move {
                        // Increment active connections
                        let mut active = active_connections.lock().await;
                        *active += 1;
                        drop(active);
                        
                        // Generate request ID for tracing
                        let request_id = Uuid::new_v4().to_string();
                        
                        // Start timing the request
                        let start_time = Instant::now();
                        
                        // Process the request
                        let result = Self::process_request(
                            req,
                            &request_id,
                            &remote_addr,
                            &service_discovery,
                            &metrics,
                            &circuit_breakers,
                            &client,
                            &config,
                        ).await;
                        
                        // Calculate request duration
                        let duration = start_time.elapsed();
                        
                        // Log request completion
                        match &result {
                            Ok(response) => {
                                if config.enable_request_logging {
                                    info!(
                                        "Request {} completed with status {} in {:.2}ms",
                                        request_id,
                                        response.status().as_u16(),
                                        duration.as_millis() as f64
                                    );
                                }
                            }
                            Err(e) => {
                                error!(
                                    "Request {} failed: {} in {:.2}ms",
                                    request_id,
                                    e,
                                    duration.as_millis() as f64
                                );
                            }
                        }
                        
                        // Decrement active connections
                        let mut active = active_connections.lock().await;
                        *active -= 1;
                        drop(active);
                        
                        result
                    }
                }))
            }
        });
        
        // Create the server
        let server = Server::bind(&addr).serve(make_svc);
        
        info!("Proxy server listening on http://{}", addr);
        
        // Run the server
        if let Err(e) = server.await {
            error!("Proxy server error: {}", e);
            return Err(anyhow!("Proxy server error: {}", e));
        }
        
        Ok(())
    }
    
    /// Start metrics reporting task
    async fn start_metrics_reporting(&self) {
        let metrics = self.metrics.clone();
        let config = self.config.read().await.clone();
        
        if !config.enable_metrics {
            return;
        }
        
        let interval = config.metrics_reporting_interval_seconds;
        
        tokio::spawn(async move {
            loop {
                // Sleep for the reporting interval
                tokio::time::sleep(Duration::from_secs(interval)).await;
                
                // Get current metrics
                let metrics_lock = metrics.read().await;
                
                // Log metrics for each service
                for (service_name, metrics) in metrics_lock.iter() {
                    info!(
                        "Service {} metrics: total={}, success={}, failed={}, rejected={}, avg_time={:.2}ms",
                        service_name,
                        metrics.total_requests,
                        metrics.successful_requests,
                        metrics.failed_requests,
                        metrics.rejected_requests,
                        metrics.avg_response_time_ms
                    );
                    
                    // Log status code distribution
                    let status_codes: Vec<(u16, u64)> = metrics.status_codes
                        .iter()
                        .map(|(k, v)| (*k, *v))
                        .collect();
                    
                    debug!(
                        "Service {} status codes: {:?}",
                        service_name,
                        status_codes
                    );
                    
                    // Send metrics to external monitoring system
                    logging::log_external_monitoring(
                        service_name,
                        "prometheus",
                        true
                    );
                }
            }
        });
    }
    
    /// Process a request
    #[allow(clippy::too_many_arguments)]
    async fn process_request(
        req: Request<Body>,
        request_id: &str,
        remote_addr: &SocketAddr,
        service_discovery: &ServiceDiscovery,
        metrics: &Arc<RwLock<HashMap<String, RequestMetrics>>>,
        circuit_breakers: &Arc<RwLock<HashMap<String, Arc<CircuitBreaker>>>>,
        client: &Client<HttpsConnector<HttpConnector>>,
        config: &ProxyServerConfig,
    ) -> Result<Response<Body>> {
        // Extract the host from the request
        let host = match req.headers().get(hyper::header::HOST) {
            Some(host) => match host.to_str() {
                Ok(host) => host.split(':').next().unwrap_or("").to_string(),
                Err(_) => {
                    return Ok(Response::builder()
                        .status(StatusCode::BAD_REQUEST)
                        .body(Body::from("Invalid Host header"))
                        .unwrap());
                }
            },
            None => {
                return Ok(Response::builder()
                    .status(StatusCode::BAD_REQUEST)
                    .body(Body::from("Missing Host header"))
                    .unwrap());
            }
        };
        
        // Extract tenant ID from headers if available
        let tenant_id = req.headers()
            .get("X-Tenant-ID")
            .and_then(|h| h.to_str().ok())
            .unwrap_or("default");
        
        // Log request start
        if config.enable_request_logging {
            info!(
                "Request {} from {} to {} {} (tenant: {})",
                request_id,
                remote_addr,
                req.method(),
                req.uri(),
                tenant_id
            );
        }
        
        // Get the path for path-based routing
        let path = req.uri().path();
        
        // Look up the service by domain
        let service_result = service_discovery.get_service_by_domain(&host).await;
        
        if service_result.is_none() {
            // Try path-based routing
            let routing_rules = service_discovery.list_routing_rules().await;
            
            for rule in routing_rules {
                if let Some(path_prefix) = &rule.path_prefix {
                    if path.starts_with(path_prefix) {
                        // Found a matching path-based rule
                        if let Some(endpoints) = service_discovery.get_service_endpoints(&rule.service_name).await {
                            // Process the request for this service
                            return Self::process_service_request(
                                req,
                                request_id,
                                &rule.service_name,
                                endpoints,
                                tenant_id,
                                service_discovery,
                                metrics,
                                circuit_breakers,
                                client,
                                config,
                            ).await;
                        }
                    }
                }
            }
            
            // No service found
            return Ok(Response::builder()
                .status(StatusCode::NOT_FOUND)
                .body(Body::from("Service not found"))
                .unwrap());
        }
        
        let (service_name, endpoints) = service_result.unwrap();
        
        // Process the request for this service
        Self::process_service_request(
            req,
            request_id,
            &service_name,
            endpoints,
            tenant_id,
            service_discovery,
            metrics,
            circuit_breakers,
            client,
            config,
        ).await
    }
    
    /// Process a request for a specific service
    #[allow(clippy::too_many_arguments)]
    async fn process_service_request(
        req: Request<Body>,
        request_id: &str,
        service_name: &str,
        endpoints: Vec<ServiceEndpoint>,
        tenant_id: &str,
        service_discovery: &ServiceDiscovery,
        metrics: &Arc<RwLock<HashMap<String, RequestMetrics>>>,
        circuit_breakers: &Arc<RwLock<HashMap<String, Arc<CircuitBreaker>>>>,
        client: &Client<HttpsConnector<HttpConnector>>,
        config: &ProxyServerConfig,
    ) -> Result<Response<Body>> {
        // Update metrics
        if config.enable_metrics {
            let mut metrics_lock = metrics.write().await;
            let service_metrics = metrics_lock
                .entry(service_name.to_string())
                .or_insert_with(RequestMetrics::default);
            
            service_metrics.total_requests += 1;
            service_metrics.last_updated = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_secs();
        }
        
        // Check circuit breaker if enabled
        if config.enable_circuit_breaking {
            let circuit_breaker = {
                let circuit_breakers_lock = circuit_breakers.read().await;
                circuit_breakers_lock.get(service_name).cloned()
            };
            
            let circuit_breaker = if let Some(cb) = circuit_breaker {
                cb
            } else {
                // Create new circuit breaker
                let cb_config = CircuitBreakerConfig {
                    consecutive_errors_threshold: 5,
                    open_to_half_open_timeout_ms: 30000,
                    half_open_success_threshold: 3,
                    ..CircuitBreakerConfig::default()
                };
                
                let cb = Arc::new(CircuitBreaker::new(service_name.to_string(), cb_config));
                
                // Store the circuit breaker
                let mut circuit_breakers_lock = circuit_breakers.write().await;
                circuit_breakers_lock.insert(service_name.to_string(), cb.clone());
                
                cb
            };
            
            // Check if circuit breaker allows the request
            if !circuit_breaker.allow_request().await {
                // Update metrics
                if config.enable_metrics {
                    let mut metrics_lock = metrics.write().await;
                    let service_metrics = metrics_lock
                        .entry(service_name.to_string())
                        .or_insert_with(RequestMetrics::default);
                    
                    service_metrics.rejected_requests += 1;
                    service_metrics.circuit_breaker_open_count += 1;
                }
                
                warn!("Circuit breaker open for service {}", service_name);
                
                // Record rejected request
                if let Err(e) = circuit_breaker.record_rejected().await {
                    error!("Failed to record rejected request: {}", e);
                }
                
                return Ok(Response::builder()
                    .status(StatusCode::SERVICE_UNAVAILABLE)
                    .body(Body::from("Service unavailable: circuit breaker open"))
                    .unwrap());
            }
        }
        
        // Filter to only healthy endpoints
        let healthy_endpoints: Vec<&ServiceEndpoint> = endpoints
            .iter()
            .filter(|e| e.health_status == ServiceHealth::Healthy)
            .collect();
        
        // If no healthy endpoints, return error
        if healthy_endpoints.is_empty() {
            // Update metrics
            if config.enable_metrics {
                let mut metrics_lock = metrics.write().await;
                let service_metrics = metrics_lock
                    .entry(service_name.to_string())
                    .or_insert_with(RequestMetrics::default);
                
                service_metrics.failed_requests += 1;
            }
            
            // Record failure in circuit breaker
            if config.enable_circuit_breaking {
                if let Some(circuit_breaker) = circuit_breakers.read().await.get(service_name) {
                    if let Err(e) = circuit_breaker.record_failure().await {
                        error!("Failed to record circuit breaker failure: {}", e);
                    }
                }
            }
            
            warn!("No healthy endpoints for service {}", service_name);
            
            return Ok(Response::builder()
                .status(StatusCode::SERVICE_UNAVAILABLE)
                .body(Body::from("No healthy endpoints available"))
                .unwrap());
        }
        
        // Get load balancing strategy for this service
        let load_balancing_strategy = service_discovery
            .get_load_balancing_strategy(service_name)
            .await
            .unwrap_or(LoadBalancingStrategy::RoundRobin);
        
        // Get selected endpoint using service discovery's load balancing
        let selected_endpoint_result = service_discovery
            .get_service_endpoint_with_options(
                service_name,
                Some(load_balancing_strategy),
                None,
                None,
            )
            .await;
        
        if selected_endpoint_result.is_none() {
            // Update metrics
            if config.enable_metrics {
                let mut metrics_lock = metrics.write().await;
                let service_metrics = metrics_lock
                    .entry(service_name.to_string())
                    .or_insert_with(RequestMetrics::default);
                
                service_metrics.failed_requests += 1;
            }
            
            // Record failure in circuit breaker
            if config.enable_circuit_breaking {
                if let Some(circuit_breaker) = circuit_breakers.read().await.get(service_name) {
                    if let Err(e) = circuit_breaker.record_failure().await {
                        error!("Failed to record circuit breaker failure: {}", e);
                    }
                }
            }
            
            error!("Failed to select endpoint for service {}", service_name);
            
            return Ok(Response::builder()
                .status(StatusCode::INTERNAL_SERVER_ERROR)
                .body(Body::from("Failed to select endpoint"))
                .unwrap());
        }
        
        let selected_endpoint = selected_endpoint_result.unwrap();
        
        // Create the forwarding URI
        let uri = format!("http://{}:{}{}",
            selected_endpoint.ip_address,
            selected_endpoint.port,
            req.uri().path_and_query().map_or("", |p| p.as_str())
        );
        
        // Create a new request with the same body but new URI
        let mut new_req = Request::builder()
            .method(req.method().clone())
            .uri(uri);
        
        // Copy headers
        for (name, value) in req.headers() {
            if name != hyper::header::HOST {
                new_req = new_req.header(name, value);
            }
        }
        
        // Add tracing headers
        new_req = new_req.header("X-Request-ID", request_id);
        new_req = new_req.header("X-Forwarded-For", remote_addr.ip().to_string());
        new_req = new_req.header("X-Forwarded-Proto", "http");
        new_req = new_req.header("X-Service-Name", service_name);
        
        // Forward the request
        let start_time = Instant::now();
        
        let result = match new_req.body(req.into_body()) {
            Ok(new_req) => {
                // Apply timeout
                let timeout_duration = Duration::from_millis(config.request_timeout_ms);
                match timeout(timeout_duration, client.request(new_req)).await {
                    Ok(result) => match result {
                        Ok(response) => {
                            // Record success in circuit breaker
                            if config.enable_circuit_breaking {
                                if let Some(circuit_breaker) = circuit_breakers.read().await.get(service_name) {
                                    if let Err(e) = circuit_breaker.record_success().await {
                                        error!("Failed to record circuit breaker success: {}", e);
                                    }
                                }
                            }
                            
                            // Update metrics
                            if config.enable_metrics {
                                let elapsed = start_time.elapsed().as_millis() as f64;
                                let mut metrics_lock = metrics.write().await;
                                let service_metrics = metrics_lock
                                    .entry(service_name.to_string())
                                    .or_insert_with(RequestMetrics::default);
                                
                                service_metrics.successful_requests += 1;
                                
                                // Update average response time
                                let total_requests = service_metrics.successful_requests + service_metrics.failed_requests;
                                service_metrics.avg_response_time_ms = 
                                    ((service_metrics.avg_response_time_ms * (total_requests - 1) as f64) + elapsed) / total_requests as f64;
                                
                                // Update status code count
                                let status = response.status().as_u16();
                                *service_metrics.status_codes.entry(status).or_insert(0) += 1;
                            }
                            
                            Ok(response)
                        },
                        Err(e) => {
                            // Record failure in circuit breaker
                            if config.enable_circuit_breaking {
                                if let Some(circuit_breaker) = circuit_breakers.read().await.get(service_name) {
                                    if let Err(e) = circuit_breaker.record_failure().await {
                                        error!("Failed to record circuit breaker failure: {}", e);
                                    }
                                }
                            }
                            
                            // Update metrics
                            if config.enable_metrics {
                                let mut metrics_lock = metrics.write().await;
                                let service_metrics = metrics_lock
                                    .entry(service_name.to_string())
                                    .or_insert_with(RequestMetrics::default);
                                
                                service_metrics.failed_requests += 1;
                            }
                            
                            error!("Proxy error forwarding to {}: {}", selected_endpoint.ip_address, e);
                            
                            Ok(Response::builder()
                                .status(StatusCode::BAD_GATEWAY)
                                .body(Body::from("Error forwarding request"))
                                .unwrap())
                        }
                    },
                    Err(_) => {
                        // Request timed out
                        
                        // Record failure in circuit breaker
                        if config.enable_circuit_breaking {
                            if let Some(circuit_breaker) = circuit_breakers.read().await.get(service_name) {
                                if let Err(e) = circuit_breaker.record_failure().await {
                                    error!("Failed to record circuit breaker failure: {}", e);
                                }
                            }
                        }
                        
                        // Update metrics
                        if config.enable_metrics {
                            let mut metrics_lock = metrics.write().await;
                            let service_metrics = metrics_lock
                                .entry(service_name.to_string())
                                .or_insert_with(RequestMetrics::default);
                            
                            service_metrics.failed_requests += 1;
                        }
                        
                        error!("Request to {} timed out after {}ms", selected_endpoint.ip_address, config.request_timeout_ms);
                        
                        Ok(Response::builder()
                            .status(StatusCode::GATEWAY_TIMEOUT)
                            .body(Body::from("Request timed out"))
                            .unwrap())
                    }
                }
            },
            Err(e) => {
                error!("Proxy error creating request: {}", e);
                
                // Update metrics
                if config.enable_metrics {
                    let mut metrics_lock = metrics.write().await;
                    let service_metrics = metrics_lock
                        .entry(service_name.to_string())
                        .or_insert_with(RequestMetrics::default);
                    
                    service_metrics.failed_requests += 1;
                }
                
                Ok(Response::builder()
                    .status(StatusCode::INTERNAL_SERVER_ERROR)
                    .body(Body::from("Error creating request"))
                    .unwrap())
            }
        };
        
        result
    }
    
    /// Get metrics for a service
    pub async fn get_metrics(&self, service_name: &str) -> Option<RequestMetrics> {
        let metrics = self.metrics.read().await;
        metrics.get(service_name).cloned()
    }
    
    /// Get all metrics
    pub async fn get_all_metrics(&self) -> HashMap<String, RequestMetrics> {
        self.metrics.read().await.clone()
    }
    
    /// Get active connections count
    pub async fn get_active_connections(&self) -> u32 {
        *self.active_connections.lock().await
    }
}