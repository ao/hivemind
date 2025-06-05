use crate::app::{AppManager, ServiceConfig};
use crate::containerd_manager::{Container, ContainerStatus};
use crate::node::{NodeManager, NodeResources};
use crate::service_discovery::ServiceDiscovery;
use crate::network::NetworkManager;
use anyhow::Result;
use std::collections::{HashMap, HashSet};
use std::sync::Arc;

pub struct ContainerScheduler {
    app_manager: AppManager,
    node_manager: Option<Arc<NodeManager>>,
    service_discovery: Option<Arc<ServiceDiscovery>>,
    network_manager: Option<Arc<NetworkManager>>,
    scheduling_interval: u64, // seconds
    rebalance_threshold: f64, // percentage difference to trigger rebalancing
    service_affinity_map: HashMap<String, HashSet<String>>, // service -> nodes it prefers
    service_antiaffinity_map: HashMap<String, HashSet<String>>, // service -> nodes it avoids
    node_network_topology: HashMap<String, HashMap<String, f64>>, // node -> node -> network distance
    
    // Resource tracking
    node_cpu_reservations: HashMap<String, f64>, // node_id -> reserved CPU percentage
    node_memory_reservations: HashMap<String, u64>, // node_id -> reserved memory in bytes
    
    // Resource overcommitment policies
    cpu_overcommit_ratio: f64, // How much to overcommit CPU (e.g., 1.5 = 150%)
    memory_overcommit_ratio: f64, // How much to overcommit memory
    
    // Taints and tolerations
    node_taints: HashMap<String, Vec<Taint>>, // node_id -> list of taints
    
    // Priority-based scheduling
    priority_classes: HashMap<String, u32>, // priority class name -> priority value
    container_priorities: HashMap<String, u32>, // container_id -> priority value
    
    // Bin-packing strategy
    bin_packing_strategy: BinPackingStrategy,
}

struct NodeLoad {
    node_id: String,
    resources: NodeResources,
    container_count: usize,
    load_score: f64,
    reserved_cpu: f64,
    reserved_memory: u64,
    taints: Vec<Taint>,
}

#[derive(Clone, Debug, PartialEq)]
pub struct ResourceRequirements {
    pub requests: ResourceRequest,
    pub limits: ResourceLimit,
}

#[derive(Clone, Debug, PartialEq)]
pub struct ResourceRequest {
    pub cpu: f64,    // CPU cores requested (e.g., 0.5 = 500m)
    pub memory: u64, // Memory in bytes
}

#[derive(Clone, Debug, PartialEq)]
pub struct ResourceLimit {
    pub cpu: f64,    // CPU cores limit
    pub memory: u64, // Memory limit in bytes
}

#[derive(Clone, Debug, PartialEq)]
pub struct Taint {
    pub key: String,
    pub value: String,
    pub effect: TaintEffect,
}

#[derive(Clone, Debug, PartialEq)]
pub enum TaintEffect {
    NoSchedule,      // Prevents scheduling of new containers
    PreferNoSchedule, // Tries to avoid scheduling but not guaranteed
    NoExecute,       // Evicts containers that don't tolerate the taint
}

#[derive(Clone, Debug, PartialEq)]
pub struct Toleration {
    pub key: String,
    pub value: Option<String>, // None means tolerate any value
    pub effect: Option<TaintEffect>, // None means tolerate all effects
}

#[derive(Clone, Debug, PartialEq)]
pub enum BinPackingStrategy {
    BestFit,    // Pack containers onto as few nodes as possible
    WorstFit,   // Spread containers across nodes evenly
    FirstFit,   // Use the first node that fits
    Random,     // Randomly select a node that fits
}

impl ContainerScheduler {
    pub fn new(app_manager: AppManager) -> Self {
        Self {
            app_manager,
            node_manager: None,
            service_discovery: None,
            network_manager: None,
            scheduling_interval: 30,
            rebalance_threshold: 20.0, // 20% difference in load will trigger rebalancing
            service_affinity_map: HashMap::new(),
            service_antiaffinity_map: HashMap::new(),
            node_network_topology: HashMap::new(),
            node_cpu_reservations: HashMap::new(),
            node_memory_reservations: HashMap::new(),
            cpu_overcommit_ratio: 1.5, // Allow 50% CPU overcommitment by default
            memory_overcommit_ratio: 1.2, // Allow 20% memory overcommitment by default
            node_taints: HashMap::new(),
            priority_classes: HashMap::new(),
            container_priorities: HashMap::new(),
            bin_packing_strategy: BinPackingStrategy::BestFit,
        }
    }

    pub fn with_node_manager(mut self, node_manager: Arc<NodeManager>) -> Self {
        self.node_manager = Some(node_manager);
        self
    }

    pub fn with_service_discovery(mut self, service_discovery: Arc<ServiceDiscovery>) -> Self {
        self.service_discovery = Some(service_discovery);
        self
    }

