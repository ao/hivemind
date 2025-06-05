use anyhow::{Result, Context};
use serde::{Deserialize, Serialize};
use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use tokio::sync::RwLock;
use std::time::{SystemTime, UNIX_EPOCH};
use uuid::Uuid;
use argon2::{self, Config};
use rand::Rng;

/// User represents a user in the system
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct User {
    pub id: String,
    pub username: String,
    #[serde(skip_serializing)]
    pub password_hash: String,
    pub email: String,
    pub full_name: String,
    pub roles: Vec<String>,
    pub groups: Vec<String>,
    pub created_at: i64,
    pub updated_at: i64,
    pub last_login: Option<i64>,
    pub active: bool,
}

/// Role represents a set of permissions
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Role {
    pub id: String,
    pub name: String,
    pub description: String,
    pub permissions: Vec<Permission>,
    pub created_at: i64,
    pub updated_at: i64,
}

/// Group represents a collection of users
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Group {
    pub id: String,
    pub name: String,
    pub description: String,
    pub members: Vec<String>, // User IDs
    pub roles: Vec<String>,   // Role IDs
    pub created_at: i64,
    pub updated_at: i64,
}

/// Permission represents an action that can be performed on a resource
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, Hash)]
pub struct Permission {
    pub resource: String,
    pub action: String,
    pub scope: PermissionScope,
}

/// Scope of a permission
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, Hash)]
pub enum PermissionScope {
    Global,
    Namespace(String),
    Resource(String),
}

/// Authentication request
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AuthRequest {
    pub username: String,
    pub password: String,
}

/// Authentication response
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AuthResponse {
    pub token: String,
    pub user_id: String,
    pub username: String,
    pub roles: Vec<String>,
    pub expires_at: i64,
}

/// Audit log entry
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AuditLogEntry {
    pub id: String,
    pub timestamp: i64,
    pub user_id: String,
    pub username: String,
    pub action: String,
    pub resource: String,
    pub resource_id: Option<String>,
    pub status: AuditStatus,
    pub details: HashMap<String, String>,
    pub client_ip: Option<String>,
    pub user_agent: Option<String>,
}

/// Audit log status
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum AuditStatus {
    Success,
    Failure,
    Denied,
}

/// RBAC Manager handles authentication, authorization, and audit logging
pub struct RbacManager {
    users: Arc<RwLock<HashMap<String, User>>>,
    roles: Arc<RwLock<HashMap<String, Role>>>,
    groups: Arc<RwLock<HashMap<String, Group>>>,
    active_tokens: Arc<RwLock<HashMap<String, String>>>, // token -> user_id
    audit_logs: Arc<RwLock<Vec<AuditLogEntry>>>,
}

impl RbacManager {
    pub fn new() -> Self {
        let rbac = Self {
            users: Arc::new(RwLock::new(HashMap::new())),
            roles: Arc::new(RwLock::new(HashMap::new())),
            groups: Arc::new(RwLock::new(HashMap::new())),
            active_tokens: Arc::new(RwLock::new(HashMap::new())),
            audit_logs: Arc::new(RwLock::new(Vec::new())),
        };
        
        // Initialize with default roles and admin user
        tokio::spawn(async move {
            if let Err(e) = rbac.initialize_defaults().await {
                eprintln!("Failed to initialize RBAC defaults: {}", e);
            }
        });
        
        rbac
    }
    
