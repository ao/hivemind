use hivemind::app::ServiceConfig;
use hivemind::network::{ContainerNetworkConfig, NetworkManager};
use hivemind::service_discovery::{
    CircuitBreakerPolicy, CircuitBreakerState, HealthCheckConfig, HealthCheckProtocol,
    LoadBalancingStrategy, ServiceDiscovery, ServiceEndpoint, ServiceHealth, TrafficSplit,
    TrafficSplitConfig,
};
use std::collections::HashMap;
use std::net::Ipv4Addr;
use std::sync::Arc;
use std::time::Duration;
use tokio::sync::Mutex;
use tokio::time::sleep;

// Helper function to create a test service discovery instance
async fn create_test_service_discovery() -> ServiceDiscovery {
    let network_manager = Arc::new(NetworkManager::new());
    ServiceDiscovery::new().with_network_manager(network_manager)
}

// Helper function to register test services
async fn register_test_services(sd: &ServiceDiscovery) {
    // Create service configs
    let service1 = ServiceConfig {
        name: "service1".to_string(),
        domain: "service1.test".to_string(),
        container_ids: vec!["container1".to_string(), "container2".to_string()],
        desired_replicas: 2,
        current_replicas: 2,
    };

    let service2 = ServiceConfig {
        name: "service2".to_string(),
        domain: "service2.test".to_string(),
        container_ids: vec!["container3".to_string()],
        desired_replicas: 1,
        current_replicas: 1,
    };

    // Register endpoints
    sd.register_service(&service1, "node1", "192.168.1.10", 8080)
        .await
        .unwrap();
    sd.register_service(&service1, "node2", "192.168.1.11", 8080)
        .await
        .unwrap();
    sd.register_service(&service2, "node1", "192.168.1.12", 9090)
        .await
        .unwrap();
}

#[tokio::test]
async fn test_service_registration() {
    let sd = create_test_service_discovery().await;

    // Create a service config
    let service = ServiceConfig {
        name: "test-service".to_string(),
        domain: "test.service".to_string(),
        container_ids: vec!["container1".to_string()],
        desired_replicas: 1,
        current_replicas: 1,
    };

    // Register a service endpoint
    sd.register_service(&service, "node1", "192.168.1.10", 8080)
        .await
        .unwrap();

    // Verify the service was registered
    let endpoints = sd.get_service_endpoints("test-service").await.unwrap();
    assert_eq!(endpoints.len(), 1);
    assert_eq!(endpoints[0].service_name, "test-service");
    assert_eq!(endpoints[0].domain, "test.service");
    assert_eq!(endpoints[0].ip_address, "192.168.1.10");
    assert_eq!(endpoints[0].port, 8080);
    assert_eq!(endpoints[0].node_id, "node1");
}

#[tokio::test]
async fn test_service_deregistration() {
    let sd = create_test_service_discovery().await;

    // Register a service
    let service = ServiceConfig {
        name: "test-service".to_string(),
        domain: "test.service".to_string(),
        container_ids: vec!["container1".to_string()],
        desired_replicas: 1,
        current_replicas: 1,
    };

    sd.register_service(&service, "node1", "192.168.1.10", 8080)
        .await
        .unwrap();

    // Verify the service was registered
    let endpoints = sd.get_service_endpoints("test-service").await.unwrap();
    assert_eq!(endpoints.len(), 1);

    // Deregister the service
    sd.deregister_service("test-service", "node1", "192.168.1.10", 8080)
        .await
        .unwrap();

    // Verify the service was deregistered
    let endpoints = sd.get_service_endpoints("test-service").await;
    assert!(endpoints.is_none());
}

#[tokio::test]
async fn test_load_balancing_strategies() {
    let sd = create_test_service_discovery().await;
    register_test_services(&sd).await;

    // Configure different load balancing strategies
    sd.configure_load_balancing("service1", LoadBalancingStrategy::RoundRobin)
        .await
        .unwrap();
    sd.configure_load_balancing("service2", LoadBalancingStrategy::Random)
        .await
        .unwrap();

    // Test round-robin strategy
    let endpoint1 = sd.get_service_endpoint("service1").await.unwrap();
    let endpoint2 = sd.get_service_endpoint("service1").await.unwrap();
    // With round-robin, we should get different endpoints
    assert_ne!(endpoint1.ip_address, endpoint2.ip_address);

    // Test random strategy
    let _endpoint = sd.get_service_endpoint("service2").await.unwrap();
    // We can't assert much about random selection, but it should return an endpoint
}