    pub fn with_network_manager(mut self, network_manager: Arc<NetworkManager>) -> Self {
        self.network_manager = Some(network_manager);
        self
    }

    #[allow(dead_code)]
    pub fn set_scheduling_interval(mut self, interval_secs: u64) -> Self {
        self.scheduling_interval = interval_secs;
        self
    }

    // Configure service affinity - services that should be placed on the same nodes
    pub fn set_service_affinity(&mut self, service_name: &str, preferred_nodes: Vec<String>) -> &mut Self {
        let mut node_set = HashSet::new();
        for node in preferred_nodes {
            node_set.insert(node);
        }
        self.service_affinity_map.insert(service_name.to_string(), node_set);
        self
    }

    // Configure service anti-affinity - services that should be placed on different nodes
    pub fn set_service_antiaffinity(&mut self, service_name: &str, avoided_nodes: Vec<String>) -> &mut Self {
        let mut node_set = HashSet::new();
        for node in avoided_nodes {
            node_set.insert(node);
        }
        self.service_antiaffinity_map.insert(service_name.to_string(), node_set);
        self
    }

    // Update network topology information
    pub fn update_network_topology(&mut self, node_a: &str, node_b: &str, distance: f64) -> &mut Self {
        // Ensure both nodes have entries in the topology map
        self.node_network_topology.entry(node_a.to_string())
            .or_insert_with(HashMap::new)
            .insert(node_b.to_string(), distance);
        
        self.node_network_topology.entry(node_b.to_string())
            .or_insert_with(HashMap::new)
            .insert(node_a.to_string(), distance);
        
        self
    }
    
    // Define a topology zone for a node
    pub fn set_node_topology_zone(&mut self, node_id: &str, zone: &str, region: &str) -> &mut Self {
        // In a real implementation, we would store this information in a dedicated data structure
        // For now, we'll use taints to represent topology information
        self.add_node_taint(node_id, "topology.zone", zone, TaintEffect::PreferNoSchedule);
        self.add_node_taint(node_id, "topology.region", region, TaintEffect::PreferNoSchedule);
        
        self
    }
    
    // Add custom scheduling constraint
    pub fn add_custom_scheduling_constraint(&mut self, node_id: &str, key: &str, value: &str) -> &mut Self {
        // Custom constraints are implemented as taints with PreferNoSchedule effect
        self.add_node_taint(node_id, key, value, TaintEffect::PreferNoSchedule);
        
        self
    }

    pub async fn start(&self) -> Result<()> {
        println!("Starting container scheduler...");

        // Initialize network topology if we have a network manager
        if let Some(network_manager) = &self.network_manager {
            if let Err(e) = self.discover_network_topology(network_manager).await {
                eprintln!("Failed to discover network topology: {}", e);
            }
        }

        loop {
            if let Err(e) = self.schedule_containers().await {
                eprintln!("Scheduling error: {}", e);
            }

            // Periodically update network topology
            if let Some(network_manager) = &self.network_manager {
                if let Err(e) = self.discover_network_topology(network_manager).await {
                    eprintln!("Failed to update network topology: {}", e);
                }
            }

            tokio::time::sleep(tokio::time::Duration::from_secs(self.scheduling_interval)).await;
        }
    }

    // Discover and update network topology using the network manager
    async fn discover_network_topology(&self, _network_manager: &NetworkManager) -> Result<()> {
        println!("Discovering network topology...");
        
        // Skip if we don't have a node manager
        let node_manager = match &self.node_manager {
            Some(nm) => nm,
            None => {
                println!("No node manager available, skipping topology discovery");
                return Ok(());
            }
        };
        
        // Get all nodes
        let nodes = node_manager.list_nodes().await?;
        if nodes.len() <= 1 {
            println!("Not enough nodes for topology discovery");
            return Ok(());
        }
        
        // For each pair of nodes, measure network latency
        for i in 0..nodes.len() {
            for j in (i+1)..nodes.len() {
                let node_a = &nodes[i];
                let node_b = &nodes[j];
                
                // Calculate network distance between nodes
                // Since NetworkManager doesn't have a get_network_distance method,
                // we'll simulate it based on node IDs
                let hash_a = node_a.bytes().map(|b| b as u32).sum::<u32>();
                let hash_b = node_b.bytes().map(|b| b as u32).sum::<u32>();
                let xor_distance = (hash_a ^ hash_b) as f64;
                
                // Normalize to a reasonable range (1-100)
                let distance = 1.0 + (xor_distance % 100.0);
                
                println!("Network distance between {} and {}: {:.2}", node_a, node_b, distance);
                
                // Update topology map
                // Since node_network_topology is immutable (self is immutable), we can't modify it directly
                // In a real implementation, we would use interior mutability or a different approach
                // For now, we'll just log the distances
                println!("Would update network topology: {} <-> {} = {:.2}", node_a, node_b, distance);
                
                // In a real implementation with mutable access, we would do:
                // self.node_network_topology.entry(node_a.clone())
                //     .or_insert_with(HashMap::new)
                //     .insert(node_b.clone(), distance);
                // 
                // self.node_network_topology.entry(node_b.clone())
                //     .or_insert_with(HashMap::new)
                //     .insert(node_a.clone(), distance);
            }
        }
        
        Ok(())
    }

