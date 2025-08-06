package service

import (
	"context"
	"fmt"
	"time"

	"github.com/ao/hivemind/pkg/api"
)

// GetServiceURL returns the URL for a service
func (d *Discovery) GetServiceURL(ctx context.Context, serviceName string) (string, error) {
	d.servicesMutex.RLock()
	defer d.servicesMutex.RUnlock()

	if endpoints, exists := d.services[serviceName]; exists && len(endpoints) > 0 {
		// Find a healthy endpoint
		for _, endpoint := range endpoints {
			if endpoint.HealthStatus == ServiceHealthy {
				return fmt.Sprintf("http://%s:%d", endpoint.IPAddress, endpoint.Port), nil
			}
		}

		// If no healthy endpoint, use the first one
		endpoint := endpoints[0]
		return fmt.Sprintf("http://%s:%d", endpoint.IPAddress, endpoint.Port), nil
	}

	return "", fmt.Errorf("service %s not found", serviceName)
}

// GetServiceURLSimple is the original GetServiceURL method without context and error
func (d *Discovery) GetServiceURLSimple(serviceName string) string {
	url, _ := d.GetServiceURL(context.Background(), serviceName)
	return url
}

// GetServiceEndpointWithOptions returns a service endpoint with options
func (d *Discovery) GetServiceEndpointWithOptions(
	serviceName string,
	clientIP string,
	sessionID string,
	preferredVersion string,
) *ServiceEndpoint {
	d.servicesMutex.RLock()
	defer d.servicesMutex.RUnlock()

	if endpoints, exists := d.services[serviceName]; exists && len(endpoints) > 0 {
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

		// Filter by version if specified
		if preferredVersion != "" {
			versionEndpoints := make([]ServiceEndpoint, 0)
			for _, endpoint := range healthyEndpoints {
				if endpoint.Version == preferredVersion {
					versionEndpoints = append(versionEndpoints, endpoint)
				}
			}

			if len(versionEndpoints) > 0 {
				healthyEndpoints = versionEndpoints
			}
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

		switch strategy {
		case LoadBalancingRoundRobin:
			selectedEndpoint = d.selectRoundRobin(healthyEndpoints)
		case LoadBalancingWeightedRoundRobin:
			selectedEndpoint = d.selectWeightedRoundRobin(healthyEndpoints)
		case LoadBalancingLeastConnections:
			selectedEndpoint = d.selectLeastConnections(healthyEndpoints)
		case LoadBalancingRandom:
			selectedEndpoint = d.selectRandom(healthyEndpoints)
		case LoadBalancingIPHash:
			selectedEndpoint = d.selectIPHash(healthyEndpoints, clientIP)
		default:
			selectedEndpoint = d.selectRoundRobin(healthyEndpoints)
		}

		if selectedEndpoint != nil {
			// Update endpoint stats
			d.updateEndpointStats(selectedEndpoint.ServiceName, selectedEndpoint.IPAddress, selectedEndpoint.Port)
			return selectedEndpoint
		}
	}

	return nil
}

// selectRoundRobin selects an endpoint using round-robin
func (d *Discovery) selectRoundRobin(endpoints []ServiceEndpoint) *ServiceEndpoint {
	if len(endpoints) == 0 {
		return nil
	}

	// Simple round-robin
	index := time.Now().UnixNano() % int64(len(endpoints))
	return &endpoints[index]
}

// selectWeightedRoundRobin selects an endpoint using weighted round-robin
func (d *Discovery) selectWeightedRoundRobin(endpoints []ServiceEndpoint) *ServiceEndpoint {
	if len(endpoints) == 0 {
		return nil
	}

	// Calculate total weight
	totalWeight := uint32(0)
	for _, endpoint := range endpoints {
		weight := endpoint.Weight
		if weight == 0 {
			weight = 1 // Default weight
		}
		totalWeight += weight
	}

	if totalWeight == 0 {
		// If no weights, use simple round-robin
		return d.selectRoundRobin(endpoints)
	}

	// Select based on weight
	randomWeight := uint32(time.Now().UnixNano() % int64(totalWeight))
	currentWeight := uint32(0)

	for i, endpoint := range endpoints {
		weight := endpoint.Weight
		if weight == 0 {
			weight = 1 // Default weight
		}
		currentWeight += weight
		if currentWeight > randomWeight {
			return &endpoints[i]
		}
	}

	// Fallback to first endpoint
	return &endpoints[0]
}

// selectLeastConnections selects an endpoint using least connections
func (d *Discovery) selectLeastConnections(endpoints []ServiceEndpoint) *ServiceEndpoint {
	if len(endpoints) == 0 {
		return nil
	}

	d.statsMutex.RLock()
	defer d.statsMutex.RUnlock()

	var selectedEndpoint *ServiceEndpoint
	minConnections := uint32(^uint32(0)) // Max uint32 value

	for i, endpoint := range endpoints {
		key := fmt.Sprintf("%s:%s:%d", endpoint.ServiceName, endpoint.IPAddress, endpoint.Port)
		stats, exists := d.endpointStats[key]
		connections := uint32(0)
		if exists {
			connections = stats.ActiveConnections
		}

		if connections < minConnections {
			minConnections = connections
			selectedEndpoint = &endpoints[i]
		}
	}

	return selectedEndpoint
}

// selectRandom selects an endpoint randomly
func (d *Discovery) selectRandom(endpoints []ServiceEndpoint) *ServiceEndpoint {
	if len(endpoints) == 0 {
		return nil
	}

	// Random selection
	index := time.Now().UnixNano() % int64(len(endpoints))
	return &endpoints[index]
}

// selectIPHash selects an endpoint using IP hash
func (d *Discovery) selectIPHash(endpoints []ServiceEndpoint, clientIP string) *ServiceEndpoint {
	if len(endpoints) == 0 {
		return nil
	}

	// IP hash
	hash := uint32(0)
	for _, c := range clientIP {
		hash = hash*31 + uint32(c)
	}
	index := int(hash % uint32(len(endpoints)))
	return &endpoints[index]
}

// AddRoutingRule adds a routing rule
func (d *Discovery) AddRoutingRule(rule RoutingRule) error {
	d.routingMutex.Lock()
	defer d.routingMutex.Unlock()

	d.routingRules = append(d.routingRules, rule)
	d.logger.Infof("Added routing rule for service %s: %s", rule.ServiceName, rule.PathPrefix)

	return nil
}

// RemoveRoutingRule removes a routing rule
func (d *Discovery) RemoveRoutingRule(serviceName string, pathPrefix string) error {
	d.routingMutex.Lock()
	defer d.routingMutex.Unlock()

	newRules := make([]RoutingRule, 0, len(d.routingRules))
	for _, rule := range d.routingRules {
		if rule.ServiceName != serviceName || (pathPrefix != "" && rule.PathPrefix != pathPrefix) {
			newRules = append(newRules, rule)
		}
	}

	if len(newRules) < len(d.routingRules) {
		d.routingRules = newRules
		d.logger.Infof("Removed routing rule for service %s: %s", serviceName, pathPrefix)
	}

	return nil
}

// ListRoutingRules returns all routing rules
func (d *Discovery) ListRoutingRules() []RoutingRule {
	d.routingMutex.RLock()
	defer d.routingMutex.RUnlock()

	rules := make([]RoutingRule, len(d.routingRules))
	copy(rules, d.routingRules)

	return rules
}

// ConfigureTrafficSplit configures traffic splitting for a service
func (d *Discovery) ConfigureTrafficSplit(config TrafficSplitConfig) error {
	d.splitsMutex.Lock()
	defer d.splitsMutex.Unlock()

	d.trafficSplits[config.ServiceName] = config
	d.logger.Infof("Configured traffic split for service %s", config.ServiceName)

	return nil
}

// RemoveTrafficSplit removes traffic splitting for a service
func (d *Discovery) RemoveTrafficSplit(serviceName string) error {
	d.splitsMutex.Lock()
	defer d.splitsMutex.Unlock()

	if _, exists := d.trafficSplits[serviceName]; exists {
		delete(d.trafficSplits, serviceName)
		d.logger.Infof("Removed traffic split for service %s", serviceName)
	}

	return nil
}

// GetTrafficSplit returns the traffic split configuration for a service
func (d *Discovery) GetTrafficSplit(serviceName string) *TrafficSplitConfig {
	d.splitsMutex.RLock()
	defer d.splitsMutex.RUnlock()

	if config, exists := d.trafficSplits[serviceName]; exists {
		return &config
	}

	return nil
}

// RegisterServiceWithVersion registers a service with a specific version
func (d *Discovery) RegisterServiceWithVersion(
	serviceConfig *api.ServiceConfig,
	nodeID string,
	ipAddress string,
	port uint16,
	version string,
	weight uint32,
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
		Version:         version,
		Weight:          weight,
		Metadata:        make(map[string]string),
	}

	d.servicesMutex.Lock()
	defer d.servicesMutex.Unlock()

	if endpoints, exists := d.services[serviceConfig.Name]; exists {
		// Check if this endpoint already exists
		exists := false
		for i, e := range endpoints {
			if e.NodeID == nodeID && e.IPAddress == ipAddress && e.Port == port {
				// Update existing endpoint
				endpoints[i].Version = version
				endpoints[i].Weight = weight
				exists = true
				break
			}
		}

		if !exists {
			d.services[serviceConfig.Name] = append(endpoints, endpoint)
			d.logger.Infof(
				"Registered new endpoint for service %s: %s:%d (version %s, weight %d)",
				serviceConfig.Name, ipAddress, port, version, weight,
			)
		}
	} else {
		// First endpoint for this service
		d.services[serviceConfig.Name] = []ServiceEndpoint{endpoint}
		d.logger.Infof(
			"Registered new service %s with endpoint %s:%d (version %s, weight %d)",
			serviceConfig.Name, ipAddress, port, version, weight,
		)
	}

	return nil
}

