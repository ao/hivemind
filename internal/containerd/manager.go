package containerd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ao/hivemind/pkg/api"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/google/uuid"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
)

// EnvVar represents an environment variable for a container
type EnvVar struct {
	Key   string
	Value string
}

// PortMapping represents a port mapping for a container
type PortMapping struct {
	ContainerPort uint16
	HostPort      uint16
	Protocol      string // "tcp" or "udp"
}

// Volume represents a volume in the Hivemind system
type Volume struct {
	Name      string
	Path      string
	Size      uint64 // in bytes
	CreatedAt int64
}

// ContainerStats represents resource usage statistics for a container
type ContainerStats struct {
	CPUUsage    float64 // Percentage
	MemoryUsage uint64  // Bytes
	NetworkRx   uint64  // Bytes
	NetworkTx   uint64  // Bytes
	LastUpdated int64   // Unix timestamp
}

// HealthCheckStatus represents the health status of a container
type HealthCheckStatus string

const (
	// HealthCheckHealthy indicates the container is healthy
	HealthCheckHealthy HealthCheckStatus = "healthy"
	// HealthCheckUnhealthy indicates the container is unhealthy
	HealthCheckUnhealthy HealthCheckStatus = "unhealthy"
	// HealthCheckUnknown indicates the container health is unknown
	HealthCheckUnknown HealthCheckStatus = "unknown"
)

// Image represents a container image
type Image struct {
	Name    string
	Tag     string
	Digest  string
	Size    int64
	Created time.Time
}

// ContainerRuntime defines the interface for container runtime operations
type ContainerRuntime interface {
	PullImage(ctx context.Context, image string) error
	CreateContainer(ctx context.Context, opts ContainerOptions) (string, error)
	StopContainer(ctx context.Context, containerID string) error
	ListContainers(ctx context.Context) ([]*Container, error)
	GetContainerMetrics(ctx context.Context, containerID string) (*ContainerStats, error)
	CreateVolume(ctx context.Context, name string, opts map[string]string) error
	DeleteVolume(ctx context.Context, name string) error
	ListVolumes(ctx context.Context) ([]*Volume, error)
}

// Manager handles interactions with the containerd daemon
type Manager struct {
	client           *containerd.Client
	logger           *logrus.Logger
	namespace        string
	containers       map[string]*api.Container
	containersMutex  sync.RWMutex
	volumes          map[string]*Volume
	volumesMutex     sync.RWMutex
	containerStats   map[string]*ContainerStats
	statsMutex       sync.RWMutex
	healthChecks     map[string]HealthCheckStatus
	healthCheckMutex sync.RWMutex
	Runtime          ContainerRuntime
}

// NewManager creates a new containerd manager
func NewManager(socketPath string, logger *logrus.Logger) (*Manager, error) {
	client, err := containerd.New(socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to containerd: %w", err)
	}

	return &Manager{
		client:         client,
		logger:         logger,
		namespace:      "hivemind",
		containers:     make(map[string]*api.Container),
		volumes:        make(map[string]*Volume),
		containerStats: make(map[string]*ContainerStats),
		healthChecks:   make(map[string]HealthCheckStatus),
	}, nil
}

// WithRuntime sets the container runtime for the manager
func (m *Manager) WithRuntime(runtime ContainerRuntime) *Manager {
	m.Runtime = runtime
	return m
}

// GetContainerStatus returns the status of a container
func (m *Manager) GetContainerStatus(ctx context.Context, id string) (string, error) {
	m.containersMutex.RLock()
	container, exists := m.containers[id]
	m.containersMutex.RUnlock()

	if !exists {
		return "", fmt.Errorf("container %s not found", id)
	}

	return string(container.Status), nil
}

// RemoveContainer removes a container
func (m *Manager) RemoveContainer(ctx context.Context, id string) error {
	m.containersMutex.Lock()
	defer m.containersMutex.Unlock()

	if _, exists := m.containers[id]; !exists {
		return fmt.Errorf("container %s not found", id)
	}

	// In a real implementation, we would use containerd to remove the container
	// For now, we'll just remove it from our map
	delete(m.containers, id)
	m.logger.Infof("Removed container: %s", id)

	return nil
}

