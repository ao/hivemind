use anyhow::Result;
use hivemind::app::{AppManager, ServiceConfig};
use hivemind::containerd_manager::{Container, ContainerStatus};
use hivemind::node::{NodeManager, NodeResources};
use hivemind::scheduler::{
    BinPackingStrategy, ContainerScheduler, ResourceLimit, ResourceRequest, ResourceRequirements,
    Taint, TaintEffect, Toleration,
};
use hivemind::service_discovery::ServiceDiscovery;
use hivemind::storage::StorageManager;
use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use tempfile::TempDir;
use tokio::sync::Mutex;

// Mock NodeManager for testing scheduler
struct MockNodeManager {
    nodes: Arc<Mutex<HashMap<String, (String, NodeResources)>>>,
}

impl MockNodeManager {
    fn new() -> Self {
        Self {
            nodes: Arc::new(Mutex::new(HashMap::new())),
        }
    }

    async fn add_node(&self, id: &str, address: &str, resources: NodeResources) {
        let mut nodes = self.nodes.lock().await;
        nodes.insert(id.to_string(), (address.to_string(), resources));
    }
}

#[async_trait::async_trait]
impl NodeManager for MockNodeManager {
    fn get_node_id(&self) -> String {
        "test-node".to_string()
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

#[tokio::test]
async fn test_scheduler_initialization() {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await.unwrap();
    let app_manager = AppManager::with_storage(storage).await.unwrap();
    
    let scheduler = ContainerScheduler::new(app_manager.clone());
    assert_eq!(scheduler.get_scheduling_interval(), 30); // Default interval
}

#[tokio::test]
async fn test_bin_packing_strategies() {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await.unwrap();
    let app_manager = AppManager::with_storage(storage).await.unwrap();
    let node_manager = Arc::new(MockNodeManager::new());
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    // Create scheduler with BestFit strategy
    let mut scheduler = ContainerScheduler::new(app_manager.clone())
        .with_node_manager(node_manager.clone())
        .with_service_discovery(service_discovery.clone());
    
    scheduler.set_bin_packing_strategy(BinPackingStrategy::BestFit);
    assert_eq!(scheduler.get_bin_packing_strategy(), BinPackingStrategy::BestFit);
    
    // Change to WorstFit strategy
    scheduler.set_bin_packing_strategy(BinPackingStrategy::WorstFit);
    assert_eq!(scheduler.get_bin_packing_strategy(), BinPackingStrategy::WorstFit);
    
    // Change to FirstFit strategy
    scheduler.set_bin_packing_strategy(BinPackingStrategy::FirstFit);
    assert_eq!(scheduler.get_bin_packing_strategy(), BinPackingStrategy::FirstFit);
    
    // Change to Random strategy
    scheduler.set_bin_packing_strategy(BinPackingStrategy::Random);
    assert_eq!(scheduler.get_bin_packing_strategy(), BinPackingStrategy::Random);
}

#[tokio::test]
async fn test_node_taints_and_tolerations() {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await.unwrap();
    let app_manager = AppManager::with_storage(storage).await.unwrap();
    let node_manager = Arc::new(MockNodeManager::new());
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    let mut scheduler = ContainerScheduler::new(app_manager.clone())
        .with_node_manager(node_manager.clone())
        .with_service_discovery(service_discovery.clone());
    
    // Add a taint to a node
    scheduler.add_node_taint("node1", "key1", "value1", TaintEffect::NoSchedule);
    
    // Verify taint was added
    let taints = scheduler.get_node_taints("node1");
    assert_eq!(taints.len(), 1);
    assert_eq!(taints[0].key, "key1");
    assert_eq!(taints[0].value, "value1");
    assert_eq!(taints[0].effect, TaintEffect::NoSchedule);
    
    // Test toleration matching
    let toleration = Toleration {
        key: "key1".to_string(),
        value: Some("value1".to_string()),
        effect: Some(TaintEffect::NoSchedule),
    };
    
    assert!(scheduler.toleration_matches_taint(&toleration, &taints[0]));
    
    // Test toleration with wildcard value
    let wildcard_toleration = Toleration {
        key: "key1".to_string(),
        value: None, // Matches any value
        effect: Some(TaintEffect::NoSchedule),
    };
    
    assert!(scheduler.toleration_matches_taint(&wildcard_toleration, &taints[0]));
    
    // Test toleration with wildcard effect
    let wildcard_effect_toleration = Toleration {
        key: "key1".to_string(),
        value: Some("value1".to_string()),
        effect: None, // Matches any effect
    };
    
    assert!(scheduler.toleration_matches_taint(&wildcard_effect_toleration, &taints[0]));
}

#[tokio::test]
async fn test_resource_requirements() {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await.unwrap();
    let app_manager = AppManager::with_storage(storage).await.unwrap();
    let node_manager = Arc::new(MockNodeManager::new());
    
    // Add nodes with different resource capacities
    node_manager.add_node(
        "node1", 
        "192.168.1.1", 
        NodeResources {
            cpu_total: 8.0,
            cpu_available: 8.0,
            memory_total: 16 * 1024 * 1024 * 1024, // 16GB
            memory_available: 16 * 1024 * 1024 * 1024,
            disk_total: 100 * 1024 * 1024 * 1024, // 100GB
            disk_available: 100 * 1024 * 1024 * 1024,
        }
    ).await;
    
    node_manager.add_node(
        "node2", 
        "192.168.1.2", 
        NodeResources {
            cpu_total: 4.0,
            cpu_available: 4.0,
            memory_total: 8 * 1024 * 1024 * 1024, // 8GB
            memory_available: 8 * 1024 * 1024 * 1024,
            disk_total: 50 * 1024 * 1024 * 1024, // 50GB
            disk_available: 50 * 1024 * 1024 * 1024,
        }
    ).await;
    
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    let mut scheduler = ContainerScheduler::new(app_manager.clone())
        .with_node_manager(node_manager.clone())
        .with_service_discovery(service_discovery.clone());
    
    // Create resource requirements
    let requirements = ResourceRequirements {
        requests: ResourceRequest {
            cpu: 2.0,
            memory: 4 * 1024 * 1024 * 1024, // 4GB
        },
        limits: ResourceLimit {
            cpu: 4.0,
            memory: 8 * 1024 * 1024 * 1024, // 8GB
        },
    };
    
    // Test if nodes can fit the requirements
    assert!(scheduler.can_node_fit_requirements("node1", &requirements));
    assert!(scheduler.can_node_fit_requirements("node2", &requirements));
    
    // Reserve resources on node2
    scheduler.reserve_resources("node2", &requirements);
    
    // Node2 should now have less available resources
    assert_eq!(scheduler.get_reserved_cpu("node2"), 2.0);
    assert_eq!(scheduler.get_reserved_memory("node2"), 4 * 1024 * 1024 * 1024);
    
    // Create larger requirements that won't fit on node2 anymore
    let large_requirements = ResourceRequirements {
        requests: ResourceRequest {
            cpu: 3.0,
            memory: 5 * 1024 * 1024 * 1024, // 5GB
        },
        limits: ResourceLimit {
            cpu: 6.0,
            memory: 10 * 1024 * 1024 * 1024, // 10GB
        },
    };
    
    // Node1 should still fit the larger requirements
    assert!(scheduler.can_node_fit_requirements("node1", &large_requirements));
    
    // Node2 should not fit the larger requirements anymore
    assert!(!scheduler.can_node_fit_requirements("node2", &large_requirements));
}

#[tokio::test]
async fn test_service_affinity() {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await.unwrap();
    let app_manager = AppManager::with_storage(storage).await.unwrap();
    let node_manager = Arc::new(MockNodeManager::new());
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    let mut scheduler = ContainerScheduler::new(app_manager.clone())
        .with_node_manager(node_manager.clone())
        .with_service_discovery(service_discovery.clone());
    
    // Set service affinity
    scheduler.set_service_affinity("web-service", vec!["node1".to_string(), "node2".to_string()]);
    
    // Verify affinity was set
    let affinity = scheduler.get_service_affinity("web-service");
    assert_eq!(affinity.len(), 2);
    assert!(affinity.contains("node1"));
    assert!(affinity.contains("node2"));
    
    // Set service anti-affinity
    scheduler.set_service_antiaffinity("db-service", vec!["node3".to_string()]);
    
    // Verify anti-affinity was set
    let anti_affinity = scheduler.get_service_antiaffinity("db-service");
    assert_eq!(anti_affinity.len(), 1);
    assert!(anti_affinity.contains("node3"));
}

#[tokio::test]
async fn test_network_topology_awareness() {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await.unwrap();
    let app_manager = AppManager::with_storage(storage).await.unwrap();
    let node_manager = Arc::new(MockNodeManager::new());
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    let mut scheduler = ContainerScheduler::new(app_manager.clone())
        .with_node_manager(node_manager.clone())
        .with_service_discovery(service_discovery.clone());
    
    // Update network topology
    scheduler.update_network_topology("node1", "node2", 10.0);
    scheduler.update_network_topology("node1", "node3", 50.0);
    scheduler.update_network_topology("node2", "node3", 30.0);
    
    // Verify topology was updated
    let distance_1_2 = scheduler.get_network_distance("node1", "node2");
    let distance_1_3 = scheduler.get_network_distance("node1", "node3");
    let distance_2_3 = scheduler.get_network_distance("node2", "node3");
    
    assert_eq!(distance_1_2, 10.0);
    assert_eq!(distance_1_3, 50.0);
    assert_eq!(distance_2_3, 30.0);
    
    // Set node topology zones
    scheduler.set_node_topology_zone("node1", "zone1", "region1");
    scheduler.set_node_topology_zone("node2", "zone1", "region1");
    scheduler.set_node_topology_zone("node3", "zone2", "region1");
    
    // Verify zones were set (implemented as taints)
    let node1_taints = scheduler.get_node_taints("node1");
    let node3_taints = scheduler.get_node_taints("node3");
    
    let zone_taint = node1_taints.iter().find(|t| t.key == "topology.zone").unwrap();
    assert_eq!(zone_taint.value, "zone1");
    
    let zone_taint = node3_taints.iter().find(|t| t.key == "topology.zone").unwrap();
    assert_eq!(zone_taint.value, "zone2");
}

#[tokio::test]
async fn test_priority_based_scheduling() {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await.unwrap();
    let app_manager = AppManager::with_storage(storage).await.unwrap();
    let node_manager = Arc::new(MockNodeManager::new());
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    let mut scheduler = ContainerScheduler::new(app_manager.clone())
        .with_node_manager(node_manager.clone())
        .with_service_discovery(service_discovery.clone());
    
    // Define priority classes
    scheduler.define_priority_class("high", 100);
    scheduler.define_priority_class("medium", 50);
    scheduler.define_priority_class("low", 10);
    
    // Set container priorities
    scheduler.set_priority_class("container1", 100);
    scheduler.set_priority_class("container2", 50);
    scheduler.set_priority_class("container3", 10);
    
    // Verify priorities were set
    assert_eq!(scheduler.get_container_priority("container1"), 100);
    assert_eq!(scheduler.get_container_priority("container2"), 50);
    assert_eq!(scheduler.get_container_priority("container3"), 10);
    
    // Test priority comparison
    assert!(scheduler.has_higher_priority("container1", "container2"));
    assert!(scheduler.has_higher_priority("container2", "container3"));
    assert!(!scheduler.has_higher_priority("container3", "container1"));
}

#[tokio::test]
async fn test_resource_overcommit_ratios() {
    let temp_dir = TempDir::new().unwrap();
    let storage = StorageManager::new(temp_dir.path()).await.unwrap();
    let app_manager = AppManager::with_storage(storage).await.unwrap();
    let node_manager = Arc::new(MockNodeManager::new());
    
    // Add a node
    node_manager.add_node(
        "node1", 
        "192.168.1.1", 
        NodeResources {
            cpu_total: 4.0,
            cpu_available: 4.0,
            memory_total: 8 * 1024 * 1024 * 1024, // 8GB
            memory_available: 8 * 1024 * 1024 * 1024,
            disk_total: 100 * 1024 * 1024 * 1024, // 100GB
            disk_available: 100 * 1024 * 1024 * 1024,
        }
    ).await;
    
    let service_discovery = Arc::new(ServiceDiscovery::new());
    
    let mut scheduler = ContainerScheduler::new(app_manager.clone())
        .with_node_manager(node_manager.clone())
        .with_service_discovery(service_discovery.clone());
    
    // Set resource overcommit ratios
    scheduler.set_resource_overcommit_ratios(2.0, 1.5);
    
    // Create resource requirements that would normally exceed capacity
    let requirements = ResourceRequirements {
        requests: ResourceRequest {
            cpu: 6.0, // Exceeds 4.0 CPU but within 2.0x overcommit
            memory: 10 * 1024 * 1024 * 1024, // Exceeds 8GB but within 1.5x overcommit
        },
        limits: ResourceLimit {
            cpu: 8.0,
            memory: 12 * 1024 * 1024 * 1024,
        },
    };
    
    // With overcommit, the node should fit the requirements
    assert!(scheduler.can_node_fit_requirements_with_overcommit("node1", &requirements));
    
    // Create requirements that exceed even the overcommit ratios
    let excessive_requirements = ResourceRequirements {
        requests: ResourceRequest {
            cpu: 10.0, // Exceeds 4.0 CPU * 2.0 overcommit
            memory: 15 * 1024 * 1024 * 1024, // Exceeds 8GB * 1.5 overcommit
        },
        limits: ResourceLimit {
            cpu: 12.0,
            memory: 16 * 1024 * 1024 * 1024,
        },
    };
    
    // Even with overcommit, the node should not fit these requirements
    assert!(!scheduler.can_node_fit_requirements_with_overcommit("node1", &excessive_requirements));
}