    async fn schedule_containers(&self) -> Result<()> {
        println!("Running container scheduling cycle...");

        // Skip scheduling if we don't have node manager
        let node_manager = match &self.node_manager {
            Some(nm) => nm,
            None => {
                println!("No node manager available, skipping scheduling");
                return Ok(());
            }
        };

        // Get all nodes and their resources
        let node_details = node_manager.get_node_details().await?;
        if node_details.is_empty() {
            println!("No nodes available for scheduling");
            return Ok(());
        }

        // Get all containers
        let containers = self.app_manager.get_container_details().await?;
        
        // Get all services
        let services = match self.app_manager.list_services().await {
            Ok(services) => services,
            Err(_) => Vec::new(),
        };
        
        // Calculate current load for each node
        let mut node_loads = self.calculate_node_loads(&node_details, &containers).await?;
        
        // Sort nodes by load (least loaded first)
        node_loads.sort_by(|a, b| a.load_score.partial_cmp(&b.load_score).unwrap());
        
        // Check if rebalancing is needed
        if node_loads.len() > 1 {
            let min_load = node_loads.first().unwrap().load_score;
            let max_load = node_loads.last().unwrap().load_score;
            
            let load_difference_percent = if min_load > 0.0 {
                ((max_load - min_load) / min_load) * 100.0
            } else {
                0.0
            };
            
            if load_difference_percent > self.rebalance_threshold {
                println!("Load imbalance detected: {:.2}%, rebalancing containers", load_difference_percent);
                self.rebalance_containers_with_constraints(&node_loads, &containers, &services).await?;
            } else {
                println!("Nodes are balanced (load difference: {:.2}%)", load_difference_percent);
            }
        }
        
        // Check for pending containers that need placement
        let mut pending_containers: Vec<&Container> = containers.iter()
            .filter(|c| c.status == ContainerStatus::Pending)
            .collect();
            
        if !pending_containers.is_empty() {
            println!("Found {} pending containers to schedule", pending_containers.len());
            
            // Sort pending containers by priority (highest first)
            pending_containers.sort_by(|a, b| {
                let priority_a = self.container_priorities.get(&a.id).cloned().unwrap_or(0);
                let priority_b = self.container_priorities.get(&b.id).cloned().unwrap_or(0);
                priority_b.cmp(&priority_a) // Reverse order for highest first
            });
            
            // Track which nodes have been updated during this scheduling cycle
            let mut updated_nodes = HashSet::new();
            
            for container in pending_containers {
                // Find the service this container belongs to
                let service = services.iter().find(|s| {
                    s.container_ids.contains(&container.id) ||
                    (container.service_domain.is_some() && container.service_domain == Some(s.domain.clone()))
                });
                
                // Get container priority
                let priority = self.container_priorities.get(&container.id).cloned().unwrap_or(0);
                
                // Get container resource requirements
                let resource_requirements = self.get_container_resource_requirements(container);
                
                println!("Scheduling container {} ({}) with priority {} - requires {:.2} CPU, {} MB memory",
                    container.id,
                    container.name,
                    priority,
                    resource_requirements.requests.cpu,
                    resource_requirements.requests.memory / (1024 * 1024));
                
                // Select the best node for this container
                if let Some(best_node) = self.select_best_node_for_container(
                    container,
                    service,
                    &node_loads,
                    &containers
                ).await? {
                    println!("Selected node {} for container {} ({})",
                        best_node, container.id, container.name);
                    
                    // Mark this node as updated
                    updated_nodes.insert(best_node.clone());
                        
                    // In a real implementation, we would deploy the container to the selected node
                    // For now, we'll just log the decision
                } else {
                    println!("No suitable node found for container {} ({})",
                        container.id, container.name);
                    
                    // If this is a high priority container, try preemptive scheduling
                    if priority > 0 {
                        println!("Attempting preemptive scheduling for priority {} container", priority);
                        
                        if let Some(preemption_target) = self.find_preemption_candidate(
                            container,
                            service,
                            &node_loads,
                            &containers
                        ).await? {
                            println!("Found preemption candidate on node {}", preemption_target);
                            
                            // Perform preemptive scheduling
                            self.perform_preemptive_scheduling(
                                container,
                                service,
                                &preemption_target,
                                &containers
                            ).await?;
                            
                            // Mark this node as updated
                            updated_nodes.insert(preemption_target);
                        } else {
                            println!("No preemption candidates found for container {} ({})",
                                container.id, container.name);
                        }
                    }
                }
            }
            
            // If any nodes were updated, recalculate their loads
            if !updated_nodes.is_empty() {
                println!("Recalculating loads for {} updated nodes", updated_nodes.len());
                
                // In a real implementation, we would recalculate the loads for the updated nodes
                // For now, we'll just log the nodes that would be updated
                for node_id in updated_nodes {
                    println!("Would recalculate load for node {}", node_id);
                }
            }
        }
        
        Ok(())
    }
    
