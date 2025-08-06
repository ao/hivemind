package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ao/hivemind/pkg/api"
	_ "github.com/mattn/go-sqlite3"
	"github.com/sirupsen/logrus"
)

// AppState represents the state of an application
type AppState string

const (
	// AppStatePending indicates the application is pending
	AppStatePending AppState = "pending"
	// AppStateRunning indicates the application is running
	AppStateRunning AppState = "running"
	// AppStateStopped indicates the application is stopped
	AppStateStopped AppState = "stopped"
	// AppStateFailed indicates the application has failed
	AppStateFailed AppState = "failed"
)

// AppStatus represents the status of an application
type AppStatus struct {
	ID           string
	Name         string
	Image        string
	Command      []string
	Env          map[string]string
	Volumes      []string
	Ports        map[string]string
	Resources    ResourceRequirements
	State        AppState
	NodeID       string
	CreatedAt    int64
	StartedAt    int64
	FinishedAt   int64
	RestartCount int
	ExitCode     int
	TenantID     string
	UserID       string
}

// ResourceRequirements represents the resource requirements of an application
type ResourceRequirements struct {
	CPU    float64
	Memory int64
	GPU    int
	Disk   int64
}

// AppOptions represents options for creating an application
type AppOptions struct {
	Name      string
	Image     string
	Command   []string
	Env       map[string]string
	Volumes   []string
	Ports     map[string]string
	Resources ResourceRequirements
	TenantID  string
	UserID    string
	Metadata  map[string]string
}

// AppFilter represents a filter for listing applications
type AppFilter struct {
	TenantID string
	UserID   string
	State    AppState
	NodeID   string
}

// AppAuditLogEntry represents an entry in the application audit log
type AppAuditLogEntry struct {
	Timestamp int64
	Action    string
	UserID    string
	TenantID  string
	AppID     string
	Details   string
}

// TenantAppStats represents application statistics for a tenant
type TenantAppStats struct {
	TenantID     string
	AppCount     int
	RunningCount int
	FailedCount  int
	TotalCPU     float64
	TotalMemory  int64
	TotalGPU     int
	TotalDisk    int64
}

// Tenant represents a tenant in the system
type Tenant struct {
	ID          string
	Name        string
	Description string
	CreatedAt   int64
	UpdatedAt   int64
	Status      string
	Owner       string
	Quota       ResourceRequirements
	UsedQuota   ResourceRequirements
}

// DeploymentStatus represents the status of a deployment
type DeploymentStatus struct {
	ID        string
	Status    string
	Progress  int
	Details   string
	CreatedAt time.Time
	UpdatedAt time.Time
	Completed bool
	Success   bool
}

// Service represents a service in the application
type Service struct {
	ID           string
	Name         string
	Image        string
	Command      []string
	Env          map[string]string
	Volumes      []string
	Ports        map[string]string
	Resources    ResourceRequirements
	Replicas     int
	TenantID     string
	UserID       string
	CreatedAt    int64
	UpdatedAt    int64
	State        AppState
	Metadata     map[string]string
	Domain       string
	ContainerIDs []string
}

// AccessControl handles access control for applications
type AccessControl interface {
	CheckAccess(userID, tenantID, resource, action string) bool
	LogAccess(userID, tenantID, resource, action, details string)
	GetAuditLogs() []AppAuditLogEntry
	GetTenantAuditLogs(tenantID string) []AppAuditLogEntry
	GetAppAuditLogs(appID string) []AppAuditLogEntry
}

// ContainerManager handles container operations
type ContainerManager interface {
	CreateContainer(ctx context.Context, name, image string, command []string, env map[string]string, volumes []string, ports map[string]string) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string) error
	RemoveContainer(ctx context.Context, id string) error
	GetContainerStatus(ctx context.Context, id string) (string, error)
	GetContainerLogs(ctx context.Context, id string) (string, error)
	ExecInContainer(ctx context.Context, id string, command []string) (string, error)
	ListContainers(ctx context.Context) ([]api.Container, error)
}

// StorageManager handles storage operations
type StorageManager interface {
	CreateVolume(ctx context.Context, options interface{}) (interface{}, error)
	DeleteVolume(ctx context.Context, name string) error
	MountVolume(ctx context.Context, volumeName, containerID, mountPath string) error
	UnmountVolume(ctx context.Context, volumeName, containerID string) error
	ListVolumes(ctx context.Context) ([]interface{}, error)
	CreateVolumeSimple(ctx context.Context, name string, size int64) (interface{}, error)
}

