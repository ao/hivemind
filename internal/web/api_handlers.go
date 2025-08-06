package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ao/hivemind/internal/app"
	"github.com/ao/hivemind/pkg/api"
	"github.com/gin-gonic/gin"
)

// apiNodesHandler handles the API endpoint for nodes
func (ws *WebServer) apiNodesHandler(c *gin.Context) {
	nodes, err := ws.nodeManager.GetNodeDetails()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, nodes)
}

// apiContainersHandler handles the API endpoint for containers
func (ws *WebServer) apiContainersHandler(c *gin.Context) {
	containers, err := ws.containerManager.ListContainers(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, containers)
}

// apiImagesHandler handles the API endpoint for images
func (ws *WebServer) apiImagesHandler(c *gin.Context) {
	images, err := ws.containerManager.ListImages(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, images)
}

// apiServicesHandler handles the API endpoint for services
func (ws *WebServer) apiServicesHandler(c *gin.Context) {
	services, err := ws.appManager.ListServices(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, services)
}

// apiServiceEndpointsHandler handles the API endpoint for service endpoints
func (ws *WebServer) apiServiceEndpointsHandler(c *gin.Context) {
	endpoints := ws.serviceDiscovery.ListServices()

	c.JSON(http.StatusOK, endpoints)
}

// apiHealthHandler handles the API endpoint for health
func (ws *WebServer) apiHealthHandler(c *gin.Context) {
	// Get node health
	nodeHealth, err := ws.nodeManager.CheckHealth()
	if err != nil {
		ws.logger.Errorf("Failed to check node health: %v", err)
		nodeHealth = false
	}

	// Get containers
	containers := ws.containerManager.GetContainers()

	// Calculate container health
	runningContainers := 0
	for _, container := range containers {
		if container.Status == api.ContainerRunning {
			runningContainers++
		}
	}

	containerHealth := 0.0
	if len(containers) > 0 {
		containerHealth = float64(runningContainers) / float64(len(containers))
	}

	c.JSON(http.StatusOK, gin.H{
		"status":           "basic",
		"node_health":      nodeHealth,
		"container_health": containerHealth,
		"overall_health":   nodeHealth && containerHealth > 0.8,
	})
}

// DeployRequest represents a request to deploy an application
type DeployRequest struct {
	Name    string            `json:"name" binding:"required"`
	Image   string            `json:"image" binding:"required"`
	Service string            `json:"service"`
	Env     map[string]string `json:"env"`
	Volumes []string          `json:"volumes"`
}

// apiDeployHandler handles the API endpoint for deploying an application
func (ws *WebServer) apiDeployHandler(c *gin.Context) {
	var req DeployRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	options := app.AppOptions{
		Name:    req.Name,
		Image:   req.Image,
		Volumes: req.Volumes,
		Env:     req.Env,
		// Add other fields as needed
	}

	containerID, err := ws.appManager.DeployApp(c.Request.Context(), options.Image, options.Name, "", options.Volumes, options.Env)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"container_id": containerID,
		"message":      "Deployed application " + req.Name + " with container " + containerID,
	})
}

// ScaleRequest represents a request to scale an application
type ScaleRequest struct {
	Name     string `json:"name" binding:"required"`
	Replicas int    `json:"replicas" binding:"required,min=1"`
}

// apiScaleHandler handles the API endpoint for scaling an application
func (ws *WebServer) apiScaleHandler(c *gin.Context) {
	var req ScaleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	err := ws.appManager.ScaleApp(c.Request.Context(), req.Name, req.Replicas)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Scaled application " + req.Name + " to " + strconv.Itoa(req.Replicas) + " replicas",
	})
}

// RestartRequest represents a request to restart an application
type RestartRequest struct {
	Name string `json:"name" binding:"required"`
}

// apiRestartHandler handles the API endpoint for restarting an application
func (ws *WebServer) apiRestartHandler(c *gin.Context) {
	var req RestartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	err := ws.appManager.RestartAppSimple(c.Request.Context(), req.Name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Restarted application " + req.Name,
	})
}

// ServiceURLRequest represents a request to get a service URL
type ServiceURLRequest struct {
	ServiceName string `json:"service_name" binding:"required"`
}

