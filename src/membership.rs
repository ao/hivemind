use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::collections::{HashMap, HashSet};
use std::net::SocketAddr;
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::net::UdpSocket;
use tokio::sync::Mutex;
use tokio::time;

// Protocol constants
const PROTOCOL_PERIOD: Duration = Duration::from_secs(1);
const PING_TIMEOUT: Duration = Duration::from_millis(500);
const INDIRECT_PING_COUNT: usize = 3;
const SUSPICION_MULTIPLIER: u32 = 5;
const MAX_TRANSMIT_SIZE: usize = 512;
const GOSSIP_FACTOR: usize = 3; // Number of members to gossip to per round
const MAX_GOSSIP_UPDATES: usize = 5; // Maximum number of updates to include in gossip
const REINTEGRATION_TIME: Duration = Duration::from_secs(60); // Time before allowing a failed node to rejoin
const QUORUM_PERCENTAGE: f64 = 0.51; // Percentage of nodes needed for quorum (>50%)
const LEADER_CHECK_INTERVAL: Duration = Duration::from_secs(5); // How often to check leader status

// Node states in the membership protocol
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub enum NodeState {
    Alive,
    Suspected,
    Dead,
    Left,
    Maintenance, // New state for planned maintenance
}

// Member represents a node in the cluster
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Member {
    pub id: String,
    pub address: String,
    pub state: NodeState,
    pub incarnation: u64,
    pub last_state_change: u64,
    pub is_leader: bool, // Whether this node is a leader
    pub last_leader_check: u64, // Last time leadership was checked
    pub metadata: Option<HashMap<String, String>>, // Additional metadata for the node
}

// Message types for the membership protocol
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum MessageType {
    Ping,
    PingReq,
    Ack,
    Sync,
    Join,
    Leave,
    LeaderElection, // For leader election
    LeaderAnnounce, // For announcing a new leader
    Maintenance,    // For entering maintenance mode
    Reintegrate,    // For reintegrating after failure
    StateSync,      // For state synchronization
}

// Protocol message structure
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Message {
    pub message_type: MessageType,
    pub sender_id: String,
    pub sender_addr: String,
    pub target_id: Option<String>,
    pub incarnation: u64,
    pub members: Option<Vec<Member>>,
}

// MembershipEvent represents changes in the cluster membership
#[derive(Debug, Clone)]
pub enum MembershipEvent {
    MemberJoined(Member),
    MemberSuspected(Member),
    MemberDead(Member),
    MemberLeft(Member),
    MemberAlive(Member),
    MemberMaintenance(Member),
    LeaderElected(Member),
    NetworkPartitionDetected,
    NetworkPartitionResolved,
    QuorumLost,
    QuorumRestored,
}

// MembershipConfig holds configuration for the membership protocol
#[derive(Clone, Debug)]
pub struct MembershipConfig {
    pub bind_addr: String,
    pub bind_port: u16,
    pub protocol_period: Duration,
    pub ping_timeout: Duration,
    pub suspicion_mult: u32,
    pub indirect_checks: usize,
    pub gossip_factor: usize,
    pub max_gossip_updates: usize,
    pub reintegration_time: Duration,
    pub quorum_percentage: f64,
    pub leader_check_interval: Duration,
}

impl Default for MembershipConfig {
    fn default() -> Self {
        Self {
            bind_addr: "0.0.0.0".to_string(),
            bind_port: 7946,
            protocol_period: PROTOCOL_PERIOD,
            ping_timeout: PING_TIMEOUT,
            suspicion_mult: SUSPICION_MULTIPLIER,
            indirect_checks: INDIRECT_PING_COUNT,
            gossip_factor: GOSSIP_FACTOR,
            max_gossip_updates: MAX_GOSSIP_UPDATES,
            reintegration_time: REINTEGRATION_TIME,
            quorum_percentage: QUORUM_PERCENTAGE,
            leader_check_interval: LEADER_CHECK_INTERVAL,
        }
    }
}

// MembershipProtocol implements the SWIM protocol for cluster membership
#[derive(Debug)]
pub struct MembershipProtocol {
    node_id: String,
    addr: String,
    incarnation: Arc<Mutex<u64>>,
    members: Arc<Mutex<HashMap<String, Member>>>,
    suspect_timers: Arc<Mutex<HashMap<String, Instant>>>,
    reintegration_timers: Arc<Mutex<HashMap<String, Instant>>>,
    config: MembershipConfig,
    socket: Arc<UdpSocket>,
    event_tx: tokio::sync::mpsc::Sender<MembershipEvent>,
    event_rx: Arc<Mutex<tokio::sync::mpsc::Receiver<MembershipEvent>>>,
    shutdown_tx: tokio::sync::mpsc::Sender<()>,
    shutdown_rx: Arc<Mutex<tokio::sync::mpsc::Receiver<()>>>,
    state_store: Arc<Mutex<HashMap<String, Vec<u8>>>>,
    state_versions: Arc<Mutex<HashMap<String, u64>>>,
    is_leader: Arc<Mutex<bool>>,
    quorum_size: Arc<Mutex<usize>>,
    partition_detected: Arc<Mutex<bool>>,
}

impl MembershipProtocol {
    pub async fn new(node_id: String, addr: String, config: Option<MembershipConfig>) -> Result<Self> {
        let config = config.unwrap_or_default();
        
        // Create UDP socket for membership protocol
        let socket_addr = format!("{}:{}", config.bind_addr, config.bind_port);
        let socket = UdpSocket::bind(&socket_addr).await?;
        socket.set_broadcast(true)?;
        
        // Create channels for events and shutdown
        let (event_tx, event_rx) = tokio::sync::mpsc::channel(100);
        let (shutdown_tx, shutdown_rx) = tokio::sync::mpsc::channel(1);
        
        // Initialize with self as the only member
        let mut members = HashMap::new();
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();
            
        let self_member = Member {
            id: node_id.clone(),
            address: addr.clone(),
            state: NodeState::Alive,
            incarnation: 1,
            last_state_change: now,
            is_leader: false, // Start as non-leader
            last_leader_check: now,
            metadata: Some(HashMap::new()),
        };
        
        members.insert(node_id.clone(), self_member);
        
        // Calculate initial quorum size (just 1 for single node)
        let quorum_size = 1;
        
        Ok(Self {
            node_id,
            addr,
            incarnation: Arc::new(Mutex::new(1)),
            members: Arc::new(Mutex::new(members)),
            suspect_timers: Arc::new(Mutex::new(HashMap::new())),
            reintegration_timers: Arc::new(Mutex::new(HashMap::new())),
            config,
            socket: Arc::new(socket),
            event_tx,
            event_rx: Arc::new(Mutex::new(event_rx)),
            shutdown_tx,
            shutdown_rx: Arc::new(Mutex::new(shutdown_rx)),
            state_store: Arc::new(Mutex::new(HashMap::new())),
            state_versions: Arc::new(Mutex::new(HashMap::new())),
            is_leader: Arc::new(Mutex::new(false)),
            quorum_size: Arc::new(Mutex::new(quorum_size)),
            partition_detected: Arc::new(Mutex::new(false)),
        })
    }
    
    // Start the membership protocol
    pub async fn start(&self) -> Result<()> {
        println!("Starting membership protocol on {}", self.addr);
        
        // Clone necessary data for the protocol task
        let node_id = self.node_id.clone();
        let addr = self.addr.clone();
        let members = self.members.clone();
        let incarnation = self.incarnation.clone();
        let suspect_timers = self.suspect_timers.clone();
        let reintegration_timers = self.reintegration_timers.clone();
        let socket = self.socket.clone();
        let config = self.config.clone();
        let event_tx = self.event_tx.clone();
        let shutdown_rx = self.shutdown_rx.clone();
        let state_store = self.state_store.clone();
        let state_versions = self.state_versions.clone();
        let is_leader = self.is_leader.clone();
        let quorum_size = self.quorum_size.clone();
        let partition_detected = self.partition_detected.clone();
        
        // Start the main protocol loop
        tokio::spawn(async move {
            if let Err(e) = Self::run_protocol(
                node_id,
                addr,
                members,
                incarnation,
                suspect_timers,
                reintegration_timers,
                socket,
                config,
                event_tx,
                shutdown_rx,
                state_store,
                state_versions,
                is_leader,
                quorum_size,
                partition_detected,
            )
            .await
            {
                eprintln!("Membership protocol error: {}", e);
            }
        });
        
        Ok(())
    }
    
