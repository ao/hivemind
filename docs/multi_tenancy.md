# Multi-tenancy in Hivemind

This document provides an overview of the multi-tenancy feature in Hivemind, including its architecture, configuration, and usage.

## Overview

Multi-tenancy allows Hivemind to securely host workloads from multiple users or organizations on the same infrastructure. The implementation provides flexible isolation levels, resource quotas, and tenant-specific access controls.

## Architecture

The multi-tenancy feature is built on several key components:

1. **Tenant Manager**: Central component that manages tenant lifecycle and integration with other Hivemind components
2. **RBAC Integration**: Tight integration with the Role-Based Access Control system for tenant-specific permissions
3. **Resource Isolation**: Mechanisms to enforce resource quotas and isolation between tenants
4. **Tenant Context**: Context propagation throughout the system to maintain tenant boundaries

### Component Diagram

```
┌─────────────────┐      ┌─────────────────┐
│   API Server    │      │  Web Dashboard  │
└────────┬────────┘      └────────┬────────┘
         │                        │
         ▼                        ▼
┌─────────────────────────────────────────┐
│             Tenant Manager              │
├─────────────────────────────────────────┤
│ • Tenant Lifecycle Management           │
│ • Resource Quota Enforcement            │
│ • Isolation Level Management            │
│ • Tenant Context Propagation            │
└───────────────────┬─────────────────────┘
                    │
        ┌───────────┴───────────┐
        │                       │
        ▼                       ▼
┌───────────────┐      ┌───────────────────┐
│ RBAC Manager  │      │ Other Components  │
└───────────────┘      └───────────────────┘
```

## Tenant Model

### Tenant

A tenant represents a logical isolation boundary in the system with the following properties:

- **ID**: Unique identifier for the tenant
- **Name**: Human-readable name for the tenant
- **Isolation Level**: Level of isolation from other tenants (Hard, Soft, NetworkOnly, Custom)
- **Resource Quotas**: Resource limits for the tenant
- **Namespaces**: Collection of namespaces owned by the tenant
- **Owner**: User who owns the tenant
- **Status**: Current status of the tenant (Active, Suspended, Pending, Terminating)

### Isolation Levels

Hivemind supports multiple isolation levels to balance security and resource efficiency:

1. **Hard Isolation**: Complete isolation with dedicated nodes, network, and storage
2. **Soft Isolation**: Logical isolation with resource quotas but shared infrastructure
3. **Network-Only Isolation**: Network isolation but shared compute resources
4. **Custom Isolation**: Fine-grained control over isolation aspects

### Resource Quotas

Resource quotas can be defined for each tenant to limit resource consumption:

- CPU limits
- Memory limits
- Storage limits
- Maximum number of containers
- Maximum number of services

## Usage

### Creating a Tenant

To create a new tenant:

```rust
let tenant = Tenant {
    id: "tenant-id".to_string(),
    name: "My Tenant".to_string(),
    isolation_level: IsolationLevel::Soft,
    resource_quotas: ResourceQuotas {
        cpu_limit: Some(4),
        memory_limit: Some(8 * 1024 * 1024 * 1024), // 8 GB
        storage_limit: Some(100 * 1024 * 1024 * 1024), // 100 GB
        max_containers: Some(10),
        max_services: Some(5),
    },
    namespaces: vec!["my-namespace".to_string()],
    created_at: Utc::now(),
    updated_at: Utc::now(),
    owner_id: "user-id".to_string(),
    description: Some("My tenant description".to_string()),
    labels: HashMap::new(),
    status: TenantStatus::Active,
};

tenant_manager.create_tenant(tenant).await?;
```

### Tenant-Aware Operations

When performing operations in a multi-tenant environment, always include the tenant context:

```rust
let tenant_context = TenantContext::new("tenant-id".to_string(), "user-id".to_string());

// Deploy an application in the tenant context
app_manager.deploy_app_with_tenant_context(
    &image,
    &name,
    service.as_deref(),
    volumes,
    &tenant_context,
).await?;
```

### Managing Tenant Resources

To update resource quotas for a tenant:

```rust
let quotas = ResourceQuotas {
    cpu_limit: Some(8),
    memory_limit: Some(16 * 1024 * 1024 * 1024), // 16 GB
    storage_limit: Some(200 * 1024 * 1024 * 1024), // 200 GB
    max_containers: Some(20),
    max_services: Some(10),
};

tenant_manager.update_resource_quotas("tenant-id", quotas).await?;
```

### Tenant Access Control

The tenant manager integrates with the RBAC system to control access to tenant resources:

```rust
// Check if a user has access to a tenant
let has_access = tenant_manager.check_tenant_access("user-id", "tenant-id").await?;

// Get all tenants a user has access to
let accessible_tenants = tenant_manager.get_tenant_context("user-id").await?;
```

## Best Practices

1. **Always Use Tenant Context**: When performing operations that affect resources, always include the tenant context to ensure proper isolation.

2. **Set Appropriate Resource Quotas**: Configure resource quotas based on tenant needs to prevent resource starvation.

3. **Choose the Right Isolation Level**: Select the isolation level that balances security requirements with resource efficiency.

4. **Implement Proper Access Controls**: Use the RBAC system to control access to tenant resources.

5. **Monitor Tenant Resource Usage**: Regularly monitor resource usage to identify potential issues.

## Future Enhancements

The multi-tenancy feature will be enhanced in future releases with:

1. **Tenant-Specific Dashboards**: Custom dashboards for each tenant
2. **Enhanced Network Isolation**: More advanced network isolation features
3. **Tenant Migration**: Tools to migrate tenants between clusters
4. **Tenant Billing**: Resource usage tracking and billing integration
5. **Tenant Templates**: Pre-configured tenant templates for common use cases

## Troubleshooting

### Common Issues

1. **Resource Quota Exceeded**: If a tenant exceeds its resource quota, new resource creation will fail. Check the tenant's resource usage and adjust quotas if necessary.

2. **Access Denied**: If a user cannot access tenant resources, verify that the user has the appropriate roles and permissions.

3. **Tenant Creation Fails**: If tenant creation fails, check for duplicate tenant names or IDs.

## API Reference

For detailed API documentation, see the [Tenant API Reference](api_reference.md#tenant-api).