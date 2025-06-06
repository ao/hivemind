use crate::app::ServiceConfig;
use crate::network::NetworkManager;
use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::str::FromStr;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::net::{TcpStream, UdpSocket};
use tokio::sync::Mutex;
use trust_dns_proto::op::{Header, MessageType, OpCode, ResponseCode};
use trust_dns_proto::rr::{Record, RecordType, RData};
use trust_dns_proto::serialize::binary::{BinDecodable, BinEncodable, BinEncoder};

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ServiceEndpoint {
    pub service_name: String,
    pub domain: String,
    pub ip_address: String,
    pub port: u16,
    pub node_id: String,
    pub health_status: ServiceHealth,
    pub last_health_check: i64,
    pub version: Option<String>,
    pub weight: Option<u32>,
    pub metadata: Option<HashMap<String, String>>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub enum ServiceHealth {
    Healthy,
    Unhealthy,
    Unknown,
}

impl std::fmt::Display for ServiceHealth {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.as_str())
    }
}

impl ServiceHealth {
    pub fn as_str(&self) -> &'static str {
        match self {
            ServiceHealth::Healthy => "healthy",
            ServiceHealth::Unhealthy => "unhealthy",
            ServiceHealth::Unknown => "unknown",
        }
    }

    pub fn from_str(s: &str) -> Self {
        match s {
            "healthy" => ServiceHealth::Healthy,
            "unhealthy" => ServiceHealth::Unhealthy,
            _ => ServiceHealth::Unknown,
        }
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct HealthCheckConfig {
    pub protocol: HealthCheckProtocol,
    pub path: Option<String>,       // For HTTP health checks
    pub interval_secs: u64,         // How often to check
    pub timeout_secs: u64,          // Timeout for each check
    pub healthy_threshold: u32,     // Number of successful checks to mark as healthy
    pub unhealthy_threshold: u32,   // Number of failed checks to mark as unhealthy
}

impl Default for HealthCheckConfig {
    fn default() -> Self {
        Self {
            protocol: HealthCheckProtocol::TCP,
            path: None,
            interval_secs: 30,
            timeout_secs: 5,
            healthy_threshold: 2,
            unhealthy_threshold: 3,
        }
    }
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub enum HealthCheckProtocol {
    HTTP,
    HTTPS,
    TCP,
    Command,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub enum LoadBalancingStrategy {
    RoundRobin,
    WeightedRoundRobin,
    LeastConnections,
    Random,
    IPHash,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct RoutingRule {
    pub path_prefix: Option<String>,
    pub headers: Option<HashMap<String, String>>,
    pub service_name: String,
    pub weight: Option<u32>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct TrafficSplitConfig {
    pub service_name: String,
    pub splits: Vec<TrafficSplit>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct TrafficSplit {
    pub version: String,
    pub weight: u32,
}

// Service mesh integration structs
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ServiceMeshConfig {
    pub enabled: bool,
    pub mtls_enabled: bool,
    pub tracing_enabled: bool,
    pub retry_policy: Option<RetryPolicy>,
    pub timeout_ms: Option<u64>,
    pub circuit_breaker: Option<CircuitBreakerPolicy>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct RetryPolicy {
    pub max_retries: u32,
    pub retry_on: Vec<String>, // e.g., "5xx", "connect-failure"
    pub timeout_ms: u64,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct CircuitBreakerPolicy {
    pub max_connections: u32,
    pub max_pending_requests: u32,
    pub max_requests: u32,
    pub max_retries: u32,
    pub consecutive_errors_threshold: u32,
    pub interval_ms: u64,
    pub base_ejection_time_ms: u64,
    pub state: CircuitBreakerState,
    pub last_state_change: i64,
    pub failure_count: u32,
    pub success_count: u32,
    pub half_open_allowed_calls: u32,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub enum CircuitBreakerState {
    Closed,    // Normal operation, requests are allowed
    Open,      // Circuit is open, requests are rejected
    HalfOpen,  // Testing if the service is healthy again
}

impl Default for CircuitBreakerPolicy {
    fn default() -> Self {
        Self {
            max_connections: 100,
            max_pending_requests: 10,
            max_requests: 1000,
            max_retries: 3,
            consecutive_errors_threshold: 5,
            interval_ms: 10000,
            base_ejection_time_ms: 30000,
            state: CircuitBreakerState::Closed,
            last_state_change: 0,
            failure_count: 0,
            success_count: 0,
            half_open_allowed_calls: 5,
        }
    }
}

#[derive(Clone, Debug)]
pub struct ServiceDiscovery {
    services: Arc<Mutex<HashMap<String, Vec<ServiceEndpoint>>>>,
    dns_port: u16,
    proxy_port: u16,
    health_check_config: Arc<Mutex<HashMap<String, HealthCheckConfig>>>,
    load_balancing_strategy: Arc<Mutex<HashMap<String, LoadBalancingStrategy>>>,
    endpoint_stats: Arc<Mutex<HashMap<String, EndpointStats>>>,
    network_manager: Option<Arc<NetworkManager>>,
    routing_rules: Arc<Mutex<Vec<RoutingRule>>>,
    traffic_splits: Arc<Mutex<HashMap<String, TrafficSplitConfig>>>,
}

#[derive(Clone, Debug)]
struct EndpointStats {
    active_connections: u32,
    total_requests: u64,
    failed_requests: u64,
    avg_response_time_ms: f64,
    last_selected: i64,
}

impl ServiceDiscovery {
    pub fn new() -> Self {
        Self {
            services: Arc::new(Mutex::new(HashMap::new())),
            dns_port: 53,     // Standard DNS port
            proxy_port: 8080, // Default proxy port
            health_check_config: Arc::new(Mutex::new(HashMap::new())),
            load_balancing_strategy: Arc::new(Mutex::new(HashMap::new())),
            endpoint_stats: Arc::new(Mutex::new(HashMap::new())),
            network_manager: None,
            routing_rules: Arc::new(Mutex::new(Vec::new())),
            traffic_splits: Arc::new(Mutex::new(HashMap::new())),
        }
    }
    
    pub fn with_network_manager(mut self, network_manager: Arc<NetworkManager>) -> Self {
        self.network_manager = Some(network_manager);
        self
    }
    
    // Initialize service discovery with network integration
    pub async fn initialize(&self) -> Result<()> {
        println!("Initializing service discovery system");
        
        // Start the health check system
        self.start_health_check_system().await?;
        
        // Start the DNS server
        self.start_dns_server().await?;
        
        // Start the proxy server
        self.start_proxy_server().await?;
        
        // If we have a network manager, set up the integration
        if let Some(network_manager) = &self.network_manager {
            self.setup_network_integration(network_manager).await?;
        }
        
        Ok(())
    }
    
    // Set up integration with the network manager
    async fn setup_network_integration(&self, _network_manager: &NetworkManager) -> Result<()> {
        println!("Setting up network integration for service discovery");
        
        // Register for network events
        self.register_network_events().await?;
        
        // Sync existing services with network
        self.sync_services_with_network().await?;
        
        Ok(())
    }
    
    // Register for network events
    async fn register_network_events(&self) -> Result<()> {
        // In a real implementation, this would set up event handlers for network changes
        // For now, we'll just log that we're registering
        println!("Registered for network events");
        Ok(())
    }
    
    // Sync existing services with network
    async fn sync_services_with_network(&self) -> Result<()> {
        println!("Syncing services with network");
        
        // Get all services
        let services = self.services.lock().await;
        
        // For each service, ensure network connectivity
        for (service_name, endpoints) in services.iter() {
            println!("Syncing service {} with network", service_name);
            
            for endpoint in endpoints {
                // Ensure network connectivity for this endpoint
                if let Some(_network_manager) = &self.network_manager {
                    // In a real implementation, this would ensure network routes exist
                    // For now, we'll just log that we're syncing
                    println!(
                        "Ensuring network connectivity for endpoint {} ({}:{})",
                        endpoint.node_id, endpoint.ip_address, endpoint.port
                    );
                }
            }
        }
        
        Ok(())
    }
    
    // Handle a new container being created
    pub async fn handle_container_created(
        &self,
        container_id: &str,
        service_name: &str,
        domain: &str,
        node_id: &str,
        network_config: &crate::network::ContainerNetworkConfig,
    ) -> Result<()> {
        println!(
            "Handling container creation for service {}: {} on node {}",
            service_name, container_id, node_id
        );
        
        // Create a service config
        let service_config = ServiceConfig {
            name: service_name.to_string(),
            domain: domain.to_string(),
            container_ids: vec![container_id.to_string()],
            desired_replicas: 1,
            current_replicas: 1,
        };
        
        // Register the service endpoint
        self.register_service(
            &service_config,
            node_id,
            &network_config.ip_address.to_string(),
            80, // Default port, in a real implementation this would come from the container config
        ).await?;
        
        // Set up default health check
        let health_check_config = HealthCheckConfig {
            protocol: HealthCheckProtocol::TCP,
            path: None,
            interval_secs: 30,
            timeout_secs: 5,
            healthy_threshold: 2,
            unhealthy_threshold: 3,
        };
        
        self.configure_health_check(service_name, health_check_config).await?;
        
        // Set up default load balancing strategy
        self.configure_load_balancing(service_name, LoadBalancingStrategy::RoundRobin).await?;
        
        Ok(())
    }
    
    // Handle a container being removed
    pub async fn handle_container_removed(
        &self,
        container_id: &str,
        service_name: &str,
        node_id: &str,
        ip_address: &str,
    ) -> Result<()> {
        println!(
            "Handling container removal for service {}: {} on node {}",
            service_name, container_id, node_id
        );
        
        // Deregister the service endpoint
        self.deregister_service(
            service_name,
            node_id,
            ip_address,
            80, // Default port, in a real implementation this would come from the container config
        ).await?;
        
        Ok(())
    }
    
    // Handle a node joining the cluster
    pub async fn handle_node_joined(&self, node_id: &str, node_ip: &str) -> Result<()> {
        println!("Handling node join: {} ({})", node_id, node_ip);
        
        // In a real implementation, this would update routing information
        // For now, we'll just log that we're handling the node join
        
        Ok(())
    }
    
    // Handle a node leaving the cluster
    pub async fn handle_node_left(&self, node_id: &str) -> Result<()> {
        println!("Handling node departure: {}", node_id);
        
        // Remove all services on this node
        let mut services = self.services.lock().await;
        
        // For each service, remove endpoints on this node
        for (service_name, endpoints) in services.iter_mut() {
            // Remove endpoints on this node
            endpoints.retain(|e| e.node_id != node_id);
            
            println!(
                "Removed endpoints on node {} for service {}",
                node_id, service_name
            );
        }
        
        // Remove empty services
        services.retain(|_, endpoints| !endpoints.is_empty());
        
        Ok(())
    }

    pub async fn register_service(
        &self,
        service_config: &ServiceConfig,
        node_id: &str,
        ip_address: &str,
        port: u16,
    ) -> Result<()> {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64;

        let endpoint = ServiceEndpoint {
            service_name: service_config.name.clone(),
            domain: service_config.domain.clone(),
            ip_address: ip_address.to_string(),
            port,
            node_id: node_id.to_string(),
            health_status: ServiceHealth::Unknown,
            last_health_check: now,
            version: None,
            weight: None,
            metadata: None,
        };

        let mut services = self.services.lock().await;

        if let Some(endpoints) = services.get_mut(&service_config.name) {
            // Check if this endpoint already exists
            let exists = endpoints
                .iter()
                .any(|e| e.node_id == node_id && e.ip_address == ip_address && e.port == port);

            if !exists {
                endpoints.push(endpoint);
                println!(
                    "Registered new endpoint for service {}: {}:{}",
                    service_config.name, ip_address, port
                );
            }
        } else {
            // First endpoint for this service
            services.insert(service_config.name.clone(), vec![endpoint]);
            println!(
                "Registered new service {} with endpoint {}:{}",
                service_config.name, ip_address, port
            );
        }

        Ok(())
    }

    pub async fn deregister_service(
        &self,
        service_name: &str,
        node_id: &str,
        ip_address: &str,
        port: u16,
    ) -> Result<()> {
        let mut services = self.services.lock().await;

        if let Some(endpoints) = services.get_mut(service_name) {
            // Remove matching endpoints
            endpoints.retain(|e| {
                !(e.node_id == node_id && e.ip_address == ip_address && e.port == port)
            });

            // If no endpoints left, remove the service
            if endpoints.is_empty() {
                services.remove(service_name);
                println!("Removed service {} as it has no endpoints", service_name);
            } else {
                println!(
                    "Deregistered endpoint {}:{} for service {}",
                    ip_address, port, service_name
                );
            }
        }

        Ok(())
    }

    pub async fn get_service_endpoints(&self, service_name: &str) -> Option<Vec<ServiceEndpoint>> {
        let services = self.services.lock().await;
        services.get(service_name).cloned()
    }

    pub async fn get_service_by_domain(
        &self,
        domain: &str,
    ) -> Option<(String, Vec<ServiceEndpoint>)> {
        let services = self.services.lock().await;

        for (service_name, endpoints) in services.iter() {
            // Check if any endpoint matches this domain
            if let Some(_endpoint) = endpoints.iter().find(|e| e.domain == domain) {
                return Some((service_name.clone(), endpoints.clone()));
            }
        }

        None
    }

    pub async fn list_services(&self) -> HashMap<String, Vec<ServiceEndpoint>> {
        let services = self.services.lock().await;
        services.clone()
    }

    pub async fn start_dns_server(&self) -> Result<()> {
        println!("Starting DNS server on port {}", self.dns_port);

        // Clone necessary data for the DNS server task
        let services = self.services.clone();
        let dns_port = self.dns_port;

        tokio::spawn(async move {
            if let Err(e) = Self::run_dns_server(dns_port, services).await {
                eprintln!("DNS server error: {}", e);
            }
        });

        Ok(())
    }

    async fn run_dns_server(
        dns_port: u16,
        services: Arc<Mutex<HashMap<String, Vec<ServiceEndpoint>>>>,
    ) -> Result<()> {
        // Bind to UDP socket for DNS
        let socket = UdpSocket::bind(SocketAddr::new(
            IpAddr::V4(Ipv4Addr::new(0, 0, 0, 0)),
            dns_port,
        ))
        .await?;

        println!("DNS server listening on port {}", dns_port);

        // Create a buffer for receiving DNS queries
        let mut query_buffer = [0u8; 512]; // Standard DNS message size

        loop {
            match socket.recv_from(&mut query_buffer).await {
                Ok((len, src)) => {
                    println!("Received DNS query from {}", src);
                    
                    // Try to parse the DNS message
                    match trust_dns_proto::op::Message::from_bytes(&query_buffer[..len]) {
                        Ok(query) => {
                            // Process the query and create a response
                            let response = Self::process_dns_query(query, &services).await;
                            
                            // Encode the response
                            let mut response_buffer = Vec::with_capacity(512);
                            let mut encoder = BinEncoder::new(&mut response_buffer);
                            if let Err(e) = response.emit(&mut encoder) {
                                eprintln!("Failed to encode DNS response: {}", e);
                                continue;
                            }
                            
                            // Send the response
                            if let Err(e) = socket.send_to(&response_buffer, src).await {
                                eprintln!("Failed to send DNS response: {}", e);
                            }
                        }
                        Err(e) => {
                            eprintln!("Failed to parse DNS query: {}", e);
                            // Send an error response
                            let error_response = Self::create_error_response(ResponseCode::FormErr);
                            let mut response_buffer = Vec::with_capacity(512);
                            let mut encoder = BinEncoder::new(&mut response_buffer);
                            if let Err(e) = error_response.emit(&mut encoder) {
                                eprintln!("Failed to encode error response: {}", e);
                                continue;
                            }
                            
                            if let Err(e) = socket.send_to(&response_buffer, src).await {
                                eprintln!("Failed to send error response: {}", e);
                            }
                        }
                    }
                }
                Err(e) => eprintln!("Failed to receive DNS query: {}", e),
            }
        }
    }
    
    // Process a DNS query and create a response
    async fn process_dns_query(
        query: trust_dns_proto::op::Message,
        services: &Arc<Mutex<HashMap<String, Vec<ServiceEndpoint>>>>,
    ) -> trust_dns_proto::op::Message {
        // Create a new response message directly
        let mut response = trust_dns_proto::op::Message::new();
        response.set_header(Header::response_from_request(query.header()));
        
        // Check if we have any queries
        if query.query_count() == 0 {
            return Self::create_error_response(ResponseCode::FormErr);
        }
        
        // Get the first query
        let query_question = &query.queries()[0];
        let query_name = query_question.name().clone();
        let query_type = query_question.query_type();
        
        // Convert the query name to a string
        let domain = query_name.to_string();
        // Remove the trailing dot if present
        let domain = domain.trim_end_matches('.');
        
        println!("DNS query for domain: {} (type: {:?})", domain, query_type);
        
        // Look up the domain in our services
        let services_lock = services.lock().await;
        let mut found_service = false;
        let mut records = Vec::new();
        
        // Find service by domain
        let mut service_name = String::new();
        let mut service_endpoints = Vec::new();
        
        for (name, endpoints) in services_lock.iter() {
            for endpoint in endpoints {
                if endpoint.domain == domain {
                    found_service = true;
                    service_name = name.clone();
                    service_endpoints = endpoints.clone();
                    break;
                }
            }
            if found_service {
                break;
            }
        }
        
        if !found_service {
            // Domain not found
            return Self::create_error_response(ResponseCode::NXDomain);
        }
        
        // Filter to only healthy endpoints if we have any
        let healthy_endpoints: Vec<&ServiceEndpoint> = service_endpoints
            .iter()
            .filter(|e| e.health_status == ServiceHealth::Healthy)
            .collect();
        
        // If no healthy endpoints, use all endpoints
        let endpoints_to_use = if healthy_endpoints.is_empty() {
            service_endpoints.iter().collect::<Vec<&ServiceEndpoint>>()
        } else {
            healthy_endpoints
        };
        
        // Handle different record types
        match query_type {
            RecordType::A => {
                // Create A records for all endpoints
                for endpoint in endpoints_to_use {
                    if let Ok(ip) = Ipv4Addr::from_str(&endpoint.ip_address) {
                        let rdata = RData::A(ip);
                        let record = Record::from_rdata(query_name.clone(), 300, rdata);
                        records.push(record);
                    }
                }
            },
            RecordType::SRV => {
                // Create SRV records for all endpoints
                for (i, endpoint) in endpoints_to_use.iter().enumerate() {
                    if let Ok(ip) = Ipv4Addr::from_str(&endpoint.ip_address) {
                        // Create a target name for this endpoint
                        let target_name = trust_dns_proto::rr::Name::from_str(
                            &format!("{}-{}.{}", service_name, i, domain)
                        ).unwrap_or_else(|_| query_name.clone());
                        
                        // Create the SRV record
                        let rdata = RData::SRV {
                            priority: 0,
                            weight: 10,
                            port: endpoint.port,
                            target: target_name.clone(),
                        };
                        let srv_record = Record::from_rdata(query_name.clone(), 300, rdata);
                        records.push(srv_record);
                        
                        // Also add an A record for the target
                        let a_rdata = RData::A(ip);
                        let a_record = Record::from_rdata(target_name, 300, a_rdata);
                        records.push(a_record);
                    }
                }
            },
            RecordType::TXT => {
                // Create TXT record with service metadata
                let mut txt_data = Vec::<String>::new();
                
                // Add service name
                txt_data.push(format!("service={}", service_name));
                
                // Add endpoint count
                txt_data.push(format!("endpoints={}", endpoints_to_use.len()));
                
                // Add health information
                let healthy_count = healthy_endpoints.len();
                txt_data.push(format!("healthy={}", healthy_count));
                
                let rdata = RData::TXT(txt_data);
                let record = Record::from_rdata(query_name.clone(), 300, rdata);
                records.push(record);
            },
            _ => {
                // Unsupported record type
                return Self::create_error_response(ResponseCode::NotImp);
            }
        }
        
        if records.is_empty() {
            // No valid records could be created
            return Self::create_error_response(ResponseCode::ServFail);
        }
        
        // Build the response with the records
        response.add_answers(records);
        response
    }
    
    // Create an error response
    fn create_error_response(code: ResponseCode) -> trust_dns_proto::op::Message {
        let mut header = Header::new();
        header.set_message_type(MessageType::Response);
        header.set_op_code(OpCode::Query);
        header.set_response_code(code);
        
        let mut message = trust_dns_proto::op::Message::new();
        message.set_header(header);
        message
    }

    pub async fn get_service_url(&self, service_name: &str) -> Option<String> {
        let services = self.services.lock().await;
        if let Some(endpoints) = services.get(service_name) {
            // Find a healthy endpoint
            if let Some(endpoint) = endpoints
                .iter()
                .find(|e| e.health_status == ServiceHealth::Healthy)
            {
                return Some(format!("http://{}:{}", endpoint.ip_address, endpoint.port));
            }
        }
        None
    }

    pub async fn start_proxy_server(&self) -> Result<()> {
        println!("Starting proxy server on port {}", self.proxy_port);

        // Clone necessary data for the proxy server task
        let services = self.services.clone();
        let proxy_port = self.proxy_port;
        let routing_rules = self.routing_rules.clone();
        let traffic_splits = self.traffic_splits.clone();
        let load_balancing_strategy = self.load_balancing_strategy.clone();
        let endpoint_stats = self.endpoint_stats.clone();

        tokio::spawn(async move {
            if let Err(e) = Self::run_proxy_server(
                proxy_port,
                services,
                routing_rules,
                traffic_splits,
                load_balancing_strategy,
                endpoint_stats
            ).await {
                eprintln!("Proxy server error: {}", e);
            }
        });

        Ok(())
    }

    async fn run_proxy_server(
        proxy_port: u16,
        services: Arc<Mutex<HashMap<String, Vec<ServiceEndpoint>>>>,
        routing_rules: Arc<Mutex<Vec<RoutingRule>>>,
        traffic_splits: Arc<Mutex<HashMap<String, TrafficSplitConfig>>>,
        load_balancing_strategies: Arc<Mutex<HashMap<String, LoadBalancingStrategy>>>,
        endpoint_stats: Arc<Mutex<HashMap<String, EndpointStats>>>,
    ) -> Result<()> {
        use hyper::service::{make_service_fn, service_fn};
        use hyper::{Body, Client, Request, Response, Server, StatusCode};
        use std::convert::Infallible;
        use std::net::SocketAddr;
        use std::sync::Arc as StdArc;
        
        // Create a client for forwarding requests
        let client = Client::new();
        let services_arc = StdArc::new(services);
        
        // Create a service function that will handle incoming requests
        let make_svc = make_service_fn(move |_conn| {
            let services = services_arc.clone();
            let client = client.clone();
            
            async move {
                Ok::<_, Infallible>(service_fn(move |req: Request<Body>| {
                    let services = services.clone();
                    let client = client.clone();
                    
                    async move {
                        // Extract the host from the request
                        // Extract tenant ID from headers if available
                        let tenant_id = req.headers()
                            .get("X-Tenant-ID")
                            .and_then(|h| h.to_str().ok())
                            .unwrap_or("default");
                        
                        // Log tenant ID for debugging
                        println!("Request from tenant: {}", tenant_id);
                        
                        let host = match req.headers().get(hyper::header::HOST) {
                            Some(host) => match host.to_str() {
                                Ok(host) => host.split(':').next().unwrap_or("").to_string(),
                                Err(_) => return Ok(Response::builder()
                                    .status(StatusCode::BAD_REQUEST)
                                    .body(Body::from("Invalid Host header"))
                                    .unwrap()),
                            },
                            None => return Ok(Response::builder()
                                .status(StatusCode::BAD_REQUEST)
                                .body(Body::from("Missing Host header"))
                                .unwrap()),
                        };
                        
                        // Get the path for path-based routing
                        let path = req.uri().path();
                        
                        // Look up the service by domain
                        let services_lock = services.lock().await;
                        let routing_rules_lock = routing_rules.lock().await;
                        let traffic_splits_lock = traffic_splits.lock().await;
                        
                        // Get the routing rules and traffic splits
                        let routing_rules_vec = routing_rules_lock.clone();
                        let traffic_splits_map = traffic_splits_lock.clone();
                        
                        let mut found_service = false;
                        let mut service_name = String::new();
                        let mut service_endpoints = Vec::new();
                        let mut service_tenant_id = String::new();
                        
                        // First, try to find a service by domain
                        for (name, endpoints) in services_lock.iter() {
                            for endpoint in endpoints {
                                if endpoint.domain == host {
                                    found_service = true;
                                    service_name = name.clone();
                                    service_endpoints = endpoints.clone();
                                    
                                    // Extract tenant ID from metadata if available
                                    if let Some(metadata) = &endpoint.metadata {
                                        if let Some(id) = metadata.get("tenant_id") {
                                            service_tenant_id = id.clone();
                                        }
                                    }
                                    break;
                                }
                            }
                            if found_service {
                                break;
                            }
                        }
                        
                        // If no service found by domain, try path-based routing
                        if !found_service {
                            for rule in routing_rules_lock.iter() {
                                if let Some(path_prefix) = &rule.path_prefix {
                                    if path.starts_with(path_prefix) {
                                        // Found a matching path-based rule
                                        if let Some(endpoints) = services_lock.get(&rule.service_name) {
                                            found_service = true;
                                            service_name = rule.service_name.clone();
                                            service_endpoints = endpoints.clone();
                                            
                                            // Extract tenant ID from the first endpoint's metadata
                                            if !endpoints.is_empty() {
                                                if let Some(metadata) = &endpoints[0].metadata {
                                                    if let Some(id) = metadata.get("tenant_id") {
                                                        service_tenant_id = id.clone();
                                                    }
                                                }
                                            }
                                            break;
                                        }
                                    }
                                }
                            }
                        }
                        
                        // If still no service found, try header-based routing
                        if !found_service {
                            for rule in routing_rules_lock.iter() {
                                if let Some(headers) = &rule.headers {
                                    let mut headers_match = true;
                                    
                                    for (header_name, header_value) in headers {
                                        if let Some(req_header) = req.headers().get(header_name) {
                                            if let Ok(req_value) = req_header.to_str() {
                                                if req_value != header_value {
                                                    headers_match = false;
                                                    break;
                                                }
                                            } else {
                                                headers_match = false;
                                                break;
                                            }
                                        } else {
                                            headers_match = false;
                                            break;
                                        }
                                    }
                                    
                                    if headers_match {
                                        // Found a matching header-based rule
                                        if let Some(endpoints) = services_lock.get(&rule.service_name) {
                                            found_service = true;
                                            service_name = rule.service_name.clone();
                                            service_endpoints = endpoints.clone();
                                            break;
                                        }
                                    }
                                }
                            }
                        }
                        
                        if !found_service || service_endpoints.is_empty() {
                            return Ok(Response::builder()
                                .status(StatusCode::NOT_FOUND)
                                .body(Body::from("Service not found"))
                                .unwrap());
                        }
                        
                        // Tenant boundary check - ensure the request tenant can access the service tenant
                        if !service_tenant_id.is_empty() && tenant_id != "default" && tenant_id != service_tenant_id {
                            println!("Tenant boundary violation: {} attempting to access service in tenant {}",
                                     tenant_id, service_tenant_id);
                            return Ok(Response::builder()
                                .status(StatusCode::FORBIDDEN)
                                .body(Body::from("Access denied: tenant boundary violation"))
                                .unwrap());
                        }
                        
                        // Filter to only healthy endpoints
                        let mut healthy_endpoints: Vec<&ServiceEndpoint> = service_endpoints
                            .iter()
                            .filter(|e| e.health_status == ServiceHealth::Healthy)
                            .collect();
                        
                        // If no healthy endpoints, return error
                        if healthy_endpoints.is_empty() {
                            // Log the health status of all endpoints for debugging
                            println!("No healthy endpoints for service {}. Available endpoints:", service_name);
                            for (i, endpoint) in service_endpoints.iter().enumerate() {
                                println!("  Endpoint {}: {} ({})", i, endpoint.ip_address, endpoint.health_status);
                            }
                            
                            return Ok(Response::builder()
                                .status(StatusCode::SERVICE_UNAVAILABLE)
                                .body(Body::from("No healthy endpoints available"))
                                .unwrap());
                        }
                        
                        // Check for traffic splitting configuration
                        let mut selected_version: Option<String> = None;
                        
                        // Apply traffic splitting if configured
                        if let Some(traffic_split_config) = traffic_splits_map.get(&service_name) {
                            // Get a random number for weighted selection
                            let now = SystemTime::now()
                                .duration_since(UNIX_EPOCH)
                                .unwrap()
                                .subsec_nanos();
                            
                            // Calculate total weight
                            let total_weight: u32 = traffic_split_config.splits.iter()
                                .map(|s| s.weight)
                                .sum();
                            
                            if total_weight > 0 {
                                let mut random_val = (now as u32) % total_weight;
                                
                                // Select version based on weight
                                for split in &traffic_split_config.splits {
                                    if random_val < split.weight {
                                        selected_version = Some(split.version.clone());
                                        break;
                                    }
                                    random_val -= split.weight;
                                }
                                
                                // Filter endpoints by selected version
                                if let Some(version) = &selected_version {
                                    let version_filtered: Vec<&ServiceEndpoint> = healthy_endpoints
                                        .iter()
                                        .filter(|e| e.version.as_ref().map_or(false, |v| v == version))
                                        .cloned()
                                        .collect();
                                    
                                    // Only use version filtered endpoints if we found some
                                    if !version_filtered.is_empty() {
                                        healthy_endpoints = version_filtered;
                                    }
                                }
                            }
                        }
                        
                        // Use load balancing to select an endpoint
                        let now = SystemTime::now()
                            .duration_since(UNIX_EPOCH)
                            .unwrap();
                        
                        // Get client IP for IP hash if available
                        let client_ip = req.headers()
                            .get("X-Forwarded-For")
                            .and_then(|h| h.to_str().ok())
                            .or_else(|| {
                                req.extensions()
                                    .get::<std::net::SocketAddr>()
                                    .map(|addr| addr.ip().to_string().as_str())
                                    .ok()
                            });
                        
                        // Get session ID for session affinity if available
                        let session_id = req.headers()
                            .get("Cookie")
                            .and_then(|c| c.to_str().ok())
                            .and_then(|cookie| {
                                cookie.split(';')
                                    .find(|part| part.trim().starts_with("session="))
                                    .map(|s| s.trim().trim_start_matches("session="))
                            });
                        
                        // Get the load balancing strategy for this service
                        let load_balancing_strategy = load_balancing_strategies.lock().await
                            .get(&service_name)
                            .cloned()
                            .unwrap_or(LoadBalancingStrategy::RoundRobin);
                        
                        // Select an endpoint based on the strategy
                        let selected = match load_balancing_strategy {
                            LoadBalancingStrategy::RoundRobin => {
                                // Use round robin selection based on timestamp
                                let index = now.subsec_nanos() as usize % healthy_endpoints.len();
                                healthy_endpoints[index]
                            },
                            LoadBalancingStrategy::LeastConnections => {
                                // Find endpoint with least active connections
                                let mut min_connections = u32::MAX;
                                let mut selected_index = 0;
                                
                                for (i, endpoint) in healthy_endpoints.iter().enumerate() {
                                    let stats_key = format!("{}_{}", service_name, endpoint.ip_address);
                                    let connections = endpoint_stats
                                        .entry(stats_key.clone())
                                        .or_insert(EndpointStats {
                                            active_connections: 0,
                                            total_requests: 0,
                                            failed_requests: 0,
                                            avg_response_time_ms: 0.0,
                                            last_selected: 0,
                                        })
                                        .active_connections;
                                    
                                    if connections < min_connections {
                                        min_connections = connections;
                                        selected_index = i;
                                    }
                                }
                                
                                healthy_endpoints[selected_index]
                            },
                            LoadBalancingStrategy::Random => {
                                // Simple random selection
                                let index = (now.subsec_nanos() as usize) % healthy_endpoints.len();
                                healthy_endpoints[index]
                            },
                            LoadBalancingStrategy::WeightedRoundRobin => {
                                // Use weights if available
                                let mut weights = Vec::with_capacity(healthy_endpoints.len());
                                let mut total_weight = 0;
                                
                                for endpoint in &healthy_endpoints {
                                    let weight = endpoint.weight.unwrap_or(10);
                                    weights.push(weight);
                                    total_weight += weight;
                                }
                                
                                if total_weight == 0 {
                                    // Fallback to simple round robin
                                    let index = now.subsec_nanos() as usize % healthy_endpoints.len();
                                    healthy_endpoints[index]
                                } else {
                                    // Select based on weight
                                    let mut random_val = (now.subsec_nanos() as u32) % total_weight;
                                    
                                    for (i, weight) in weights.iter().enumerate() {
                                        if random_val < *weight {
                                            return healthy_endpoints[i];
                                        }
                                        random_val -= *weight;
                                    }
                                    
                                    // Fallback
                                    healthy_endpoints[0]
                                }
                            },
                            LoadBalancingStrategy::IPHash => {
                                if let Some(ip) = client_ip {
                                    // Use client IP for consistent routing
                                    let mut hash = 0u32;
                                    for byte in ip.bytes() {
                                        hash = hash.wrapping_mul(31).wrapping_add(byte as u32);
                                    }
                                    
                                    let index = (hash as usize) % healthy_endpoints.len();
                                    healthy_endpoints[index]
                                } else if let Some(id) = session_id {
                                    // Use session ID if available
                                    let mut hash = 0u32;
                                    for byte in id.bytes() {
                                        hash = hash.wrapping_mul(31).wrapping_add(byte as u32);
                                    }
                                    
                                    let index = (hash as usize) % healthy_endpoints.len();
                                    healthy_endpoints[index]
                                } else {
                                    // Fallback to round robin
                                    let index = now.subsec_nanos() as usize % healthy_endpoints.len();
                                    healthy_endpoints[index]
                                }
                            }
                        };
                        
                        // Update metrics for the selected endpoint
                        let stats_key = format!("{}_{}", service_name, selected.ip_address);
                        let mut endpoint_stats_lock = endpoint_stats.lock().await;
                        let stats = endpoint_stats_lock
                            .entry(stats_key.clone())
                            .or_insert(EndpointStats {
                                active_connections: 0,
                                total_requests: 0,
                                failed_requests: 0,
                                avg_response_time_ms: 0.0,
                                last_selected: 0,
                            });
                        
                        // Increment active connections and total requests
                        stats.active_connections += 1;
                        stats.total_requests += 1;
                        stats.last_selected = std::time::SystemTime::now()
                            .duration_since(std::time::UNIX_EPOCH)
                            .unwrap_or_default()
                            .as_secs() as i64;
                        
                        // Create the forwarding URI
                        let uri = format!("http://{}:{}{}",
                            selected.ip_address,
                            selected.port,
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
                        
                        // Add routing metadata headers
                        if let Some(version) = &selected_version {
                            new_req = new_req.header("X-Service-Version", version);
                        }
                        
                        // Add tracing headers if available
                        if let Some(trace_id) = req.headers().get("X-Trace-ID") {
                            new_req = new_req.header("X-Trace-ID", trace_id);
                        }
                        
                        // Add service mesh headers if needed
                        new_req = new_req.header("X-Forwarded-For", host);
                        new_req = new_req.header("X-Forwarded-Proto", "http");
                        
                        // Forward the request
                        // Record start time for response time metrics
                        let start_time = std::time::Instant::now();
                        
                        let result = match new_req.body(req.into_body()) {
                            Ok(new_req) => match client.request(new_req).await {
                                Ok(res) => {
                                    // Record successful request
                                    let elapsed = start_time.elapsed().as_millis() as f64;
                                    
                                    // Update endpoint stats with response time
                                    if let Some(stats) = endpoint_stats_lock.get_mut(&stats_key) {
                                        // Update average response time
                                        let total_requests = stats.total_requests as f64;
                                        stats.avg_response_time_ms =
                                            ((stats.avg_response_time_ms * (total_requests - 1.0)) + elapsed) / total_requests;
                                        
                                        // Decrement active connections
                                        if stats.active_connections > 0 {
                                            stats.active_connections -= 1;
                                        }
                                    }
                                    
                                    Ok(res)
                                },
                                Err(e) => {
                                    // Record failed request
                                    if let Some(stats) = endpoint_stats_lock.get_mut(&stats_key) {
                                        stats.failed_requests += 1;
                                        
                                        // Decrement active connections
                                        if stats.active_connections > 0 {
                                            stats.active_connections -= 1;
                                        }
                                    }
                                    
                                    eprintln!("Proxy error forwarding to {}: {}", selected.ip_address, e);
                                    Ok(Response::builder()
                                        .status(StatusCode::BAD_GATEWAY)
                                        .body(Body::from("Error forwarding request"))
                                        .unwrap())
                                },
                            },
                            Err(e) => {
                                eprintln!("Proxy error creating request: {}", e);
                                Ok(Response::builder()
                                    .status(StatusCode::INTERNAL_SERVER_ERROR)
                                    .body(Body::from("Error creating request"))
                                    .unwrap())
                            },
                        };
                        
                        result
                    }
                }))
            }
        });
        
        // Create the server
        let addr = SocketAddr::from(([0, 0, 0, 0], proxy_port));
        let server = Server::bind(&addr).serve(make_svc);
        
        println!("Proxy server listening on http://{}", addr);
        
        // Run the server
        if let Err(e) = server.await {
            eprintln!("Proxy server error: {}", e);
        }
        
        Ok(())
    }

    // Configure health check for a service
    pub async fn configure_health_check(&self, service_name: &str, config: HealthCheckConfig) -> Result<()> {
        let mut health_check_config = self.health_check_config.lock().await;
        health_check_config.insert(service_name.to_string(), config);
        println!("Configured health check for service {}", service_name);
        Ok(())
    }
    
    // Configure load balancing strategy for a service
    pub async fn configure_load_balancing(&self, service_name: &str, strategy: LoadBalancingStrategy) -> Result<()> {
        let mut load_balancing_strategy = self.load_balancing_strategy.lock().await;
        load_balancing_strategy.insert(service_name.to_string(), strategy);
        println!("Configured load balancing strategy for service {}", service_name);
        Ok(())
    }
    
    // Start the health check system
    pub async fn start_health_check_system(&self) -> Result<()> {
        println!("Starting health check system");
        
        // Clone necessary data for the health check task
        let services = self.services.clone();
        let health_check_config = self.health_check_config.clone();
        
        tokio::spawn(async move {
            loop {
                if let Err(e) = Self::run_health_checks(&services, &health_check_config).await {
                    eprintln!("Health check error: {}", e);
                }
                
                // Sleep for a while before the next round of health checks
                tokio::time::sleep(Duration::from_secs(10)).await;
            }
        });
        
        Ok(())
    }
    
    // Run health checks for all services
    async fn run_health_checks(
        services: &Arc<Mutex<HashMap<String, Vec<ServiceEndpoint>>>>,
        health_check_config: &Arc<Mutex<HashMap<String, HealthCheckConfig>>>,
    ) -> Result<()> {
        // Track health status changes for proxy routing updates
        let mut health_status_changed = false;
        let mut services_lock = services.lock().await;
        let health_check_config_lock = health_check_config.lock().await;
        let _now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64;
        
        for (service_name, endpoints) in services_lock.iter_mut() {
            // Get health check config for this service, or use default
            let config = health_check_config_lock
                .get(service_name)
                .cloned()
                .unwrap_or_default();
            
            for endpoint in endpoints.iter_mut() {
                // Check if it's time to run a health check for this endpoint
                if _now - endpoint.last_health_check >= config.interval_secs as i64 {
                    // Perform health check based on protocol
                    let health_status = match config.protocol {
                        HealthCheckProtocol::HTTP => {
                            Self::check_http_health(endpoint, &config).await
                        }
                        HealthCheckProtocol::HTTPS => {
                            Self::check_https_health(endpoint, &config).await
                        }
                        HealthCheckProtocol::TCP => {
                            Self::check_tcp_health(endpoint, &config).await
                        }
                        HealthCheckProtocol::Command => {
                            // Command-based health checks not implemented yet
                            ServiceHealth::Unknown
                        }
                    };
                    
                    // Check if health status changed
                    let previous_status = endpoint.health_status.clone();
                    
                    // Update endpoint health status
                    endpoint.health_status = health_status.clone();
                    endpoint.last_health_check = _now;
                    
                    // If health status changed, mark for proxy routing update
                    if previous_status != health_status {
                        health_status_changed = true;
                    }
                    
                    println!(
                        "Health check for {}/{}: {}",
                        service_name, endpoint.ip_address, health_status
                    );
                }
            }
        }
        
        // If health status changed, update proxy routing
        if health_status_changed {
            // In a real implementation, we would call a method to update the proxy routing
            println!("Health status changed, updating proxy routing");
        }
        
        Ok(())
    }
    
    // Check HTTP health
    async fn check_http_health(endpoint: &ServiceEndpoint, config: &HealthCheckConfig) -> ServiceHealth {
        let path = config.path.clone().unwrap_or_else(|| "/health".to_string());
        let url = format!("http://{}:{}{}", endpoint.ip_address, endpoint.port, path);
        let timeout = Duration::from_secs(config.timeout_secs);
        
        // Create a simple HTTP client with timeout
        match tokio::time::timeout(timeout, Self::make_http_request(&url)).await {
            Ok(Ok(_)) => ServiceHealth::Healthy,
            _ => ServiceHealth::Unhealthy,
        }
    }
    
    // Check HTTPS health
    async fn check_https_health(endpoint: &ServiceEndpoint, config: &HealthCheckConfig) -> ServiceHealth {
        let path = config.path.clone().unwrap_or_else(|| "/health".to_string());
        let url = format!("https://{}:{}{}", endpoint.ip_address, endpoint.port, path);
        let timeout = Duration::from_secs(config.timeout_secs);
        
        // Create a simple HTTPS client with timeout
        match tokio::time::timeout(timeout, Self::make_http_request(&url)).await {
            Ok(Ok(_)) => ServiceHealth::Healthy,
            _ => ServiceHealth::Unhealthy,
        }
    }
    
    // Make HTTP/HTTPS request
    async fn make_http_request(url: &str) -> Result<()> {
        // Use reqwest to make an actual HTTP request
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(5))
            .build()?;
        
        let response = client.get(url).send().await?;
        
        // Check if the response is successful (status code 2xx)
        if response.status().is_success() {
            Ok(())
        } else {
            Err(anyhow::anyhow!("Health check failed with status: {}", response.status()))
        }
    }
    
    // Check TCP health
    async fn check_tcp_health(endpoint: &ServiceEndpoint, config: &HealthCheckConfig) -> ServiceHealth {
        let addr = format!("{}:{}", endpoint.ip_address, endpoint.port);
        let timeout = Duration::from_secs(config.timeout_secs);
        
        // Try to connect to the TCP port
        match tokio::time::timeout(timeout, TcpStream::connect(&addr)).await {
            Ok(Ok(_)) => ServiceHealth::Healthy,
            _ => ServiceHealth::Unhealthy,
        }
    }
    
    // Get a service endpoint using the configured load balancing strategy
    pub async fn get_service_endpoint(&self, service_name: &str) -> Option<ServiceEndpoint> {
        self.get_service_endpoint_with_options(service_name, None, None).await
    }
    
    pub async fn get_service_endpoint_with_options(
        &self,
        service_name: &str,
        client_ip: Option<&str>,
        session_id: Option<&str>
    ) -> Option<ServiceEndpoint> {
        let services = self.services.lock().await;
        let load_balancing_strategies = self.load_balancing_strategy.lock().await;
        let mut endpoint_stats = self.endpoint_stats.lock().await;
        
        if let Some(endpoints) = services.get(service_name) {
            // Filter to only healthy endpoints
            let healthy_endpoints: Vec<&ServiceEndpoint> = endpoints
                .iter()
                .filter(|e| e.health_status == ServiceHealth::Healthy)
                .collect();
            
            if healthy_endpoints.is_empty() {
                // Circuit breaker pattern - if no healthy endpoints, return None
                return None;
            }
            
            // Get the load balancing strategy for this service, or use default
            let strategy = load_balancing_strategies
                .get(service_name)
                .cloned()
                .unwrap_or(LoadBalancingStrategy::RoundRobin);
            
            // Select an endpoint based on the strategy
            let selected_endpoint = match strategy {
                LoadBalancingStrategy::RoundRobin => {
                    Self::select_round_robin(&healthy_endpoints, service_name, &mut endpoint_stats)
                }
                LoadBalancingStrategy::LeastConnections => {
                    Self::select_least_connections(&healthy_endpoints, service_name, &mut endpoint_stats)
                }
                LoadBalancingStrategy::Random => {
                    Self::select_random(&healthy_endpoints)
                }
                LoadBalancingStrategy::WeightedRoundRobin => {
                    Self::select_weighted_round_robin(&healthy_endpoints, service_name, &mut endpoint_stats)
                }
                LoadBalancingStrategy::IPHash => {
                    if let Some(ip) = client_ip {
                        Self::select_ip_hash(&healthy_endpoints, ip)
                    } else if let Some(session) = session_id {
                        // Fall back to session-based hashing if IP not available
                        Self::select_session_affinity(&healthy_endpoints, session)
                    } else {
                        // Fall back to round robin if no IP or session ID
                        Self::select_round_robin(&healthy_endpoints, service_name, &mut endpoint_stats)
                    }
                }
            };
            
            // Update stats for the selected endpoint
            let stats_key = format!("{}_{}", service_name, selected_endpoint.ip_address);
            if let Some(stats) = endpoint_stats.get_mut(&stats_key) {
                stats.active_connections += 1;
                stats.total_requests += 1;
                stats.last_selected = SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .unwrap()
                    .as_secs() as i64;
            }
            
            return Some(selected_endpoint.clone());
        }
        
        None
    }
    
    // Round-robin selection
    fn select_round_robin<'a>(
        endpoints: &[&'a ServiceEndpoint],
        service_name: &str,
        endpoint_stats: &mut HashMap<String, EndpointStats>,
    ) -> &'a ServiceEndpoint {
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64;
        
        // Get or create stats for this service
        let stats_key = format!("{}_last_index", service_name);
        let last_index = endpoint_stats
            .entry(stats_key.clone())
            .or_insert(EndpointStats {
                active_connections: 0,
                total_requests: 0,
                failed_requests: 0,
                avg_response_time_ms: 0.0,
                last_selected: 0,
            })
            .last_selected as usize;
        
        // Calculate next index
        let next_index = (last_index + 1) % endpoints.len();
        
        // Update last selected index
        if let Some(stats) = endpoint_stats.get_mut(&stats_key) {
            stats.last_selected = next_index as i64;
            stats.total_requests += 1;
            stats.last_selected = now;
        }
        
        endpoints[next_index]
    }
    
    // Least connections selection
    fn select_least_connections<'a>(
        endpoints: &[&'a ServiceEndpoint],
        service_name: &str,
        endpoint_stats: &mut HashMap<String, EndpointStats>,
    ) -> &'a ServiceEndpoint {
        // Find endpoint with least active connections
        let mut min_connections = u32::MAX;
        let mut selected_index = 0;
        
        for (i, endpoint) in endpoints.iter().enumerate() {
            let stats_key = format!("{}_{}", service_name, endpoint.ip_address);
            let connections = endpoint_stats
                .entry(stats_key.clone())
                .or_insert(EndpointStats {
                    active_connections: 0,
                    total_requests: 0,
                    failed_requests: 0,
                    avg_response_time_ms: 0.0,
                    last_selected: 0,
                })
                .active_connections;
            
            if connections < min_connections {
                min_connections = connections;
                selected_index = i;
            }
        }
        
        // Update stats for selected endpoint
        let selected_endpoint = endpoints[selected_index];
        let stats_key = format!("{}_{}", service_name, selected_endpoint.ip_address);
        if let Some(stats) = endpoint_stats.get_mut(&stats_key) {
            stats.active_connections += 1;
            stats.total_requests += 1;
        }
        
        selected_endpoint
    }
    
    // Random selection
    fn select_random<'a>(endpoints: &[&'a ServiceEndpoint]) -> &'a ServiceEndpoint {
        use std::time::SystemTime;
        
        // Simple pseudo-random selection based on current time
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .subsec_nanos();
        
        let index = (now as usize) % endpoints.len();
        endpoints[index]
    }
    
    // Configure service mesh for a service
    pub async fn configure_service_mesh(&self, service_name: &str, config: ServiceMeshConfig) -> Result<()> {
        // In a real implementation, this would store the configuration and apply it
        // For now, we'll just log that we're configuring the service mesh
        println!("Configuring service mesh for service {}: {:?}", service_name, config);
        
        // If we have a network manager, update the network configuration
        if let Some(network_manager) = &self.network_manager {
            if config.enabled {
                println!("Enabling service mesh for service {} with network integration", service_name);
                // In a real implementation, this would configure the network for service mesh
            }
        }
        
        Ok(())
    }
    
    // Get service mesh metrics
    pub async fn get_service_mesh_metrics(&self, service_name: &str) -> Result<HashMap<String, f64>> {
        // In a real implementation, this would collect metrics from the service mesh
        // For now, we'll just return some mock metrics
        let mut metrics = HashMap::new();
        metrics.insert("request_count".to_string(), 100.0);
        metrics.insert("error_rate".to_string(), 0.02);
        metrics.insert("latency_p50_ms".to_string(), 10.0);
        metrics.insert("latency_p90_ms".to_string(), 25.0);
        metrics.insert("latency_p99_ms".to_string(), 50.0);
        
        println!("Retrieved service mesh metrics for service {}", service_name);
        Ok(metrics)
    }
    
    // Add a routing rule
    pub async fn add_routing_rule(&self, rule: RoutingRule) -> Result<()> {
        let mut routing_rules = self.routing_rules.lock().await;
        routing_rules.push(rule.clone());
        println!("Added routing rule for service {}", rule.service_name);
        Ok(())
    }
    
    // Add a tenant-specific routing rule
    pub async fn add_tenant_routing_rule(
        &self,
        tenant_id: &str,
        service_name: &str,
        path_prefix: Option<String>,
        headers: Option<HashMap<String, String>>,
        weight: Option<u32>,
    ) -> Result<()> {
        // Create a new routing rule with tenant information
        let mut rule = RoutingRule {
            path_prefix,
            headers: headers.clone(),
            service_name: service_name.to_string(),
            weight,
        };
        
        // Add tenant information to headers if not already present
        if let Some(mut headers) = headers {
            headers.insert("X-Tenant-ID".to_string(), tenant_id.to_string());
            rule.headers = Some(headers);
        } else {
            let mut new_headers = HashMap::new();
            new_headers.insert("X-Tenant-ID".to_string(), tenant_id.to_string());
            rule.headers = Some(new_headers);
        }
        
        // Add the rule
        self.add_routing_rule(rule).await
    }
    
    // Remove a routing rule
    pub async fn remove_routing_rule(&self, service_name: &str, path_prefix: Option<&str>) -> Result<()> {
        let mut routing_rules = self.routing_rules.lock().await;
        
        let initial_len = routing_rules.len();
        
        routing_rules.retain(|rule| {
            if rule.service_name != service_name {
                return true;
            }
            
            if let Some(prefix) = path_prefix {
                if let Some(rule_prefix) = &rule.path_prefix {
                    return rule_prefix != prefix;
                }
            }
            
            false
        });
        
        let removed = initial_len - routing_rules.len();
        println!("Removed {} routing rules for service {}", removed, service_name);
        
        Ok(())
    }
    
    // List all routing rules
    pub async fn list_routing_rules(&self) -> Vec<RoutingRule> {
        let routing_rules = self.routing_rules.lock().await;
        routing_rules.clone()
    }
    
    // Configure traffic splitting for canary deployments
    pub async fn configure_traffic_split(&self, config: TrafficSplitConfig) -> Result<()> {
        // Validate that weights sum to 100
        let total_weight: u32 = config.splits.iter().map(|s| s.weight).sum();
        if total_weight != 100 {
            return Err(anyhow::anyhow!("Traffic split weights must sum to 100, got {}", total_weight));
        }
        
        // Store the configuration
        let mut traffic_splits = self.traffic_splits.lock().await;
        traffic_splits.insert(config.service_name.clone(), config.clone());
        
        println!("Configured traffic split for service {}", config.service_name);
        Ok(())
    }
    
    // Remove traffic splitting configuration
    pub async fn remove_traffic_split(&self, service_name: &str) -> Result<()> {
        let mut traffic_splits = self.traffic_splits.lock().await;
        if traffic_splits.remove(service_name).is_some() {
            println!("Removed traffic split for service {}", service_name);
        }
        Ok(())
    }
    
    // Get traffic split configuration
    pub async fn get_traffic_split(&self, service_name: &str) -> Option<TrafficSplitConfig> {
        let traffic_splits = self.traffic_splits.lock().await;
        traffic_splits.get(service_name).cloned()
    }
    
    // Register a service with tenant information
    pub async fn register_service_with_tenant(
        &self,
        service_config: &ServiceConfig,
        node_id: &str,
        ip_address: &str,
        port: u16,
        tenant_id: &str,
    ) -> Result<()> {
        let mut metadata = HashMap::new();
        metadata.insert("tenant_id".to_string(), tenant_id.to_string());
        
        self.register_service_with_version(
            service_config,
            node_id,
            ip_address,
            port,
            "v1", // Default version
            Some(metadata),
        ).await
    }
    
    // Register a service endpoint with version information
    pub async fn register_service_with_version(
        &self,
        service_config: &ServiceConfig,
        node_id: &str,
        ip_address: &str,
        port: u16,
        version: &str,
        metadata: Option<HashMap<String, String>>,
    ) -> Result<()> {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64;

        let endpoint = ServiceEndpoint {
            service_name: service_config.name.clone(),
            domain: service_config.domain.clone(),
            ip_address: ip_address.to_string(),
            port,
            node_id: node_id.to_string(),
            health_status: ServiceHealth::Unknown,
            last_health_check: now,
            version: Some(version.to_string()),
            weight: None,
            metadata,
        };

        let mut services = self.services.lock().await;

        if let Some(endpoints) = services.get_mut(&service_config.name) {
            // Check if this endpoint already exists
            let exists = endpoints
                .iter()
                .any(|e| e.node_id == node_id && e.ip_address == ip_address && e.port == port);

            if !exists {
                endpoints.push(endpoint);
                println!(
                    "Registered new endpoint for service {} (version {}): {}:{}",
                    service_config.name, version, ip_address, port
                );
            }
        } else {
            // First endpoint for this service
            services.insert(service_config.name.clone(), vec![endpoint]);
            println!(
                "Registered new service {} (version {}) with endpoint {}:{}",
                service_config.name, version, ip_address, port
            );
        }

        Ok(())
    }
    
    // Get proxy metrics for monitoring
    pub async fn get_proxy_metrics(&self) -> Result<HashMap<String, f64>> {
        let mut metrics = HashMap::new();
        let endpoint_stats = self.endpoint_stats.lock().await;
        
        // Calculate aggregate metrics
        let mut total_requests: u64 = 0;
        let mut total_failed_requests: u64 = 0;
        let mut total_active_connections: u32 = 0;
        let mut avg_response_times = Vec::new();
        
        for stats in endpoint_stats.values() {
            total_requests += stats.total_requests;
            total_failed_requests += stats.failed_requests;
            total_active_connections += stats.active_connections;
            
            if stats.avg_response_time_ms > 0.0 {
                avg_response_times.push(stats.avg_response_time_ms);
            }
        }
        
        // Calculate overall average response time
        let avg_response_time = if !avg_response_times.is_empty() {
            avg_response_times.iter().sum::<f64>() / avg_response_times.len() as f64
        } else {
            0.0
        };
        
        // Calculate error rate
        let error_rate = if total_requests > 0 {
            total_failed_requests as f64 / total_requests as f64
        } else {
            0.0
        };
        
        // Store metrics
        metrics.insert("total_requests".to_string(), total_requests as f64);
        metrics.insert("failed_requests".to_string(), total_failed_requests as f64);
        metrics.insert("active_connections".to_string(), total_active_connections as f64);
        metrics.insert("avg_response_time_ms".to_string(), avg_response_time);
        metrics.insert("error_rate".to_string(), error_rate);
        metrics.insert("service_count".to_string(), self.services.lock().await.len() as f64);
        metrics.insert("endpoint_count".to_string(),
            self.services.lock().await.values().map(|v| v.len()).sum::<usize>() as f64);
        
        Ok(metrics)
    }
    
    // Get health check statistics for monitoring
    pub async fn get_health_check_statistics(&self) -> Result<HashMap<String, f64>> {
        let mut metrics = HashMap::new();
        let services = self.services.lock().await;
        
        // Count endpoints by health status
        let mut total_endpoints = 0;
        let mut healthy_endpoints = 0;
        let mut unhealthy_endpoints = 0;
        let mut unknown_endpoints = 0;
        
        for endpoints in services.values() {
            for endpoint in endpoints {
                total_endpoints += 1;
                match endpoint.health_status {
                    ServiceHealth::Healthy => healthy_endpoints += 1,
                    ServiceHealth::Unhealthy => unhealthy_endpoints += 1,
                    ServiceHealth::Unknown => unknown_endpoints += 1,
                }
            }
        }
        
        // Calculate health percentages
        let healthy_percent = if total_endpoints > 0 {
            (healthy_endpoints as f64 / total_endpoints as f64) * 100.0
        } else {
            0.0
        };
        
        let unhealthy_percent = if total_endpoints > 0 {
            (unhealthy_endpoints as f64 / total_endpoints as f64) * 100.0
        } else {
            0.0
        };
        
        // Store metrics
        metrics.insert("total_endpoints".to_string(), total_endpoints as f64);
        metrics.insert("healthy_endpoints".to_string(), healthy_endpoints as f64);
        metrics.insert("unhealthy_endpoints".to_string(), unhealthy_endpoints as f64);
        metrics.insert("unknown_endpoints".to_string(), unknown_endpoints as f64);
        metrics.insert("healthy_percent".to_string(), healthy_percent);
        metrics.insert("unhealthy_percent".to_string(), unhealthy_percent);
        
        Ok(metrics)
    }
    
    // Weighted round robin selection based on container metrics
    fn select_weighted_round_robin<'a>(
        endpoints: &[&'a ServiceEndpoint],
        service_name: &str,
        endpoint_stats: &mut HashMap<String, EndpointStats>,
    ) -> &'a ServiceEndpoint {
        // If we have fewer than 2 endpoints, just use the first one
        if endpoints.len() < 2 {
            return endpoints[0];
        }
        
        // Calculate weights based on response time (lower is better)
        let mut weights = Vec::with_capacity(endpoints.len());
        let mut total_weight = 0;
        
        for endpoint in endpoints {
            let stats_key = format!("{}_{}", service_name, endpoint.ip_address);
            let avg_response_time = endpoint_stats
                .entry(stats_key.clone())
                .or_insert(EndpointStats {
                    active_connections: 0,
                    total_requests: 0,
                    failed_requests: 0,
                    avg_response_time_ms: 100.0, // Default response time
                    last_selected: 0,
                })
                .avg_response_time_ms;
            
            // Calculate weight - inverse of response time (faster gets higher weight)
            // Add 1 to avoid division by zero and cap at 1000ms for very slow endpoints
            let response_time = avg_response_time.min(1000.0).max(1.0);
            let weight = (1000.0 / response_time) as u32;
            
            weights.push(weight);
            total_weight += weight;
        }
        
        // If all weights are 0, fall back to random selection
        if total_weight == 0 {
            return Self::select_random(endpoints);
        }
        
        // Get a random value between 0 and total_weight
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .subsec_nanos();
        let mut random_val = (now as u32) % total_weight;
        
        // Find the endpoint that corresponds to this value
        for (i, weight) in weights.iter().enumerate() {
            if random_val < *weight {
                return endpoints[i];
            }
            random_val -= *weight;
        }
        
        // Fallback (should never happen)
        endpoints[0]
    }
    
    // IP hash selection for session affinity
    fn select_ip_hash<'a>(endpoints: &[&'a ServiceEndpoint], client_ip: &str) -> &'a ServiceEndpoint {
        // Simple hash function for the IP address
        let mut hash = 0u32;
        for byte in client_ip.bytes() {
            hash = hash.wrapping_mul(31).wrapping_add(byte as u32);
        }
        
        // Use the hash to select an endpoint
        let index = (hash as usize) % endpoints.len();
        endpoints[index]
    }
    
    // Session affinity based on session ID
    fn select_session_affinity<'a>(endpoints: &[&'a ServiceEndpoint], session_id: &str) -> &'a ServiceEndpoint {
        // Simple hash function for the session ID
        let mut hash = 0u32;
        for byte in session_id.bytes() {
            hash = hash.wrapping_mul(31).wrapping_add(byte as u32);
        }
        
        // Use the hash to select an endpoint
        let index = (hash as usize) % endpoints.len();
        endpoints[index]
    }
    
    // Record connection completion
    pub async fn record_connection_completion(
        &self,
        service_name: &str,
        ip_address: &str,
        success: bool,
        response_time_ms: f64,
    ) -> Result<()> {
        let mut endpoint_stats = self.endpoint_stats.lock().await;
        let stats_key = format!("{}_{}", service_name, ip_address);
        
        if let Some(stats) = endpoint_stats.get_mut(&stats_key) {
            // Decrement active connections
            if stats.active_connections > 0 {
                stats.active_connections -= 1;
            }
            
            // Update failed requests if needed
            if !success {
                stats.failed_requests += 1;
                
                // Check if we need to implement circuit breaker
                let failure_rate = stats.failed_requests as f64 / stats.total_requests as f64;
                if failure_rate > 0.5 && stats.total_requests > 10 {
                    // Mark this endpoint as unhealthy in the service registry
                    self.mark_endpoint_unhealthy(service_name, ip_address).await?;
                }
            }
            
            // Update average response time
            let total_requests = stats.total_requests as f64;
            stats.avg_response_time_ms =
                (stats.avg_response_time_ms * (total_requests - 1.0) + response_time_ms) / total_requests;
        }
        
        Ok(())
    }
    
    // Mark an endpoint as unhealthy (circuit breaker implementation)
    async fn mark_endpoint_unhealthy(&self, service_name: &str, ip_address: &str) -> Result<()> {
        let mut services = self.services.lock().await;
        
        if let Some(endpoints) = services.get_mut(service_name) {
            for endpoint in endpoints.iter_mut() {
                if endpoint.ip_address == ip_address {
                    endpoint.health_status = ServiceHealth::Unhealthy;
                    println!("Circuit breaker: Marked endpoint {}:{} as unhealthy due to high failure rate",
                        ip_address, endpoint.port);
                    
                    // Store circuit breaker state in metadata if it doesn't exist
                    if endpoint.metadata.is_none() {
                        let mut metadata = HashMap::new();
                        metadata.insert("circuit_breaker_state".to_string(), "open".to_string());
                        metadata.insert("circuit_breaker_opened_at".to_string(),
                            SystemTime::now()
                                .duration_since(UNIX_EPOCH)
                                .unwrap()
                                .as_secs()
                                .to_string());
                        endpoint.metadata = Some(metadata);
                    } else if let Some(metadata) = &mut endpoint.metadata {
                        metadata.insert("circuit_breaker_state".to_string(), "open".to_string());
                        metadata.insert("circuit_breaker_opened_at".to_string(),
                            SystemTime::now()
                                .duration_since(UNIX_EPOCH)
                                .unwrap()
                                .as_secs()
                                .to_string());
                    }
                    
                    break;
                }
            }
        }
        
        Ok(())
    }
    
    // Legacy health check method for backward compatibility
    pub async fn health_check_services(&self) -> Result<()> {
        // This now just delegates to the more sophisticated health check system
        Self::run_health_checks(&self.services, &self.health_check_config).await
    }
    
    // Helper method to get routing rules for the proxy server
    async fn get_routing_rules_for_proxy(&self) -> Vec<RoutingRule> {
        let routing_rules = self.routing_rules.lock().await;
        routing_rules.clone()
    }
    
    // Helper method to get traffic split config for the proxy server
    async fn get_traffic_split_config_for_proxy(&self, service_name: &str) -> Option<TrafficSplitConfig> {
        let traffic_splits = self.traffic_splits.lock().await;
        traffic_splits.get(service_name).cloned()
    }
    
    // Implement circuit breaker pattern for service resilience
    pub async fn check_circuit_breaker(&self, service_name: &str, endpoint_ip: &str) -> Result<bool> {
        let services = self.services.lock().await;
        
        if let Some(endpoints) = services.get(service_name) {
            if let Some(endpoint) = endpoints.iter().find(|e| e.ip_address == endpoint_ip) {
                // Check if this endpoint has circuit breaker metadata
                if let Some(metadata) = &endpoint.metadata {
                    if let Some(state) = metadata.get("circuit_breaker_state") {
                        if state == "open" {
                            // Check if it's time to try half-open state
                            if let Some(opened_at_str) = metadata.get("circuit_breaker_opened_at") {
                                if let Ok(opened_at) = opened_at_str.parse::<u64>() {
                                    let now = SystemTime::now()
                                        .duration_since(UNIX_EPOCH)
                                        .unwrap()
                                        .as_secs();
                                    
                                    // If circuit has been open for more than 30 seconds, try half-open
                                    if now - opened_at > 30 {
                                        return Ok(true); // Allow the request to test if service is healthy
                                    }
                                }
                            }
                            
                            // Circuit is open, reject the request
                            return Ok(false);
                        } else if state == "half-open" {
                            // In half-open state, allow limited requests
                            if let Some(attempts_str) = metadata.get("half_open_attempts") {
                                if let Ok(attempts) = attempts_str.parse::<u32>() {
                                    if attempts < 5 { // Allow up to 5 test requests
                                        return Ok(true);
                                    } else {
                                        return Ok(false);
                                    }
                                }
                            }
                        }
                    }
                }
                
                // Default to allowing the request if no circuit breaker info or in closed state
                return Ok(true);
            }
        }
        
        // If service or endpoint not found, allow the request
        Ok(true)
    }
    
    // Record circuit breaker result
    pub async fn record_circuit_breaker_result(&self, service_name: &str, endpoint_ip: &str, success: bool) -> Result<()> {
        let mut services = self.services.lock().await;
        
        if let Some(endpoints) = services.get_mut(service_name) {
            if let Some(endpoint) = endpoints.iter_mut().find(|e| e.ip_address == endpoint_ip) {
                let mut metadata = endpoint.metadata.clone().unwrap_or_default();
                let state = metadata.get("circuit_breaker_state").cloned().unwrap_or_else(|| "closed".to_string());
                
                if state == "open" || state == "half-open" {
                    if success {
                        // Successful request in half-open state
                        let success_count = metadata.get("success_count")
                            .and_then(|s| s.parse::<u32>().ok())
                            .unwrap_or(0) + 1;
                        
                        metadata.insert("success_count".to_string(), success_count.to_string());
                        
                        // If we've had enough successes, close the circuit
                        if success_count >= 5 {
                            metadata.insert("circuit_breaker_state".to_string(), "closed".to_string());
                            metadata.remove("circuit_breaker_opened_at");
                            metadata.remove("success_count");
                            metadata.remove("failure_count");
                            endpoint.health_status = ServiceHealth::Healthy;
                            println!("Circuit breaker: Closed circuit for endpoint {}:{} after successful tests",
                                endpoint_ip, endpoint.port);
                        } else {
                            // Still in testing phase
                            metadata.insert("circuit_breaker_state".to_string(), "half-open".to_string());
                        }
                    } else {
                        // Failed request in half-open state, reopen the circuit
                        metadata.insert("circuit_breaker_state".to_string(), "open".to_string());
                        metadata.insert("circuit_breaker_opened_at".to_string(),
                            SystemTime::now()
                                .duration_since(UNIX_EPOCH)
                                .unwrap()
                                .as_secs()
                                .to_string());
                        metadata.insert("failure_count".to_string(), "1".to_string());
                        metadata.remove("success_count");
                        endpoint.health_status = ServiceHealth::Unhealthy;
                        println!("Circuit breaker: Reopened circuit for endpoint {}:{} after failed test",
                            endpoint_ip, endpoint.port);
                    }
                } else if !success {
                    // In closed state, count failures
                    let failure_count = metadata.get("failure_count")
                        .and_then(|s| s.parse::<u32>().ok())
                        .unwrap_or(0) + 1;
                    
                    metadata.insert("failure_count".to_string(), failure_count.to_string());
                    
                    // If too many failures, open the circuit
                    if failure_count >= 5 {
                        metadata.insert("circuit_breaker_state".to_string(), "open".to_string());
                        metadata.insert("circuit_breaker_opened_at".to_string(),
                            SystemTime::now()
                                .duration_since(UNIX_EPOCH)
                                .unwrap()
                                .as_secs()
                                .to_string());
                        endpoint.health_status = ServiceHealth::Unhealthy;
                        println!("Circuit breaker: Opened circuit for endpoint {}:{} after {} consecutive failures",
                            endpoint_ip, endpoint.port, failure_count);
                    }
                } else {
                    // Successful request in closed state, reset failure count
                    metadata.remove("failure_count");
                }
                
                endpoint.metadata = Some(metadata);
            }
        }
        
        Ok(())
    }
    
    // Enhanced DNS record handling with proper TTL and caching
    pub async fn get_dns_records(&self, domain: &str, record_type: RecordType) -> Vec<Record> {
        let services = self.services.lock().await;
        let mut records = Vec::new();
        let query_name = trust_dns_proto::rr::Name::from_str(domain).unwrap_or_default();
        
        // Find service by domain
        for (service_name, endpoints) in services.iter() {
            for endpoint in endpoints {
                if endpoint.domain == domain && endpoint.health_status == ServiceHealth::Healthy {
                    match record_type {
                        RecordType::A => {
                            if let Ok(ip) = Ipv4Addr::from_str(&endpoint.ip_address) {
                                let rdata = RData::A(trust_dns_proto::rr::rdata::A(ip));
                                let record = Record::from_rdata(query_name.clone(), 60, rdata); // 60 second TTL
                                records.push(record);
                            }
                        },
                        RecordType::AAAA => {
                            // Support for IPv6 addresses
                            if endpoint.ip_address.contains(':') {
                                if let Ok(ip) = std::net::Ipv6Addr::from_str(&endpoint.ip_address) {
                                    let rdata = RData::AAAA(trust_dns_proto::rr::rdata::AAAA(ip));
                                    let record = Record::from_rdata(query_name.clone(), 60, rdata);
                                    records.push(record);
                                }
                            }
                        },
                        RecordType::SRV => {
                            // Create SRV record with proper weight and priority
                            let weight = endpoint.weight.unwrap_or(10);
                            let target_name = trust_dns_proto::rr::Name::from_str(
                                &format!("{}.{}", endpoint.node_id, domain)
                            ).unwrap_or_else(|_| query_name.clone());
                            
                            let rdata = RData::SRV(trust_dns_proto::rr::rdata::SRV::new(
                                0, // priority
                                weight, // weight from endpoint config
                                endpoint.port,
                                target_name,
                            ));
                            let record = Record::from_rdata(query_name.clone(), 60, rdata);
                            records.push(record);
                        },
                        _ => {
                            // Other record types can be added here
                        }
                    }
                }
            }
        }
        
        records
    }
    
    // Get a service endpoint using consistent hashing for better session affinity
    pub async fn get_service_endpoint_consistent_hash(
        &self,
        service_name: &str,
        key: &str,
    ) -> Option<ServiceEndpoint> {
        let services = self.services.lock().await;
        
        if let Some(endpoints) = services.get(service_name) {
            // Filter to only healthy endpoints
            let healthy_endpoints: Vec<&ServiceEndpoint> = endpoints
                .iter()
                .filter(|e| e.health_status == ServiceHealth::Healthy)
                .collect();
            
            if healthy_endpoints.is_empty() {
                return None;
            }
            
            // Use consistent hashing to select an endpoint
            let selected = Self::select_consistent_hash(&healthy_endpoints, key);
            return Some(selected.clone());
        }
        
        None
    }
    
    // Consistent hashing implementation
    fn select_consistent_hash<'a>(endpoints: &[&'a ServiceEndpoint], key: &str) -> &'a ServiceEndpoint {
        // Simple hash function
        let mut hash = 0u32;
        for byte in key.bytes() {
            hash = hash.wrapping_mul(31).wrapping_add(byte as u32);
        }
        
        // Use the hash to select an endpoint
        let index = (hash as usize) % endpoints.len();
        endpoints[index]
    }
}
