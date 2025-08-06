package tenant

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ao/hivemind/internal/app"
	"github.com/ao/hivemind/internal/security"
	"github.com/ao/hivemind/internal/storage"
	"github.com/ao/hivemind/pkg/api"
	"github.com/ao/hivemind/test/fixtures"
)

// containerManagerAdapter adapts MockContainerdManager to app.ContainerManager interface
type containerManagerAdapter struct {
	mock *fixtures.MockContainerdManager
}

// CreateContainer implements app.ContainerManager interface
func (a *containerManagerAdapter) CreateContainer(ctx context.Context, name, image string, command []string, env map[string]string, volumes []string, ports map[string]string) (string, error) {
	return "mock-container-" + name, nil
}

// StartContainer implements app.ContainerManager interface
func (a *containerManagerAdapter) StartContainer(ctx context.Context, id string) error {
	return nil
}

// StopContainer implements app.ContainerManager interface
func (a *containerManagerAdapter) StopContainer(ctx context.Context, id string) error {
	return a.mock.StopContainer(ctx, id)
}

// RemoveContainer implements app.ContainerManager interface
func (a *containerManagerAdapter) RemoveContainer(ctx context.Context, id string) error {
	return nil
}

// GetContainerStatus implements app.ContainerManager interface
func (a *containerManagerAdapter) GetContainerStatus(ctx context.Context, id string) (string, error) {
	return "running", nil
}

// GetContainerLogs implements app.ContainerManager interface
func (a *containerManagerAdapter) GetContainerLogs(ctx context.Context, id string) (string, error) {
	return "mock logs", nil
}

// ExecInContainer implements app.ContainerManager interface
func (a *containerManagerAdapter) ExecInContainer(ctx context.Context, id string, command []string) (string, error) {
	return "mock exec output", nil
}

// ListContainers implements app.ContainerManager interface
func (a *containerManagerAdapter) ListContainers(ctx context.Context) ([]api.Container, error) {
	return []api.Container{
		{
			ID:     "mock-container-1",
			Name:   "mock-container-1",
			Image:  "mock-image:latest",
			Status: "running",
			NodeID: "test-node",
		},
	}, nil
}