// apiServiceURLHandler handles the API endpoint for getting a service URL
func (ws *WebServer) apiServiceURLHandler(c *gin.Context) {
	var req ServiceURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	url, err := ws.serviceDiscovery.GetServiceURL(c.Request.Context(), req.ServiceName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if url == "" {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error":   "Service not found or no healthy endpoints available",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"url":     url,
	})
}

// VolumeResponse represents a volume response
type VolumeResponse struct {
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	Size      uint64 `json:"size"`
}

// apiListVolumes handles the API endpoint for listing volumes
func (ws *WebServer) apiListVolumes(c *gin.Context) {
	volumes, err := ws.storageManager.ListVolumes(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to list volumes: " + err.Error(),
		})
		return
	}

	var response []VolumeResponse
	for _, v := range volumes {
		response = append(response, VolumeResponse{
			Name:      v.Name,
			CreatedAt: v.CreatedAt,
			Size:      uint64(v.Size),
		})
	}

	c.JSON(http.StatusOK, response)
}

// CreateVolumeRequest represents a request to create a volume
type CreateVolumeRequest struct {
	Name string `json:"name" binding:"required"`
}

// apiCreateVolume handles the API endpoint for creating a volume
func (ws *WebServer) apiCreateVolume(c *gin.Context) {
	var req CreateVolumeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	// Use a default size of 1GB (1024*1024*1024 bytes)
	volumeID, err := ws.storageManager.CreateVolumeSimple(c.Request.Context(), req.Name, 1024*1024*1024)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to create volume: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"message": "Volume " + req.Name + " created",
		"id":      volumeID,
	})
}

// DeleteVolumeRequest represents a request to delete a volume
type DeleteVolumeRequest struct {
	Name string `json:"name" binding:"required"`
}

// apiDeleteVolume handles the API endpoint for deleting a volume
func (ws *WebServer) apiDeleteVolume(c *gin.Context) {
	var req DeleteVolumeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	err := ws.storageManager.DeleteVolumeWithContext(c.Request.Context(), req.Name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to delete volume: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Volume " + req.Name + " deleted",
	})
}

// apiListTenants handles the API endpoint for listing tenants
func (ws *WebServer) apiListTenants(c *gin.Context) {
	tenants, err := ws.appManager.ListTenants(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Tenant manager is not available",
		})
		return
	}

	c.JSON(http.StatusOK, tenants)
}

// apiGetTenant handles the API endpoint for getting a tenant
func (ws *WebServer) apiGetTenant(c *gin.Context) {
	tenantID := c.Param("id")

	tenant, err := ws.appManager.GetTenant(c.Request.Context(), tenantID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "Tenant " + tenantID + " not found",
		})
		return
	}

	c.JSON(http.StatusOK, tenant)
}

// CreateTenantRequest represents a request to create a tenant
type CreateTenantRequest struct {
	Name           string  `json:"name" binding:"required"`
	IsolationLevel string  `json:"isolation_level" binding:"required"`
	Description    string  `json:"description"`
	CPULimit       *uint32 `json:"cpu_limit"`
	MemoryLimit    *uint64 `json:"memory_limit"`
	StorageLimit   *uint64 `json:"storage_limit"`
	MaxContainers  *uint32 `json:"max_containers"`
	MaxServices    *uint32 `json:"max_services"`
	OwnerID        string  `json:"owner_id" binding:"required"`
}

// apiCreateTenant handles the API endpoint for creating a tenant
func (ws *WebServer) apiCreateTenant(c *gin.Context) {
	var req CreateTenantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	tenant, err := ws.appManager.CreateTenant(c.Request.Context(), req.Name, req.Description, req.OwnerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to create tenant: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusCreated, tenant)
}

// UpdateTenantRequest represents a request to update a tenant
type UpdateTenantRequest struct {
	ID             string  `json:"id" binding:"required"`
	Name           string  `json:"name" binding:"required"`
	IsolationLevel string  `json:"isolation_level" binding:"required"`
	Description    string  `json:"description"`
	CPULimit       *uint32 `json:"cpu_limit"`
	MemoryLimit    *uint64 `json:"memory_limit"`
	StorageLimit   *uint64 `json:"storage_limit"`
	MaxContainers  *uint32 `json:"max_containers"`
	MaxServices    *uint32 `json:"max_services"`
	Status         string  `json:"status" binding:"required"`
}

