package security

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// Secret represents a sensitive piece of information
type Secret struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Description    string            `json:"description"`
	Data           []byte            `json:"-"` // Skip serializing secret data
	Version        uint32            `json:"version"`
	CreatedAt      int64             `json:"created_at"`
	UpdatedAt      int64             `json:"updated_at"`
	CreatedBy      string            `json:"created_by"`
	LastAccessed   *int64            `json:"last_accessed,omitempty"`
	Labels         map[string]string `json:"labels"`
	Metadata       map[string]string `json:"metadata"`
	Expiration     *int64            `json:"expiration,omitempty"`
	RotationPeriod *int64            `json:"rotation_period,omitempty"`
	LastRotated    *int64            `json:"last_rotated,omitempty"`
}

// SecretReference represents a reference to a secret for a container
type SecretReference struct {
	SecretID    string  `json:"secret_id"`
	ContainerID string  `json:"container_id"`
	MountPath   string  `json:"mount_path"`
	EnvVar      *string `json:"env_var,omitempty"`
	FileName    *string `json:"file_name,omitempty"`
	FileMode    *uint32 `json:"file_mode,omitempty"`
	CreatedAt   int64   `json:"created_at"`
}

// SecretAction represents the type of action performed on a secret
type SecretAction string

const (
	// SecretActionCreate represents a secret creation action
	SecretActionCreate SecretAction = "create"
	// SecretActionRead represents a secret read action
	SecretActionRead SecretAction = "read"
	// SecretActionUpdate represents a secret update action
	SecretActionUpdate SecretAction = "update"
	// SecretActionDelete represents a secret deletion action
	SecretActionDelete SecretAction = "delete"
	// SecretActionRotate represents a secret rotation action
	SecretActionRotate SecretAction = "rotate"
	// SecretActionMount represents a secret mount action
	SecretActionMount SecretAction = "mount"
	// SecretActionUnmount represents a secret unmount action
	SecretActionUnmount SecretAction = "unmount"
)

// SecretAccessLog represents a log entry for secret access
type SecretAccessLog struct {
	ID        string       `json:"id"`
	SecretID  string       `json:"secret_id"`
	UserID    string       `json:"user_id"`
	Timestamp int64        `json:"timestamp"`
	Action    SecretAction `json:"action"`
	ClientIP  *string      `json:"client_ip,omitempty"`
	UserAgent *string      `json:"user_agent,omitempty"`
}

// EncryptionAlgorithm represents the algorithm used for encryption
type EncryptionAlgorithm string

const (
	// EncryptionAlgorithmAES256GCM represents AES-256-GCM encryption
	EncryptionAlgorithmAES256GCM EncryptionAlgorithm = "AES256GCM"
	// EncryptionAlgorithmChaCha20Poly1305 represents ChaCha20-Poly1305 encryption
	EncryptionAlgorithmChaCha20Poly1305 EncryptionAlgorithm = "ChaCha20Poly1305"
	// EncryptionAlgorithmRSA2048 represents RSA-2048 encryption
	EncryptionAlgorithmRSA2048 EncryptionAlgorithm = "RSA2048"
	// EncryptionAlgorithmRSA4096 represents RSA-4096 encryption
	EncryptionAlgorithmRSA4096 EncryptionAlgorithm = "RSA4096"
)

// EncryptionKey represents a key used for encrypting secrets
type EncryptionKey struct {
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	Algorithm EncryptionAlgorithm `json:"algorithm"`
	KeyData   []byte              `json:"-"` // Skip serializing key data
	CreatedAt int64               `json:"created_at"`
	ExpiresAt *int64              `json:"expires_at,omitempty"`
	IsPrimary bool                `json:"is_primary"`
}

// SecretManager handles secure storage and distribution of secrets
type SecretManager struct {
	secrets          map[string]Secret
	secretReferences map[string][]SecretReference // container_id -> references
	accessLogs       []SecretAccessLog
	encryptionKeys   map[string]EncryptionKey
	primaryKeyID     *string
	mutex            sync.RWMutex
	logger           *logrus.Logger
}

