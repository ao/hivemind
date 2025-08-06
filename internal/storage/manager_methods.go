package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// VolumeOptions represents options for creating a volume
type VolumeOptions struct {
	Name     string
	Size     int64
	TenantID string
	UserID   string
	Metadata map[string]string
}

// CreateVolumeWithOptions creates a new volume
func (m *Manager) CreateVolumeWithOptions(ctx context.Context, options VolumeOptions) (*VolumeStatus, error) {
	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// Check if volume already exists
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM volumes WHERE name = ?", options.Name).Scan(&count)
	if err != nil {
		return nil, fmt.Errorf("failed to check if volume exists: %w", err)
	}

	if count > 0 {
		return nil, fmt.Errorf("volume %s already exists", options.Name)
	}

	// Check access if access control is available
	if m.accessControl != nil && options.UserID != "" && options.TenantID != "" {
		if !m.accessControl.CheckAccess(options.UserID, options.TenantID, fmt.Sprintf("volume:%s", options.Name), "create") {
			return nil, fmt.Errorf("access denied to create volume %s", options.Name)
		}
	}

	// Create volume file
	volumePath := filepath.Join(m.volumesDir, options.Name)
	volumeFile, err := os.Create(volumePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create volume file: %w", err)
	}
	defer volumeFile.Close()

	// Allocate space for the volume if size is specified
	if options.Size > 0 {
		err = volumeFile.Truncate(options.Size)
		if err != nil {
			// Clean up on error
			os.Remove(volumePath)
			return nil, fmt.Errorf("failed to allocate space for volume: %w", err)
		}
	}

	// Save volume information
	now := time.Now().Unix()
	err = m.SaveVolume(options.Name, volumePath, options.Size, now)
	if err != nil {
		// Clean up on error
		os.Remove(volumePath)
		return nil, fmt.Errorf("failed to save volume information: %w", err)
	}

	// Log audit event if access control is available
	if m.accessControl != nil && options.UserID != "" && options.TenantID != "" {
		// Log the access event
		m.accessControl.LogAccess(
			options.UserID,
			options.TenantID,
			fmt.Sprintf("volume:%s", options.Name),
			"create",
			fmt.Sprintf("Created volume %s with size %d", options.Name, options.Size),
		)

		// Also log an access grant event to establish ownership
		m.accessControl.LogAccess(
			"system",
			options.TenantID,
			fmt.Sprintf("volume:%s", options.Name),
			"grant_access",
			fmt.Sprintf("Granted access to volume %s for tenant %s", options.Name, options.TenantID),
		)
	}

	// Update tenant stats if QoS manager is available
	if m.qosManager != nil && options.TenantID != "" {
		stats := m.qosManager.GetTenantStats(options.TenantID)
		if stats != nil {
			stats.TotalSize += options.Size
			stats.VolumeCount++
			stats.LastUpdateTime = now
		}
	}

	// Return volume status
	return &VolumeStatus{
		Name:      options.Name,
		Path:      volumePath,
		Size:      options.Size,
		CreatedAt: now,
		State:     VolumeStateAvailable,
	}, nil
}

// DeleteVolumeWithOptions deletes a volume with options
func (m *Manager) DeleteVolumeWithOptions(ctx context.Context, name, userID, tenantID string) error {
	// Check access if access control is available
	if m.accessControl != nil && userID != "" && tenantID != "" {
		if !m.accessControl.CheckAccess(userID, tenantID, fmt.Sprintf("volume:%s", name), "delete") {
			return fmt.Errorf("access denied to delete volume %s", name)
		}
	}

	// Get volume status
	volume, err := m.GetVolumeStatus(name)
	if err != nil {
		return err
	}

	// Check if volume is in use
	if volume.State == VolumeStateInUse {
		return fmt.Errorf("cannot delete volume %s because it is in use", name)
	}

	// Delete volume file
	volumePath := filepath.Join(m.volumesDir, name)
	err = os.Remove(volumePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete volume file: %w", err)
	}

	// Delete volume from database
	err = m.DeleteVolume(name)
	if err != nil {
		return err
	}

	// Log audit event if access control is available
	if m.accessControl != nil && userID != "" && tenantID != "" {
		m.accessControl.LogAccess(
			userID,
			tenantID,
			fmt.Sprintf("volume:%s", name),
			"delete",
			fmt.Sprintf("Deleted volume %s", name),
		)
	}

	// Update tenant stats if QoS manager is available
	if m.qosManager != nil && tenantID != "" {
		stats := m.qosManager.GetTenantStats(tenantID)
		if stats != nil {
			stats.TotalSize -= volume.Size
			stats.VolumeCount--
			stats.LastUpdateTime = time.Now().Unix()
		}
	}

	return nil
}