// ConfigureServiceMesh configures service mesh for a service
func (d *Discovery) ConfigureServiceMesh(serviceName string, config ServiceMeshConfig) error {
	d.logger.Infof("Configured service mesh for service %s", serviceName)

	// In a real implementation, this would configure the service mesh

	return nil
}

// GetProxyMetrics returns metrics for the proxy
func (d *Discovery) GetProxyMetrics() map[string]float64 {
	metrics := make(map[string]float64)

	d.statsMutex.RLock()
	defer d.statsMutex.RUnlock()

	totalRequests := uint64(0)
	failedRequests := uint64(0)
	activeConnections := uint32(0)

	for _, stats := range d.endpointStats {
		totalRequests += stats.TotalRequests
		failedRequests += stats.FailedRequests
		activeConnections += stats.ActiveConnections
	}

	metrics["total_requests"] = float64(totalRequests)
	metrics["failed_requests"] = float64(failedRequests)
	metrics["active_connections"] = float64(activeConnections)

	if totalRequests > 0 {
		metrics["error_rate"] = float64(failedRequests) / float64(totalRequests) * 100.0
	} else {
		metrics["error_rate"] = 0.0
	}

	return metrics
}

// GetHealthCheckStatistics returns statistics for health checks
func (d *Discovery) GetHealthCheckStatistics() map[string]float64 {
	stats := make(map[string]float64)

	d.servicesMutex.RLock()
	defer d.servicesMutex.RUnlock()

	totalEndpoints := 0
	healthyEndpoints := 0
	unhealthyEndpoints := 0
	unknownEndpoints := 0

	for _, endpoints := range d.services {
		for _, endpoint := range endpoints {
			totalEndpoints++

			switch endpoint.HealthStatus {
			case ServiceHealthy:
				healthyEndpoints++
			case ServiceUnhealthy:
				unhealthyEndpoints++
			case ServiceUnknown:
				unknownEndpoints++
			}
		}
	}

	stats["total_endpoints"] = float64(totalEndpoints)
	stats["healthy_endpoints"] = float64(healthyEndpoints)
	stats["unhealthy_endpoints"] = float64(unhealthyEndpoints)
	stats["unknown_endpoints"] = float64(unknownEndpoints)

	if totalEndpoints > 0 {
		stats["healthy_percentage"] = float64(healthyEndpoints) / float64(totalEndpoints) * 100.0
		stats["unhealthy_percentage"] = float64(unhealthyEndpoints) / float64(totalEndpoints) * 100.0
		stats["unknown_percentage"] = float64(unknownEndpoints) / float64(totalEndpoints) * 100.0
	}

	return stats
}

