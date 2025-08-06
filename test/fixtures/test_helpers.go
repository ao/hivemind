package fixtures

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/ao/hivemind/internal/app"
	"github.com/ao/hivemind/internal/containerd"
	"github.com/ao/hivemind/internal/membership"
	"github.com/ao/hivemind/internal/node"
	"github.com/ao/hivemind/internal/scheduler"
	"github.com/ao/hivemind/internal/security"
	"github.com/ao/hivemind/internal/service"
	"github.com/ao/hivemind/internal/storage"
)

// TestContext creates a context with timeout for testing
func TestContext(t *testing.T) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// TestLogger creates a logger for testing
func TestLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(os.Stdout)
	logger.SetLevel(logrus.DebugLevel)
	return logger
}

// TestTempDir creates a temporary directory for testing
func TestTempDir(t *testing.T) string {
	tempDir, err := os.MkdirTemp("", "hivemind-test-*")
	require.NoError(t, err)
	t.Cleanup(func() {
		os.RemoveAll(tempDir)
	})
	return tempDir
}

// TestComponents creates all components needed for testing
func TestComponents(t *testing.T) (*app.Manager, *containerd.Manager, *node.Manager, *membership.Manager, *scheduler.EnhancedScheduler, *service.Discovery, *storage.Manager) {
	logger := TestLogger()
	tempDir := TestTempDir(t)

	// Initialize Container Manager
	containerManager, err := containerd.NewManager("/run/containerd/containerd.sock", logger)
	require.NoError(t, err)
	t.Cleanup(func() {
		containerManager.Close()
	})

	// Initialize Node Manager
	nodeManager, err := node.NewManager()
	require.NoError(t, err)
	t.Cleanup(func() {
		nodeManager.Close()
	})

	// Initialize Membership Manager
	membershipManager, err := membership.NewManager("test-node", "127.0.0.1", 7946, logger)
	require.NoError(t, err)
	t.Cleanup(func() {
		membershipManager.Close()
	})

	// Initialize Scheduler
	schedulerManager, err := scheduler.NewEnhancedScheduler(logger)
	require.NoError(t, err)
	t.Cleanup(func() {
		schedulerManager.Close()
	})

	// Initialize Service Discovery
	serviceDiscovery, err := service.NewDiscovery(logger)
	require.NoError(t, err)
	t.Cleanup(func() {
		serviceDiscovery.Close()
	})

	// Initialize Storage Manager
	storageDir := filepath.Join(tempDir, "storage")
	storageManager, err := storage.NewManager(storageDir, logger)
	require.NoError(t, err)
	t.Cleanup(func() {
		storageManager.Close()
	})

	// Initialize App Manager
	appDir := filepath.Join(tempDir, "apps")
	appManager, err := app.NewManager(appDir, logger)
	require.NoError(t, err)
	t.Cleanup(func() {
		appManager.Close()
	})

	// Connect components
	// These methods don't exist, so we'll comment them out for now
	// nodeManager.WithMembershipManager(membershipManager)
	// schedulerManager.WithNodeManager(nodeManager)
	// appManager.WithContainerManager(containerManager)
	// appManager.WithStorageManager(storageManager)
	// serviceDiscovery.WithNodeManager(nodeManager)

	return appManager, containerManager, nodeManager, membershipManager, schedulerManager, serviceDiscovery, storageManager
}

// TestSecurityComponents creates all security components needed for testing
func TestSecurityComponents(t *testing.T) (*security.SecretManager, *security.RbacManager, *security.ContainerScanner, *security.NetworkPolicyManager, *security.NetworkPolicyController, *security.StorageACLManager, *security.StorageEncryptionManager, *security.StorageQoSManager) {
	logger := TestLogger()

	// Initialize Secret Manager
	secretManager := security.NewSecretManager(logger)
	require.NotNil(t, secretManager)

	// Initialize RBAC Manager
	rbacManager := security.NewRBACManager(logger)
	require.NotNil(t, rbacManager)

	// Initialize Container Scanning Manager
	containerScanningManager := security.NewContainerScanningManager(logger)
	require.NotNil(t, containerScanningManager)

	// Initialize Network Policy Manager
	networkPolicyManager := security.NewNetworkPolicyManager(logger)
	require.NotNil(t, networkPolicyManager)

	// Initialize Network Policy Controller
	networkPolicyController := security.NewNetworkPolicyController(logger)
	require.NotNil(t, networkPolicyController)

	// Initialize Storage ACL Manager
	storageACLManager := security.NewStorageACLManager(logger)
	require.NotNil(t, storageACLManager)

	// Initialize Storage Encryption Manager
	storageEncryptionManager := security.NewStorageEncryptionManager(logger, secretManager)
	require.NotNil(t, storageEncryptionManager)

	// Initialize Storage QoS Manager
	storageQoSManager := security.NewStorageQoSManager(logger)
	require.NotNil(t, storageQoSManager)

	// Connect components
	networkPolicyManager.WithNetworkPolicyController(networkPolicyController)

	return secretManager, rbacManager, containerScanningManager, networkPolicyManager, networkPolicyController, storageACLManager, storageEncryptionManager, storageQoSManager
}

// CreateTestNodeInfo creates a test node info
func CreateTestNodeInfo(id, hostname, address string) *node.NodeInfo {
	return &node.NodeInfo{
		ID:       id,
		Hostname: hostname,
		Address:  address,
		LastSeen: time.Now(),
		Status:   node.NodeStatusReady,
		Resources: node.NodeResources{
			CPUAvailable:      4,
			MemoryAvailable:   8192,
			ContainersRunning: 0,
			IsLeader:          false,
			CPU:               8,
			Memory:            16384,
			Disk:              200000,
		},
	}
}

// CreateTestTask creates a test task for scheduling
func CreateTestTask(id, name string) *scheduler.Task {
	return &scheduler.Task{
		ID:   id,
		Name: name,
		Resources: scheduler.ResourceRequirements{
			CPU:    2,
			Memory: 4096,
			Disk:   50000,
		},
	}
}

// CreateTestVolumeOptions creates test volume options
func CreateTestVolumeOptions(name string) storage.VolumeOptions {
	return storage.VolumeOptions{
		Name: name,
		Size: 1024,
	}
}

// CreateTestServiceInfo creates test service info
func CreateTestServiceInfo(name, namespace string) *service.ServiceInfo {
	return &service.ServiceInfo{
		Name:      name,
		Namespace: namespace,
		Endpoints: []service.ServiceEndpoint{
			{
				ServiceName:     name,
				Domain:          "example.com",
				IPAddress:       "192.168.1.1",
				Port:            4483,
				NodeID:          "test-node-1",
				HealthStatus:    service.ServiceHealthy,
				LastHealthCheck: time.Now().Unix(),
				Version:         "1.0.0",
				Weight:          1,
				Metadata:        map[string]string{},
			},
		},
	}
}

// CreateTestAppOptions creates test app options
func CreateTestAppOptions(name, image string) app.AppOptions {
	return app.AppOptions{
		Name:  name,
		Image: image,
		Command: []string{
			"nginx",
			"-g",
			"daemon off;",
		},
		Env: map[string]string{
			"ENV_VAR": "value",
		},
		Resources: app.ResourceRequirements{
			CPU:    1,
			Memory: 512,
		},
		TenantID: "test-tenant",
		UserID:   "test-user",
	}
}
