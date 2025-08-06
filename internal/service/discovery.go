package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ao/hivemind/pkg/api"
	"github.com/sirupsen/logrus"
)

// ServiceHealth represents the health status of a service
type ServiceHealth string

const (
	// ServiceHealthy indicates the service is healthy
	ServiceHealthy ServiceHealth = "healthy"
	// ServiceUnhealthy indicates the service is unhealthy
	ServiceUnhealthy ServiceHealth = "unhealthy"
	// ServiceUnknown indicates the service health is unknown
	ServiceUnknown ServiceHealth = "unknown"
)

// ServiceEndpoint represents an endpoint for a service
type ServiceEndpoint struct {
	ServiceName     string
	Domain          string
	IPAddress       string
	Port            uint16
	NodeID          string
	HealthStatus    ServiceHealth
	LastHealthCheck int64
	Version         string
	Weight          uint32
	Metadata        map[string]string
}

// HealthCheckConfig represents the configuration for health checks
type HealthCheckConfig struct {
	Protocol           HealthCheckProtocol
	Path               string // For HTTP health checks
	IntervalSeconds    uint64
	TimeoutSeconds     uint64
	HealthyThreshold   uint32
	UnhealthyThreshold uint32
}

// HealthCheckProtocol represents the protocol for health checks
type HealthCheckProtocol string

const (
	// HealthCheckHTTP indicates HTTP health checks
	HealthCheckHTTP HealthCheckProtocol = "http"
	// HealthCheckHTTPS indicates HTTPS health checks
	HealthCheckHTTPS HealthCheckProtocol = "https"
	// HealthCheckTCP indicates TCP health checks
	HealthCheckTCP HealthCheckProtocol = "tcp"
	// HealthCheckCommand indicates command health checks
	HealthCheckCommand HealthCheckProtocol = "command"
)

// LoadBalancingStrategy represents the strategy for load balancing
type LoadBalancingStrategy string

const (
	// LoadBalancingRoundRobin indicates round-robin load balancing
	LoadBalancingRoundRobin LoadBalancingStrategy = "round_robin"
	// LoadBalancingWeightedRoundRobin indicates weighted round-robin load balancing
	LoadBalancingWeightedRoundRobin LoadBalancingStrategy = "weighted_round_robin"
	// LoadBalancingLeastConnections indicates least connections load balancing
	LoadBalancingLeastConnections LoadBalancingStrategy = "least_connections"
	// LoadBalancingRandom indicates random load balancing
	LoadBalancingRandom LoadBalancingStrategy = "random"
	// LoadBalancingIPHash indicates IP hash load balancing
	LoadBalancingIPHash LoadBalancingStrategy = "ip_hash"
)

// RoutingRule represents a routing rule for a service
type RoutingRule struct {
	PathPrefix  string
	Headers     map[string]string
	ServiceName string
	Weight      uint32
}

// TrafficSplitConfig represents a traffic split configuration
type TrafficSplitConfig struct {
	ServiceName string
	Splits      []TrafficSplit
}

// TrafficSplit represents a traffic split
type TrafficSplit struct {
	Version string
	Weight  uint32
}

// ServiceMeshConfig represents a service mesh configuration
type ServiceMeshConfig struct {
	Enabled        bool
	MTLSEnabled    bool
	TracingEnabled bool
	RetryPolicy    *RetryPolicy
	TimeoutMS      uint64
	CircuitBreaker *CircuitBreakerPolicy
}

// RetryPolicy represents a retry policy
type RetryPolicy struct {
	MaxRetries uint32
	RetryOn    []string
	TimeoutMS  uint64
}

// CircuitBreakerPolicy represents a circuit breaker policy
type CircuitBreakerPolicy struct {
	MaxConnections             uint32
	MaxPendingRequests         uint32
	MaxRequests                uint32
	MaxRetries                 uint32
	ConsecutiveErrorsThreshold uint32
	IntervalMS                 uint64
	BaseEjectionTimeMS         uint64
	State                      CircuitBreakerState
	LastStateChange            int64
	FailureCount               uint32
	SuccessCount               uint32
	HalfOpenAllowedCalls       uint32
}