// NetworkManager handles network operations
type NetworkManager interface {
	CreateNetwork(ctx context.Context, name string) (string, error)
	DeleteNetwork(ctx context.Context, id string) error
	ConnectContainerToNetwork(ctx context.Context, containerID, networkID string) error
	DisconnectContainerFromNetwork(ctx context.Context, containerID, networkID string) error
}

// Manager handles application operations
type Manager struct {
	db               *sql.DB
	dataDir          string
	containerManager ContainerManager
	storageManager   StorageManager
	networkManager   NetworkManager
	accessControl    AccessControl
	tenantManager    interface{}
	logger           *logrus.Logger
	appsMutex        sync.RWMutex
	services         map[string]*Service
	servicesMutex    sync.RWMutex
}

// ListServices returns a list of services
func (m *Manager) ListServices(ctx context.Context) ([]*Service, error) {
	m.servicesMutex.RLock()
	defer m.servicesMutex.RUnlock()

	services := make([]*Service, 0, len(m.services))
	for _, service := range m.services {
		services = append(services, service)
	}

	return services, nil
}

// NewManager creates a new application manager
func NewManager(dataDir string, logger *logrus.Logger) (*Manager, error) {
	// Create data directory if it doesn't exist
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	// Open database
	dbPath := filepath.Join(dataDir, "app.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Initialize database
	if err := initializeDatabase(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}

	return &Manager{
		db:      db,
		dataDir: dataDir,
		logger:  logger,
	}, nil
}

// WithContainerManager sets the container manager
func (m *Manager) WithContainerManager(containerManager ContainerManager) *Manager {
	m.containerManager = containerManager
	return m
}

// WithStorageManager sets the storage manager
func (m *Manager) WithStorageManager(storageManager StorageManager) *Manager {
	m.storageManager = storageManager
	return m
}

// WithNetworkManager sets the network manager
func (m *Manager) WithNetworkManager(networkManager NetworkManager) *Manager {
	m.networkManager = networkManager
	return m
}

// WithAccessControl sets the access control
func (m *Manager) WithAccessControl(accessControl AccessControl) *Manager {
	m.accessControl = accessControl
	return m
}

// WithTenantManager sets the tenant manager
func (m *Manager) WithTenantManager(tenantManager interface{}) *Manager {
	m.tenantManager = tenantManager
	return m
}

// Close closes the application manager
func (m *Manager) Close() error {
	return m.db.Close()
}

// initializeDatabase initializes the database schema
func initializeDatabase(db *sql.DB) error {
	// Create apps table
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS apps (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			image TEXT NOT NULL,
			command TEXT NOT NULL,
			env TEXT NOT NULL,
			volumes TEXT NOT NULL,
			ports TEXT NOT NULL,
			cpu REAL NOT NULL,
			memory INTEGER NOT NULL,
			gpu INTEGER NOT NULL,
			disk INTEGER NOT NULL,
			state TEXT NOT NULL,
			node_id TEXT,
			created_at INTEGER NOT NULL,
			started_at INTEGER,
			finished_at INTEGER,
			restart_count INTEGER NOT NULL,
			exit_code INTEGER,
			tenant_id TEXT,
			user_id TEXT,
			metadata TEXT
		)
	`)
	if err != nil {
		return err
	}

	// Create audit_logs table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS audit_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp INTEGER NOT NULL,
			action TEXT NOT NULL,
			user_id TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			app_id TEXT NOT NULL,
			details TEXT NOT NULL
		)
	`)
	if err != nil {
		return err
	}

	// Create tenant_stats table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS tenant_stats (
			tenant_id TEXT PRIMARY KEY,
			app_count INTEGER NOT NULL,
			running_count INTEGER NOT NULL,
			failed_count INTEGER NOT NULL,
			total_cpu REAL NOT NULL,
			total_memory INTEGER NOT NULL,
			total_gpu INTEGER NOT NULL,
			total_disk INTEGER NOT NULL
		)
	`)
	if err != nil {
		return err
	}

	return nil
}

// SaveApp saves application information to the database
func (m *Manager) SaveApp(app *AppStatus) error {
	m.appsMutex.Lock()
	defer m.appsMutex.Unlock()

	// Convert command to string
	command := ""
	for i, cmd := range app.Command {
		if i > 0 {
			command += ","
		}
		command += cmd
	}

	// Convert env to string
	env := ""
	for k, v := range app.Env {
		if env != "" {
			env += ","
		}
		env += k + "=" + v
	}

	// Convert volumes to string
	volumes := ""
	for i, volume := range app.Volumes {
		if i > 0 {
			volumes += ","
		}
		volumes += volume
	}

	// Convert ports to string
	ports := ""
	for k, v := range app.Ports {
		if ports != "" {
			ports += ","
		}
		ports += k + ":" + v
	}

	// Check if app already exists
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM apps WHERE id = ?", app.ID).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if app exists: %w", err)
	}

	if count > 0 {
		// Update existing app
		_, err = m.db.Exec(
			`UPDATE apps SET 
				name = ?, image = ?, command = ?, env = ?, volumes = ?, ports = ?,
				cpu = ?, memory = ?, gpu = ?, disk = ?,
				state = ?, node_id = ?, created_at = ?, started_at = ?, finished_at = ?,
				restart_count = ?, exit_code = ?, tenant_id = ?, user_id = ?
			WHERE id = ?`,
			app.Name, app.Image, command, env, volumes, ports,
			app.Resources.CPU, app.Resources.Memory, app.Resources.GPU, app.Resources.Disk,
			string(app.State), app.NodeID, app.CreatedAt, app.StartedAt, app.FinishedAt,
			app.RestartCount, app.ExitCode, app.TenantID, app.UserID,
			app.ID,
		)
		if err != nil {
			return fmt.Errorf("failed to update app: %w", err)
		}
	} else {
		// Insert new app
		_, err = m.db.Exec(
			`INSERT INTO apps (
				id, name, image, command, env, volumes, ports,
				cpu, memory, gpu, disk,
				state, node_id, created_at, started_at, finished_at,
				restart_count, exit_code, tenant_id, user_id
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			app.ID, app.Name, app.Image, command, env, volumes, ports,
			app.Resources.CPU, app.Resources.Memory, app.Resources.GPU, app.Resources.Disk,
			string(app.State), app.NodeID, app.CreatedAt, app.StartedAt, app.FinishedAt,
			app.RestartCount, app.ExitCode, app.TenantID, app.UserID,
		)
		if err != nil {
			return fmt.Errorf("failed to insert app: %w", err)
		}
	}

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			app.UserID,
			app.TenantID,
			fmt.Sprintf("app:%s", app.ID),
			"save",
			fmt.Sprintf("Saved app %s with state %s", app.Name, app.State),
		)
	}

	return nil
}