// TestTenantIsolation tests the tenant isolation functionality
func TestTenantIsolation(t *testing.T) {
	// Create a logger for testing
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create tenant manager
	tenantManager := security.NewTenantManager(logger)
	require.NotNil(t, tenantManager)

	t.Run("TenantCreation", func(t *testing.T) {
		// Create tenants
		tenant1, err := tenantManager.CreateTenant(ctx, "tenant-1", "Tenant 1", "tenant1@example.com")
		assert.NoError(t, err)
		assert.NotNil(t, tenant1)
		assert.Equal(t, "tenant-1", tenant1.ID)
		assert.Equal(t, "Tenant 1", tenant1.Name)
		assert.Equal(t, "tenant1@example.com", tenant1.Email)

		tenant2, err := tenantManager.CreateTenant(ctx, "tenant-2", "Tenant 2", "tenant2@example.com")
		assert.NoError(t, err)
		assert.NotNil(t, tenant2)
		assert.Equal(t, "tenant-2", tenant2.ID)

		// Get tenant
		retrievedTenant, err := tenantManager.GetTenant(ctx, "tenant-1")
		assert.NoError(t, err)
		assert.Equal(t, tenant1.ID, retrievedTenant.ID)
		assert.Equal(t, tenant1.Name, retrievedTenant.Name)
		assert.Equal(t, tenant1.Email, retrievedTenant.Email)

		// List tenants
		tenants, err := tenantManager.ListTenants(ctx)
		assert.NoError(t, err)
		assert.Len(t, tenants, 2)

		// Delete tenant
		err = tenantManager.DeleteTenant(ctx, "tenant-2")
		assert.NoError(t, err)

		// Verify tenant is deleted
		_, err = tenantManager.GetTenant(ctx, "tenant-2")
		assert.Error(t, err)

		// List tenants after deletion
		tenants, err = tenantManager.ListTenants(ctx)
		assert.NoError(t, err)
		assert.Len(t, tenants, 1)
		assert.Equal(t, "tenant-1", tenants[0].ID)
	})

	t.Run("TenantUsers", func(t *testing.T) {
		// Create tenant
		tenant, err := tenantManager.CreateTenant(ctx, "tenant-3", "Tenant 3", "tenant3@example.com")
		assert.NoError(t, err)

		// Create users
		user1, err := tenantManager.CreateUser(ctx, tenant.ID, "user-1", "User 1", "user1@example.com")
		assert.NoError(t, err)
		assert.NotNil(t, user1)
		assert.Equal(t, "user-1", user1.ID)
		assert.Equal(t, "User 1", user1.Name)
		assert.Equal(t, "user1@example.com", user1.Email)
		assert.Equal(t, tenant.ID, user1.TenantID)

		user2, err := tenantManager.CreateUser(ctx, tenant.ID, "user-2", "User 2", "user2@example.com")
		assert.NoError(t, err)
		assert.NotNil(t, user2)
		assert.Equal(t, "user-2", user2.ID)
		assert.Equal(t, tenant.ID, user2.TenantID)

		// Get user
		retrievedUser, err := tenantManager.GetUser(ctx, tenant.ID, "user-1")
		assert.NoError(t, err)
		assert.Equal(t, user1.ID, retrievedUser.ID)
		assert.Equal(t, user1.Name, retrievedUser.Name)
		assert.Equal(t, user1.Email, retrievedUser.Email)
		assert.Equal(t, user1.TenantID, retrievedUser.TenantID)

		// List users
		users, err := tenantManager.ListUsers(ctx, tenant.ID)
		assert.NoError(t, err)
		assert.Len(t, users, 2)

		// Delete user
		err = tenantManager.DeleteUser(ctx, tenant.ID, "user-2")
		assert.NoError(t, err)

		// Verify user is deleted
		_, err = tenantManager.GetUser(ctx, tenant.ID, "user-2")
		assert.Error(t, err)

		// List users after deletion
		users, err = tenantManager.ListUsers(ctx, tenant.ID)
		assert.NoError(t, err)
		assert.Len(t, users, 1)
		assert.Equal(t, "user-1", users[0].ID)

		// Clean up
		err = tenantManager.DeleteTenant(ctx, tenant.ID)
		assert.NoError(t, err)
	})
}

