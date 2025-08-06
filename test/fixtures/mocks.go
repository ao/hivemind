package fixtures

import (
	"context"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/app"
	"github.com/ao/hivemind/internal/containerd"
	"github.com/ao/hivemind/internal/node"
	"github.com/ao/hivemind/internal/scheduler"
	"github.com/ao/hivemind/internal/security"
	"github.com/ao/hivemind/internal/service"
	"github.com/stretchr/testify/mock"
)

// MockContainerRuntime is a mock implementation of the container runtime
type MockContainerRuntime struct {
	mock.Mock
	containers sync.Map
	volumes    sync.Map
}

// NewMockContainerRuntime creates a new mock container runtime
func NewMockContainerRuntime() *MockContainerRuntime {
	return &MockContainerRuntime{}
}

// PullImage mocks pulling an image
func (m *MockContainerRuntime) PullImage(ctx context.Context, image string) error {
	args := m.Called(ctx, image)
	return args.Error(0)
}

// CreateContainer mocks creating a container
func (m *MockContainerRuntime) CreateContainer(ctx context.Context, opts containerd.ContainerOptions) (string, error) {
	args := m.Called(ctx, opts)

	if args.Error(1) != nil {
		return "", args.Error(1)
	}

	containerID := args.String(0)
	container := &containerd.Container{
		ID:        containerID,
		Name:      opts.Name,
		Image:     opts.Image,
		Status:    containerd.ContainerStatusRunning,
		NodeID:    "test-node",
		CreatedAt: time.Now(),
	}

	m.containers.Store(containerID, container)
	return containerID, nil
}

// StopContainer mocks stopping a container
func (m *MockContainerRuntime) StopContainer(ctx context.Context, containerID string) error {
	args := m.Called(ctx, containerID)

	if args.Error(0) != nil {
		return args.Error(0)
	}

	if container, ok := m.containers.Load(containerID); ok {
		c := container.(*containerd.Container)
		c.Status = containerd.ContainerStatusStopped
		m.containers.Store(containerID, c)
	}

	return nil
}

// ListContainers mocks listing containers
func (m *MockContainerRuntime) ListContainers(ctx context.Context) ([]*containerd.Container, error) {
	args := m.Called(ctx)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	var containers []*containerd.Container
	m.containers.Range(func(key, value interface{}) bool {
		containers = append(containers, value.(*containerd.Container))
		return true
	})

	return containers, nil
}

// GetContainerMetrics mocks getting container metrics
func (m *MockContainerRuntime) GetContainerMetrics(ctx context.Context, containerID string) (*containerd.ContainerStats, error) {
	args := m.Called(ctx, containerID)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	return &containerd.ContainerStats{
		CPUUsage:    25.5,
		MemoryUsage: 512 * 1024 * 1024, // 512MB
		NetworkRx:   1024 * 1024,       // 1MB
		NetworkTx:   2 * 1024 * 1024,   // 2MB
		LastUpdated: time.Now().Unix(),
	}, nil
}

// CreateVolume mocks creating a volume
func (m *MockContainerRuntime) CreateVolume(ctx context.Context, name string, opts map[string]string) error {
	args := m.Called(ctx, name, opts)

	if args.Error(0) != nil {
		return args.Error(0)
	}

	volume := &containerd.Volume{
		Name:      name,
		Path:      "/var/lib/hivemind/volumes/" + name,
		Size:      1024 * 1024 * 1024, // 1GB default
		CreatedAt: time.Now().Unix(),
	}

	m.volumes.Store(name, volume)
	return nil
}

// DeleteVolume mocks deleting a volume
func (m *MockContainerRuntime) DeleteVolume(ctx context.Context, name string) error {
	args := m.Called(ctx, name)

	if args.Error(0) != nil {
		return args.Error(0)
	}

	m.volumes.Delete(name)
	return nil
}

// ListVolumes mocks listing volumes
func (m *MockContainerRuntime) ListVolumes(ctx context.Context) ([]*containerd.Volume, error) {
	args := m.Called(ctx)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	var volumes []*containerd.Volume
	m.volumes.Range(func(key, value interface{}) bool {
		volumes = append(volumes, value.(*containerd.Volume))
		return true
	})

	return volumes, nil
}

// MockNodeManager is a mock implementation of the node manager
type MockNodeManager struct {
	mock.Mock
	nodes  sync.Map
	nodeID string
}

// ListNodes mocks listing nodes
func (m *MockNodeManager) ListNodes(ctx context.Context) ([]interface{}, error) {
	args := m.Called(ctx)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	var nodes []interface{}
	m.nodes.Range(func(key, value interface{}) bool {
		nodes = append(nodes, value)
		return true
	})

	return nodes, nil
}

// GetNodeDetails returns details about all known nodes
func (m *MockNodeManager) GetNodeDetails() ([]interface{}, error) {
	args := m.Called()

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	var nodes []interface{}
	m.nodes.Range(func(key, value interface{}) bool {
		nodes = append(nodes, value)
		return true
	})

	return nodes, nil
}

