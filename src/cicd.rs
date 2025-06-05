use anyhow::{Context, Result};
use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;
use tokio::sync::RwLock;

use crate::app::AppManager;
use crate::security::SecurityManager;

/// Represents a CI/CD pipeline configuration
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PipelineConfig {
    /// Unique identifier for the pipeline
    pub id: String,
    /// Name of the pipeline
    pub name: String,
    /// Source repository URL
    pub repository_url: String,
    /// Branch to monitor
    pub branch: String,
    /// Build configuration
    pub build_config: BuildConfig,
    /// Deployment configuration
    pub deployment_config: DeploymentConfig,
    /// Testing configuration
    pub test_config: Option<TestConfig>,
    /// Release configuration
    pub release_config: Option<ReleaseConfig>,
    /// Webhook URL for notifications
    pub webhook_url: Option<String>,
    /// Environment variables for the pipeline
    pub environment: HashMap<String, String>,
    /// Created timestamp
    pub created_at: chrono::DateTime<chrono::Utc>,
    /// Last updated timestamp
    pub updated_at: chrono::DateTime<chrono::Utc>,
}

/// Build configuration for a CI/CD pipeline
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BuildConfig {
    /// Build command to execute
    pub build_command: String,
    /// Docker image to use for building
    pub docker_image: Option<String>,
    /// Path to Dockerfile if building a custom image
    pub dockerfile_path: Option<String>,
    /// Build arguments
    pub build_args: HashMap<String, String>,
    /// Cache configuration
    pub cache_config: Option<CacheConfig>,
}

/// Cache configuration for builds
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CacheConfig {
    /// Paths to cache
    pub paths: Vec<String>,
    /// Cache key
    pub key: String,
    /// Cache restore keys
    pub restore_keys: Vec<String>,
}

/// Deployment configuration for a CI/CD pipeline
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DeploymentConfig {
    /// Deployment strategy
    pub strategy: DeploymentStrategy,
    /// Target environment
    pub environment: String,
    /// Replicas to deploy
    pub replicas: u32,
    /// Resources to allocate
    pub resources: Option<ResourceConfig>,
    /// Health check configuration
    pub health_check: Option<HealthCheckConfig>,
    /// Rollback configuration
    pub rollback: Option<RollbackConfig>,
}

/// Deployment strategy for a CI/CD pipeline
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum DeploymentStrategy {
    /// Rolling update deployment
    RollingUpdate {
        /// Maximum number of unavailable replicas
        max_unavailable: u32,
        /// Maximum number of surge replicas
        max_surge: u32,
    },
    /// Blue-green deployment
    BlueGreen {
        /// Verification timeout in seconds
        verification_timeout: u64,
    },
    /// Canary deployment
    Canary {
        /// Percentage of traffic to route to canary
        percentage: u32,
        /// Steps for incremental rollout
        steps: Vec<u32>,
        /// Interval between steps in seconds
        interval: u64,
    },
}

/// Resource configuration for a deployment
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ResourceConfig {
    /// CPU request
    pub cpu_request: String,
    /// Memory request
    pub memory_request: String,
    /// CPU limit
    pub cpu_limit: Option<String>,
    /// Memory limit
    pub memory_limit: Option<String>,
}

/// Health check configuration for a deployment
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HealthCheckConfig {
    /// Path for HTTP health check
    pub path: String,
    /// Port for health check
    pub port: u16,
    /// Initial delay in seconds
    pub initial_delay_seconds: u32,
    /// Period in seconds
    pub period_seconds: u32,
    /// Timeout in seconds
    pub timeout_seconds: u32,
    /// Success threshold
    pub success_threshold: u32,
    /// Failure threshold
    pub failure_threshold: u32,
}

/// Rollback configuration for a deployment
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RollbackConfig {
    /// Whether to enable automatic rollback
    pub enabled: bool,
    /// Maximum number of revisions to keep
    pub revision_history_limit: u32,
}

/// Testing configuration for a CI/CD pipeline
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TestConfig {
    /// Test command to execute
    pub test_command: String,
    /// Test environment
    pub environment: HashMap<String, String>,
    /// Test reports path
    pub reports_path: Option<String>,
    /// Test timeout in seconds
    pub timeout_seconds: u32,
    /// Whether to fail the pipeline on test failure
    pub fail_on_test_failure: bool,
}

/// Release configuration for a CI/CD pipeline
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ReleaseConfig {
    /// Release type
    pub release_type: ReleaseType,
    /// Release tags
    pub tags: Vec<String>,
    /// Release notes template
    pub release_notes_template: Option<String>,
    /// Whether to create GitHub release
    pub create_github_release: bool,
    /// Whether to publish to container registry
    pub publish_to_registry: bool,
    /// Container registry URL
    pub registry_url: Option<String>,
    /// Container registry credentials
    pub registry_credentials: Option<RegistryCredentials>,
}

