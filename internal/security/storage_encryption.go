package security

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// EncryptionStatus represents the encryption status of a storage resource
type EncryptionStatus string

const (
	// EncryptionStatusUnencrypted represents an unencrypted resource
	EncryptionStatusUnencrypted EncryptionStatus = "unencrypted"
	// EncryptionStatusEncrypting represents a resource being encrypted
	EncryptionStatusEncrypting EncryptionStatus = "encrypting"
	// EncryptionStatusEncrypted represents an encrypted resource
	EncryptionStatusEncrypted EncryptionStatus = "encrypted"
	// EncryptionStatusDecrypting represents a resource being decrypted
	EncryptionStatusDecrypting EncryptionStatus = "decrypting"
	// EncryptionStatusFailed represents a failed encryption/decryption operation
	EncryptionStatusFailed EncryptionStatus = "failed"
)

// EncryptionAlgorithmType represents the type of encryption algorithm
type EncryptionAlgorithmType string

const (
	// EncryptionAlgorithmAES256 represents AES-256 encryption
	EncryptionAlgorithmAES256 EncryptionAlgorithmType = "aes256"
	// EncryptionAlgorithmAES128 represents AES-128 encryption
	EncryptionAlgorithmAES128 EncryptionAlgorithmType = "aes128"
	// EncryptionAlgorithmChaCha20 represents ChaCha20 encryption
	EncryptionAlgorithmChaCha20 EncryptionAlgorithmType = "chacha20"
)

// StorageEncryptionKey represents an encryption key for storage resources
type StorageEncryptionKey struct {
	ID          string                  `json:"id"`
	Name        string                  `json:"name"`
	Algorithm   EncryptionAlgorithmType `json:"algorithm"`
	KeyData     []byte                  `json:"-"` // Skip serializing key data
	CreatedAt   int64                   `json:"created_at"`
	ExpiresAt   *int64                  `json:"expires_at,omitempty"`
	IsPrimary   bool                    `json:"is_primary"`
	KeyHash     string                  `json:"key_hash"`
	Description string                  `json:"description"`
	CreatedBy   string                  `json:"created_by"`
	Version     uint32                  `json:"version"`
	Labels      map[string]string       `json:"labels,omitempty"`
}

// EncryptedVolume represents an encrypted storage volume
type EncryptedVolume struct {
	ID              string           `json:"id"`
	VolumeID        string           `json:"volume_id"`
	KeyID           string           `json:"key_id"`
	Status          EncryptionStatus `json:"status"`
	CreatedAt       int64            `json:"created_at"`
	UpdatedAt       int64            `json:"updated_at"`
	EncryptedAt     *int64           `json:"encrypted_at,omitempty"`
	LastVerifiedAt  *int64           `json:"last_verified_at,omitempty"`
	EncryptionError *string          `json:"encryption_error,omitempty"`
}

// EncryptionOperation represents an encryption or decryption operation
type EncryptionOperation struct {
	ID          string           `json:"id"`
	VolumeID    string           `json:"volume_id"`
	KeyID       string           `json:"key_id"`
	Operation   string           `json:"operation"` // "encrypt" or "decrypt"
	Status      EncryptionStatus `json:"status"`
	StartedAt   int64            `json:"started_at"`
	CompletedAt *int64           `json:"completed_at,omitempty"`
	Error       *string          `json:"error,omitempty"`
	Progress    float64          `json:"progress"`
}

// StorageEncryptionManager handles encryption for storage resources
type StorageEncryptionManager struct {
	keys             map[string]StorageEncryptionKey
	encryptedVolumes map[string]EncryptedVolume
	operations       map[string]EncryptionOperation
	primaryKeyID     *string
	mutex            sync.RWMutex
	logger           *logrus.Logger
	secretManager    *SecretManager
}