// DeleteApp deletes an application from the database
func (m *Manager) DeleteApp(id string) error {
	m.appsMutex.Lock()
	defer m.appsMutex.Unlock()

	// Check if app exists
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM apps WHERE id = ?", id).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if app exists: %w", err)
	}

	if count == 0 {
		return fmt.Errorf("app %s not found", id)
	}

	// Get app information for audit log and resource usage update
	var name, tenantID, userID string
	var cpu float64
	var memory int64
	err = m.db.QueryRow("SELECT name, tenant_id, user_id, cpu, memory FROM apps WHERE id = ?", id).Scan(&name, &tenantID, &userID, &cpu, &memory)
	if err != nil {
		return fmt.Errorf("failed to get app information: %w", err)
	}

	// Delete app
	_, err = m.db.Exec("DELETE FROM apps WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete app: %w", err)
	}

	// Update resource usage in tenant manager
	if m.tenantManager != nil && tenantID != "" {
		if tenantManager, ok := m.tenantManager.(interface {
			UpdateResourceUsageForApp(context.Context, string, float64, int64, bool) error
		}); ok {
			ctx := context.Background()
			err := tenantManager.UpdateResourceUsageForApp(ctx, tenantID, cpu, memory, false)
			if err != nil {
				m.logger.WithError(err).Errorf("Failed to update resource usage for tenant %s", tenantID)
				// Continue anyway, as the app is already deleted
			}
		} else {
			m.logger.Warn("Tenant manager doesn't implement UpdateResourceUsageForApp method")
		}
	}

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			userID,
			tenantID,
			fmt.Sprintf("app:%s", id),
			"delete",
			fmt.Sprintf("Deleted app %s", name),
		)
	}

	return nil
}

