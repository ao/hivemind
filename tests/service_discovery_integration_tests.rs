use hivemind::app::ServiceConfig;
use hivemind::containerd_manager::ContainerManager;
use hivemind::network::{ContainerNetworkConfig, NetworkManager, NetworkPlugin};
use hivemind::service_discovery::{
    HealthCheckConfig, HealthCheckProtocol, LoadBalancingStrategy, ServiceDiscovery, ServiceHealth,
    TrafficSplitConfig, TrafficSplit,
};
use std::collections::HashMap;
use std::net::Ipv4Addr;
use std::sync::Arc;
use std::time::Duration;
use tokio::sync::Mutex;
use tokio::time::sleep;

// Mock container manager for testing
struct MockContainerManager {
    containers: Arc<Mutex<HashMap<String, String>>>, // container_id -> service_name
}

impl MockContainerManager {
    fn new() -> Self {
        Self {
            containers: Arc::new(Mutex::new(HashMap::new())),
        }
    }

    async fn create_container(&self, service_name: &str) -> String {
        let container_id = format!("{}-container-{}", service_name, rand::random::<u32>());
        let mut containers = self.containers.lock().await;
        containers.insert(container_id.clone(), service_name.to_string());
        container_id
    }

    async fn remove_container(&self, container_id: &str) -> bool {
        let mut containers = self.containers.lock().await;
        containers.remove(container_id).is_some()
    }
}

// Mock network plugin for testing
struct MockNetworkPlugin {
    networks: Arc<Mutex<HashMap<String, Vec<String>>>>, // network_id -> container_ids
    container_ips: Arc<Mutex<HashMap<String, String>>>, // container_id -> ip
}

impl MockNetworkPlugin {
    fn new() -> Self {
        Self {
            networks: Arc::new(Mutex::new(HashMap::new())),
            container_ips: Arc::new(Mutex::new(HashMap::new())),
        }
    }

    async fn create_network(&self, network_id: &str) {
        let mut networks = self.networks.lock().await;
        networks.insert(network_id.to_string(), Vec::new());
    }

    async fn connect_container(&self, network_id: &str, container_id: &str) -> String {
        // Assign an IP address
        let ip = format!("192.168.{}.{}", rand::random::<u8>(), rand::random::<u8>());
        
        // Add container to network
        let mut networks = self.networks.lock().await;
        if let Some(containers) = networks.get_mut(network_id) {
            containers.push(container_id.to_string());
        }
        
        // Store container IP
        let mut container_ips = self.container_ips.lock().await;
        container_ips.insert(container_id.to_string(), ip.clone());
        
        ip
    }

    async fn disconnect_container(&self, network_id: &str, container_id: &str) {
        // Remove container from network
        let mut networks = self.networks.lock().await;
        if let Some(containers) = networks.get_mut(network_id) {
            containers.retain(|id| id != container_id);
        }
        
        // Remove container IP
        let mut container_ips = self.container_ips.lock().await;
        container_ips.remove(container_id);
    }
}

// Setup function to create test environment
async fn setup_test_environment() -> (
    ServiceDiscovery,
    Arc<NetworkManager>,
    MockContainerManager,
    MockNetworkPlugin,
) {
    // Create network plugin
    let network_plugin = MockNetworkPlugin::new();
    
    // Create network manager
    let network_manager = Arc::new(NetworkManager::new());
    
    // Create service discovery
    let service_discovery = ServiceDiscovery::new().with_network_manager(network_manager.clone());
    
    // Create container manager
    let container_manager = MockContainerManager::new();
    
    // Initialize service discovery
    service_discovery.initialize().await.unwrap();
    
    // Create test network
    network_plugin.create_network("test-network").await;
    
    (service_discovery, network_manager, container_manager, network_plugin)
}