    async fn calculate_node_loads(
        &self,
        node_details: &[(String, String, NodeResources)],
        containers: &[Container],
    ) -> Result<Vec<NodeLoad>> {
        let mut node_loads = Vec::new();
        
        for (node_id, _, resources) in node_details {
            // Count containers on this node
            let container_count = containers
                .iter()
                .filter(|c| &c.node_id == node_id && c.status == ContainerStatus::Running)
                .count();
            
            // Get reserved resources for this node
            let reserved_cpu = self.node_cpu_reservations.get(node_id).cloned().unwrap_or(0.0);
            let reserved_memory = self.node_memory_reservations.get(node_id).cloned().unwrap_or(0);
            
            // Calculate effective available resources (considering reservations)
            let effective_cpu_available = (resources.cpu_available - reserved_cpu).max(0.0);
            let effective_memory_available = (resources.memory_available as i64 - reserved_memory as i64).max(0) as u64;
            
            // Calculate load score (weighted combination of CPU, memory, and container count)
            // Lower score means more available resources
            let cpu_factor = (100.0 - effective_cpu_available) / 100.0;
            let memory_factor = 1.0 - (effective_memory_available as f64 / (1024.0 * 1024.0 * 1024.0));
            let container_factor = container_count as f64 * 0.1; // Each container adds 0.1 to the score
            
            // Get node taints
            let taints = self.node_taints.get(node_id).cloned().unwrap_or_default();
            
            // Adjust score based on bin packing strategy
            let strategy_factor = match self.bin_packing_strategy {
                BinPackingStrategy::BestFit => {
                    // For BestFit, we prefer nodes with higher utilization (lower available resources)
                    // This encourages packing containers onto fewer nodes
                    0.0 // No adjustment needed as higher utilization already gives higher score
                },
                BinPackingStrategy::WorstFit => {
                    // For WorstFit, we prefer nodes with lower utilization (higher available resources)
                    // This encourages spreading containers across nodes
                    -0.3 * (cpu_factor + memory_factor) // Reduce score for highly utilized nodes
                },
                BinPackingStrategy::FirstFit => {
                    // For FirstFit, we don't adjust the score based on utilization
                    0.0
                },
                BinPackingStrategy::Random => {
                    // For Random, add a small random factor
                    (std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos() % 100) as f64 * 0.001
                }
            };
            
            let load_score = (cpu_factor * 0.4) + (memory_factor * 0.4) + (container_factor * 0.2) + strategy_factor;
            
            node_loads.push(NodeLoad {
                node_id: node_id.clone(),
                resources: resources.clone(),
                container_count,
                load_score,
                reserved_cpu,
                reserved_memory,
                taints,
            });
            
            println!("Node {} load: {:.2} (CPU: {:.2}% available, {:.2}% reserved, Memory: {} MB available, {} MB reserved, Containers: {})",
                node_id,
                load_score,
                effective_cpu_available,
                reserved_cpu,
                effective_memory_available / (1024 * 1024),
                reserved_memory / (1024 * 1024),
                container_count);
        }
        
        Ok(node_loads)
    }
    