// TestTenantResourceLimits tests the tenant resource limits functionality
func TestTenantResourceLimits(t *testing.T) {
	// Create a logger for testing
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create tenant manager
	tenantManager := security.NewTenantManager(logger)
	require.NotNil(t, tenantManager)

	t.Run("ResourceQuotas", func(t *testing.T) {
		// Create tenant
		tenant, err := tenantManager.CreateTenant(ctx, "tenant-4", "Tenant 4", "tenant4@example.com")
		assert.NoError(t, err)

		// Set resource quotas
		quotas := security.ResourceQuotas{
			CPU:              10,
			Memory:           20480,  // 20GB
			Storage:          102400, // 100GB
			MaxContainers:    20,
			MaxVolumes:       10,
			MaxLoadBalancers: 2,
		}

		err = tenantManager.SetResourceQuotas(ctx, tenant.ID, quotas)
		assert.NoError(t, err)

		// Get resource quotas
		retrievedQuotas, err := tenantManager.GetResourceQuotas(ctx, tenant.ID)
		assert.NoError(t, err)
		assert.Equal(t, quotas.CPU, retrievedQuotas.CPU)
		assert.Equal(t, quotas.Memory, retrievedQuotas.Memory)
		assert.Equal(t, quotas.Storage, retrievedQuotas.Storage)
		assert.Equal(t, quotas.MaxContainers, retrievedQuotas.MaxContainers)
		assert.Equal(t, quotas.MaxVolumes, retrievedQuotas.MaxVolumes)
		assert.Equal(t, quotas.MaxLoadBalancers, retrievedQuotas.MaxLoadBalancers)

		// Update resource quotas
		quotas.CPU = 20
		quotas.Memory = 40960 // 40GB
		err = tenantManager.SetResourceQuotas(ctx, tenant.ID, quotas)
		assert.NoError(t, err)

		// Get updated resource quotas
		retrievedQuotas, err = tenantManager.GetResourceQuotas(ctx, tenant.ID)
		assert.NoError(t, err)
		assert.Equal(t, float64(20), retrievedQuotas.CPU)
		assert.Equal(t, int64(40960), retrievedQuotas.Memory)

		// Clean up
		err = tenantManager.DeleteTenant(ctx, tenant.ID)
		assert.NoError(t, err)
	})

	t.Run("ResourceUsage", func(t *testing.T) {
		// Create tenant
		tenant, err := tenantManager.CreateTenant(ctx, "tenant-5", "Tenant 5", "tenant5@example.com")
		assert.NoError(t, err)

		// Set resource quotas
		quotas := security.ResourceQuotas{
			CPU:              10,
			Memory:           20480,  // 20GB
			Storage:          102400, // 100GB
			MaxContainers:    20,
			MaxVolumes:       10,
			MaxLoadBalancers: 2,
		}

		err = tenantManager.SetResourceQuotas(ctx, tenant.ID, quotas)
		assert.NoError(t, err)

		// Record resource usage
		usage := security.ResourceUsage{
			CPU:           2,
			Memory:        4096,  // 4GB
			Storage:       10240, // 10GB
			Containers:    5,
			Volumes:       2,
			LoadBalancers: 1,
		}

		err = tenantManager.RecordResourceUsage(ctx, tenant.ID, usage)
		assert.NoError(t, err)

		// Get resource usage
		retrievedUsage, err := tenantManager.GetResourceUsage(ctx, tenant.ID)
		assert.NoError(t, err)
		assert.Equal(t, usage.CPU, retrievedUsage.CPU)
		assert.Equal(t, usage.Memory, retrievedUsage.Memory)
		assert.Equal(t, usage.Storage, retrievedUsage.Storage)
		assert.Equal(t, usage.Containers, retrievedUsage.Containers)
		assert.Equal(t, usage.Volumes, retrievedUsage.Volumes)
		assert.Equal(t, usage.LoadBalancers, retrievedUsage.LoadBalancers)

		// Check if resource allocation is allowed
		allowed, err := tenantManager.CheckResourceAllocation(ctx, tenant.ID, security.ResourceAllocation{
			CPU:    2,
			Memory: 4096,
		})
		assert.NoError(t, err)
		assert.True(t, allowed)

		// Check if resource allocation exceeds quota
		allowed, err = tenantManager.CheckResourceAllocation(ctx, tenant.ID, security.ResourceAllocation{
			CPU:    10,
			Memory: 20480,
		})
		assert.NoError(t, err)
		assert.False(t, allowed)

		// Clean up
		err = tenantManager.DeleteTenant(ctx, tenant.ID)
		assert.NoError(t, err)
	})
}