// CheckHealth checks the health of the node
func (m *MockNodeManager) CheckHealth() (bool, error) {
	args := m.Called()
	return args.Bool(0), args.Error(1)
}

// NewMockNodeManager creates a new mock node manager
func NewMockNodeManager(nodeID string) *MockNodeManager {
	m := &MockNodeManager{
		nodeID: nodeID,
	}

	// Add self node
	m.nodes.Store(nodeID, &node.NodeInfo{
		ID:       nodeID,
		Address:  "192.168.1.1",
		LastSeen: time.Now(),
		Resources: node.NodeResources{
			CPUAvailable:      8,
			MemoryAvailable:   16384,
			ContainersRunning: 0,
			IsLeader:          false,
		},
	})

	return m
}

// GetNodeID returns the node ID
func (m *MockNodeManager) GetNodeID() string {
	return m.nodeID
}

// RegisterNode mocks registering a node
func (m *MockNodeManager) RegisterNode(ctx context.Context, nodeInfo *node.NodeInfo) error {
	args := m.Called(ctx, nodeInfo)

	if args.Error(0) != nil {
		return args.Error(0)
	}

	m.nodes.Store(nodeInfo.ID, nodeInfo)
	return nil
}

// GetNodes mocks getting nodes
func (m *MockNodeManager) GetNodes(ctx context.Context) ([]*node.NodeInfo, error) {
	args := m.Called(ctx)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	// If a specific return value was set up, use it
	if args.Get(0) != nil {
		return args.Get(0).([]*node.NodeInfo), nil
	}

	// Otherwise, return the nodes from the sync.Map
	var nodes []*node.NodeInfo
	m.nodes.Range(func(key, value interface{}) bool {
		nodes = append(nodes, value.(*node.NodeInfo))
		return true
	})

	// If no nodes were found in the map, add the default node
	if len(nodes) == 0 && m.nodeID != "" {
		nodes = append(nodes, &node.NodeInfo{
			ID:       m.nodeID,
			Address:  "192.168.1.1",
			Hostname: "test-host-1",
			LastSeen: time.Now(),
			Resources: node.NodeResources{
				CPU:               8,
				Memory:            16384,
				Disk:              100000,
				CPUAvailable:      8,
				MemoryAvailable:   16384,
				ContainersRunning: 0,
				IsLeader:          false,
			},
			Status: node.NodeStatusReady,
		})
	}

	return nodes, nil
}

// AddNode adds a node to the mock
func (m *MockNodeManager) AddNode(id, hostname, address string) {
	m.nodes.Store(id, &node.NodeInfo{
		ID:       id,
		Address:  address,
		LastSeen: time.Now(),
		Resources: node.NodeResources{
			CPUAvailable:      8,
			MemoryAvailable:   16384,
			ContainersRunning: 0,
			IsLeader:          false,
		},
	})
}

// RemoveNode removes a node from the mock
func (m *MockNodeManager) RemoveNode(id string) {
	// Actually remove the node from the sync.Map
	m.nodes.Delete(id)

	// Don't re-add the default node automatically
	// This would interfere with tests that expect the node to be removed
}

// UpdateNodeResources updates node resources
func (m *MockNodeManager) UpdateNodeResources(id string, resources node.Resources) {
	if nodeInfo, ok := m.nodes.Load(id); ok {
		node := nodeInfo.(*node.NodeInfo)
		// Convert Resources to NodeResources
		node.Resources.CPU = resources.CPU
		node.Resources.Memory = resources.Memory
		node.Resources.Disk = resources.Disk
		m.nodes.Store(id, node)

		// Clear any expectations for GetNodes to ensure we return the correct nodes
		m.ExpectedCalls = nil
	}
}

// MockServiceDiscovery is a mock implementation of the service discovery
type MockServiceDiscovery struct {
	mock.Mock
	services sync.Map
}

// NewMockServiceDiscovery creates a new mock service discovery
func NewMockServiceDiscovery() *MockServiceDiscovery {
	return &MockServiceDiscovery{}
}

// RegisterService mocks registering a service
func (m *MockServiceDiscovery) RegisterService(ctx context.Context, serviceInfo *service.ServiceInfo) error {
	args := m.Called(ctx, serviceInfo)

	if args.Error(0) != nil {
		return args.Error(0)
	}

	m.services.Store(serviceInfo.Name, serviceInfo)
	return nil
}

// DiscoverService mocks discovering a service
func (m *MockServiceDiscovery) DiscoverService(ctx context.Context, name, namespace string) (*service.ServiceInfo, error) {
	args := m.Called(ctx, name, namespace)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	if serviceInfo, ok := m.services.Load(name); ok {
		return serviceInfo.(*service.ServiceInfo), nil
	}

	return nil, nil
}

// ListServices mocks listing services
func (m *MockServiceDiscovery) ListServices(ctx context.Context) (map[string][]*service.ServiceInfo, error) {
	args := m.Called(ctx)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	services := make(map[string][]*service.ServiceInfo)
	m.services.Range(func(key, value interface{}) bool {
		name := key.(string)
		serviceInfo := value.(*service.ServiceInfo)
		services[name] = append(services[name], serviceInfo)
		return true
	})

	return services, nil
}

