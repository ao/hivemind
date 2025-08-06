package service

import (
	"time"
)

// ServiceInfo represents information about a service
type ServiceInfo struct {
	Name            string
	Namespace       string
	Version         string
	Endpoints       []ServiceEndpoint
	Labels          map[string]string
	Annotations     map[string]string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	HealthStatus    ServiceHealth
	LastHealthCheck time.Time
	Metadata        map[string]string
}
