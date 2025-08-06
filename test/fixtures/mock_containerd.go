package fixtures

import (
	"context"
	"fmt"
	"time"

	"github.com/ao/hivemind/internal/containerd"
	"github.com/ao/hivemind/pkg/api"
	"github.com/sirupsen/logrus"
)

// MockContainerdManager is a mock implementation of the containerd manager
type MockContainerdManager struct {
	logger *logrus.Logger
}

// NewMockContainerdManager creates a new mock containerd manager
func NewMockContainerdManager(logger *logrus.Logger) *MockContainerdManager {
	if logger == nil {
		logger = logrus.New()
		logger.SetLevel(logrus.InfoLevel)
	}

	return &MockContainerdManager{
		logger: logger,
	}
}

// PullImage mocks pulling an image
func (m *MockContainerdManager) PullImage(ctx context.Context, image string) error {
	m.logger.WithField("image", image).Info("Mock: Pulled image")
	return nil
}

// CreateContainer mocks creating a container
func (m *MockContainerdManager) CreateContainer(ctx context.Context, opts containerd.ContainerOptions) (string, error) {
	containerID := fmt.Sprintf("mock-container-%s", opts.Name)
	m.logger.WithFields(logrus.Fields{
		"container_id": containerID,
		"name":         opts.Name,
		"image":        opts.Image,
	}).Info("Mock: Created container")
	return containerID, nil
}

// CreateContainerWithOpts mocks creating a container with options
func (m *MockContainerdManager) CreateContainerWithOpts(ctx context.Context, opts containerd.ContainerOptions) (string, error) {
	// Just delegate to CreateContainer
	return m.CreateContainer(ctx, opts)
}

// StopContainer mocks stopping a container
func (m *MockContainerdManager) StopContainer(ctx context.Context, containerID string) error {
	m.logger.WithField("container_id", containerID).Info("Mock: Stopped container")
	return nil
}

// ListContainers mocks listing containers
func (m *MockContainerdManager) ListContainers(ctx context.Context) ([]*containerd.Container, error) {
	containers := []*containerd.Container{
		{
			ID:        "mock-container-1",
			Name:      "mock-container-1",
			Image:     "mock-image:latest",
			Status:    containerd.ContainerStatusRunning,
			NodeID:    "test-node",
			CreatedAt: time.Now(),
		},
		{
			ID:        "mock-container-2",
			Name:      "mock-container-2",
			Image:     "mock-image:latest",
			Status:    containerd.ContainerStatusRunning,
			NodeID:    "test-node",
			CreatedAt: time.Now(),
		},
	}
	m.logger.Info("Mock: Listed containers")
	return containers, nil
}

// GetContainerMetrics mocks getting container metrics
func (m *MockContainerdManager) GetContainerMetrics(ctx context.Context, containerID string) (*containerd.ContainerStats, error) {
	stats := &containerd.ContainerStats{
		CPUUsage:    25.5,
		MemoryUsage: 512 * 1024 * 1024, // 512MB
		NetworkRx:   1024 * 1024,       // 1MB
		NetworkTx:   2 * 1024 * 1024,   // 2MB
		LastUpdated: time.Now().Unix(),
	}
	m.logger.WithField("container_id", containerID).Info("Mock: Got container metrics")
	return stats, nil
}

// CreateVolume mocks creating a volume
func (m *MockContainerdManager) CreateVolume(ctx context.Context, name string, opts map[string]string) error {
	m.logger.WithField("volume", name).Info("Mock: Created volume")
	return nil
}

// DeleteVolume mocks deleting a volume
func (m *MockContainerdManager) DeleteVolume(ctx context.Context, name string) error {
	m.logger.WithField("volume", name).Info("Mock: Deleted volume")
	return nil
}

// ListVolumes mocks listing volumes
func (m *MockContainerdManager) ListVolumes(ctx context.Context) ([]*containerd.Volume, error) {
	volumes := []*containerd.Volume{
		{
			Name:      "mock-volume-1",
			Path:      "/var/lib/hivemind/volumes/mock-volume-1",
			Size:      1024 * 1024 * 1024, // 1GB
			CreatedAt: time.Now().Unix(),
		},
		{
			Name:      "mock-volume-2",
			Path:      "/var/lib/hivemind/volumes/mock-volume-2",
			Size:      2 * 1024 * 1024 * 1024, // 2GB
			CreatedAt: time.Now().Unix(),
		},
	}
	m.logger.Info("Mock: Listed volumes")
	return volumes, nil
}

// Close mocks closing the containerd manager
func (m *MockContainerdManager) Close() error {
	m.logger.Info("Mock: Closed containerd manager")
	return nil
}

// ListImages mocks listing images
func (m *MockContainerdManager) ListImages(ctx context.Context) ([]api.Image, error) {
	images := []api.Image{
		{
			ID:        "mock-image-1",
			Name:      "mock-image",
			Tag:       "latest",
			Digest:    "sha256:mock1",
			Size:      1024 * 1024 * 10, // 10MB
			CreatedAt: time.Now().Add(-24 * time.Hour),
			Labels:    map[string]string{"env": "test"},
		},
		{
			ID:        "mock-image-2",
			Name:      "mock-image",
			Tag:       "v1.0",
			Digest:    "sha256:mock2",
			Size:      1024 * 1024 * 20, // 20MB
			CreatedAt: time.Now().Add(-48 * time.Hour),
			Labels:    map[string]string{"env": "prod"},
		},
	}
	m.logger.Info("Mock: Listed images")
	return images, nil
}