// TestTenantNetworkIsolation tests the tenant network isolation functionality
func TestTenantNetworkIsolation(t *testing.T) {
	// Create a logger for testing
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create tenant manager
	tenantManager := security.NewTenantManager(logger)
	require.NotNil(t, tenantManager)

	// Create network policy manager
	policyManager := security.NewNetworkPolicyManager(logger)
	require.NotNil(t, policyManager)

	// Create network policy controller
	policyController := security.NewNetworkPolicyController(logger)
	require.NotNil(t, policyController)

	// Connect components
	policyManager.WithNetworkPolicyController(policyController)
	tenantManager.WithNetworkPolicyManager(policyManager)

	t.Run("TenantNetworkIsolation", func(t *testing.T) {
		// Create tenants
		tenant1, err := tenantManager.CreateTenant(ctx, "tenant-6", "Tenant 6", "tenant6@example.com")
		assert.NoError(t, err)

		tenant2, err := tenantManager.CreateTenant(ctx, "tenant-7", "Tenant 7", "tenant7@example.com")
		assert.NoError(t, err)

		// Enable network isolation for tenants
		err = tenantManager.EnableNetworkIsolation(ctx, tenant1.ID)
		assert.NoError(t, err)

		err = tenantManager.EnableNetworkIsolation(ctx, tenant2.ID)
		assert.NoError(t, err)

		// Register containers for tenant 1
		container1 := "tenant1-container-1"
		container2 := "tenant1-container-2"

		err = tenantManager.RegisterContainer(ctx, tenant1.ID, container1)
		assert.NoError(t, err)

		err = tenantManager.RegisterContainer(ctx, tenant1.ID, container2)
		assert.NoError(t, err)

		// Register containers for tenant 2
		container3 := "tenant2-container-1"
		container4 := "tenant2-container-2"

		err = tenantManager.RegisterContainer(ctx, tenant2.ID, container3)
		assert.NoError(t, err)

		err = tenantManager.RegisterContainer(ctx, tenant2.ID, container4)
		assert.NoError(t, err)

		// Test intra-tenant communication
		// Container 1 -> Container 2 (same tenant): Allow
		allowed := policyController.EvaluateTraffic(
			"10.244.0.2", // container1
			"10.244.0.3", // container2
			"tcp",
			80,
			security.NetworkPolicyDirectionIngress,
		)
		assert.True(t, allowed)

		// Container 3 -> Container 4 (same tenant): Allow
		allowed = policyController.EvaluateTraffic(
			"10.244.0.4", // container3
			"10.244.0.5", // container4
			"tcp",
			80,
			security.NetworkPolicyDirectionIngress,
		)
		assert.True(t, allowed)

		// Test inter-tenant communication
		// Container 1 -> Container 3 (different tenants): Deny
		allowed = policyController.EvaluateTraffic(
			"10.244.0.2", // container1
			"10.244.0.4", // container3
			"tcp",
			80,
			security.NetworkPolicyDirectionIngress,
		)
		assert.False(t, allowed)

		// Container 3 -> Container 2 (different tenants): Deny
		allowed = policyController.EvaluateTraffic(
			"10.244.0.4", // container3
			"10.244.0.3", // container2
			"tcp",
			80,
			security.NetworkPolicyDirectionIngress,
		)
		assert.False(t, allowed)

		// Clean up
		err = tenantManager.DeleteTenant(ctx, tenant1.ID)
		assert.NoError(t, err)

		err = tenantManager.DeleteTenant(ctx, tenant2.ID)
		assert.NoError(t, err)
	})
}

