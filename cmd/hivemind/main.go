package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/app"
	"github.com/ao/hivemind/internal/containerd"
	"github.com/ao/hivemind/internal/docker"
	"github.com/ao/hivemind/internal/membership"
	"github.com/ao/hivemind/internal/node"
	"github.com/ao/hivemind/internal/scheduler"
	"github.com/ao/hivemind/internal/service"
	"github.com/ao/hivemind/internal/storage"
	"github.com/ao/hivemind/internal/web"
)

var (
	// Version is set during build
	Version = "dev"
	// BuildTime is set during build
	BuildTime = "unknown"
	// Default data directory
	defaultDataDir = "/var/lib/hivemind"
)

// HiveMindServer represents the Hivemind server
type HiveMindServer struct {
	containerManager  *containerd.Manager
	nodeManager       *node.Manager
	membershipManager *membership.Manager
	schedulerManager  *scheduler.EnhancedScheduler
	serviceDiscovery  *service.Discovery
	storageManager    *storage.Manager
	appStorageManager *StorageManagerAdapter
	appManager        *app.Manager
	webServer         *web.WebServer
	dataDir           string
	logger            *logrus.Logger
}

// StorageManagerAdapter adapts storage.Manager to app.StorageManager
type StorageManagerAdapter struct {
	manager *storage.Manager
}

// CreateVolume adapts the CreateVolume method
func (a *StorageManagerAdapter) CreateVolume(ctx context.Context, options interface{}) (interface{}, error) {
	if opts, ok := options.(storage.VolumeOptions); ok {
		return a.manager.CreateVolumeWithOptions(ctx, opts)
	}
	return nil, fmt.Errorf("invalid options type: %T", options)
}

// DeleteVolume adapts the DeleteVolume method
func (a *StorageManagerAdapter) DeleteVolume(ctx context.Context, name string) error {
	return a.manager.DeleteVolumeWithContext(ctx, name)
}

// MountVolume adapts the MountVolume method
func (a *StorageManagerAdapter) MountVolume(ctx context.Context, volumeName, containerID, mountPath string) error {
	return a.manager.MountVolume(ctx, volumeName, containerID, mountPath)
}

// UnmountVolume adapts the UnmountVolume method
func (a *StorageManagerAdapter) UnmountVolume(ctx context.Context, volumeName, containerID string) error {
	return a.manager.UnmountVolume(ctx, volumeName, containerID)
}

// ListVolumes adapts the ListVolumes method
func (a *StorageManagerAdapter) ListVolumes(ctx context.Context) ([]interface{}, error) {
	volumes, err := a.manager.ListVolumes(ctx)
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

// CreateVolumeSimple adapts the CreateVolumeSimple method
func (a *StorageManagerAdapter) CreateVolumeSimple(ctx context.Context, name string, size int64) (interface{}, error) {
	return a.manager.CreateVolumeSimple(ctx, name, size)
}

func main() {
	log := logrus.New()
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	// Get data directory from environment variable if set
	envDataDir := os.Getenv("HIVEMIND_DATA_DIR")

	var (
		dataDir string
		webPort uint16 = 4483
		runtime string = "containerd" // Default runtime
	)

	rootCmd := &cobra.Command{
		Use:   "hivemind",
		Short: "Hivemind Container Orchestration System",
		Long: `Hivemind is a container orchestration system designed for
distributed environments with a focus on resilience and scalability.`,
		Run: func(cmd *cobra.Command, args []string) {
			log.Infof("Starting Hivemind %s (built at %s)", Version, BuildTime)
			runServer(log, dataDir, webPort, runtime)
		},
	}

	// Use environment variable as default if set, otherwise use the hardcoded default
	defaultDir := defaultDataDir
	if envDataDir != "" {
		defaultDir = envDataDir
	}

	rootCmd.Flags().StringVar(&dataDir, "data-dir", defaultDir, "Data directory (can also be set via HIVEMIND_DATA_DIR env var)")
	rootCmd.Flags().Uint16Var(&webPort, "web-port", webPort, "Web UI port")
	rootCmd.Flags().StringVar(&runtime, "runtime", runtime, "Container runtime to use (containerd or docker)")

	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the version number",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("Hivemind %s (built at %s)\n", Version, BuildTime)
		},
	})

	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("Failed to execute command: %v", err)
	}
}

func runServer(log *logrus.Logger, dataDir string, webPort uint16, runtime string) {
	// Create context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create server instance
	server, err := createServer(ctx, log, dataDir, webPort, runtime)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	// Set up signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	log.Info("Hivemind server is running. Press Ctrl+C to stop.")

	// Wait for termination signal
	sig := <-sigCh
	log.Infof("Received signal %v, shutting down...", sig)

	// Cancel context to signal shutdown to all components
	cancel()

	// Perform cleanup and graceful shutdown
	if err := shutdownServer(server); err != nil {
		log.Errorf("Error during shutdown: %v", err)
	}

	log.Info("Shutdown complete")
}