    /// Initialize default roles and admin user
    async fn initialize_defaults(&self) -> Result<()> {
        // Create default roles
        let admin_role = Role {
            id: "admin".to_string(),
            name: "Administrator".to_string(),
            description: "Full system access".to_string(),
            permissions: vec![
                Permission {
                    resource: "*".to_string(),
                    action: "*".to_string(),
                    scope: PermissionScope::Global,
                },
            ],
            created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        };
        
        let operator_role = Role {
            id: "operator".to_string(),
            name: "Operator".to_string(),
            description: "Can manage containers and services".to_string(),
            permissions: vec![
                Permission {
                    resource: "container".to_string(),
                    action: "*".to_string(),
                    scope: PermissionScope::Global,
                },
                Permission {
                    resource: "service".to_string(),
                    action: "*".to_string(),
                    scope: PermissionScope::Global,
                },
                Permission {
                    resource: "volume".to_string(),
                    action: "*".to_string(),
                    scope: PermissionScope::Global,
                },
            ],
            created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        };
        
        let viewer_role = Role {
            id: "viewer".to_string(),
            name: "Viewer".to_string(),
            description: "Read-only access".to_string(),
            permissions: vec![
                Permission {
                    resource: "*".to_string(),
                    action: "get".to_string(),
                    scope: PermissionScope::Global,
                },
                Permission {
                    resource: "*".to_string(),
                    action: "list".to_string(),
                    scope: PermissionScope::Global,
                },
            ],
            created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
        };
        
        // Add roles
        let mut roles = self.roles.write().await;
        roles.insert(admin_role.id.clone(), admin_role);
        roles.insert(operator_role.id.clone(), operator_role);
        roles.insert(viewer_role.id.clone(), viewer_role);
        
        // Create admin user with a default password
        // In a real system, this would be set during installation or from environment variables
        let admin_user = User {
            id: "admin".to_string(),
            username: "admin".to_string(),
            password_hash: self.hash_password("admin123")?,
            email: "admin@example.com".to_string(),
            full_name: "System Administrator".to_string(),
            roles: vec!["admin".to_string()],
            groups: Vec::new(),
            created_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            updated_at: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            last_login: None,
            active: true,
        };
        
        // Add admin user
        let mut users = self.users.write().await;
        users.insert(admin_user.id.clone(), admin_user);
        
        Ok(())
    }
    
    /// Create a new user
    pub async fn create_user(&self, user: User) -> Result<User> {
        let mut users = self.users.write().await;
        
        // Check if username already exists
        if users.values().any(|u| u.username == user.username) {
            anyhow::bail!("Username already exists");
        }
        
        let user_id = user.id.clone();
        users.insert(user_id.clone(), user.clone());
        
        // Log the action
        self.log_audit(
            "system",
            "admin",
            "create_user",
            "user",
            Some(&user_id),
            AuditStatus::Success,
            HashMap::new(),
            None,
            None,
        ).await?;
        
        Ok(user)
    }
    
    /// Get a user by ID
    pub async fn get_user(&self, user_id: &str) -> Option<User> {
        let users = self.users.read().await;
        users.get(user_id).cloned()
    }
    
    /// Get a user by username
    pub async fn get_user_by_username(&self, username: &str) -> Option<User> {
        let users = self.users.read().await;
        users.values().find(|u| u.username == username).cloned()
    }
    
    /// Update a user
    pub async fn update_user(&self, user_id: &str, updated_user: User) -> Result<User> {
        let mut users = self.users.write().await;
        
        if !users.contains_key(user_id) {
            anyhow::bail!("User not found");
        }
        
        // Check if username is being changed and if it conflicts
        let existing_user = users.get(user_id).unwrap();
        if existing_user.username != updated_user.username && 
           users.values().any(|u| u.username == updated_user.username) {
            anyhow::bail!("Username already exists");
        }
        
        users.insert(user_id.to_string(), updated_user.clone());
        
        Ok(updated_user)
    }
    
    /// Delete a user
    pub async fn delete_user(&self, user_id: &str) -> Result<()> {
        let mut users = self.users.write().await;
        
        if !users.contains_key(user_id) {
            anyhow::bail!("User not found");
        }
        
        users.remove(user_id);
        
        // Log the action
        self.log_audit(
            "system",
            "admin",
            "delete_user",
            "user",
            Some(user_id),
            AuditStatus::Success,
            HashMap::new(),
            None,
            None,
        ).await?;
        
        Ok(())
    }
    