// NewSecretManager creates a new secret manager
func NewSecretManager(logger *logrus.Logger) *SecretManager {
	if logger == nil {
		logger = logrus.New()
		logger.SetLevel(logrus.InfoLevel)
	}

	sm := &SecretManager{
		secrets:          make(map[string]Secret),
		secretReferences: make(map[string][]SecretReference),
		accessLogs:       make([]SecretAccessLog, 0),
		encryptionKeys:   make(map[string]EncryptionKey),
		logger:           logger,
	}

	// Initialize encryption keys
	if err := sm.InitializeEncryptionKeys(); err != nil {
		logger.WithError(err).Error("Failed to initialize encryption keys")
	}

	return sm
}

// InitializeEncryptionKeys initializes encryption keys
func (s *SecretManager) InitializeEncryptionKeys() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Generate a new encryption key
	keyID := uuid.New().String()
	keyData := make([]byte, 32) // 256 bits
	if _, err := io.ReadFull(rand.Reader, keyData); err != nil {
		return fmt.Errorf("failed to generate random key: %w", err)
	}

	key := EncryptionKey{
		ID:        keyID,
		Name:      "Primary Key",
		Algorithm: EncryptionAlgorithmAES256GCM,
		KeyData:   keyData,
		CreatedAt: time.Now().Unix(),
		IsPrimary: true,
	}

	// Store the key
	s.encryptionKeys[keyID] = key

	// Set as primary key
	s.primaryKeyID = &keyID

	return nil
}

// CreateSecret creates a new secret
func (s *SecretManager) CreateSecret(
	ctx context.Context,
	name string,
	description string,
	data []byte,
	createdBy string,
	labels map[string]string,
	metadata map[string]string,
	expiration *int64,
	rotationPeriod *int64,
) (*Secret, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Generate a new ID
	secretID := uuid.New().String()
	now := time.Now().Unix()

	// Encrypt the secret data
	encryptedData, err := s.encryptData(data)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt data: %w", err)
	}

	secret := Secret{
		ID:             secretID,
		Name:           name,
		Description:    description,
		Data:           encryptedData,
		Version:        1,
		CreatedAt:      now,
		UpdatedAt:      now,
		CreatedBy:      createdBy,
		Labels:         labels,
		Metadata:       metadata,
		Expiration:     expiration,
		RotationPeriod: rotationPeriod,
		LastRotated:    &now,
	}

	// Store the secret
	s.secrets[secretID] = secret

	// Log the action
	if err := s.logAccess(
		secretID,
		createdBy,
		SecretActionCreate,
		nil,
		nil,
	); err != nil {
		s.logger.WithError(err).Error("Failed to log secret access")
	}

	return &secret, nil
}

// GetSecret gets a secret by ID
// This method was modified to decrypt the data before returning it
// Previously it was returning the encrypted data, which was causing issues
func (s *SecretManager) GetSecret(ctx context.Context, secretID string, userID string) (*Secret, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	secret, exists := s.secrets[secretID]
	if !exists {
		return nil, fmt.Errorf("secret not found: %s", secretID)
	}

	// Decrypt the data before returning it
	// This fix ensures that clients receive decrypted data instead of encrypted data
	if secret.Data != nil {
		decryptedData, err := s.decryptData(secret.Data)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt secret data: %w", err)
		}
		// Create a copy of the secret with decrypted data
		secretCopy := secret
		secretCopy.Data = decryptedData
		secret = secretCopy
	}

	// Update last accessed time
	now := time.Now().Unix()
	secret.LastAccessed = &now
	s.secrets[secretID] = secret

	// Log the access
	if err := s.logAccess(
		secretID,
		userID,
		SecretActionRead,
		nil,
		nil,
	); err != nil {
		s.logger.WithError(err).Error("Failed to log secret access")
	}

	return &secret, nil
}