// CircuitBreakerState represents the state of a circuit breaker
type CircuitBreakerState string

const (
	// CircuitBreakerClosed indicates the circuit breaker is closed (normal operation)
	CircuitBreakerClosed CircuitBreakerState = "closed"
	// CircuitBreakerOpen indicates the circuit breaker is open (requests are rejected)
	CircuitBreakerOpen CircuitBreakerState = "open"
	// CircuitBreakerHalfOpen indicates the circuit breaker is half-open (testing if service is healthy)
	CircuitBreakerHalfOpen CircuitBreakerState = "half_open"
)

// EndpointStats represents statistics for an endpoint
type EndpointStats struct {
	ActiveConnections uint32
	TotalRequests     uint64
	FailedRequests    uint64
	AvgResponseTimeMS float64
	LastSelected      int64
}

// Discovery handles service discovery and health checking
type Discovery struct {
	services              map[string][]ServiceEndpoint
	servicesMutex         sync.RWMutex
	dnsPort               uint16
	proxyPort             uint16
	healthCheckConfigs    map[string]HealthCheckConfig
	healthCheckMutex      sync.RWMutex
	loadBalancingStrategy map[string]LoadBalancingStrategy
	strategyMutex         sync.RWMutex
	endpointStats         map[string]EndpointStats
	statsMutex            sync.RWMutex
	networkManager        interface{} // Will be replaced with actual NetworkManager
	networkMutex          sync.RWMutex
	routingRules          []RoutingRule
	routingMutex          sync.RWMutex
	trafficSplits         map[string]TrafficSplitConfig
	splitsMutex           sync.RWMutex
	logger                *logrus.Logger
}

// NewDiscovery creates a new service discovery manager
func NewDiscovery(logger *logrus.Logger) (*Discovery, error) {
	discovery := &Discovery{
		services:              make(map[string][]ServiceEndpoint),
		dnsPort:               53,   // Standard DNS port
		proxyPort:             4483, // Default proxy port (spells "HIVE")
		healthCheckConfigs:    make(map[string]HealthCheckConfig),
		loadBalancingStrategy: make(map[string]LoadBalancingStrategy),
		endpointStats:         make(map[string]EndpointStats),
		routingRules:          make([]RoutingRule, 0),
		trafficSplits:         make(map[string]TrafficSplitConfig),
		logger:                logger,
	}
	return discovery, nil
}

// WithNetworkManager sets the network manager
func (d *Discovery) WithNetworkManager(networkManager interface{}) *Discovery {
	d.networkMutex.Lock()
	defer d.networkMutex.Unlock()
	d.networkManager = networkManager
	return d
}

// WithNodeManager sets the node manager
func (d *Discovery) WithNodeManager(nodeManager interface{}) *Discovery {
	d.networkMutex.Lock()
	defer d.networkMutex.Unlock()
	// Store the node manager in the networkManager field
	// This is intentional as the service discovery uses the node manager
	// as a network manager for service discovery purposes
	d.networkManager = nodeManager
	d.logger.Info("Node manager set for service discovery")
	return d
}

// Initialize initializes the service discovery system
func (d *Discovery) Initialize() error {
	d.logger.Info("Initializing service discovery system")

	// Start the health check system
	go d.startHealthCheckSystem()

	// Start the DNS server
	go d.startDNSServer()

	// Start the proxy server
	go d.startProxyServer()

	// If we have a network manager, set up the integration
	d.networkMutex.RLock()
	networkManager := d.networkManager
	d.networkMutex.RUnlock()

	if networkManager != nil {
		d.setupNetworkIntegration()
	}

	return nil
}