    /// List all users
    pub async fn list_users(&self) -> Vec<User> {
        let users = self.users.read().await;
        users.values().cloned().collect()
    }
    
    /// Create a new role
    pub async fn create_role(&self, role: Role) -> Result<Role> {
        let mut roles = self.roles.write().await;
        
        // Check if role name already exists
        if roles.values().any(|r| r.name == role.name) {
            anyhow::bail!("Role name already exists");
        }
        
        let role_id = role.id.clone();
        roles.insert(role_id.clone(), role.clone());
        
        // Log the action
        self.log_audit(
            "system",
            "admin",
            "create_role",
            "role",
            Some(&role_id),
            AuditStatus::Success,
            HashMap::new(),
            None,
            None,
        ).await?;
        
        Ok(role)
    }
    
    /// Get a role by ID
    pub async fn get_role(&self, role_id: &str) -> Option<Role> {
        let roles = self.roles.read().await;
        roles.get(role_id).cloned()
    }
    
    /// Update a role
    pub async fn update_role(&self, role_id: &str, updated_role: Role) -> Result<Role> {
        let mut roles = self.roles.write().await;
        
        if !roles.contains_key(role_id) {
            anyhow::bail!("Role not found");
        }
        
        // Check if role name is being changed and if it conflicts
        let existing_role = roles.get(role_id).unwrap();
        if existing_role.name != updated_role.name && 
           roles.values().any(|r| r.name == updated_role.name) {
            anyhow::bail!("Role name already exists");
        }
        
        roles.insert(role_id.to_string(), updated_role.clone());
        
        Ok(updated_role)
    }
    
    /// Delete a role
    pub async fn delete_role(&self, role_id: &str) -> Result<()> {
        let mut roles = self.roles.write().await;
        
        if !roles.contains_key(role_id) {
            anyhow::bail!("Role not found");
        }
        
        roles.remove(role_id);
        
        // Log the action
        self.log_audit(
            "system",
            "admin",
            "delete_role",
            "role",
            Some(role_id),
            AuditStatus::Success,
            HashMap::new(),
            None,
            None,
        ).await?;
        
        Ok(())
    }
    
    /// List all roles
    pub async fn list_roles(&self) -> Vec<Role> {
        let roles = self.roles.read().await;
        roles.values().cloned().collect()
    }
    
    /// Create a new group
    pub async fn create_group(&self, group: Group) -> Result<Group> {
        let mut groups = self.groups.write().await;
        
        // Check if group name already exists
        if groups.values().any(|g| g.name == group.name) {
            anyhow::bail!("Group name already exists");
        }
        
        let group_id = group.id.clone();
        groups.insert(group_id.clone(), group.clone());
        
        // Log the action
        self.log_audit(
            "system",
            "admin",
            "create_group",
            "group",
            Some(&group_id),
            AuditStatus::Success,
            HashMap::new(),
            None,
            None,
        ).await?;
        
        Ok(group)
    }
    
    /// Get a group by ID
    pub async fn get_group(&self, group_id: &str) -> Option<Group> {
        let groups = self.groups.read().await;
        groups.get(group_id).cloned()
    }
    
    /// Update a group
    pub async fn update_group(&self, group_id: &str, updated_group: Group) -> Result<Group> {
        let mut groups = self.groups.write().await;
        
        if !groups.contains_key(group_id) {
            anyhow::bail!("Group not found");
        }
        
        // Check if group name is being changed and if it conflicts
        let existing_group = groups.get(group_id).unwrap();
        if existing_group.name != updated_group.name && 
           groups.values().any(|g| g.name == updated_group.name) {
            anyhow::bail!("Group name already exists");
        }
        
        groups.insert(group_id.to_string(), updated_group.clone());
        
        Ok(updated_group)
    }
    