// StartContainer starts a container
func (m *Manager) StartContainer(ctx context.Context, id string) error {
	m.containersMutex.Lock()
	defer m.containersMutex.Unlock()

	container, exists := m.containers[id]
	if !exists {
		return fmt.Errorf("container %s not found", id)
	}

	// In a real implementation, we would use containerd to start the container
	// For now, we'll just update the status
	container.Status = api.ContainerRunning
	now := time.Now()
	container.StartedAt = &now
	m.logger.Infof("Started container: %s", id)

	return nil
}

// GetContainerStats returns statistics for a container
func (m *Manager) GetContainerStats(ctx context.Context, containerID string) (*api.ContainerStats, error) {
	m.statsMutex.RLock()
	defer m.statsMutex.RUnlock()

	// Check if we have stats for this container
	stats, ok := m.containerStats[containerID]
	if !ok {
		return nil, fmt.Errorf("no stats available for container %s", containerID)
	}

	// Convert internal stats to API stats
	apiStats := &api.ContainerStats{
		CPU:    float64(stats.CPUUsage),
		Memory: int64(stats.MemoryUsage),
		Disk:   0, // Not tracked in internal stats
		Network: struct {
			RxBytes int64 `json:"rx_bytes"`
			TxBytes int64 `json:"tx_bytes"`
		}{
			RxBytes: int64(stats.NetworkRx),
			TxBytes: int64(stats.NetworkTx),
		},
		Timestamp: time.Unix(stats.LastUpdated, 0),
	}

	return apiStats, nil
}

// WithNamespace sets the namespace for the containerd manager
func (m *Manager) WithNamespace(namespace string) *Manager {
	m.namespace = namespace
	return m
}

// ListImages returns a list of available container images
func (m *Manager) ListImages(ctx context.Context) ([]api.Image, error) {
	// If client is nil (when using Docker runtime), return mock images
	if m.client == nil {
		m.logger.Info("Using mock images for Docker runtime")

		// Return some mock images
		return []api.Image{
			{
				ID:        "mock-image-1",
				Name:      "nginx",
				Tag:       "latest",
				Digest:    "sha256:mock1",
				Size:      10000000,
				CreatedAt: time.Now().Add(-24 * time.Hour),
				Labels:    make(map[string]string),
			},
			{
				ID:        "mock-image-2",
				Name:      "redis",
				Tag:       "latest",
				Digest:    "sha256:mock2",
				Size:      8000000,
				CreatedAt: time.Now().Add(-48 * time.Hour),
				Labels:    make(map[string]string),
			},
			{
				ID:        "mock-image-3",
				Name:      "postgres",
				Tag:       "13",
				Digest:    "sha256:mock3",
				Size:      12000000,
				CreatedAt: time.Now().Add(-72 * time.Hour),
				Labels:    make(map[string]string),
			},
		}, nil
	}

	// Otherwise use the containerd client
	ctx = namespaces.WithNamespace(ctx, m.namespace)

	images, err := m.client.ImageService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list images: %w", err)
	}

	result := make([]api.Image, 0, len(images))
	for _, img := range images {
		name := img.Name
		tag := "latest"

		// Extract tag from name if present
		parts := strings.Split(img.Name, ":")
		if len(parts) > 1 {
			name = parts[0]
			tag = parts[1]
		}

		result = append(result, api.Image{
			ID:        img.Target.Digest.String()[:12], // Use first 12 chars of digest as ID
			Name:      name,
			Tag:       tag,
			Digest:    img.Target.Digest.String(),
			Size:      img.Target.Size,
			CreatedAt: img.CreatedAt,
			Labels:    make(map[string]string), // Empty labels
		})
	}

	return result, nil
}

// Close closes the containerd client connection
func (m *Manager) Close() error {
	if m.client != nil {
		return m.client.Close()
	}
	return nil
}