// MountVolume mounts a volume to a container
func (m *Manager) MountVolume(ctx context.Context, volumeName, containerID, mountPath string) error {
	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// Get volume status
	volume, err := m.GetVolumeStatus(volumeName)
	if err != nil {
		return err
	}

	// Check if volume is available
	if volume.State != VolumeStateAvailable {
		return fmt.Errorf("volume %s is not available", volumeName)
	}

	// Update volume state
	err = m.UpdateVolumeState(volumeName, VolumeStateInUse)
	if err != nil {
		return err
	}

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			"system", // User ID
			"",       // Tenant ID
			fmt.Sprintf("volume:%s", volumeName),
			"mount",
			fmt.Sprintf("Mounted volume %s to container %s at %s", volumeName, containerID, mountPath),
		)
	}

	return nil
}

// UnmountVolume unmounts a volume from a container
func (m *Manager) UnmountVolume(ctx context.Context, volumeName, containerID string) error {
	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// Get volume status
	volume, err := m.GetVolumeStatus(volumeName)
	if err != nil {
		return err
	}

	// Check if volume is in use
	if volume.State != VolumeStateInUse {
		return fmt.Errorf("volume %s is not in use", volumeName)
	}

	// Update volume state
	err = m.UpdateVolumeState(volumeName, VolumeStateAvailable)
	if err != nil {
		return err
	}

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			"system", // User ID
			"",       // Tenant ID
			fmt.Sprintf("volume:%s", volumeName),
			"unmount",
			fmt.Sprintf("Unmounted volume %s from container %s", volumeName, containerID),
		)
	}

	return nil
}

// ResizeVolume resizes a volume
func (m *Manager) ResizeVolume(ctx context.Context, name string, newSize int64) error {
	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// Get volume status
	volume, err := m.GetVolumeStatus(name)
	if err != nil {
		return err
	}

	// Check if volume is in use
	if volume.State == VolumeStateInUse {
		return fmt.Errorf("cannot resize volume %s because it is in use", name)
	}

	// Open volume file
	volumePath := filepath.Join(m.volumesDir, name)
	volumeFile, err := os.OpenFile(volumePath, os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to open volume file: %w", err)
	}
	defer volumeFile.Close()

	// Resize volume
	err = volumeFile.Truncate(newSize)
	if err != nil {
		return fmt.Errorf("failed to resize volume: %w", err)
	}

	// Update volume information
	err = m.SaveVolume(name, volumePath, newSize, volume.CreatedAt)
	if err != nil {
		return fmt.Errorf("failed to update volume information: %w", err)
	}

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			"system", // User ID
			"",       // Tenant ID
			fmt.Sprintf("volume:%s", name),
			"resize",
			fmt.Sprintf("Resized volume %s from %d to %d", name, volume.Size, newSize),
		)
	}

	return nil
}

// CloneVolume clones a volume
func (m *Manager) CloneVolume(ctx context.Context, sourceName, targetName string) error {
	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// Get source volume status
	sourceVolume, err := m.GetVolumeStatus(sourceName)
	if err != nil {
		return err
	}

	// Check if target volume already exists
	var count int
	err = m.db.QueryRow("SELECT COUNT(*) FROM volumes WHERE name = ?", targetName).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check if target volume exists: %w", err)
	}

	if count > 0 {
		return fmt.Errorf("target volume %s already exists", targetName)
	}

	// Create target volume file
	targetPath := filepath.Join(m.volumesDir, targetName)
	targetFile, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("failed to create target volume file: %w", err)
	}
	defer targetFile.Close()

	// Open source volume file
	sourcePath := filepath.Join(m.volumesDir, sourceName)
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		// Clean up on error
		os.Remove(targetPath)
		return fmt.Errorf("failed to open source volume file: %w", err)
	}
	defer sourceFile.Close()

	// Copy source volume data to target volume
	_, err = io.Copy(targetFile, sourceFile)
	if err != nil {
		// Clean up on error
		os.Remove(targetPath)
		return fmt.Errorf("failed to copy volume data: %w", err)
	}

	// Save target volume information
	now := time.Now().Unix()
	err = m.SaveVolume(targetName, targetPath, sourceVolume.Size, now)
	if err != nil {
		// Clean up on error
		os.Remove(targetPath)
		return fmt.Errorf("failed to save target volume information: %w", err)
	}

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			"system", // User ID
			"",       // Tenant ID
			fmt.Sprintf("volume:%s", targetName),
			"clone",
			fmt.Sprintf("Cloned volume %s from %s", targetName, sourceName),
		)
	}

	return nil
}