    /// Delete a group
    pub async fn delete_group(&self, group_id: &str) -> Result<()> {
        let mut groups = self.groups.write().await;
        
        if !groups.contains_key(group_id) {
            anyhow::bail!("Group not found");
        }
        
        groups.remove(group_id);
        
        // Log the action
        self.log_audit(
            "system",
            "admin",
            "delete_group",
            "group",
            Some(group_id),
            AuditStatus::Success,
            HashMap::new(),
            None,
            None,
        ).await?;
        
        Ok(())
    }
    
    /// List all groups
    pub async fn list_groups(&self) -> Vec<Group> {
        let groups = self.groups.read().await;
        groups.values().cloned().collect()
    }
    
    /// Add a user to a group
    pub async fn add_user_to_group(&self, user_id: &str, group_id: &str) -> Result<()> {
        let mut groups = self.groups.write().await;
        
        let group = groups.get_mut(group_id).context("Group not found")?;
        
        if !group.members.contains(&user_id.to_string()) {
            group.members.push(user_id.to_string());
            group.updated_at = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;
        }
        
        Ok(())
    }
    
    /// Remove a user from a group
    pub async fn remove_user_from_group(&self, user_id: &str, group_id: &str) -> Result<()> {
        let mut groups = self.groups.write().await;
        
        let group = groups.get_mut(group_id).context("Group not found")?;
        
        group.members.retain(|id| id != user_id);
        group.updated_at = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;
        
        Ok(())
    }
    
    /// Authenticate a user
    pub async fn authenticate(&self, auth_request: &AuthRequest) -> Result<AuthResponse> {
        // Find user by username
        let user = self.get_user_by_username(&auth_request.username).await
            .context("Invalid username or password")?;
        
        // Check if user is active
        if !user.active {
            anyhow::bail!("User account is inactive");
        }
        
        // Verify password
        if !self.verify_password(&auth_request.password, &user.password_hash)? {
            // Log failed login attempt
            self.log_audit(
                &user.id,
                &user.username,
                "login",
                "auth",
                None,
                AuditStatus::Failure,
                HashMap::new(),
                None,
                None,
            ).await?;
            
            anyhow::bail!("Invalid username or password");
        }
        
        // Generate token
        let token = self.generate_token();
        let expires_at = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64 + 3600; // 1 hour
        
        // Store token
        let mut active_tokens = self.active_tokens.write().await;
        active_tokens.insert(token.clone(), user.id.clone());
        
        // Update last login time
        let mut users = self.users.write().await;
        if let Some(user_mut) = users.get_mut(&user.id) {
            user_mut.last_login = Some(SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64);
        }
        
        // Log successful login
        self.log_audit(
            &user.id,
            &user.username,
            "login",
            "auth",
            None,
            AuditStatus::Success,
            HashMap::new(),
            None,
            None,
        ).await?;
        
        Ok(AuthResponse {
            token,
            user_id: user.id,
            username: user.username,
            roles: user.roles,
            expires_at,
        })
    }
    
    /// Validate a token and get the associated user
    pub async fn validate_token(&self, token: &str) -> Option<User> {
        let active_tokens = self.active_tokens.read().await;
        
        if let Some(user_id) = active_tokens.get(token) {
            let users = self.users.read().await;
            return users.get(user_id).cloned();
        }
        
        None
    }
    
    /// Logout (invalidate token)
    pub async fn logout(&self, token: &str) -> Result<()> {
        let mut active_tokens = self.active_tokens.write().await;
        active_tokens.remove(token);
        Ok(())
    }
    
    /// Check if a user has permission to perform an action on a resource
    pub async fn check_permission(
        &self,
        user_id: &str,
        resource: &str,
        action: &str,
        scope: &PermissionScope,
    ) -> Result<bool> {
        // Get user
        let user = match self.get_user(user_id).await {
            Some(u) => u,
            None => return Ok(false),
        };
        
        // If user is inactive, deny permission
        if !user.active {
            return Ok(false);
        }
        
        // Get all roles for the user (directly assigned and via groups)
        let mut role_ids = user.roles.clone();
        
        // Add roles from groups
        let groups = self.groups.read().await;
        for group in groups.values() {
            if group.members.contains(&user_id.to_string()) {
                role_ids.extend(group.roles.clone());
            }
        }
        
        // Remove duplicates
        role_ids.sort();
        role_ids.dedup();
        
        // Get all roles
        let roles = self.roles.read().await;
        
        // Check if any role grants the permission
        for role_id in role_ids {
            if let Some(role) = roles.get(&role_id) {
                for permission in &role.permissions {
                    if self.permission_matches(permission, resource, action, scope) {
                        return Ok(true);
                    }
                }
            }
        }
        
        Ok(false)
    }
    
