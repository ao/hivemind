use anyhow::Result;
use hivemind::app::AppManager;
use hivemind::containerd_manager::{Container, ContainerStatus};
use hivemind::health_monitor::{HealthCheckConfig, HealthMonitor, ResourceThresholds, RestartPolicy};
use hivemind::membership::{Member, MembershipConfig, MembershipProtocol, NodeState};
use hivemind::network::{NetworkManager, NetworkPolicy, NetworkRule, NetworkSelector, PortRange, Protocol, NetworkPeer, PolicyAction};
use hivemind::node::{NodeManager, NodeResources};
use hivemind::scheduler::{BinPackingStrategy, ContainerScheduler};
use hivemind::service_discovery::ServiceDiscovery;
use hivemind::storage::StorageManager;
use ipnetwork::IpNetwork;
use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr};
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tempfile::TempDir;
use tokio::sync::Mutex;
use tokio::time::sleep;

// Helper function to create a test container
fn create_test_container(id: &str, name: &str, node_id: &str) -> Container {
    Container {
        id: id.to_string(),
        name: name.to_string(),
        image: "test-image:latest".to_string(),
        status: ContainerStatus::Running,
        node_id: node_id.to_string(),
        created_at: SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64,
        ports: Vec::new(),
        env_vars: Vec::new(),
        volumes: Vec::new(),
        service_domain: None,
    }
}

// Helper function to measure execution time
async fn measure_execution_time<F, Fut, T>(f: F) -> (T, Duration)
where
    F: FnOnce() -> Fut,
    Fut: std::future::Future<Output = T>,
{
    let start = Instant::now();
    let result = f().await;
    let duration = start.elapsed();
    (result, duration)
}

// Mock NodeManager for performance testing
struct MockNodeManager {
    nodes: Arc<Mutex<HashMap<String, (String, NodeResources)>>>,
    node_id: String,
}

impl MockNodeManager {
    fn new(node_id: &str) -> Self {
        let mut nodes = HashMap::new();
        
        // Add self node
        nodes.insert(
            node_id.to_string(),
            (
                "192.168.1.1".to_string(),
                NodeResources {
                    cpu_total: 8.0,
                    cpu_available: 8.0,
                    memory_total: 16 * 1024 * 1024 * 1024,
                    memory_available: 16 * 1024 * 1024 * 1024,
                    disk_total: 100 * 1024 * 1024 * 1024,
                    disk_available: 100 * 1024 * 1024 * 1024,
                },
            ),
        );
        
        Self {
            nodes: Arc::new(Mutex::new(nodes)),
            node_id: node_id.to_string(),
        }
    }
    
    async fn add_node(&self, id: &str, address: &str, resources: NodeResources) {
        let mut nodes = self.nodes.lock().await;
        nodes.insert(id.to_string(), (address.to_string(), resources));
    }
    
    async fn add_many_nodes(&self, count: usize, base_resources: &NodeResources) {
        let mut nodes = self.nodes.lock().await;
        for i in 0..count {
            let node_id = format!("node-{}", i + 2); // Start from node-2
            let address = format!("192.168.1.{}", i + 2);
            nodes.insert(node_id, (address, base_resources.clone()));
        }
    }
}

#[async_trait::async_trait]
impl NodeManager for MockNodeManager {
    fn get_node_id(&self) -> String {
        self.node_id.clone()
    }

    async fn list_nodes(&self) -> Result<Vec<String>> {
        let nodes = self.nodes.lock().await;
        Ok(nodes.keys().cloned().collect())
    }

    async fn get_node_details(&self) -> Result<Vec<(String, String, NodeResources)>> {
        let nodes = self.nodes.lock().await;
        let mut result = Vec::new();
        for (id, (address, resources)) in nodes.iter() {
            result.push((id.clone(), address.clone(), resources.clone()));
        }
        Ok(result)
    }
}