// SnapshotVolume creates a snapshot of a volume
func (m *Manager) SnapshotVolume(ctx context.Context, volumeName, snapshotName string) error {
	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// Check if volume exists
	_, err := m.GetVolumeStatus(volumeName)
	if err != nil {
		return err
	}

	// Create snapshot directory if it doesn't exist
	snapshotsDir := filepath.Join(m.dataDir, "snapshots")
	if err := os.MkdirAll(snapshotsDir, 0755); err != nil {
		return fmt.Errorf("failed to create snapshots directory: %w", err)
	}

	// Create snapshot file
	snapshotPath := filepath.Join(snapshotsDir, snapshotName)
	snapshotFile, err := os.Create(snapshotPath)
	if err != nil {
		return fmt.Errorf("failed to create snapshot file: %w", err)
	}
	defer snapshotFile.Close()

	// Open volume file
	volumePath := filepath.Join(m.volumesDir, volumeName)
	volumeFile, err := os.Open(volumePath)
	if err != nil {
		// Clean up on error
		os.Remove(snapshotPath)
		return fmt.Errorf("failed to open volume file: %w", err)
	}
	defer volumeFile.Close()

	// Copy volume data to snapshot file
	_, err = io.Copy(snapshotFile, volumeFile)
	if err != nil {
		// Clean up on error
		os.Remove(snapshotPath)
		return fmt.Errorf("failed to copy volume data: %w", err)
	}

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			"system", // User ID
			"",       // Tenant ID
			fmt.Sprintf("volume:%s", volumeName),
			"snapshot",
			fmt.Sprintf("Created snapshot %s of volume %s", snapshotName, volumeName),
		)
	}

	return nil
}

// RestoreVolumeFromSnapshot restores a volume from a snapshot
func (m *Manager) RestoreVolumeFromSnapshot(ctx context.Context, volumeName, snapshotName string) error {
	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// Get volume status
	volume, err := m.GetVolumeStatus(volumeName)
	if err != nil {
		return err
	}

	// Check if volume is in use
	if volume.State == VolumeStateInUse {
		return fmt.Errorf("cannot restore volume %s because it is in use", volumeName)
	}

	// Check if snapshot exists
	snapshotPath := filepath.Join(m.dataDir, "snapshots", snapshotName)
	if _, err := os.Stat(snapshotPath); os.IsNotExist(err) {
		return fmt.Errorf("snapshot %s does not exist", snapshotName)
	}

	// Open volume file
	volumePath := filepath.Join(m.volumesDir, volumeName)
	volumeFile, err := os.Create(volumePath)
	if err != nil {
		return fmt.Errorf("failed to open volume file: %w", err)
	}
	defer volumeFile.Close()

	// Open snapshot file
	snapshotFile, err := os.Open(snapshotPath)
	if err != nil {
		return fmt.Errorf("failed to open snapshot file: %w", err)
	}
	defer snapshotFile.Close()

	// Copy snapshot data to volume file
	_, err = io.Copy(volumeFile, snapshotFile)
	if err != nil {
		return fmt.Errorf("failed to copy snapshot data: %w", err)
	}

	// Log audit event if access control is available
	if m.accessControl != nil {
		m.accessControl.LogAccess(
			"system", // User ID
			"",       // Tenant ID
			fmt.Sprintf("volume:%s", volumeName),
			"restore",
			fmt.Sprintf("Restored volume %s from snapshot %s", volumeName, snapshotName),
		)
	}

	return nil
}

// WriteToVolume writes data to a volume
func (m *Manager) WriteToVolume(ctx context.Context, volumeName string, data []byte, offset int64) error {
	m.volumesMutex.Lock()
	defer m.volumesMutex.Unlock()

	// Check if volume exists
	_, err := m.GetVolumeStatus(volumeName)
	if err != nil {
		return err
	}

	// Open volume file
	volumePath := filepath.Join(m.volumesDir, volumeName)
	volumeFile, err := os.OpenFile(volumePath, os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open volume file: %w", err)
	}
	defer volumeFile.Close()

	// Seek to offset
	_, err = volumeFile.Seek(offset, io.SeekStart)
	if err != nil {
		return fmt.Errorf("failed to seek to offset: %w", err)
	}

	// Write data to volume
	_, err = volumeFile.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write data to volume: %w", err)
	}

	// Update QoS stats if QoS manager is available
	if m.qosManager != nil {
		// Get tenant ID from volume metadata
		var tenantID string
		// For now, we don't have a way to get tenant ID from volume metadata
		// This would be implemented in a real system

		if tenantID != "" {
			stats := m.qosManager.GetTenantStats(tenantID)
			if stats != nil {
				stats.WriteOps++
				stats.WriteBytes += int64(len(data))
				stats.LastUpdateTime = time.Now().Unix()
			}
		}
	}

	return nil
}