// MockAppManager is a mock implementation of the app manager
type MockAppManager struct {
	mock.Mock
	apps sync.Map
}

// NewMockAppManager creates a new mock app manager
func NewMockAppManager() *MockAppManager {
	return &MockAppManager{}
}

// CreateApp mocks creating an app
func (m *MockAppManager) CreateApp(ctx context.Context, opts app.AppOptions) (*app.App, error) {
	args := m.Called(ctx, opts)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	app := &app.App{
		ID:        "app-" + opts.Name,
		Name:      opts.Name,
		Image:     opts.Image,
		Command:   opts.Command,
		Env:       opts.Env,
		Resources: opts.Resources,
		State:     app.AppStatePending,
		TenantID:  opts.TenantID,
		UserID:    opts.UserID,
		CreatedAt: time.Now(),
	}

	m.apps.Store(app.ID, app)
	return app, nil
}

// GetApp mocks getting an app
func (m *MockAppManager) GetApp(ctx context.Context, id string) (*app.App, error) {
	args := m.Called(ctx, id)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	if appInfo, ok := m.apps.Load(id); ok {
		return appInfo.(*app.App), nil
	}

	return nil, nil
}

// ListApps mocks listing apps
func (m *MockAppManager) ListApps(ctx context.Context) ([]*app.App, error) {
	args := m.Called(ctx)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	var apps []*app.App
	m.apps.Range(func(key, value interface{}) bool {
		apps = append(apps, value.(*app.App))
		return true
	})

	return apps, nil
}

// DeleteApp mocks deleting an app
func (m *MockAppManager) DeleteApp(ctx context.Context, id string) error {
	args := m.Called(ctx, id)

	if args.Error(0) != nil {
		return args.Error(0)
	}

	m.apps.Delete(id)
	return nil
}

// MockScheduler is a mock implementation of the scheduler
type MockScheduler struct {
	mock.Mock
}

// NewMockScheduler creates a new mock scheduler
func NewMockScheduler() *MockScheduler {
	return &MockScheduler{}
}

// Schedule mocks scheduling a task
func (m *MockScheduler) Schedule(ctx context.Context, task *scheduler.Task) (string, error) {
	args := m.Called(ctx, task)
	return args.String(0), args.Error(1)
}

// MockSecurityManager is a mock implementation of the security manager
type MockSecurityManager struct {
	mock.Mock
	policies sync.Map
	secrets  sync.Map
}

// NewMockSecurityManager creates a new mock security manager
func NewMockSecurityManager() *MockSecurityManager {
	return &MockSecurityManager{}
}

// CreateNetworkPolicy mocks creating a network policy
func (m *MockSecurityManager) CreateNetworkPolicy(ctx context.Context, name string, selector security.NetworkSelector, ingressRules, egressRules []security.NetworkRule) (*security.NetworkPolicy, error) {
	args := m.Called(ctx, name, selector, ingressRules, egressRules)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	policy := &security.NetworkPolicy{
		Name:         name,
		Selector:     selector,
		IngressRules: ingressRules,
		EgressRules:  egressRules,
		Priority:     0,
		CreatedAt:    time.Now().Unix(),
		UpdatedAt:    time.Now().Unix(),
	}

	m.policies.Store(name, policy)
	return policy, nil
}

// GetNetworkPolicy mocks getting a network policy
func (m *MockSecurityManager) GetNetworkPolicy(ctx context.Context, name string) (*security.NetworkPolicy, error) {
	args := m.Called(ctx, name)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	if policy, ok := m.policies.Load(name); ok {
		return policy.(*security.NetworkPolicy), nil
	}

	return nil, nil
}

// CreateSecret mocks creating a secret
func (m *MockSecurityManager) CreateSecret(ctx context.Context, name, description string, data []byte, createdBy string, labels, annotations map[string]string, expiration *time.Time, rotationPolicy *security.SecretRotationPolicy) (*security.Secret, error) {
	args := m.Called(ctx, name, description, data, createdBy, labels, annotations, expiration, rotationPolicy)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	secret := &security.Secret{
		ID:          "secret-" + name,
		Name:        name,
		Description: description,
		Data:        data,
		CreatedBy:   createdBy,
		CreatedAt:   time.Now().Unix(),
		Labels:      labels,
		Metadata:    annotations,
		Expiration:  expirationToInt64(expiration),
	}

	m.secrets.Store(secret.ID, secret)
	return secret, nil
}

// GetSecret mocks getting a secret
func (m *MockSecurityManager) GetSecret(ctx context.Context, id, requestedBy string) (*security.Secret, error) {
	args := m.Called(ctx, id, requestedBy)

	if args.Error(1) != nil {
		return nil, args.Error(1)
	}

	if secret, ok := m.secrets.Load(id); ok {
		return secret.(*security.Secret), nil
	}

	return nil, nil
}

// Helper function to convert *time.Time to *int64
func expirationToInt64(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	unix := t.Unix()
	return &unix
}