// setupNetworkIntegration sets up integration with the network manager
func (d *Discovery) setupNetworkIntegration() {
	d.logger.Info("Setting up network integration for service discovery")

	// Register for network events
	d.registerNetworkEvents()

	// Sync existing services with network
	d.syncServicesWithNetwork()
}

// registerNetworkEvents registers for network events
func (d *Discovery) registerNetworkEvents() {
	// In a real implementation, this would set up event handlers for network changes
	d.logger.Info("Registered for network events")
}

// syncServicesWithNetwork syncs existing services with the network
func (d *Discovery) syncServicesWithNetwork() {
	d.logger.Info("Syncing services with network")

	// Get all services
	d.servicesMutex.RLock()
	defer d.servicesMutex.RUnlock()

	// For each service, ensure network connectivity
	for serviceName, endpoints := range d.services {
		d.logger.Infof("Syncing service %s with network", serviceName)

		for _, endpoint := range endpoints {
			// Ensure network connectivity for this endpoint
			d.networkMutex.RLock()
			networkManager := d.networkManager
			d.networkMutex.RUnlock()

			if networkManager != nil {
				// In a real implementation, this would ensure network routes exist
				d.logger.Infof(
					"Ensuring network connectivity for endpoint %s (%s:%d)",
					endpoint.NodeID, endpoint.IPAddress, endpoint.Port,
				)
			}
		}
	}
}

// HandleContainerCreated handles a new container being created
func (d *Discovery) HandleContainerCreated(
	containerID string,
	serviceName string,
	domain string,
	nodeID string,
	ipAddress string,
	port uint16,
) error {
	d.logger.Infof(
		"Handling container creation for service %s: %s on node %s",
		serviceName, containerID, nodeID,
	)

	// Register the service endpoint
	serviceConfig := &api.ServiceConfig{
		Name:            serviceName,
		Domain:          domain,
		ContainerIDs:    []string{containerID},
		DesiredReplicas: 1,
		CurrentReplicas: 1,
	}

	if err := d.RegisterService(serviceConfig, nodeID, ipAddress, port); err != nil {
		return err
	}

	// Set up default health check
	healthCheckConfig := HealthCheckConfig{
		Protocol:           HealthCheckTCP,
		IntervalSeconds:    30,
		TimeoutSeconds:     5,
		HealthyThreshold:   2,
		UnhealthyThreshold: 3,
	}

	if err := d.ConfigureHealthCheck(serviceName, healthCheckConfig); err != nil {
		return err
	}

	// Set up default load balancing strategy
	if err := d.ConfigureLoadBalancing(serviceName, LoadBalancingRoundRobin); err != nil {
		return err
	}

	return nil
}

// HandleContainerRemoved handles a container being removed
func (d *Discovery) HandleContainerRemoved(
	containerID string,
	serviceName string,
	nodeID string,
	ipAddress string,
	port uint16,
) error {
	d.logger.Infof(
		"Handling container removal for service %s: %s on node %s",
		serviceName, containerID, nodeID,
	)

	// Deregister the service endpoint
	return d.DeregisterService(serviceName, nodeID, ipAddress, port)
}

// HandleNodeJoined handles a node joining the cluster
func (d *Discovery) HandleNodeJoined(nodeID string, nodeIP string) error {
	d.logger.Infof("Handling node join: %s (%s)", nodeID, nodeIP)

	// In a real implementation, this would update routing information

	return nil
}

// HandleNodeLeft handles a node leaving the cluster
func (d *Discovery) HandleNodeLeft(nodeID string) error {
	d.logger.Infof("Handling node departure: %s", nodeID)

	// Remove all services on this node
	d.servicesMutex.Lock()
	defer d.servicesMutex.Unlock()

	// For each service, remove endpoints on this node
	for serviceName, endpoints := range d.services {
		// Remove endpoints on this node
		newEndpoints := make([]ServiceEndpoint, 0, len(endpoints))
		for _, endpoint := range endpoints {
			if endpoint.NodeID != nodeID {
				newEndpoints = append(newEndpoints, endpoint)
			}
		}

		if len(newEndpoints) < len(endpoints) {
			d.logger.Infof(
				"Removed endpoints on node %s for service %s",
				nodeID, serviceName,
			)
			d.services[serviceName] = newEndpoints
		}
	}

	// Remove empty services
	for serviceName, endpoints := range d.services {
		if len(endpoints) == 0 {
			delete(d.services, serviceName)
		}
	}

	return nil
}