// RecordConnectionCompletion records the completion of a connection
func (d *Discovery) RecordConnectionCompletion(
	serviceName string,
	ipAddress string,
	port uint16,
	durationMS float64,
	success bool,
) error {
	key := fmt.Sprintf("%s:%s:%d", serviceName, ipAddress, port)

	d.statsMutex.Lock()
	defer d.statsMutex.Unlock()

	stats, exists := d.endpointStats[key]
	if !exists {
		stats = EndpointStats{}
	}

	// Update stats
	if stats.ActiveConnections > 0 {
		stats.ActiveConnections--
	}

	if !success {
		stats.FailedRequests++
	}

	// Update average response time
	if stats.TotalRequests > 0 {
		stats.AvgResponseTimeMS = (stats.AvgResponseTimeMS*float64(stats.TotalRequests-1) + durationMS) / float64(stats.TotalRequests)
	} else {
		stats.AvgResponseTimeMS = durationMS
	}

	d.endpointStats[key] = stats

	return nil
}

// MarkEndpointUnhealthy marks an endpoint as unhealthy
func (d *Discovery) MarkEndpointUnhealthy(serviceName string, ipAddress string, port uint16) error {
	d.servicesMutex.Lock()
	defer d.servicesMutex.Unlock()

	if endpoints, exists := d.services[serviceName]; exists {
		for i, endpoint := range endpoints {
			if endpoint.IPAddress == ipAddress && endpoint.Port == port {
				endpoints[i].HealthStatus = ServiceUnhealthy
				endpoints[i].LastHealthCheck = time.Now().Unix()
				d.logger.Infof("Marked endpoint %s:%d as unhealthy", ipAddress, port)
				break
			}
		}
	}

	return nil
}

