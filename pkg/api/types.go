package api

import "time"

// Container represents a container in the Hivemind system
type Container struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	Command   []string          `json:"command,omitempty"`
	Status    ContainerStatus   `json:"status"`
	CreatedAt time.Time         `json:"created_at"`
	StartedAt *time.Time        `json:"started_at,omitempty"`
	NodeID    string            `json:"node_id"`
	Labels    map[string]string `json:"labels,omitempty"`
	Ports     []PortMapping     `json:"ports,omitempty"`
	Resources ResourceSpec      `json:"resources"`
}

// ContainerStatus represents the status of a container
type ContainerStatus string

const (
	// ContainerCreated indicates the container has been created but not started
	ContainerCreated ContainerStatus = "created"
	// ContainerRunning indicates the container is running
	ContainerRunning ContainerStatus = "running"
	// ContainerStopped indicates the container has stopped
	ContainerStopped ContainerStatus = "stopped"
	// ContainerPaused indicates the container is paused
	ContainerPaused ContainerStatus = "paused"
	// ContainerFailed indicates the container has failed
	ContainerFailed ContainerStatus = "failed"
)

// PortMapping represents a port mapping for a container
type PortMapping struct {
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port"`
	Protocol      string `json:"protocol"`
}

// ResourceSpec represents resource specifications for a container
type ResourceSpec struct {
	CPU    float64 `json:"cpu"`
	Memory int64   `json:"memory"`
	Disk   int64   `json:"disk"`
}

// Node represents a node in the Hivemind cluster
type Node struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Address   string            `json:"address"`
	Status    NodeStatus        `json:"status"`
	Labels    map[string]string `json:"labels,omitempty"`
	Resources ResourceStatus    `json:"resources"`
	JoinedAt  time.Time         `json:"joined_at"`
}

// NodeStatus represents the status of a node
type NodeStatus string

const (
	// NodeReady indicates the node is ready to accept containers
	NodeReady NodeStatus = "ready"
	// NodeNotReady indicates the node is not ready to accept containers
	NodeNotReady NodeStatus = "not_ready"
	// NodeMaintenance indicates the node is in maintenance mode
	NodeMaintenance NodeStatus = "maintenance"
	// NodeDraining indicates the node is draining containers
	NodeDraining NodeStatus = "draining"
)

// ResourceStatus represents the resource status of a node
type ResourceStatus struct {
	CPU     ResourceStatusDetail `json:"cpu"`
	Memory  ResourceStatusDetail `json:"memory"`
	Disk    ResourceStatusDetail `json:"disk"`
	Network ResourceStatusDetail `json:"network"`
}

// ResourceStatusDetail represents the detail of a resource status
type ResourceStatusDetail struct {
	Capacity int64 `json:"capacity"`
	Used     int64 `json:"used"`
}

// Service represents a service in the Hivemind system
type Service struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Command     []string          `json:"command,omitempty"`
	Replicas    int               `json:"replicas"`
	Labels      map[string]string `json:"labels,omitempty"`
	Ports       []PortMapping     `json:"ports,omitempty"`
	Resources   ResourceSpec      `json:"resources"`
	Environment map[string]string `json:"environment,omitempty"`
	Volumes     []VolumeMount     `json:"volumes,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// VolumeMount represents a volume mount for a container
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
	ReadOnly  bool   `json:"read_only"`
}

// Volume represents a volume in the Hivemind system
type Volume struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Size      int64             `json:"size"`
	Type      string            `json:"type"`
	Status    VolumeStatus      `json:"status"`
	NodeID    string            `json:"node_id"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// VolumeStatus represents the status of a volume
type VolumeStatus string

const (
	// VolumeAvailable indicates the volume is available
	VolumeAvailable VolumeStatus = "available"
	// VolumeInUse indicates the volume is in use
	VolumeInUse VolumeStatus = "in_use"
	// VolumeFailed indicates the volume has failed
	VolumeFailed VolumeStatus = "failed"
)

// Error represents an API error
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ServiceConfig represents a service configuration
type ServiceConfig struct {
	Name            string   `json:"name"`
	Domain          string   `json:"domain"`
	ContainerIDs    []string `json:"container_ids"`
	DesiredReplicas int      `json:"desired_replicas"`
	CurrentReplicas int      `json:"current_replicas"`
}

// ContainerStats represents statistics for a container
type ContainerStats struct {
	CPU     float64 `json:"cpu"`
	Memory  int64   `json:"memory"`
	Disk    int64   `json:"disk"`
	Network struct {
		RxBytes int64 `json:"rx_bytes"`
		TxBytes int64 `json:"tx_bytes"`
	} `json:"network"`
	Timestamp time.Time `json:"timestamp"`
}

// Image represents a container image
type Image struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Tag       string            `json:"tag"`
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	CreatedAt time.Time         `json:"created_at"`
	Labels    map[string]string `json:"labels,omitempty"`
}
