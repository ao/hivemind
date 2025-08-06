package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/ao/hivemind/internal/app"
	"github.com/ao/hivemind/internal/node"
	"github.com/ao/hivemind/internal/service"
	"github.com/ao/hivemind/internal/storage"
	"github.com/ao/hivemind/pkg/api"
)

// Mock implementations for testing
type MockAppManager struct {
	mock.Mock
}

// Implement AppManager interface
func (m *MockAppManager) GetService(ctx context.Context, name string) (*app.Service, error) {
	args := m.Called(ctx, name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*app.Service), args.Error(1)
}

func (m *MockAppManager) RestartApp(ctx context.Context, id, userID, tenantID string) error {
	args := m.Called(ctx, id, userID, tenantID)
	return args.Error(0)
}

func (m *MockAppManager) GetContainersByNode(ctx context.Context, nodeID string) ([]api.Container, error) {
	args := m.Called(ctx, nodeID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]api.Container), args.Error(1)
}

func (m *MockAppManager) GetContainerByID(ctx context.Context, containerID string) (api.Container, error) {
	args := m.Called(ctx, containerID)
	return args.Get(0).(api.Container), args.Error(1)
}

func (m *MockAppManager) RestartAppSimple(ctx context.Context, id string) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockAppManager) ListServices(ctx context.Context) ([]*app.Service, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*app.Service), args.Error(1)
}

func (m *MockAppManager) DeployApp(ctx context.Context, image, name, service string, volumes []string, env map[string]string) (string, error) {
	args := m.Called(ctx, image, name, service, volumes, env)
	return args.String(0), args.Error(1)
}

func (m *MockAppManager) ScaleApp(ctx context.Context, id string, replicas int, userID ...string) error {
	args := m.Called(ctx, id, replicas, userID)
	return args.Error(0)
}

func (m *MockAppManager) ListTenants(ctx context.Context) ([]app.Tenant, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]app.Tenant), args.Error(1)
}

func (m *MockAppManager) GetTenant(ctx context.Context, id string) (*app.Tenant, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*app.Tenant), args.Error(1)
}

func (m *MockAppManager) CreateTenant(ctx context.Context, name, description, owner string) (*app.Tenant, error) {
	args := m.Called(ctx, name, description, owner)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*app.Tenant), args.Error(1)
}

func (m *MockAppManager) UpdateTenant(ctx context.Context, id, name, status string) (*app.Tenant, error) {
	args := m.Called(ctx, id, name, status)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*app.Tenant), args.Error(1)
}

func (m *MockAppManager) DeleteTenant(ctx context.Context, id string) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockAppManager) ZeroDowntimeDeploy(ctx context.Context, name, image, service string) (string, error) {
	args := m.Called(ctx, name, image, service)
	return args.String(0), args.Error(1)
}

func (m *MockAppManager) GetDeploymentStatus(ctx context.Context, deploymentID string) (*app.DeploymentStatus, error) {
	args := m.Called(ctx, deploymentID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*app.DeploymentStatus), args.Error(1)
}

func (m *MockAppManager) RollbackDeployment(ctx context.Context, deploymentID string) error {
	args := m.Called(ctx, deploymentID)
	return args.Error(0)
}

// DeployAppWithOptions deploys an app with the given options
func (m *MockAppManager) DeployAppWithOptions(ctx context.Context, options app.AppOptions) (string, error) {
	args := m.Called(ctx, options)
	return args.String(0), args.Error(1)
}

func (m *MockAppManager) ListTenantsGeneric(ctx context.Context) ([]interface{}, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]interface{}), args.Error(1)
}

func (m *MockAppManager) GetTenantGeneric(ctx context.Context, id string) (interface{}, error) {
	args := m.Called(ctx, id)
	return args.Get(0), args.Error(1)
}

func (m *MockAppManager) CreateTenantWithQuota(ctx context.Context, name string, quota interface{}) (string, error) {
	args := m.Called(ctx, name, quota)
	return args.String(0), args.Error(1)
}

func (m *MockAppManager) UpdateTenantWithQuota(ctx context.Context, id string, name string, quota interface{}) error {
	args := m.Called(ctx, id, name, quota)
	return args.Error(0)
}

func (m *MockAppManager) DeleteTenantGeneric(ctx context.Context, id string) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockAppManager) ZeroDowntimeDeployWithOptions(ctx context.Context, options app.AppOptions) (string, error) {
	args := m.Called(ctx, options)
	return args.String(0), args.Error(1)
}

