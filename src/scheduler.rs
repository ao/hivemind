use crate::app::{AppManager, Container, ContainerStatus};
use crate::node::{NodeManager, NodeResources};
use anyhow::Result;
use std::sync::Arc;

pub struct ContainerScheduler {
    app_manager: AppManager,
    node_manager: Option<Arc<NodeManager>>,
    scheduling_interval: u64, // seconds
    rebalance_threshold: f64, // percentage difference to trigger rebalancing
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
            scheduling_interval: 30,
            rebalance_threshold: 20.0, // 20% difference in load will trigger rebalancing
        }
    }

    pub fn with_node_manager(mut self, node_manager: Arc<NodeManager>) -> Self {
        self.node_manager = Some(node_manager);
        self
    }
    #[allow(dead_code)]
    pub fn set_scheduling_interval(mut self, interval_secs: u64) -> Self {
        self.scheduling_interval = interval_secs;
        self
    }

    pub async fn start(&self) -> Result<()> {
        println!("Starting container scheduler...");

        loop {
            if let Err(e) = self.schedule_containers().await {
                eprintln!("Scheduling error: {}", e);
            }

            tokio::time::sleep(tokio::time::Duration::from_secs(self.scheduling_interval)).await;
        }
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
                self.rebalance_containers(&node_loads, &containers).await?;
            } else {
                println!("Nodes are balanced (load difference: {:.2}%)", load_difference_percent);
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
    
    async fn rebalance_containers(
        &self,
        node_loads: &[NodeLoad],
        containers: &[Container],
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
        let movable_containers: Vec<&Container> = containers.iter()
            .filter(|c| c.node_id == most_loaded.node_id && c.status == ContainerStatus::Running)
            .collect();
        
        if movable_containers.is_empty() {
            println!("No movable containers found on the most loaded node");
            return Ok(());
        }
        
        // In a real implementation, we would:
        // 1. Select a container to move based on resource requirements
        // 2. Stop the container on the source node
        // 3. Start an identical container on the target node
        // 4. Update container records
        
        // For this simulation, we'll just log what would happen
        let container_to_move = movable_containers[0];
        println!("Would move container {} ({}) from node {} to node {}",
            container_to_move.id, container_to_move.name,
            most_loaded.node_id, least_loaded.node_id);
        
        Ok(())
    }
}