    // Main protocol loop
    async fn run_protocol(
        node_id: String,
        addr: String,
        members: Arc<Mutex<HashMap<String, Member>>>,
        incarnation: Arc<Mutex<u64>>,
        suspect_timers: Arc<Mutex<HashMap<String, Instant>>>,
        reintegration_timers: Arc<Mutex<HashMap<String, Instant>>>,
        socket: Arc<UdpSocket>,
        config: MembershipConfig,
        event_tx: tokio::sync::mpsc::Sender<MembershipEvent>,
        shutdown_rx: Arc<Mutex<tokio::sync::mpsc::Receiver<()>>>,
        state_store: Arc<Mutex<HashMap<String, Vec<u8>>>>,
        state_versions: Arc<Mutex<HashMap<String, u64>>>,
        is_leader: Arc<Mutex<bool>>,
        quorum_size: Arc<Mutex<usize>>,
        partition_detected: Arc<Mutex<bool>>,
    ) -> Result<()> {
        // Set up protocol period ticker
        let protocol_period = time::interval(config.protocol_period);
        tokio::pin!(protocol_period);
        
        // Set up leader check ticker
        let leader_check = time::interval(config.leader_check_interval);
        tokio::pin!(leader_check);
        
        // Buffer for receiving messages
        let mut buf = [0u8; MAX_TRANSMIT_SIZE];
        
        loop {
            tokio::select! {
                _ = protocol_period.tick() => {
                    // Perform protocol period tasks
                    Self::protocol_tick(
                        &node_id,
                        &addr,
                        &members,
                        &incarnation,
                        &suspect_timers,
                        &reintegration_timers,
                        &socket,
                        &config,
                        &event_tx,
                    ).await?;
                    
                    // Perform gossip dissemination
                    Self::gossip_dissemination(
                        &node_id,
                        &addr,
                        &members,
                        &incarnation,
                        &socket,
                        &config,
                    ).await?;
                    
                    // Check for quorum
                    Self::check_quorum(
                        &members,
                        &quorum_size,
                        &partition_detected,
                        &event_tx,
                    ).await?;
                }
                
                _ = leader_check.tick() => {
                    // Check leader status and elect if needed
                    Self::check_leader_status(
                        &node_id,
                        &members,
                        &is_leader,
                        &socket,
                        &config,
                        &event_tx,
                    ).await?;
                }
                result = socket.recv_from(&mut buf) => {
                    match result {
                        Ok((len, src)) => {
                            // Process received message
                            if let Ok(msg) = bincode::deserialize::<Message>(&buf[..len]) {
                                Self::handle_message(
                                    msg,
                                    src,
                                    &node_id,
                                    &addr,
                                    &members,
                                    &incarnation,
                                    &suspect_timers,
                                    &reintegration_timers,
                                    &socket,
                                    &config,
                                    &event_tx,
                                    &state_store,
                                    &state_versions,
                                    &is_leader,
                                    &quorum_size,
                                    &partition_detected,
                                ).await?;
                            }
                        }
                        Err(e) => eprintln!("Failed to receive: {}", e),
                    }
                }
                _ = async {
                    let mut rx = shutdown_rx.lock().await;
                    rx.recv().await
                } => {
                    println!("Shutting down membership protocol");
                    break;
                }
            }
        }
        
        Ok(())
    }
    
    // Protocol period tick - select a random member and ping it
    async fn protocol_tick(
        node_id: &str,
        addr: &str,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        incarnation: &Arc<Mutex<u64>>,
        suspect_timers: &Arc<Mutex<HashMap<String, Instant>>>,
        reintegration_timers: &Arc<Mutex<HashMap<String, Instant>>>,
        socket: &Arc<UdpSocket>,
        config: &MembershipConfig,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        // Check suspect timers
        Self::check_suspect_timers(members, suspect_timers, config, event_tx).await?;
        
        // Check reintegration timers
        Self::check_reintegration_timers(members, reintegration_timers, event_tx).await?;
        
        // Select a random member to ping
        let target = {
            let members_lock = members.lock().await;
            let alive_members: Vec<_> = members_lock
                .iter()
                .filter(|(id, m)| *id != node_id && m.state == NodeState::Alive)
                .map(|(_, m)| m.clone())
                .collect();
                
            if alive_members.is_empty() {
                return Ok(());
            }
            
            // Select a random member
            let idx = rand::random::<usize>() % alive_members.len();
            alive_members[idx].clone()
        };
        
        // Send ping to the selected member
        let inc = *incarnation.lock().await;
        let ping_msg = Message {
            message_type: MessageType::Ping,
            sender_id: node_id.to_string(),
            sender_addr: addr.to_string(),
            target_id: Some(target.id.clone()),
            incarnation: inc,
            members: None,
        };
        
        let encoded = bincode::serialize(&ping_msg)?;
        let target_addr = format!("{}:{}", target.address, config.bind_port);
        
        // Send ping and wait for ack with timeout
        socket.send_to(&encoded, &target_addr).await?;
        
        // Clone necessary data for the ping task
        let node_id = node_id.to_string();
        let addr = addr.to_string();
        let target_id = target.id.clone();
        let target_addr = target.address.clone();
        let members_clone = members.clone();
        let incarnation_clone = incarnation.clone();
        let suspect_timers_clone = suspect_timers.clone();
        let socket_clone = socket.clone();
        let config_clone = config.clone();
        let event_tx_clone = event_tx.clone();
        let ping_timeout = config.ping_timeout;
        
        // Spawn a task to handle the ping timeout
        tokio::spawn(async move {
            // Wait for timeout
            tokio::time::sleep(ping_timeout).await;
            
            // Check if the member is still in the alive state
            let is_alive = {
                let members_lock = members_clone.lock().await;
                if let Some(member) = members_lock.get(&target_id) {
                    member.state == NodeState::Alive
                } else {
                    false
                }
            };
            
            if is_alive {
                // If still alive, start indirect pinging
                if let Err(e) = Self::indirect_ping(
                    &node_id,
                    &addr,
                    &target_id,
                    &target_addr,
                    &members_clone,
                    &incarnation_clone,
                    &suspect_timers_clone,
                    &socket_clone,
                    &config_clone,
                    &event_tx_clone,
                )
                .await
                {
                    eprintln!("Error in indirect ping: {}", e);
                }
            }
        });
        
        Ok(())
    }
    