// UpdateSecret updates a secret
func (s *SecretManager) UpdateSecret(
	ctx context.Context,
	secretID string,
	description *string,
	data []byte,
	labels map[string]string,
	metadata map[string]string,
	expiration *int64,
	rotationPeriod *int64,
	userID string,
) (*Secret, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	secret, exists := s.secrets[secretID]
	if !exists {
		return nil, fmt.Errorf("secret not found: %s", secretID)
	}

	now := time.Now().Unix()

	// Update fields if provided
	if description != nil {
		secret.Description = *description
	}

	if data != nil {
		encryptedData, err := s.encryptData(data)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt data: %w", err)
		}
		secret.Data = encryptedData
		secret.Version++
		secret.LastRotated = &now
	}

	if labels != nil {
		secret.Labels = labels
	}

	if metadata != nil {
		secret.Metadata = metadata
	}

	secret.Expiration = expiration
	secret.RotationPeriod = rotationPeriod
	secret.UpdatedAt = now

	// Store the updated secret
	s.secrets[secretID] = secret

	// Log the action
	if err := s.logAccess(
		secretID,
		userID,
		SecretActionUpdate,
		nil,
		nil,
	); err != nil {
		s.logger.WithError(err).Error("Failed to log secret access")
	}

	return &secret, nil
}

// DeleteSecret deletes a secret
func (s *SecretManager) DeleteSecret(ctx context.Context, secretID string, userID string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if _, exists := s.secrets[secretID]; !exists {
		return fmt.Errorf("secret not found: %s", secretID)
	}

	// Remove the secret
	delete(s.secrets, secretID)

	// Log the action
	if err := s.logAccess(
		secretID,
		userID,
		SecretActionDelete,
		nil,
		nil,
	); err != nil {
		s.logger.WithError(err).Error("Failed to log secret access")
	}

	return nil
}

// ListSecrets lists all secrets (without sensitive data)
func (s *SecretManager) ListSecrets(ctx context.Context) []Secret {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	// Return secrets without the actual secret data
	secrets := make([]Secret, 0, len(s.secrets))
	for _, secret := range s.secrets {
		// Create a copy without the data
		secretCopy := secret
		secretCopy.Data = nil
		secrets = append(secrets, secretCopy)
	}

	return secrets
}

// RotateSecret rotates a secret
func (s *SecretManager) RotateSecret(ctx context.Context, secretID string, newData []byte, userID string) (*Secret, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	secret, exists := s.secrets[secretID]
	if !exists {
		return nil, fmt.Errorf("secret not found: %s", secretID)
	}

	now := time.Now().Unix()

	// Encrypt the new data
	encryptedData, err := s.encryptData(newData)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt data: %w", err)
	}

	// Update the secret
	secret.Data = encryptedData
	secret.Version++
	secret.UpdatedAt = now
	secret.LastRotated = &now

	// Store the updated secret
	s.secrets[secretID] = secret

	// Log the action
	if err := s.logAccess(
		secretID,
		userID,
		SecretActionRotate,
		nil,
		nil,
	); err != nil {
		s.logger.WithError(err).Error("Failed to log secret access")
	}

	return &secret, nil
}

// MountSecretToContainer mounts a secret to a container
func (s *SecretManager) MountSecretToContainer(
	ctx context.Context,
	secretID string,
	containerID string,
	mountPath string,
	envVar *string,
	fileName *string,
	fileMode *uint32,
	userID string,
) (*SecretReference, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Check if the secret exists
	if _, exists := s.secrets[secretID]; !exists {
		return nil, fmt.Errorf("secret not found: %s", secretID)
	}

	now := time.Now().Unix()

	// Create the secret reference
	reference := SecretReference{
		SecretID:    secretID,
		ContainerID: containerID,
		MountPath:   mountPath,
		EnvVar:      envVar,
		FileName:    fileName,
		FileMode:    fileMode,
		CreatedAt:   now,
	}

	// Store the reference
	s.secretReferences[containerID] = append(s.secretReferences[containerID], reference)

	// Log the action
	if err := s.logAccess(
		secretID,
		userID,
		SecretActionMount,
		nil,
		nil,
	); err != nil {
		s.logger.WithError(err).Error("Failed to log secret access")
	}

	return &reference, nil
}