#[tokio::test]
async fn test_health_checks() {
    let sd = create_test_service_discovery().await;

    // Register a service
    let service = ServiceConfig {
        name: "health-test".to_string(),
        domain: "health.test".to_string(),
        container_ids: vec!["container1".to_string()],
        desired_replicas: 1,
        current_replicas: 1,
    };

    sd.register_service(&service, "node1", "192.168.1.10", 8080)
        .await
        .unwrap();

    // Configure health check
    let health_check = HealthCheckConfig {
        protocol: HealthCheckProtocol::TCP,
        path: None,
        interval_secs: 1,
        timeout_secs: 1,
        healthy_threshold: 1,
        unhealthy_threshold: 1,
    };

    sd.configure_health_check("health-test", health_check)
        .await
        .unwrap();

    // Start health check system
    sd.start_health_check_system().await.unwrap();

    // Wait for health check to run
    sleep(Duration::from_secs(2)).await;

    // Get the service endpoint and check its health status
    let endpoints = sd.get_service_endpoints("health-test").await.unwrap();
    assert_eq!(endpoints.len(), 1);
    
    // Since we're using a non-existent IP in the test, the endpoint should be marked as unhealthy
    assert_eq!(endpoints[0].health_status, ServiceHealth::Unhealthy);
}

#[tokio::test]
async fn test_circuit_breaker() {
    let sd = create_test_service_discovery().await;

    // Register a service
    let service = ServiceConfig {
        name: "circuit-test".to_string(),
        domain: "circuit.test".to_string(),
        container_ids: vec!["container1".to_string()],
        desired_replicas: 1,
        current_replicas: 1,
    };

    sd.register_service(&service, "node1", "192.168.1.10", 8080)
        .await
        .unwrap();

    // Simulate multiple failed requests
    for _ in 0..5 {
        sd.record_connection_completion("circuit-test", "192.168.1.10", false, 100.0)
            .await
            .unwrap();
    }

    // Check if circuit breaker opened
    let allow_request = sd.check_circuit_breaker("circuit-test", "192.168.1.10").await.unwrap();
    assert!(!allow_request); // Circuit should be open, so request should not be allowed

    // Get the service endpoint and check its health status
    let endpoints = sd.get_service_endpoints("circuit-test").await.unwrap();
    assert_eq!(endpoints.len(), 1);
    assert_eq!(endpoints[0].health_status, ServiceHealth::Unhealthy);

    // Simulate circuit half-open and successful request
    if let Some(metadata) = &endpoints[0].metadata {
        if let Some(state) = metadata.get("circuit_breaker_state") {
            assert_eq!(state, "open");
        }
    }

    // Record a successful request to close the circuit
    sd.record_circuit_breaker_result("circuit-test", "192.168.1.10", true)
        .await
        .unwrap();
    sd.record_circuit_breaker_result("circuit-test", "192.168.1.10", true)
        .await
        .unwrap();
    sd.record_circuit_breaker_result("circuit-test", "192.168.1.10", true)
        .await
        .unwrap();
    sd.record_circuit_breaker_result("circuit-test", "192.168.1.10", true)
        .await
        .unwrap();
    sd.record_circuit_breaker_result("circuit-test", "192.168.1.10", true)
        .await
        .unwrap();

    // Check if circuit breaker closed
    let allow_request = sd.check_circuit_breaker("circuit-test", "192.168.1.10").await.unwrap();
    assert!(allow_request); // Circuit should be closed, so request should be allowed
}

