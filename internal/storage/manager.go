package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/sirupsen/logrus"
)

// VolumeState represents the state of a volume
type VolumeState string

const (
	// VolumeStateAvailable indicates the volume is available
	VolumeStateAvailable VolumeState = "available"
	// VolumeStateInUse indicates the volume is in use
	VolumeStateInUse VolumeState = "in_use"
	// VolumeStateFailed indicates the volume has failed
	VolumeStateFailed VolumeState = "failed"
)

// VolumeStatus represents the status of a volume
type VolumeStatus struct {
	Name      string
	Path      string
	Size      int64
	CreatedAt int64
	State     VolumeState
}

// StorageAuditLogEntry represents an entry in the storage audit log
type StorageAuditLogEntry struct {
	Timestamp int64
	Action    string
	UserID    string
	TenantID  string
	Resource  string
	Details   string
}

// TenantStorageStats represents storage statistics for a tenant
type TenantStorageStats struct {
	TotalSize      int64
	UsedSize       int64
	VolumeCount    int
	ReadOps        int64
	WriteOps       int64
	ReadBytes      int64
	WriteBytes     int64
	LastUpdateTime int64
}

// EncryptionManager handles encryption of data
type EncryptionManager interface {
	Encrypt(tenantID *string, data []byte) ([]byte, error)
	Decrypt(tenantID *string, encryptedData []byte) ([]byte, error)
}

// AccessControl handles access control for storage
type AccessControl interface {
	CheckAccess(userID, tenantID, resource, action string) bool
	LogAccess(userID, tenantID, resource, action, details string)
	GetAuditLogs() []StorageAuditLogEntry
	GetTenantAuditLogs(tenantID string) []StorageAuditLogEntry
	GetVolumeAuditLogs(volumeName string) []StorageAuditLogEntry
}

// QoSManager handles quality of service for storage
type QoSManager interface {
	SetTenantRateLimit(tenantID string, rateLimit uint32) error
	GetTenantStats(tenantID string) *TenantStorageStats
}

// Manager handles storage operations
type Manager struct {
	db                *sql.DB
	dataDir           string
	volumesDir        string
	encryptionManager EncryptionManager
	accessControl     AccessControl
	qosManager        QoSManager
	logger            *logrus.Logger
	volumesMutex      sync.RWMutex
}

// NewManager creates a new storage manager
func NewManager(dataDir string, logger *logrus.Logger) (*Manager, error) {
	// Create data directory if it doesn't exist
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	// Create volumes directory
	volumesDir := filepath.Join(dataDir, "volumes")
	if err := os.MkdirAll(volumesDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create volumes directory: %w", err)
	}

	// Open database
	dbPath := filepath.Join(dataDir, "storage.db")
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
		db:         db,
		dataDir:    dataDir,
		volumesDir: volumesDir,
		logger:     logger,
	}, nil
}

// WithEncryptionManager sets the encryption manager
func (m *Manager) WithEncryptionManager(encryptionManager EncryptionManager) *Manager {
	m.encryptionManager = encryptionManager
	return m
}

// WithAccessControl sets the access control
func (m *Manager) WithAccessControl(accessControl AccessControl) *Manager {
	m.accessControl = accessControl
	return m
}

// WithQoSManager sets the QoS manager
func (m *Manager) WithQoSManager(qosManager QoSManager) *Manager {
	m.qosManager = qosManager
	return m
}

// Close closes the storage manager
func (m *Manager) Close() error {
	return m.db.Close()
}

// initializeDatabase initializes the database schema
func initializeDatabase(db *sql.DB) error {
	// Create volumes table
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS volumes (
			name TEXT PRIMARY KEY,
			path TEXT NOT NULL,
			size INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			state TEXT NOT NULL
		)
	`)
	if err != nil {
		return err
	}

	// Create nodes table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS nodes (
			id TEXT PRIMARY KEY,
			address TEXT NOT NULL,
			joined_at INTEGER NOT NULL
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
			resource TEXT NOT NULL,
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
			total_size INTEGER NOT NULL,
			used_size INTEGER NOT NULL,
			volume_count INTEGER NOT NULL,
			read_ops INTEGER NOT NULL,
			write_ops INTEGER NOT NULL,
			read_bytes INTEGER NOT NULL,
			write_bytes INTEGER NOT NULL,
			last_update_time INTEGER NOT NULL
		)
	`)
	if err != nil {
		return err
	}

	return nil
}