// RegisterService registers a service endpoint
func (d *Discovery) RegisterService(
	serviceConfig *api.ServiceConfig,
	nodeID string,
	ipAddress string,
	port uint16,
) error {
	now := time.Now().Unix()

	endpoint := ServiceEndpoint{
		ServiceName:     serviceConfig.Name,
		Domain:          serviceConfig.Domain,
		IPAddress:       ipAddress,
		Port:            port,
		NodeID:          nodeID,
		HealthStatus:    ServiceUnknown,
		LastHealthCheck: now,
		Version:         "",
		Weight:          0,
		Metadata:        make(map[string]string),
	}

	d.servicesMutex.Lock()
	defer d.servicesMutex.Unlock()

	if endpoints, exists := d.services[serviceConfig.Name]; exists {
		// Check if this endpoint already exists
		exists := false
		for _, e := range endpoints {
			if e.NodeID == nodeID && e.IPAddress == ipAddress && e.Port == port {
				exists = true
				break
			}
		}

		if !exists {
			d.services[serviceConfig.Name] = append(endpoints, endpoint)
			d.logger.Infof(
				"Registered new endpoint for service %s: %s:%d",
				serviceConfig.Name, ipAddress, port,
			)
		}
	} else {
		// First endpoint for this service
		d.services[serviceConfig.Name] = []ServiceEndpoint{endpoint}
		d.logger.Infof(
			"Registered new service %s with endpoint %s:%d",
			serviceConfig.Name, ipAddress, port,
		)
	}

	return nil
}

// DeregisterService deregisters a service endpoint
func (d *Discovery) DeregisterService(
	serviceName string,
	nodeID string,
	ipAddress string,
	port uint16,
) error {
	d.servicesMutex.Lock()
	defer d.servicesMutex.Unlock()

	if endpoints, exists := d.services[serviceName]; exists {
		// Remove matching endpoints
		newEndpoints := make([]ServiceEndpoint, 0, len(endpoints))
		for _, endpoint := range endpoints {
			if !(endpoint.NodeID == nodeID && endpoint.IPAddress == ipAddress && endpoint.Port == port) {
				newEndpoints = append(newEndpoints, endpoint)
			}
		}

		// If no endpoints left, remove the service
		if len(newEndpoints) == 0 {
			delete(d.services, serviceName)
			d.logger.Infof("Removed service %s as it has no endpoints", serviceName)
		} else {
			d.services[serviceName] = newEndpoints
			d.logger.Infof(
				"Deregistered endpoint %s:%d for service %s",
				ipAddress, port, serviceName,
			)
		}
	}

	return nil
}

// GetServiceEndpoints returns the endpoints for a service
func (d *Discovery) GetServiceEndpoints(serviceName string) []ServiceEndpoint {
	d.servicesMutex.RLock()
	defer d.servicesMutex.RUnlock()

	if endpoints, exists := d.services[serviceName]; exists {
		return endpoints
	}

	return nil
}

// GetServiceByDomain returns a service by domain
func (d *Discovery) GetServiceByDomain(domain string) (string, []ServiceEndpoint) {
	d.servicesMutex.RLock()
	defer d.servicesMutex.RUnlock()

	for serviceName, endpoints := range d.services {
		// Check if any endpoint matches this domain
		for _, endpoint := range endpoints {
			if endpoint.Domain == domain {
				return serviceName, endpoints
			}
		}
	}

	return "", nil
}

