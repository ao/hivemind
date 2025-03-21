use crate::app::{AppManager, ServiceConfig};
use crate::youki_manager::{Container, ContainerStatus};
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
}

struct NodeLoad {
    node_id: String,
    #[allow(dead_code)]
    resources: NodeResources,
    #[allow(dead_code)]
    container_count: usize,
    load_score: f64,
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
        let pending_containers: Vec<&Container> = containers.iter()
            .filter(|c| c.status == ContainerStatus::Pending)
            .collect();
            
        if !pending_containers.is_empty() {
            println!("Found {} pending containers to schedule", pending_containers.len());
            
            for container in pending_containers {
                // Find the service this container belongs to
                let service = services.iter().find(|s| {
                    s.container_ids.contains(&container.id) || 
                    (container.service_domain.is_some() && container.service_domain == Some(s.domain.clone()))
                });
                
                // Select the best node for this container
                if let Some(best_node) = self.select_best_node_for_container(
                    container, 
                    service, 
                    &node_loads, 
                    &containers
                ).await? {
                    println!("Selected node {} for container {} ({})", 
                        best_node, container.id, container.name);
                        
                    // In a real implementation, we would deploy the container to the selected node
                    // For now, we'll just log the decision
                } else {
                    println!("No suitable node found for container {} ({})", 
                        container.id, container.name);
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
            
            // Calculate load score (weighted combination of CPU, memory, and container count)
            // Lower score means more available resources
            let cpu_factor = (100.0 - resources.cpu_available) / 100.0;
            let memory_factor = 1.0 - (resources.memory_available as f64 / (1024.0 * 1024.0 * 1024.0));
            let container_factor = container_count as f64 * 0.1; // Each container adds 0.1 to the score
            
            let load_score = (cpu_factor * 0.4) + (memory_factor * 0.4) + (container_factor * 0.2);
            
            node_loads.push(NodeLoad {
                node_id: node_id.clone(),
                resources: resources.clone(),
                container_count,
                load_score,
            });
            
            println!("Node {} load: {:.2} (CPU: {:.2}%, Memory: {} MB, Containers: {})", 
                node_id, 
                load_score, 
                100.0 - resources.cpu_available,
                resources.memory_available / (1024 * 1024),
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
        
        // Create a scoring system for each node
        let mut node_scores: HashMap<String, f64> = HashMap::new();
        
        // Initialize scores based on load (lower load = higher score)
        for node in node_loads {
            // Start with a base score that's inverse to the load
            let base_score = 1.0 - node.load_score;
            node_scores.insert(node.node_id.clone(), base_score);
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
        
        Ok(best_node)
    }
    
    // Enhanced rebalancing that considers service constraints
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
        let mut movable_containers: Vec<(&Container, Option<&ServiceConfig>)> = Vec::new();
        
        for container in containers.iter().filter(|c| 
            c.node_id == most_loaded.node_id && c.status == ContainerStatus::Running
        ) {
            // Find the service this container belongs to
            let service = services.iter().find(|s| {
                s.container_ids.contains(&container.id) || 
                (container.service_domain.is_some() && container.service_domain == Some(s.domain.clone()))
            });
            
            movable_containers.push((container, service));
        }
        
        if movable_containers.is_empty() {
            println!("No movable containers found on the most loaded node");
            return Ok(());
        }
        
        // Sort containers by how suitable they are for moving
        // Containers with fewer constraints are better candidates for moving
        movable_containers.sort_by(|(container_a, service_a), (container_b, service_b)| {
            let a_constraints = self.count_container_constraints(container_a, *service_a);
            let b_constraints = self.count_container_constraints(container_b, *service_b);
            a_constraints.cmp(&b_constraints)
        });
        
        // Try to move the container with the fewest constraints
        let (container_to_move, service) = &movable_containers[0];
        
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
    
    // Count the number of constraints for a container
    fn count_container_constraints(&self, _container: &Container, service: Option<&ServiceConfig>) -> usize {
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
        
        constraints
    }
}