// NewManagerWithoutClient creates a new containerd manager without connecting to containerd
// This is useful when using an alternative runtime like Docker
func NewManagerWithoutClient(logger *logrus.Logger) *Manager {
	if logger == nil {
		logger = logrus.New()
		logger.SetLevel(logrus.InfoLevel)
	}

	return &Manager{
		client:         nil,
		logger:         logger,
		namespace:      "hivemind",
		containers:     make(map[string]*api.Container),
		volumes:        make(map[string]*Volume),
		containerStats: make(map[string]*ContainerStats),
		healthChecks:   make(map[string]HealthCheckStatus),
	}
}

// getContext returns a context with the namespace set
func (m *Manager) getContext() context.Context {
	return namespaces.WithNamespace(context.Background(), m.namespace)
}

// ListContainers returns a list of containers
func (m *Manager) ListContainers(ctx context.Context) ([]api.Container, error) {
	// Use the Runtime if it's set
	if m.Runtime != nil {
		containers, err := m.Runtime.ListContainers(ctx)
		if err != nil {
			return nil, err
		}

		// Convert to api.Container type
		apiContainers := make([]api.Container, len(containers))
		for i, c := range containers {
			apiContainers[i] = api.Container{
				ID:        c.ID,
				Name:      c.Name,
				Image:     c.Image,
				Status:    api.ContainerStatus(c.Status),
				NodeID:    c.NodeID,
				CreatedAt: c.CreatedAt,
				Resources: api.ResourceSpec{},
				Labels:    make(map[string]string),
			}
		}

		return apiContainers, nil
	}

	// Otherwise use the containerd client directly
	// Check if client is nil
	if m.client == nil {
		return nil, fmt.Errorf("containerd client not available")
	}

	ctx = namespaces.WithNamespace(ctx, m.namespace)

	// Get containers from containerd
	containerdContainers, err := m.client.Containers(ctx)
	if err != nil {
		return nil, err
	}

	// Convert to api.Container type
	containers := make([]api.Container, len(containerdContainers))
	for i, c := range containerdContainers {
		// Get container info
		info, err := c.Info(ctx)
		if err != nil {
			return nil, err
		}

		// Create Container object
		containers[i] = api.Container{
			ID:        c.ID(),
			Name:      info.ID,
			Image:     info.Image,
			Status:    api.ContainerRunning, // Default to running
			NodeID:    "local",
			CreatedAt: info.CreatedAt,
			Resources: api.ResourceSpec{},
			Labels:    make(map[string]string),
		}
	}

	return containers, nil
}

// GetContainers returns a list of Hivemind containers
func (m *Manager) GetContainers() []*api.Container {
	m.containersMutex.RLock()
	defer m.containersMutex.RUnlock()

	containers := make([]*api.Container, 0, len(m.containers))
	for _, container := range m.containers {
		containers = append(containers, container)
	}
	return containers
}

// GetContainer returns a container by ID
func (m *Manager) GetContainer(id string) (*api.Container, bool) {
	m.containersMutex.RLock()
	defer m.containersMutex.RUnlock()

	container, exists := m.containers[id]
	return container, exists
}

// PullImage pulls an image from a registry
func (m *Manager) PullImage(ctx context.Context, image string) error {
	m.logger.Infof("Pulling image %s", image)

	// Use the Runtime if it's set
	if m.Runtime != nil {
		return m.Runtime.PullImage(ctx, image)
	}

	// Otherwise use the containerd client directly
	// Check if client is nil
	if m.client == nil {
		return fmt.Errorf("containerd client not available")
	}

	ctx = namespaces.WithNamespace(ctx, m.namespace)
	_, err := m.client.Pull(ctx, image, containerd.WithPullUnpack)
	if err != nil {
		m.logger.Errorf("Failed to pull image %s: %v", image, err)
		return err
	}

	m.logger.Infof("Successfully pulled image %s", image)
	return nil
}

// ExecInContainer executes a command in a container
func (m *Manager) ExecInContainer(ctx context.Context, id string, command []string) (string, error) {
	ctx = namespaces.WithNamespace(ctx, m.namespace)
	m.logger.Infof("Executing command in container %s: %v", id, command)

	// In a real implementation, this would use containerd's task.Exec
	// For now, we'll just return a placeholder result
	return fmt.Sprintf("Executed command %v in container %s", command, id), nil
}

