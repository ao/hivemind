use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::PathBuf;
use std::process::Command;
use std::sync::Arc;
use tokio::fs;
use tokio::sync::RwLock;

/// Represents a Helm chart
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HelmChart {
    /// Name of the chart
    pub name: String,
    /// Version of the chart
    pub version: String,
    /// Description of the chart
    pub description: String,
    /// App version
    pub app_version: String,
    /// Keywords for the chart
    pub keywords: Vec<String>,
    /// Home URL for the chart
    pub home: Option<String>,
    /// Sources for the chart
    pub sources: Vec<String>,
    /// Maintainers for the chart
    pub maintainers: Vec<HelmMaintainer>,
    /// Icon URL for the chart
    pub icon: Option<String>,
    /// API version for the chart
    pub api_version: String,
    /// Type of chart
    pub chart_type: Option<String>,
    /// Dependencies for the chart
    pub dependencies: Vec<HelmDependency>,
    /// Values for the chart
    pub values: serde_yaml::Value,
}

/// Represents a Helm chart maintainer
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HelmMaintainer {
    /// Name of the maintainer
    pub name: String,
    /// Email of the maintainer
    pub email: Option<String>,
    /// URL of the maintainer
    pub url: Option<String>,
}

/// Represents a Helm chart dependency
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HelmDependency {
    /// Name of the dependency
    pub name: String,
    /// Version of the dependency
    pub version: String,
    /// Repository URL for the dependency
    pub repository: String,
    /// Condition for the dependency
    pub condition: Option<String>,
    /// Tags for the dependency
    pub tags: Vec<String>,
    /// Whether the dependency is enabled
    pub enabled: Option<bool>,
    /// Import values from the dependency
    pub import_values: Option<Vec<serde_yaml::Value>>,
    /// Alias for the dependency
    pub alias: Option<String>,
}

/// Represents a Helm repository
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HelmRepository {
    /// Name of the repository
    pub name: String,
    /// URL of the repository
    pub url: String,
    /// Username for the repository
    pub username: Option<String>,
    /// Password for the repository
    pub password_encrypted: Option<String>,
    /// Whether the repository is OCI-based
    pub is_oci: bool,
}

/// Represents a Helm release
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HelmRelease {
    /// Name of the release
    pub name: String,
    /// Namespace of the release
    pub namespace: String,
    /// Chart for the release
    pub chart: String,
    /// Version of the chart
    pub version: String,
    /// Values for the release
    pub values: serde_yaml::Value,
    /// Status of the release
    pub status: HelmReleaseStatus,
    /// Revision of the release
    pub revision: u32,
    /// Last updated timestamp
    pub updated: chrono::DateTime<chrono::Utc>,
}

/// Represents the status of a Helm release
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum HelmReleaseStatus {
    /// Release is deployed
    Deployed,
    /// Release is pending
    Pending,
    /// Release is failed
    Failed,
    /// Release is superseded
    Superseded,
    /// Release is uninstalled
    Uninstalled,
}

/// Helm manager for Hivemind
pub struct HelmManager {
    /// Base directory for Helm charts
    base_dir: PathBuf,
    /// Repositories
    repositories: RwLock<HashMap<String, HelmRepository>>,
    /// Releases
    releases: RwLock<HashMap<String, HelmRelease>>,
}

impl HelmManager {
    /// Create a new Helm manager
    pub fn new(base_dir: PathBuf) -> Self {
        Self {
            base_dir,
            repositories: RwLock::new(HashMap::new()),
            releases: RwLock::new(HashMap::new()),
        }
    }
    
    /// Initialize the Helm manager
    pub async fn initialize(&self) -> Result<()> {
        // Create base directory if it doesn't exist
        fs::create_dir_all(&self.base_dir).await?;
        
        // Create charts directory if it doesn't exist
        let charts_dir = self.base_dir.join("charts");
        fs::create_dir_all(&charts_dir).await?;
        
        // Create repositories directory if it doesn't exist
        let repositories_dir = self.base_dir.join("repositories");
        fs::create_dir_all(&repositories_dir).await?;
        
        // Create releases directory if it doesn't exist
        let releases_dir = self.base_dir.join("releases");
        fs::create_dir_all(&releases_dir).await?;
        
        Ok(())
    }
    