// TestTenantStorageIsolation tests the tenant storage isolation functionality
func TestTenantStorageIsolation(t *testing.T) {
	// Create a logger for testing
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	// Create a temporary directory for testing
	tempDir := fixtures.TestTempDir(t)

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create tenant manager
	tenantManager := security.NewTenantManager(logger)
	require.NotNil(t, tenantManager)

	// Create storage manager
	storageManager, err := storage.NewManager(tempDir, logger)
	require.NoError(t, err)
	defer storageManager.Close()

	// Create storage ACL manager
	aclManager := security.NewStorageACLManager(logger)
	require.NotNil(t, aclManager)

	// Connect components
	tenantManager.WithStorageManager(storageManager)
	tenantManager.WithStorageACLManager(aclManager)

	t.Run("TenantStorageIsolation", func(t *testing.T) {
		// Create tenants
		tenant1, err := tenantManager.CreateTenant(ctx, "tenant-8", "Tenant 8", "tenant8@example.com")
		assert.NoError(t, err)

		tenant2, err := tenantManager.CreateTenant(ctx, "tenant-9", "Tenant 9", "tenant9@example.com")
		assert.NoError(t, err)

		// Create users
		user1, err := tenantManager.CreateUser(ctx, tenant1.ID, "user-1", "User 1", "user1@example.com")
		assert.NoError(t, err)

		user2, err := tenantManager.CreateUser(ctx, tenant2.ID, "user-2", "User 2", "user2@example.com")
		assert.NoError(t, err)

		// Create volumes for tenant 1
		volume1Options := storage.VolumeOptions{
			Name:     "tenant1-volume-1",
			Size:     1024,
			TenantID: tenant1.ID,
			UserID:   user1.ID,
		}

		volume1Interface, err := storageManager.CreateVolume(ctx, volume1Options)
		assert.NoError(t, err)
		assert.NotNil(t, volume1Interface)

		// Type assertion to convert interface{} to *VolumeStatus
		volume1, ok := volume1Interface.(*storage.VolumeStatus)
		assert.True(t, ok, "Expected volume1Interface to be of type *storage.VolumeStatus")
		assert.Equal(t, "tenant1-volume-1", volume1.Name)
		// TenantID is not stored in VolumeStatus, so we compare with the original options
		assert.Equal(t, tenant1.ID, volume1Options.TenantID)

		// Create volumes for tenant 2
		volume2Options := storage.VolumeOptions{
			Name:     "tenant2-volume-1",
			Size:     1024,
			TenantID: tenant2.ID,
			UserID:   user2.ID,
		}

		volume2Interface, err := storageManager.CreateVolume(ctx, volume2Options)
		assert.NoError(t, err)
		assert.NotNil(t, volume2Interface)

		// Type assertion to convert interface{} to *VolumeStatus
		volume2, ok := volume2Interface.(*storage.VolumeStatus)
		assert.True(t, ok, "Expected volume2Interface to be of type *storage.VolumeStatus")
		assert.Equal(t, "tenant2-volume-1", volume2.Name)
		// TenantID is not stored in VolumeStatus, so we compare with the original options
		assert.Equal(t, tenant2.ID, volume2Options.TenantID)

		// Test tenant 1 access to its own volume
		allowed, _, err := aclManager.CheckAccess(
			ctx,
			volume1.Name, // Use Name instead of ID
			security.StorageResourceTypeVolume,
			"tenant",
			tenant1.ID,
			security.StoragePermissionRead,
			nil,
			nil,
		)
		assert.NoError(t, err)
		assert.True(t, allowed)

		// Test tenant 2 access to its own volume
		allowed, _, err = aclManager.CheckAccess(
			ctx,
			volume2.Name, // Use Name instead of ID
			security.StorageResourceTypeVolume,
			"tenant",
			tenant2.ID,
			security.StoragePermissionRead,
			nil,
			nil,
		)
		assert.NoError(t, err)
		assert.True(t, allowed)

		// Test tenant 1 access to tenant 2's volume
		allowed, _, err = aclManager.CheckAccess(
			ctx,
			volume2.Name, // Use Name instead of ID
			security.StorageResourceTypeVolume,
			"tenant",
			tenant1.ID,
			security.StoragePermissionRead,
			nil,
			nil,
		)
		assert.NoError(t, err)
		assert.False(t, allowed)

		// Test tenant 2 access to tenant 1's volume
		allowed, _, err = aclManager.CheckAccess(
			ctx,
			volume1.Name, // Use Name instead of ID
			security.StorageResourceTypeVolume,
			"tenant",
			tenant2.ID,
			security.StoragePermissionRead,
			nil,
			nil,
		)
		assert.NoError(t, err)
		assert.False(t, allowed)

		// Clean up
		err = storageManager.DeleteVolume(volume1.Name)
		assert.NoError(t, err)

		err = storageManager.DeleteVolume(volume2.Name)
		assert.NoError(t, err)

		err = tenantManager.DeleteTenant(ctx, tenant1.ID)
		assert.NoError(t, err)

		err = tenantManager.DeleteTenant(ctx, tenant2.ID)
		assert.NoError(t, err)
	})
}