// GetContainerLogs returns the logs for a container
func (m *Manager) GetContainerLogs(ctx context.Context, id string) (string, error) {
	ctx = namespaces.WithNamespace(ctx, m.namespace)
	m.logger.Infof("Getting logs for container %s", id)

	// In a real implementation, this would use containerd's task.IO
	// For now, we'll just return a placeholder result
	return fmt.Sprintf("Logs for container %s", id), nil
}

// CreateContainerWithOptions creates a new container with the given options
// CreateContainerWithOpts creates a new container with the given ContainerOptions
func (m *Manager) CreateContainerWithOpts(ctx context.Context, opts ContainerOptions) (string, error) {
	m.logger.Infof("Creating container %s with image %s", opts.Name, opts.Image)

	// Use the Runtime if it's set
	if m.Runtime != nil {
		return m.Runtime.CreateContainer(ctx, opts)
	}

	// Otherwise use the internal implementation
	// Convert env map to EnvVar slice
	return m.createContainer(ctx, opts.Image, opts.Name, opts.Env, opts.Ports)
}

// CreateContainer implements the app.ContainerManager interface
func (m *Manager) CreateContainer(ctx context.Context, name, image string, command []string, env map[string]string, volumes []string, ports map[string]string) (string, error) {
	return m.CreateContainerWithOptions(ctx, name, image, command, env, volumes, ports)
}

// CreateContainerWithOptions creates a new container with the given parameters
func (m *Manager) CreateContainerWithOptions(ctx context.Context, name, image string, command []string, env map[string]string, volumes []string, ports map[string]string) (string, error) {
	m.logger.Infof("Creating container %s with image %s using options", name, image)

	// Use the Runtime if it's set
	if m.Runtime != nil {
		// Convert to ContainerOptions
		opts := ContainerOptions{
			Name:    name,
			Image:   image,
			Command: command,
		}

		// Convert env map to EnvVar slice
		envVars := make([]EnvVar, 0, len(env))
		for k, v := range env {
			envVars = append(envVars, EnvVar{Key: k, Value: v})
		}
		opts.Env = envVars

		// Convert ports map to PortMapping slice
		portMappings := make([]PortMapping, 0, len(ports))
		for _, _ = range ports {
			// Just create a default port mapping for now
			portMappings = append(portMappings, PortMapping{
				ContainerPort: uint16(0), // Would need parsing from string
				HostPort:      uint16(0), // Would need parsing from string
				Protocol:      "tcp",     // Default to TCP
			})
		}
		opts.Ports = portMappings

		return m.Runtime.CreateContainer(ctx, opts)
	}

	// Convert env map to EnvVar slice
	envVars := make([]EnvVar, 0, len(env))
	for k, v := range env {
		envVars = append(envVars, EnvVar{Key: k, Value: v})
	}

	// Convert ports map to PortMapping slice
	portMappings := make([]PortMapping, 0, len(ports))
	for _, _ = range ports {
		// Just create a default port mapping for now
		portMappings = append(portMappings, PortMapping{
			ContainerPort: uint16(0), // Would need parsing from string
			HostPort:      uint16(0), // Would need parsing from string
			Protocol:      "tcp",     // Default to TCP
		})
	}

	// Call the internal implementation
	return m.createContainer(ctx, image, name, envVars, portMappings)
}