func createServer(ctx context.Context, log *logrus.Logger, dataDir string, webPort uint16, runtime string) (*HiveMindServer, error) {
	// Create data directory if it doesn't exist
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	// Create server instance
	server := &HiveMindServer{
		dataDir: dataDir,
		logger:  log,
	}

	// Initialize Container Manager based on runtime selection
	var containerManager *containerd.Manager
	var err error

	switch runtime {
	case "docker":
		log.Info("Using Docker runtime")
		// Create a Docker manager
		dockerManager := docker.NewManager(log)

		// Create a containerd.Manager without connecting to containerd
		containerManager = containerd.NewManagerWithoutClient(log)

		// Set the Docker runtime as the container runtime
		containerManager.WithRuntime(dockerManager)
	case "containerd", "":
		log.Info("Using containerd runtime")
		containerManager, err = containerd.NewManager("/run/containerd/containerd.sock", log)
		if err != nil {
			return nil, fmt.Errorf("failed to create container manager: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported runtime: %s (supported: containerd, docker)", runtime)
	}

	server.containerManager = containerManager

	// Initialize Node Manager
	nodeManager, err := node.NewManager()
	if err != nil {
		return nil, fmt.Errorf("failed to create node manager: %w", err)
	}
	server.nodeManager = nodeManager

	// Initialize Membership Manager
	membershipManager, err := membership.NewManager("hivemind-node", "0.0.0.0", 7946, log)
	if err != nil {
		return nil, fmt.Errorf("failed to create membership manager: %w", err)
	}
	server.membershipManager = membershipManager

	// Initialize Scheduler
	schedulerManager, err := scheduler.NewEnhancedScheduler(log)
	if err != nil {
		return nil, fmt.Errorf("failed to create scheduler: %w", err)
	}
	server.schedulerManager = schedulerManager

	// Initialize Service Discovery
	serviceDiscovery, err := service.NewDiscovery(log)
	if err != nil {
		return nil, fmt.Errorf("failed to create service discovery: %w", err)
	}
	server.serviceDiscovery = serviceDiscovery

	// Initialize Storage Manager
	storageDir := filepath.Join(dataDir, "storage")
	storageManager, err := storage.NewManager(storageDir, log)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage manager: %w", err)
	}
	server.storageManager = storageManager

	// Initialize App Manager
	appDir := filepath.Join(dataDir, "apps")
	appManager, err := app.NewManager(appDir, log)
	if err != nil {
		return nil, fmt.Errorf("failed to create app manager: %w", err)
	}
	server.appManager = appManager

	// Connect components
	if err := connectComponents(server); err != nil {
		return nil, fmt.Errorf("failed to connect components: %w", err)
	}

	// Initialize Web Server
	webServer, err := web.NewWebServer(
		appManager,
		nodeManager,
		containerManager,
		serviceDiscovery,
		storageManager,
		membershipManager,
		log,
		webPort,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create web server: %w", err)
	}
	server.webServer = webServer

	// Start health monitoring
	startHealthMonitoring(ctx, server)

	// Start web server
	if err := webServer.Start(); err != nil {
		return nil, fmt.Errorf("failed to start web server: %w", err)
	}

	return server, nil
}

func connectComponents(server *HiveMindServer) error {
	// Connect Node Manager with Membership Manager
	server.nodeManager.WithMembershipManager(server.membershipManager)

	// Connect Scheduler with Node Manager
	server.schedulerManager.WithNodeManager(server.nodeManager)

	// Connect App Manager with Container Manager and Storage Manager
	server.appManager.WithContainerManager(server.containerManager)

	// Create and use the storage manager adapter
	server.appStorageManager = &StorageManagerAdapter{manager: server.storageManager}
	server.appManager.WithStorageManager(server.appStorageManager)

	// Connect Service Discovery with Node Manager
	server.serviceDiscovery.WithNodeManager(server.nodeManager)

	// Connect Container Manager with Storage Manager
	// This is a placeholder as we don't have a direct connection method in the current implementation
	// server.containerManager.WithStorageManager(server.storageManager)

	return nil
}

func startHealthMonitoring(ctx context.Context, server *HiveMindServer) {
	// Start node health monitoring
	server.nodeManager.StartHealthMonitoring(ctx, 30*time.Second)

	// Start membership health monitoring
	server.membershipManager.StartHealthMonitoring(ctx, 10*time.Second)

	// Start volume health monitoring
	server.storageManager.StartVolumeHealthMonitoring(ctx, 60*time.Second)

	// Start app health monitoring
	server.appManager.StartAppHealthMonitoring(ctx, 20*time.Second)
}

func shutdownServer(server *HiveMindServer) error {
	// Stop Web Server
	if server.webServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := server.webServer.Stop(ctx); err != nil {
			server.logger.Errorf("Failed to stop web server: %v", err)
		}
	}
	// Close App Manager
	if server.appManager != nil {
		if err := server.appManager.Close(); err != nil {
			server.logger.Errorf("Failed to close app manager: %v", err)
		}
	}

	// Close Storage Manager
	if server.storageManager != nil {
		if err := server.storageManager.Close(); err != nil {
			server.logger.Errorf("Failed to close storage manager: %v", err)
		}
	}

	// Close Service Discovery
	if server.serviceDiscovery != nil {
		if err := server.serviceDiscovery.Close(); err != nil {
			server.logger.Errorf("Failed to close service discovery: %v", err)
		}
	}

	// Close Scheduler
	if server.schedulerManager != nil {
		if err := server.schedulerManager.Close(); err != nil {
			server.logger.Errorf("Failed to close scheduler: %v", err)
		}
	}

	// Close Membership Manager
	if server.membershipManager != nil {
		if err := server.membershipManager.Close(); err != nil {
			server.logger.Errorf("Failed to close membership manager: %v", err)
		}
	}

	// Close Node Manager
	if server.nodeManager != nil {
		if err := server.nodeManager.Close(); err != nil {
			server.logger.Errorf("Failed to close node manager: %v", err)
		}
	}

	// Close Container Manager
	if server.containerManager != nil {
		if err := server.containerManager.Close(); err != nil {
			server.logger.Errorf("Failed to close container manager: %v", err)
		}
	}

	return nil
}