// SaveVolume saves volume information to the database
func (m *Manager) SaveVolume(name, path string, size, createdAt int64) error {
	// First check if volume exists without holding the mutex
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM volumes WHERE name = ?", name).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if volume exists: %w", err)
	}

	var execErr error
	if count > 0 {
		// Update existing volume without holding the mutex
		_, execErr = m.db.Exec(
			"UPDATE volumes SET path = ?, size = ?, created_at = ?, state = ? WHERE name = ?",
			path, size, createdAt, string(VolumeStateAvailable), name,
		)
		if execErr != nil {
			return fmt.Errorf("failed to update volume: %w", execErr)
		}
	} else {
		// Insert new volume without holding the mutex
		_, execErr = m.db.Exec(
			"INSERT INTO volumes (name, path, size, created_at, state) VALUES (?, ?, ?, ?, ?)",
			name, path, size, createdAt, string(VolumeStateAvailable),
		)
		if execErr != nil {
			return fmt.Errorf("failed to insert volume: %w", execErr)
		}
	}

	// Don't acquire any mutex for logging to avoid potential deadlocks
	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			"system", // User ID
			"",       // Tenant ID
			fmt.Sprintf("volume:%s", name),
			"create",
			fmt.Sprintf("Created volume %s with size %d", name, size),
		)
	}

	return nil
}

// DeleteVolume deletes a volume from the database
func (m *Manager) DeleteVolume(name string) error {
	// Check if volume exists without holding the mutex
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM volumes WHERE name = ?", name).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if volume exists: %w", err)
	}

	if count == 0 {
		return fmt.Errorf("volume %s not found", name)
	}

	// Delete volume from database without holding the mutex
	_, err = m.db.Exec("DELETE FROM volumes WHERE name = ?", name)
	if err != nil {
		return fmt.Errorf("failed to delete volume: %w", err)
	}

	// Acquire the mutex only for the access control logging
	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			"system", // User ID
			"",       // Tenant ID
			fmt.Sprintf("volume:%s", name),
			"delete",
			fmt.Sprintf("Deleted volume %s", name),
		)
	}

	return nil
}

// GetVolumes returns all volumes
func (m *Manager) GetVolumes() ([]VolumeStatus, error) {
	// Execute the database query without holding the mutex
	rows, err := m.db.Query("SELECT name, path, size, created_at, state FROM volumes")
	if err != nil {
		return nil, fmt.Errorf("failed to query volumes: %w", err)
	}
	defer rows.Close()

	var volumes []VolumeStatus
	var states []string

	// Scan all rows without holding the mutex
	for rows.Next() {
		var volume VolumeStatus
		var state string
		err := rows.Scan(&volume.Name, &volume.Path, &volume.Size, &volume.CreatedAt, &state)
		if err != nil {
			return nil, fmt.Errorf("failed to scan volume: %w", err)
		}
		volumes = append(volumes, volume)
		states = append(states, state)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating volumes: %w", err)
	}

	// Acquire a read lock only for the final state conversion
	m.volumesMutex.RLock()
	defer m.volumesMutex.RUnlock()

	// Convert states to VolumeState
	for i := range volumes {
		volumes[i].State = VolumeState(states[i])
	}

	return volumes, nil
}

// GetVolumeStatus returns the status of a volume
func (m *Manager) GetVolumeStatus(name string) (*VolumeStatus, error) {
	// Execute the database query without holding the mutex
	var volume VolumeStatus
	var state string
	err := m.db.QueryRow(
		"SELECT name, path, size, created_at, state FROM volumes WHERE name = ?",
		name,
	).Scan(&volume.Name, &volume.Path, &volume.Size, &volume.CreatedAt, &state)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("volume %s not found", name)
		}
		return nil, fmt.Errorf("failed to query volume: %w", err)
	}

	// Acquire a read lock only for the final state conversion
	m.volumesMutex.RLock()
	defer m.volumesMutex.RUnlock()

	volume.State = VolumeState(state)
	return &volume, nil
}

// UpdateVolumeState updates the state of a volume
func (m *Manager) UpdateVolumeState(name string, state VolumeState) error {
	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// Check if volume exists
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM volumes WHERE name = ?", name).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if volume exists: %w", err)
	}

	if count == 0 {
		return fmt.Errorf("volume %s not found", name)
	}

	// Update volume state
	_, err = m.db.Exec("UPDATE volumes SET state = ? WHERE name = ?", string(state), name)
	if err != nil {
		return fmt.Errorf("failed to update volume state: %w", err)
	}

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			"system", // User ID
			"",       // Tenant ID
			fmt.Sprintf("volume:%s", name),
			"update_state",
			fmt.Sprintf("Updated volume %s state to %s", name, state),
		)
	}

	return nil
}