/// Release type for a CI/CD pipeline
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum ReleaseType {
    /// Semantic versioning
    Semver {
        /// Whether to auto-increment major version
        auto_increment_major: bool,
        /// Whether to auto-increment minor version
        auto_increment_minor: bool,
        /// Whether to auto-increment patch version
        auto_increment_patch: bool,
    },
    /// Date-based versioning
    DateBased {
        /// Format for date-based version
        format: String,
    },
    /// Custom versioning
    Custom {
        /// Custom version template
        template: String,
    },
}

/// Registry credentials for a CI/CD pipeline
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RegistryCredentials {
    /// Registry username
    pub username: String,
    /// Registry password (encrypted)
    pub password_encrypted: String,
}

/// Status of a pipeline run
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum PipelineStatus {
    /// Pipeline is pending
    Pending,
    /// Pipeline is running
    Running,
    /// Pipeline succeeded
    Success,
    /// Pipeline failed
    Failed {
        /// Error message
        error: String,
        /// Stage that failed
        stage: String,
    },
    /// Pipeline was cancelled
    Cancelled,
}

/// Represents a CI/CD pipeline run
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PipelineRun {
    /// Unique identifier for the run
    pub id: String,
    /// Pipeline ID
    pub pipeline_id: String,
    /// Commit hash
    pub commit_hash: String,
    /// Commit message
    pub commit_message: String,
    /// Author of the commit
    pub author: String,
    /// Status of the run
    pub status: PipelineStatus,
    /// Start time
    pub start_time: chrono::DateTime<chrono::Utc>,
    /// End time
    pub end_time: Option<chrono::DateTime<chrono::Utc>>,
    /// Duration in seconds
    pub duration_seconds: Option<u64>,
    /// Stages of the run
    pub stages: Vec<PipelineStage>,
    /// Artifacts produced by the run
    pub artifacts: Vec<PipelineArtifact>,
}

/// Represents a stage in a pipeline run
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PipelineStage {
    /// Name of the stage
    pub name: String,
    /// Status of the stage
    pub status: PipelineStatus,
    /// Start time
    pub start_time: chrono::DateTime<chrono::Utc>,
    /// End time
    pub end_time: Option<chrono::DateTime<chrono::Utc>>,
    /// Duration in seconds
    pub duration_seconds: Option<u64>,
    /// Steps in the stage
    pub steps: Vec<PipelineStep>,
}

/// Represents a step in a pipeline stage
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PipelineStep {
    /// Name of the step
    pub name: String,
    /// Status of the step
    pub status: PipelineStatus,
    /// Start time
    pub start_time: chrono::DateTime<chrono::Utc>,
    /// End time
    pub end_time: Option<chrono::DateTime<chrono::Utc>>,
    /// Duration in seconds
    pub duration_seconds: Option<u64>,
    /// Log output
    pub log_output: Option<String>,
}

/// Represents an artifact produced by a pipeline run
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PipelineArtifact {
    /// Name of the artifact
    pub name: String,
    /// Type of the artifact
    pub artifact_type: String,
    /// Path to the artifact
    pub path: String,
    /// Size of the artifact in bytes
    pub size_bytes: u64,
    /// Checksum of the artifact
    pub checksum: Option<String>,
}

/// Trait for CI/CD providers
#[async_trait]
pub trait CicdProvider: Send + Sync {
    /// Get the name of the provider
    fn name(&self) -> &str;
    
    /// Get the description of the provider
    fn description(&self) -> &str;
    
    /// Initialize the provider
    async fn initialize(&self) -> Result<()>;
    
    /// Create a new pipeline
    async fn create_pipeline(&self, config: PipelineConfig) -> Result<String>;
    
    /// Update an existing pipeline
    async fn update_pipeline(&self, id: &str, config: PipelineConfig) -> Result<()>;
    
    /// Delete a pipeline
    async fn delete_pipeline(&self, id: &str) -> Result<()>;
    
    /// Get a pipeline by ID
    async fn get_pipeline(&self, id: &str) -> Result<PipelineConfig>;
    
    /// List all pipelines
    async fn list_pipelines(&self) -> Result<Vec<PipelineConfig>>;
    
    /// Trigger a pipeline run
    async fn trigger_pipeline(&self, id: &str) -> Result<String>;
    
    /// Get a pipeline run by ID
    async fn get_pipeline_run(&self, id: &str) -> Result<PipelineRun>;
    
    /// List runs for a pipeline
    async fn list_pipeline_runs(&self, pipeline_id: &str) -> Result<Vec<PipelineRun>>;
    
