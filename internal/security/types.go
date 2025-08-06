package security

import (
	"context"
)

// RBACManager defines the interface for RBAC management
type RBACManager interface {
	CreateUser(ctx context.Context, user User) (*User, error)
	GetUser(ctx context.Context, userID string) (*User, error)
	GetUserByUsername(ctx context.Context, username string) (*User, error)
	UpdateUser(ctx context.Context, userID string, updatedUser User) (*User, error)
	DeleteUser(ctx context.Context, userID string) error
	ListUsers(ctx context.Context) []User
	CreateRole(ctx context.Context, role Role) (*Role, error)
	GetRole(ctx context.Context, roleID string) (*Role, error)
	UpdateRole(ctx context.Context, roleID string, updatedRole Role) (*Role, error)
	DeleteRole(ctx context.Context, roleID string) error
	Authenticate(ctx context.Context, authRequest *AuthRequest) (*AuthResponse, error)
}

// ContainerScanningManager defines the interface for container scanning
type ContainerScanningManager interface {
	ScanImage(ctx context.Context, imageName, imageID string) (*ScanResult, error)
	CreatePolicy(ctx context.Context, policy SecurityPolicy) error
	GetPolicy(ctx context.Context, policyID string) (*SecurityPolicy, error)
	ListPolicies(ctx context.Context) []SecurityPolicy
	DeletePolicy(ctx context.Context, policyID string) error
	CheckContainerRuntime(ctx context.Context, containerID string) (*RuntimeCheckResult, error)
}

// ResourceQuotas represents resource quotas for a tenant
type ResourceQuotas struct {
	CPU              float64 `json:"cpu"`
	Memory           int64   `json:"memory"`
	Storage          int64   `json:"storage"`
	MaxContainers    int     `json:"max_containers"`
	MaxVolumes       int     `json:"max_volumes"`
	MaxLoadBalancers int     `json:"max_load_balancers"`
}

// ResourceUsage represents resource usage for a tenant
type ResourceUsage struct {
	CPU           float64 `json:"cpu"`
	Memory        int64   `json:"memory"`
	Storage       int64   `json:"storage"`
	Containers    int     `json:"containers"`
	Volumes       int     `json:"volumes"`
	LoadBalancers int     `json:"load_balancers"`
}

// ResourceAllocation represents a resource allocation request
type ResourceAllocation struct {
	CPU     float64 `json:"cpu"`
	Memory  int64   `json:"memory"`
	Storage int64   `json:"storage,omitempty"`
}

// SecretRotationPolicy defines the policy for secret rotation
type SecretRotationPolicy struct {
	Enabled       bool   `json:"enabled"`
	IntervalDays  int    `json:"interval_days"`
	AutoRotate    bool   `json:"auto_rotate"`
	NotifyBefore  int    `json:"notify_before"`
	LastRotatedAt int64  `json:"last_rotated_at"`
	NextRotation  int64  `json:"next_rotation"`
	SecretType    string `json:"secret_type"`
}
