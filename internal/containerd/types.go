package containerd

import (
	"time"
)

// ContainerOptions represents options for creating a container
type ContainerOptions struct {
	Name      string
	Image     string
	Command   []string
	Env       []EnvVar
	Ports     []PortMapping
	Volumes   []Volume
	Resources ResourceLimits
}

// ResourceLimits represents resource limits for a container
type ResourceLimits struct {
	CPU    float64
	Memory uint64
	Disk   uint64
}

// Container represents a container in the system
type Container struct {
	ID        string
	Name      string
	Image     string
	Status    ContainerStatus
	NodeID    string
	CreatedAt time.Time
	StartedAt *time.Time
	StoppedAt *time.Time
	Command   []string
	Env       []EnvVar
	Ports     []PortMapping
	Resources ResourceLimits
}

// ContainerStatus represents the status of a container
type ContainerStatus string

const (
	// ContainerStatusCreated indicates the container is created but not started
	ContainerStatusCreated ContainerStatus = "created"
	// ContainerStatusRunning indicates the container is running
	ContainerStatusRunning ContainerStatus = "running"
	// ContainerStatusStopped indicates the container is stopped
	ContainerStatusStopped ContainerStatus = "stopped"
	// ContainerStatusPaused indicates the container is paused
	ContainerStatusPaused ContainerStatus = "paused"
	// ContainerStatusFailed indicates the container failed to start or run
	ContainerStatusFailed ContainerStatus = "failed"
)