    /// Cancel a pipeline run
    async fn cancel_pipeline_run(&self, id: &str) -> Result<()>;
    
    /// Get logs for a pipeline run
    async fn get_pipeline_run_logs(&self, id: &str) -> Result<String>;
}

/// GitHub Actions CI/CD provider
pub struct GitHubActionsProvider {
    /// Base URL for GitHub API
    base_url: String,
    /// GitHub token
    token: String,
    /// GitHub organization or user
    owner: String,
    /// GitHub repository
    repo: String,
    /// Security manager for handling secrets
    security_manager: Arc<SecurityManager>,
}

impl GitHubActionsProvider {
    /// Create a new GitHub Actions provider
    pub fn new(
        token: String,
        owner: String,
        repo: String,
        security_manager: Arc<SecurityManager>,
    ) -> Self {
        Self {
            base_url: "https://api.github.com".to_string(),
            token,
            owner,
            repo,
            security_manager,
        }
    }
    
    /// Generate a GitHub Actions workflow file
    pub fn generate_workflow_file(&self, config: &PipelineConfig) -> Result<String> {
        let mut workflow = String::new();
        
        // Add workflow header
        workflow.push_str(&format!(
            "name: {}\n\n",
            config.name
        ));
        
        // Add triggers
        workflow.push_str("on:\n");
        workflow.push_str("  push:\n");
        workflow.push_str(&format!("    branches: [ {} ]\n", config.branch));
        workflow.push_str("  pull_request:\n");
        workflow.push_str(&format!("    branches: [ {} ]\n", config.branch));
        workflow.push_str("  workflow_dispatch:\n\n");
        
        // Add jobs
        workflow.push_str("jobs:\n");
        
        // Build job
        workflow.push_str("  build:\n");
        workflow.push_str("    runs-on: ubuntu-latest\n");
        workflow.push_str("    steps:\n");
        workflow.push_str("    - uses: actions/checkout@v3\n");
        
        // Add cache if configured
        if let Some(cache_config) = &config.build_config.cache_config {
            workflow.push_str("    - name: Cache dependencies\n");
            workflow.push_str("      uses: actions/cache@v3\n");
            workflow.push_str("      with:\n");
            workflow.push_str(&format!("        path: {}\n", cache_config.paths.join("\n          ")));
            workflow.push_str(&format!("        key: {}\n", cache_config.key));
            if !cache_config.restore_keys.is_empty() {
                workflow.push_str("        restore-keys: |\n");
                for key in &cache_config.restore_keys {
                    workflow.push_str(&format!("          {}\n", key));
                }
            }
        }
        
        // Add build step
        workflow.push_str("    - name: Build\n");
        if let Some(docker_image) = &config.build_config.docker_image {
            workflow.push_str(&format!("      uses: docker://{}\n", docker_image));
        } else {
            workflow.push_str("      run: |\n");
            workflow.push_str(&format!("        {}\n", config.build_config.build_command));
        }
        
        // Add environment variables
        if !config.environment.is_empty() {
            workflow.push_str("      env:\n");
            for (key, value) in &config.environment {
                workflow.push_str(&format!("        {}: {}\n", key, value));
            }
        }
        
        // Add test job if configured
        if let Some(test_config) = &config.test_config {
            workflow.push_str("\n  test:\n");
            workflow.push_str("    needs: build\n");
            workflow.push_str("    runs-on: ubuntu-latest\n");
            workflow.push_str("    steps:\n");
            workflow.push_str("    - uses: actions/checkout@v3\n");
            
            // Add test step
            workflow.push_str("    - name: Test\n");
            workflow.push_str("      run: |\n");
            workflow.push_str(&format!("        {}\n", test_config.test_command));
            
            // Add environment variables
            if !test_config.environment.is_empty() {
                workflow.push_str("      env:\n");
                for (key, value) in &test_config.environment {
                    workflow.push_str(&format!("        {}: {}\n", key, value));
                }
            }
            
            // Add test report if configured
            if let Some(reports_path) = &test_config.reports_path {
                workflow.push_str("    - name: Upload test results\n");
                workflow.push_str("      uses: actions/upload-artifact@v3\n");
                workflow.push_str("      with:\n");
                workflow.push_str("        name: test-results\n");
                workflow.push_str(&format!("        path: {}\n", reports_path));
                workflow.push_str(&format!("      if: always() && !cancelled()\n"));
            }
        }
        
        // Add deploy job
        workflow.push_str("\n  deploy:\n");
        if config.test_config.is_some() {
            workflow.push_str("    needs: test\n");
        } else {
            workflow.push_str("    needs: build\n");
        }
        workflow.push_str("    runs-on: ubuntu-latest\n");
        workflow.push_str("    steps:\n");
        workflow.push_str("    - uses: actions/checkout@v3\n");
        
        // Add deployment step based on strategy
        workflow.push_str("    - name: Deploy\n");
        workflow.push_str("      run: |\n");
        match &config.deployment_config.strategy {
            DeploymentStrategy::RollingUpdate { .. } => {
                workflow.push_str("        echo \"Performing rolling update deployment\"\n");
                workflow.push_str("        # Add your rolling update deployment commands here\n");
            }
            DeploymentStrategy::BlueGreen { .. } => {
                workflow.push_str("        echo \"Performing blue-green deployment\"\n");
                workflow.push_str("        # Add your blue-green deployment commands here\n");
            }
            DeploymentStrategy::Canary { .. } => {
                workflow.push_str("        echo \"Performing canary deployment\"\n");
                workflow.push_str("        # Add your canary deployment commands here\n");
            }
        }
        
        // Add release job if configured
        if let Some(release_config) = &config.release_config {
            workflow.push_str("\n  release:\n");
            workflow.push_str("    needs: deploy\n");
            workflow.push_str("    runs-on: ubuntu-latest\n");
            workflow.push_str("    steps:\n");
            workflow.push_str("    - uses: actions/checkout@v3\n");
            
            // Add release step
            workflow.push_str("    - name: Release\n");
            
            if release_config.create_github_release {
                workflow.push_str("    - name: Create GitHub Release\n");
                workflow.push_str("      id: create_release\n");
                workflow.push_str("      uses: actions/create-release@v1\n");
                workflow.push_str("      env:\n");
                workflow.push_str("        GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}\n");
                workflow.push_str("      with:\n");
                workflow.push_str("        tag_name: ${{ github.ref }}\n");
                workflow.push_str("        release_name: Release ${{ github.ref }}\n");
                workflow.push_str("        draft: false\n");
                workflow.push_str("        prerelease: false\n");
            }
            
            if release_config.publish_to_registry {
                workflow.push_str("    - name: Login to Container Registry\n");
                workflow.push_str("      uses: docker/login-action@v2\n");
                workflow.push_str("      with:\n");
                if let Some(registry_url) = &release_config.registry_url {
                    workflow.push_str(&format!("        registry: {}\n", registry_url));
                }
                workflow.push_str("        username: ${{ secrets.REGISTRY_USERNAME }}\n");
                workflow.push_str("        password: ${{ secrets.REGISTRY_PASSWORD }}\n");
                
                workflow.push_str("    - name: Build and push Docker image\n");
                workflow.push_str("      uses: docker/build-push-action@v4\n");
                workflow.push_str("      with:\n");
                if let Some(dockerfile_path) = &config.build_config.dockerfile_path {
                    workflow.push_str(&format!("        file: {}\n", dockerfile_path));
                }
                workflow.push_str("        push: true\n");
                workflow.push_str("        tags: ${{ steps.meta.outputs.tags }}\n");
            }
        }
        
        Ok(workflow)
    }
}