// NewStorageEncryptionManager creates a new storage encryption manager
func NewStorageEncryptionManager(logger *logrus.Logger, secretManager *SecretManager) *StorageEncryptionManager {
	if logger == nil {
		logger = logrus.New()
		logger.SetLevel(logrus.InfoLevel)
	}

	manager := &StorageEncryptionManager{
		keys:             make(map[string]StorageEncryptionKey),
		encryptedVolumes: make(map[string]EncryptedVolume),
		operations:       make(map[string]EncryptionOperation),
		logger:           logger,
		secretManager:    secretManager,
	}

	// Initialize encryption keys
	if err := manager.InitializeEncryptionKeys(); err != nil {
		logger.WithError(err).Error("Failed to initialize encryption keys")
	}

	return manager
}

// InitializeEncryptionKeys initializes encryption keys
func (m *StorageEncryptionManager) InitializeEncryptionKeys() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Generate a new encryption key
	keyID := uuid.New().String()
	keyData := make([]byte, 32) // 256 bits
	if _, err := io.ReadFull(rand.Reader, keyData); err != nil {
		return fmt.Errorf("failed to generate random key: %w", err)
	}

	// Calculate key hash
	keyHash := fmt.Sprintf("%x", sha256.Sum256(keyData))

	key := StorageEncryptionKey{
		ID:          keyID,
		Name:        "Primary Storage Encryption Key",
		Algorithm:   EncryptionAlgorithmAES256,
		KeyData:     keyData,
		CreatedAt:   time.Now().Unix(),
		IsPrimary:   true,
		KeyHash:     keyHash,
		Description: "Default encryption key for storage volumes",
		CreatedBy:   "system",
		Version:     1,
	}

	// Store the key
	m.keys[keyID] = key

	// Set as primary key
	m.primaryKeyID = &keyID

	m.logger.WithField("key_id", keyID).Info("Initialized primary encryption key")

	return nil
}

// CreateEncryptionKey creates a new encryption key
func (m *StorageEncryptionManager) CreateEncryptionKey(
	ctx context.Context,
	name string,
	algorithm EncryptionAlgorithmType,
	description string,
	expiresAt *int64,
	makePrimary bool,
	createdBy string,
	labels map[string]string,
) (*StorageEncryptionKey, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Generate a new key ID
	keyID := uuid.New().String()

	// Generate key data based on algorithm
	var keySize int
	switch algorithm {
	case EncryptionAlgorithmAES256:
		keySize = 32 // 256 bits
	case EncryptionAlgorithmAES128:
		keySize = 16 // 128 bits
	case EncryptionAlgorithmChaCha20:
		keySize = 32 // 256 bits
	default:
		return nil, fmt.Errorf("unsupported encryption algorithm: %s", algorithm)
	}

	keyData := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, keyData); err != nil {
		return nil, fmt.Errorf("failed to generate random key: %w", err)
	}

	// Calculate key hash
	keyHash := fmt.Sprintf("%x", sha256.Sum256(keyData))

	key := StorageEncryptionKey{
		ID:          keyID,
		Name:        name,
		Algorithm:   algorithm,
		KeyData:     keyData,
		CreatedAt:   time.Now().Unix(),
		ExpiresAt:   expiresAt,
		IsPrimary:   makePrimary,
		KeyHash:     keyHash,
		Description: description,
		CreatedBy:   createdBy,
		Version:     1,
		Labels:      labels,
	}

	// If this is the new primary key, update all other keys
	if makePrimary {
		for id, existingKey := range m.keys {
			existingKey.IsPrimary = false
			m.keys[id] = existingKey
		}

		// Update the primary key ID
		m.primaryKeyID = &keyID
	}

	// Store the key
	m.keys[keyID] = key

	m.logger.WithFields(logrus.Fields{
		"key_id":    keyID,
		"algorithm": algorithm,
		"primary":   makePrimary,
	}).Info("Created encryption key")

	return &key, nil
}