// ReadFromVolume reads data from a volume
func (m *Manager) ReadFromVolume(ctx context.Context, volumeName string, offset, length int64) ([]byte, error) {
	m.volumesMutex.RLock()
	defer m.volumesMutex.RUnlock()

	// Check if volume exists
	_, err := m.GetVolumeStatus(volumeName)
	if err != nil {
		return nil, err
	}

	// Open volume file
	volumePath := filepath.Join(m.volumesDir, volumeName)
	volumeFile, err := os.Open(volumePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open volume file: %w", err)
	}
	defer volumeFile.Close()

	// Seek to offset
	_, err = volumeFile.Seek(offset, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("failed to seek to offset: %w", err)
	}

	// Read data from volume
	data := make([]byte, length)
	n, err := volumeFile.Read(data)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("failed to read data from volume: %w", err)
	}

	// Update QoS stats if QoS manager is available
	if m.qosManager != nil {
		// Get tenant ID from volume metadata
		var tenantID string
		// For now, we don't have a way to get tenant ID from volume metadata
		// This would be implemented in a real system

		if tenantID != "" {
			stats := m.qosManager.GetTenantStats(tenantID)
			if stats != nil {
				stats.ReadOps++
				stats.ReadBytes += int64(n)
				stats.LastUpdateTime = time.Now().Unix()
			}
		}
	}

	return data[:n], nil
}

// MonitorVolumeHealth monitors the health of a volume
func (m *Manager) MonitorVolumeHealth(ctx context.Context, volumeName string) error {
	// Get volume status
	volume, err := m.GetVolumeStatus(volumeName)
	if err != nil {
		return err
	}

	// Check if volume file exists
	volumePath := filepath.Join(m.volumesDir, volumeName)
	if _, err := os.Stat(volumePath); os.IsNotExist(err) {
		// Update volume state to failed
		err = m.UpdateVolumeState(volumeName, VolumeStateFailed)
		if err != nil {
			return fmt.Errorf("failed to update volume state: %w", err)
		}

		// Log audit event if access control is available
		if m.accessControl != nil {
			m.accessControl.LogAccess(
				"system", // User ID
				"",       // Tenant ID
				fmt.Sprintf("volume:%s", volumeName),
				"health_check",
				fmt.Sprintf("Volume %s is missing", volumeName),
			)
		}

		return fmt.Errorf("volume %s is missing", volumeName)
	}

	// Check if volume is readable
	volumeFile, err := os.Open(volumePath)
	if err != nil {
		// Update volume state to failed
		err = m.UpdateVolumeState(volumeName, VolumeStateFailed)
		if err != nil {
			return fmt.Errorf("failed to update volume state: %w", err)
		}

		// Log audit event if access control is available
		if m.accessControl != nil {
			m.accessControl.LogAccess(
				"system", // User ID
				"",       // Tenant ID
				fmt.Sprintf("volume:%s", volumeName),
				"health_check",
				fmt.Sprintf("Volume %s is not readable", volumeName),
			)
		}

		return fmt.Errorf("volume %s is not readable", volumeName)
	}
	volumeFile.Close()

	// Check if volume is writable
	volumeFile, err = os.OpenFile(volumePath, os.O_WRONLY, 0644)
	if err != nil {
		// Update volume state to failed
		err = m.UpdateVolumeState(volumeName, VolumeStateFailed)
		if err != nil {
			return fmt.Errorf("failed to update volume state: %w", err)
		}

		// Log audit event if access control is available
		if m.accessControl != nil {
			m.accessControl.LogAccess(
				"system", // User ID
				"",       // Tenant ID
				fmt.Sprintf("volume:%s", volumeName),
				"health_check",
				fmt.Sprintf("Volume %s is not writable", volumeName),
			)
		}

		return fmt.Errorf("volume %s is not writable", volumeName)
	}
	volumeFile.Close()

	// Update volume state to available if it was failed
	if volume.State == VolumeStateFailed {
		err = m.UpdateVolumeState(volumeName, VolumeStateAvailable)
		if err != nil {
			return fmt.Errorf("failed to update volume state: %w", err)
		}

		// Log audit event if access control is available
		if m.accessControl != nil {
			m.accessControl.LogAccess(
				"system", // User ID
				"",       // Tenant ID
				fmt.Sprintf("volume:%s", volumeName),
				"health_check",
				fmt.Sprintf("Volume %s is now available", volumeName),
			)
		}
	}

	return nil
}