// SaveNode saves node information to the database
func (m *Manager) SaveNode(id, address string, joinedAt int64) error {
	// Check if node already exists
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM nodes WHERE id = ?", id).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if node exists: %w", err)
	}

	if count > 0 {
		// Update existing node
		_, err = m.db.Exec(
			"UPDATE nodes SET address = ?, joined_at = ? WHERE id = ?",
			address, joinedAt, id,
		)
		if err != nil {
			return fmt.Errorf("failed to update node: %w", err)
		}
	} else {
		// Insert new node
		_, err = m.db.Exec(
			"INSERT INTO nodes (id, address, joined_at) VALUES (?, ?, ?)",
			id, address, joinedAt,
		)
		if err != nil {
			return fmt.Errorf("failed to insert node: %w", err)
		}
	}

	return nil
}

// DeleteNode deletes a node from the database
func (m *Manager) DeleteNode(id string) error {
	// Check if node exists
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM nodes WHERE id = ?", id).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if node exists: %w", err)
	}

	if count == 0 {
		return fmt.Errorf("node %s not found", id)
	}

	// Delete node
	_, err = m.db.Exec("DELETE FROM nodes WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete node: %w", err)
	}

	return nil
}

// GetNodes returns all nodes
func (m *Manager) GetNodes() ([]struct {
	ID       string
	Address  string
	JoinedAt int64
}, error) {
	rows, err := m.db.Query("SELECT id, address, joined_at FROM nodes")
	if err != nil {
		return nil, fmt.Errorf("failed to query nodes: %w", err)
	}
	defer rows.Close()

	var nodes []struct {
		ID       string
		Address  string
		JoinedAt int64
	}
	for rows.Next() {
		var node struct {
			ID       string
			Address  string
			JoinedAt int64
		}
		err := rows.Scan(&node.ID, &node.Address, &node.JoinedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan node: %w", err)
		}
		nodes = append(nodes, node)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating nodes: %w", err)
	}

	return nodes, nil
}

// IsVolumeInUse checks if a volume is in use
func (m *Manager) IsVolumeInUse(name string) (bool, error) {
	volume, err := m.GetVolumeStatus(name)
	if err != nil {
		return false, err
	}

	return volume.State == VolumeStateInUse, nil
}

// EncryptData encrypts data for a tenant
func (m *Manager) EncryptData(tenantID *string, data []byte) ([]byte, error) {
	if m.encryptionManager != nil {
		return m.encryptionManager.Encrypt(tenantID, data)
	}

	// If encryption manager is not available, return the data as-is
	return data, nil
}

// DecryptData decrypts data for a tenant
func (m *Manager) DecryptData(tenantID *string, encryptedData []byte) ([]byte, error) {
	if m.encryptionManager != nil {
		return m.encryptionManager.Decrypt(tenantID, encryptedData)
	}

	// If encryption manager is not available, return the data as-is
	return encryptedData, nil
}

// GetStorageAuditLogs returns storage audit logs
func (m *Manager) GetStorageAuditLogs() []StorageAuditLogEntry {
	if m.accessControl != nil {
		return m.accessControl.GetAuditLogs()
	}

	return []StorageAuditLogEntry{}
}

// GetTenantStorageAuditLogs returns storage audit logs for a tenant
func (m *Manager) GetTenantStorageAuditLogs(tenantID string) []StorageAuditLogEntry {
	if m.accessControl != nil {
		return m.accessControl.GetTenantAuditLogs(tenantID)
	}

	return []StorageAuditLogEntry{}
}

// GetVolumeAuditLogs returns storage audit logs for a volume
func (m *Manager) GetVolumeAuditLogs(volumeName string) []StorageAuditLogEntry {
	if m.accessControl != nil {
		return m.accessControl.GetVolumeAuditLogs(volumeName)
	}

	return []StorageAuditLogEntry{}
}

// SetTenantStorageRateLimit sets the rate limit for a tenant
func (m *Manager) SetTenantStorageRateLimit(tenantID string, rateLimit uint32) error {
	if m.qosManager != nil {
		return m.qosManager.SetTenantRateLimit(tenantID, rateLimit)
	}

	return fmt.Errorf("QoS manager not available")
}

// GetTenantStorageStats returns storage stats for a tenant
func (m *Manager) GetTenantStorageStats(tenantID string) *TenantStorageStats {
	if m.qosManager != nil {
		return m.qosManager.GetTenantStats(tenantID)
	}

	return nil
}

// BackupVolume backs up a volume to a file
func (m *Manager) BackupVolume(name, outputPath string) error {
	m.volumesMutex.RLock()
	defer m.volumesMutex.RUnlock()

	// Check if volume exists
	_, err := m.GetVolumeStatus(name)
	if err != nil {
		return err
	}

	// Create output file
	outputFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outputFile.Close()

	// Open volume file
	volumePath := filepath.Join(m.volumesDir, name)
	volumeFile, err := os.Open(volumePath)
	if err != nil {
		return fmt.Errorf("failed to open volume file: %w", err)
	}
	defer volumeFile.Close()

	// Copy volume data to output file
	_, err = io.Copy(outputFile, volumeFile)
	if err != nil {
		return fmt.Errorf("failed to copy volume data: %w", err)
	}

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			"system", // User ID
			"",       // Tenant ID
			fmt.Sprintf("volume:%s", name),
			"backup",
			fmt.Sprintf("Backed up volume %s to %s", name, outputPath),
		)
	}

	return nil
}