// GetEncryptionKey gets an encryption key by ID
func (m *StorageEncryptionManager) GetEncryptionKey(ctx context.Context, keyID string) (*StorageEncryptionKey, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	key, exists := m.keys[keyID]
	if !exists {
		return nil, fmt.Errorf("encryption key not found: %s", keyID)
	}

	return &key, nil
}

// ListEncryptionKeys lists all encryption keys (without sensitive data)
func (m *StorageEncryptionManager) ListEncryptionKeys(ctx context.Context) []StorageEncryptionKey {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	// Return keys without the actual key data
	keys := make([]StorageEncryptionKey, 0, len(m.keys))
	for _, key := range m.keys {
		// Create a copy without the key data
		keyCopy := key
		keyCopy.KeyData = nil
		keys = append(keys, keyCopy)
	}

	return keys
}

// DeleteEncryptionKey deletes an encryption key
func (m *StorageEncryptionManager) DeleteEncryptionKey(ctx context.Context, keyID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	key, exists := m.keys[keyID]
	if !exists {
		return fmt.Errorf("encryption key not found: %s", keyID)
	}

	// Check if this is the primary key
	if key.IsPrimary {
		return errors.New("cannot delete primary encryption key")
	}

	// Check if any volumes are using this key
	for _, volume := range m.encryptedVolumes {
		if volume.KeyID == keyID {
			return fmt.Errorf("key is in use by volume: %s", volume.VolumeID)
		}
	}

	// Delete the key
	delete(m.keys, keyID)

	m.logger.WithField("key_id", keyID).Info("Deleted encryption key")

	return nil
}

// EncryptVolume encrypts a storage volume
func (m *StorageEncryptionManager) EncryptVolume(
	ctx context.Context,
	volumeID string,
	keyID string,
) (*EncryptionOperation, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Check if the volume is already encrypted
	if volume, exists := m.encryptedVolumes[volumeID]; exists {
		if volume.Status == EncryptionStatusEncrypted {
			return nil, fmt.Errorf("volume is already encrypted: %s", volumeID)
		}
		if volume.Status == EncryptionStatusEncrypting {
			return nil, fmt.Errorf("volume is already being encrypted: %s", volumeID)
		}
	}

	// Check if the key exists
	if _, exists := m.keys[keyID]; !exists {
		return nil, fmt.Errorf("encryption key not found: %s", keyID)
	}

	// Create a new operation
	operationID := uuid.New().String()
	now := time.Now().Unix()

	operation := EncryptionOperation{
		ID:        operationID,
		VolumeID:  volumeID,
		KeyID:     keyID,
		Operation: "encrypt",
		Status:    EncryptionStatusEncrypting,
		StartedAt: now,
		Progress:  0.0,
	}

	// Store the operation
	m.operations[operationID] = operation

	// Create or update the encrypted volume
	volume := EncryptedVolume{
		ID:        uuid.New().String(),
		VolumeID:  volumeID,
		KeyID:     keyID,
		Status:    EncryptionStatusEncrypting,
		CreatedAt: now,
		UpdatedAt: now,
	}

	m.encryptedVolumes[volumeID] = volume

	m.logger.WithFields(logrus.Fields{
		"operation_id": operationID,
		"volume_id":    volumeID,
		"key_id":       keyID,
	}).Info("Started volume encryption")

	// Start the encryption process in a goroutine
	go m.performEncryption(operationID, volumeID, keyID)

	return &operation, nil
}