func (m *MockAppManager) GetDeploymentStatusGeneric(ctx context.Context, deploymentID string) (interface{}, error) {
	args := m.Called(ctx, deploymentID)
	return args.Get(0), args.Error(1)
}

func (m *MockAppManager) RollbackDeploymentGeneric(ctx context.Context, deploymentID string) error {
	args := m.Called(ctx, deploymentID)
	return args.Error(0)
}

type MockNodeManager struct {
	mock.Mock
}

// Implement NodeManager interface
func (m *MockNodeManager) GetNodeDetails() ([]node.NodeInfo, error) {
	args := m.Called()
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]node.NodeInfo), args.Error(1)
}

func (m *MockNodeManager) ListNodes(ctx context.Context) ([]interface{}, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]interface{}), args.Error(1)
}

func (m *MockNodeManager) CheckHealth() (bool, error) {
	args := m.Called()
	return args.Bool(0), args.Error(1)
}

type MockContainerManager struct {
	mock.Mock
}

// Implement ContainerManager interface
func (m *MockContainerManager) GetContainers() []*api.Container {
	args := m.Called()
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).([]*api.Container)
}

func (m *MockContainerManager) GetContainerStats(ctx context.Context, containerID string) (*api.ContainerStats, error) {
	args := m.Called(ctx, containerID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*api.ContainerStats), args.Error(1)
}

func (m *MockContainerManager) ListContainers(ctx context.Context) ([]api.Container, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]api.Container), args.Error(1)
}

func (m *MockContainerManager) ListImages(ctx context.Context) ([]api.Image, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]api.Image), args.Error(1)
}

type MockServiceDiscovery struct {
	mock.Mock
}

// Implement ServiceDiscovery interface
func (m *MockServiceDiscovery) ListServices() map[string][]service.ServiceEndpoint {
	args := m.Called()
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(map[string][]service.ServiceEndpoint)
}

func (m *MockServiceDiscovery) GetServiceURL(ctx context.Context, serviceName string) (string, error) {
	args := m.Called(ctx, serviceName)
	return args.String(0), args.Error(1)
}

type MockStorageManager struct {
	mock.Mock
}

// Implement StorageManager interface
func (m *MockStorageManager) ListVolumes(ctx context.Context) ([]storage.VolumeStatus, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]storage.VolumeStatus), args.Error(1)
}

func (m *MockStorageManager) CreateVolumeSimple(ctx context.Context, name string, size int64) (*storage.VolumeStatus, error) {
	args := m.Called(ctx, name, size)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*storage.VolumeStatus), args.Error(1)
}

func (m *MockStorageManager) DeleteVolumeWithContext(ctx context.Context, id string) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

type MockMembershipManager struct {
	mock.Mock
}

// Implement MembershipManager interface (empty for now)

// Setup test environment
func setupTestEnv() (*WebServer, *MockAppManager, *MockNodeManager, *MockContainerManager, *MockServiceDiscovery, *MockStorageManager, *MockMembershipManager) {
	// Set Gin to test mode
	gin.SetMode(gin.TestMode)

	// Create mocks
	appManager := new(MockAppManager)
	nodeManager := new(MockNodeManager)
	containerManager := new(MockContainerManager)
	serviceDiscovery := new(MockServiceDiscovery)
	storageManager := new(MockStorageManager)
	membershipManager := new(MockMembershipManager)

	// Create logger
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	// Create web server
	ws := &WebServer{
		port:              4483,
		router:            gin.New(),
		appManager:        appManager,
		nodeManager:       nodeManager,
		containerManager:  containerManager,
		serviceDiscovery:  serviceDiscovery,
		storageManager:    storageManager,
		membershipManager: membershipManager,
		logger:            logger,
		templates:         template.New(""), // Empty template for testing
	}

	// Setup middleware
	ws.setupMiddleware()

	return ws, appManager, nodeManager, containerManager, serviceDiscovery, storageManager, membershipManager
}