    // Indirect ping through k random members
    async fn indirect_ping(
        node_id: &str,
        addr: &str,
        target_id: &str,
        _target_addr: &str,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        incarnation: &Arc<Mutex<u64>>,
        suspect_timers: &Arc<Mutex<HashMap<String, Instant>>>,
        socket: &Arc<UdpSocket>,
        config: &MembershipConfig,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        // Select k random members for indirect pinging
        let ping_reqs = {
            let members_lock = members.lock().await;
            let alive_members: Vec<_> = members_lock
                .iter()
                .filter(|(id, m)| 
                    *id != node_id && 
                    *id != target_id && 
                    m.state == NodeState::Alive
                )
                .map(|(_, m)| m.clone())
                .collect();
                
            if alive_members.is_empty() {
                // No other members to ask, mark as suspected directly
                Self::suspect_member(
                    target_id,
                    members,
                    incarnation,
                    suspect_timers,
                    config,
                    event_tx,
                ).await?;
                return Ok(());
            }
            
            // Select up to k random members
            let mut selected = Vec::new();
            let mut indices = HashSet::new();
            let k = std::cmp::min(config.indirect_checks, alive_members.len());
            
            while selected.len() < k {
                let idx = rand::random::<usize>() % alive_members.len();
                if indices.insert(idx) {
                    selected.push(alive_members[idx].clone());
                }
            }
            
            selected
        };
        
        // Send ping-req to selected members
        let inc = *incarnation.lock().await;
        for member in ping_reqs {
            let ping_req_msg = Message {
                message_type: MessageType::PingReq,
                sender_id: node_id.to_string(),
                sender_addr: addr.to_string(),
                target_id: Some(target_id.to_string()),
                incarnation: inc,
                members: None,
            };
            
            let encoded = bincode::serialize(&ping_req_msg)?;
            let member_addr = format!("{}:{}", member.address, config.bind_port);
            socket.send_to(&encoded, &member_addr).await?;
        }
        
        // Clone necessary data for the timeout task
        let target_id = target_id.to_string();
        let members_clone = members.clone();
        let incarnation_clone = incarnation.clone();
        let suspect_timers_clone = suspect_timers.clone();
        let config_clone = config.clone();
        let event_tx_clone = event_tx.clone();
        let ping_timeout = config.ping_timeout * 2;
        
        // Spawn a task to handle the indirect ping timeout
        tokio::spawn(async move {
            // Wait for timeout
            tokio::time::sleep(ping_timeout).await;
            
            // Check if the member is still in the alive state
            let is_alive = {
                let members_lock = members_clone.lock().await;
                if let Some(member) = members_lock.get(&target_id) {
                    member.state == NodeState::Alive
                } else {
                    false
                }
            };
            
            if is_alive {
                // If still alive after indirect pinging, mark as suspected
                if let Err(e) = Self::suspect_member(
                    &target_id,
                    &members_clone,
                    &incarnation_clone,
                    &suspect_timers_clone,
                    &config_clone,
                    &event_tx_clone,
                )
                .await
                {
                    eprintln!("Error in suspect member: {}", e);
                }
            }
        });
        
        Ok(())
    }
    
    // Mark a member as suspected
    async fn suspect_member(
        target_id: &str,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        _incarnation: &Arc<Mutex<u64>>,
        suspect_timers: &Arc<Mutex<HashMap<String, Instant>>>,
        config: &MembershipConfig,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        let mut members_lock = members.lock().await;
        
        if let Some(member) = members_lock.get_mut(target_id) {
            if member.state != NodeState::Alive {
                return Ok(());
            }
            
            // Update member state to suspected
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs();
                
            member.state = NodeState::Suspected;
            member.last_state_change = now;
            
            // Clone for event
            let member_clone = member.clone();
            
            // Set up suspicion timer
            let mut suspect_timers_lock = suspect_timers.lock().await;
            suspect_timers_lock.insert(
                target_id.to_string(),
                Instant::now() + config.protocol_period * config.suspicion_mult,
            );
            
            // Send event
            let _ = event_tx
                .send(MembershipEvent::MemberSuspected(member_clone))
                .await;
                
            println!("Member {} is now suspected", target_id);
        }
        
        Ok(())
    }
    
    // Check suspect timers and mark members as dead if timeout
    async fn check_suspect_timers(
        members: &Arc<Mutex<HashMap<String, Member>>>,
        suspect_timers: &Arc<Mutex<HashMap<String, Instant>>>,
        _config: &MembershipConfig,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        let now = Instant::now();
        let mut expired = Vec::new();
        
        // Find expired timers
        {
            let suspect_timers_lock = suspect_timers.lock().await;
            for (id, deadline) in suspect_timers_lock.iter() {
                if now >= *deadline {
                    expired.push(id.clone());
                }
            }
        }
        
        // Process expired timers
        if !expired.is_empty() {
            let mut suspect_timers_lock = suspect_timers.lock().await;
            let mut members_lock = members.lock().await;
            
            for id in expired {
                // Remove timer
                suspect_timers_lock.remove(&id);
                
                // Mark member as dead
                if let Some(member) = members_lock.get_mut(&id) {
                    if member.state == NodeState::Suspected {
                        let now = std::time::SystemTime::now()
                            .duration_since(std::time::UNIX_EPOCH)
                            .unwrap()
                            .as_secs();
                            
                        member.state = NodeState::Dead;
                        member.last_state_change = now;
                        
                        // Clone for event
                        let member_clone = member.clone();
                        
                        // Send event
                        let _ = event_tx
                            .send(MembershipEvent::MemberDead(member_clone))
                            .await;
                            
                        println!("Member {} is now dead", id);
                    }
                }
            }
        }
        
        Ok(())
    }
    
    // Handle received messages
    async fn handle_message(
        msg: Message,
        src: SocketAddr,
        node_id: &str,
        addr: &str,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        incarnation: &Arc<Mutex<u64>>,
        suspect_timers: &Arc<Mutex<HashMap<String, Instant>>>,
        reintegration_timers: &Arc<Mutex<HashMap<String, Instant>>>,
        socket: &Arc<UdpSocket>,
        config: &MembershipConfig,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
        state_store: &Arc<Mutex<HashMap<String, Vec<u8>>>>,
        state_versions: &Arc<Mutex<HashMap<String, u64>>>,
        is_leader: &Arc<Mutex<bool>>,
        quorum_size: &Arc<Mutex<usize>>,
        partition_detected: &Arc<Mutex<bool>>,
    ) -> Result<()> {
        match msg.message_type {
            MessageType::Ping => {
                // Respond with ack
                let inc = *incarnation.lock().await;
                let ack_msg = Message {
                    message_type: MessageType::Ack,
                    sender_id: node_id.to_string(),
                    sender_addr: addr.to_string(),
                    target_id: Some(msg.sender_id),
                    incarnation: inc,
                    members: Some(Self::get_members_list(members).await),
                };
                
                let encoded = bincode::serialize(&ack_msg)?;
                socket.send_to(&encoded, src).await?;
            }
            MessageType::PingReq => {
                // Forward ping to target
                if let Some(target_id) = &msg.target_id {
                    let target_addr = {
                        let members_lock = members.lock().await;
                        if let Some(member) = members_lock.get(target_id) {
                            format!("{}:{}", member.address, config.bind_port)
                        } else {
                            return Ok(());
                        }
                    };
                    
                    // Send ping to target
                    let inc = *incarnation.lock().await;
                    let ping_msg = Message {
                        message_type: MessageType::Ping,
                        sender_id: node_id.to_string(),
                        sender_addr: addr.to_string(),
                        target_id: Some(target_id.clone()),
                        incarnation: inc,
                        members: None,
                    };
                    
                    let encoded = bincode::serialize(&ping_msg)?;
                    socket.send_to(&encoded, &target_addr).await?;
                    
                    // Clone necessary data
                    let node_id = node_id.to_string();
                    let addr = addr.to_string();
                    let target_id = target_id.clone();
                    let sender_addr = msg.sender_addr.clone();
                    let socket_clone = socket.clone();
                    let ping_timeout = config.ping_timeout;
                    let bind_port = config.bind_port;
                    
                    // Spawn task to handle response or timeout
                    tokio::spawn(async move {
                        // Wait for timeout
                        tokio::time::sleep(ping_timeout).await;
                        
                        // Send ack back to original sender
                        let ack_msg = Message {
                            message_type: MessageType::Ack,
                            sender_id: node_id,
                            sender_addr: addr,
                            target_id: Some(target_id),
                            incarnation: 0, // Not important for this ack
                            members: None,
                        };
                        
                        if let Ok(encoded) = bincode::serialize(&ack_msg) {
                            let sender_addr = format!("{}:{}", sender_addr, bind_port);
                            let _ = socket_clone.send_to(&encoded, sender_addr).await;
                        }
                    });
                }
            }
            MessageType::Ack => {
                // Process ack - update member status
                Self::process_ack(
                    &msg.sender_id,
                    msg.incarnation,
                    members,
                    suspect_timers,
                    event_tx,
                ).await?;
                
                // Process member list if provided
                if let Some(member_list) = msg.members {
                    Self::process_member_list(
                        member_list,
                        members,
                        incarnation,
                        suspect_timers,
                        event_tx,
                    ).await?;
                }
            }
            MessageType::Join => {
                // Process join request
                Self::process_join(
                    &msg.sender_id,
                    &msg.sender_addr,
                    members,
                    socket,
                    config,
                    event_tx,
                ).await?;
            }
            MessageType::Leave => {
                // Process leave notification
                Self::process_leave(
                    &msg.sender_id,
                    members,
                    event_tx,
                ).await?;
            }
            MessageType::Sync => {
                // Process sync message with member list
                if let Some(member_list) = msg.members {
                    Self::process_member_list(
                        member_list,
                        members,
                        incarnation,
                        suspect_timers,
                        event_tx,
                    ).await?;
                }
            }
            MessageType::LeaderElection => {
                // Process leader election message
                Self::process_leader_election(
                    &msg.sender_id,
                    &msg.sender_addr,
                    msg.incarnation,
                    members,
                    incarnation,
                    socket,
                    config,
                    is_leader,
                    event_tx,
                ).await?;
            }
            MessageType::LeaderAnnounce => {
                // Process leader announcement
                Self::process_leader_announce(
                    &msg.sender_id,
                    msg.incarnation,
                    members,
                    is_leader,
                    event_tx,
                ).await?;
            }
            MessageType::Maintenance => {
                // Process maintenance mode request
                Self::process_maintenance(
                    &msg.sender_id,
                    members,
                    event_tx,
                ).await?;
            }
            MessageType::Reintegrate => {
                // Process reintegration request
                Self::process_reintegration(
                    &msg.sender_id,
                    &msg.sender_addr,
                    members,
                    reintegration_timers,
                    event_tx,
                ).await?;
            }
            MessageType::StateSync => {
                // Process state synchronization
                if let Some(member_list) = msg.members {
                    Self::process_state_sync(
                        &msg.sender_id,
                        member_list,
                        state_store,
                        state_versions,
                        socket,
                        src,
                    ).await?;
                }
            }
        }
        
        Ok(())
    }
    