// StartVolumeHealthMonitoring starts monitoring the health of all volumes
func (m *Manager) StartVolumeHealthMonitoring(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				volumes, err := m.GetVolumes()
				if err != nil {
					m.logger.WithError(err).Error("Failed to get volumes for health monitoring")
					continue
				}

				for _, volume := range volumes {
					err := m.MonitorVolumeHealth(ctx, volume.Name)
					if err != nil {
						m.logger.WithError(err).WithField("volume", volume.Name).Error("Volume health check failed")
					}
				}
			}
		}
	}()
}

// GetVolumeUsage returns the usage of a volume
func (m *Manager) GetVolumeUsage(ctx context.Context, volumeName string) (int64, error) {
	// Check if volume exists
	_, err := m.GetVolumeStatus(volumeName)
	if err != nil {
		return 0, err
	}

	// Open volume file
	volumePath := filepath.Join(m.volumesDir, volumeName)
	volumeFile, err := os.Open(volumePath)
	if err != nil {
		return 0, fmt.Errorf("failed to open volume file: %w", err)
	}
	defer volumeFile.Close()

	// Get file size
	fileInfo, err := volumeFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("failed to get file info: %w", err)
	}

	return fileInfo.Size(), nil
}

// GetTotalStorageUsage returns the total storage usage
func (m *Manager) GetTotalStorageUsage(ctx context.Context) (int64, error) {
	volumes, err := m.GetVolumes()
	if err != nil {
		return 0, err
	}

	var totalSize int64
	for _, volume := range volumes {
		totalSize += volume.Size
	}

	return totalSize, nil
}

// GetTenantStorageUsage returns the storage usage for a tenant
func (m *Manager) GetTenantStorageUsage(ctx context.Context, tenantID string) (int64, error) {
	// In a real system, we would filter volumes by tenant ID
	// For now, we don't have a way to associate volumes with tenants
	// This would be implemented in a real system
	return 0, nil
}

// SetVolumeMetadata sets metadata for a volume
func (m *Manager) SetVolumeMetadata(ctx context.Context, volumeName string, metadata map[string]string) error {
	// In a real system, we would store metadata in the database
	// For now, we don't have a way to store metadata
	// This would be implemented in a real system
	return nil
}

// GetVolumeMetadata gets metadata for a volume
func (m *Manager) GetVolumeMetadata(ctx context.Context, volumeName string) (map[string]string, error) {
	// In a real system, we would retrieve metadata from the database
	return nil, nil
}

// ListVolumes returns all volumes with context
func (m *Manager) ListVolumes(ctx context.Context) ([]VolumeStatus, error) {
	return m.GetVolumes()
}

// ListVolumesInterface is an adapter method to match the app.StorageManager interface
func (m *Manager) ListVolumesInterface(ctx context.Context) ([]interface{}, error) {
	volumes, err := m.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]interface{}, len(volumes))
	for i, v := range volumes {
		volume := v // Create a copy to avoid issues with references
		result[i] = &volume
	}

	return result, nil
}

// DeleteVolumeWithContext deletes a volume with context
func (m *Manager) DeleteVolumeWithContext(ctx context.Context, name string) error {
	// Just forward to the original method without context
	return m.DeleteVolume(name)
}

// CreateVolumeSimple creates a new volume with just a name and size
func (m *Manager) CreateVolumeSimple(ctx context.Context, name string, size int64) (*VolumeStatus, error) {
	options := VolumeOptions{
		Name: name,
		Size: size,
	}

	return m.CreateVolumeWithOptions(ctx, options)
}

// CreateVolumeSimpleInterface is an adapter method to match the app.StorageManager interface
func (m *Manager) CreateVolumeSimpleInterface(ctx context.Context, name string, size int64) (interface{}, error) {
	return m.CreateVolumeSimple(ctx, name, size)
}

// CreateVolumeInterface is an adapter method to match the app.StorageManager interface
func (m *Manager) CreateVolume(ctx context.Context, options interface{}) (interface{}, error) {
	if opts, ok := options.(VolumeOptions); ok {
		return m.CreateVolumeWithOptions(ctx, opts)
	}
	return nil, fmt.Errorf("invalid options type: %T", options)
}