// apiUpdateTenant handles the API endpoint for updating a tenant
func (ws *WebServer) apiUpdateTenant(c *gin.Context) {
	var req UpdateTenantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	tenant, err := ws.appManager.UpdateTenant(c.Request.Context(), req.ID, req.Name, req.Status)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to update tenant: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, tenant)
}

// DeleteTenantRequest represents a request to delete a tenant
type DeleteTenantRequest struct {
	ID string `json:"id" binding:"required"`
}

// apiDeleteTenant handles the API endpoint for deleting a tenant
func (ws *WebServer) apiDeleteTenant(c *gin.Context) {
	var req DeleteTenantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	err := ws.appManager.DeleteTenant(c.Request.Context(), req.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to delete tenant: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Tenant " + req.ID + " deleted",
	})
}

// ZeroDowntimeDeployRequest represents a request for zero-downtime deployment
type ZeroDowntimeDeployRequest struct {
	Name               string  `json:"name" binding:"required"`
	Image              string  `json:"image" binding:"required"`
	Service            string  `json:"service"`
	BatchSize          *uint32 `json:"batch_size"`
	BatchDelay         *uint64 `json:"batch_delay"`
	HealthCheckPath    string  `json:"health_check_path"`
	HealthCheckPort    *uint16 `json:"health_check_port"`
	HealthCheckTimeout *uint64 `json:"health_check_timeout"`
	DrainTimeout       *uint64 `json:"drain_timeout"`
}

// ZeroDowntimeDeployResponse represents a response for zero-downtime deployment
type ZeroDowntimeDeployResponse struct {
	Success      bool   `json:"success"`
	DeploymentID string `json:"deployment_id,omitempty"`
	Error        string `json:"error,omitempty"`
}

// apiZeroDowntimeDeployHandler handles the API endpoint for zero-downtime deployment
func (ws *WebServer) apiZeroDowntimeDeployHandler(c *gin.Context) {
	var req ZeroDowntimeDeployRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ZeroDowntimeDeployResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	// Check if deployment manager is available
	deploymentID, err := ws.appManager.ZeroDowntimeDeploy(
		c.Request.Context(),
		req.Name,
		req.Image,
		req.Service,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ZeroDowntimeDeployResponse{
			Success: false,
			Error:   "Failed to create deployment: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusAccepted, ZeroDowntimeDeployResponse{
		Success:      true,
		DeploymentID: deploymentID,
	})
}

// DeploymentStatusResponse represents a response for deployment status
type DeploymentStatusResponse struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	Progress  float32 `json:"progress"`
	Details   string  `json:"details,omitempty"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	Completed bool    `json:"completed"`
	Success   bool    `json:"success,omitempty"`
}

// apiDeploymentStatusHandler handles the API endpoint for deployment status
func (ws *WebServer) apiDeploymentStatusHandler(c *gin.Context) {
	deploymentID := c.Param("id")

	status, err := ws.appManager.GetDeploymentStatus(c.Request.Context(), deploymentID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error":   "Failed to get deployment status: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, DeploymentStatusResponse{
		ID:        deploymentID,
		Status:    status.Status,
		Progress:  float32(status.Progress),
		Details:   status.Details,
		CreatedAt: status.CreatedAt.Format(time.RFC3339),
		UpdatedAt: status.UpdatedAt.Format(time.RFC3339),
		Completed: status.Completed,
		Success:   status.Success,
	})
}

// apiDeploymentRollbackHandler handles the API endpoint for deployment rollback
func (ws *WebServer) apiDeploymentRollbackHandler(c *gin.Context) {
	deploymentID := c.Param("id")

	err := ws.appManager.RollbackDeployment(c.Request.Context(), deploymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   "Failed to rollback deployment: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"success": true,
		"message": "Rollback of deployment " + deploymentID + " initiated",
	})
}