// CheckCircuitBreaker checks if a circuit breaker is open
func (d *Discovery) CheckCircuitBreaker(serviceName string, endpointIP string) (bool, error) {
	// In a real implementation, this would check the circuit breaker state
	// For now, we'll just return false (circuit breaker is closed)
	return false, nil
}

// RecordCircuitBreakerResult records the result of a request for circuit breaking
func (d *Discovery) RecordCircuitBreakerResult(serviceName string, endpointIP string, success bool) error {
	// In a real implementation, this would update circuit breaker state
	return nil
}

// Close closes the service discovery
func (d *Discovery) Close() error {
	d.logger.Info("Closing service discovery")

	// Stop the DNS server
	// In a real implementation, this would stop the DNS server

	// Stop the proxy server
	// In a real implementation, this would stop the proxy server

	// Stop the health check system
	// In a real implementation, this would stop the health check system

	return nil
}

// updateEndpointStats updates the stats for an endpoint
func (d *Discovery) updateEndpointStats(serviceName string, ipAddress string, port uint16) {
	key := fmt.Sprintf("%s:%s:%d", serviceName, ipAddress, port)

	d.statsMutex.Lock()
	defer d.statsMutex.Unlock()

	stats, exists := d.endpointStats[key]
	if !exists {
		stats = EndpointStats{
			ActiveConnections: 0,
			TotalRequests:     0,
			FailedRequests:    0,
			AvgResponseTimeMS: 0,
			LastSelected:      0,
		}
	}

	stats.ActiveConnections++
	stats.TotalRequests++
	stats.LastSelected = time.Now().Unix()

	d.endpointStats[key] = stats
}
