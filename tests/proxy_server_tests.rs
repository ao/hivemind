use hivemind::proxy_server::{ProxyServer, ProxyServerConfig};
use hivemind::service_discovery::{ServiceDiscovery, ServiceEndpoint, ServiceHealth};
use std::sync::Arc;
use tokio::sync::Mutex;
use std::collections::HashMap;
use std::time::Duration;
use anyhow::Result;

#[tokio::test]
async fn test_proxy_server_creation() -> Result<()> {
    // Create a service discovery instance
    let service_discovery = ServiceDiscovery::new();
    
    // Create a proxy server
    let proxy_server = ProxyServer::new(Arc::new(service_discovery));
    
    // Configure the proxy server
    let config = ProxyServerConfig {
        port: 8081, // Use a different port for testing
        ..ProxyServerConfig::default()
    };
    proxy_server.configure(config).await;
    
    // Verify the proxy server was created successfully
    assert_eq!(proxy_server.get_active_connections().await, 0);
    
    Ok(())
}

#[tokio::test]
async fn test_proxy_server_metrics() -> Result<()> {
    // Create a service discovery instance
    let service_discovery = ServiceDiscovery::new();
    
    // Create a proxy server
    let proxy_server = ProxyServer::new(Arc::new(service_discovery));
    
    // Get metrics for a non-existent service
    let metrics = proxy_server.get_metrics("test-service").await;
    assert!(metrics.is_none());
    
    // Get all metrics
    let all_metrics = proxy_server.get_all_metrics().await;
    assert!(all_metrics.is_empty());
    
    Ok(())
}

// This test requires a running service to proxy to, so we'll skip it for now
// and just outline what it would test
#[tokio::test]
#[ignore]
async fn test_proxy_server_forwarding() -> Result<()> {
    // Create a service discovery instance
    let service_discovery = ServiceDiscovery::new();
    
    // Register a test service
    // service_discovery.register_service(...).await?;
    
    // Create a proxy server
    let proxy_server = ProxyServer::new(Arc::new(service_discovery));
    
    // Configure the proxy server
    let config = ProxyServerConfig {
        port: 8082, // Use a different port for testing
        ..ProxyServerConfig::default()
    };
    proxy_server.configure(config).await;
    
    // Start the proxy server in a separate task
    let proxy_server_clone = Arc::new(proxy_server);
    let proxy_handle = tokio::spawn(async move {
        proxy_server_clone.start().await.unwrap();
    });
    
    // Wait for the proxy server to start
    tokio::time::sleep(Duration::from_millis(100)).await;
    
    // Make a request to the proxy server
    // let client = reqwest::Client::new();
    // let response = client.get("http://localhost:8082/test").send().await?;
    
    // Verify the response
    // assert_eq!(response.status(), 200);
    
    // Stop the proxy server
    // proxy_handle.abort();
    
    Ok(())
}