#[async_trait]
impl CicdProvider for GitHubActionsProvider {
    fn name(&self) -> &str {
        "GitHub Actions"
    }
    
    fn description(&self) -> &str {
        "GitHub Actions CI/CD provider"
    }
    
    async fn initialize(&self) -> Result<()> {
        // Validate GitHub token
        let client = reqwest::Client::new();
        let response = client
            .get(&format!("{}/user", self.base_url))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .send()
            .await
            .context("Failed to validate GitHub token")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Invalid GitHub token"));
        }
        
        Ok(())
    }
    
    async fn create_pipeline(&self, config: PipelineConfig) -> Result<String> {
        // Generate workflow file
        let workflow_content = self.generate_workflow_file(&config)?;
        
        // Create workflow file in GitHub repository
        let client = reqwest::Client::new();
        let workflow_path = format!(".github/workflows/{}.yml", config.name.replace(" ", "_").to_lowercase());
        
        // Get the current commit SHA to use as the parent
        let response = client
            .get(&format!(
                "{}/repos/{}/{}/git/refs/heads/{}",
                self.base_url, self.owner, self.repo, config.branch
            ))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .send()
            .await
            .context("Failed to get branch reference")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Failed to get branch reference: {}", response.status()));
        }
        
        let branch_ref: serde_json::Value = response.json().await?;
        let parent_sha = branch_ref["object"]["sha"]
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("Invalid branch reference response"))?;
        
        // Create blob for workflow file
        let blob_data = serde_json::json!({
            "content": workflow_content,
            "encoding": "utf-8"
        });
        
        let response = client
            .post(&format!(
                "{}/repos/{}/{}/git/blobs",
                self.base_url, self.owner, self.repo
            ))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .json(&blob_data)
            .send()
            .await
            .context("Failed to create blob")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Failed to create blob: {}", response.status()));
        }
        
        let blob: serde_json::Value = response.json().await?;
        let blob_sha = blob["sha"]
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("Invalid blob response"))?;
        
        // Create tree with the new file
        let tree_data = serde_json::json!({
            "base_tree": parent_sha,
            "tree": [
                {
                    "path": workflow_path,
                    "mode": "100644",
                    "type": "blob",
                    "sha": blob_sha
                }
            ]
        });
        
        let response = client
            .post(&format!(
                "{}/repos/{}/{}/git/trees",
                self.base_url, self.owner, self.repo
            ))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .json(&tree_data)
            .send()
            .await
            .context("Failed to create tree")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Failed to create tree: {}", response.status()));
        }
        
        let tree: serde_json::Value = response.json().await?;
        let tree_sha = tree["sha"]
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("Invalid tree response"))?;
        
        // Create commit
        let commit_data = serde_json::json!({
            "message": format!("Add CI/CD pipeline: {}", config.name),
            "parents": [parent_sha],
            "tree": tree_sha
        });
        
        let response = client
            .post(&format!(
                "{}/repos/{}/{}/git/commits",
                self.base_url, self.owner, self.repo
            ))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .json(&commit_data)
            .send()
            .await
            .context("Failed to create commit")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Failed to create commit: {}", response.status()));
        }
        
        let commit: serde_json::Value = response.json().await?;
        let commit_sha = commit["sha"]
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("Invalid commit response"))?;
        
        // Update branch reference
        let ref_data = serde_json::json!({
            "sha": commit_sha,
            "force": false
        });
        
        let response = client
            .patch(&format!(
                "{}/repos/{}/{}/git/refs/heads/{}",
                self.base_url, self.owner, self.repo, config.branch
            ))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .json(&ref_data)
            .send()
            .await
            .context("Failed to update branch reference")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Failed to update branch reference: {}", response.status()));
        }
        
        Ok(config.id)
    }
    
    async fn update_pipeline(&self, id: &str, config: PipelineConfig) -> Result<()> {
        // Generate workflow file
        let workflow_content = self.generate_workflow_file(&config)?;
        
        // Update workflow file in GitHub repository
        let client = reqwest::Client::new();
        let workflow_path = format!(".github/workflows/{}.yml", config.name.replace(" ", "_").to_lowercase());
        
        // Get the current file SHA
        let response = client
            .get(&format!(
                "{}/repos/{}/{}/contents/{}",
                self.base_url, self.owner, self.repo, workflow_path
            ))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .send()
            .await
            .context("Failed to get workflow file")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Failed to get workflow file: {}", response.status()));
        }
        
        let file_info: serde_json::Value = response.json().await?;
        let file_sha = file_info["sha"]
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("Invalid file info response"))?;
        
        // Update the file
        let update_data = serde_json::json!({
            "message": format!("Update CI/CD pipeline: {}", config.name),
            "content": base64::encode(workflow_content),
            "sha": file_sha
        });
        
        let response = client
            .put(&format!(
                "{}/repos/{}/{}/contents/{}",
                self.base_url, self.owner, self.repo, workflow_path
            ))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .json(&update_data)
            .send()
            .await
            .context("Failed to update workflow file")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Failed to update workflow file: {}", response.status()));
        }
        
        Ok(())
    }
    
    async fn delete_pipeline(&self, id: &str) -> Result<()> {
        // Get the pipeline config
        let pipeline = self.get_pipeline(id).await?;
        let workflow_path = format!(".github/workflows/{}.yml", pipeline.name.replace(" ", "_").to_lowercase());
        
        // Get the current file SHA
        let client = reqwest::Client::new();
        let response = client
            .get(&format!(
                "{}/repos/{}/{}/contents/{}",
                self.base_url, self.owner, self.repo, workflow_path
            ))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .send()
            .await
            .context("Failed to get workflow file")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Failed to get workflow file: {}", response.status()));
        }
        
        let file_info: serde_json::Value = response.json().await?;
        let file_sha = file_info["sha"]
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("Invalid file info response"))?;
        
        // Delete the file
        let delete_data = serde_json::json!({
            "message": format!("Delete CI/CD pipeline: {}", pipeline.name),
            "sha": file_sha
        });
        
        let response = client
            .delete(&format!(
                "{}/repos/{}/{}/contents/{}",
                self.base_url, self.owner, self.repo, workflow_path
            ))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .json(&delete_data)
            .send()
            .await
            .context("Failed to delete workflow file")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Failed to delete workflow file: {}", response.status()));
        }
        
        Ok(())
    }
    
    async fn get_pipeline(&self, id: &str) -> Result<PipelineConfig> {
        // In a real implementation, we would store pipeline configs in a database
        // For now, we'll just return a dummy pipeline config
        Err(anyhow::anyhow!("Pipeline not found"))
    }
    
    async fn list_pipelines(&self) -> Result<Vec<PipelineConfig>> {
        // List workflow files in GitHub repository
        let client = reqwest::Client::new();
        let response = client
            .get(&format!(
                "{}/repos/{}/{}/contents/.github/workflows",
                self.base_url, self.owner, self.repo
            ))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .send()
            .await
            .context("Failed to list workflow files")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Failed to list workflow files: {}", response.status()));
        }
        
        let files: Vec<serde_json::Value> = response.json().await?;
        let mut pipelines = Vec::new();
        
        for file in files {
            let name = file["name"]
                .as_str()
                .ok_or_else(|| anyhow::anyhow!("Invalid file name"))?;
            
            if name.ends_with(".yml") || name.ends_with(".yaml") {
                // This is a workflow file, create a dummy pipeline config
                let pipeline_name = name
                    .trim_end_matches(".yml")
                    .trim_end_matches(".yaml")
                    .replace("_", " ");
                
                let pipeline = PipelineConfig {
                    id: uuid::Uuid::new_v4().to_string(),
                    name: pipeline_name,
                    repository_url: format!("https://github.com/{}/{}", self.owner, self.repo),
                    branch: "main".to_string(),
                    build_config: BuildConfig {
                        build_command: "".to_string(),
                        docker_image: None,
                        dockerfile_path: None,
                        build_args: HashMap::new(),
                        cache_config: None,
                    },
                    deployment_config: DeploymentConfig {
                        strategy: DeploymentStrategy::RollingUpdate {
                            max_unavailable: 1,
                            max_surge: 1,
                        },
                        environment: "production".to_string(),
                        replicas: 1,
                        resources: None,
                        health_check: None,
                        rollback: None,
                    },
                    test_config: None,
                    release_config: None,
                    webhook_url: None,
                    environment: HashMap::new(),
                    created_at: chrono::Utc::now(),
                    updated_at: chrono::Utc::now(),
                };
                
                pipelines.push(pipeline);
            }
        }
        
        Ok(pipelines)
    }
    
    async fn trigger_pipeline(&self, id: &str) -> Result<String> {
        // Get the pipeline config
        let pipeline = self.get_pipeline(id).await?;
        
        // Trigger workflow dispatch event
        let client = reqwest::Client::new();
        let dispatch_data = serde_json::json!({
            "ref": pipeline.branch,
            "inputs": {}
        });
        
        let workflow_path = format!(".github/workflows/{}.yml", pipeline.name.replace(" ", "_").to_lowercase());
        
        let response = client
            .post(&format!(
                "{}/repos/{}/{}/actions/workflows/{}/dispatches",
                self.base_url, self.owner, self.repo, workflow_path
            ))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .json(&dispatch_data)
            .send()
            .await
            .context("Failed to trigger workflow")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Failed to trigger workflow: {}", response.status()));
        }
        
        // Return a dummy run ID
        Ok(uuid::Uuid::new_v4().to_string())
    }
    
    async fn get_pipeline_run(&self, id: &str) -> Result<PipelineRun> {
        // In a real implementation, we would fetch the run from GitHub API
        // For now, we'll just return a dummy run
        Err(anyhow::anyhow!("Pipeline run not found"))
    }
    
    async fn list_pipeline_runs(&self, pipeline_id: &str) -> Result<Vec<PipelineRun>> {
        // Get the pipeline config
        let pipeline = self.get_pipeline(pipeline_id).await?;
        
        // List workflow runs in GitHub repository
        let client = reqwest::Client::new();
        let workflow_path = format!(".github/workflows/{}.yml", pipeline.name.replace(" ", "_").to_lowercase());
        
        let response = client
            .get(&format!(
                "{}/repos/{}/{}/actions/workflows/{}/runs",
                self.base_url, self.owner, self.repo, workflow_path
            ))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .send()
            .await
            .context("Failed to list workflow runs")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Failed to list workflow runs: {}", response.status()));
        }
        
        let runs_data: serde_json::Value = response.json().await?;
        let runs = runs_data["workflow_runs"]
            .as_array()
            .ok_or_else(|| anyhow::anyhow!("Invalid workflow runs response"))?;
        
        let mut pipeline_runs = Vec::new();
        
        for run in runs {
            let id = run["id"]
                .as_u64()
                .ok_or_else(|| anyhow::anyhow!("Invalid run ID"))?
                .to_string();
            
            let status_str = run["status"]
                .as_str()
                .ok_or_else(|| anyhow::anyhow!("Invalid run status"))?;
            
            let conclusion = run["conclusion"].as_str();
            
            let status = match (status_str, conclusion) {
                ("queued", _) => PipelineStatus::Pending,
                ("in_progress", _) => PipelineStatus::Running,
                ("completed", Some("success")) => PipelineStatus::Success,
                ("completed", Some("failure")) => PipelineStatus::Failed {
                    error: "Workflow failed".to_string(),
                    stage: "unknown".to_string(),
                },
                ("completed", Some("cancelled")) => PipelineStatus::Cancelled,
                _ => PipelineStatus::Pending,
            };
            
            let start_time_str = run["created_at"]
                .as_str()
                .ok_or_else(|| anyhow::anyhow!("Invalid run start time"))?;
            
            let start_time = chrono::DateTime::parse_from_rfc3339(start_time_str)
                .map(|dt| dt.with_timezone(&chrono::Utc))
                .context("Failed to parse start time")?;
            
            let end_time = if status_str == "completed" {
                let end_time_str = run["updated_at"]
                    .as_str()
                    .ok_or_else(|| anyhow::anyhow!("Invalid run end time"))?;
                
                Some(
                    chrono::DateTime::parse_from_rfc3339(end_time_str)
                        .map(|dt| dt.with_timezone(&chrono::Utc))
                        .context("Failed to parse end time")?,
                )
            } else {
                None
            };
            
            let duration_seconds = if let Some(end_time) = end_time {
                Some((end_time - start_time).num_seconds() as u64)
            } else {
                None
            };
            
            let commit_hash = run["head_sha"]
                .as_str()
                .ok_or_else(|| anyhow::anyhow!("Invalid commit hash"))?
                .to_string();
            
            let commit_message = run["head_commit"]["message"]
                .as_str()
                .unwrap_or("No commit message")
                .to_string();
            
            let author = run["head_commit"]["author"]["name"]
                .as_str()
                .unwrap_or("Unknown")
                .to_string();
            
            let pipeline_run = PipelineRun {
                id,
                pipeline_id: pipeline_id.to_string(),
                commit_hash,
                commit_message,
                author,
                status,
                start_time,
                end_time,
                duration_seconds,
                stages: Vec::new(), // We would need to fetch job details to populate this
                artifacts: Vec::new(), // We would need to fetch artifacts to populate this
            };
            
            pipeline_runs.push(pipeline_run);
        }
        
        Ok(pipeline_runs)
    }
    
    async fn cancel_pipeline_run(&self, id: &str) -> Result<()> {
        // Cancel workflow run in GitHub repository
        let client = reqwest::Client::new();
        let response = client
            .post(&format!(
                "{}/repos/{}/{}/actions/runs/{}/cancel",
                self.base_url, self.owner, self.repo, id
            ))
            .header("Authorization", format!("token {}", self.token))
            .header("User-Agent", "hivemind-cicd")
            .send()
            .await
            .context("Failed to cancel workflow run")?;
        
        if !response.status().is_success() {
            return Err(anyhow::anyhow!("Failed to cancel workflow run: {}", response.status()));
        }
        
        Ok(())
    }
    
    async fn get_pipeline_run_logs(&self, id: &str) -> Result<String> {
        // In a real implementation, we would fetch logs from GitHub API
        // For now, we'll just return a dummy log
        Ok("Pipeline run logs...".to_string())
    }
}

