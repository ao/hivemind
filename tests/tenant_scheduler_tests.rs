use anyhow::Result;
use hivemind::app::AppManager;
use hivemind::containerd_manager::{Container, ContainerStatus, PortMapping, EnvVar, Volume};
use hivemind::node::{NodeManager, NodeResources};
use hivemind::scheduler::{ContainerScheduler, ResourceRequirements, ResourceRequest, ResourceLimit};
use hivemind::security::rbac::RbacManager;
use hivemind::service_discovery::ServiceDiscovery;
use hivemind::tenant::{TenantManager, Tenant, IsolationLevel, ResourceQuotas, TenantStatus};
use std::collections::HashMap;
use std::sync::Arc;
use chrono::Utc;

#[tokio::test]
async fn test_tenant_quota_enforcement() -> Result<()> {
    // Setup
    let app_manager = AppManager::new().await?;
    let node_manager = Arc::new(NodeManager::new());
    let service_discovery = Arc::new(ServiceDiscovery::new());
    let rbac_manager = Arc::new(RbacManager::new());
    let tenant_manager = Arc::new(TenantManager::new(rbac_manager));
    
    // Create a scheduler with tenant manager
    let mut scheduler = ContainerScheduler::new(app_manager)
        .with_node_manager(node_manager)
        .with_service_discovery(service_discovery)
        .with_tenant_manager(tenant_manager.clone());
    
    // Create a test tenant with limited resources
    let tenant_id = "test-tenant".to_string();
    let tenant = Tenant {
        id: tenant_id.clone(),
        name: "Test Tenant".to_string(),
        isolation_level: IsolationLevel::Soft,
        resource_quotas: ResourceQuotas {
            cpu_limit: Some(2),
            memory_limit: Some(2 * 1024 * 1024 * 1024), // 2 GB
            storage_limit: Some(10 * 1024 * 1024 * 1024), // 10 GB
            max_containers: Some(3),
            max_services: Some(2),
        },
        namespaces: vec!["test-namespace".to_string()],
        created_at: Utc::now(),
        updated_at: Utc::now(),
        owner_id: "test-user".to_string(),
        description: Some("Test tenant for unit tests".to_string()),
        labels: HashMap::new(),
        status: TenantStatus::Active,
    };
    
    // Create the tenant
    tenant_manager.create_tenant(tenant).await?;
    
    // Create a container with resource requirements within quota
    let container1 = Container {
        id: "container1".to_string(),
        name: format!("{}-container1", tenant_id),
        image: "test-image:latest".to_string(),
        status: ContainerStatus::Pending,
        node_id: "".to_string(),
        created_at: Utc::now().timestamp(),
        ports: Vec::new(),
        env_vars: Vec::new(),
        volumes: Vec::new(),
        service_domain: None,
    };
    
    let resource_requirements1 = ResourceRequirements {
        requests: ResourceRequest {
            cpu: 1.0,
            memory: 1 * 1024 * 1024 * 1024, // 1 GB
        },
        limits: ResourceLimit {
            cpu: 1.5,
            memory: 1.5 * 1024 * 1024 * 1024, // 1.5 GB
        },
    };
    
    // Check if container1 passes quota check
    let quota_check1 = scheduler.check_tenant_quotas(&tenant_id, &resource_requirements1).await?;
    assert!(quota_check1, "Container1 should pass quota check");
    
    // Update tenant resource usage
    scheduler.update_tenant_resource_usage(&tenant_id, &resource_requirements1, true);
    
    // Create a second container that would exceed CPU quota
    let container2 = Container {
        id: "container2".to_string(),
        name: format!("{}-container2", tenant_id),
        image: "test-image:latest".to_string(),
        status: ContainerStatus::Pending,
        node_id: "".to_string(),
        created_at: Utc::now().timestamp(),
        ports: Vec::new(),
        env_vars: Vec::new(),
        volumes: Vec::new(),
        service_domain: None,
    };
    
    let resource_requirements2 = ResourceRequirements {
        requests: ResourceRequest {
            cpu: 1.5,
            memory: 0.5 * 1024 * 1024 * 1024, // 0.5 GB
        },
        limits: ResourceLimit {
            cpu: 2.0,
            memory: 1.0 * 1024 * 1024 * 1024, // 1 GB
        },
    };
    
    // Check if container2 fails quota check (CPU quota exceeded)
    let quota_check2 = scheduler.check_tenant_quotas(&tenant_id, &resource_requirements2).await?;
    assert!(!quota_check2, "Container2 should fail quota check due to CPU limit");
    
    // Create a third container that would exceed memory quota
    let container3 = Container {
        id: "container3".to_string(),
        name: format!("{}-container3", tenant_id),
        image: "test-image:latest".to_string(),
        status: ContainerStatus::Pending,
        node_id: "".to_string(),
        created_at: Utc::now().timestamp(),
        ports: Vec::new(),
        env_vars: Vec::new(),
        volumes: Vec::new(),
        service_domain: None,
    };
    
    let resource_requirements3 = ResourceRequirements {
        requests: ResourceRequest {
            cpu: 0.5,
            memory: 1.5 * 1024 * 1024 * 1024, // 1.5 GB
        },
        limits: ResourceLimit {
            cpu: 1.0,
            memory: 2.0 * 1024 * 1024 * 1024, // 2 GB
        },
    };
    
    // Check if container3 fails quota check (memory quota exceeded)
    let quota_check3 = scheduler.check_tenant_quotas(&tenant_id, &resource_requirements3).await?;
    assert!(!quota_check3, "Container3 should fail quota check due to memory limit");
    
    // Update tenant resource usage for container1 removal
    scheduler.update_tenant_resource_usage(&tenant_id, &resource_requirements1, false);
    
    // Now container2 should pass quota check
    let quota_check4 = scheduler.check_tenant_quotas(&tenant_id, &resource_requirements2).await?;
    assert!(quota_check4, "Container2 should pass quota check after container1 removal");
    
    // Add two more containers to reach the container count limit
    scheduler.update_tenant_resource_usage(&tenant_id, &resource_requirements2, true);
    scheduler.update_tenant_resource_usage(&tenant_id, &resource_requirements3, true);
    
    // Create a fourth container that would exceed container count quota
    let container4 = Container {
        id: "container4".to_string(),
        name: format!("{}-container4", tenant_id),
        image: "test-image:latest".to_string(),
        status: ContainerStatus::Pending,
        node_id: "".to_string(),
        created_at: Utc::now().timestamp(),
        ports: Vec::new(),
        env_vars: Vec::new(),
        volumes: Vec::new(),
        service_domain: None,
    };
    
    let resource_requirements4 = ResourceRequirements {
        requests: ResourceRequest {
            cpu: 0.1,
            memory: 0.1 * 1024 * 1024 * 1024, // 0.1 GB
        },
        limits: ResourceLimit {
            cpu: 0.2,
            memory: 0.2 * 1024 * 1024 * 1024, // 0.2 GB
        },
    };
    
    // Check if container4 fails quota check (container count quota exceeded)
    let quota_check5 = scheduler.check_tenant_quotas(&tenant_id, &resource_requirements4).await?;
    assert!(!quota_check5, "Container4 should fail quota check due to container count limit");
    
    // Test service count quota
    scheduler.update_tenant_service_count(&tenant_id, true);
    scheduler.update_tenant_service_count(&tenant_id, true);
    
    // Check if adding another service would exceed quota
    let (_, _, _, service_count) = scheduler.get_tenant_resource_usage(&tenant_id);
    assert_eq!(service_count, 2, "Service count should be 2");
    
    // Adding another service should exceed quota
    scheduler.update_tenant_service_count(&tenant_id, true);
    let (_, _, _, service_count) = scheduler.get_tenant_resource_usage(&tenant_id);
    assert_eq!(service_count, 3, "Service count should be 3");
    
    // Check resource usage reporting
    let (cpu_usage, memory_usage, container_count, _) = scheduler.get_tenant_resource_usage(&tenant_id);
    assert_eq!(cpu_usage, 2.0, "CPU usage should be 2.0");
    assert_eq!(memory_usage, 1.6 * 1024 * 1024 * 1024 as u64, "Memory usage should be 1.6 GB");
    assert_eq!(container_count, 3, "Container count should be 3");
    
    Ok(())
}