    // Process ack message
    async fn process_ack(
        sender_id: &str,
        sender_incarnation: u64,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        suspect_timers: &Arc<Mutex<HashMap<String, Instant>>>,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        let mut members_lock = members.lock().await;
        
        if let Some(member) = members_lock.get_mut(sender_id) {
            // Update incarnation if newer
            if sender_incarnation > member.incarnation {
                member.incarnation = sender_incarnation;
            }
            
            // If member was suspected, mark as alive
            if member.state == NodeState::Suspected {
                let now = std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_secs();
                    
                member.state = NodeState::Alive;
                member.last_state_change = now;
                
                // Remove from suspect timers
                let mut suspect_timers_lock = suspect_timers.lock().await;
                suspect_timers_lock.remove(sender_id);
                
                // Clone for event
                let member_clone = member.clone();
                
                // Send event
                let _ = event_tx
                    .send(MembershipEvent::MemberAlive(member_clone))
                    .await;
                    
                println!("Member {} is now alive again", sender_id);
            }
        } else {
            // Unknown member, request full sync
            // This would be implemented in a real system
        }
        
        Ok(())
    }
    
    // Process member list from messages
    async fn process_member_list(
        member_list: Vec<Member>,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        _incarnation: &Arc<Mutex<u64>>,
        suspect_timers: &Arc<Mutex<HashMap<String, Instant>>>,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        let mut members_lock = members.lock().await;
        let mut suspect_timers_lock = suspect_timers.lock().await;
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();
            
        for remote_member in member_list {
            let remote_id = remote_member.id.clone();
            
            match members_lock.get_mut(&remote_id) {
                Some(local_member) => {
                    // Compare incarnation numbers
                    if remote_member.incarnation > local_member.incarnation {
                        // Remote has newer information
                        let old_state = local_member.state.clone();
                        local_member.incarnation = remote_member.incarnation;
                        local_member.state = remote_member.state.clone();
                        local_member.last_state_change = now;
                        
                        // Update suspect timers if needed
                        match remote_member.state {
                            NodeState::Alive => {
                                suspect_timers_lock.remove(&remote_id);
                                
                                if old_state != NodeState::Alive {
                                    // Send event
                                    let _ = event_tx
                                        .send(MembershipEvent::MemberAlive(local_member.clone()))
                                        .await;
                                }
                            }
                            NodeState::Suspected => {
                                if old_state == NodeState::Alive {
                                    // Add to suspect timers
                                    suspect_timers_lock.insert(
                                        remote_id.clone(),
                                        Instant::now() + PROTOCOL_PERIOD * SUSPICION_MULTIPLIER,
                                    );
                                    
                                    // Send event
                                    let _ = event_tx
                                        .send(MembershipEvent::MemberSuspected(local_member.clone()))
                                        .await;
                                }
                            }
                            NodeState::Dead => {
                                if old_state != NodeState::Dead {
                                    // Send event
                                    let _ = event_tx
                                        .send(MembershipEvent::MemberDead(local_member.clone()))
                                        .await;
                                }
                            }
                            NodeState::Left => {
                                if old_state != NodeState::Left {
                                    // Send event
                                    let _ = event_tx
                                        .send(MembershipEvent::MemberLeft(local_member.clone()))
                                        .await;
                                }
                            }
                            NodeState::Maintenance => {
                                if old_state != NodeState::Maintenance {
                                    // Send event
                                    let _ = event_tx
                                        .send(MembershipEvent::MemberMaintenance(local_member.clone()))
                                        .await;
                                }
                            }
                        }
                    }
                }
                None => {
                    // New member, add to our list
                    members_lock.insert(
                        remote_id.clone(),
                        remote_member.clone(),
                    );
                    
                    // If suspected, set up timer
                    if remote_member.state == NodeState::Suspected {
                        suspect_timers_lock.insert(
                            remote_id.clone(),
                            Instant::now() + PROTOCOL_PERIOD * SUSPICION_MULTIPLIER,
                        );
                    }
                    
                    // Send event
                    match remote_member.state {
                        NodeState::Alive => {
                            let _ = event_tx
                                .send(MembershipEvent::MemberJoined(remote_member))
                                .await;
                        }
                        NodeState::Suspected => {
                            let _ = event_tx
                                .send(MembershipEvent::MemberSuspected(remote_member))
                                .await;
                        }
                        NodeState::Dead => {
                            let _ = event_tx
                                .send(MembershipEvent::MemberDead(remote_member))
                                .await;
                        }
                        NodeState::Left => {
                            let _ = event_tx
                                .send(MembershipEvent::MemberLeft(remote_member))
                                .await;
                        }
                        NodeState::Maintenance => {
                            let _ = event_tx
                                .send(MembershipEvent::MemberMaintenance(remote_member))
                                .await;
                        }
                    }
                }
            }
        }
        
        Ok(())
    }
    
    // Process join request
    async fn process_join(
        sender_id: &str,
        sender_addr: &str,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        socket: &Arc<UdpSocket>,
        config: &MembershipConfig,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();
            
        // Add the new member
        let mut members_lock = members.lock().await;
        
        // Check if member already exists
        if members_lock.contains_key(sender_id) {
            // Update address if needed
            if let Some(member) = members_lock.get_mut(sender_id) {
                member.address = sender_addr.to_string();
                member.state = NodeState::Alive;
                member.last_state_change = now;
            }
        } else {
            // Add new member
            let new_member = Member {
                id: sender_id.to_string(),
                address: sender_addr.to_string(),
                state: NodeState::Alive,
                incarnation: 1,
                last_state_change: now,
                is_leader: false,
                last_leader_check: now,
                metadata: Some(HashMap::new()),
            };
            
            members_lock.insert(sender_id.to_string(), new_member.clone());
            
            // Send event
            let _ = event_tx
                .send(MembershipEvent::MemberJoined(new_member))
                .await;
        }
        
        // Send sync message with full member list
        let member_list = members_lock.values().cloned().collect();
        
        let sync_msg = Message {
            message_type: MessageType::Sync,
            sender_id: "".to_string(), // Not important for sync
            sender_addr: "".to_string(),
            target_id: None,
            incarnation: 0,
            members: Some(member_list),
        };
        
        let encoded = bincode::serialize(&sync_msg)?;
        let target_addr = format!("{}:{}", sender_addr, config.bind_port);
        socket.send_to(&encoded, target_addr).await?;
        
        println!("Processed join request from {}", sender_id);
        Ok(())
    }
    