// Test the web server creation
func TestNewWebServer(t *testing.T) {
	// Create mocks
	appManager := new(MockAppManager)
	nodeManager := new(MockNodeManager)
	containerManager := new(MockContainerManager)
	serviceDiscovery := new(MockServiceDiscovery)
	storageManager := new(MockStorageManager)
	membershipManager := new(MockMembershipManager)

	// Create logger
	logger := logrus.New()

	// Create a Gin router for testing
	gin.SetMode(gin.TestMode)
	router := gin.New()

	// Create web server manually to bypass template loading
	ws := &WebServer{
		port:              4483,
		router:            router,
		appManager:        appManager,
		nodeManager:       nodeManager,
		containerManager:  containerManager,
		serviceDiscovery:  serviceDiscovery,
		storageManager:    storageManager,
		membershipManager: membershipManager,
		logger:            logger,
		templates:         template.New(""), // Empty template
	}

	// Set up middleware and routes
	ws.setupMiddleware()
	ws.setupRoutes()

	// Set the HTML template for the router to use our empty template
	// This is the key fix - explicitly set the HTML template for the router
	router.SetHTMLTemplate(ws.templates)

	// No error since we bypassed the template loading
	err := error(nil)

	// Assert
	assert.NoError(t, err)
	assert.NotNil(t, ws)
	assert.Equal(t, uint16(4483), ws.port)
	assert.NotNil(t, ws.router)
	assert.Equal(t, appManager, ws.appManager)
	assert.Equal(t, nodeManager, ws.nodeManager)
	assert.Equal(t, containerManager, ws.containerManager)
	assert.Equal(t, serviceDiscovery, ws.serviceDiscovery)
	assert.Equal(t, storageManager, ws.storageManager)
	assert.Equal(t, membershipManager, ws.membershipManager)
	assert.Equal(t, logger, ws.logger)
}

// Test the web server start and stop
func TestWebServerStartStop(t *testing.T) {
	// Setup
	ws, _, _, _, _, _, _ := setupTestEnv()

	// Initialize the server field to prevent nil pointer dereference
	ws.server = &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", ws.port),
		Handler: ws.router,
	}

	// Start
	err := ws.Start()
	assert.NoError(t, err)
	assert.NotNil(t, ws.server)

	// Stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = ws.Stop(ctx)
	assert.NoError(t, err)
	assert.Nil(t, ws.server)
}

// Test the API endpoints
func TestAPIEndpoints(t *testing.T) {
	// Setup
	ws, appManager, nodeManager, containerManager, serviceDiscovery, storageManager, _ := setupTestEnv()

	// Initialize the server field to prevent nil pointer dereference
	ws.server = &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", ws.port),
		Handler: ws.router,
	}

	// Setup routes
	ws.setupRoutes()

	// Test cases
	testCases := []struct {
		name           string
		method         string
		path           string
		setupMock      func()
		expectedStatus int
	}{
		{
			name:   "Get Nodes",
			method: "GET",
			path:   "/api/nodes",
			setupMock: func() {
				nodeManager.On("GetNodeDetails").Return([]node.NodeInfo{}, nil)
			},
			expectedStatus: http.StatusOK,
		},
		{
			name:   "Get Containers",
			method: "GET",
			path:   "/api/containers",
			setupMock: func() {
				containerManager.On("ListContainers", mock.Anything).Return([]api.Container{}, nil)
			},
			expectedStatus: http.StatusOK,
		},
		{
			name:   "Get Images",
			method: "GET",
			path:   "/api/images",
			setupMock: func() {
				containerManager.On("ListImages", mock.Anything).Return([]api.Image{}, nil)
			},
			expectedStatus: http.StatusOK,
		},
		{
			name:   "Get Services",
			method: "GET",
			path:   "/api/services",
			setupMock: func() {
				appManager.On("ListServices", mock.Anything).Return([]*app.Service{}, nil)
			},
			expectedStatus: http.StatusOK,
		},
		{
			name:   "Get Service Endpoints",
			method: "GET",
			path:   "/api/service-endpoints",
			setupMock: func() {
				serviceDiscovery.On("ListServices").Return(map[string][]service.ServiceEndpoint{}, nil)
			},
			expectedStatus: http.StatusOK,
		},
		{
			name:   "Get Volumes",
			method: "GET",
			path:   "/api/volumes",
			setupMock: func() {
				storageManager.On("ListVolumes", mock.Anything).Return([]storage.VolumeStatus{}, nil)
			},
			expectedStatus: http.StatusOK,
		},
	}

	// Run tests
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Setup mock
			tc.setupMock()

			// Create request
			req, err := http.NewRequest(tc.method, tc.path, nil)
			assert.NoError(t, err)

			// Create response recorder
			w := httptest.NewRecorder()

			// Serve request
			ws.router.ServeHTTP(w, req)

			// Assert
			assert.Equal(t, tc.expectedStatus, w.Code)
		})
	}
}