// ListServices returns a list of all services
func (d *Discovery) ListServices() map[string][]ServiceEndpoint {
	d.servicesMutex.RLock()
	defer d.servicesMutex.RUnlock()

	// Create a copy to avoid race conditions
	services := make(map[string][]ServiceEndpoint)
	for serviceName, endpoints := range d.services {
		endpointsCopy := make([]ServiceEndpoint, len(endpoints))
		copy(endpointsCopy, endpoints)
		services[serviceName] = endpointsCopy
	}

	return services
}

// startDNSServer starts the DNS server
func (d *Discovery) startDNSServer() {
	d.logger.Infof("Starting DNS server on port %d", d.dnsPort)

	// Create UDP server
	addr := fmt.Sprintf(":%d", d.dnsPort)
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		d.logger.Errorf("Failed to resolve UDP address: %v", err)
		return
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		d.logger.Errorf("Failed to listen on UDP: %v", err)
		return
	}
	defer conn.Close()

	// Buffer for DNS queries
	buffer := make([]byte, 512)

	for {
		// Read query
		n, clientAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			d.logger.Errorf("Error reading from UDP: %v", err)
			continue
		}

		// Handle query in a goroutine
		go func(query []byte, addr *net.UDPAddr) {
			// In a real implementation, we would parse and respond to DNS queries
			// For now, we'll just log that we received a query
			d.logger.Debugf("Received DNS query from %s", addr.String())

			// Send a dummy response
			response := []byte{0, 0, 0x81, 0x80, 0, 1, 0, 1, 0, 0, 0, 0}
			if _, err := conn.WriteToUDP(response, addr); err != nil {
				d.logger.Errorf("Error sending DNS response: %v", err)
			}
		}(buffer[:n], clientAddr)
	}
}

// startProxyServer starts the proxy server
func (d *Discovery) startProxyServer() {
	d.logger.Infof("Starting proxy server on port %d", d.proxyPort)

	// Create HTTP server
	server := &http.Server{
		Addr: fmt.Sprintf(":%d", d.proxyPort),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract host from request
			host := r.Host

			// Find service by domain
			serviceName, endpoints := d.GetServiceByDomain(host)
			if serviceName == "" || len(endpoints) == 0 {
				http.Error(w, "Service not found", http.StatusNotFound)
				return
			}

			// Get load balancing strategy
			d.strategyMutex.RLock()
			strategy, exists := d.loadBalancingStrategy[serviceName]
			if !exists {
				strategy = LoadBalancingRoundRobin
			}
			d.strategyMutex.RUnlock()

			// Select endpoint based on strategy
			var selectedEndpoint *ServiceEndpoint

			// Filter healthy endpoints
			healthyEndpoints := make([]ServiceEndpoint, 0)
			for _, endpoint := range endpoints {
				if endpoint.HealthStatus == ServiceHealthy {
					healthyEndpoints = append(healthyEndpoints, endpoint)
				}
			}

			// If no healthy endpoints, use all endpoints
			if len(healthyEndpoints) == 0 {
				healthyEndpoints = endpoints
			}

			// Select endpoint based on strategy
			switch strategy {
			case LoadBalancingRoundRobin:
				// Simple round-robin
				index := rand.Intn(len(healthyEndpoints))
				selectedEndpoint = &healthyEndpoints[index]

			case LoadBalancingWeightedRoundRobin:
				// Weighted round-robin
				totalWeight := uint32(0)
				for _, endpoint := range healthyEndpoints {
					totalWeight += endpoint.Weight
				}

				if totalWeight == 0 {
					// If no weights, use simple round-robin
					index := rand.Intn(len(healthyEndpoints))
					selectedEndpoint = &healthyEndpoints[index]
				} else {
					// Select based on weight
					randomWeight := rand.Uint32() % totalWeight
					currentWeight := uint32(0)

					for i, endpoint := range healthyEndpoints {
						currentWeight += endpoint.Weight
						if currentWeight >= randomWeight {
							selectedEndpoint = &healthyEndpoints[i]
							break
						}
					}
				}

			case LoadBalancingRandom:
				// Random selection
				index := rand.Intn(len(healthyEndpoints))
				selectedEndpoint = &healthyEndpoints[index]

			case LoadBalancingIPHash:
				// IP hash
				clientIP := r.RemoteAddr
				hash := uint32(0)
				for _, c := range clientIP {
					hash = hash*31 + uint32(c)
				}
				index := int(hash % uint32(len(healthyEndpoints)))
				selectedEndpoint = &healthyEndpoints[index]

			default:
				// Default to round-robin
				index := rand.Intn(len(healthyEndpoints))
				selectedEndpoint = &healthyEndpoints[index]
			}

			if selectedEndpoint == nil {
				http.Error(w, "No endpoints available", http.StatusServiceUnavailable)
				return
			}

			// Update endpoint stats
			d.updateEndpointStats(selectedEndpoint.ServiceName, selectedEndpoint.IPAddress, selectedEndpoint.Port)

			// Proxy the request
			targetURL := fmt.Sprintf("http://%s:%d%s", selectedEndpoint.IPAddress, selectedEndpoint.Port, r.URL.Path)
			d.logger.Debugf("Proxying request to %s", targetURL)

			// In a real implementation, we would proxy the request to the target
			// For now, we'll just return a dummy response
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"service":  serviceName,
				"endpoint": fmt.Sprintf("%s:%d", selectedEndpoint.IPAddress, selectedEndpoint.Port),
				"status":   "proxied",
			})
		}),
	}

	// Start the server
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		d.logger.Errorf("Proxy server error: %v", err)
	}
}