// GetApps returns all applications
func (m *Manager) GetApps() ([]*AppStatus, error) {
	m.appsMutex.RLock()
	defer m.appsMutex.RUnlock()

	rows, err := m.db.Query(`
		SELECT id, name, image, command, env, volumes, ports,
			cpu, memory, gpu, disk,
			state, node_id, created_at, started_at, finished_at,
			restart_count, exit_code, tenant_id, user_id
		FROM apps
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query apps: %w", err)
	}
	defer rows.Close()

	var apps []*AppStatus
	for rows.Next() {
		var app AppStatus
		var command, env, volumes, ports, state string
		err := rows.Scan(
			&app.ID, &app.Name, &app.Image, &command, &env, &volumes, &ports,
			&app.Resources.CPU, &app.Resources.Memory, &app.Resources.GPU, &app.Resources.Disk,
			&state, &app.NodeID, &app.CreatedAt, &app.StartedAt, &app.FinishedAt,
			&app.RestartCount, &app.ExitCode, &app.TenantID, &app.UserID,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan app: %w", err)
		}

		// Parse command
		if command != "" {
			app.Command = []string{}
			for _, cmd := range splitCSV(command) {
				app.Command = append(app.Command, cmd)
			}
		}

		// Parse env
		app.Env = make(map[string]string)
		for _, e := range splitCSV(env) {
			kv := splitKeyValue(e)
			if len(kv) == 2 {
				app.Env[kv[0]] = kv[1]
			}
		}

		// Parse volumes
		if volumes != "" {
			app.Volumes = []string{}
			for _, volume := range splitCSV(volumes) {
				app.Volumes = append(app.Volumes, volume)
			}
		}

		// Parse ports
		app.Ports = make(map[string]string)
		for _, p := range splitCSV(ports) {
			kv := splitKeyValue(p)
			if len(kv) == 2 {
				app.Ports[kv[0]] = kv[1]
			}
		}

		app.State = AppState(state)
		apps = append(apps, &app)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating apps: %w", err)
	}

	return apps, nil
}

// GetAppStatus returns the status of an application
func (m *Manager) GetAppStatus(id string) (*AppStatus, error) {
	m.appsMutex.RLock()
	defer m.appsMutex.RUnlock()

	var app AppStatus
	var command, env, volumes, ports, state string
	err := m.db.QueryRow(`
		SELECT id, name, image, command, env, volumes, ports,
			cpu, memory, gpu, disk,
			state, node_id, created_at, started_at, finished_at,
			restart_count, exit_code, tenant_id, user_id
		FROM apps
		WHERE id = ?
	`, id).Scan(
		&app.ID, &app.Name, &app.Image, &command, &env, &volumes, &ports,
		&app.Resources.CPU, &app.Resources.Memory, &app.Resources.GPU, &app.Resources.Disk,
		&state, &app.NodeID, &app.CreatedAt, &app.StartedAt, &app.FinishedAt,
		&app.RestartCount, &app.ExitCode, &app.TenantID, &app.UserID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("app %s not found", id)
		}
		return nil, fmt.Errorf("failed to query app: %w", err)
	}

	// Parse command
	if command != "" {
		app.Command = []string{}
		for _, cmd := range splitCSV(command) {
			app.Command = append(app.Command, cmd)
		}
	}

	// Parse env
	app.Env = make(map[string]string)
	for _, e := range splitCSV(env) {
		kv := splitKeyValue(e)
		if len(kv) == 2 {
			app.Env[kv[0]] = kv[1]
		}
	}

	// Parse volumes
	if volumes != "" {
		app.Volumes = []string{}
		for _, volume := range splitCSV(volumes) {
			app.Volumes = append(app.Volumes, volume)
		}
	}

	// Parse ports
	app.Ports = make(map[string]string)
	for _, p := range splitCSV(ports) {
		kv := splitKeyValue(p)
		if len(kv) == 2 {
			app.Ports[kv[0]] = kv[1]
		}
	}

	app.State = AppState(state)
	return &app, nil
}

// UpdateAppState updates the state of an application
func (m *Manager) UpdateAppState(id string, state AppState) error {
	m.appsMutex.Lock()
	defer m.appsMutex.Unlock()

	// Check if app exists
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM apps WHERE id = ?", id).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if app exists: %w", err)
	}

	if count == 0 {
		return fmt.Errorf("app %s not found", id)
	}

	// Get app information for audit log
	var name, tenantID, userID string
	err = m.db.QueryRow("SELECT name, tenant_id, user_id FROM apps WHERE id = ?", id).Scan(&name, &tenantID, &userID)
	if err != nil {
		return fmt.Errorf("failed to get app information: %w", err)
	}

	// Update app state
	_, err = m.db.Exec("UPDATE apps SET state = ? WHERE id = ?", string(state), id)
	if err != nil {
		return fmt.Errorf("failed to update app state: %w", err)
	}

	// Update started_at or finished_at if necessary
	now := time.Now().Unix()
	if state == AppStateRunning {
		_, err = m.db.Exec("UPDATE apps SET started_at = ? WHERE id = ?", now, id)
		if err != nil {
			return fmt.Errorf("failed to update app started_at: %w", err)
		}
	} else if state == AppStateStopped || state == AppStateFailed {
		_, err = m.db.Exec("UPDATE apps SET finished_at = ? WHERE id = ?", now, id)
		if err != nil {
			return fmt.Errorf("failed to update app finished_at: %w", err)
		}
	}

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			userID,
			tenantID,
			fmt.Sprintf("app:%s", id),
			"update_state",
			fmt.Sprintf("Updated app %s state to %s", name, state),
		)
	}

	return nil
}

// UpdateAppNodeID updates the node ID of an application
func (m *Manager) UpdateAppNodeID(id, nodeID string) error {
	m.appsMutex.Lock()
	defer m.appsMutex.Unlock()

	// Check if app exists
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM apps WHERE id = ?", id).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if app exists: %w", err)
	}

	if count == 0 {
		return fmt.Errorf("app %s not found", id)
	}

	// Get app information for audit log
	var name, tenantID, userID string
	err = m.db.QueryRow("SELECT name, tenant_id, user_id FROM apps WHERE id = ?", id).Scan(&name, &tenantID, &userID)
	if err != nil {
		return fmt.Errorf("failed to get app information: %w", err)
	}

	// Update app node ID
	_, err = m.db.Exec("UPDATE apps SET node_id = ? WHERE id = ?", nodeID, id)
	if err != nil {
		return fmt.Errorf("failed to update app node ID: %w", err)
	}

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			userID,
			tenantID,
			fmt.Sprintf("app:%s", id),
			"update_node",
			fmt.Sprintf("Updated app %s node ID to %s", name, nodeID),
		)
	}

	return nil
}

// UpdateAppRestartCount updates the restart count of an application
func (m *Manager) UpdateAppRestartCount(id string, restartCount int) error {
	m.appsMutex.Lock()
	defer m.appsMutex.Unlock()

	// Check if app exists
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM apps WHERE id = ?", id).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if app exists: %w", err)
	}

	if count == 0 {
		return fmt.Errorf("app %s not found", id)
	}

	// Update app restart count
	_, err = m.db.Exec("UPDATE apps SET restart_count = ? WHERE id = ?", restartCount, id)
	if err != nil {
		return fmt.Errorf("failed to update app restart count: %w", err)
	}

	return nil
}

// UpdateAppExitCode updates the exit code of an application
func (m *Manager) UpdateAppExitCode(id string, exitCode int) error {
	m.appsMutex.Lock()
	defer m.appsMutex.Unlock()

	// Check if app exists
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM apps WHERE id = ?", id).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if app exists: %w", err)
	}

	if count == 0 {
		return fmt.Errorf("app %s not found", id)
	}

	// Update app exit code
	_, err = m.db.Exec("UPDATE apps SET exit_code = ? WHERE id = ?", exitCode, id)
	if err != nil {
		return fmt.Errorf("failed to update app exit code: %w", err)
	}

	return nil
}

// GetAppsByFilter returns applications filtered by criteria
func (m *Manager) GetAppsByFilter(filter AppFilter) ([]*AppStatus, error) {
	m.appsMutex.RLock()
	defer m.appsMutex.RUnlock()

	query := `
		SELECT id, name, image, command, env, volumes, ports,
			cpu, memory, gpu, disk,
			state, node_id, created_at, started_at, finished_at,
			restart_count, exit_code, tenant_id, user_id
		FROM apps
		WHERE 1=1
	`
	var args []interface{}

	if filter.TenantID != "" {
		query += " AND tenant_id = ?"
		args = append(args, filter.TenantID)
	}

	if filter.UserID != "" {
		query += " AND user_id = ?"
		args = append(args, filter.UserID)
	}

	if filter.State != "" {
		query += " AND state = ?"
		args = append(args, string(filter.State))
	}

	if filter.NodeID != "" {
		query += " AND node_id = ?"
		args = append(args, filter.NodeID)
	}

	rows, err := m.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query apps: %w", err)
	}
	defer rows.Close()

	var apps []*AppStatus
	for rows.Next() {
		var app AppStatus
		var command, env, volumes, ports, state string
		err := rows.Scan(
			&app.ID, &app.Name, &app.Image, &command, &env, &volumes, &ports,
			&app.Resources.CPU, &app.Resources.Memory, &app.Resources.GPU, &app.Resources.Disk,
			&state, &app.NodeID, &app.CreatedAt, &app.StartedAt, &app.FinishedAt,
			&app.RestartCount, &app.ExitCode, &app.TenantID, &app.UserID,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan app: %w", err)
		}

		// Parse command
		if command != "" {
			app.Command = []string{}
			for _, cmd := range splitCSV(command) {
				app.Command = append(app.Command, cmd)
			}
		}

		// Parse env
		app.Env = make(map[string]string)
		for _, e := range splitCSV(env) {
			kv := splitKeyValue(e)
			if len(kv) == 2 {
				app.Env[kv[0]] = kv[1]
			}
		}

		// Parse volumes
		if volumes != "" {
			app.Volumes = []string{}
			for _, volume := range splitCSV(volumes) {
				app.Volumes = append(app.Volumes, volume)
			}
		}

		// Parse ports
		app.Ports = make(map[string]string)
		for _, p := range splitCSV(ports) {
			kv := splitKeyValue(p)
			if len(kv) == 2 {
				app.Ports[kv[0]] = kv[1]
			}
		}

		app.State = AppState(state)
		apps = append(apps, &app)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating apps: %w", err)
	}

	return apps, nil
}

// GetAppAuditLogs returns audit logs for an application
func (m *Manager) GetAppAuditLogs(appID string) []AppAuditLogEntry {
	if m.accessControl != nil {
		return m.accessControl.GetAppAuditLogs(appID)
	}

	return []AppAuditLogEntry{}
}

// GetTenantAppStats returns application statistics for a tenant
func (m *Manager) GetTenantAppStats(tenantID string) (*TenantAppStats, error) {
	m.appsMutex.RLock()
	defer m.appsMutex.RUnlock()

	var stats TenantAppStats
	stats.TenantID = tenantID

	// Get app count
	err := m.db.QueryRow("SELECT COUNT(*) FROM apps WHERE tenant_id = ?", tenantID).Scan(&stats.AppCount)
	if err != nil {
		return nil, fmt.Errorf("failed to get app count: %w", err)
	}

	// Get running count
	err = m.db.QueryRow("SELECT COUNT(*) FROM apps WHERE tenant_id = ? AND state = ?", tenantID, string(AppStateRunning)).Scan(&stats.RunningCount)
	if err != nil {
		return nil, fmt.Errorf("failed to get running count: %w", err)
	}

	// Get failed count
	err = m.db.QueryRow("SELECT COUNT(*) FROM apps WHERE tenant_id = ? AND state = ?", tenantID, string(AppStateFailed)).Scan(&stats.FailedCount)
	if err != nil {
		return nil, fmt.Errorf("failed to get failed count: %w", err)
	}

	// Get total resources
	err = m.db.QueryRow(`
		SELECT 
			COALESCE(SUM(cpu), 0),
			COALESCE(SUM(memory), 0),
			COALESCE(SUM(gpu), 0),
			COALESCE(SUM(disk), 0)
		FROM apps
		WHERE tenant_id = ?
	`, tenantID).Scan(&stats.TotalCPU, &stats.TotalMemory, &stats.TotalGPU, &stats.TotalDisk)
	if err != nil {
		return nil, fmt.Errorf("failed to get total resources: %w", err)
	}

	return &stats, nil
}

// splitString splits a string by a separator
func splitString(s string, sep rune) []string {
	var result []string
	var current string
	var inQuote bool

	for _, c := range s {
		if c == '"' {
			inQuote = !inQuote
		} else if c == sep && !inQuote {
			result = append(result, current)
			current = ""
		} else {
			current += string(c)
		}
	}

	// Add the last part if there is any
	if current != "" {
		result = append(result, current)
	}

	return result
}