// UnmountSecretFromContainer unmounts a secret from a container
func (s *SecretManager) UnmountSecretFromContainer(
	ctx context.Context,
	secretID string,
	containerID string,
	userID string,
) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	containerRefs, exists := s.secretReferences[containerID]
	if !exists {
		return fmt.Errorf("no secret references found for container: %s", containerID)
	}

	// Remove the reference
	newRefs := make([]SecretReference, 0, len(containerRefs))
	for _, ref := range containerRefs {
		if ref.SecretID != secretID {
			newRefs = append(newRefs, ref)
		}
	}

	// If no more references, remove the container entry
	if len(newRefs) == 0 {
		delete(s.secretReferences, containerID)
	} else {
		s.secretReferences[containerID] = newRefs
	}

	// Log the action
	if err := s.logAccess(
		secretID,
		userID,
		SecretActionUnmount,
		nil,
		nil,
	); err != nil {
		s.logger.WithError(err).Error("Failed to log secret access")
	}

	return nil
}

// GetContainerSecrets gets all secrets mounted to a container
func (s *SecretManager) GetContainerSecrets(ctx context.Context, containerID string) []SecretReference {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	containerRefs, exists := s.secretReferences[containerID]
	if !exists {
		return []SecretReference{}
	}

	// Return a copy of the references
	refs := make([]SecretReference, len(containerRefs))
	copy(refs, containerRefs)
	return refs
}

// CheckRotationNeeded checks for secrets that need rotation
func (s *SecretManager) CheckRotationNeeded(ctx context.Context) []Secret {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	now := time.Now().Unix()
	needsRotation := make([]Secret, 0)

	for _, secret := range s.secrets {
		// Check if rotation period is set and exceeded
		if secret.RotationPeriod != nil && secret.LastRotated != nil {
			if now-*secret.LastRotated > *secret.RotationPeriod {
				// Create a copy without the data
				secretCopy := secret
				secretCopy.Data = nil
				needsRotation = append(needsRotation, secretCopy)
				continue
			}
		}

		// Check if secret is expired
		if secret.Expiration != nil && now > *secret.Expiration {
			// Create a copy without the data
			secretCopy := secret
			secretCopy.Data = nil
			needsRotation = append(needsRotation, secretCopy)
		}
	}

	return needsRotation
}

// encryptData encrypts data using the primary key
func (s *SecretManager) encryptData(data []byte) ([]byte, error) {
	// In a real implementation, we would use proper encryption
	// For now, we'll just do a simple base64 encoding with a prefix

	if s.primaryKeyID == nil {
		return nil, errors.New("no primary encryption key found")
	}

	_, exists := s.encryptionKeys[*s.primaryKeyID]
	if !exists {
		return nil, errors.New("primary encryption key not found")
	}

	// In a real implementation, we would use the key to encrypt the data
	// For now, just base64 encode it with a prefix indicating the key ID
	encoded := base64.StdEncoding.EncodeToString(data)
	prefixed := fmt.Sprintf("%s:%s", *s.primaryKeyID, encoded)

	return []byte(prefixed), nil
}

// decryptData decrypts data using the appropriate key
func (s *SecretManager) decryptData(data []byte) ([]byte, error) {
	// In a real implementation, we would use proper decryption
	// For now, we'll just do a simple base64 decoding

	dataStr := string(data)
	parts := strings.SplitN(dataStr, ":", 2)
	if len(parts) != 2 {
		return nil, errors.New("invalid encrypted data format")
	}

	keyID := parts[0]
	encoded := parts[1]

	// Check if the key exists
	_, exists := s.encryptionKeys[keyID]
	if !exists {
		return nil, fmt.Errorf("encryption key not found: %s", keyID)
	}

	// Decode the data
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to decode data: %w", err)
	}

	return decoded, nil
}

// logAccess logs a secret access
func (s *SecretManager) logAccess(
	secretID string,
	userID string,
	action SecretAction,
	clientIP *string,
	userAgent *string,
) error {
	logEntry := SecretAccessLog{
		ID:        uuid.New().String(),
		SecretID:  secretID,
		UserID:    userID,
		Timestamp: time.Now().Unix(),
		Action:    action,
		ClientIP:  clientIP,
		UserAgent: userAgent,
	}

	s.accessLogs = append(s.accessLogs, logEntry)

	// In a real implementation, we would also write to a persistent store

	return nil
}