// RestoreVolume restores a volume from a backup file
func (m *Manager) RestoreVolume(name, inputPath string) error {
	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// Check if input file exists
	if _, err := os.Stat(inputPath); os.IsNotExist(err) {
		return fmt.Errorf("input file %s does not exist", inputPath)
	}

	// Create volume file
	volumePath := filepath.Join(m.volumesDir, name)
	volumeFile, err := os.Create(volumePath)
	if err != nil {
		return fmt.Errorf("failed to create volume file: %w", err)
	}
	defer volumeFile.Close()

	// Open input file
	inputFile, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("failed to open input file: %w", err)
	}
	defer inputFile.Close()

	// Copy input data to volume file
	_, err = io.Copy(volumeFile, inputFile)
	if err != nil {
		return fmt.Errorf("failed to copy input data: %w", err)
	}

	// Get file size
	fileInfo, err := volumeFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to get file info: %w", err)
	}

	// Save volume information
	now := time.Now().Unix()
	err = m.SaveVolume(name, volumePath, fileInfo.Size(), now)
	if err != nil {
		return fmt.Errorf("failed to save volume information: %w", err)
	}

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			"system", // User ID
			"",       // Tenant ID
			fmt.Sprintf("volume:%s", name),
			"restore",
			fmt.Sprintf("Restored volume %s from %s", name, inputPath),
		)
	}

	return nil
}

// UpdateNodeResources updates the resources of a node in the database
func (m *Manager) UpdateNodeResources(nodeID string, cpu int, memory int, disk int64) error {
	// Check if node exists
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM nodes WHERE id = ?", nodeID).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if node exists: %w", err)
	}

	if count == 0 {
		return fmt.Errorf("node %s not found", nodeID)
	}

	// Check if the resources columns exist
	var cpuColumnExists bool
	err = m.db.QueryRow(`
		SELECT COUNT(*) > 0 FROM pragma_table_info('nodes') WHERE name = 'cpu'
	`).Scan(&cpuColumnExists)
	if err != nil {
		return fmt.Errorf("failed to check if cpu column exists: %w", err)
	}

	// If the resources columns don't exist, add them
	if !cpuColumnExists {
		_, err = m.db.Exec(`ALTER TABLE nodes ADD COLUMN cpu INTEGER`)
		if err != nil {
			return fmt.Errorf("failed to add cpu column: %w", err)
		}

		_, err = m.db.Exec(`ALTER TABLE nodes ADD COLUMN memory INTEGER`)
		if err != nil {
			return fmt.Errorf("failed to add memory column: %w", err)
		}

		_, err = m.db.Exec(`ALTER TABLE nodes ADD COLUMN disk INTEGER`)
		if err != nil {
			return fmt.Errorf("failed to add disk column: %w", err)
		}
	}

	// Update the node resources
	_, err = m.db.Exec("UPDATE nodes SET cpu = ?, memory = ?, disk = ? WHERE id = ?",
		cpu, memory, disk, nodeID)
	if err != nil {
		return fmt.Errorf("failed to update node resources: %w", err)
	}

	return nil
}

// UpdateNodeStatus updates the status of a node in the database
func (m *Manager) UpdateNodeStatus(nodeID string, status string) error {
	// Check if node exists
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM nodes WHERE id = ?", nodeID).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if node exists: %w", err)
	}

	if count == 0 {
		return fmt.Errorf("node %s not found", nodeID)
	}

	// Update node status
	// First, check if the status column exists
	var statusColumnExists bool
	err = m.db.QueryRow(`
		SELECT COUNT(*) > 0 FROM pragma_table_info('nodes') WHERE name = 'status'
	`).Scan(&statusColumnExists)
	if err != nil {
		return fmt.Errorf("failed to check if status column exists: %w", err)
	}

	// If the status column doesn't exist, add it
	if !statusColumnExists {
		_, err = m.db.Exec(`ALTER TABLE nodes ADD COLUMN status TEXT`)
		if err != nil {
			return fmt.Errorf("failed to add status column: %w", err)
		}
	}

	// Update the node status
	_, err = m.db.Exec("UPDATE nodes SET status = ? WHERE id = ?", status, nodeID)
	if err != nil {
		return fmt.Errorf("failed to update node status: %w", err)
	}

	return nil
}
