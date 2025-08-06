package web

import (
	"context"

	"github.com/ao/hivemind/internal/app"
	"github.com/ao/hivemind/internal/node"
	"github.com/ao/hivemind/internal/service"
	"github.com/ao/hivemind/internal/storage"
	"github.com/ao/hivemind/pkg/api"
)

// AppManager defines the interface for application management
type AppManager interface {
	GetService(ctx context.Context, name string) (*app.Service, error)
	RestartApp(ctx context.Context, id, userID, tenantID string) error
	RestartAppSimple(ctx context.Context, id string) error
	GetContainersByNode(ctx context.Context, nodeID string) ([]api.Container, error)
	GetContainerByID(ctx context.Context, containerID string) (api.Container, error)
	ListServices(ctx context.Context) ([]*app.Service, error)
	DeployApp(ctx context.Context, image, name, service string, volumes []string, env map[string]string) (string, error)
	ScaleApp(ctx context.Context, id string, replicas int, userID ...string) error
	ListTenants(ctx context.Context) ([]app.Tenant, error)
	GetTenant(ctx context.Context, id string) (*app.Tenant, error)
	CreateTenant(ctx context.Context, name, description, owner string) (*app.Tenant, error)
	UpdateTenant(ctx context.Context, id, name, status string) (*app.Tenant, error)
	DeleteTenant(ctx context.Context, id string) error
	ZeroDowntimeDeploy(ctx context.Context, name, image, service string) (string, error)
	GetDeploymentStatus(ctx context.Context, deploymentID string) (*app.DeploymentStatus, error)
	RollbackDeployment(ctx context.Context, deploymentID string) error
}

// NodeManager defines the interface for node management
type NodeManager interface {
	GetNodeDetails() ([]node.NodeInfo, error)
	CheckHealth() (bool, error)
	ListNodes(ctx context.Context) ([]interface{}, error)
}

// ContainerManager defines the interface for container management
type ContainerManager interface {
	GetContainers() []*api.Container
	GetContainerStats(ctx context.Context, containerID string) (*api.ContainerStats, error)
	ListContainers(ctx context.Context) ([]api.Container, error)
	ListImages(ctx context.Context) ([]api.Image, error)
}

// ServiceDiscovery defines the interface for service discovery
type ServiceDiscovery interface {
	ListServices() map[string][]service.ServiceEndpoint
	GetServiceURL(ctx context.Context, serviceName string) (string, error)
}

// StorageManager defines the interface for storage management
type StorageManager interface {
	ListVolumes(ctx context.Context) ([]storage.VolumeStatus, error)
	CreateVolumeSimple(ctx context.Context, name string, size int64) (*storage.VolumeStatus, error)
	DeleteVolumeWithContext(ctx context.Context, id string) error
}

// MembershipManager defines the interface for membership management
type MembershipManager interface {
}