    /// Check if a permission matches the requested resource, action, and scope
    fn permission_matches(
        &self,
        permission: &Permission,
        resource: &str,
        action: &str,
        scope: &PermissionScope,
    ) -> bool {
        // Check resource
        if permission.resource != "*" && permission.resource != resource {
            return false;
        }
        
        // Check action
        if permission.action != "*" && permission.action != action {
            return false;
        }
        
        // Check scope
        match (&permission.scope, scope) {
            // Global permission applies to all scopes
            (PermissionScope::Global, _) => true,
            
            // Namespace permission applies to the same namespace or resources in that namespace
            (PermissionScope::Namespace(perm_ns), PermissionScope::Namespace(req_ns)) => {
                perm_ns == req_ns
            }
            (PermissionScope::Namespace(perm_ns), PermissionScope::Resource(req_res)) => {
                req_res.starts_with(&format!("{}/", perm_ns))
            }
            
            // Resource permission only applies to the exact resource
            (PermissionScope::Resource(perm_res), PermissionScope::Resource(req_res)) => {
                perm_res == req_res
            }
            
            // Other combinations don't match
            _ => false,
        }
    }
    
    /// Log an audit event
    pub async fn log_audit(
        &self,
        user_id: &str,
        username: &str,
        action: &str,
        resource: &str,
        resource_id: Option<&str>,
        status: AuditStatus,
        details: HashMap<String, String>,
        client_ip: Option<&str>,
        user_agent: Option<&str>,
    ) -> Result<()> {
        let entry = AuditLogEntry {
            id: Uuid::new_v4().to_string(),
            timestamp: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64,
            user_id: user_id.to_string(),
            username: username.to_string(),
            action: action.to_string(),
            resource: resource.to_string(),
            resource_id: resource_id.map(|s| s.to_string()),
            status,
            details,
            client_ip: client_ip.map(|s| s.to_string()),
            user_agent: user_agent.map(|s| s.to_string()),
        };
        
        let mut logs = self.audit_logs.write().await;
        logs.push(entry);
        
        // In a real implementation, we would also write to a persistent store
        
        Ok(())
    }
    
    /// Get audit logs
    pub async fn get_audit_logs(&self) -> Vec<AuditLogEntry> {
        let logs = self.audit_logs.read().await;
        logs.clone()
    }
    
    /// Get audit logs for a specific user
    pub async fn get_user_audit_logs(&self, user_id: &str) -> Vec<AuditLogEntry> {
        let logs = self.audit_logs.read().await;
        logs.iter()
            .filter(|log| log.user_id == user_id)
            .cloned()
            .collect()
    }
    
    /// Hash a password
    fn hash_password(&self, password: &str) -> Result<String> {
        // Generate a random salt
        let mut salt = [0u8; 32];
        for i in 0..salt.len() {
            salt[i] = rand::random::<u8>();
        }
        
        let config = Config::default();
        let hash = argon2::hash_encoded(password.as_bytes(), &salt, &config)?;
        Ok(hash)
    }
    
    /// Verify a password against a hash
    fn verify_password(&self, password: &str, hash: &str) -> Result<bool> {
        let result = argon2::verify_encoded(hash, password.as_bytes())?;
        Ok(result)
    }
    
    /// Generate a random token
    fn generate_token(&self) -> String {
        Uuid::new_v4().to_string()
    }
}