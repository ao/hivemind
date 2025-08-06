package security

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

// User represents a user in the system
type User struct {
	ID           string   `json:"id"`
	Username     string   `json:"username"`
	PasswordHash string   `json:"-"` // Skip serializing password hash
	Email        string   `json:"email"`
	FullName     string   `json:"full_name"`
	Roles        []string `json:"roles"`
	Groups       []string `json:"groups"`
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`
	LastLogin    *int64   `json:"last_login,omitempty"`
	Active       bool     `json:"active"`
}

// Role represents a set of permissions
type Role struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Permissions []Permission `json:"permissions"`
	CreatedAt   int64        `json:"created_at"`
	UpdatedAt   int64        `json:"updated_at"`
}

// Group represents a collection of users
type Group struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Members     []string `json:"members"` // User IDs
	Roles       []string `json:"roles"`   // Role IDs
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
}

// Permission represents an action that can be performed on a resource
type Permission struct {
	Resource string          `json:"resource"`
	Action   string          `json:"action"`
	Scope    PermissionScope `json:"scope"`
}

// PermissionScope represents the scope of a permission
type PermissionScope interface {
	isPermissionScope()
	String() string
}

// GlobalScope represents a global permission scope
type GlobalScope struct{}

func (GlobalScope) isPermissionScope() {}
func (GlobalScope) String() string     { return "global" }

// NamespaceScope represents a namespace-scoped permission
type NamespaceScope struct {
	Namespace string
}

func (NamespaceScope) isPermissionScope() {}
func (n NamespaceScope) String() string   { return fmt.Sprintf("namespace:%s", n.Namespace) }

// ResourceScope represents a resource-scoped permission
type ResourceScope struct {
	Resource string
}

func (ResourceScope) isPermissionScope() {}
func (r ResourceScope) String() string   { return fmt.Sprintf("resource:%s", r.Resource) }

// AuthRequest represents an authentication request
type AuthRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// AuthResponse represents an authentication response
type AuthResponse struct {
	Token     string   `json:"token"`
	UserID    string   `json:"user_id"`
	Username  string   `json:"username"`
	Roles     []string `json:"roles"`
	ExpiresAt int64    `json:"expires_at"`
}

// AuditStatus represents the status of an audit log entry
type AuditStatus string

const (
	// AuditStatusSuccess indicates a successful action
	AuditStatusSuccess AuditStatus = "success"
	// AuditStatusFailure indicates a failed action
	AuditStatusFailure AuditStatus = "failure"
	// AuditStatusDenied indicates a denied action
	AuditStatusDenied AuditStatus = "denied"
)

// AuditLogEntry represents an entry in the audit log
type AuditLogEntry struct {
	ID         string            `json:"id"`
	Timestamp  int64             `json:"timestamp"`
	UserID     string            `json:"user_id"`
	Username   string            `json:"username"`
	Action     string            `json:"action"`
	Resource   string            `json:"resource"`
	ResourceID *string           `json:"resource_id,omitempty"`
	Status     AuditStatus       `json:"status"`
	Details    map[string]string `json:"details"`
	ClientIP   *string           `json:"client_ip,omitempty"`
	UserAgent  *string           `json:"user_agent,omitempty"`
}

// RbacManager handles authentication, authorization, and audit logging
type RbacManager struct {
	users        map[string]User
	roles        map[string]Role
	groups       map[string]Group
	activeTokens map[string]string // token -> user_id
	auditLogs    []AuditLogEntry
	mutex        sync.RWMutex
	logger       *logrus.Logger
}

// NewRBACManager creates a new RBAC manager (alias for NewRbacManager for backward compatibility)
func NewRBACManager(logger *logrus.Logger) *RbacManager {
	return NewRbacManager(logger)
}

// NewRbacManager creates a new RBAC manager
func NewRbacManager(logger *logrus.Logger) *RbacManager {
	if logger == nil {
		logger = logrus.New()
		logger.SetLevel(logrus.InfoLevel)
	}

	rbac := &RbacManager{
		users:        make(map[string]User),
		roles:        make(map[string]Role),
		groups:       make(map[string]Group),
		activeTokens: make(map[string]string),
		auditLogs:    make([]AuditLogEntry, 0),
		logger:       logger,
	}

	// Initialize default roles and admin user
	if err := rbac.InitializeDefaults(); err != nil {
		logger.WithError(err).Error("Failed to initialize RBAC defaults")
	}

	return rbac
}

// InitializeDefaults initializes default roles and admin user
func (r *RbacManager) InitializeDefaults() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	now := time.Now().Unix()

	// Create default roles
	adminRole := Role{
		ID:          "admin",
		Name:        "Administrator",
		Description: "Full system access",
		Permissions: []Permission{
			{
				Resource: "*",
				Action:   "*",
				Scope:    GlobalScope{},
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	operatorRole := Role{
		ID:          "operator",
		Name:        "Operator",
		Description: "Can manage containers and services",
		Permissions: []Permission{
			{
				Resource: "container",
				Action:   "*",
				Scope:    GlobalScope{},
			},
			{
				Resource: "service",
				Action:   "*",
				Scope:    GlobalScope{},
			},
			{
				Resource: "volume",
				Action:   "*",
				Scope:    GlobalScope{},
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	viewerRole := Role{
		ID:          "viewer",
		Name:        "Viewer",
		Description: "Read-only access",
		Permissions: []Permission{
			{
				Resource: "*",
				Action:   "get",
				Scope:    GlobalScope{},
			},
			{
				Resource: "*",
				Action:   "list",
				Scope:    GlobalScope{},
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Add roles
	r.roles["admin"] = adminRole
	r.roles["operator"] = operatorRole
	r.roles["viewer"] = viewerRole

	// Create admin user with a default password
	// In a real system, this would be set during installation or from environment variables
	passwordHash, err := r.hashPassword("admin123")
	if err != nil {
		return fmt.Errorf("failed to hash admin password: %w", err)
	}

	adminUser := User{
		ID:           "admin",
		Username:     "admin",
		PasswordHash: passwordHash,
		Email:        "admin@example.com",
		FullName:     "System Administrator",
		Roles:        []string{"admin"},
		Groups:       []string{},
		CreatedAt:    now,
		UpdatedAt:    now,
		Active:       true,
	}

	// Add admin user
	r.users["admin"] = adminUser

	return nil
}

// CreateUser creates a new user
func (r *RbacManager) CreateUser(ctx context.Context, user User) (*User, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Check if username already exists
	for _, u := range r.users {
		if u.Username == user.Username {
			return nil, errors.New("username already exists")
		}
	}

	// Store the user
	r.users[user.ID] = user

	// Log the action
	if err := r.LogAudit(
		ctx,
		"system",
		"admin",
		"create_user",
		"user",
		&user.ID,
		AuditStatusSuccess,
		map[string]string{},
		nil,
		nil,
	); err != nil {
		r.logger.WithError(err).Error("Failed to log audit event")
	}

	return &user, nil
}

// GetUser gets a user by ID
func (r *RbacManager) GetUser(ctx context.Context, userID string) (*User, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	user, exists := r.users[userID]
	if !exists {
		return nil, fmt.Errorf("user not found: %s", userID)
	}

	return &user, nil
}

// GetUserByUsername gets a user by username
func (r *RbacManager) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	for _, user := range r.users {
		if user.Username == username {
			return &user, nil
		}
	}

	return nil, fmt.Errorf("user not found: %s", username)
}

// UpdateUser updates a user
func (r *RbacManager) UpdateUser(ctx context.Context, userID string, updatedUser User) (*User, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Check if user exists
	_, exists := r.users[userID]
	if !exists {
		return nil, fmt.Errorf("user not found: %s", userID)
	}

	// Check if username is being changed and if it conflicts
	existingUser := r.users[userID]
	if existingUser.Username != updatedUser.Username {
		for _, u := range r.users {
			if u.ID != userID && u.Username == updatedUser.Username {
				return nil, errors.New("username already exists")
			}
		}
	}

	// Update the user
	r.users[userID] = updatedUser

	return &updatedUser, nil
}

// DeleteUser deletes a user
func (r *RbacManager) DeleteUser(ctx context.Context, userID string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Check if user exists
	_, exists := r.users[userID]
	if !exists {
		return fmt.Errorf("user not found: %s", userID)
	}

	// Delete the user
	delete(r.users, userID)

	// Log the action
	if err := r.LogAudit(
		ctx,
		"system",
		"admin",
		"delete_user",
		"user",
		&userID,
		AuditStatusSuccess,
		map[string]string{},
		nil,
		nil,
	); err != nil {
		r.logger.WithError(err).Error("Failed to log audit event")
	}

	return nil
}

// ListUsers lists all users
func (r *RbacManager) ListUsers(ctx context.Context) []User {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	users := make([]User, 0, len(r.users))
	for _, user := range r.users {
		users = append(users, user)
	}

	return users
}

// CreateRole creates a new role
func (r *RbacManager) CreateRole(ctx context.Context, role Role) (*Role, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Check if role name already exists
	for _, r := range r.roles {
		if r.Name == role.Name {
			return nil, errors.New("role name already exists")
		}
	}

	// Store the role
	r.roles[role.ID] = role

	// Log the action
	if err := r.LogAudit(
		ctx,
		"system",
		"admin",
		"create_role",
		"role",
		&role.ID,
		AuditStatusSuccess,
		map[string]string{},
		nil,
		nil,
	); err != nil {
		r.logger.WithError(err).Error("Failed to log audit event")
	}

	return &role, nil
}

// GetRole gets a role by ID
func (r *RbacManager) GetRole(ctx context.Context, roleID string) (*Role, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	role, exists := r.roles[roleID]
	if !exists {
		return nil, fmt.Errorf("role not found: %s", roleID)
	}

	return &role, nil
}

// UpdateRole updates a role
func (r *RbacManager) UpdateRole(ctx context.Context, roleID string, updatedRole Role) (*Role, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Check if role exists
	_, exists := r.roles[roleID]
	if !exists {
		return nil, fmt.Errorf("role not found: %s", roleID)
	}

	// Check if role name is being changed and if it conflicts
	existingRole := r.roles[roleID]
	if existingRole.Name != updatedRole.Name {
		for _, r := range r.roles {
			if r.ID != roleID && r.Name == updatedRole.Name {
				return nil, errors.New("role name already exists")
			}
		}
	}

	// Update the role
	r.roles[roleID] = updatedRole

	return &updatedRole, nil
}

// DeleteRole deletes a role
func (r *RbacManager) DeleteRole(ctx context.Context, roleID string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Check if role exists
	_, exists := r.roles[roleID]
	if !exists {
		return fmt.Errorf("role not found: %s", roleID)
	}

	// Delete the role
	delete(r.roles, roleID)

	// Log the action
	if err := r.LogAudit(
		ctx,
		"system",
		"admin",
		"delete_role",
		"role",
		&roleID,
		AuditStatusSuccess,
		map[string]string{},
		nil,
		nil,
	); err != nil {
		r.logger.WithError(err).Error("Failed to log audit event")
	}

	return nil
}

// ListRoles lists all roles
func (r *RbacManager) ListRoles(ctx context.Context) []Role {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	roles := make([]Role, 0, len(r.roles))
	for _, role := range r.roles {
		roles = append(roles, role)
	}

	return roles
}

// CreateGroup creates a new group
func (r *RbacManager) CreateGroup(ctx context.Context, group Group) (*Group, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Check if group name already exists
	for _, g := range r.groups {
		if g.Name == group.Name {
			return nil, errors.New("group name already exists")
		}
	}

	// Store the group
	r.groups[group.ID] = group

	// Log the action
	if err := r.LogAudit(
		ctx,
		"system",
		"admin",
		"create_group",
		"group",
		&group.ID,
		AuditStatusSuccess,
		map[string]string{},
		nil,
		nil,
	); err != nil {
		r.logger.WithError(err).Error("Failed to log audit event")
	}

	return &group, nil
}

// GetGroup gets a group by ID
func (r *RbacManager) GetGroup(ctx context.Context, groupID string) (*Group, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	group, exists := r.groups[groupID]
	if !exists {
		return nil, fmt.Errorf("group not found: %s", groupID)
	}

	return &group, nil
}

// UpdateGroup updates a group
func (r *RbacManager) UpdateGroup(ctx context.Context, groupID string, updatedGroup Group) (*Group, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Check if group exists
	_, exists := r.groups[groupID]
	if !exists {
		return nil, fmt.Errorf("group not found: %s", groupID)
	}

	// Check if group name is being changed and if it conflicts
	existingGroup := r.groups[groupID]
	if existingGroup.Name != updatedGroup.Name {
		for _, g := range r.groups {
			if g.ID != groupID && g.Name == updatedGroup.Name {
				return nil, errors.New("group name already exists")
			}
		}
	}

	// Update the group
	r.groups[groupID] = updatedGroup

	return &updatedGroup, nil
}

// DeleteGroup deletes a group
func (r *RbacManager) DeleteGroup(ctx context.Context, groupID string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Check if group exists
	_, exists := r.groups[groupID]
	if !exists {
		return fmt.Errorf("group not found: %s", groupID)
	}

	// Delete the group
	delete(r.groups, groupID)

	// Log the action
	if err := r.LogAudit(
		ctx,
		"system",
		"admin",
		"delete_group",
		"group",
		&groupID,
		AuditStatusSuccess,
		map[string]string{},
		nil,
		nil,
	); err != nil {
		r.logger.WithError(err).Error("Failed to log audit event")
	}

	return nil
}

// ListGroups lists all groups
func (r *RbacManager) ListGroups(ctx context.Context) []Group {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	groups := make([]Group, 0, len(r.groups))
	for _, group := range r.groups {
		groups = append(groups, group)
	}

	return groups
}

// AddUserToGroup adds a user to a group
func (r *RbacManager) AddUserToGroup(ctx context.Context, userID, groupID string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Check if group exists
	group, exists := r.groups[groupID]
	if !exists {
		return fmt.Errorf("group not found: %s", groupID)
	}

	// Check if user is already in the group
	for _, member := range group.Members {
		if member == userID {
			return nil // User is already in the group
		}
	}

	// Add user to the group
	group.Members = append(group.Members, userID)
	group.UpdatedAt = time.Now().Unix()
	r.groups[groupID] = group

	return nil
}

// RemoveUserFromGroup removes a user from a group
func (r *RbacManager) RemoveUserFromGroup(ctx context.Context, userID, groupID string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Check if group exists
	group, exists := r.groups[groupID]
	if !exists {
		return fmt.Errorf("group not found: %s", groupID)
	}

	// Remove user from the group
	newMembers := make([]string, 0, len(group.Members))
	for _, member := range group.Members {
		if member != userID {
			newMembers = append(newMembers, member)
		}
	}
	group.Members = newMembers
	group.UpdatedAt = time.Now().Unix()
	r.groups[groupID] = group

	return nil
}

// Authenticate authenticates a user
func (r *RbacManager) Authenticate(ctx context.Context, authRequest *AuthRequest) (*AuthResponse, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Find user by username
	var user *User
	for _, u := range r.users {
		if u.Username == authRequest.Username {
			user = &u
			break
		}
	}

	if user == nil {
		// Log failed login attempt
		if err := r.LogAudit(
			ctx,
			"unknown",
			authRequest.Username,
			"login",
			"auth",
			nil,
			AuditStatusFailure,
			map[string]string{},
			nil,
			nil,
		); err != nil {
			r.logger.WithError(err).Error("Failed to log audit event")
		}

		return nil, errors.New("invalid username or password")
	}

	// Check if user is active
	if !user.Active {
		return nil, errors.New("user account is inactive")
	}

	// Verify password
	if !r.verifyPassword(authRequest.Password, user.PasswordHash) {
		// Log failed login attempt
		if err := r.LogAudit(
			ctx,
			user.ID,
			user.Username,
			"login",
			"auth",
			nil,
			AuditStatusFailure,
			map[string]string{},
			nil,
			nil,
		); err != nil {
			r.logger.WithError(err).Error("Failed to log audit event")
		}

		return nil, errors.New("invalid username or password")
	}

	// Generate token
	token := r.generateToken()
	expiresAt := time.Now().Unix() + 3600 // 1 hour

	// Store token
	r.activeTokens[token] = user.ID

	// Update last login time
	userCopy := *user
	now := time.Now().Unix()
	userCopy.LastLogin = &now
	r.users[user.ID] = userCopy

	// Log successful login
	if err := r.LogAudit(
		ctx,
		user.ID,
		user.Username,
		"login",
		"auth",
		nil,
		AuditStatusSuccess,
		map[string]string{},
		nil,
		nil,
	); err != nil {
		r.logger.WithError(err).Error("Failed to log audit event")
	}

	return &AuthResponse{
		Token:     token,
		UserID:    user.ID,
		Username:  user.Username,
		Roles:     user.Roles,
		ExpiresAt: expiresAt,
	}, nil
}

// ValidateToken validates a token and gets the associated user
func (r *RbacManager) ValidateToken(ctx context.Context, token string) (*User, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	userID, exists := r.activeTokens[token]
	if !exists {
		return nil, errors.New("invalid token")
	}

	user, exists := r.users[userID]
	if !exists {
		return nil, errors.New("user not found")
	}

	return &user, nil
}

// Logout invalidates a token
func (r *RbacManager) Logout(ctx context.Context, token string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	delete(r.activeTokens, token)
	return nil
}

// CheckPermission checks if a user has permission to perform an action on a resource
func (r *RbacManager) CheckPermission(
	ctx context.Context,
	userID string,
	resource string,
	action string,
	scope PermissionScope,
) (bool, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	// Get user
	user, exists := r.users[userID]
	if !exists {
		r.logger.WithField("user_id", userID).Debug("CheckPermission: User not found")
		return false, nil
	}

	// If user is inactive, deny permission
	if !user.Active {
		r.logger.WithField("user_id", userID).Debug("CheckPermission: User is inactive")
		return false, nil
	}

	r.logger.WithFields(logrus.Fields{
		"user_id":  userID,
		"roles":    user.Roles,
		"resource": resource,
		"action":   action,
		"scope":    scope.String(),
	}).Debug("CheckPermission: Checking permission")

	// Get all roles for the user (directly assigned and via groups)
	roleIDs := make(map[string]struct{})
	roleNames := make(map[string]struct{})

	// This section was enhanced to handle both role IDs and role names
	// This fixes the issue where roles were assigned by name but checked by ID
	// Now we handle both cases by checking if the roleIdentifier exists as a role ID
	// If not, we assume it's a role name and handle it separately
	for _, roleIdentifier := range user.Roles {
		// Check if this is a role ID or role name
		if _, exists := r.roles[roleIdentifier]; exists {
			// It's a role ID
			roleIDs[roleIdentifier] = struct{}{}
			r.logger.WithFields(logrus.Fields{
				"role_id": roleIdentifier,
			}).Debug("CheckPermission: Found role by ID")
		} else {
			// Assume it's a role name
			roleNames[roleIdentifier] = struct{}{}
			r.logger.WithFields(logrus.Fields{
				"role_name": roleIdentifier,
			}).Debug("CheckPermission: Found role by name")
		}
	}

	// Add roles from groups
	for _, group := range r.groups {
		for _, member := range group.Members {
			if member == userID {
				for _, roleID := range group.Roles {
					roleIDs[roleID] = struct{}{}
					r.logger.WithFields(logrus.Fields{
						"group_id": group.ID,
						"role_id":  roleID,
					}).Debug("CheckPermission: Added role from group")
				}
				break
			}
		}
	}

	// Debug: Print all available roles
	r.logger.Debug("CheckPermission: Available roles:")
	for id, role := range r.roles {
		r.logger.WithFields(logrus.Fields{
			"role_id":   id,
			"role_name": role.Name,
		}).Debug("CheckPermission: Available role")
	}

	// Check if any role grants the permission
	// First check roles by ID
	for roleID := range roleIDs {
		role, exists := r.roles[roleID]
		if !exists {
			r.logger.WithField("role_id", roleID).Debug("CheckPermission: Role ID not found")
			continue
		}

		r.logger.WithFields(logrus.Fields{
			"role_id":     roleID,
			"role_name":   role.Name,
			"permissions": len(role.Permissions),
		}).Debug("CheckPermission: Checking role by ID")

		for _, permission := range role.Permissions {
			r.logger.WithFields(logrus.Fields{
				"permission_resource": permission.Resource,
				"permission_action":   permission.Action,
				"requested_resource":  resource,
				"requested_action":    action,
			}).Debug("CheckPermission: Checking permission match")

			if r.permissionMatches(&permission, resource, action, scope) {
				r.logger.Debug("CheckPermission: Permission granted by role ID")
				return true, nil
			}
		}
	}

	// Then check roles by name
	// This section was added to support role assignment by name
	// It iterates through all roles to find ones with matching names
	for roleName := range roleNames {
		// Find the role with this name
		roleFound := false
		for _, role := range r.roles {
			if role.Name == roleName {
				roleFound = true
				r.logger.WithFields(logrus.Fields{
					"role_name":   roleName,
					"role_id":     role.ID,
					"permissions": len(role.Permissions),
				}).Debug("CheckPermission: Checking role by name")

				for _, permission := range role.Permissions {
					r.logger.WithFields(logrus.Fields{
						"permission_resource": permission.Resource,
						"permission_action":   permission.Action,
						"requested_resource":  resource,
						"requested_action":    action,
					}).Debug("CheckPermission: Checking permission match")

					if r.permissionMatches(&permission, resource, action, scope) {
						r.logger.Debug("CheckPermission: Permission granted by role name")
						return true, nil
					}
				}
				break // Found the role, no need to continue searching
			}
		}

		if !roleFound {
			r.logger.WithField("role_name", roleName).Debug("CheckPermission: Role name not found")
		}
	}

	r.logger.Debug("CheckPermission: Permission denied")
	return false, nil
}

// permissionMatches checks if a permission matches the requested resource, action, and scope
func (r *RbacManager) permissionMatches(
	permission *Permission,
	resource string,
	action string,
	scope PermissionScope,
) bool {
	// Check resource
	if permission.Resource != "*" && permission.Resource != resource {
		return false
	}

	// Check action
	if permission.Action != "*" && permission.Action != action {
		return false
	}

	// Check scope
	switch permScope := permission.Scope.(type) {
	// Global permission applies to all scopes
	case GlobalScope:
		return true

	// Namespace permission applies to the same namespace or resources in that namespace
	case NamespaceScope:
		switch reqScope := scope.(type) {
		case NamespaceScope:
			return permScope.Namespace == reqScope.Namespace
		case ResourceScope:
			return strings.HasPrefix(reqScope.Resource, permScope.Namespace+"/")
		default:
			return false
		}

	// Resource permission only applies to the exact resource
	case ResourceScope:
		if reqScope, ok := scope.(ResourceScope); ok {
			return permScope.Resource == reqScope.Resource
		}
		return false

	// Other combinations don't match
	default:
		return false
	}
}

// LogAudit logs an audit event
func (r *RbacManager) LogAudit(
	ctx context.Context,
	userID string,
	username string,
	action string,
	resource string,
	resourceID *string,
	status AuditStatus,
	details map[string]string,
	clientIP *string,
	userAgent *string,
) error {
	// We don't need to acquire the mutex here if it's already held by the caller
	// This avoids deadlocks when LogAudit is called from methods that already hold the mutex

	entry := AuditLogEntry{
		ID:         uuid.New().String(),
		Timestamp:  time.Now().Unix(),
		UserID:     userID,
		Username:   username,
		Action:     action,
		Resource:   resource,
		ResourceID: resourceID,
		Status:     status,
		Details:    details,
		ClientIP:   clientIP,
		UserAgent:  userAgent,
	}

	r.auditLogs = append(r.auditLogs, entry)

	// In a real implementation, we would also write to a persistent store

	return nil
}

// GetAuditLogs gets all audit logs
func (r *RbacManager) GetAuditLogs(ctx context.Context) []AuditLogEntry {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	logs := make([]AuditLogEntry, len(r.auditLogs))
	copy(logs, r.auditLogs)
	return logs
}

// GetUserAuditLogs gets audit logs for a specific user
func (r *RbacManager) GetUserAuditLogs(ctx context.Context, userID string) []AuditLogEntry {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	logs := make([]AuditLogEntry, 0)
	for _, log := range r.auditLogs {
		if log.UserID == userID {
			logs = append(logs, log)
		}
	}
	return logs
}

// hashPassword hashes a password
func (r *RbacManager) hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash password: %w", err)
	}
	return string(hash), nil
}

// verifyPassword verifies a password against a hash
func (r *RbacManager) verifyPassword(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// generateToken generates a random token
func (r *RbacManager) generateToken() string {
	return uuid.New().String()
}