#[tokio::test]
async fn test_service_discovery_with_container_lifecycle() {
    let (service_discovery, _network_manager, container_manager, network_plugin) =
        setup_test_environment().await;

    // Create a service
    let service_name = "web-service";
    let domain = "web.service";
    let node_id = "test-node";

    // Create containers for the service
    let container_id1 = container_manager.create_container(service_name).await;
    let container_id2 = container_manager.create_container(service_name).await;

    // Connect containers to network
    let ip1 = network_plugin.connect_container("test-network", &container_id1).await;
    let ip2 = network_plugin.connect_container("test-network", &container_id2).await;

    // Create network configs
    let network_config1 = ContainerNetworkConfig {
        container_id: container_id1.clone(),
        network_id: "test-network".to_string(),
        ip_address: ip1.parse::<Ipv4Addr>().unwrap(),
        gateway: "192.168.1.1".parse::<Ipv4Addr>().unwrap(),
        subnet_mask: "255.255.255.0".to_string(),
        dns_servers: vec!["8.8.8.8".to_string()],
        mac_address: None,
    };

    let network_config2 = ContainerNetworkConfig {
        container_id: container_id2.clone(),
        network_id: "test-network".to_string(),
        ip_address: ip2.parse::<Ipv4Addr>().unwrap(),
        gateway: "192.168.1.1".parse::<Ipv4Addr>().unwrap(),
        subnet_mask: "255.255.255.0".to_string(),
        dns_servers: vec!["8.8.8.8".to_string()],
        mac_address: None,
    };

    // Register containers with service discovery
    service_discovery
        .handle_container_created(&container_id1, service_name, domain, node_id, &network_config1)
        .await
        .unwrap();
    service_discovery
        .handle_container_created(&container_id2, service_name, domain, node_id, &network_config2)
        .await
        .unwrap();

    // Verify service endpoints
    let endpoints = service_discovery.get_service_endpoints(service_name).await.unwrap();
    assert_eq!(endpoints.len(), 2);
    assert!(endpoints.iter().any(|e| e.ip_address == ip1));
    assert!(endpoints.iter().any(|e| e.ip_address == ip2));

    // Configure health checks
    let health_check = HealthCheckConfig {
        protocol: HealthCheckProtocol::TCP,
        path: None,
        interval_secs: 1,
        timeout_secs: 1,
        healthy_threshold: 1,
        unhealthy_threshold: 1,
    };

    service_discovery
        .configure_health_check(service_name, health_check)
        .await
        .unwrap();

    // Configure load balancing
    service_discovery
        .configure_load_balancing(service_name, LoadBalancingStrategy::RoundRobin)
        .await
        .unwrap();

    // Test load balancing
    let endpoint1 = service_discovery.get_service_endpoint(service_name).await.unwrap();
    let endpoint2 = service_discovery.get_service_endpoint(service_name).await.unwrap();
    assert_ne!(endpoint1.ip_address, endpoint2.ip_address);

    // Remove one container
    container_manager.remove_container(&container_id1).await;
    network_plugin
        .disconnect_container("test-network", &container_id1)
        .await;
    service_discovery
        .handle_container_removed(&container_id1, service_name, node_id, &ip1)
        .await
        .unwrap();

    // Verify service endpoints after removal
    let endpoints = service_discovery.get_service_endpoints(service_name).await.unwrap();
    assert_eq!(endpoints.len(), 1);
    assert_eq!(endpoints[0].ip_address, ip2);
}