    /// Add a Helm repository
    pub async fn add_repository(&self, repository: HelmRepository) -> Result<()> {
        // Add the repository to Helm
        let mut cmd = Command::new("helm");
        cmd.arg("repo").arg("add").arg(&repository.name).arg(&repository.url);
        
        if let Some(username) = &repository.username {
            cmd.arg("--username").arg(username);
        }
        
        if let Some(password) = &repository.password_encrypted {
            // In a real implementation, we would decrypt the password
            // For now, we'll just use a placeholder
            cmd.arg("--password").arg("placeholder");
        }
        
        if repository.is_oci {
            cmd.arg("--oci");
        }
        
        let output = cmd.output().context("Failed to execute helm repo add command")?;
        
        if !output.status.success() {
            return Err(anyhow::anyhow!(
                "Failed to add Helm repository: {}",
                String::from_utf8_lossy(&output.stderr)
            ));
        }
        
        // Update the repository
        let output = Command::new("helm")
            .arg("repo")
            .arg("update")
            .output()
            .context("Failed to execute helm repo update command")?;
        
        if !output.status.success() {
            return Err(anyhow::anyhow!(
                "Failed to update Helm repository: {}",
                String::from_utf8_lossy(&output.stderr)
            ));
        }
        
        // Store the repository
        let mut repositories = self.repositories.write().await;
        repositories.insert(repository.name.clone(), repository);
        
        Ok(())
    }
    
    /// Remove a Helm repository
    pub async fn remove_repository(&self, name: &str) -> Result<()> {
        // Remove the repository from Helm
        let output = Command::new("helm")
            .arg("repo")
            .arg("remove")
            .arg(name)
            .output()
            .context("Failed to execute helm repo remove command")?;
        
        if !output.status.success() {
            return Err(anyhow::anyhow!(
                "Failed to remove Helm repository: {}",
                String::from_utf8_lossy(&output.stderr)
            ));
        }
        
        // Remove the repository from storage
        let mut repositories = self.repositories.write().await;
        repositories.remove(name);
        
        Ok(())
    }
    
    /// List Helm repositories
    pub async fn list_repositories(&self) -> Vec<HelmRepository> {
        let repositories = self.repositories.read().await;
        repositories.values().cloned().collect()
    }
    