    // Select the best node for a container based on constraints and optimization criteria
    async fn select_best_node_for_container(
        &self,
        container: &Container,
        service: Option<&ServiceConfig>,
        node_loads: &[NodeLoad],
        containers: &[Container],
    ) -> Result<Option<String>> {
        if node_loads.is_empty() {
            return Ok(None);
        }
        
        // Get container resource requirements
        let resource_requirements = self.get_container_resource_requirements(container);
        
        // Get container tolerations
        let tolerations = self.get_container_tolerations(container);
        
        // Create a scoring system for each node
        let mut node_scores: HashMap<String, f64> = HashMap::new();
        
        // First, filter out nodes that don't meet resource requirements or have untolerated taints
        for node in node_loads {
            // Check if node has enough resources
            let has_enough_cpu = node.resources.cpu_available - node.reserved_cpu >= resource_requirements.requests.cpu;
            let has_enough_memory = node.resources.memory_available >= resource_requirements.requests.memory + node.reserved_memory;
            
            // Check if node has untolerated taints
            let has_untolerated_taints = self.has_untolerated_taints(&node.taints, &tolerations);
            
            // Skip node if it doesn't meet requirements
            if !has_enough_cpu || !has_enough_memory || has_untolerated_taints {
                continue;
            }
            
            // Start with a base score that's inverse to the load
            let base_score = 1.0 - node.load_score;
            node_scores.insert(node.node_id.clone(), base_score);
        }
        
        // If no nodes meet the basic requirements, return None
        if node_scores.is_empty() {
            println!("No nodes meet resource requirements for container {}", container.name);
            return Ok(None);
        }
        
        // Apply service affinity constraints if applicable
        if let Some(service_config) = service {
            let service_name = &service_config.name;
            
            // Get all node IDs for iteration
            let node_ids: Vec<String> = node_scores.keys().cloned().collect();
            
            // Check if this service has affinity constraints
            if let Some(preferred_nodes) = self.service_affinity_map.get(service_name) {
                for node_id in &node_ids {
                    if preferred_nodes.contains(node_id) {
                        // Boost score for preferred nodes
                        if let Some(score) = node_scores.get_mut(node_id) {
                            *score += 0.3; // Significant boost for affinity
                        }
                    }
                }
            }
            
            // Check if this service has anti-affinity constraints
            if let Some(avoided_nodes) = self.service_antiaffinity_map.get(service_name) {
                for node_id in &node_ids {
                    if avoided_nodes.contains(node_id) {
                        // Penalize score for avoided nodes
                        if let Some(score) = node_scores.get_mut(node_id) {
                            *score -= 0.5; // Significant penalty for anti-affinity
                        }
                    }
                }
            }
            
            // Consider service co-location for related services
            // Find nodes where other containers of the same service are running
            let service_nodes: HashSet<String> = containers.iter()
                .filter(|c| c.service_domain == Some(service_config.domain.clone()) &&
                           c.status == ContainerStatus::Running)
                .map(|c| c.node_id.clone())
                .collect();
                
            for node_id in &node_ids {
                if service_nodes.contains(node_id) {
                    // Boost score for nodes already running this service
                    if let Some(score) = node_scores.get_mut(node_id) {
                        *score += 0.2; // Moderate boost for service co-location
                    }
                }
            }
        }
        
        // Consider network topology if available
        if !self.node_network_topology.is_empty() {
            // Find nodes where related containers are running
            // For now, we'll consider containers with the same image to be related
            let related_nodes: HashMap<String, usize> = containers.iter()
                .filter(|c| c.image == container.image &&
                           c.id != container.id &&
                           c.status == ContainerStatus::Running)
                .fold(HashMap::new(), |mut acc, c| {
                    *acc.entry(c.node_id.clone()).or_insert(0) += 1;
                    acc
                });
                
            // Adjust scores based on network distance to related nodes
            for node_id in node_scores.keys().cloned().collect::<Vec<String>>() {
                if let Some(distances) = self.node_network_topology.get(&node_id) {
                    let mut network_score = 0.0;
                    
                    for (related_node, count) in &related_nodes {
                        if let Some(distance) = distances.get(related_node) {
                            // Lower distance = better score
                            // Normalize distance to a 0-1 scale (assuming max distance is 100)
                            let normalized_distance = (100.0 - distance.min(100.0)) / 100.0;
                            network_score += normalized_distance * (*count as f64 * 0.05);
                        }
                    }
                    
                    // Add network score to node score
                    if let Some(score) = node_scores.get_mut(&node_id) {
                        *score += network_score;
                    }
                }
            }
        }
        
        // Apply bin-packing strategy adjustments
        match self.bin_packing_strategy {
            BinPackingStrategy::BestFit => {
                // For BestFit, we prefer nodes with higher utilization
                // This is already handled in calculate_node_loads
            },
            BinPackingStrategy::WorstFit => {
                // For WorstFit, we prefer nodes with lower utilization
                // This is already handled in calculate_node_loads
            },
            BinPackingStrategy::FirstFit => {
                // For FirstFit, sort nodes by ID and pick the first one that fits
                // We'll implement this by boosting scores of nodes with lower IDs
                let mut sorted_nodes: Vec<&String> = node_scores.keys().collect();
                sorted_nodes.sort();
                
                for (idx, node_id) in sorted_nodes.iter().enumerate() {
                    if let Some(score) = node_scores.get_mut(*node_id) {
                        *score += 0.1 * (sorted_nodes.len() - idx) as f64 / sorted_nodes.len() as f64;
                    }
                }
            },
            BinPackingStrategy::Random => {
                // For Random, add a random factor to each score
                for (_, score) in node_scores.iter_mut() {
                    let random_factor = (std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos() % 1000) as f64 * 0.001;
                    *score += random_factor;
                }
            }
        }
        
        // Consider container priority
        let container_priority = self.container_priorities.get(&container.id).cloned().unwrap_or(0);
        if container_priority > 0 {
            // High priority containers get better nodes (less loaded)
            for (_, score) in node_scores.iter_mut() {
                *score += (container_priority as f64 * 0.01) * (1.0 - *score);
            }
        }
        
        // Find the node with the highest score
        let mut best_node = None;
        let mut best_score = -1.0;
        
        for (node_id, score) in node_scores {
            println!("Node {} score for container {}: {:.2}",
                node_id, container.name, score);
                
            if score > best_score {
                best_score = score;
                best_node = Some(node_id);
            }
        }
        
        // If we found a suitable node, reserve resources on it
        if let Some(node_id) = &best_node {
            self.reserve_resources(node_id, &resource_requirements);
        }
        
        Ok(best_node)
    }
    
