package web

import (
	"fmt"
	"net/http"

	"github.com/ao/hivemind/pkg/api"
	"github.com/gin-gonic/gin"
)

// dashboardHandler handles the dashboard page
func (ws *WebServer) dashboardHandler(c *gin.Context) {
	// Get data for dashboard
	apiContainers := ws.containerManager.GetContainers()

	nodes, err := ws.nodeManager.ListNodes(c.Request.Context())
	if err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Failed to get nodes: %v", err), "/")
		return
	}

	services, err := ws.appManager.ListServices(c.Request.Context())
	if err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Failed to get services: %v", err), "/")
		return
	}

	// Calculate statistics
	runningContainers := 0
	apiContainersList := make([]api.Container, 0, len(apiContainers))
	for _, c := range apiContainers {
		if c.Status == api.ContainerRunning {
			runningContainers++
		}
		apiContainersList = append(apiContainersList, *c)
	}

	// Render template
	c.HTML(http.StatusOK, "dashboard.html", gin.H{
		"containers_count":   len(apiContainers),
		"running_containers": runningContainers,
		"nodes_count":        len(nodes),
		"services_count":     len(services),
		"containers":         apiContainersList,
		"recent_containers":  getRecentContainers(apiContainersList, 5),
		"services":           services,
	})
}

// nodesHandler handles the nodes page
func (ws *WebServer) nodesHandler(c *gin.Context) {
	// Get nodes data
	nodeDetails, err := ws.nodeManager.GetNodeDetails()
	if err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Failed to get node details: %v", err), "/")
		return
	}

	// Get containers per node
	var nodesWithContainers []NodeWithContainers
	for _, node := range nodeDetails {
		containers, err := ws.appManager.GetContainersByNode(c.Request.Context(), node.ID)
		if err != nil {
			ws.logger.Errorf("Failed to get containers for node %s: %v", node.ID, err)
			continue
		}

		runningContainers := 0
		for _, container := range containers {
			if container.Status == api.ContainerRunning {
				runningContainers++
			}
		}

		nodesWithContainers = append(nodesWithContainers, NodeWithContainers{
			ID:                node.ID,
			Address:           node.Address,
			CPUAvailable:      int64(node.Resources.CPUAvailable),
			MemoryAvailable:   int64(node.Resources.MemoryAvailable),
			ContainersRunning: runningContainers,
			Containers:        containers,
		})
	}

	// Render template
	c.HTML(http.StatusOK, "nodes.html", gin.H{
		"nodes": nodesWithContainers,
	})
}

// applicationsHandler handles the applications page
func (ws *WebServer) applicationsHandler(c *gin.Context) {
	// Get services data
	services, err := ws.appManager.ListServices(c.Request.Context())
	if err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Failed to get services: %v", err), "/")
		return
	}

	// Get containers for each service
	var servicesWithDetails []ServiceWithContainers
	for _, service := range services {
		var containers []api.Container
		for _, containerID := range service.ContainerIDs {
			container, err := ws.appManager.GetContainerByID(c.Request.Context(), containerID)
			if err != nil {
				ws.logger.Errorf("Failed to get container %s: %v", containerID, err)
				continue
			}
			containers = append(containers, container)
		}

		servicesWithDetails = append(servicesWithDetails, ServiceWithContainers{
			Name:            service.Name,
			Domain:          service.Domain,
			DesiredReplicas: service.Replicas,
			CurrentReplicas: len(containers),
			Containers:      containers,
		})
	}

	// Render template
	c.HTML(http.StatusOK, "applications.html", gin.H{
		"services": servicesWithDetails,
	})
}

// containersHandler handles the containers page
func (ws *WebServer) containersHandler(c *gin.Context) {
	// Get containers data
	containers := ws.containerManager.GetContainers()

	// Get container stats
	var containersWithStats []ContainerWithStats
	for _, container := range containers {
		stats, err := ws.containerManager.GetContainerStats(c.Request.Context(), container.ID)
		if err != nil {
			ws.logger.Errorf("Failed to get stats for container %s: %v", container.ID, err)
		}

		containersWithStats = append(containersWithStats, ContainerWithStats{
			Container: *container, // Dereference the pointer
			Stats:     stats,
		})
	}

	// Render template
	c.HTML(http.StatusOK, "containers.html", gin.H{
		"containers": containersWithStats,
	})
}

// servicesHandler handles the services page
func (ws *WebServer) servicesHandler(c *gin.Context) {
	// Get services data
	serviceEndpoints := ws.serviceDiscovery.ListServices()

	// Render template
	c.HTML(http.StatusOK, "services.html", gin.H{
		"service_endpoints": serviceEndpoints,
	})
}

// healthHandler handles the health page
func (ws *WebServer) healthHandler(c *gin.Context) {
	// Get containers and their basic status
	containers, err := ws.containerManager.ListContainers(c.Request.Context())
	if err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Failed to get containers: %v", err), "/")
		return
	}

	// Get node health
	nodeHealth, err := ws.nodeManager.CheckHealth()
	if err != nil {
		ws.logger.Errorf("Failed to check node health: %v", err)
		nodeHealth = false
	}

	// Render template
	c.HTML(http.StatusOK, "health.html", gin.H{
		"containers":         containers,
		"node_health":        nodeHealth,
		"has_health_monitor": false, // We don't have a health monitor in the Go implementation yet
	})
}