    // Process leave notification
    async fn process_leave(
        sender_id: &str,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        let mut members_lock = members.lock().await;
        
        if let Some(member) = members_lock.get_mut(sender_id) {
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs();
                
            member.state = NodeState::Left;
            member.last_state_change = now;
            
            // Clone for event
            let member_clone = member.clone();
            
            // Send event
            let _ = event_tx
                .send(MembershipEvent::MemberLeft(member_clone))
                .await;
                
            println!("Member {} has left the cluster", sender_id);
        }
        
        Ok(())
    }
    
    // Get current members list
    // Public method to process a message (used in tests)
    pub async fn process_message(&self, msg: Message) -> Result<()> {
        // Create a dummy socket address for testing
        let src = "127.0.0.1:7946".parse::<SocketAddr>().unwrap();
        
        Self::handle_message(
            msg,
            src,
            &self.node_id,
            &self.addr,
            &self.members,
            &self.incarnation,
            &self.suspect_timers,
            &self.reintegration_timers,
            &self.socket,
            &self.config,
            &self.event_tx,
            &self.state_store,
            &self.state_versions,
            &self.is_leader,
            &self.quorum_size,
            &self.partition_detected,
        ).await
    }

    async fn get_members_list(members: &Arc<Mutex<HashMap<String, Member>>>) -> Vec<Member> {
        let members_lock = members.lock().await;
        members_lock.values().cloned().collect()
    }
    
    // Get list of alive members
    pub async fn get_alive_members(&self) -> Vec<Member> {
        let members_lock = self.members.lock().await;
        members_lock
            .values()
            .filter(|m| m.state == NodeState::Alive)
            .cloned()
            .collect()
    }
    
    // Get list of all members
    pub async fn get_all_members(&self) -> Vec<Member> {
        let members_lock = self.members.lock().await;
        members_lock.values().cloned().collect()
    }
    
    // Join an existing cluster
    pub async fn join_cluster(&self, seed_addr: &str) -> Result<()> {
        println!("Joining cluster through seed node {}", seed_addr);
        
        // Create join message
        let inc = *self.incarnation.lock().await;
        let join_msg = Message {
            message_type: MessageType::Join,
            sender_id: self.node_id.clone(),
            sender_addr: self.addr.clone(),
            target_id: None,
            incarnation: inc,
            members: None,
        };
        
        // Send join message to seed node
        let encoded = bincode::serialize(&join_msg)?;
        let seed_addr_full = format!("{}:{}", seed_addr, self.config.bind_port);
        self.socket.send_to(&encoded, &seed_addr_full).await?;
        
        println!("Join message sent to {}", seed_addr_full);
        Ok(())
    }
    
    // Leave the cluster
    pub async fn leave_cluster(&self) -> Result<()> {
        println!("Leaving cluster");
        
        // Increment incarnation
        let mut inc_lock = self.incarnation.lock().await;
        *inc_lock += 1;
        let inc = *inc_lock;
        
        // Create leave message
        let leave_msg = Message {
            message_type: MessageType::Leave,
            sender_id: self.node_id.clone(),
            sender_addr: self.addr.clone(),
            target_id: None,
            incarnation: inc,
            members: None,
        };
        
        // Send leave message to all alive members
        let encoded = bincode::serialize(&leave_msg)?;
        
        let members = self.get_alive_members().await;
        for member in members {
            if member.id != self.node_id {
                let addr = format!("{}:{}", member.address, self.config.bind_port);
                self.socket.send_to(&encoded, addr).await?;
            }
        }
        
        // Update own state
        let mut members_lock = self.members.lock().await;
        if let Some(self_member) = members_lock.get_mut(&self.node_id) {
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs();
                
            self_member.state = NodeState::Left;
            self_member.last_state_change = now;
        }
        
        // Send shutdown signal
        let _ = self.shutdown_tx.send(()).await;
        
        println!("Left cluster");
        Ok(())
    }
    
    // Get event receiver for membership events
    pub async fn get_event_receiver(&self) -> tokio::sync::mpsc::Receiver<MembershipEvent> {
        let (tx, rx) = tokio::sync::mpsc::channel(100);
        
        // Clone the necessary data to avoid capturing self in the closure
        let event_rx = self.event_rx.clone();
        
        // Spawn a task to forward events from the internal receiver to the new one
        tokio::spawn(async move {
            let mut internal_rx = event_rx.lock().await;
            while let Some(event) = internal_rx.recv().await {
                if tx.send(event.clone()).await.is_err() {
                    break;
                }
            }
        });
        
        rx
    }

    // Extract common event emission
    async fn emit_member_event(&self, event: MembershipEvent) -> Result<()> {
        self.event_tx.send(event).await.map_err(|e| anyhow::anyhow!(e))
    }

    // Add a member to the cluster (used in tests)
    pub async fn add_member(&self, member: Member) -> Result<()> {
        let mut members_lock = self.members.lock().await;
        members_lock.insert(member.id.clone(), member.clone());
        
        // If the member is suspected, set up a timer
        if member.state == NodeState::Suspected {
            let mut suspect_timers_lock = self.suspect_timers.lock().await;
            suspect_timers_lock.insert(
                member.id.clone(),
                Instant::now() + self.config.protocol_period * self.config.suspicion_mult,
            );
        }
        
        Ok(())
    }
    
    // Manually suspect a member (used in tests)
    pub async fn suspect_member(&self, member_id: &str) -> Result<()> {
        Self::suspect_member(
            member_id,
            &self.members,
            &self.incarnation,
            &self.suspect_timers,
            &self.config,
            &self.event_tx,
        ).await
    }

    async fn update_member_state(&self, member: &mut Member, new_state: NodeState) -> Result<()> {
        let old_state = member.state.clone();
        if old_state == new_state {
            return Ok(());
        }

        member.state = new_state.clone();
        member.last_state_change = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)?
            .as_secs();