#[tokio::test]
async fn test_traffic_splitting() {
    let sd = create_test_service_discovery().await;

    // Register service with versions
    let service = ServiceConfig {
        name: "split-test".to_string(),
        domain: "split.test".to_string(),
        container_ids: vec!["container1".to_string(), "container2".to_string()],
        desired_replicas: 2,
        current_replicas: 2,
    };

    sd.register_service_with_version(&service, "node1", "192.168.1.10", 8080, "v1", None)
        .await
        .unwrap();
    sd.register_service_with_version(&service, "node2", "192.168.1.11", 8080, "v2", None)
        .await
        .unwrap();

    // Configure traffic split
    let traffic_split = TrafficSplitConfig {
        service_name: "split-test".to_string(),
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

    sd.configure_traffic_split(traffic_split).await.unwrap();

    // Get the traffic split configuration
    let config = sd.get_traffic_split("split-test").await.unwrap();
    assert_eq!(config.splits.len(), 2);
    assert_eq!(config.splits[0].version, "v1");
    assert_eq!(config.splits[0].weight, 80);
    assert_eq!(config.splits[1].version, "v2");
    assert_eq!(config.splits[1].weight, 20);
}

#[tokio::test]
async fn test_dns_records() {
    let sd = create_test_service_discovery().await;

    // Register a service
    let service = ServiceConfig {
        name: "dns-test".to_string(),
        domain: "dns.test".to_string(),
        container_ids: vec!["container1".to_string()],
        desired_replicas: 1,
        current_replicas: 1,
    };

    sd.register_service(&service, "node1", "192.168.1.10", 8080)
        .await
        .unwrap();

    // Mark the endpoint as healthy
    let mut services = sd.list_services().await;
    if let Some(endpoints) = services.get_mut("dns-test") {
        endpoints[0].health_status = ServiceHealth::Healthy;
    }

    // Get DNS A records
    let records = sd
        .get_dns_records("dns.test", trust_dns_proto::rr::RecordType::A)
        .await;
    assert_eq!(records.len(), 1);

    // Get DNS SRV records
    let records = sd
        .get_dns_records("dns.test", trust_dns_proto::rr::RecordType::SRV)
        .await;
    assert_eq!(records.len(), 1);
}

#[tokio::test]
async fn test_container_integration() {
    let sd = create_test_service_discovery().await;

    // Simulate container creation
    let container_id = "test-container-123";
    let service_name = "container-test";
    let domain = "container.test";
    let node_id = "node1";
    let network_config = ContainerNetworkConfig {
        container_id: container_id.to_string(),
        network_id: "test-network".to_string(),
        ip_address: "192.168.1.20".parse::<Ipv4Addr>().unwrap(),
        gateway: "192.168.1.1".parse::<Ipv4Addr>().unwrap(),
        subnet_mask: "255.255.255.0".to_string(),
        dns_servers: vec!["8.8.8.8".to_string()],
        mac_address: None,
    };

    sd.handle_container_created(container_id, service_name, domain, node_id, &network_config)
        .await
        .unwrap();

    // Verify the service was registered
    let endpoints = sd.get_service_endpoints(service_name).await.unwrap();
    assert_eq!(endpoints.len(), 1);
    assert_eq!(endpoints[0].service_name, service_name);
    assert_eq!(endpoints[0].domain, domain);
    assert_eq!(endpoints[0].ip_address, "192.168.1.20");
    assert_eq!(endpoints[0].node_id, node_id);

    // Simulate container removal
    sd.handle_container_removed(container_id, service_name, node_id, "192.168.1.20")
        .await
        .unwrap();

    // Verify the service was deregistered
    let endpoints = sd.get_service_endpoints(service_name).await;
    assert!(endpoints.is_none());
}

#[tokio::test]
async fn test_node_events() {
    let sd = create_test_service_discovery().await;

    // Register services on multiple nodes
    let service = ServiceConfig {
        name: "node-test".to_string(),
        domain: "node.test".to_string(),
        container_ids: vec!["container1".to_string(), "container2".to_string()],
        desired_replicas: 2,
        current_replicas: 2,
    };

    sd.register_service(&service, "node1", "192.168.1.10", 8080)
        .await
        .unwrap();
    sd.register_service(&service, "node2", "192.168.1.11", 8080)
        .await
        .unwrap();

    // Verify services are registered
    let endpoints = sd.get_service_endpoints("node-test").await.unwrap();
    assert_eq!(endpoints.len(), 2);

    // Simulate node leaving
    sd.handle_node_left("node1").await.unwrap();

    // Verify endpoints on node1 are removed
    let endpoints = sd.get_service_endpoints("node-test").await.unwrap();
    assert_eq!(endpoints.len(), 1);
    assert_eq!(endpoints[0].node_id, "node2");

    // Simulate node joining
    sd.handle_node_joined("node3", "192.168.1.12").await.unwrap();

    // Register service on new node
    sd.register_service(&service, "node3", "192.168.1.12", 8080)
        .await
        .unwrap();

    // Verify new endpoint is registered
    let endpoints = sd.get_service_endpoints("node-test").await.unwrap();
    assert_eq!(endpoints.len(), 2);
    assert!(endpoints.iter().any(|e| e.node_id == "node3"));
}

#[tokio::test]
async fn test_consistent_hashing() {
    let sd = create_test_service_discovery().await;

    // Register multiple endpoints for a service
    let service = ServiceConfig {
        name: "hash-test".to_string(),
        domain: "hash.test".to_string(),
        container_ids: vec!["container1".to_string(), "container2".to_string(), "container3".to_string()],
        desired_replicas: 3,
        current_replicas: 3,
    };

    sd.register_service(&service, "node1", "192.168.1.10", 8080)
        .await
        .unwrap();
    sd.register_service(&service, "node2", "192.168.1.11", 8080)
        .await
        .unwrap();
    sd.register_service(&service, "node3", "192.168.1.12", 8080)
        .await
        .unwrap();

    // Mark all endpoints as healthy
    let mut services = sd.list_services().await;
    if let Some(endpoints) = services.get_mut("hash-test") {
        for endpoint in endpoints {
            endpoint.health_status = ServiceHealth::Healthy;
        }
    }

    // Test consistent hashing with the same key
    let endpoint1 = sd.get_service_endpoint_consistent_hash("hash-test", "user123").await.unwrap();
    let endpoint2 = sd.get_service_endpoint_consistent_hash("hash-test", "user123").await.unwrap();
    
    // Same key should route to the same endpoint
    assert_eq!(endpoint1.ip_address, endpoint2.ip_address);
    
    // Different keys should potentially route to different endpoints
    let endpoint3 = sd.get_service_endpoint_consistent_hash("hash-test", "user456").await.unwrap();
    // Note: There's a small chance this could be the same endpoint by random chance
}