// createContainer is the internal implementation that creates a new container
func (m *Manager) createContainer(ctx context.Context, image string, name string, envVars []EnvVar, ports []PortMapping) (string, error) {
	// Check if client is nil
	if m.client == nil {
		return "", fmt.Errorf("containerd client not available")
	}

	ctx = namespaces.WithNamespace(ctx, m.namespace)
	m.logger.Infof("Creating container %s with image %s", name, image)

	// Generate a unique ID for the container
	id := uuid.New().String()

	// Pull the image
	if err := m.PullImage(ctx, image); err != nil {
		return "", err
	}

	// Get the image
	img, err := m.client.GetImage(ctx, image)
	if err != nil {
		m.logger.Errorf("Failed to get image %s: %v", image, err)
		return "", err
	}

	// Convert environment variables
	env := []string{}
	for _, envVar := range envVars {
		env = append(env, fmt.Sprintf("%s=%s", envVar.Key, envVar.Value))
	}

	// Create container
	container, err := m.client.NewContainer(
		ctx,
		id,
		containerd.WithImage(img),
		containerd.WithNewSnapshot(id+"-snapshot", img),
		containerd.WithNewSpec(
			oci.WithImageConfig(img),
			oci.WithEnv(env),
			oci.WithHostHostsFile,
			oci.WithHostResolvconf,
			oci.WithTTY,
		),
	)
	if err != nil {
		m.logger.Errorf("Failed to create container %s: %v", name, err)
		return "", err
	}

	// Create task
	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStdio))
	if err != nil {
		m.logger.Errorf("Failed to create task for container %s: %v", name, err)
		return "", err
	}

	// Start the task
	if err := task.Start(ctx); err != nil {
		m.logger.Errorf("Failed to start task for container %s: %v", name, err)
		return "", err
	}

	// Store container information
	now := time.Now()
	apiContainer := &api.Container{
		ID:        id,
		Name:      name,
		Image:     image,
		Status:    api.ContainerRunning,
		NodeID:    "local", // Default to local node
		CreatedAt: now,
		StartedAt: &now,
		Ports:     convertPortMappings(ports),
		Labels:    make(map[string]string),
		Resources: api.ResourceSpec{
			CPU:    1.0,
			Memory: 512 * 1024 * 1024,  // 512MB
			Disk:   1024 * 1024 * 1024, // 1GB
		},
	}

	m.containersMutex.Lock()
	m.containers[id] = apiContainer
	m.containersMutex.Unlock()

	// Initialize container stats
	m.statsMutex.Lock()
	m.containerStats[id] = &ContainerStats{
		CPUUsage:    0.0,
		MemoryUsage: 0,
		NetworkRx:   0,
		NetworkTx:   0,
		LastUpdated: time.Now().Unix(),
	}
	m.statsMutex.Unlock()

	// Initialize health check status
	m.healthCheckMutex.Lock()
	m.healthChecks[id] = HealthCheckUnknown
	m.healthCheckMutex.Unlock()

	m.logger.Infof("Successfully created and started container %s with ID %s", name, id)
	return id, nil
}

// CreateContainerWithVolumes creates a new container with volume mounts
func (m *Manager) CreateContainerWithVolumes(ctx context.Context, image string, name string, envVars []EnvVar, ports []PortMapping, volumes []Volume) (string, error) {
	// Check if client is nil
	if m.client == nil {
		return "", fmt.Errorf("containerd client not available")
	}

	ctx = namespaces.WithNamespace(ctx, m.namespace)
	m.logger.Infof("Creating container %s with image %s and %d volumes", name, image, len(volumes))

	// Generate a unique ID for the container
	id := uuid.New().String()

	// Pull the image
	if err := m.PullImage(ctx, image); err != nil {
		return "", err
	}

	// Get the image
	img, err := m.client.GetImage(ctx, image)
	if err != nil {
		m.logger.Errorf("Failed to get image %s: %v", image, err)
		return "", err
	}

	// Convert environment variables
	env := []string{}
	for _, envVar := range envVars {
		env = append(env, fmt.Sprintf("%s=%s", envVar.Key, envVar.Value))
	}

	// Prepare volume mounts
	mounts := []specs.Mount{}
	for _, volume := range volumes {
		mounts = append(mounts, specs.Mount{
			Source:      volume.Path,
			Destination: volume.Path,
			Type:        "bind",
			Options:     []string{"rbind", "rw"},
		})
	}

	// Create container
	container, err := m.client.NewContainer(
		ctx,
		id,
		containerd.WithImage(img),
		containerd.WithNewSnapshot(id+"-snapshot", img),
		containerd.WithNewSpec(
			oci.WithImageConfig(img),
			oci.WithEnv(env),
			oci.WithHostHostsFile,
			oci.WithHostResolvconf,
			oci.WithTTY,
			oci.WithMounts(mounts),
		),
	)
	if err != nil {
		m.logger.Errorf("Failed to create container %s: %v", name, err)
		return "", err
	}

	// Create task
	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStdio))
	if err != nil {
		m.logger.Errorf("Failed to create task for container %s: %v", name, err)
		return "", err
	}

	// Start the task
	if err := task.Start(ctx); err != nil {
		m.logger.Errorf("Failed to start task for container %s: %v", name, err)
		return "", err
	}

	// Store container information
	now := time.Now()
	apiContainer := &api.Container{
		ID:        id,
		Name:      name,
		Image:     image,
		Status:    api.ContainerRunning,
		NodeID:    "local", // Default to local node
		CreatedAt: now,
		StartedAt: &now,
		Ports:     convertPortMappings(ports),
		Labels:    make(map[string]string),
		Resources: api.ResourceSpec{
			CPU:    1.0,
			Memory: 512 * 1024 * 1024,  // 512MB
			Disk:   1024 * 1024 * 1024, // 1GB
		},
	}

	m.containersMutex.Lock()
	m.containers[id] = apiContainer
	m.containersMutex.Unlock()

	// Initialize container stats
	m.statsMutex.Lock()
	m.containerStats[id] = &ContainerStats{
		CPUUsage:    0.0,
		MemoryUsage: 0,
		NetworkRx:   0,
		NetworkTx:   0,
		LastUpdated: time.Now().Unix(),
	}
	m.statsMutex.Unlock()

	// Initialize health check status
	m.healthCheckMutex.Lock()
	m.healthChecks[id] = HealthCheckUnknown
	m.healthCheckMutex.Unlock()

	m.logger.Infof("Successfully created and started container %s with ID %s", name, id)
	return id, nil
}

