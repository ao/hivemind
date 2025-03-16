use crate::app::ServiceConfig;
use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::str::FromStr;
use std::sync::Arc;
use tokio::net::UdpSocket;
use tokio::sync::Mutex;
use trust_dns_proto::op::{Header, MessageType, OpCode, ResponseCode};
use trust_dns_proto::rr::{Name, Record, RecordType, RData, DNSClass};
use trust_dns_proto::serialize::binary::{BinDecodable, BinDecoder, BinEncodable, BinEncoder};
use trust_dns_server::authority::MessageResponseBuilder;

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

#[derive(Clone)]
pub struct ServiceDiscovery {
    services: Arc<Mutex<HashMap<String, Vec<ServiceEndpoint>>>>,
    dns_port: u16,
    proxy_port: u16,
}

impl ServiceDiscovery {
    pub fn new() -> Self {
        Self {
            services: Arc::new(Mutex::new(HashMap::new())),
            dns_port: 53,     // Standard DNS port
            proxy_port: 8080, // Default proxy port
        }
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

    pub async fn health_check_services(&self) -> Result<()> {
        let mut services = self.services.lock().await;
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64;

        for endpoints in services.values_mut() {
            for endpoint in endpoints.iter_mut() {
                // TODO: Implement actual health check logic
                endpoint.health_status = ServiceHealth::Healthy;
                endpoint.last_health_check = now;
            }
        }

        Ok(())
    }
}