// volumesHandler handles the volumes page
func (ws *WebServer) volumesHandler(c *gin.Context) {
	// Get volumes
	volumes, err := ws.storageManager.ListVolumes(c.Request.Context())
	if err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Failed to list volumes: %v", err), "/")
		return
	}

	// Render template
	c.HTML(http.StatusOK, "volumes.html", gin.H{
		"volumes": volumes,
	})
}

// tenantsHandler handles the tenants page
func (ws *WebServer) tenantsHandler(c *gin.Context) {
	// Get tenants
	tenants, err := ws.appManager.ListTenants(c.Request.Context())
	if err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Failed to list tenants: %v", err), "/")
		return
	}

	// Render template
	c.HTML(http.StatusOK, "tenants.html", gin.H{
		"tenants": tenants,
	})
}

// deployFormHandler handles the deploy form page
func (ws *WebServer) deployFormHandler(c *gin.Context) {
	// Get available images
	images, err := ws.containerManager.ListImages(c.Request.Context())
	if err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Failed to list images: %v", err), "/")
		return
	}

	// Get available volumes
	volumes, err := ws.storageManager.ListVolumes(c.Request.Context())
	if err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Failed to list volumes: %v", err), "/")
		return
	}

	// Render template
	c.HTML(http.StatusOK, "deploy.html", gin.H{
		"images":  images,
		"volumes": volumes,
	})
}

// deployHandler handles the deploy form submission
func (ws *WebServer) deployHandler(c *gin.Context) {
	// Parse form
	var form DeployFormParams
	if err := c.ShouldBind(&form); err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Invalid form data: %v", err), "/deploy")
		return
	}

	// Deploy the application
	containerID, err := ws.appManager.DeployApp(
		c.Request.Context(),
		form.Image,
		form.Name,
		form.Service,
		nil, // volumes
		nil, // env
	)
	if err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Failed to deploy application: %v", err), "/deploy")
		return
	}

	// Redirect to applications page with success message
	c.Redirect(http.StatusFound, fmt.Sprintf("/applications?success=Deployed application %s with container %s", form.Name, containerID))
}

// scaleFormHandler handles the scale form page
func (ws *WebServer) scaleFormHandler(c *gin.Context) {
	appName := c.Param("app_name")

	// Get the service
	service, err := ws.appManager.GetService(c.Request.Context(), appName)
	if err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Failed to get service: %v", err), "/applications")
		return
	}

	// Render template
	c.HTML(http.StatusOK, "scale.html", gin.H{
		"service":  service,
		"app_name": appName,
	})
}

// scaleHandler handles the scale form submission
func (ws *WebServer) scaleHandler(c *gin.Context) {
	appName := c.Param("app_name")

	// Parse form
	var form ScaleFormParams
	if err := c.ShouldBind(&form); err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Invalid form data: %v", err), fmt.Sprintf("/scale/%s", appName))
		return
	}

	// Scale the application
	err := ws.appManager.ScaleApp(c.Request.Context(), appName, form.Replicas)
	if err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Failed to scale application: %v", err), "/applications")
		return
	}

	// Redirect to applications page with success message
	c.Redirect(http.StatusFound, fmt.Sprintf("/applications?success=Scaled application %s to %d replicas", appName, form.Replicas))
}

// restartHandler handles the restart action
func (ws *WebServer) restartHandler(c *gin.Context) {
	appName := c.Param("app_name")

	// Restart the application
	// Use empty strings for userID and tenantID since we don't have them in the context
	err := ws.appManager.RestartApp(c.Request.Context(), appName, "", "")
	if err != nil {
		ws.renderErrorPage(c, fmt.Sprintf("Failed to restart application: %v", err), "/applications")
		return
	}

	// Redirect to applications page with success message
	c.Redirect(http.StatusFound, fmt.Sprintf("/applications?success=Restarted application %s", appName))
}

// renderErrorPage renders the error page
func (ws *WebServer) renderErrorPage(c *gin.Context, errorMsg, backURL string) {
	c.HTML(http.StatusInternalServerError, "error.html", gin.H{
		"error":    errorMsg,
		"back_url": backURL,
	})
}

// Helper function to get recent containers
func getRecentContainers(containers []api.Container, count int) []api.Container {
	if len(containers) <= count {
		return containers
	}
	return containers[:count]
}

// NodeWithContainers represents a node with its containers
type NodeWithContainers struct {
	ID                string
	Address           string
	CPUAvailable      int64
	MemoryAvailable   int64
	ContainersRunning int
	Containers        []api.Container
}

// ServiceWithContainers represents a service with its containers
type ServiceWithContainers struct {
	Name            string
	Domain          string
	DesiredReplicas int
	CurrentReplicas int
	Containers      []api.Container
}

// ContainerWithStats represents a container with its stats
type ContainerWithStats struct {
	Container api.Container
	Stats     *api.ContainerStats
}

// DeployFormParams represents the parameters for the deploy form
type DeployFormParams struct {
	Name    string `form:"name" binding:"required"`
	Image   string `form:"image" binding:"required"`
	Service string `form:"service"`
}

// ScaleFormParams represents the parameters for the scale form
type ScaleFormParams struct {
	Replicas int `form:"replicas" binding:"required,min=1"`
}