    // Enhanced rebalancing that considers service constraints and priorities
    async fn rebalance_containers_with_constraints(
        &self,
        node_loads: &[NodeLoad],
        containers: &[Container],
        services: &[ServiceConfig],
    ) -> Result<()> {
        if node_loads.len() < 2 {
            return Ok(()); // Need at least 2 nodes to rebalance
        }
        
        // Find the most and least loaded nodes
        let least_loaded = &node_loads[0];
        let most_loaded = &node_loads[node_loads.len() - 1];
        
        println!("Rebalancing from node {} (load: {:.2}) to node {} (load: {:.2})",
            most_loaded.node_id, most_loaded.load_score,
            least_loaded.node_id, least_loaded.load_score);
        
        // Find containers on the most loaded node that could be moved
        let mut movable_containers: Vec<(&Container, Option<&ServiceConfig>, u32)> = Vec::new();
        
        for container in containers.iter().filter(|c|
            c.node_id == most_loaded.node_id && c.status == ContainerStatus::Running
        ) {
            // Find the service this container belongs to
            let service = services.iter().find(|s| {
                s.container_ids.contains(&container.id) ||
                (container.service_domain.is_some() && container.service_domain == Some(s.domain.clone()))
            });
            
            // Get container priority (default to 0 if not set)
            let priority = self.container_priorities.get(&container.id).cloned().unwrap_or(0);
            
            movable_containers.push((container, service, priority));
        }
        
        if movable_containers.is_empty() {
            println!("No movable containers found on the most loaded node");
            return Ok(());
        }
        
        // Sort containers by priority (lower priority first) and then by constraints
        movable_containers.sort_by(|(container_a, service_a, priority_a), (container_b, service_b, priority_b)| {
            // First compare by priority (lower priority containers are moved first)
            let priority_cmp = priority_a.cmp(priority_b);
            if priority_cmp != std::cmp::Ordering::Equal {
                return priority_cmp;
            }
            
            // If priorities are equal, compare by constraints
            let a_constraints = self.count_container_constraints(container_a, *service_a);
            let b_constraints = self.count_container_constraints(container_b, *service_b);
            a_constraints.cmp(&b_constraints)
        });
        
        // Try to move the container with the lowest priority and fewest constraints
        let (container_to_move, service, priority) = &movable_containers[0];
        
        println!("Selected container {} ({}) with priority {} for rebalancing",
            container_to_move.id, container_to_move.name, priority);
        
        // Check if the least loaded node is suitable for this container
        let target_node = match self.select_best_node_for_container(
            container_to_move,
            *service,
            &node_loads[..node_loads.len()-1], // Exclude the most loaded node
            containers
        ).await? {
            Some(node) => node,
            None => {
                println!("No suitable target node found for container {} ({})",
                    container_to_move.id, container_to_move.name);
                
                // If this is a low priority container and we couldn't find a suitable node,
                // we might need to preempt a lower priority container on another node
                if *priority > 0 {
                    println!("Attempting preemptive scheduling for priority {} container", priority);
                    if let Some(preemption_target) = self.find_preemption_candidate(
                        container_to_move,
                        *service,
                        &node_loads[..node_loads.len()-1],
                        containers
                    ).await? {
                        println!("Found preemption candidate on node {}", preemption_target);
                        return self.perform_preemptive_scheduling(
                            container_to_move,
                            *service,
                            &preemption_target,
                            containers
                        ).await;
                    }
                }
                
                return Ok(());
            }
        };
        
        println!("Would move container {} ({}) from node {} to node {}",
            container_to_move.id, container_to_move.name,
            most_loaded.node_id, target_node);
        
        // In a real implementation, we would:
        // 1. Stop the container on the source node
        // 2. Start an identical container on the target node
        // 3. Update container records and service discovery
        
        Ok(())
    }
    