// Performance test for cluster scaling
#[tokio::test]
async fn test_cluster_scaling_performance() -> Result<()> {
    println!("Testing cluster scaling performance");
    
    // Create components
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(MockNodeManager::new("node-1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Test with different cluster sizes
    let cluster_sizes = vec![10, 50, 100, 200, 500];
    
    for size in cluster_sizes {
        println!("Testing with cluster size: {}", size);
        
        // Add nodes to the cluster
        let (_, duration) = measure_execution_time(|| async {
            let base_resources = NodeResources {
                cpu_total: 8.0,
                cpu_available: 8.0,
                memory_total: 16 * 1024 * 1024 * 1024,
                memory_available: 16 * 1024 * 1024 * 1024,
                disk_total: 100 * 1024 * 1024 * 1024,
                disk_available: 100 * 1024 * 1024 * 1024,
            };
            
            node_manager.add_many_nodes(size - 1, &base_resources).await;
            Ok::<_, anyhow::Error>(())
        }).await;
        
        println!("Adding {} nodes took {:?}", size, duration);
        
        // Create scheduler
        let scheduler = ContainerScheduler::new(app_manager.clone())
            .with_node_manager(node_manager.clone())
            .with_service_discovery(service_discovery.clone())
            .set_bin_packing_strategy(BinPackingStrategy::BestFit);
        
        // Deploy containers
        let container_count = size * 2; // 2 containers per node
        let (_, deploy_duration) = measure_execution_time(|| async {
            for i in 0..container_count {
                let container = create_test_container(
                    &format!("container-{}", i),
                    &format!("app-{}", i),
                    "pending", // Not assigned to a node yet
                );
                
                scheduler.schedule_container(&container).await?;
                Ok::<_, anyhow::Error>(())
            }
        }).await;
        
        println!("Scheduling {} containers across {} nodes took {:?}", container_count, size, deploy_duration);
        
        // Calculate containers per second
        let containers_per_second = container_count as f64 / deploy_duration.as_secs_f64();
        println!("Performance: {:.2} containers/second", containers_per_second);
        
        // Assert minimum performance requirements
        assert!(containers_per_second > 1.0, "Scheduling performance too low: {:.2} containers/second", containers_per_second);
    }
    
    Ok(())
}

// Performance test for service discovery under load
#[tokio::test]
async fn test_service_discovery_performance_under_load() -> Result<()> {
    println!("Testing service discovery performance under load");
    
    // Create service discovery
    let service_discovery = ServiceDiscovery::new();
    
    // Test with different service counts and concurrent requests
    let service_counts = vec![100, 500, 1000, 5000];
    let concurrent_requests = vec![10, 50, 100, 200];
    
    for &count in &service_counts {
        println!("Registering {} services", count);
        
        // Register services
        let (_, register_duration) = measure_execution_time(|| async {
            for i in 0..count {
                let service_config = hivemind::app::ServiceConfig {
                    name: format!("service-{}", i),
                    domain: format!("service-{}.local", i),
                    container_ids: vec![format!("container-{}", i)],
                    desired_replicas: 1,
                    current_replicas: 1,
                };
                
                service_discovery.register_service(
                    &service_config,
                    &format!("node-{}", i % 20 + 1),
                    &format!("192.168.1.{}", (i % 254) + 1),
                    8080,
                ).await?;
                
                Ok::<_, anyhow::Error>(())
            }
        }).await;
        
        println!("Registering {} services took {:?}", count, register_duration);
        
        // Test concurrent lookups
        for &concurrent in &concurrent_requests {
            println!("Testing with {} concurrent requests", concurrent);
            
            let (_, lookup_duration) = measure_execution_time(|| async {
                let mut handles = Vec::new();
                
                for _ in 0..concurrent {
                    let service_discovery_clone = service_discovery.clone();
                    let handle = tokio::spawn(async move {
                        for i in 0..100 {
                            let service_idx = i % count;
                            service_discovery_clone.get_service_url(&format!("service-{}", service_idx)).await;
                        }
                    });
                    handles.push(handle);
                }
                
                for handle in handles {
                    handle.await.unwrap();
                }
            }).await;
            
            let total_lookups = concurrent * 100;
            let lookups_per_second = total_lookups as f64 / lookup_duration.as_secs_f64();
            
            println!("Performance: {:.2} lookups/second with {} concurrent clients", lookups_per_second, concurrent);
            
            // Assert minimum performance requirements
            assert!(lookups_per_second > 100.0, "Service discovery performance too low: {:.2} lookups/second", lookups_per_second);
        }
    }
    
    Ok(())
}

// Performance test for network operations
#[tokio::test]
async fn test_network_operations_performance() -> Result<()> {
    println!("Testing network operations performance");
    
    // Create components
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let node_manager = Arc::new(MockNodeManager::new("node-1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Add a moderate number of nodes
    let base_resources = NodeResources {
        cpu_total: 8.0,
        cpu_available: 8.0,
        memory_total: 16 * 1024 * 1024 * 1024,
        memory_available: 16 * 1024 * 1024 * 1024,
        disk_total: 100 * 1024 * 1024 * 1024,
        disk_available: 100 * 1024 * 1024 * 1024,
    };
    
    node_manager.add_many_nodes(49, &base_resources).await;
    
    // Create network manager
    let network_manager = NetworkManager::new(
        node_manager.clone(),
        service_discovery.clone(),
        None,
    ).await?;
    
    // Test network policy application performance
    let policy_counts = vec![10, 50, 100, 500];
    
    for &count in &policy_counts {
        println!("Testing with {} network policies", count);
        
        let (_, policy_duration) = measure_execution_time(|| async {
            for i in 0..count {
                let policy = NetworkPolicy {
                    name: format!("policy-{}", i),
                    selector: NetworkSelector {
                        labels: HashMap::new(),
                    },
                    ingress_rules: vec![
                        NetworkRule {
                            ports: vec![PortRange {
                                protocol: Protocol::TCP,
                                port_min: 8080,
                                port_max: 8080,
                            }],
                            from: vec![NetworkPeer {
                                ip_block: None,
                                selector: Some(NetworkSelector {
                                    labels: {
                                        let mut labels = HashMap::new();
                                        labels.insert("service".to_string(), format!("service-{}", i));
                                        labels
                                    },
                                }),
                            }],
                            action: Some(PolicyAction::Allow),
                            log: false,
                            description: Some(format!("Test policy {}", i)),
                            id: None,
                        },
                    ],
                    egress_rules: vec![],
                    priority: 100,
                    namespace: None,
                    labels: HashMap::new(),
                    created_at: SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs(),
                    updated_at: SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs(),
                };
                
                network_manager.apply_network_policy(&policy).await?;
                Ok::<_, anyhow::Error>(())
            }
        }).await;
        
        println!("Applying {} network policies took {:?}", count, policy_duration);
        
        let policies_per_second = count as f64 / policy_duration.as_secs_f64();
        println!("Performance: {:.2} policies/second", policies_per_second);
        
        // Assert minimum performance requirements
        assert!(policies_per_second > 1.0, "Network policy application performance too low: {:.2} policies/second", policies_per_second);
        
        // Clean up policies
        for i in 0..count {
            network_manager.delete_network_policy(&format!("policy-{}", i)).await?;
        }
    }
    
    // Test container network setup performance
    let container_counts = vec![10, 50, 100, 500];
    
    for &count in &container_counts {
        println!("Testing network setup for {} containers", count);
        
        let (_, setup_duration) = measure_execution_time(|| async {
            network_manager.simulate_container_network_setup(count).await
        }).await;
        
        println!("Setting up {} container networks took {:?}", count, setup_duration);
        
        let containers_per_second = count as f64 / setup_duration.as_secs_f64();
        println!("Performance: {:.2} container networks/second", containers_per_second);
        
        // Assert minimum performance requirements
        assert!(containers_per_second > 1.0, "Container network setup performance too low: {:.2} containers/second", containers_per_second);
    }
    
    Ok(())
}

// Performance test for distributed state operations
#[tokio::test]
async fn test_distributed_state_performance() -> Result<()> {
    println!("Testing distributed state performance");
    
    // Create temporary directories for storage
    let temp_dir1 = TempDir::new().unwrap();
    let temp_dir2 = TempDir::new().unwrap();
    let temp_dir3 = TempDir::new().unwrap();
    
    // Create storage managers
    let storage1 = StorageManager::new(temp_dir1.path()).await?;
    let storage2 = StorageManager::new(temp_dir2.path()).await?;
    let storage3 = StorageManager::new(temp_dir3.path()).await?;
    
    // Create node managers
    let mut node1 = NodeManager::with_storage(storage1.clone()).await;
    let mut node2 = NodeManager::with_storage(storage2.clone()).await;
    let mut node3 = NodeManager::with_storage(storage3.clone()).await;
    
    // Initialize membership protocols
    node1.init_membership_protocol().await?;
    node2.init_membership_protocol().await?;
    node3.init_membership_protocol().await?;
    
    // Get node IDs and addresses
    let node1_id = node1.get_node_id();
    let node2_id = node2.get_node_id();
    let node3_id = node3.get_node_id();
    
    // Get node details
    let node1_details = node1.get_node_details().await?;
    let node2_details = node2.get_node_details().await?;
    let node3_details = node3.get_node_details().await?;
    
    let node1_addr = node1_details.iter().find(|(id, _, _)| id == &node1_id).unwrap().1.clone();
    
    // Node 2 and 3 join Node 1's cluster
    node2.join_cluster(&node1_addr).await?;
    node3.join_cluster(&node1_addr).await?;
    
    // Wait for cluster to stabilize
    sleep(Duration::from_millis(500)).await;
    
    // Verify all nodes see each other
    let node1_peers = node1.list_nodes().await?;
    assert!(node1_peers.contains(&node2_id));
    assert!(node1_peers.contains(&node3_id));
    
    // Test distributed state write performance
    let data_sizes = vec![100, 1000, 10000, 100000]; // Bytes
    
    for &size in &data_sizes {
        println!("Testing with data size: {} bytes", size);
        
        // Create test data
        let test_data = vec![0u8; size];
        
        // Test write performance
        let (_, write_duration) = measure_execution_time(|| async {
            for i in 0..100 {
                node1.store_distributed_state(&format!("key-{}", i), &test_data).await?;
                Ok::<_, anyhow::Error>(())
            }
        }).await;
        
        println!("Writing 100 entries of {} bytes took {:?}", size, write_duration);
        
        // Wait for state to propagate
        sleep(Duration::from_millis(500)).await;
        
        // Test read performance from different nodes
        let (_, read_duration1) = measure_execution_time(|| async {
            for i in 0..100 {
                node1.get_distributed_state(&format!("key-{}", i)).await;
            }
        }).await;
        
        let (_, read_duration2) = measure_execution_time(|| async {
            for i in 0..100 {
                node2.get_distributed_state(&format!("key-{}", i)).await;
            }
        }).await;
        
        let (_, read_duration3) = measure_execution_time(|| async {
            for i in 0..100 {
                node3.get_distributed_state(&format!("key-{}", i)).await;
            }
        }).await;
        
        println!("Reading 100 entries of {} bytes from node1 took {:?}", size, read_duration1);
        println!("Reading 100 entries of {} bytes from node2 took {:?}", size, read_duration2);
        println!("Reading 100 entries of {} bytes from node3 took {:?}", size, read_duration3);
        
        // Calculate operations per second
        let writes_per_second = 100.0 / write_duration.as_secs_f64();
        let reads1_per_second = 100.0 / read_duration1.as_secs_f64();
        let reads2_per_second = 100.0 / read_duration2.as_secs_f64();
        let reads3_per_second = 100.0 / read_duration3.as_secs_f64();
        
        println!("Write performance: {:.2} ops/second", writes_per_second);
        println!("Read performance (node1): {:.2} ops/second", reads1_per_second);
        println!("Read performance (node2): {:.2} ops/second", reads2_per_second);
        println!("Read performance (node3): {:.2} ops/second", reads3_per_second);
        
        // Assert minimum performance requirements
        assert!(writes_per_second > 1.0, "Write performance too low: {:.2} ops/second", writes_per_second);
        assert!(reads1_per_second > 1.0, "Read performance on node1 too low: {:.2} ops/second", reads1_per_second);
        assert!(reads2_per_second > 1.0, "Read performance on node2 too low: {:.2} ops/second", reads2_per_second);
        assert!(reads3_per_second > 1.0, "Read performance on node3 too low: {:.2} ops/second", reads3_per_second);
    }
    
    // Clean up
    node2.leave_cluster().await?;
    node3.leave_cluster().await?;
    
    Ok(())
}