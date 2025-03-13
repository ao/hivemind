use crate::storage::StorageManager;
use anyhow::Result;
use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::net::UdpSocket;
use tokio::sync::Mutex;
use tokio::time;
use uuid::Uuid;

#[derive(Clone)]
pub struct NodeManager {
    node_id: String,
    peers: Arc<Mutex<HashMap<String, NodeInfo>>>,
    health_status: Arc<Mutex<NodeHealth>>,
    storage: Option<StorageManager>,
    address: String,
    discovery_port: u16,
}

#[derive(Clone, Debug)]
pub struct NodeInfo {
    pub id: String,
    pub address: String,
    #[allow(dead_code)]
    pub last_seen: Instant,
    pub resources: NodeResources,
}

#[derive(Clone, Debug)]
pub struct NodeResources {
    pub cpu_available: f64,
    pub memory_available: u64,
    #[allow(dead_code)]
    pub containers_running: u32,
}

pub struct NodeHealth {
    is_healthy: bool,
    last_check: Instant,
    check_count: u32,
    failure_count: u32,
    max_failure_threshold: u32,
}

impl NodeManager {
    pub fn new() -> Self {
        let node_id = format!("node-{}", Uuid::new_v4());
        let address = Self::get_local_ip().unwrap_or_else(|| "127.0.0.1".to_string());

        let mut peers = HashMap::new();

        // Register self as a node
        peers.insert(
            node_id.clone(),
            NodeInfo {
                id: node_id.clone(),
                address: address.clone(),
                last_seen: Instant::now(),
                resources: NodeResources {
                    cpu_available: 100.0,
                    memory_available: 1024 * 1024 * 1024, // 1GB
                    containers_running: 0,
                },
            },
        );

        Self {
            node_id,
            peers: Arc::new(Mutex::new(peers)),
            health_status: Arc::new(Mutex::new(NodeHealth {
                is_healthy: true,
                last_check: Instant::now(),
                check_count: 0,
                failure_count: 0,
                max_failure_threshold: 3,
            })),
            storage: None,
            address,
            discovery_port: 8901,
        }
    }

    pub async fn with_storage(storage: StorageManager) -> Self {
        let mut manager = Self::new();
        manager.storage = Some(storage);
        manager
    }

    #[allow(dead_code)]
    pub fn get_node_id(&self) -> String {
        self.node_id.clone()
    }

    fn get_local_ip() -> Option<String> {
        // In a real implementation, this would detect the actual local IP
        // For now, we'll use a placeholder
        Some("127.0.0.1".to_string())
    }

    pub async fn start_discovery(&self) -> Result<()> {
        println!("Starting node discovery on {}:{}", self.address, self.discovery_port);

        // Save the current node to storage if available
        if let Some(storage) = &self.storage {
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs() as i64;

            storage.save_node(&self.node_id, &self.address, now).await?
        }

        // Clone necessary data for the discovery task
        let node_id = self.node_id.clone();
        let peers = self.peers.clone();
        let discovery_port = self.discovery_port;
        let address = self.address.clone();

        // Start UDP discovery service
        tokio::spawn(async move {
            if let Err(e) = Self::run_discovery_service(node_id, address, discovery_port, peers).await {
                eprintln!("Discovery service error: {}", e);
            }
        });

        println!("Local node registered with ID: {}", self.node_id);
        Ok(())
    }

    async fn run_discovery_service(
        node_id: String,
        address: String,
        discovery_port: u16,
        peers: Arc<Mutex<HashMap<String, NodeInfo>>>,
    ) -> Result<()> {
        // Bind to UDP socket for discovery
        let socket = UdpSocket::bind(format!("0.0.0.0:{}", discovery_port)).await?;
        socket.set_broadcast(true)?;

        // Create a buffer for receiving discovery messages
        let mut buf = [0u8; 1024];

        // Set up periodic broadcast
        let broadcast_interval = time::interval(Duration::from_secs(5));
        tokio::pin!(broadcast_interval);

        // Discovery message format: "HIVEMIND_DISCOVERY:<node_id>:<address>"
        let discovery_msg = format!("HIVEMIND_DISCOVERY:{}:{}", node_id, address);

        loop {
            tokio::select! {
                _ = broadcast_interval.tick() => {
                    // Broadcast discovery message
                    let broadcast_addr = format!("255.255.255.255:{}", discovery_port);
                    if let Err(e) = socket.send_to(discovery_msg.as_bytes(), broadcast_addr).await {
                        eprintln!("Failed to send discovery broadcast: {}", e);
                    }
                }
                result = socket.recv_from(&mut buf) => {
                    match result {
                        Ok((len, _src)) => {
                            // Process received discovery message
                            if let Ok(msg) = std::str::from_utf8(&buf[..len]) {
                                if msg.starts_with("HIVEMIND_DISCOVERY:") {
                                    let parts: Vec<&str> = msg.split(':').collect();
                                    if parts.len() >= 3 {
                                        let peer_id = parts[1].to_string();
                                        let peer_addr = parts[2].to_string();
                                        
                                        // Don't add self as a peer
                                        if peer_id != node_id {
                                            let mut peers_lock = peers.lock().await;
                                            peers_lock.insert(peer_id.clone(), NodeInfo {
                                                id: peer_id.clone(),
                                                address: peer_addr,
                                                last_seen: Instant::now(),
                                                resources: NodeResources {
                                                    cpu_available: 100.0,
                                                    memory_available: 1024 * 1024 * 1024,
                                                    containers_running: 0,
                                                },
                                            });
                                            println!("Discovered peer: {}", peer_id);
                                        }
                                    }
                                }
                            }
                        }
                        Err(e) => eprintln!("Failed to receive: {}", e),
                    }
                }
            }
        }
    }

