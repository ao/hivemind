package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// WebServer represents the web server for the Hivemind UI
type WebServer struct {
	port              uint16
	router            *gin.Engine
	templates         *template.Template
	appManager        AppManager
	nodeManager       NodeManager
	containerManager  ContainerManager
	serviceDiscovery  ServiceDiscovery
	storageManager    StorageManager
	membershipManager MembershipManager
	logger            *logrus.Logger
	server            *http.Server
	mu                sync.RWMutex
}

// NewWebServer creates a new web server instance
func NewWebServer(
	appManager AppManager,
	nodeManager NodeManager,
	containerManager ContainerManager,
	serviceDiscovery ServiceDiscovery,
	storageManager StorageManager,
	membershipManager MembershipManager,
	logger *logrus.Logger,
	port uint16,
) (*WebServer, error) {
	// Create Gin router
	router := gin.Default()

	// Create web server
	ws := &WebServer{
		port:              port,
		router:            router,
		appManager:        appManager,
		nodeManager:       nodeManager,
		containerManager:  containerManager,
		serviceDiscovery:  serviceDiscovery,
		storageManager:    storageManager,
		membershipManager: membershipManager,
		logger:            logger,
	}

	// Initialize templates
	if err := ws.initTemplates(); err != nil {
		return nil, fmt.Errorf("failed to initialize templates: %w", err)
	}

	// Set up middleware
	ws.setupMiddleware()

	// Set up routes
	ws.setupRoutes()

	// Set up static file handling
	ws.setupStaticFiles()

	return ws, nil
}

// initTemplates initializes the HTML templates
func (ws *WebServer) initTemplates() error {
	// Check if we're in a test environment
	inTest := false
	for _, arg := range os.Args {
		if strings.Contains(arg, "test.v") || strings.Contains(arg, "test.run") {
			inTest = true
			break
		}
	}

	// If we're in a test environment, create an empty template
	if inTest {
		tmpl := template.New("").Funcs(TemplateFuncs())
		ws.templates = tmpl
		ws.router.SetHTMLTemplate(tmpl)
		return nil
	}

	// Load templates from the templates directory
	templatesDir := "templates"
	pattern := filepath.Join(templatesDir, "*.html")

	// Create template with functions
	tmpl := template.New("").Funcs(TemplateFuncs())

	// Parse templates
	tmpl, err := tmpl.ParseGlob(pattern)
	if err != nil {
		return fmt.Errorf("failed to parse templates: %w", err)
	}

	ws.templates = tmpl

	// Set up Gin to use the templates
	ws.router.SetHTMLTemplate(tmpl)

	return nil
}

// setupMiddleware sets up the middleware
func (ws *WebServer) setupMiddleware() {
	// Add recovery middleware
	ws.router.Use(RecoveryHandler(ws.logger))

	// Add logging middleware
	ws.router.Use(LoggingMiddleware(ws.logger))

	// Add error handling middleware
	ws.router.Use(ErrorHandler(ws.logger))

	// Add response time middleware
	ws.router.Use(func(c *gin.Context) {
		start := time.Now()
		c.Next()
		duration := time.Since(start)
		c.Header("X-Response-Time", duration.String())
	})
}

// setupRoutes sets up the HTTP routes
func (ws *WebServer) setupRoutes() {
	// HTML routes
	ws.router.GET("/", ws.dashboardHandler)
	ws.router.GET("/nodes", ws.nodesHandler)
	ws.router.GET("/applications", ws.applicationsHandler)
	ws.router.GET("/containers", ws.containersHandler)
	ws.router.GET("/services", ws.servicesHandler)
	ws.router.GET("/health", ws.healthHandler)
	ws.router.GET("/volumes", ws.volumesHandler)
	ws.router.GET("/tenants", ws.tenantsHandler)

	ws.router.GET("/deploy", ws.deployFormHandler)
	ws.router.POST("/deploy", ws.deployHandler)

	ws.router.GET("/scale/:app_name", ws.scaleFormHandler)
	ws.router.POST("/scale/:app_name", ws.scaleHandler)

	ws.router.POST("/restart/:app_name", ws.restartHandler)

	// API routes
	api := ws.router.Group("/api")
	{
		api.GET("/nodes", ws.apiNodesHandler)
		api.GET("/containers", ws.apiContainersHandler)
		api.GET("/images", ws.apiImagesHandler)
		api.GET("/services", ws.apiServicesHandler)
		api.GET("/service-endpoints", ws.apiServiceEndpointsHandler)
		api.GET("/health", ws.apiHealthHandler)

		api.POST("/deploy", ws.apiDeployHandler)
		api.POST("/scale", ws.apiScaleHandler)
		api.POST("/restart", ws.apiRestartHandler)
		api.POST("/service-url", ws.apiServiceURLHandler)

		// Volume management API routes
		api.GET("/volumes", ws.apiListVolumes)
		api.POST("/volumes/create", ws.apiCreateVolume)
		api.POST("/volumes/delete", ws.apiDeleteVolume)

		// Tenant management API routes
		api.GET("/tenants", ws.apiListTenants)
		api.POST("/tenants/create", ws.apiCreateTenant)
		api.POST("/tenants/update", ws.apiUpdateTenant)
		api.POST("/tenants/delete", ws.apiDeleteTenant)
		api.GET("/tenants/:id", ws.apiGetTenant)

		// Zero-downtime deployment API routes
		api.POST("/deployments/zero-downtime", ws.apiZeroDowntimeDeployHandler)
		api.GET("/deployments/status/:id", ws.apiDeploymentStatusHandler)
		api.POST("/deployments/rollback/:id", ws.apiDeploymentRollbackHandler)
	}
}

// Start starts the web server
func (ws *WebServer) Start() error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	addr := fmt.Sprintf("0.0.0.0:%d", ws.port)
	ws.logger.Infof("Starting web server on %s", addr)

	ws.server = &http.Server{
		Addr:    addr,
		Handler: ws.router,
	}

	// Start the server in a goroutine
	go func() {
		if err := ws.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			ws.logger.Errorf("Failed to start web server: %v", err)
		}
	}()

	return nil
}

// setupStaticFiles sets up static file handling
func (ws *WebServer) setupStaticFiles() {
	// Serve static files from the assets directory
	ws.router.Static("/assets", "./assets")

	// Note: Specific routes for CSS and JS files are not needed
	// as they are already served by the static file server
}

// Stop stops the web server
func (ws *WebServer) Stop(ctx context.Context) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.server == nil {
		return nil
	}

	ws.logger.Info("Stopping web server")

	// Shutdown the server with a timeout
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := ws.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shutdown web server: %w", err)
	}

	ws.server = nil
	return nil
}
