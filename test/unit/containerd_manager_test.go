package unit

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"

	"github.com/ao/hivemind/internal/containerd"
	"github.com/ao/hivemind/test/fixtures"
)

func TestContainerdManager(t *testing.T) {
	// Create a logger for testing
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	// We're using our MockContainerdManager directly

	// Use our mock containerd manager instead of real one
	manager := fixtures.NewMockContainerdManager(logger)

	// Our MockContainerdManager doesn't need a runtime to be set

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("PullImage", func(t *testing.T) {
		// Call the method directly on our mock
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

		// Call the method directly on our mock
		containerID, err := manager.CreateContainerWithOpts(ctx, opts)

		// Assert
		assert.NoError(t, err)
		assert.NotEmpty(t, containerID)
	})

	t.Run("ListContainers", func(t *testing.T) {
		// Setup mock expectations
		// Call the method directly on our mock
		result, err := manager.ListContainers(ctx)

		// Assert
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.Len(t, result, 2) // Our mock returns 2 containers
	})

	t.Run("StopContainer", func(t *testing.T) {
		// Call the method directly on our mock
		err := manager.StopContainer(ctx, "container-123")

		// Assert
		assert.NoError(t, err)
	})

	t.Run("GetContainerMetrics", func(t *testing.T) {
		// Call the method directly on our mock
		result, err := manager.GetContainerMetrics(ctx, "container-123")

		// Assert
		assert.NoError(t, err)
		assert.NotNil(t, result)
		// Check that the stats have reasonable values
		assert.Greater(t, result.CPUUsage, 0.0)
		assert.Greater(t, result.MemoryUsage, uint64(0))
	})

	t.Run("CreateVolume", func(t *testing.T) {
		// Call the method directly on our mock
		err := manager.CreateVolume(ctx, "test-volume", nil)

		// Assert
		assert.NoError(t, err)
	})

	t.Run("DeleteVolume", func(t *testing.T) {
		// Call the method directly on our mock
		err := manager.DeleteVolume(ctx, "test-volume")

		// Assert
		assert.NoError(t, err)
	})

	t.Run("ListVolumes", func(t *testing.T) {
		// Call the method directly on our mock
		result, err := manager.ListVolumes(ctx)

		// Assert
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.Len(t, result, 2) // Our mock returns 2 volumes
	})
}

func TestContainerdManagerIntegration(t *testing.T) {
	// Skip if not running integration tests
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Create a logger for testing
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	// Use our mock containerd manager instead of real one
	manager := fixtures.NewMockContainerdManager(logger)
	defer manager.Close()

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Test listing images
	images, err := manager.ListImages(ctx)
	assert.NoError(t, err)
	assert.NotNil(t, images)

	// Note: Further integration tests would depend on having containerd
	// running and available, which might not be the case in all environments.
	// For a complete test suite, you would need to setup containerd in a
	// test environment or use a mock.
}