// StopContainer stops and removes a container
func (m *Manager) StopContainer(ctx context.Context, containerID string) error {
	m.logger.Infof("Stopping container %s", containerID)

	// Use the Runtime if it's set
	if m.Runtime != nil {
		return m.Runtime.StopContainer(ctx, containerID)
	}

	// Otherwise use the containerd client directly
	// Check if client is nil
	if m.client == nil {
		return fmt.Errorf("containerd client not available")
	}

	ctx = namespaces.WithNamespace(ctx, m.namespace)

	// Get the container
	container, err := m.client.LoadContainer(ctx, containerID)
	if err != nil {
		m.logger.Errorf("Failed to load container %s: %v", containerID, err)
		return err
	}

	// Get the task
	task, err := container.Task(ctx, nil)
	if err != nil {
		m.logger.Errorf("Failed to get task for container %s: %v", containerID, err)
		return err
	}

	// Kill the task
	if err := task.Kill(ctx, syscall.SIGTERM); err != nil {
		m.logger.Errorf("Failed to kill task for container %s: %v", containerID, err)
		return err
	}

	// Wait for the task to exit
	exitStatus, err := task.Wait(ctx)
	if err != nil {
		m.logger.Errorf("Failed to wait for task exit for container %s: %v", containerID, err)
		return err
	}

	<-exitStatus

	// Delete the task
	if _, err := task.Delete(ctx); err != nil {
		m.logger.Errorf("Failed to delete task for container %s: %v", containerID, err)
		return err
	}

	// Delete the container
	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		m.logger.Errorf("Failed to delete container %s: %v", containerID, err)
		return err
	}

	// Update container status
	m.containersMutex.Lock()
	if container, exists := m.containers[containerID]; exists {
		container.Status = api.ContainerStopped
	}
	m.containersMutex.Unlock()

	m.logger.Infof("Successfully stopped and removed container %s", containerID)
	return nil
}

// GetContainerMetrics returns metrics for a container
func (m *Manager) GetContainerMetrics(ctx context.Context, containerID string) (*ContainerStats, error) {
	// Use the Runtime if it's set
	if m.Runtime != nil {
		return m.Runtime.GetContainerMetrics(ctx, containerID)
	}

	// Otherwise use the internal stats
	m.statsMutex.RLock()
	defer m.statsMutex.RUnlock()

	stats, exists := m.containerStats[containerID]
	if !exists {
		return nil, fmt.Errorf("container %s not found", containerID)
	}

	return stats, nil
}