    // Find a candidate node for preemptive scheduling
    async fn find_preemption_candidate(
        &self,
        container: &Container,
        service: Option<&ServiceConfig>,
        node_loads: &[NodeLoad],
        containers: &[Container],
    ) -> Result<Option<String>> {
        // Get container priority
        let container_priority = self.container_priorities.get(&container.id).cloned().unwrap_or(0);
        
        // If container has no priority, it can't preempt others
        if container_priority == 0 {
            return Ok(None);
        }
        
        // Get container resource requirements
        let resource_requirements = self.get_container_resource_requirements(container);
        
        // For each node, check if we can preempt lower priority containers to make room
        for node in node_loads {
            // Get containers on this node
            let node_containers: Vec<&Container> = containers.iter()
                .filter(|c| c.node_id == node.node_id && c.status == ContainerStatus::Running)
                .collect();
            
            // Get containers with lower priority than our container
            let mut lower_priority_containers: Vec<(&Container, u32)> = node_containers.iter()
                .filter_map(|c| {
                    let priority = self.container_priorities.get(&c.id).cloned().unwrap_or(0);
                    if priority < container_priority {
                        Some((*c, priority))
                    } else {
                        None
                    }
                })
                .collect();
            
            // Sort by priority (lowest first)
            lower_priority_containers.sort_by(|(_, priority_a), (_, priority_b)| {
                priority_a.cmp(priority_b)
            });
            
            // Calculate total resources that could be freed
            let mut freeable_cpu = 0.0;
            let mut freeable_memory = 0;
            
            for (preemptible_container, _) in &lower_priority_containers {
                let container_resources = self.get_container_resource_requirements(preemptible_container);
                freeable_cpu += container_resources.requests.cpu;
                freeable_memory += container_resources.requests.memory;
                
                // If we've freed enough resources, this node is a candidate
                if freeable_cpu >= resource_requirements.requests.cpu &&
                   freeable_memory >= resource_requirements.requests.memory {
                    return Ok(Some(node.node_id.clone()));
                }
            }
        }
        
        Ok(None)
    }
    
    // Perform preemptive scheduling
    async fn perform_preemptive_scheduling(
        &self,
        container: &Container,
        service: Option<&ServiceConfig>,
        target_node: &str,
        containers: &[Container],
    ) -> Result<()> {
        // Get container priority
        let container_priority = self.container_priorities.get(&container.id).cloned().unwrap_or(0);
        
        // Get container resource requirements
        let resource_requirements = self.get_container_resource_requirements(container);
        
        // Get containers on the target node
        let node_containers: Vec<&Container> = containers.iter()
            .filter(|c| c.node_id == target_node && c.status == ContainerStatus::Running)
            .collect();
        
        // Get containers with lower priority than our container
        let mut lower_priority_containers: Vec<(&Container, u32)> = node_containers.iter()
            .filter_map(|c| {
                let priority = self.container_priorities.get(&c.id).cloned().unwrap_or(0);
                if priority < container_priority {
                    Some((*c, priority))
                } else {
                    None
                }
            })
            .collect();
        
        // Sort by priority (lowest first)
        lower_priority_containers.sort_by(|(_, priority_a), (_, priority_b)| {
            priority_a.cmp(priority_b)
        });
        
        // Calculate which containers need to be evicted
        let mut containers_to_evict = Vec::new();
        let mut freed_cpu = 0.0;
        let mut freed_memory = 0;
        
        for (preemptible_container, priority) in lower_priority_containers {
            let container_resources = self.get_container_resource_requirements(preemptible_container);
            freed_cpu += container_resources.requests.cpu;
            freed_memory += container_resources.requests.memory;
            
            containers_to_evict.push((preemptible_container, priority));
            
            // If we've freed enough resources, we can stop
            if freed_cpu >= resource_requirements.requests.cpu &&
               freed_memory >= resource_requirements.requests.memory {
                break;
            }
        }
        
        // Log the preemption plan
        println!("Preemption plan for container {} (priority {}) on node {}:",
            container.id, container_priority, target_node);
            
        for (evicted_container, priority) in &containers_to_evict {
            println!("  Would evict container {} (priority {}) to free {:.2} CPU and {} MB memory",
                evicted_container.id, priority,
                self.get_container_resource_requirements(evicted_container).requests.cpu,
                self.get_container_resource_requirements(evicted_container).requests.memory / (1024 * 1024));
        }
        
        // In a real implementation, we would:
        // 1. Gracefully evict the lower priority containers
        // 2. Deploy the higher priority container to the node
        // 3. Update container records and service discovery
        
        println!("Would move container {} ({}) to node {} after preemption",
            container.id, container.name, target_node);
            
        Ok(())
    }
    
    // Count the number of constraints for a container
    fn count_container_constraints(&self, container: &Container, service: Option<&ServiceConfig>) -> usize {
        let mut constraints = 0;
        
        // Service affinity constraints
        if let Some(service_config) = service {
            if self.service_affinity_map.contains_key(&service_config.name) {
                constraints += 1;
            }
            
            // Service anti-affinity constraints
            if self.service_antiaffinity_map.contains_key(&service_config.name) {
                constraints += 1;
            }
            
            // Service co-location (other containers of same service)
            constraints += 1;
        }
        
        // Network topology constraints
        if !self.node_network_topology.is_empty() {
            constraints += 1;
        }
        
        // Resource constraints
        let resource_requirements = self.get_container_resource_requirements(container);
        if resource_requirements.requests.cpu > 0.0 || resource_requirements.requests.memory > 0 {
            constraints += 1;
        }
        
        // Toleration constraints
        let tolerations = self.get_container_tolerations(container);
        if !tolerations.is_empty() {
            constraints += 1;
        }
        
        // Priority constraints
        if self.container_priorities.contains_key(&container.id) {
            constraints += 1;
        }
        
        constraints
    }
    