#[tokio::test]
async fn test_service_discovery_with_node_events() {
    let (service_discovery, _network_manager, container_manager, network_plugin) =
        setup_test_environment().await;

    // Create services on multiple nodes
    let service_name = "api-service";
    let domain = "api.service";
    let node1_id = "node1";
    let node2_id = "node2";

    // Create containers for the service on different nodes
    let container_id1 = container_manager.create_container(service_name).await;
    let container_id2 = container_manager.create_container(service_name).await;

    // Connect containers to network
    let ip1 = network_plugin.connect_container("test-network", &container_id1).await;
    let ip2 = network_plugin.connect_container("test-network", &container_id2).await;

    // Create network configs
    let network_config1 = ContainerNetworkConfig {
        container_id: container_id1.clone(),
        network_id: "test-network".to_string(),
        ip_address: ip1.parse::<Ipv4Addr>().unwrap(),
        gateway: "192.168.1.1".parse::<Ipv4Addr>().unwrap(),
        subnet_mask: "255.255.255.0".to_string(),
        dns_servers: vec!["8.8.8.8".to_string()],
        mac_address: None,
    };

    let network_config2 = ContainerNetworkConfig {
        container_id: container_id2.clone(),
        network_id: "test-network".to_string(),
        ip_address: ip2.parse::<Ipv4Addr>().unwrap(),
        gateway: "192.168.1.1".parse::<Ipv4Addr>().unwrap(),
        subnet_mask: "255.255.255.0".to_string(),
        dns_servers: vec!["8.8.8.8".to_string()],
        mac_address: None,
    };

    // Register containers with service discovery on different nodes
    service_discovery
        .handle_container_created(&container_id1, service_name, domain, node1_id, &network_config1)
        .await
        .unwrap();
    service_discovery
        .handle_container_created(&container_id2, service_name, domain, node2_id, &network_config2)
        .await
        .unwrap();

    // Verify service endpoints
    let endpoints = service_discovery.get_service_endpoints(service_name).await.unwrap();
    assert_eq!(endpoints.len(), 2);

    // Simulate node1 leaving the cluster
    service_discovery.handle_node_left(node1_id).await.unwrap();

    // Verify service endpoints after node1 left
    let endpoints = service_discovery.get_service_endpoints(service_name).await.unwrap();
    assert_eq!(endpoints.len(), 1);
    assert_eq!(endpoints[0].node_id, node2_id);
    assert_eq!(endpoints[0].ip_address, ip2);

    // Simulate node1 rejoining with a new IP
    let node1_ip = "192.168.10.10";
    service_discovery.handle_node_joined(node1_id, node1_ip).await.unwrap();

    // Create a new container on node1
    let container_id3 = container_manager.create_container(service_name).await;
    let ip3 = network_plugin.connect_container("test-network", &container_id3).await;

    let network_config3 = ContainerNetworkConfig {
        container_id: container_id3.clone(),
        network_id: "test-network".to_string(),
        ip_address: ip3.parse::<Ipv4Addr>().unwrap(),
        gateway: "192.168.1.1".parse::<Ipv4Addr>().unwrap(),
        subnet_mask: "255.255.255.0".to_string(),
        dns_servers: vec!["8.8.8.8".to_string()],
        mac_address: None,
    };

    // Register the new container
    service_discovery
        .handle_container_created(&container_id3, service_name, domain, node1_id, &network_config3)
        .await
        .unwrap();

    // Verify service endpoints after node1 rejoined
    let endpoints = service_discovery.get_service_endpoints(service_name).await.unwrap();
    assert_eq!(endpoints.len(), 2);
    assert!(endpoints.iter().any(|e| e.node_id == node1_id));
    assert!(endpoints.iter().any(|e| e.node_id == node2_id));
}

#[tokio::test]
async fn test_service_discovery_with_traffic_splitting() {
    let (service_discovery, _network_manager, container_manager, network_plugin) =
        setup_test_environment().await;

    // Create a service with multiple versions
    let service_name = "frontend";
    let domain = "frontend.service";
    let node_id = "test-node";

    // Create containers for different versions
    let container_id_v1 = container_manager.create_container(&format!("{}-v1", service_name)).await;
    let container_id_v2 = container_manager.create_container(&format!("{}-v2", service_name)).await;

    // Connect containers to network
    let ip_v1 = network_plugin.connect_container("test-network", &container_id_v1).await;
    let ip_v2 = network_plugin.connect_container("test-network", &container_id_v2).await;

    // Create network configs
    let network_config_v1 = ContainerNetworkConfig {
        container_id: container_id_v1.clone(),
        network_id: "test-network".to_string(),
        ip_address: ip_v1.parse::<Ipv4Addr>().unwrap(),
        gateway: "192.168.1.1".parse::<Ipv4Addr>().unwrap(),
        subnet_mask: "255.255.255.0".to_string(),
        dns_servers: vec!["8.8.8.8".to_string()],
        mac_address: None,
    };

    let network_config_v2 = ContainerNetworkConfig {
        container_id: container_id_v2.clone(),
        network_id: "test-network".to_string(),
        ip_address: ip_v2.parse::<Ipv4Addr>().unwrap(),
        gateway: "192.168.1.1".parse::<Ipv4Addr>().unwrap(),
        subnet_mask: "255.255.255.0".to_string(),
        dns_servers: vec!["8.8.8.8".to_string()],
        mac_address: None,
    };

    // Register containers with service discovery with version information
    let service_config = ServiceConfig {
        name: service_name.to_string(),
        domain: domain.to_string(),
        container_ids: vec![container_id_v1.clone(), container_id_v2.clone()],
        desired_replicas: 2,
        current_replicas: 2,
    };

    service_discovery
        .register_service_with_version(&service_config, node_id, &ip_v1, 8080, "v1", None)
        .await
        .unwrap();
    service_discovery
        .register_service_with_version(&service_config, node_id, &ip_v2, 8080, "v2", None)
        .await
        .unwrap();

    // Configure traffic split (80% to v1, 20% to v2)
    let traffic_split = TrafficSplitConfig {
        service_name: service_name.to_string(),
        splits: vec![
            TrafficSplit {
                version: "v1".to_string(),
                weight: 80,
            },
            TrafficSplit {
                version: "v2".to_string(),
                weight: 20,
            },
        ],
    };

    service_discovery
        .configure_traffic_split(traffic_split)
        .await
        .unwrap();

    // Mark endpoints as healthy for testing
    let mut services = service_discovery.list_services().await;
    if let Some(endpoints) = services.get_mut(service_name) {
        for endpoint in endpoints {
            endpoint.health_status = ServiceHealth::Healthy;
        }
    }

    // Test traffic splitting by making multiple requests
    let mut v1_count = 0;
    let mut v2_count = 0;
    let total_requests = 100;

    for _ in 0..total_requests {
        let endpoint = service_discovery.get_service_endpoint(service_name).await.unwrap();
        if endpoint.version.as_deref() == Some("v1") {
            v1_count += 1;
        } else if endpoint.version.as_deref() == Some("v2") {
            v2_count += 1;
        }
    }

    // Check that the distribution is roughly as expected
    // Allow for some statistical variation
    assert!(v1_count > v2_count);
    assert!(v1_count >= 65); // Should be around 80, but allow for variation
    assert!(v2_count <= 35); // Should be around 20, but allow for variation
}