// updateEndpointStatsLegacy is kept for reference but not used
// Use the implementation in discovery_methods.go instead
func (d *Discovery) updateEndpointStatsLegacy(serviceName, ipAddress string, port uint16) {
	// This method is deprecated and should not be used
}

// startHealthCheckSystem starts the health check system
func (d *Discovery) startHealthCheckSystem() {
	d.logger.Info("Starting health check system")

	// Run health checks periodically
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			d.runHealthChecks()
		}
	}
}

// runHealthChecks runs health checks for all services
func (d *Discovery) runHealthChecks() {
	d.servicesMutex.RLock()
	services := make(map[string][]ServiceEndpoint)
	for serviceName, endpoints := range d.services {
		endpointsCopy := make([]ServiceEndpoint, len(endpoints))
		copy(endpointsCopy, endpoints)
		services[serviceName] = endpointsCopy
	}
	d.servicesMutex.RUnlock()

	d.healthCheckMutex.RLock()
	configs := make(map[string]HealthCheckConfig)
	for serviceName, config := range d.healthCheckConfigs {
		configs[serviceName] = config
	}
	d.healthCheckMutex.RUnlock()

	// Run health checks for each service
	for serviceName, endpoints := range services {
		config, exists := configs[serviceName]
		if !exists {
			// Use default config
			config = HealthCheckConfig{
				Protocol:           HealthCheckTCP,
				IntervalSeconds:    30,
				TimeoutSeconds:     5,
				HealthyThreshold:   2,
				UnhealthyThreshold: 3,
			}
		}

		// Run health checks for each endpoint
		for _, endpoint := range endpoints {
			go func(endpoint ServiceEndpoint, config HealthCheckConfig) {
				// Run health check
				var health ServiceHealth

				switch config.Protocol {
				case HealthCheckHTTP:
					health = d.checkHTTPHealth(endpoint, config)
				case HealthCheckHTTPS:
					health = d.checkHTTPSHealth(endpoint, config)
				case HealthCheckTCP:
					health = d.checkTCPHealth(endpoint, config)
				case HealthCheckCommand:
					health = ServiceUnknown // Not implemented
				default:
					health = ServiceUnknown
				}

				// Update endpoint health
				d.updateEndpointHealth(endpoint.ServiceName, endpoint.IPAddress, endpoint.Port, health)
			}(endpoint, config)
		}
	}
}