    /// Create a new Helm chart
    pub async fn create_chart(&self, name: &str, description: &str) -> Result<PathBuf> {
        // Create the chart directory
        let chart_dir = self.base_dir.join("charts").join(name);
        fs::create_dir_all(&chart_dir).await?;
        
        // Create the Chart.yaml file
        let chart_yaml = chart_dir.join("Chart.yaml");
        let chart_yaml_content = format!(
            r#"apiVersion: v2
name: {}
description: {}
version: 0.1.0
appVersion: "1.0.0"
"#,
            name, description
        );
        fs::write(&chart_yaml, chart_yaml_content).await?;
        
        // Create the values.yaml file
        let values_yaml = chart_dir.join("values.yaml");
        let values_yaml_content = r#"# Default values for the chart
replicaCount: 1

image:
  repository: nginx
  pullPolicy: IfNotPresent
  tag: ""

imagePullSecrets: []
nameOverride: ""
fullnameOverride: ""

serviceAccount:
  create: true
  annotations: {}
  name: ""

podAnnotations: {}

podSecurityContext: {}

securityContext: {}

service:
  type: ClusterIP
  port: 80

ingress:
  enabled: false
  className: ""
  annotations: {}
  hosts:
    - host: chart-example.local
      paths:
        - path: /
          pathType: ImplementationSpecific
  tls: []

resources: {}

autoscaling:
  enabled: false
  minReplicas: 1
  maxReplicas: 100
  targetCPUUtilizationPercentage: 80

nodeSelector: {}

tolerations: []

affinity: {}
"#;
        fs::write(&values_yaml, values_yaml_content).await?;
        
        // Create the templates directory
        let templates_dir = chart_dir.join("templates");
        fs::create_dir_all(&templates_dir).await?;
        
        // Create the deployment.yaml template
        let deployment_yaml = templates_dir.join("deployment.yaml");
        let deployment_yaml_content = r#"apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "chart.fullname" . }}
  labels:
    {{- include "chart.labels" . | nindent 4 }}
spec:
  {{- if not .Values.autoscaling.enabled }}
  replicas: {{ .Values.replicaCount }}
  {{- end }}
  selector:
    matchLabels:
      {{- include "chart.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      {{- with .Values.podAnnotations }}
      annotations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      labels:
        {{- include "chart.selectorLabels" . | nindent 8 }}
    spec:
      {{- with .Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      serviceAccountName: {{ include "chart.serviceAccountName" . }}
      securityContext:
        {{- toYaml .Values.podSecurityContext | nindent 8 }}
      containers:
        - name: {{ .Chart.Name }}
          securityContext:
            {{- toYaml .Values.securityContext | nindent 12 }}
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          ports:
            - name: http
              containerPort: 80
              protocol: TCP
          livenessProbe:
            httpGet:
              path: /
              port: http
          readinessProbe:
            httpGet:
              path: /
              port: http
          resources:
            {{- toYaml .Values.resources | nindent 12 }}
      {{- with .Values.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
"#;
        fs::write(&deployment_yaml, deployment_yaml_content).await?;
        
        // Create the service.yaml template
        let service_yaml = templates_dir.join("service.yaml");
        let service_yaml_content = r#"apiVersion: v1
kind: Service
metadata:
  name: {{ include "chart.fullname" . }}
  labels:
    {{- include "chart.labels" . | nindent 4 }}
spec:
  type: {{ .Values.service.type }}
  ports:
    - port: {{ .Values.service.port }}
      targetPort: http
      protocol: TCP
      name: http
  selector:
    {{- include "chart.selectorLabels" . | nindent 4 }}
"#;
        fs::write(&service_yaml, service_yaml_content).await?;
        
        // Create the _helpers.tpl template
        let helpers_tpl = templates_dir.join("_helpers.tpl");
        let helpers_tpl_content = r#"{{/*
Expand the name of the chart.
*/}}
{{- define "chart.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "chart.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "chart.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "chart.labels" -}}
helm.sh/chart: {{ include "chart.chart" . }}
{{ include "chart.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "chart.selectorLabels" -}}
app.kubernetes.io/name: {{ include "chart.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "chart.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "chart.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
"#;
        fs::write(&helpers_tpl, helpers_tpl_content).await?;
        
        // Create the NOTES.txt template
        let notes_txt = templates_dir.join("NOTES.txt");
        let notes_txt_content = r#"1. Get the application URL by running these commands:
{{- if .Values.ingress.enabled }}
{{- range $host := .Values.ingress.hosts }}
  {{- range .paths }}
  http{{ if $.Values.ingress.tls }}s{{ end }}://{{ $host.host }}{{ .path }}
  {{- end }}
{{- end }}
{{- else if contains "NodePort" .Values.service.type }}
  export NODE_PORT=$(kubectl get --namespace {{ .Release.Namespace }} -o jsonpath="{.spec.ports[0].nodePort}" services {{ include "chart.fullname" . }})
  export NODE_IP=$(kubectl get nodes --namespace {{ .Release.Namespace }} -o jsonpath="{.items[0].status.addresses[0].address}")
  echo http://$NODE_IP:$NODE_PORT
{{- else if contains "LoadBalancer" .Values.service.type }}
     NOTE: It may take a few minutes for the LoadBalancer IP to be available.
           You can watch the status of by running 'kubectl get --namespace {{ .Release.Namespace }} svc -w {{ include "chart.fullname" . }}'
  export SERVICE_IP=$(kubectl get svc --namespace {{ .Release.Namespace }} {{ include "chart.fullname" . }} --template "{{"{{ range (index .status.loadBalancer.ingress 0) }}{{.}}{{ end }}"}}")
  echo http://$SERVICE_IP:{{ .Values.service.port }}
{{- else if contains "ClusterIP" .Values.service.type }}
  export POD_NAME=$(kubectl get pods --namespace {{ .Release.Namespace }} -l "app.kubernetes.io/name={{ include "chart.name" . }},app.kubernetes.io/instance={{ .Release.Name }}" -o jsonpath="{.items[0].metadata.name}")
  export CONTAINER_PORT=$(kubectl get pod --namespace {{ .Release.Namespace }} $POD_NAME -o jsonpath="{.spec.containers[0].ports[0].containerPort}")
  echo "Visit http://127.0.0.1:8080 to use your application"
  kubectl --namespace {{ .Release.Namespace }} port-forward $POD_NAME 8080:$CONTAINER_PORT
{{- end }}
"#;
        fs::write(&notes_txt, notes_txt_content).await?;
        
        // Create the serviceaccount.yaml template
        let serviceaccount_yaml = templates_dir.join("serviceaccount.yaml");
        let serviceaccount_yaml_content = r#"{{- if .Values.serviceAccount.create -}}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "chart.serviceAccountName" . }}
  labels:
    {{- include "chart.labels" . | nindent 4 }}
  {{- with .Values.serviceAccount.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end }}
"#;
        fs::write(&serviceaccount_yaml, serviceaccount_yaml_content).await?;
        
        // Create the hpa.yaml template
        let hpa_yaml = templates_dir.join("hpa.yaml");
        let hpa_yaml_content = r#"{{- if .Values.autoscaling.enabled }}
apiVersion: autoscaling/v2beta1
kind: HorizontalPodAutoscaler
metadata:
  name: {{ include "chart.fullname" . }}
  labels:
    {{- include "chart.labels" . | nindent 4 }}
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: {{ include "chart.fullname" . }}
  minReplicas: {{ .Values.autoscaling.minReplicas }}
  maxReplicas: {{ .Values.autoscaling.maxReplicas }}
  metrics:
    {{- if .Values.autoscaling.targetCPUUtilizationPercentage }}
    - type: Resource
      resource:
        name: cpu
        targetAverageUtilization: {{ .Values.autoscaling.targetCPUUtilizationPercentage }}
    {{- end }}
    {{- if .Values.autoscaling.targetMemoryUtilizationPercentage }}
    - type: Resource
      resource:
        name: memory
        targetAverageUtilization: {{ .Values.autoscaling.targetMemoryUtilizationPercentage }}
    {{- end }}
{{- end }}
"#;
        fs::write(&hpa_yaml, hpa_yaml_content).await?;
        
        // Create the ingress.yaml template
        let ingress_yaml = templates_dir.join("ingress.yaml");
        let ingress_yaml_content = r#"{{- if .Values.ingress.enabled -}}
{{- $fullName := include "chart.fullname" . -}}
{{- $svcPort := .Values.service.port -}}
{{- if and .Values.ingress.className (not (semverCompare ">=1.18-0" .Capabilities.KubeVersion.GitVersion)) }}
  {{- if not (hasKey .Values.ingress.annotations "kubernetes.io/ingress.class") }}
  {{- $_ := set .Values.ingress.annotations "kubernetes.io/ingress.class" .Values.ingress.className}}
  {{- end }}
{{- end }}
{{- if semverCompare ">=1.19-0" .Capabilities.KubeVersion.GitVersion -}}
apiVersion: networking.k8s.io/v1
{{- else if semverCompare ">=1.14-0" .Capabilities.KubeVersion.GitVersion -}}
apiVersion: networking.k8s.io/v1beta1
{{- else -}}
apiVersion: extensions/v1beta1
{{- end }}
kind: Ingress
metadata:
  name: {{ $fullName }}
  labels:
    {{- include "chart.labels" . | nindent 4 }}
  {{- with .Values.ingress.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  {{- if and .Values.ingress.className (semverCompare ">=1.18-0" .Capabilities.KubeVersion.GitVersion) }}
  ingressClassName: {{ .Values.ingress.className }}
  {{- end }}
  {{- if .Values.ingress.tls }}
  tls:
    {{- range .Values.ingress.tls }}
    - hosts:
        {{- range .hosts }}
        - {{ . | quote }}
        {{- end }}
      secretName: {{ .secretName }}
    {{- end }}
  {{- end }}
  rules:
    {{- range .Values.ingress.hosts }}
    - host: {{ .host | quote }}
      http:
        paths:
          {{- range .paths }}
          - path: {{ .path }}
            {{- if and .pathType (semverCompare ">=1.18-0" $.Capabilities.KubeVersion.GitVersion) }}
            pathType: {{ .pathType }}
            {{- end }}
            backend:
              {{- if semverCompare ">=1.19-0" $.Capabilities.KubeVersion.GitVersion }}
              service:
                name: {{ $fullName }}
                port:
                  number: {{ $svcPort }}
              {{- else }}
              serviceName: {{ $fullName }}
              servicePort: {{ $svcPort }}
              {{- end }}
          {{- end }}
    {{- end }}
{{- end }}
"#;
        fs::write(&ingress_yaml, ingress_yaml_content).await?;
        
        // Create the tests directory
        let tests_dir = templates_dir.join("tests");
        fs::create_dir_all(&tests_dir).await?;
        
        // Create the test-connection.yaml test
        let test_connection_yaml = tests_dir.join("test-connection.yaml");
        let test_connection_yaml_content = r#"apiVersion: v1
kind: Pod
metadata:
  name: "{{ include "chart.fullname" . }}-test-connection"
  labels:
    {{- include "chart.labels" . | nindent 4 }}
  annotations:
    "helm.sh/hook": test
spec:
  containers:
    - name: wget
      image: busybox
      command: ['wget']
      args: ['{{ include "chart.fullname" . }}:{{ .Values.service.port }}']
  restartPolicy: Never
"#;
        fs::write(&test_connection_yaml, test_connection_yaml_content).await?;
        
        // Create the .helmignore file
        let helmignore = chart_dir.join(".helmignore");
        let helmignore_content = r#"# Patterns to ignore when building packages.
# This supports shell glob matching, relative path matching, and
# negation (prefixed with !). Only one pattern per line.
.DS_Store
# Common VCS dirs
.git/
.gitignore
.bzr/
.bzrignore
.hg/
.hgignore
.svn/
# Common backup files
*.swp
*.bak
*.tmp
*.orig
*~
# Various IDEs
.project
.idea/
*.tmproj
.vscode/
"#;
        fs::write(&helmignore, helmignore_content).await?;
        
        Ok(chart_dir)
    }
    
    /// Package a Helm chart
    pub async fn package_chart(&self, chart_dir: &PathBuf) -> Result<PathBuf> {
        // Package the chart
        let output = Command::new("helm")
            .arg("package")
            .arg(chart_dir)
            .arg("--destination")
            .arg(self.base_dir.join("charts"))
            .output()
            .context("Failed to execute helm package command")?;
        
        if !output.status.success() {
            return Err(anyhow::anyhow!(
                "Failed to package Helm chart: {}",
                String::from_utf8_lossy(&output.stderr)
            ));
        }
        
        // Get the packaged chart file
        let stdout = String::from_utf8_lossy(&output.stdout);
        let package_path = stdout
            .lines()
            .next()
            .and_then(|line| line.split("Successfully packaged chart and saved it to: ").nth(1))
            .ok_or_else(|| anyhow::anyhow!("Failed to parse helm package output"))?;
        
        Ok(PathBuf::from(package_path))
    }
    
    /// Install a Helm chart
    pub async fn install_chart(
        &self,
        name: &str,
        chart: &str,
        namespace: &str,
        values: Option<serde_yaml::Value>,
    ) -> Result<HelmRelease> {
        // Create a values file if provided
        let values_file = if let Some(values) = values {
            let values_file = self.base_dir.join("values.yaml");
            let values_yaml = serde_yaml::to_string(&values)?;
            fs::write(&values_file, values_yaml).await?;
            Some(values_file)
        } else {
            None
        };
        
        // Install the chart
        let mut cmd = Command::new("helm");
        cmd.arg("install")
            .arg(name)
            .arg(chart)
            .arg("--namespace")
            .arg(namespace)
            .arg("--create-namespace");
        
        if let Some(values_file) = &values_file {
            cmd.arg("--values").arg(values_file);
        }
        
        let output = cmd.output().context("Failed to execute helm install command")?;
        
        if !output.status.success() {
            return Err(anyhow::anyhow!(
                "Failed to install Helm chart: {}",
                String::from_utf8_lossy(&output.stderr)
            ));
        }
        
        // Get the release
        let release = self.get_release(name, namespace).await?;
        
        // Store the release
        let mut releases = self.releases.write().await;
        releases.insert(format!("{}/{}", namespace, name), release.clone());
        
        Ok(release)
    }
    
    /// Upgrade a Helm release
    pub async fn upgrade_release(
        &self,
        name: &str,
        chart: &str,
        namespace: &str,
        values: Option<serde_yaml::Value>,
    ) -> Result<HelmRelease> {
        // Create a values file if provided
        let values_file = if let Some(values) = values {
            let values_file = self.base_dir.join("values.yaml");
            let values_yaml = serde_yaml::to_string(&values)?;
            fs::write(&values_file, values_yaml).await?;
            Some(values_file)
        } else {
            None
        };
        
        // Upgrade the release
        let mut cmd = Command::new("helm");
        cmd.arg("upgrade")
            .arg(name)
            .arg(chart)
            .arg("--namespace")
            .arg(namespace);
        
        if let Some(values_file) = &values_file {
            cmd.arg("--values").arg(values_file);
        }
        
        let output = cmd.output().context("Failed to execute helm upgrade command")?;
        
        if !output.status.success() {
            return Err(anyhow::anyhow!(
                "Failed to upgrade Helm release: {}",
                String::from_utf8_lossy(&output.stderr)
            ));
        }
        
        // Get the release
        let release = self.get_release(name, namespace).await?;
        
        // Store the release
        let mut releases = self.releases.write().await;
        releases.insert(format!("{}/{}", namespace, name), release.clone());
        
        Ok(release)
    }
    
    /// Uninstall a Helm release
    pub async fn uninstall_release(&self, name: &str, namespace: &str) -> Result<()> {
        // Uninstall the release
        let output = Command::new("helm")
            .arg("uninstall")
            .arg(name)
            .arg("--namespace")
            .arg(namespace)
            .output()
            .context("Failed to execute helm uninstall command")?;
        
        if !output.status.success() {
            return Err(anyhow::anyhow!(
                "Failed to uninstall Helm release: {}",
                String::from_utf8_lossy(&output.stderr)
            ));
        }
        
        // Remove the release from storage
        let mut releases = self.releases.write().await;
        releases.remove(&format!("{}/{}", namespace, name));
        
        Ok(())
    }
    
    /// Get a Helm release
    pub async fn get_release(&self, name: &str, namespace: &str) -> Result<HelmRelease> {
        // Get the release
        let output = Command::new("helm")
            .arg("get")
            .arg("all")
            .arg(name)
            .arg("--namespace")
            .arg(namespace)
            .arg("--output")
            .arg("json")
            .output()
            .context("Failed to execute helm get command")?;
        
        if !output.status.success() {
            return Err(anyhow::anyhow!(
                "Failed to get Helm release: {}",
                String::from_utf8_lossy(&output.stderr)
            ));
        }
        
        // Parse the release
        let release_json: serde_json::Value = serde_json::from_slice(&output.stdout)?;
        
        // Extract the release information
        let release = HelmRelease {
            name: name.to_string(),
            namespace: namespace.to_string(),
            chart: release_json["chart"]["metadata"]["name"]
                .as_str()
                .unwrap_or("").to_string(),
            version: release_json["chart"]["metadata"]["version"]
                .as_str()
                .unwrap_or("").to_string(),
            values: serde_yaml::from_str(&release_json["config"].to_string())
                .unwrap_or(serde_yaml::Value::Null),
            status: match release_json["info"]["status"].as_str().unwrap_or("") {
                "deployed" => HelmReleaseStatus::Deployed,
                "pending" => HelmReleaseStatus::Pending,
                "failed" => HelmReleaseStatus::Failed,
                "superseded" => HelmReleaseStatus::Superseded,
                "uninstalled" => HelmReleaseStatus::Uninstalled,
                _ => HelmReleaseStatus::Failed,
            },
            revision: release_json["version"]
                .as_u64()
                .unwrap_or(0) as u32,
            updated: chrono::DateTime::parse_from_rfc3339(
                release_json["info"]["last_deployed"]
                    .as_str()
                    .unwrap_or("1970-01-01T00:00:00Z"),
            )
            .unwrap_or_default()
            .with_timezone(&chrono::Utc),
        };
        
        Ok(release)
    }
    
    /// List Helm releases
    pub async fn list_releases(&self, namespace: Option<&str>) -> Result<Vec<HelmRelease>> {
        // List the releases
        let mut cmd = Command::new("helm");
        cmd.arg("list").arg("--output").arg("json");
        
        if let Some(namespace) = namespace {
            cmd.arg("--namespace").arg(namespace);
        } else {
            cmd.arg("--all-namespaces");
        }
        
        let output = cmd.output().context("Failed to execute helm list command")?;
        
        if !output.status.success() {
            return Err(anyhow::anyhow!(
                "Failed to list Helm releases: {}",
                String::from_utf8_lossy(&output.stderr)
            ));
        }
        
        // Parse the releases
        let releases_json: Vec<serde_json::Value> = serde_json::from_slice(&output.stdout)?;
        
        // Extract the release information
        let mut releases = Vec::new();
        for release_json in releases_json {
            let name = release_json["name"].as_str().unwrap_or("").to_string();
            let namespace = release_json["namespace"].as_str().unwrap_or("default").to_string();
            
            // Get the full release
            match self.get_release(&name, &namespace).await {
                Ok(release) => releases.push(release),
                Err(e) => eprintln!("Failed to get release {}/{}: {}", namespace, name, e),
            }
        }
        
        Ok(releases)
    }
    
    /// Create Helm charts for Hivemind components
    pub async fn create_hivemind_charts(&self) -> Result<Vec<PathBuf>> {
        let mut chart_dirs = Vec::new();
        
        // Create the main Hivemind chart
        let hivemind_chart_dir = self.create_chart("hivemind", "Hivemind container orchestration platform").await?;
        chart_dirs.push(hivemind_chart_dir.clone());
        
        // Update the Chart.yaml file
        let chart_yaml = hivemind_chart_dir.join("Chart.yaml");
        let chart_yaml_content = r#"apiVersion: v2
name: hivemind
description: Hivemind container orchestration platform
version: 0.1.0
appVersion: "0.1.1"
type: application
keywords:
  - container
  - orchestration
  - kubernetes
home: https://github.com/yourusername/hivemind
sources:
  - https://github.com/yourusername/hivemind
maintainers:
  - name: Your Name
    email: your.email@example.com
icon: https://raw.githubusercontent.com/yourusername/hivemind/main/assets/logo1.png
dependencies:
  - name: hivemind-api
    version: 0.1.0
    repository: file://../hivemind-api
  - name: hivemind-scheduler
    version: 0.1.0
    repository: file://../hivemind-scheduler
  - name: hivemind-network
    version: 0.1.0
    repository: file://../hivemind-network
  - name: hivemind-storage
    version: 0.1.0
    repository: file://../hivemind-storage
"#;
        fs::write(&chart_yaml, chart_yaml_content).await?;
        
        // Update the values.yaml file
        let values_yaml = hivemind_chart_dir.join("values.yaml");
        let values_yaml_content = r#"# Default values for Hivemind
global:
  imageRegistry: ""
  imagePullSecrets: []
  storageClass: ""

hivemind-api:
  enabled: true
  replicaCount: 1
  image:
    repository: yourusername/hivemind-api
    tag: 0.1.1
    pullPolicy: IfNotPresent
  service:
    type: ClusterIP
    port: 3000
  resources:
    limits:
      cpu: 500m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi

hivemind-scheduler:
  enabled: true
  replicaCount: 1
  image:
    repository: yourusername/hivemind-scheduler
    tag: 0.1.1
    pullPolicy: IfNotPresent
  service:
    type: ClusterIP
    port: 3001
  resources:
    limits:
      cpu: 500m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi

hivemind-network:
  enabled: true
  replicaCount: 1
  image:
    repository: yourusername/hivemind-network
    tag: 0.1.1
    pullPolicy: IfNotPresent
  service:
    type: ClusterIP
    port: 3002
  resources:
    limits:
      cpu: 500m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi

hivemind-storage:
  enabled: true
  replicaCount: 1
  image:
    repository: yourusername/hivemind-storage
    tag: 0.1.1
    pullPolicy: IfNotPresent
  service:
    type: ClusterIP
    port: 3003
  resources:
    limits:
      cpu: 500m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi

ingress:
  enabled: false
  className: ""
  annotations: {}
  hosts:
    - host: hivemind.local
      paths:
        - path: /
          pathType: ImplementationSpecific
  tls: []

prometheus:
  enabled: false
  serviceMonitor:
    enabled: false

grafana:
  enabled: false
  dashboards:
    enabled: true

persistence:
  enabled: true
  size: 10Gi
"#;
        fs::write(&values_yaml, values_yaml_content).await?;
        
        // Create the API chart
        let api_chart_dir = self.create_chart("hivemind-api", "Hivemind API component").await?;
        chart_dirs.push(api_chart_dir);
        
        // Create the Scheduler chart
        let scheduler_chart_dir = self.create_chart("hivemind-scheduler", "Hivemind Scheduler component").await?;
        chart_dirs.push(scheduler_chart_dir);
        
        // Create the Network chart
        let network_chart_dir = self.create_chart("hivemind-network", "Hivemind Network component").await?;
        chart_dirs.push(network_chart_dir);
        
        // Create the Storage chart
        let storage_chart_dir = self.create_chart("hivemind-storage", "Hivemind Storage component").await?;
        chart_dirs.push(storage_chart_dir);
        
        Ok(chart_dirs)
    }
}