/// CI/CD manager for Hivemind
pub struct CicdManager {
    /// Providers for CI/CD
    providers: RwLock<HashMap<String, Box<dyn CicdProvider>>>,
    /// App manager for deploying applications
    app_manager: Arc<AppManager>,
    /// Security manager for handling secrets
    security_manager: Arc<SecurityManager>,
    /// Base directory for CI/CD configuration
    base_dir: PathBuf,
}

impl CicdManager {
    /// Create a new CI/CD manager
    pub fn new(
        app_manager: Arc<AppManager>,
        security_manager: Arc<SecurityManager>,
        base_dir: PathBuf,
    ) -> Self {
        Self {
            providers: RwLock::new(HashMap::new()),
            app_manager,
            security_manager,
            base_dir,
        }
    }
    
    /// Initialize the CI/CD manager
    pub async fn initialize(&self) -> Result<()> {
        // Create base directory if it doesn't exist
        tokio::fs::create_dir_all(&self.base_dir).await?;
        
        Ok(())
    }
    
    /// Register a CI/CD provider
    pub async fn register_provider(&self, provider: Box<dyn CicdProvider>) -> Result<()> {
        let provider_name = provider.name().to_string();
        
        // Initialize the provider
        provider.initialize().await?;
        
        // Register the provider
        let mut providers = self.providers.write().await;
        providers.insert(provider_name, provider);
        
        Ok(())
    }
    