// GetSecretAccessLogs gets access logs for a secret
func (s *SecretManager) GetSecretAccessLogs(ctx context.Context, secretID string) []SecretAccessLog {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	logs := make([]SecretAccessLog, 0)
	for _, log := range s.accessLogs {
		if log.SecretID == secretID {
			logs = append(logs, log)
		}
	}

	return logs
}

// CreateEncryptionKey creates a new encryption key
func (s *SecretManager) CreateEncryptionKey(
	ctx context.Context,
	name string,
	algorithm EncryptionAlgorithm,
	expiresAt *int64,
	makePrimary bool,
) (*EncryptionKey, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Generate a new key ID
	keyID := uuid.New().String()

	// Generate key data based on algorithm
	var keySize int
	switch algorithm {
	case EncryptionAlgorithmAES256GCM:
		keySize = 32 // 256 bits
	case EncryptionAlgorithmChaCha20Poly1305:
		keySize = 32 // 256 bits
	case EncryptionAlgorithmRSA2048:
		keySize = 256 // 2048 bits
	case EncryptionAlgorithmRSA4096:
		keySize = 512 // 4096 bits
	default:
		return nil, fmt.Errorf("unsupported encryption algorithm: %s", algorithm)
	}

	keyData := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, keyData); err != nil {
		return nil, fmt.Errorf("failed to generate random key: %w", err)
	}

	key := EncryptionKey{
		ID:        keyID,
		Name:      name,
		Algorithm: algorithm,
		KeyData:   keyData,
		CreatedAt: time.Now().Unix(),
		ExpiresAt: expiresAt,
		IsPrimary: makePrimary,
	}

	// If this is the new primary key, update all other keys
	if makePrimary {
		for id, existingKey := range s.encryptionKeys {
			existingKey.IsPrimary = false
			s.encryptionKeys[id] = existingKey
		}

		// Update the primary key ID
		s.primaryKeyID = &keyID
	}

	// Store the key
	s.encryptionKeys[keyID] = key

	return &key, nil
}

// ListEncryptionKeys lists all encryption keys (without sensitive data)
func (s *SecretManager) ListEncryptionKeys(ctx context.Context) []EncryptionKey {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	// Return keys without the actual key data
	keys := make([]EncryptionKey, 0, len(s.encryptionKeys))
	for _, key := range s.encryptionKeys {
		// Create a copy without the key data
		keyCopy := key
		keyCopy.KeyData = nil
		keys = append(keys, keyCopy)
	}

	return keys
}

// RotateEncryptionKey rotates all secrets using a new encryption key
func (s *SecretManager) RotateEncryptionKey(ctx context.Context, userID string) error {
	// Create a new encryption key
	_, err := s.CreateEncryptionKey(
		ctx,
		"Rotated Primary Key",
		EncryptionAlgorithmAES256GCM,
		nil,
		true,
	)
	if err != nil {
		return fmt.Errorf("failed to create new encryption key: %w", err)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Re-encrypt all secrets with the new key
	for secretID, secret := range s.secrets {
		// Decrypt with old key
		decrypted, err := s.decryptData(secret.Data)
		if err != nil {
			return fmt.Errorf("failed to decrypt secret %s: %w", secretID, err)
		}

		// Encrypt with new key
		encrypted, err := s.encryptData(decrypted)
		if err != nil {
			return fmt.Errorf("failed to encrypt secret %s: %w", secretID, err)
		}

		// Update the secret
		secret.Data = encrypted
		secret.Version++
		secret.UpdatedAt = time.Now().Unix()
		s.secrets[secretID] = secret

		// Log the rotation
		if err := s.logAccess(
			secretID,
			userID,
			SecretActionRotate,
			nil,
			nil,
		); err != nil {
			s.logger.WithError(err).Error("Failed to log secret access")
		}
	}

	s.logger.WithField("secret_count", len(s.secrets)).Info("Rotated encryption key and re-encrypted secrets")

	return nil
}