// checkHTTPHealth checks the health of an endpoint using HTTP
func (d *Discovery) checkHTTPHealth(endpoint ServiceEndpoint, config HealthCheckConfig) ServiceHealth {
	// Build URL
	path := "/"
	if config.Path != "" {
		path = config.Path
	}
	url := fmt.Sprintf("http://%s:%d%s", endpoint.IPAddress, endpoint.Port, path)

	// Set timeout
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.TimeoutSeconds)*time.Second)
	defer cancel()

	// Create request
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		d.logger.Errorf("Failed to create HTTP request: %v", err)
		return ServiceUnhealthy
	}

	// Send request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		d.logger.Errorf("HTTP health check failed: %v", err)
		return ServiceUnhealthy
	}
	defer resp.Body.Close()

	// Check response
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ServiceHealthy
	}

	return ServiceUnhealthy
}

// checkHTTPSHealth checks the health of an endpoint using HTTPS
func (d *Discovery) checkHTTPSHealth(endpoint ServiceEndpoint, config HealthCheckConfig) ServiceHealth {
	// Build URL
	path := "/"
	if config.Path != "" {
		path = config.Path
	}
	url := fmt.Sprintf("https://%s:%d%s", endpoint.IPAddress, endpoint.Port, path)

	// Set timeout
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.TimeoutSeconds)*time.Second)
	defer cancel()

	// Create request
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		d.logger.Errorf("Failed to create HTTPS request: %v", err)
		return ServiceUnhealthy
	}

	// Send request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		d.logger.Errorf("HTTPS health check failed: %v", err)
		return ServiceUnhealthy
	}
	defer resp.Body.Close()

	// Check response
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ServiceHealthy
	}

	return ServiceUnhealthy
}

// checkTCPHealth checks the health of an endpoint using TCP
func (d *Discovery) checkTCPHealth(endpoint ServiceEndpoint, config HealthCheckConfig) ServiceHealth {
	// Set timeout
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.TimeoutSeconds)*time.Second)
	defer cancel()

	// Create dialer
	dialer := &net.Dialer{}

	// Connect to endpoint
	conn, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", endpoint.IPAddress, endpoint.Port))
	if err != nil {
		d.logger.Errorf("TCP health check failed: %v", err)
		return ServiceUnhealthy
	}
	defer conn.Close()

	return ServiceHealthy
}

// updateEndpointHealth updates the health status of an endpoint
func (d *Discovery) updateEndpointHealth(serviceName, ipAddress string, port uint16, health ServiceHealth) {
	d.servicesMutex.Lock()
	defer d.servicesMutex.Unlock()

	if endpoints, exists := d.services[serviceName]; exists {
		for i, endpoint := range endpoints {
			if endpoint.IPAddress == ipAddress && endpoint.Port == port {
				endpoints[i].HealthStatus = health
				endpoints[i].LastHealthCheck = time.Now().Unix()
				break
			}
		}
	}
}

// ConfigureHealthCheck configures health checks for a service
func (d *Discovery) ConfigureHealthCheck(serviceName string, config HealthCheckConfig) error {
	d.healthCheckMutex.Lock()
	defer d.healthCheckMutex.Unlock()

	d.healthCheckConfigs[serviceName] = config
	d.logger.Infof("Configured health check for service %s: %v", serviceName, config.Protocol)

	return nil
}

// ConfigureLoadBalancing configures load balancing for a service
func (d *Discovery) ConfigureLoadBalancing(serviceName string, strategy LoadBalancingStrategy) error {
	d.strategyMutex.Lock()
	defer d.strategyMutex.Unlock()

	d.loadBalancingStrategy[serviceName] = strategy
	return nil
}