    // Get resource requirements for a container
    fn get_container_resource_requirements(&self, container: &Container) -> ResourceRequirements {
        // In a real implementation, this would parse resource requirements from container metadata
        // For now, we'll use some heuristics based on the container image
        
        // Default values
        let mut cpu_request = 0.1; // 100m CPU
        let mut memory_request = 128 * 1024 * 1024; // 128MB
        let mut cpu_limit = 0.5; // 500m CPU
        let mut memory_limit = 512 * 1024 * 1024; // 512MB
        
        // Adjust based on image type
        if container.image.contains("nginx") {
            cpu_request = 0.2;
            memory_request = 256 * 1024 * 1024;
            cpu_limit = 1.0;
            memory_limit = 1024 * 1024 * 1024;
        } else if container.image.contains("postgres") || container.image.contains("mysql") {
            cpu_request = 0.5;
            memory_request = 512 * 1024 * 1024;
            cpu_limit = 2.0;
            memory_limit = 2 * 1024 * 1024 * 1024;
        } else if container.image.contains("redis") {
            cpu_request = 0.3;
            memory_request = 256 * 1024 * 1024;
            cpu_limit = 1.0;
            memory_limit = 1024 * 1024 * 1024;
        }
        
        ResourceRequirements {
            requests: ResourceRequest {
                cpu: cpu_request,
                memory: memory_request,
            },
            limits: ResourceLimit {
                cpu: cpu_limit,
                memory: memory_limit,
            },
        }
    }
    
    // Get tolerations for a container
    fn get_container_tolerations(&self, _container: &Container) -> Vec<Toleration> {
        // In a real implementation, this would parse tolerations from container metadata
        // For now, we'll return an empty list
        Vec::new()
    }
    
    // Check if a node has untolerated taints
    fn has_untolerated_taints(&self, taints: &[Taint], tolerations: &[Toleration]) -> bool {
        for taint in taints {
            let mut tolerated = false;
            
            for toleration in tolerations {
                // Check if the toleration matches the taint
                let key_match = toleration.key == taint.key;
                let value_match = toleration.value.is_none() || toleration.value.as_ref() == Some(&taint.value);
                let effect_match = toleration.effect.is_none() || toleration.effect.as_ref() == Some(&taint.effect);
                
                if key_match && value_match && effect_match {
                    tolerated = true;
                    break;
                }
            }
            
            // If this taint is not tolerated and has NoSchedule effect, the node is untolerated
            if !tolerated && taint.effect == TaintEffect::NoSchedule {
                return true;
            }
        }
        
        false
    }
    
    // Reserve resources on a node for a container
    fn reserve_resources(&self, node_id: &str, requirements: &ResourceRequirements) {
        // In a real implementation, this would be a mutable method
        // For now, we'll just log the reservation
        println!("Would reserve {:.2} CPU cores and {} MB memory on node {}",
            requirements.requests.cpu,
            requirements.requests.memory / (1024 * 1024),
            node_id);
            
        // In a real implementation with mutable access, we would do:
        // let cpu_reservation = self.node_cpu_reservations.entry(node_id.to_string()).or_insert(0.0);
        // *cpu_reservation += requirements.requests.cpu;
        //
        // let memory_reservation = self.node_memory_reservations.entry(node_id.to_string()).or_insert(0);
        // *memory_reservation += requirements.requests.memory;
    }
    
    // Set priority class for a container
    pub fn set_priority_class(&mut self, container_id: &str, priority: u32) -> &mut Self {
        self.container_priorities.insert(container_id.to_string(), priority);
        self
    }
    
    // Define a priority class
    pub fn define_priority_class(&mut self, name: &str, priority: u32) -> &mut Self {
        self.priority_classes.insert(name.to_string(), priority);
        self
    }
    
    // Add a taint to a node
    pub fn add_node_taint(&mut self, node_id: &str, key: &str, value: &str, effect: TaintEffect) -> &mut Self {
        let taint = Taint {
            key: key.to_string(),
            value: value.to_string(),
            effect,
        };
        
        self.node_taints.entry(node_id.to_string())
            .or_insert_with(Vec::new)
            .push(taint);
            
        self
    }
    
    // Set bin packing strategy
    pub fn set_bin_packing_strategy(&mut self, strategy: BinPackingStrategy) -> &mut Self {
        self.bin_packing_strategy = strategy;
        self
    }
    
    // Set resource overcommit ratios
    pub fn set_resource_overcommit_ratios(&mut self, cpu_ratio: f64, memory_ratio: f64) -> &mut Self {
        self.cpu_overcommit_ratio = cpu_ratio;
        self.memory_overcommit_ratio = memory_ratio;
        self
    }
}