#[tokio::test]
async fn test_service_discovery_with_circuit_breaker() {
    let (service_discovery, _network_manager, container_manager, network_plugin) =
        setup_test_environment().await;

    // Create a service
    let service_name = "auth-service";
    let domain = "auth.service";
    let node_id = "test-node";

    // Create container for the service
    let container_id = container_manager.create_container(service_name).await;

    // Connect container to network
    let ip = network_plugin.connect_container("test-network", &container_id).await;

    // Create network config
    let network_config = ContainerNetworkConfig {
        container_id: container_id.clone(),
        network_id: "test-network".to_string(),
        ip_address: ip.parse::<Ipv4Addr>().unwrap(),
        gateway: "192.168.1.1".parse::<Ipv4Addr>().unwrap(),
        subnet_mask: "255.255.255.0".to_string(),
        dns_servers: vec!["8.8.8.8".to_string()],
        mac_address: None,
    };

    // Register container with service discovery
    service_discovery
        .handle_container_created(&container_id, service_name, domain, node_id, &network_config)
        .await
        .unwrap();

    // Mark the endpoint as healthy
    let mut services = service_discovery.list_services().await;
    if let Some(endpoints) = services.get_mut(service_name) {
        endpoints[0].health_status = ServiceHealth::Healthy;
    }

    // Verify service endpoint is available
    let endpoint = service_discovery.get_service_endpoint(service_name).await.unwrap();
    assert_eq!(endpoint.ip_address, ip);

    // Simulate multiple failed requests to trigger circuit breaker
    for _ in 0..5 {
        service_discovery
            .record_connection_completion(service_name, &ip, false, 100.0)
            .await
            .unwrap();
    }

    // Check if circuit breaker opened
    let allow_request = service_discovery.check_circuit_breaker(service_name, &ip).await.unwrap();
    assert!(!allow_request); // Circuit should be open, so request should not be allowed

    // Verify endpoint is marked as unhealthy
    let endpoints = service_discovery.get_service_endpoints(service_name).await.unwrap();
    assert_eq!(endpoints[0].health_status, ServiceHealth::Unhealthy);

    // Simulate circuit half-open and successful requests to close the circuit
    for _ in 0..5 {
        service_discovery
            .record_circuit_breaker_result(service_name, &ip, true)
            .await
            .unwrap();
    }

    // Check if circuit breaker closed
    let allow_request = service_discovery.check_circuit_breaker(service_name, &ip).await.unwrap();
    assert!(allow_request); // Circuit should be closed, so request should be allowed
}