    /// Get a CI/CD provider by name
    pub async fn get_provider(&self, name: &str) -> Result<Box<dyn CicdProvider>> {
        let providers = self.providers.read().await;
        let provider = providers
            .get(name)
            .ok_or_else(|| anyhow::anyhow!("Provider not found: {}", name))?;
        
        // We can't return a reference to the provider because it's behind an RwLock
        // In a real implementation, we would return a reference or clone the provider
        Err(anyhow::anyhow!("Not implemented"))
    }
    
    /// List all registered CI/CD providers
    pub async fn list_providers(&self) -> Vec<String> {
        let providers = self.providers.read().await;
        providers.keys().cloned().collect()
    }
    
    /// Create a new pipeline
    pub async fn create_pipeline(&self, provider_name: &str, config: PipelineConfig) -> Result<String> {
        let providers = self.providers.read().await;
        let provider = providers
            .get(provider_name)
            .ok_or_else(|| anyhow::anyhow!("Provider not found: {}", provider_name))?;
        
        provider.create_pipeline(config).await
    }
    
    /// Update an existing pipeline
    pub async fn update_pipeline(&self, provider_name: &str, id: &str, config: PipelineConfig) -> Result<()> {
        let providers = self.providers.read().await;
        let provider = providers
            .get(provider_name)
            .ok_or_else(|| anyhow::anyhow!("Provider not found: {}", provider_name))?;
        
        provider.update_pipeline(id, config).await
    }
    