// DecryptVolume decrypts a storage volume
func (m *StorageEncryptionManager) DecryptVolume(
	ctx context.Context,
	volumeID string,
) (*EncryptionOperation, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Check if the volume is encrypted
	volume, exists := m.encryptedVolumes[volumeID]
	if !exists {
		return nil, fmt.Errorf("volume is not encrypted: %s", volumeID)
	}

	if volume.Status == EncryptionStatusUnencrypted {
		return nil, fmt.Errorf("volume is not encrypted: %s", volumeID)
	}

	if volume.Status == EncryptionStatusDecrypting {
		return nil, fmt.Errorf("volume is already being decrypted: %s", volumeID)
	}

	// Create a new operation
	operationID := uuid.New().String()
	now := time.Now().Unix()

	operation := EncryptionOperation{
		ID:        operationID,
		VolumeID:  volumeID,
		KeyID:     volume.KeyID,
		Operation: "decrypt",
		Status:    EncryptionStatusDecrypting,
		StartedAt: now,
		Progress:  0.0,
	}

	// Store the operation
	m.operations[operationID] = operation

	// Update the encrypted volume
	volume.Status = EncryptionStatusDecrypting
	volume.UpdatedAt = now
	m.encryptedVolumes[volumeID] = volume

	m.logger.WithFields(logrus.Fields{
		"operation_id": operationID,
		"volume_id":    volumeID,
	}).Info("Started volume decryption")

	// Start the decryption process in a goroutine
	go m.performDecryption(operationID, volumeID, volume.KeyID)

	return &operation, nil
}

// GetEncryptionStatus gets the encryption status of a volume
func (m *StorageEncryptionManager) GetEncryptionStatus(ctx context.Context, volumeID string) (*EncryptedVolume, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	volume, exists := m.encryptedVolumes[volumeID]
	if !exists {
		return nil, fmt.Errorf("volume not found: %s", volumeID)
	}

	return &volume, nil
}

// performEncryption performs the actual encryption process
func (m *StorageEncryptionManager) performEncryption(operationID, volumeID, keyID string) {
	// In a real implementation, this would perform the actual encryption
	// For now, we'll simulate the process with a delay

	// Simulate encryption progress
	// Note: The sleep time was increased from 100ms to 500ms per iteration
	// to fix timing issues in tests where encryption status was being checked
	// before the operation had time to complete
	for progress := 0.0; progress < 1.0; progress += 0.1 {
		time.Sleep(500 * time.Millisecond)

		m.mutex.Lock()
		operation, exists := m.operations[operationID]
		if !exists {
			m.mutex.Unlock()
			return
		}

		operation.Progress = progress
		m.operations[operationID] = operation
		m.mutex.Unlock()
	}

	// Mark as completed
	now := time.Now().Unix()

	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Update the operation
	operation, exists := m.operations[operationID]
	if !exists {
		return
	}

	operation.Status = EncryptionStatusEncrypted
	operation.Progress = 1.0
	operation.CompletedAt = &now
	m.operations[operationID] = operation

	// Update the volume
	volume, exists := m.encryptedVolumes[volumeID]
	if !exists {
		return
	}

	volume.Status = EncryptionStatusEncrypted
	volume.UpdatedAt = now
	volume.EncryptedAt = &now
	m.encryptedVolumes[volumeID] = volume

	m.logger.WithFields(logrus.Fields{
		"operation_id": operationID,
		"volume_id":    volumeID,
	}).Info("Completed volume encryption")
}

// performDecryption performs the actual decryption process
func (m *StorageEncryptionManager) performDecryption(operationID, volumeID, keyID string) {
	// In a real implementation, this would perform the actual decryption
	// For now, we'll simulate the process with a delay

	// Simulate decryption progress
	// Note: The sleep time was increased from 100ms to 500ms per iteration
	// to fix timing issues in tests where decryption status was being checked
	// before the operation had time to complete
	for progress := 0.0; progress < 1.0; progress += 0.1 {
		time.Sleep(500 * time.Millisecond)

		m.mutex.Lock()
		operation, exists := m.operations[operationID]
		if !exists {
			m.mutex.Unlock()
			return
		}

		operation.Progress = progress
		m.operations[operationID] = operation
		m.mutex.Unlock()
	}

	// Mark as completed
	now := time.Now().Unix()

	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Update the operation
	operation, exists := m.operations[operationID]
	if !exists {
		return
	}

	operation.Status = EncryptionStatusUnencrypted
	operation.Progress = 1.0
	operation.CompletedAt = &now
	m.operations[operationID] = operation

	// Remove the volume from encrypted volumes
	delete(m.encryptedVolumes, volumeID)

	m.logger.WithFields(logrus.Fields{
		"operation_id": operationID,
		"volume_id":    volumeID,
	}).Info("Completed volume decryption")
}

