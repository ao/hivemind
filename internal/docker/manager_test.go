package docker

import (
	"context"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/containerd"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func TestDockerManager(t *testing.T) {
	// Create a logger for testing
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	// Create the Docker manager
	manager := NewManager(logger)

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("PullImage", func(t *testing.T) {
		// Call the method
		err := manager.PullImage(ctx, "nginx:latest")

		// Assert
		assert.NoError(t, err)
	})

	t.Run("CreateContainer", func(t *testing.T) {
		// Setup container options
		opts := containerd.ContainerOptions{
			Name:  "test-container",
			Image: "nginx:latest",
			Env: []containerd.EnvVar{
				{Key: "ENV_VAR", Value: "value"},
			},
			Ports: []containerd.PortMapping{
				{
					ContainerPort: 80,
					HostPort:      8080,
					Protocol:      "tcp",
				},
			},
		}

		// Call the method
		containerID, err := manager.CreateContainer(ctx, opts)

		// Assert
		assert.NoError(t, err)
		assert.Contains(t, containerID, "docker-test-container-")
	})

	t.Run("StopContainer", func(t *testing.T) {
		// Call the method
		err := manager.StopContainer(ctx, "container-123")

		// Assert
		assert.NoError(t, err)
	})

	t.Run("ListContainers", func(t *testing.T) {
		// Call the method
		result, err := manager.ListContainers(ctx)

		// Assert
		assert.NoError(t, err)
		assert.Len(t, result, 2)
		assert.Equal(t, "docker-container-1", result[0].ID)
		assert.Equal(t, "mock-container-1", result[0].Name)
	})

	t.Run("GetContainerMetrics", func(t *testing.T) {
		// Call the method
		result, err := manager.GetContainerMetrics(ctx, "container-123")

		// Assert
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.Equal(t, 5.0, result.CPUUsage)
		assert.Equal(t, uint64(100*1024*1024), result.MemoryUsage)
	})

	t.Run("CreateVolume", func(t *testing.T) {
		// Call the method
		err := manager.CreateVolume(ctx, "test-volume", nil)

		// Assert
		assert.NoError(t, err)
	})

	t.Run("DeleteVolume", func(t *testing.T) {
		// Call the method
		err := manager.DeleteVolume(ctx, "test-volume")

		// Assert
		assert.NoError(t, err)
	})

	t.Run("ListVolumes", func(t *testing.T) {
		// Call the method
		result, err := manager.ListVolumes(ctx)

		// Assert
		assert.NoError(t, err)
		assert.Len(t, result, 2)
		assert.Equal(t, "docker-volume-1", result[0].Name)
		assert.Equal(t, "/var/lib/docker/volumes/docker-volume-1/_data", result[0].Path)
	})
}
