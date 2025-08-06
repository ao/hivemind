package integration

import (
	"context"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/containerd"
	"github.com/ao/hivemind/internal/docker"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func TestDockerRuntime(t *testing.T) {
	// Skip if not running integration tests
	if testing.Short() {
		t.Skip("Skipping Docker integration test in short mode")
	}

	// Create a logger for testing
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	// Create the Docker manager
	manager := docker.NewManager(logger)

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Test pulling an image
	t.Run("PullImage", func(t *testing.T) {
		// This test is skipped by default as it requires Docker to be installed
		t.Skip("Skipping Docker pull image test as it requires Docker to be installed")

		err := manager.PullImage(ctx, "hello-world:latest")
		assert.NoError(t, err)
	})

	// Test creating a container
	t.Run("CreateContainer", func(t *testing.T) {
		// This test is skipped by default as it requires Docker to be installed
		t.Skip("Skipping Docker create container test as it requires Docker to be installed")

		// Create a simple container
		containerID, err := manager.CreateContainer(ctx, containerd.ContainerOptions{
			Name:  "test-container",
			Image: "hello-world:latest",
		})
		assert.NoError(t, err)
		assert.NotEmpty(t, containerID)

		// Clean up
		err = manager.StopContainer(ctx, containerID)
		assert.NoError(t, err)
	})

	// Test listing containers
	t.Run("ListContainers", func(t *testing.T) {
		// This test is skipped by default as it requires Docker to be installed
		t.Skip("Skipping Docker list containers test as it requires Docker to be installed")

		containers, err := manager.ListContainers(ctx)
		assert.NoError(t, err)
		assert.NotNil(t, containers)
	})

	// Test creating and listing volumes
	t.Run("VolumeOperations", func(t *testing.T) {
		// This test is skipped by default as it requires Docker to be installed
		t.Skip("Skipping Docker volume operations test as it requires Docker to be installed")

		// Create a volume
		volumeName := "test-volume-" + time.Now().Format("20060102150405")
		err := manager.CreateVolume(ctx, volumeName, nil)
		assert.NoError(t, err)

		// List volumes
		volumes, err := manager.ListVolumes(ctx)
		assert.NoError(t, err)
		assert.NotNil(t, volumes)

		// Verify our volume is in the list
		found := false
		for _, v := range volumes {
			if v.Name == volumeName {
				found = true
				break
			}
		}
		assert.True(t, found, "Created volume not found in list")

		// Delete the volume
		err = manager.DeleteVolume(ctx, volumeName)
		assert.NoError(t, err)
	})
}