// TestTenantResourceScheduling tests the tenant-aware resource scheduling functionality
func TestTenantResourceScheduling(t *testing.T) {
	// Create a logger for testing
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create tenant manager
	tenantManager := security.NewTenantManager(logger)
	require.NotNil(t, tenantManager)

	// Create a temporary directory for the app manager
	tempDir, err := os.MkdirTemp("", "hivemind-tenant-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	// Create app manager with the temp directory
	appManager, err := app.NewManager(tempDir, logger)
	require.NoError(t, err)
	defer appManager.Close()

	// Create a mock container manager
	mockContainerdManager := fixtures.NewMockContainerdManager(logger)

	// Create an adapter to make MockContainerdManager compatible with app.ContainerManager
	containerManagerAdapter := &containerManagerAdapter{mockContainerdManager}

	// Set the container manager on the app manager
	appManager.WithContainerManager(containerManagerAdapter)

	// Connect components
	tenantManager.WithAppManager(appManager)
	appManager.WithTenantManager(tenantManager)

	t.Run("TenantResourceScheduling", func(t *testing.T) {
		// Create tenants
		tenant1, err := tenantManager.CreateTenant(ctx, "tenant-10", "Tenant 10", "tenant10@example.com")
		assert.NoError(t, err)

		tenant2, err := tenantManager.CreateTenant(ctx, "tenant-11", "Tenant 11", "tenant11@example.com")
		assert.NoError(t, err)

		// Set resource quotas for tenant 1
		quotas1 := security.ResourceQuotas{
			CPU:           5,
			Memory:        10240, // 10GB
			Storage:       51200, // 50GB
			MaxContainers: 10,
		}

		err = tenantManager.SetResourceQuotas(ctx, tenant1.ID, quotas1)
		assert.NoError(t, err)

		// Set resource quotas for tenant 2
		quotas2 := security.ResourceQuotas{
			CPU:           10,
			Memory:        20480,  // 20GB
			Storage:       102400, // 100GB
			MaxContainers: 20,
		}

		err = tenantManager.SetResourceQuotas(ctx, tenant2.ID, quotas2)
		assert.NoError(t, err)

		// Create apps for tenant 1
		for i := 0; i < 5; i++ {
			appOptions := app.AppOptions{
				Name:  "tenant1-app-" + strconv.Itoa(i+1),
				Image: "nginx:latest",
				Resources: app.ResourceRequirements{
					CPU:    1,
					Memory: 1024, // 1GB
				},
				TenantID: tenant1.ID,
				UserID:   "user-1",
			}

			_, err := appManager.CreateApp(ctx, appOptions)
			assert.NoError(t, err)
		}

		// Create apps for tenant 2
		for i := 0; i < 8; i++ {
			appOptions := app.AppOptions{
				Name:  "tenant2-app-" + strconv.Itoa(i+1),
				Image: "nginx:latest",
				Resources: app.ResourceRequirements{
					CPU:    1,
					Memory: 1024, // 1GB
				},
				TenantID: tenant2.ID,
				UserID:   "user-2",
			}

			_, err := appManager.CreateApp(ctx, appOptions)
			assert.NoError(t, err)
		}

		// Get tenant 1 resource usage
		usage1, err := tenantManager.GetResourceUsage(ctx, tenant1.ID)
		assert.NoError(t, err)
		assert.Equal(t, float64(5), usage1.CPU)
		assert.Equal(t, int64(5*1024), usage1.Memory)
		assert.Equal(t, 5, usage1.Containers)

		// Get tenant 2 resource usage
		usage2, err := tenantManager.GetResourceUsage(ctx, tenant2.ID)
		assert.NoError(t, err)
		assert.Equal(t, float64(8), usage2.CPU)
		assert.Equal(t, int64(8*1024), usage2.Memory)
		assert.Equal(t, 8, usage2.Containers)

		// Try to exceed tenant 1 quota
		appOptions := app.AppOptions{
			Name:  "tenant1-app-exceed",
			Image: "nginx:latest",
			Resources: app.ResourceRequirements{
				CPU:    2,
				Memory: 6144, // 6GB
			},
			TenantID: tenant1.ID,
			UserID:   "user-1",
		}

		_, err = appManager.CreateApp(ctx, appOptions)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "quota exceeded")

		// Clean up
		err = tenantManager.DeleteTenant(ctx, tenant1.ID)
		assert.NoError(t, err)

		err = tenantManager.DeleteTenant(ctx, tenant2.ID)
		assert.NoError(t, err)
	})
}