// BackupKeys backs up encryption keys to the secret manager
func (m *StorageEncryptionManager) BackupKeys(ctx context.Context, userID string) error {
	if m.secretManager == nil {
		return errors.New("secret manager not available")
	}

	m.mutex.RLock()
	defer m.mutex.RUnlock()

	for keyID, key := range m.keys {
		// Serialize the key data
		keyData := base64.StdEncoding.EncodeToString(key.KeyData)

		// Create a secret for the key
		secretName := fmt.Sprintf("storage-encryption-key-%s", keyID)
		secretDesc := fmt.Sprintf("Backup of storage encryption key: %s", key.Name)

		// Store the key in the secret manager
		_, err := m.secretManager.CreateSecret(
			ctx,
			secretName,
			secretDesc,
			[]byte(keyData),
			userID,
			map[string]string{"type": "storage-encryption-key"},
			map[string]string{
				"key_id":     keyID,
				"algorithm":  string(key.Algorithm),
				"is_primary": fmt.Sprintf("%t", key.IsPrimary),
				"key_hash":   key.KeyHash,
				"version":    fmt.Sprintf("%d", key.Version),
			},
			nil,
			nil,
		)
		if err != nil {
			return fmt.Errorf("failed to backup key %s: %w", keyID, err)
		}
	}

	m.logger.WithField("key_count", len(m.keys)).Info("Backed up encryption keys")

	return nil
}

// RestoreKeys restores encryption keys from the secret manager
func (m *StorageEncryptionManager) RestoreKeys(ctx context.Context, userID string) error {
	if m.secretManager == nil {
		return errors.New("secret manager not available")
	}

	// Get all secrets with type "storage-encryption-key"
	secrets := m.secretManager.ListSecrets(ctx)
	restoredCount := 0

	for _, secret := range secrets {
		// Check if this is a storage encryption key
		if secret.Labels["type"] != "storage-encryption-key" {
			continue
		}

		// Get the secret data
		secretObj, err := m.secretManager.GetSecret(ctx, secret.ID, userID)
		if err != nil {
			m.logger.WithError(err).WithField("secret_id", secret.ID).Error("Failed to get secret")
			continue
		}

		// Decode the key data
		keyData, err := base64.StdEncoding.DecodeString(string(secretObj.Data))
		if err != nil {
			m.logger.WithError(err).WithField("secret_id", secret.ID).Error("Failed to decode key data")
			continue
		}

		// Extract metadata
		keyID := secret.Metadata["key_id"]
		algorithm := EncryptionAlgorithmType(secret.Metadata["algorithm"])
		isPrimary := secret.Metadata["is_primary"] == "true"
		keyHash := secret.Metadata["key_hash"]
		version, _ := fmt.Sscanf(secret.Metadata["version"], "%d")

		// Create the key
		key := StorageEncryptionKey{
			ID:          keyID,
			Name:        secret.Name,
			Algorithm:   algorithm,
			KeyData:     keyData,
			CreatedAt:   secret.CreatedAt,
			IsPrimary:   isPrimary,
			KeyHash:     keyHash,
			Description: secret.Description,
			CreatedBy:   secret.CreatedBy,
			Version:     uint32(version),
		}

		// Store the key
		m.mutex.Lock()
		m.keys[keyID] = key
		if isPrimary {
			m.primaryKeyID = &keyID
		}
		m.mutex.Unlock()

		restoredCount++
	}

	m.logger.WithField("restored_count", restoredCount).Info("Restored encryption keys")

	return nil
}
