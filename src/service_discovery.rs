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

#[derive(Clone, Debug)]
pub struct ServiceDiscovery {
    services: Arc<Mutex<HashMap<String, Vec<ServiceEndpoint>>>>,
    dns_port: u16,
    proxy_port: u16,
    health_check_config: Arc<Mutex<HashMap<String, HealthCheckConfig>>>,
    load_balancing_strategy: Arc<Mutex<HashMap<String, LoadBalancingStrategy>>>,
    endpoint_stats: Arc<Mutex<HashMap<String, EndpointStats>>>,
    network_manager: Option<Arc<NetworkManager>>,
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
        
        // Start the proxy server if needed
        // self.start_proxy_server().await?;
        
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
        
        // Only handle A record queries for now
        if query_type != RecordType::A {
            return Self::create_error_response(ResponseCode::NotImp);
        }
        
        // Look up the domain in our services
        let services_lock = services.lock().await;
        let mut found_service = false;
        
        for (_, endpoints) in services_lock.iter() {
            for endpoint in endpoints {
                if endpoint.domain == domain {
                    found_service = true;
                    
                    // Create an A record for this endpoint
                    if let Ok(ip) = Ipv4Addr::from_str(&endpoint.ip_address) {
                        let mut records = Vec::new();
                        
                        let rdata = RData::A(trust_dns_proto::rr::rdata::A(ip));
                        let record = Record::from_rdata(query_name.clone(), 300, rdata);
                        records.push(record);
                        
                        // Build the response with the records
                        let mut response = trust_dns_proto::op::Message::new();
                        response.set_header(Header::response_from_request(query.header()));
                        response.add_answers(records);
                        return response;
                    }
                }
            }
        }
        
        if !found_service {
            // Domain not found
            return Self::create_error_response(ResponseCode::NXDomain);
        }
        
        // If we get here, we found the service but couldn't create a valid A record
        Self::create_error_response(ResponseCode::ServFail)
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

        tokio::spawn(async move {
            if let Err(e) = Self::run_proxy_server(proxy_port, services).await {
                eprintln!("Proxy server error: {}", e);
            }
        });

        Ok(())
    }

    async fn run_proxy_server(
        _proxy_port: u16,
        _services: Arc<Mutex<HashMap<String, Vec<ServiceEndpoint>>>>,
    ) -> Result<()> {
        // TODO: Implement proxy server logic
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
                    
                    // Update endpoint health status
                    endpoint.health_status = health_status.clone();
                    endpoint.last_health_check = _now;
                    
                    println!(
                        "Health check for {}/{}: {}",
                        service_name, endpoint.ip_address, health_status
                    );
                }
            }
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
    async fn make_http_request(_url: &str) -> Result<()> {
        // In a real implementation, this would use a proper HTTP client
        // For now, we'll just simulate a successful request
        // This is a placeholder for actual HTTP client implementation
        Ok(())
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
                    // For now, fall back to regular round robin
                    Self::select_round_robin(&healthy_endpoints, service_name, &mut endpoint_stats)
                }
                LoadBalancingStrategy::IPHash => {
                    // For now, fall back to regular round robin
                    Self::select_round_robin(&healthy_endpoints, service_name, &mut endpoint_stats)
                }
            };
            
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
        let _now = SystemTime::now()
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
            }
            
            // Update average response time
            let total_requests = stats.total_requests as f64;
            stats.avg_response_time_ms = 
                (stats.avg_response_time_ms * (total_requests - 1.0) + response_time_ms) / total_requests;
        }
        
        Ok(())
    }
    
    // Legacy health check method for backward compatibility
    pub async fn health_check_services(&self) -> Result<()> {
        // This now just delegates to the more sophisticated health check system
        Self::run_health_checks(&self.services, &self.health_check_config).await
    }
}
