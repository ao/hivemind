use anyhow::Result;
use hivemind::network::{NetworkManager, NetworkConfig, NetworkPolicy, NetworkSelector, NetworkRule, PortRange, Protocol, NetworkPeer};
use hivemind::node::NodeManager;
use hivemind::security::rbac::RbacManager;
use hivemind::service_discovery::ServiceDiscovery;
use hivemind::tenant::{TenantManager, Tenant, IsolationLevel, ResourceQuotas, TenantStatus, TenantContext};
use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr};
use std::sync::Arc;
use chrono::Utc;

#[tokio::test]
async fn test_tenant_network_isolation() -> Result<()> {
    // Setup
    let node_manager = Arc::new(NodeManager::new());
    let service_discovery = Arc::new(ServiceDiscovery::new());
    let rbac_manager = Arc::new(RbacManager::new());
    let tenant_manager = Arc::new(TenantManager::new(rbac_manager));
    
    // Create network manager with tenant manager
    let network_manager = NetworkManager::new(
        node_manager.clone(),
        service_discovery.clone(),
        None, // Use default network config
    ).await?;
    
    // Create test tenants
    let tenant1_id = "tenant1".to_string();
    let tenant1 = Tenant {
        id: tenant1_id.clone(),
        name: "Tenant 1".to_string(),
        isolation_level: IsolationLevel::NetworkOnly,
        resource_quotas: ResourceQuotas {
            cpu_limit: Some(2),
            memory_limit: Some(2 * 1024 * 1024 * 1024), // 2 GB
            storage_limit: Some(10 * 1024 * 1024 * 1024), // 10 GB
            max_containers: Some(5),
            max_services: Some(3),
        },
        namespaces: vec!["tenant1".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "user1".to_string(),
        description: Some("Test tenant 1".to_string()),
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    let tenant2_id = "tenant2".to_string();
    let tenant2 = Tenant {
        id: tenant2_id.clone(),
        name: "Tenant 2".to_string(),
        isolation_level: IsolationLevel::NetworkOnly,
        resource_quotas: ResourceQuotas {
            cpu_limit: Some(2),
            memory_limit: Some(2 * 1024 * 1024 * 1024), // 2 GB
            storage_limit: Some(10 * 1024 * 1024 * 1024), // 10 GB
            max_containers: Some(5),
            max_services: Some(3),
        },
        namespaces: vec!["tenant2".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "user2".to_string(),
        description: Some("Test tenant 2".to_string()),
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    // Create the tenants
    tenant_manager.create_tenant(tenant1).await?;
    tenant_manager.create_tenant(tenant2).await?;
    
    // Initialize tenant networks
    network_manager.initialize_tenant_network(&tenant1_id, Some("10.100.0.0/16".to_string())).await?;
    network_manager.initialize_tenant_network(&tenant2_id, Some("10.200.0.0/16".to_string())).await?;
    
    // Create tenant contexts
    let tenant1_context = TenantContext::new(tenant1_id.clone(), "user1".to_string());
    let tenant2_context = TenantContext::new(tenant2_id.clone(), "user2".to_string());
    
    // Create network policy for tenant1
    let mut selector_labels = HashMap::new();
    selector_labels.insert("tenant".to_string(), tenant1_id.clone());
    
    let mut policy_labels = HashMap::new();
    policy_labels.insert("tenant".to_string(), tenant1_id.clone());
    
    let tenant1_policy = NetworkPolicy {
        name: format!("{}-isolation", tenant1_id),
        selector: NetworkSelector {
            labels: selector_labels.clone(),
        },
        ingress_rules: vec![
            NetworkRule {
                ports: vec![
                    PortRange {
                        protocol: Protocol::TCP,
                        port_min: 1,
                        port_max: 65535,
                    },
                ],
                from: vec![
                    NetworkPeer {
                        ip_block: None,
                        selector: Some(NetworkSelector {
                            labels: selector_labels.clone(),
                        }),
                    },
                ],
                action: None,
                log: false,
                description: Some("Allow traffic from same tenant".to_string()),
                id: None,
            },
        ],
        egress_rules: vec![],
        priority: 100,
        namespace: Some(tenant1_id.clone()),
        tenant_id: Some(tenant1_id.clone()),
        labels: policy_labels.clone(),
        created_at: Utc::now().timestamp() as u64,
        updated_at: Utc::now().timestamp() as u64,
    };
    
    // Create network policy for tenant2
    let mut selector_labels2 = HashMap::new();
    selector_labels2.insert("tenant".to_string(), tenant2_id.clone());
    
    let mut policy_labels2 = HashMap::new();
    policy_labels2.insert("tenant".to_string(), tenant2_id.clone());
    
    let tenant2_policy = NetworkPolicy {
        name: format!("{}-isolation", tenant2_id),
        selector: NetworkSelector {
            labels: selector_labels2.clone(),
        },
        ingress_rules: vec![
            NetworkRule {
                ports: vec![
                    PortRange {
                        protocol: Protocol::TCP,
                        port_min: 1,
                        port_max: 65535,
                    },
                ],
                from: vec![
                    NetworkPeer {
                        ip_block: None,
                        selector: Some(NetworkSelector {
                            labels: selector_labels2.clone(),
                        }),
                    },
                ],
                action: None,
                log: false,
                description: Some("Allow traffic from same tenant".to_string()),
                id: None,
            },
        ],
        egress_rules: vec![],
        priority: 100,
        namespace: Some(tenant2_id.clone()),
        tenant_id: Some(tenant2_id.clone()),
        labels: policy_labels2.clone(),
        created_at: Utc::now().timestamp() as u64,
        updated_at: Utc::now().timestamp() as u64,
    };
    
    // Apply policies
    let policies = network_manager.policies.lock().await;
    policies.add_policy(tenant1_policy).await?;
    policies.add_policy(tenant2_policy).await?;
    
    // Verify tenant policies
    let tenant1_policies = policies.get_policies_by_tenant(&tenant1_id);
    let tenant2_policies = policies.get_policies_by_tenant(&tenant2_id);
    
    assert_eq!(tenant1_policies.len(), 1, "Tenant 1 should have 1 policy");
    assert_eq!(tenant2_policies.len(), 1, "Tenant 2 should have 1 policy");
    
    // Verify tenant network CIDRs
    let tenant_cidrs = network_manager.tenant_network_cidrs.lock().await;
    assert_eq!(tenant_cidrs.get(&tenant1_id), Some(&"10.100.0.0/16".to_string()), "Tenant 1 should have CIDR 10.100.0.0/16");
    assert_eq!(tenant_cidrs.get(&tenant2_id), Some(&"10.200.0.0/16".to_string()), "Tenant 2 should have CIDR 10.200.0.0/16");
    
    // Verify tenant overlay networks
    let tenant_overlays = network_manager.tenant_overlays.lock().await;
    assert!(tenant_overlays.contains_key(&tenant1_id), "Tenant 1 should have an overlay network");
    assert!(tenant_overlays.contains_key(&tenant2_id), "Tenant 2 should have an overlay network");
    
    Ok(())
}