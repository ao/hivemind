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

// Node states in the membership protocol
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub enum NodeState {
    Alive,
    Suspected,
    Dead,
    Left,
}

// Member represents a node in the cluster
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Member {
    pub id: String,
    pub address: String,
    pub state: NodeState,
    pub incarnation: u64,
    pub last_state_change: u64,
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
    config: MembershipConfig,
    socket: Arc<UdpSocket>,
    event_tx: tokio::sync::mpsc::Sender<MembershipEvent>,
    event_rx: Arc<Mutex<tokio::sync::mpsc::Receiver<MembershipEvent>>>,
    shutdown_tx: tokio::sync::mpsc::Sender<()>,
    shutdown_rx: Arc<Mutex<tokio::sync::mpsc::Receiver<()>>>,
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
            
        members.insert(
            node_id.clone(),
            Member {
                id: node_id.clone(),
                address: addr.clone(),
                state: NodeState::Alive,
                incarnation: 1,
                last_state_change: now,
            },
        );
        
        Ok(Self {
            node_id,
            addr,
            incarnation: Arc::new(Mutex::new(1)),
            members: Arc::new(Mutex::new(members)),
            suspect_timers: Arc::new(Mutex::new(HashMap::new())),
            config,
            socket: Arc::new(socket),
            event_tx,
            event_rx: Arc::new(Mutex::new(event_rx)),
            shutdown_tx,
            shutdown_rx: Arc::new(Mutex::new(shutdown_rx)),
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
        let socket = self.socket.clone();
        let config = self.config.clone();
        let event_tx = self.event_tx.clone();
        let shutdown_rx = self.shutdown_rx.clone();
        
        // Start the main protocol loop
        tokio::spawn(async move {
            if let Err(e) = Self::run_protocol(
                node_id,
                addr,
                members,
                incarnation,
                suspect_timers,
                socket,
                config,
                event_tx,
                shutdown_rx,
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
        socket: Arc<UdpSocket>,
        config: MembershipConfig,
        event_tx: tokio::sync::mpsc::Sender<MembershipEvent>,
        shutdown_rx: Arc<Mutex<tokio::sync::mpsc::Receiver<()>>>,
    ) -> Result<()> {
        // Set up protocol period ticker
        let protocol_period = time::interval(config.protocol_period);
        tokio::pin!(protocol_period);
        
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
                                    &socket,
                                    &config,
                                    &event_tx,
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
        socket: &Arc<UdpSocket>,
        config: &MembershipConfig,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
    ) -> Result<()> {
        // Check suspect timers
        Self::check_suspect_timers(members, suspect_timers, config, event_tx).await?;
        
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
        socket: &Arc<UdpSocket>,
        config: &MembershipConfig,
        event_tx: &tokio::sync::mpsc::Sender<MembershipEvent>,
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
}