    /// Delete a pipeline
    pub async fn delete_pipeline(&self, provider_name: &str, id: &str) -> Result<()> {
        let providers = self.providers.read().await;
        let provider = providers
            .get(provider_name)
            .ok_or_else(|| anyhow::anyhow!("Provider not found: {}", provider_name))?;
        
        provider.delete_pipeline(id).await
    }
    
    /// Get a pipeline by ID
    pub async fn get_pipeline(&self, provider_name: &str, id: &str) -> Result<PipelineConfig> {
        let providers = self.providers.read().await;
        let provider = providers
            .get(provider_name)
            .ok_or_else(|| anyhow::anyhow!("Provider not found: {}", provider_name))?;
        
        provider.get_pipeline(id).await
    }
    
    /// List all pipelines for a provider
    pub async fn list_pipelines(&self, provider_name: &str) -> Result<Vec<PipelineConfig>> {
        let providers = self.providers.read().await;
        let provider = providers
            .get(provider_name)
            .ok_or_else(|| anyhow::anyhow!("Provider not found: {}", provider_name))?;
        
        provider.list_pipelines().await
    }
    
    /// Trigger a pipeline run
    pub async fn trigger_pipeline(&self, provider_name: &str, id: &str) -> Result<String> {
        let providers = self.providers.read().await;
        let provider = providers
            .get(provider_name)
            .ok_or_else(|| anyhow::anyhow!("Provider not found: {}", provider_name))?;
        
        provider.trigger_pipeline(id).await
    }
    
