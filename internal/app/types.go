package app

import (
	"time"
)

// App represents an application in the system
type App struct {
	ID        string
	Name      string
	Image     string
	Command   []string
	Env       map[string]string
	Volumes   []string
	Ports     map[string]string
	Resources ResourceRequirements
	State     AppState
	NodeID    string
	CreatedAt time.Time
	StartedAt *time.Time
	StoppedAt *time.Time
	TenantID  string
	UserID    string
	Metadata  map[string]string
}