        // Emit corresponding event
        let event = match new_state {
            NodeState::Alive => MembershipEvent::MemberAlive(member.clone()),
            NodeState::Suspected => MembershipEvent::MemberSuspected(member.clone()),
            NodeState::Dead => MembershipEvent::MemberDead(member.clone()),
            NodeState::Left => MembershipEvent::MemberLeft(member.clone()),
            NodeState::Maintenance => MembershipEvent::MemberMaintenance(member.clone()),
        };
        self.emit_member_event(event).await
    }

    async fn manage_suspect_timer(&self, member_id: &str, is_suspected: bool) {
        let mut timers = self.suspect_timers.lock().await;
        if is_suspected {
            timers.insert(
                member_id.to_string(),
                Instant::now() + self.config.protocol_period * self.config.suspicion_mult,
            );
        } else {
            timers.remove(member_id);
        }
    }

    // Check reintegration timers and allow nodes to rejoin if timeout
    async fn check_reintegration_timers(
        members: &Arc<Mutex<HashMap<String, Member>>>,
        reintegration_timers: &Arc<Mutex<HashMap<String, Instant>>>,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        let now = Instant::now();
        let mut expired = Vec::new();
        
        // Find expired timers
        {
            let reintegration_timers_lock = reintegration_timers.lock().await;
            for (id, deadline) in reintegration_timers_lock.iter() {
                if now >= *deadline {
                    expired.push(id.clone());
                }
            }
        }
        
        // Process expired timers
        if !expired.is_empty() {
            let mut reintegration_timers_lock = reintegration_timers.lock().await;
            
            for id in expired {
                // Remove timer
                reintegration_timers_lock.remove(&id);
                println!("Node {} is now allowed to reintegrate", id);
            }
        }
        
        Ok(())
    }
    
    // Gossip dissemination - spread membership information to random nodes
    async fn gossip_dissemination(
        node_id: &str,
        addr: &str,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        incarnation: &Arc<Mutex<u64>>,
        socket: &Arc<UdpSocket>,
        config: &MembershipConfig,
    ) -> Result<()> {
        // Get list of alive members
        let targets = {
            let members_lock = members.lock().await;
            let alive_members: Vec<_> = members_lock
                .iter()
                .filter(|(id, m)| *id != node_id && m.state == NodeState::Alive)
                .map(|(_, m)| m.clone())
                .collect();
                
            if alive_members.is_empty() {
                return Ok(());
            }
            
            // Select k random members for gossip
            let mut selected = Vec::new();
            let mut indices = HashSet::new();
            let k = std::cmp::min(config.gossip_factor, alive_members.len());
            
            while selected.len() < k {
                let idx = rand::random::<usize>() % alive_members.len();
                if indices.insert(idx) {
                    selected.push(alive_members[idx].clone());
                }
            }
            
            selected
        };
        
        // Get list of members with recent state changes
        let member_updates = {
            let members_lock = members.lock().await;
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs();
                
            let mut updates: Vec<_> = members_lock
                .values()
                .filter(|m| now - m.last_state_change < 60) // Members with changes in the last minute
                .cloned()
                .collect();
                
            // Limit the number of updates to avoid large messages
            if updates.len() > config.max_gossip_updates {
                updates.sort_by(|a, b| b.last_state_change.cmp(&a.last_state_change));
                updates.truncate(config.max_gossip_updates);
            }
            
            updates
        };
        
        if member_updates.is_empty() {
            return Ok(());
        }
        
        // Send gossip to selected targets
        let inc = *incarnation.lock().await;
        for target in targets {
            let sync_msg = Message {
                message_type: MessageType::Sync,
                sender_id: node_id.to_string(),
                sender_addr: addr.to_string(),
                target_id: Some(target.id.clone()),
                incarnation: inc,
                members: Some(member_updates.clone()),
            };
            
            let encoded = bincode::serialize(&sync_msg)?;
            let target_addr = format!("{}:{}", target.address, config.bind_port);
            socket.send_to(&encoded, &target_addr).await?;
        }
        
        Ok(())
    }
    
    // Check if we have quorum
    async fn check_quorum(
        members: &Arc<Mutex<HashMap<String, Member>>>,
        quorum_size: &Arc<Mutex<usize>>,
        partition_detected: &Arc<Mutex<bool>>,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        let members_lock = members.lock().await;
        let alive_count = members_lock
            .values()
            .filter(|m| m.state == NodeState::Alive)
            .count();
            
        let mut quorum_size_lock = quorum_size.lock().await;
        let mut partition_detected_lock = partition_detected.lock().await;
        
        // Update quorum size if needed (should be majority of all members)
        let total_members = members_lock.len();
        let new_quorum_size = (total_members as f64 * 0.51).ceil() as usize;
        
        if *quorum_size_lock != new_quorum_size {
            *quorum_size_lock = new_quorum_size;
        }
        
        // Check if we have quorum
        let has_quorum = alive_count >= *quorum_size_lock;
        
        // Handle partition detection
        if !has_quorum && !*partition_detected_lock {
            *partition_detected_lock = true;
            let _ = event_tx.send(MembershipEvent::QuorumLost).await;
            let _ = event_tx.send(MembershipEvent::NetworkPartitionDetected).await;
            println!("Network partition detected! Quorum lost.");
        } else if has_quorum && *partition_detected_lock {
            *partition_detected_lock = false;
            let _ = event_tx.send(MembershipEvent::QuorumRestored).await;
            let _ = event_tx.send(MembershipEvent::NetworkPartitionResolved).await;
            println!("Network partition resolved! Quorum restored.");
        }
        
        Ok(())
    }
    
    // Check leader status and elect if needed
    async fn check_leader_status(
        node_id: &str,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        is_leader: &Arc<Mutex<bool>>,
        socket: &Arc<UdpSocket>,
        config: &MembershipConfig,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();
            
        // Check if we have a leader
        let (has_leader, self_is_leader, need_election) = {
            let mut members_lock = members.lock().await;
            
            // Find current leader
            let leader = members_lock.values().find(|m| m.is_leader && m.state == NodeState::Alive);
            let has_leader = leader.is_some();
            
            // Check if we are the leader
            let self_member = members_lock.get_mut(node_id);
            let self_is_leader = if let Some(member) = self_member {
                member.last_leader_check = now;
                member.is_leader
            } else {
                false
            };
            
            // Determine if we need an election
            let need_election = !has_leader;
            
            (has_leader, self_is_leader, need_election)
        };
        
        // Update our leader status
        {
            let mut is_leader_lock = is_leader.lock().await;
            *is_leader_lock = self_is_leader;
        }
        
        // If we need an election and we're the oldest node, elect ourselves
        if need_election {
            let should_be_leader = {
                let members_lock = members.lock().await;
                
                // Find oldest alive node by incarnation number and node ID as tiebreaker
                // This ensures a deterministic leader election even when incarnation numbers are the same
                let oldest = members_lock.values()
                    .filter(|m| m.state == NodeState::Alive)
                    .min_by(|a, b| {
                        if a.incarnation == b.incarnation {
                            a.id.cmp(&b.id) // Use node ID as tiebreaker
                        } else {
                            a.incarnation.cmp(&b.incarnation)
                        }
                    });
                    
                if let Some(oldest) = oldest {
                    oldest.id == node_id
                } else {
                    false
                }
            };
            
            if should_be_leader {
                // Elect ourselves as leader
                {
                    let mut members_lock = members.lock().await;
                    if let Some(self_member) = members_lock.get_mut(node_id) {
                        self_member.is_leader = true;
                        self_member.last_leader_check = now;
                        
                        // Update our leader status
                        let mut is_leader_lock = is_leader.lock().await;
                        *is_leader_lock = true;
                        
                        // Send event
                        let _ = event_tx
                            .send(MembershipEvent::LeaderElected(self_member.clone()))
                            .await;
                            
                        println!("Node {} elected as leader", node_id);
                    }
                }
                
                // Announce leadership to all nodes
                let members_list = Self::get_members_list(members).await;
                let leader_msg = Message {
                    message_type: MessageType::LeaderAnnounce,
                    sender_id: node_id.to_string(),
                    sender_addr: "".to_string(),
                    target_id: None,
                    incarnation: 0,
                    members: Some(members_list),
                };
                
                let encoded = bincode::serialize(&leader_msg)?;
                
                // Send to all alive members
                let alive_members = {
                    let members_lock = members.lock().await;
                    members_lock.values()
                        .filter(|m| m.state == NodeState::Alive && m.id != node_id)
                        .cloned()
                        .collect::<Vec<_>>()
                };
                
                for member in alive_members {
                    let addr = format!("{}:{}", member.address, config.bind_port);
                    socket.send_to(&encoded, addr).await?;
                }
            }
        }
        
        Ok(())
    }
    
    // Process leader election message
    async fn process_leader_election(
        sender_id: &str,
        sender_addr: &str,
        sender_incarnation: u64,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        incarnation: &Arc<Mutex<u64>>,
        socket: &Arc<UdpSocket>,
        config: &MembershipConfig,
        is_leader: &Arc<Mutex<bool>>,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        // Check if we should be the leader instead
        let our_incarnation = *incarnation.lock().await;
        let our_id = members.lock().await.values()
            .find(|m| m.is_leader)
            .map(|m| m.id.clone());
            
        // Compare incarnation numbers, or node IDs if incarnations are equal
        let sender_should_be_leader = if our_incarnation == sender_incarnation {
            // If incarnations are equal, compare node IDs lexicographically
            if let Some(our_id) = our_id {
                sender_id < &our_id
            } else {
                true // If we don't have a leader ID, let sender be leader
            }
        } else {
            our_incarnation < sender_incarnation
        };
        
        if sender_should_be_leader {
            // The sender has a higher incarnation, they should be leader
            let mut members_lock = members.lock().await;
            
            // Update sender as leader
            if let Some(member) = members_lock.get_mut(sender_id) {
                member.is_leader = true;
                member.incarnation = sender_incarnation;
                
                // Update our leader status
                let mut is_leader_lock = is_leader.lock().await;
                *is_leader_lock = false;
                
                // Send event
                let _ = event_tx
                    .send(MembershipEvent::LeaderElected(member.clone()))
                    .await;
                    
                println!("Node {} elected as leader", sender_id);
            } else {
                // Add the sender as a new member
                let now = std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_secs();
                    
                let new_member = Member {
                    id: sender_id.to_string(),
                    address: sender_addr.to_string(),
                    state: NodeState::Alive,
                    incarnation: sender_incarnation,
                    last_state_change: now,
                    is_leader: true,
                    last_leader_check: now,
                    metadata: Some(HashMap::new()),
                };
                
                members_lock.insert(sender_id.to_string(), new_member.clone());
                
                // Update our leader status
                let mut is_leader_lock = is_leader.lock().await;
                *is_leader_lock = false;
                
                // Send event
                let _ = event_tx
                    .send(MembershipEvent::LeaderElected(new_member))
                    .await;
                    
                println!("Node {} elected as leader", sender_id);
            }
        } else {
            // We have a higher incarnation, we should be leader
            // Send a leader announce message
            let members_list = Self::get_members_list(members).await;
            let leader_msg = Message {
                message_type: MessageType::LeaderAnnounce,
                sender_id: "".to_string(),
                sender_addr: "".to_string(),
                target_id: Some(sender_id.to_string()),
                incarnation: our_incarnation,
                members: Some(members_list),
            };
            
            let encoded = bincode::serialize(&leader_msg)?;
            let target_addr = format!("{}:{}", sender_addr, config.bind_port);
            socket.send_to(&encoded, target_addr).await?;
        }
        
        Ok(())
    }
    
    // Process leader announcement
    async fn process_leader_announce(
        sender_id: &str,
        sender_incarnation: u64,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        is_leader: &Arc<Mutex<bool>>,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        let mut members_lock = members.lock().await;
        
        // Update all members to not be leader
        for (_, member) in members_lock.iter_mut() {
            member.is_leader = false;
        }
        
        // Update sender as leader
        if let Some(member) = members_lock.get_mut(sender_id) {
            member.is_leader = true;
            member.incarnation = sender_incarnation;
            
            // Update our leader status
            let mut is_leader_lock = is_leader.lock().await;
            *is_leader_lock = false;
            
            // Send event
            let _ = event_tx
                .send(MembershipEvent::LeaderElected(member.clone()))
                .await;
                
            println!("Node {} announced as leader", sender_id);
        }
        
        Ok(())
    }
    
    // Process maintenance mode request
    async fn process_maintenance(
        sender_id: &str,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        let mut members_lock = members.lock().await;
        
        if let Some(member) = members_lock.get_mut(sender_id) {
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs();
                
            member.state = NodeState::Maintenance;
            member.last_state_change = now;
            
            // Clone for event
            let member_clone = member.clone();
            
            // Send event
            let _ = event_tx
                .send(MembershipEvent::MemberMaintenance(member_clone))
                .await;
                
            println!("Member {} is now in maintenance mode", sender_id);
        }
        
        Ok(())
    }
    
    // Process reintegration request
    async fn process_reintegration(
        sender_id: &str,
        sender_addr: &str,
        members: &Arc<Mutex<HashMap<String, Member>>>,
        reintegration_timers: &Arc<Mutex<HashMap<String, Instant>>>,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        // Check if node is allowed to reintegrate
        let can_reintegrate = {
            let reintegration_timers_lock = reintegration_timers.lock().await;
            !reintegration_timers_lock.contains_key(sender_id)
        };
        
        if can_reintegrate {
            let mut members_lock = members.lock().await;
            
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs();
                
            if let Some(member) = members_lock.get_mut(sender_id) {
                // Update existing member
                member.state = NodeState::Alive;
                member.address = sender_addr.to_string();
                member.last_state_change = now;
                
                // Clone for event
                let member_clone = member.clone();
                
                // Send event
                let _ = event_tx
                    .send(MembershipEvent::MemberAlive(member_clone))
                    .await;
                    
                println!("Member {} has reintegrated", sender_id);
            } else {
                // Add as new member
                let new_member = Member {
                    id: sender_id.to_string(),
                    address: sender_addr.to_string(),
                    state: NodeState::Alive,
                    incarnation: 1,
                    last_state_change: now,
                    is_leader: false,
                    last_leader_check: now,
                    metadata: Some(HashMap::new()),
                };
                
                members_lock.insert(sender_id.to_string(), new_member.clone());
                
                // Send event
                let _ = event_tx
                    .send(MembershipEvent::MemberJoined(new_member))
                    .await;
                    
                println!("Member {} has joined through reintegration", sender_id);
            }
        }
        
        Ok(())
    }
    
    // Process state synchronization
    async fn process_state_sync(
        sender_id: &str,
        member_list: Vec<Member>,
        state_store: &Arc<Mutex<HashMap<String, Vec<u8>>>>,
        state_versions: &Arc<Mutex<HashMap<String, u64>>>,
        socket: &Arc<UdpSocket>,
        src: SocketAddr,
    ) -> Result<()> {
        // Process member list to extract state information
        for member in member_list {
            if let Some(metadata) = &member.metadata {
                for (key, value) in metadata {
                    if key.starts_with("state:") {
                        let state_key = key.trim_start_matches("state:");
                        let version_key = format!("version:{}", state_key);
                        
                        if let Some(version_str) = metadata.get(&version_key) {
                            if let Ok(version) = version_str.parse::<u64>() {
                                // Check if we have a newer version
                                let should_update = {
                                    let versions_lock = state_versions.lock().await;
                                    !versions_lock.contains_key(state_key) ||
                                    versions_lock.get(state_key).unwrap() < &version
                                };
                                
                                if should_update {
                                    // Update our state
                                    let mut state_store_lock = state_store.lock().await;
                                    let mut versions_lock = state_versions.lock().await;
                                    
                                    state_store_lock.insert(state_key.to_string(), value.as_bytes().to_vec());
                                    versions_lock.insert(state_key.to_string(), version);
                                    
                                    println!("Updated state {} to version {}", state_key, version);
                                }
                            }
                        }
                    }
                }
            }
        }
        
        // Send acknowledgment
        let ack_msg = Message {
            message_type: MessageType::Ack,
            sender_id: "".to_string(),
            sender_addr: "".to_string(),
            target_id: Some(sender_id.to_string()),
            incarnation: 0,
            members: None,
        };
        
        let encoded = bincode::serialize(&ack_msg)?;
        socket.send_to(&encoded, src).await?;
        
        Ok(())
    }
    
    // Enter maintenance mode
    pub async fn enter_maintenance_mode(&self) -> Result<()> {
        println!("Entering maintenance mode");
        
        // Increment incarnation
        let mut inc_lock = self.incarnation.lock().await;
        *inc_lock += 1;
        let inc = *inc_lock;
        
        // Create maintenance message
        let maintenance_msg = Message {
            message_type: MessageType::Maintenance,
            sender_id: self.node_id.clone(),
            sender_addr: self.addr.clone(),
            target_id: None,
            incarnation: inc,
            members: None,
        };
        
        // Send maintenance message to all alive members
        let encoded = bincode::serialize(&maintenance_msg)?;
        
        let members = self.get_alive_members().await;
        for member in members {
            if member.id != self.node_id {
                let addr = format!("{}:{}", member.address, self.config.bind_port);
                self.socket.send_to(&encoded, addr).await?;
            }
        }
        
        // Update own state
        let mut members_lock = self.members.lock().await;
        if let Some(self_member) = members_lock.get_mut(&self.node_id) {
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs();
                
            self_member.state = NodeState::Maintenance;
            self_member.last_state_change = now;
            
            // Send event
            let _ = self.event_tx
                .send(MembershipEvent::MemberMaintenance(self_member.clone()))
                .await;
        }
        
        println!("Entered maintenance mode");
        Ok(())
    }
    
    // Exit maintenance mode
    pub async fn exit_maintenance_mode(&self) -> Result<()> {
        println!("Exiting maintenance mode");
        
        // Increment incarnation
        let mut inc_lock = self.incarnation.lock().await;
        *inc_lock += 1;
        let inc = *inc_lock;
        
        // Update own state
        let mut members_lock = self.members.lock().await;
        if let Some(self_member) = members_lock.get_mut(&self.node_id) {
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs();
                
            self_member.state = NodeState::Alive;
            self_member.last_state_change = now;
            
            // Send event
            let _ = self.event_tx
                .send(MembershipEvent::MemberAlive(self_member.clone()))
                .await;
        }
        
        // Rejoin the cluster
        self.join_cluster(&self.addr).await?;
        
        println!("Exited maintenance mode");
        Ok(())
    }
    
    // Store distributed state
    pub async fn store_state(&self, key: &str, value: &[u8]) -> Result<()> {
        // Check if we're the leader - only leader can write state
        let is_leader = *self.is_leader.lock().await;
        
        if !is_leader {
            // If we're not the leader, forward the request to the leader
            let leader_addr = self.find_leader_address().await;
            
            if let Some(leader_addr) = leader_addr {
                // Create a state sync message to forward to the leader
                let state_member = Member {
                    id: format!("state:{}", key),
                    address: "".to_string(),
                    state: NodeState::Alive,
                    incarnation: 0, // Leader will assign the version
                    last_state_change: std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .unwrap()
                        .as_secs(),
                    is_leader: false,
                    last_leader_check: 0,
                    metadata: {
                        let mut metadata = HashMap::new();
                        metadata.insert(format!("state:{}", key), String::from_utf8_lossy(value).to_string());
                        metadata.insert(format!("version:{}", key), "0".to_string()); // Leader will assign version
                        Some(metadata)
                    },
                };
                
                let sync_msg = Message {
                    message_type: MessageType::StateSync,
                    sender_id: self.node_id.clone(),
                    sender_addr: self.addr.clone(),
                    target_id: None,
                    incarnation: *self.incarnation.lock().await,
                    members: Some(vec![state_member]),
                };
                
                let encoded = bincode::serialize(&sync_msg)?;
                let target_addr = format!("{}:{}", leader_addr, self.config.bind_port);
                self.socket.send_to(&encoded, target_addr).await?;
                
                // Wait for acknowledgment or timeout
                // In a real implementation, we would wait for an ack
                // For now, we'll just return success
                return Ok(());
            } else {
                return Err(anyhow::anyhow!("No leader available to store state"));
            }
        }
        
        // We are the leader, proceed with state storage
        let mut state_store_lock = self.state_store.lock().await;
        let mut versions_lock = self.state_versions.lock().await;
        
        // Get current version and increment
        let version = versions_lock.get(key).unwrap_or(&0) + 1;
        
        // Store state and version
        state_store_lock.insert(key.to_string(), value.to_vec());
        versions_lock.insert(key.to_string(), version);
        
        // Update metadata in member info
        let mut members_lock = self.members.lock().await;
        if let Some(self_member) = members_lock.get_mut(&self.node_id) {
            if self_member.metadata.is_none() {
                self_member.metadata = Some(HashMap::new());
            }
            
            if let Some(metadata) = &mut self_member.metadata {
                metadata.insert(format!("state:{}", key), String::from_utf8_lossy(value).to_string());
                metadata.insert(format!("version:{}", key), version.to_string());
            }
        }
        
        // Propagate state through gossip
        self.gossip_state(key, value, version).await?;
        
        Ok(())
    }
    
    // Find the address of the current leader
    async fn find_leader_address(&self) -> Option<String> {
        let members_lock = self.members.lock().await;
        
        for member in members_lock.values() {
            if member.is_leader && member.state == NodeState::Alive {
                return Some(member.address.clone());
            }
        }
        
        None
    }
    
    // Get distributed state
    pub async fn get_state(&self, key: &str) -> Option<Vec<u8>> {
        // First check local cache
        let state_store_lock = self.state_store.lock().await;
        let local_state = state_store_lock.get(key).cloned();
        
        if local_state.is_some() {
            return local_state;
        }
        
        // If not in local cache and we're not the leader, try to get from leader
        let is_leader = *self.is_leader.lock().await;
        if !is_leader {
            let leader_addr = self.find_leader_address().await;
            
            if let Some(leader_addr) = leader_addr {
                // In a real implementation, we would send a request to the leader
                // and wait for a response. For now, we'll just return None.
                // This would be implemented with a StateRequest message type.
            }
        }
        
        None
    }
    
    // Gossip state to other nodes
    async fn gossip_state(&self, key: &str, value: &[u8], version: u64) -> Result<()> {
        // Create metadata with state
        let mut metadata = HashMap::new();
        metadata.insert(format!("state:{}", key), String::from_utf8_lossy(value).to_string());
        metadata.insert(format!("version:{}", key), version.to_string());
        
        // Create a temporary member with the state
        let state_member = Member {
            id: format!("state:{}", key),
            address: "".to_string(),
            state: NodeState::Alive,
            incarnation: version,
            last_state_change: std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs(),
            is_leader: false,
            last_leader_check: 0,
            metadata: Some(metadata),
        };
        
        // Create sync message with state
        let sync_msg = Message {
            message_type: MessageType::StateSync,
            sender_id: self.node_id.clone(),
            sender_addr: self.addr.clone(),
            target_id: None,
            incarnation: *self.incarnation.lock().await,
            members: Some(vec![state_member]),
        };
        
        // Send to all alive members
        let encoded = bincode::serialize(&sync_msg)?;
        
        let members = self.get_alive_members().await;
        for member in members {
            if member.id != self.node_id {
                let addr = format!("{}:{}", member.address, self.config.bind_port);
                self.socket.send_to(&encoded, addr).await?;
            }
        }
        
        Ok(())
    }
    
    // Get the current leader
    pub async fn get_leader(&self) -> Option<Member> {
        let members_lock = self.members.lock().await;
        
        for member in members_lock.values() {
            if member.is_leader && member.state == NodeState::Alive {
                return Some(member.clone());
            }
        }
        
        None
    }
        
    // Check if this node is the leader
    pub async fn is_leader(&self) -> bool {
        *self.is_leader.lock().await
    }
    
    // Force a leader election
    pub async fn trigger_leader_election(&self) -> Result<()> {
        // Increment incarnation to increase chances of winning
        let mut inc_lock = self.incarnation.lock().await;
        *inc_lock += 1;
        let inc = *inc_lock;
            
            // Create leader election message
            let election_msg = Message {
                message_type: MessageType::LeaderElection,
                sender_id: self.node_id.clone(),
                sender_addr: self.addr.clone(),
                target_id: None,
                incarnation: inc,
                members: None,
            };
            
            // Send to all alive members
            let encoded = bincode::serialize(&election_msg)?;
            
            let members = self.get_alive_members().await;
            for member in members {
                if member.id != self.node_id {
                    let addr = format!("{}:{}", member.address, self.config.bind_port);
                    self.socket.send_to(&encoded, addr).await?;
                }
            }
            
            // Check leader status immediately
            Self::check_leader_status(
                &self.node_id,
                &self.members,
                &self.is_leader,
                &self.socket,
                &self.config,
                &self.event_tx,
            ).await?;
            
            Ok(())
        }
    }
