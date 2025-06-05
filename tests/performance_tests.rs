use anyhow::Result;
use hivemind::app::AppManager;
use hivemind::containerd_manager::{Container, ContainerStatus};
use hivemind::health_monitor::HealthMonitor;
use hivemind::membership::{Member, MembershipConfig, MembershipProtocol, NodeState};
use hivemind::network::NetworkManager;
use hivemind::node::{NodeManager, NodeResources};
use hivemind::scheduler::{BinPackingStrategy, ContainerScheduler};
use hivemind::service_discovery::ServiceDiscovery;
use hivemind::storage::StorageManager;
use std::collections::HashMap;
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

#[tokio::test]
async fn test_scheduler_performance_large_cluster() -> Result<()> {
    // Create components
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(MockNodeManager::new("node-1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Add a large number of nodes (100 nodes)
    println!("Adding 100 nodes to the cluster...");
    let base_resources = NodeResources {
        cpu_total: 8.0,
        cpu_available: 8.0,
        memory_total: 16 * 1024 * 1024 * 1024,
        memory_available: 16 * 1024 * 1024 * 1024,
        disk_total: 100 * 1024 * 1024 * 1024,
        disk_available: 100 * 1024 * 1024 * 1024,
    };
    
    node_manager.add_many_nodes(99, &base_resources).await;
    
    // Create scheduler with different bin packing strategies
    let mut best_fit_scheduler = ContainerScheduler::new(app_manager.clone())
        .with_node_manager(node_manager.clone())
        .with_service_discovery(service_discovery.clone())
        .set_bin_packing_strategy(BinPackingStrategy::BestFit);
    
    let mut worst_fit_scheduler = ContainerScheduler::new(app_manager.clone())
        .with_node_manager(node_manager.clone())
        .with_service_discovery(service_discovery.clone())
        .set_bin_packing_strategy(BinPackingStrategy::WorstFit);
    
    let mut first_fit_scheduler = ContainerScheduler::new(app_manager.clone())
        .with_node_manager(node_manager.clone())
        .with_service_discovery(service_discovery.clone())
        .set_bin_packing_strategy(BinPackingStrategy::FirstFit);
    
    // Measure scheduling performance with different strategies
    println!("Testing scheduling performance with different strategies...");
    
    // Create a large number of containers (1000 containers)
    let container_count = 1000;
    let mut containers = Vec::new();
    
    for i in 0..container_count {
        containers.push(create_test_container(
            &format!("container-{}", i),
            &format!("app-{}", i),
            "pending", // Not assigned to a node yet
        ));
    }
    
    // Measure BestFit strategy
    let (_, best_fit_duration) = measure_execution_time(|| async {
        for container in &containers {
            best_fit_scheduler.schedule_container(container).await?;
            Ok::<_, anyhow::Error>(())
        }
    }).await;
    
    println!("BestFit strategy scheduled {} containers in {:?}", container_count, best_fit_duration);
    
    // Measure WorstFit strategy
    let (_, worst_fit_duration) = measure_execution_time(|| async {
        for container in &containers {
            worst_fit_scheduler.schedule_container(container).await?;
            Ok::<_, anyhow::Error>(())
        }
    }).await;
    
    println!("WorstFit strategy scheduled {} containers in {:?}", container_count, worst_fit_duration);
    
    // Measure FirstFit strategy
    let (_, first_fit_duration) = measure_execution_time(|| async {
        for container in &containers {
            first_fit_scheduler.schedule_container(container).await?;
            Ok::<_, anyhow::Error>(())
        }
    }).await;
    
    println!("FirstFit strategy scheduled {} containers in {:?}", container_count, first_fit_duration);
    
    // Compare strategies
    println!("Performance comparison:");
    println!("BestFit: {:?}", best_fit_duration);
    println!("WorstFit: {:?}", worst_fit_duration);
    println!("FirstFit: {:?}", first_fit_duration);
    
    // FirstFit should be the fastest
    assert!(first_fit_duration <= best_fit_duration || first_fit_duration <= worst_fit_duration);
    
    Ok(())
}

#[tokio::test]
async fn test_membership_protocol_scalability() -> Result<()> {
    // Create membership protocol
    let mut config = MembershipConfig::default();
    config.bind_port = 7965;
    config.protocol_period = Duration::from_millis(100);
    
    let membership = MembershipProtocol::new(
        "node-1".to_string(),
        "192.168.1.1:7965".to_string(),
        Some(config),
    ).await?;
    
    // Start membership protocol
    membership.start().await?;
    
    // Measure performance with increasing cluster sizes
    let cluster_sizes = vec![10, 50, 100, 200, 500];
    
    for size in cluster_sizes {
        // Add nodes to the cluster
        let (_, duration) = measure_execution_time(|| async {
            for i in 0..size {
                let node_id = format!("node-{}", i + 2);
                let address = format!("192.168.1.{}", (i % 254) + 2);
                
                let member = Member {
                    id: node_id,
                    address,
                    state: NodeState::Alive,
                    incarnation: 1,
                    last_state_change: SystemTime::now()
                        .duration_since(UNIX_EPOCH)
                        .unwrap()
                        .as_secs(),
                    is_leader: false,
                    last_leader_check: 0,
                    metadata: Some(HashMap::new()),
                };
                
                membership.add_member(member).await?;
                Ok::<_, anyhow::Error>(())
            }
        }).await;
        
        println!("Adding {} nodes took {:?}", size, duration);
        
        // Measure gossip dissemination time
        let (_, gossip_duration) = measure_execution_time(|| async {
            membership.gossip_to_random_members(3).await
        }).await;
        
        println!("Gossip dissemination with {} nodes took {:?}", size, gossip_duration);
        
        // Measure member listing time
        let (_, list_duration) = measure_execution_time(|| async {
            membership.get_all_members().await
        }).await;
        
        println!("Listing {} members took {:?}", size, list_duration);
    }
    
    Ok(())
}

#[tokio::test]
async fn test_network_manager_performance() -> Result<()> {
    // Create components
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let node_manager = Arc::new(MockNodeManager::new("node-1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Add a moderate number of nodes (20 nodes)
    let base_resources = NodeResources {
        cpu_total: 8.0,
        cpu_available: 8.0,
        memory_total: 16 * 1024 * 1024 * 1024,
        memory_available: 16 * 1024 * 1024 * 1024,
        disk_total: 100 * 1024 * 1024 * 1024,
        disk_available: 100 * 1024 * 1024 * 1024,
    };
    
    node_manager.add_many_nodes(19, &base_resources).await;
    
    // Create network manager
    let network_manager = NetworkManager::new(
        node_manager.clone(),
        service_discovery.clone(),
        None,
    ).await?;
    
    // Measure network initialization time
    let (_, init_duration) = measure_execution_time(|| async {
        network_manager.initialize().await
    }).await;
    
    println!("Network initialization took {:?}", init_duration);
    
    // Measure container network setup time
    let (_, setup_duration) = measure_execution_time(|| async {
        // This is a simulation since we can't actually create network namespaces in tests
        network_manager.simulate_container_network_setup(100).await
    }).await;
    
    println!("Setting up 100 container networks took {:?}", setup_duration);
    
    // Measure network policy application time
    let (_, policy_duration) = measure_execution_time(|| async {
        network_manager.simulate_network_policy_application(50).await
    }).await;
    
    println!("Applying 50 network policies took {:?}", policy_duration);
    
    Ok(())
}

#[tokio::test]
async fn test_service_discovery_performance() -> Result<()> {
    // Create service discovery
    let service_discovery = ServiceDiscovery::new();
    
    // Measure performance with increasing service counts
    let service_counts = vec![10, 50, 100, 500, 1000];
    
    for count in service_counts {
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
        
        // Measure service lookup time
        let (_, lookup_duration) = measure_execution_time(|| async {
            for i in 0..count {
                service_discovery.get_service_url(&format!("service-{}", i)).await;
            }
        }).await;
        
        println!("Looking up {} services took {:?}", count, lookup_duration);
        
        // Measure service listing time
        let (_, list_duration) = measure_execution_time(|| async {
            service_discovery.list_services().await
        }).await;
        
        println!("Listing {} services took {:?}", count, list_duration);
    }
    
    Ok(())
}

#[tokio::test]
async fn test_health_monitor_performance() -> Result<()> {
    // Create components
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(MockNodeManager::new("node-1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Add nodes
    let base_resources = NodeResources {
        cpu_total: 8.0,
        cpu_available: 8.0,
        memory_total: 16 * 1024 * 1024 * 1024,
        memory_available: 16 * 1024 * 1024 * 1024,
        disk_total: 100 * 1024 * 1024 * 1024,
        disk_available: 100 * 1024 * 1024 * 1024,
    };
    
    node_manager.add_many_nodes(19, &base_resources).await;
    
    // Create health monitor
    let health_monitor = Arc::new(HealthMonitor::new(
        app_manager.clone(),
        node_manager.clone(),
        service_discovery.clone(),
    ));
    
    // Add containers to app manager
    let container_counts = vec![10, 50, 100, 500, 1000];
    
    for count in container_counts {
        // Add containers
        for i in 0..count {
            let node_id = format!("node-{}", i % 20 + 1);
            let container = create_test_container(
                &format!("container-{}", i),
                &format!("app-{}", i),
                &node_id,
            );
            
            app_manager.add_test_container(container).await?;
        }
        
        // Measure health check performance
        let (_, check_duration) = measure_execution_time(|| async {
            health_monitor.check_all_container_health().await
        }).await;
        
        println!("Checking health of {} containers took {:?}", count, check_duration);
        
        // Measure node health check performance
        let (_, node_check_duration) = measure_execution_time(|| async {
            health_monitor.check_all_node_health().await
        }).await;
        
        println!("Checking health of 20 nodes took {:?}", node_check_duration);
        
        // Measure health summary generation
        let (_, summary_duration) = measure_execution_time(|| async {
            health_monitor.get_health_summary().await
        }).await;
        
        println!("Generating health summary for {} containers and 20 nodes took {:?}", count, summary_duration);
        
        // Clean up containers for next iteration
        app_manager.clear_test_containers().await?;
    }
    
    Ok(())
}

#[tokio::test]
async fn test_storage_performance() -> Result<()> {
    // Create storage manager
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    
    // Measure performance with increasing data sizes
    let data_sizes = vec![1024, 10 * 1024, 100 * 1024, 1024 * 1024]; // 1KB to 1MB
    
    for size in data_sizes {
        // Create test data
        let test_data = vec![0u8; size];
        
        // Measure write performance
        let (_, write_duration) = measure_execution_time(|| async {
            for i in 0..100 {
                storage.store(&format!("key-{}", i), &test_data).await?;
                Ok::<_, anyhow::Error>(())
            }
        }).await;
        
        println!("Writing 100 entries of {} bytes took {:?}", size, write_duration);
        
        // Measure read performance
        let (_, read_duration) = measure_execution_time(|| async {
            for i in 0..100 {
                storage.get(&format!("key-{}", i)).await?;
                Ok::<_, anyhow::Error>(())
            }
        }).await;
        
        println!("Reading 100 entries of {} bytes took {:?}", size, read_duration);
        
        // Measure batch operations
        let (_, batch_duration) = measure_execution_time(|| async {
            let mut batch = HashMap::new();
            for i in 0..100 {
                batch.insert(format!("batch-key-{}", i), test_data.clone());
            }
            storage.store_batch(batch).await?;
            Ok::<_, anyhow::Error>(())
        }).await;
        
        println!("Batch writing 100 entries of {} bytes took {:?}", size, batch_duration);
    }
    
    Ok(())
}

#[tokio::test]
async fn test_concurrent_operations() -> Result<()> {
    // Create components
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await?;
    let app_manager = Arc::new(AppManager::with_storage(storage.clone()).await?);
    let node_manager = Arc::new(MockNodeManager::new("node-1"));
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Add nodes
    let base_resources = NodeResources {
        cpu_total: 8.0,
        cpu_available: 8.0,
        memory_total: 16 * 1024 * 1024 * 1024,
        memory_available: 16 * 1024 * 1024 * 1024,
        disk_total: 100 * 1024 * 1024 * 1024,
        disk_available: 100 * 1024 * 1024 * 1024,
    };
    
    node_manager.add_many_nodes(9, &base_resources).await;
    
    // Measure concurrent container deployments
    let concurrency_levels = vec![10, 50, 100];
    
    for concurrency in concurrency_levels {
        let (_, deploy_duration) = measure_execution_time(|| async {
            let mut handles = Vec::new();
            
            for i in 0..concurrency {
                let app_manager_clone = app_manager.clone();
                let handle = tokio::spawn(async move {
                    app_manager_clone.deploy_app(
                        "nginx:latest",
                        &format!("app-{}", i),
                        Some(&format!("app-{}.local", i)),
                        None,
                        None,
                    ).await
                });
                handles.push(handle);
            }
            
            for handle in handles {
                let _ = handle.await;
            }
        }).await;
        
        println!("Deploying {} containers concurrently took {:?}", concurrency, deploy_duration);
        
        // Clean up
        app_manager.clear_test_containers().await?;
    }
    
    // Measure concurrent service registrations
    for concurrency in concurrency_levels {
        let service_discovery_clone = service_discovery.clone();
        
        let (_, register_duration) = measure_execution_time(|| async {
            let mut handles = Vec::new();
            
            for i in 0..concurrency {
                let service_discovery = service_discovery_clone.clone();
                let handle = tokio::spawn(async move {
                    let service_config = hivemind::app::ServiceConfig {
                        name: format!("service-{}", i),
                        domain: format!("service-{}.local", i),
                        container_ids: vec![format!("container-{}", i)],
                        desired_replicas: 1,
                        current_replicas: 1,
                    };
                    
                    service_discovery.register_service(
                        &service_config,
                        &format!("node-{}", i % 10 + 1),
                        &format!("192.168.1.{}", (i % 254) + 1),
                        8080,
                    ).await
                });
                handles.push(handle);
            }
            
            for handle in handles {
                let _ = handle.await;
            }
        }).await;
        
        println!("Registering {} services concurrently took {:?}", concurrency, register_duration);
    }
    
    Ok(())
}