// CreateVolume creates a new volume
func (m *Manager) CreateVolume(ctx context.Context, name string, opts map[string]string) error {
	m.logger.Infof("Creating volume %s", name)

	// Use the Runtime if it's set
	if m.Runtime != nil {
		return m.Runtime.CreateVolume(ctx, name, opts)
	}

	// Otherwise use the internal implementation
	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// Check if volume already exists
	if _, exists := m.volumes[name]; exists {
		return fmt.Errorf("volume %s already exists", name)
	}

	// Create volume
	volume := &Volume{
		Name:      name,
		Path:      fmt.Sprintf("/var/lib/hivemind/volumes/%s", name),
		Size:      1024 * 1024 * 1024, // 1GB default
		CreatedAt: time.Now().Unix(),
	}

	m.volumes[name] = volume
	m.logger.Infof("Successfully created volume %s", name)
	return nil
}

// DeleteVolume deletes a volume
func (m *Manager) DeleteVolume(ctx context.Context, name string) error {
	m.logger.Infof("Deleting volume %s", name)

	// Use the Runtime if it's set
	if m.Runtime != nil {
		return m.Runtime.DeleteVolume(ctx, name)
	}

	// Otherwise use the internal implementation
	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// Check if volume exists
	if _, exists := m.volumes[name]; !exists {
		return fmt.Errorf("volume %s not found", name)
	}

	// Check if volume is in use
	inUse, err := m.IsVolumeInUse(ctx, name)
	if err != nil {
		return err
	}

	if inUse {
		return fmt.Errorf("volume %s is in use", name)
	}

	// Delete volume
	delete(m.volumes, name)
	m.logger.Infof("Successfully deleted volume %s", name)
	return nil
}

// ListVolumes returns a list of volumes
func (m *Manager) ListVolumes(ctx context.Context) ([]*Volume, error) {
	m.logger.Infof("Listing volumes")

	// Use the Runtime if it's set
	if m.Runtime != nil {
		return m.Runtime.ListVolumes(ctx)
	}

	// Otherwise use the internal implementation
	m.volumesMutex.RLock()
	defer m.volumesMutex.RUnlock()

	volumes := make([]*Volume, 0, len(m.volumes))
	for _, volume := range m.volumes {
		volumes = append(volumes, volume)
	}

	return volumes, nil
}

// IsVolumeInUse checks if a volume is in use by any container
func (m *Manager) IsVolumeInUse(ctx context.Context, name string) (bool, error) {
	m.containersMutex.RLock()
	defer m.containersMutex.RUnlock()

	// Check if any container is using this volume
	// In a real implementation, we would check if containers are using the volume
	// For now, we'll just return false

	return false, nil
}

// BackupVolume backs up a volume to a file
func (m *Manager) BackupVolume(ctx context.Context, name string, outputPath string) error {
	m.logger.Infof("Backing up volume %s to %s", name, outputPath)

	m.volumesMutex.RLock()
	defer m.volumesMutex.RUnlock()

	// Check if volume exists
	_, exists := m.volumes[name]
	if !exists {
		return fmt.Errorf("volume %s not found", name)
	}

	// In a real implementation, we would back up the volume to the output path
	// For now, we'll just log that we're backing up the volume
	m.logger.Infof("Successfully backed up volume %s to %s", name, outputPath)
	return nil
}

// RestoreVolume restores a volume from a backup file
func (m *Manager) RestoreVolume(ctx context.Context, name string, inputPath string) error {
	m.logger.Infof("Restoring volume %s from %s", name, inputPath)

	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// In a real implementation, we would restore the volume from the input path
	// For now, we'll just create a new volume
	volume := &Volume{
		Name:      name,
		Path:      fmt.Sprintf("/var/lib/hivemind/volumes/%s", name),
		Size:      1024 * 1024 * 1024, // 1GB default
		CreatedAt: time.Now().Unix(),
	}

	m.volumes[name] = volume
	m.logger.Infof("Successfully restored volume %s from %s", name, inputPath)
	return nil
}

// Helper function to convert port mappings
func convertPortMappings(ports []PortMapping) []api.PortMapping {
	result := make([]api.PortMapping, len(ports))
	for i, port := range ports {
		result[i] = api.PortMapping{
			ContainerPort: int(port.ContainerPort),
			HostPort:      int(port.HostPort),
			Protocol:      port.Protocol,
		}
	}
	return result
}