    /// Get a pipeline run by ID
    pub async fn get_pipeline_run(&self, provider_name: &str, id: &str) -> Result<PipelineRun> {
        let providers = self.providers.read().await;
        let provider = providers
            .get(provider_name)
            .ok_or_else(|| anyhow::anyhow!("Provider not found: {}", provider_name))?;
        
        provider.get_pipeline_run(id).await
    }
    
    /// List runs for a pipeline
    pub async fn list_pipeline_runs(&self, provider_name: &str, pipeline_id: &str) -> Result<Vec<PipelineRun>> {
        let providers = self.providers.read().await;
        let provider = providers
            .get(provider_name)
            .ok_or_else(|| anyhow::anyhow!("Provider not found: {}", provider_name))?;
        
        provider.list_pipeline_runs(pipeline_id).await
    }
    
    /// Cancel a pipeline run
    pub async fn cancel_pipeline_run(&self, provider_name: &str, id: &str) -> Result<()> {
        let providers = self.providers.read().await;
        let provider = providers
            .get(provider_name)
            .ok_or_else(|| anyhow::anyhow!("Provider not found: {}", provider_name))?;
        
        provider.cancel_pipeline_run(id).await
    }
    
    /// Get logs for a pipeline run
    pub async fn get_pipeline_run_logs(&self, provider_name: &str, id: &str) -> Result<String> {
        let providers = self.providers.read().await;
        let provider = providers
            .get(provider_name)
            .ok_or_else(|| anyhow::anyhow!("Provider not found: {}", provider_name))?;
        
        provider.get_pipeline_run_logs(id).await
    }
}