    pub async fn list_nodes(&self) -> Result<Vec<String>> {
        // If we have storage, get nodes from there
        if let Some(storage) = &self.storage {
            let nodes = storage.get_nodes().await?;
            return Ok(nodes.into_iter().map(|(id, _, _)| id).collect());
        }

        // Otherwise use in-memory peers
        let peers = self.peers.lock().await;
        Ok(peers.keys().cloned().collect())
    }

    pub async fn get_node_details(&self) -> Result<Vec<(String, String, NodeResources)>> {
        let peers = self.peers.lock().await;
        Ok(peers
            .iter()
            .map(|(_, info)| {
                (info.id.clone(), info.address.clone(), info.resources.clone())
            })
            .collect())
    }

    pub async fn check_health(&self) -> Result<bool> {
        let mut health = self.health_status.lock().await;
        health.last_check = Instant::now();
        health.check_count += 1;

        // Perform actual health checks
        // 1. Check system resources
        let cpu_usage = Self::get_cpu_usage().await;
        let memory_usage = Self::get_memory_usage().await;
        
        // 2. Check if resources are within acceptable limits
        let is_healthy = cpu_usage < 90.0 && memory_usage < 90.0;
        
        // Update health status
        if !is_healthy {
            health.failure_count += 1;
        } else {
            // Reset failure count if we're healthy
            health.failure_count = 0;
        }
        
        // Update overall health status
        health.is_healthy = health.failure_count < health.max_failure_threshold;

        // Update node resources in peers list
        let mut peers = self.peers.lock().await;
        if let Some(self_info) = peers.get_mut(&self.node_id) {
            self_info.resources.cpu_available = 100.0 - cpu_usage;
            self_info.resources.memory_available = ((100.0 - memory_usage) / 100.0 * 1024.0 * 1024.0 * 1024.0) as u64;
        }

        Ok(health.is_healthy)
    }

    async fn get_cpu_usage() -> f64 {
        // In a real implementation, this would check actual CPU usage
        // For now, return a random value between 10 and 70
        10.0 + (std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_secs() % 60) as f64
    }

    async fn get_memory_usage() -> f64 {
        // In a real implementation, this would check actual memory usage
        // For now, return a random value between 20 and 80
        20.0 + (std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_secs() % 60) as f64
    }

    pub async fn remove_node(&self, node_id: &str) -> Result<bool> {
        // Don't allow removing self
        if node_id == self.node_id {
            println!("Cannot remove the local node");
            return Ok(false);
        }

        let mut peers = self.peers.lock().await;
        let removed = peers.remove(node_id).is_some();

        // Also remove from storage if available
        if removed && node_id != self.node_id {
            if let Some(_storage) = &self.storage {
                // In a real implementation, we would have a method to remove a node from storage
                println!(
                    "Node {} removed from memory, would also remove from storage",
                    node_id
                );
            }
        }

        Ok(removed)
    }

    pub async fn join_cluster(&self, host: &str) -> Result<()> {
        println!("Joining cluster at {}", host);

        // Save the current node to storage if available
        if let Some(storage) = &self.storage {
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs() as i64;

            storage.save_node(&self.node_id, &self.address, now).await?
        }

        // Start discovery service
        self.start_discovery().await?;

        // Connect to the host node
        let socket = UdpSocket::bind(format!("0.0.0.0:{}", self.discovery_port)).await?;
        let msg = format!("HIVEMIND_JOIN:{}:{}", self.node_id, self.address);
        socket.send_to(msg.as_bytes(), format!("{}{}", host, self.discovery_port)).await?;

        println!("Successfully joined cluster through {}", host);
        Ok(())
    }
}
