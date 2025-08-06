package security

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Protocol represents a network protocol
type Protocol string

const (
	// ProtocolTCP represents the TCP protocol
	ProtocolTCP Protocol = "TCP"
	// ProtocolUDP represents the UDP protocol
	ProtocolUDP Protocol = "UDP"
)

// PolicyAction represents an action to take on network traffic
type PolicyAction string

const (
	// PolicyActionAllow allows the traffic
	PolicyActionAllow PolicyAction = "Allow"
	// PolicyActionDeny denies the traffic
	PolicyActionDeny PolicyAction = "Deny"
	// PolicyActionLog logs the traffic but allows it
	PolicyActionLog PolicyAction = "Log"
	// PolicyActionLimit limits the traffic rate
	PolicyActionLimit PolicyAction = "Limit"
)

// PolicyType represents the type of network policy
type PolicyType string

const (
	// PolicyTypeIsolation represents an isolation policy
	PolicyTypeIsolation PolicyType = "Isolation"
	// PolicyTypeSecurity represents a security policy
	PolicyTypeSecurity PolicyType = "Security"
	// PolicyTypeQoS represents a quality of service policy
	PolicyTypeQoS PolicyType = "QoS"
	// PolicyTypeCustom represents a custom policy type
	PolicyTypeCustom PolicyType = "Custom"
)

// NetworkSelector selects resources based on labels
type NetworkSelector struct {
	Labels map[string]string `json:"labels"`
}

// PortRange represents a range of ports for a specific protocol
type PortRange struct {
	Protocol Protocol `json:"protocol"`
	PortMin  uint16   `json:"port_min"`
	PortMax  uint16   `json:"port_max"`
}

// IPBlock represents a CIDR IP block
type IPBlock struct {
	CIDR string `json:"cidr"`
}

// NetworkPeer represents a peer in a network rule
type NetworkPeer struct {
	IPBlock  *IPBlock         `json:"ip_block,omitempty"`
	Selector *NetworkSelector `json:"selector,omitempty"`
}

// NetworkRule represents a rule for network traffic
type NetworkRule struct {
	Ports       []PortRange   `json:"ports"`
	From        []NetworkPeer `json:"from"`
	Action      *PolicyAction `json:"action,omitempty"`
	Log         bool          `json:"log"`
	Description string        `json:"description,omitempty"`
	ID          string        `json:"id,omitempty"`
}

// NetworkPolicy represents a policy for network traffic
type NetworkPolicy struct {
	Name         string            `json:"name"`
	Selector     NetworkSelector   `json:"selector"`
	IngressRules []NetworkRule     `json:"ingress_rules"`
	EgressRules  []NetworkRule     `json:"egress_rules"`
	Priority     int32             `json:"priority"`
	Namespace    string            `json:"namespace,omitempty"`
	TenantID     string            `json:"tenant_id,omitempty"`
	Labels       map[string]string `json:"labels"`
	CreatedAt    int64             `json:"created_at"`
	UpdatedAt    int64             `json:"updated_at"`
}

// EncryptionType represents the type of network traffic encryption
type EncryptionType string

const (
	// EncryptionTypeTLS represents TLS encryption
	EncryptionTypeTLS EncryptionType = "TLS"
	// EncryptionTypeIPSec represents IPSec encryption
	EncryptionTypeIPSec EncryptionType = "IPSec"
	// EncryptionTypeWireGuard represents WireGuard encryption
	EncryptionTypeWireGuard EncryptionType = "WireGuard"
	// EncryptionTypeCustom represents a custom encryption type
	EncryptionTypeCustom EncryptionType = "Custom"
)

// TrafficLoggingLevel represents the level of network traffic logging
type TrafficLoggingLevel string

const (
	// LoggingLevelNone represents no logging
	LoggingLevelNone TrafficLoggingLevel = "None"
	// LoggingLevelMetadata represents metadata-only logging
	LoggingLevelMetadata TrafficLoggingLevel = "Metadata"
	// LoggingLevelHeaders represents header-level logging
	LoggingLevelHeaders TrafficLoggingLevel = "Headers"
	// LoggingLevelFull represents full packet logging
	LoggingLevelFull TrafficLoggingLevel = "Full"
)

// NetworkAction represents an action taken on network traffic
type NetworkAction string

const (
	// NetworkActionAllow indicates the traffic was allowed
	NetworkActionAllow NetworkAction = "Allow"
	// NetworkActionDeny indicates the traffic was denied
	NetworkActionDeny NetworkAction = "Deny"
	// NetworkActionLog indicates the traffic was logged
	NetworkActionLog NetworkAction = "Log"
	// NetworkActionAlert indicates an alert was generated
	NetworkActionAlert NetworkAction = "Alert"
)

// NetworkAlertType represents the type of network security alert
type NetworkAlertType string

const (
	// AlertTypeUnauthorizedAccess indicates unauthorized access
	AlertTypeUnauthorizedAccess NetworkAlertType = "UnauthorizedAccess"
	// AlertTypeSuspiciousTraffic indicates suspicious traffic
	AlertTypeSuspiciousTraffic NetworkAlertType = "SuspiciousTraffic"
	// AlertTypeAnomalousTraffic indicates anomalous traffic
	AlertTypeAnomalousTraffic NetworkAlertType = "AnomalousTraffic"
	// AlertTypePolicyViolation indicates a policy violation
	AlertTypePolicyViolation NetworkAlertType = "PolicyViolation"
	// AlertTypeEncryptionFailure indicates an encryption failure
	AlertTypeEncryptionFailure NetworkAlertType = "EncryptionFailure"
)

// AlertSeverity represents the severity of a security alert
type AlertSeverity string

const (
	// AlertSeverityLow represents a low severity alert
	AlertSeverityLow AlertSeverity = "Low"
	// AlertSeverityMedium represents a medium severity alert
	AlertSeverityMedium AlertSeverity = "Medium"
	// AlertSeverityHigh represents a high severity alert
	AlertSeverityHigh AlertSeverity = "High"
	// AlertSeverityCritical represents a critical severity alert
	AlertSeverityCritical AlertSeverity = "Critical"
)

// EnhancedNetworkPolicy represents a network policy with enhanced security features
type EnhancedNetworkPolicy struct {
	BasePolicy          NetworkPolicy       `json:"base_policy"`
	EncryptionRequired  bool                `json:"encryption_required"`
	EncryptionType      *EncryptionType     `json:"encryption_type,omitempty"`
	TrafficLogging      bool                `json:"traffic_logging"`
	TrafficLoggingLevel TrafficLoggingLevel `json:"traffic_logging_level"`
	IntrusionDetection  bool                `json:"intrusion_detection"`
	CreatedAt           int64               `json:"created_at"`
	UpdatedAt           int64               `json:"updated_at"`
	Owner               string              `json:"owner"`
}

// NetworkTrafficLog represents a log entry for network traffic
type NetworkTrafficLog struct {
	ID                   string        `json:"id"`
	Timestamp            int64         `json:"timestamp"`
	SourceIP             string        `json:"source_ip"`
	DestinationIP        string        `json:"destination_ip"`
	SourceContainer      string        `json:"source_container,omitempty"`
	DestinationContainer string        `json:"destination_container,omitempty"`
	Protocol             Protocol      `json:"protocol"`
	SourcePort           uint16        `json:"source_port"`
	DestinationPort      uint16        `json:"destination_port"`
	BytesSent            uint64        `json:"bytes_sent"`
	BytesReceived        uint64        `json:"bytes_received"`
	Action               NetworkAction `json:"action"`
	PolicyID             string        `json:"policy_id,omitempty"`
	RuleID               string        `json:"rule_id,omitempty"`
}

// NetworkSecurityAlert represents a security alert for network traffic
type NetworkSecurityAlert struct {
	ID            string            `json:"id"`
	Timestamp     int64             `json:"timestamp"`
	AlertType     NetworkAlertType  `json:"alert_type"`
	SourceIP      string            `json:"source_ip"`
	DestinationIP string            `json:"destination_ip"`
	Description   string            `json:"description"`
	Severity      AlertSeverity     `json:"severity"`
	Details       map[string]string `json:"details"`
}

// NetworkPolicyEnforcer manages and enforces network security policies
type NetworkPolicyEnforcer struct {
	policies               map[string]EnhancedNetworkPolicy
	trafficLogs            []NetworkTrafficLog
	securityAlerts         []NetworkSecurityAlert
	containerLabels        map[string]map[string]string
	encryptionCertificates map[string][]byte
	activeConnections      map[string]map[string]struct{} // source -> set of destinations
	mutex                  sync.RWMutex
	logger                 *logrus.Logger
}

// NewNetworkPolicyEnforcer creates a new network policy enforcer
func NewNetworkPolicyEnforcer(logger *logrus.Logger) *NetworkPolicyEnforcer {
	if logger == nil {
		logger = logrus.New()
		logger.SetLevel(logrus.InfoLevel)
	}

	return &NetworkPolicyEnforcer{
		policies:               make(map[string]EnhancedNetworkPolicy),
		trafficLogs:            make([]NetworkTrafficLog, 0),
		securityAlerts:         make([]NetworkSecurityAlert, 0),
		containerLabels:        make(map[string]map[string]string),
		encryptionCertificates: make(map[string][]byte),
		activeConnections:      make(map[string]map[string]struct{}),
		logger:                 logger,
	}
}

// CreatePolicy creates a new enhanced network policy
func (e *NetworkPolicyEnforcer) CreatePolicy(ctx context.Context, policy EnhancedNetworkPolicy) error {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	e.policies[policy.BasePolicy.Name] = policy
	e.logger.WithFields(logrus.Fields{
		"policy_name": policy.BasePolicy.Name,
		"owner":       policy.Owner,
	}).Info("Created enhanced network policy")
	return nil
}

// GetPolicy gets a policy by name
func (e *NetworkPolicyEnforcer) GetPolicy(ctx context.Context, name string) (*EnhancedNetworkPolicy, error) {
	e.mutex.RLock()
	defer e.mutex.RUnlock()

	policy, exists := e.policies[name]
	if !exists {
		return nil, fmt.Errorf("policy not found: %s", name)
	}
	return &policy, nil
}

// ListPolicies lists all policies
func (e *NetworkPolicyEnforcer) ListPolicies(ctx context.Context) []EnhancedNetworkPolicy {
	e.mutex.RLock()
	defer e.mutex.RUnlock()

	policies := make([]EnhancedNetworkPolicy, 0, len(e.policies))
	for _, policy := range e.policies {
		policies = append(policies, policy)
	}
	return policies
}

// DeletePolicy deletes a policy
func (e *NetworkPolicyEnforcer) DeletePolicy(ctx context.Context, name string) error {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if _, exists := e.policies[name]; !exists {
		return fmt.Errorf("policy not found: %s", name)
	}

	delete(e.policies, name)
	e.logger.WithField("policy_name", name).Info("Deleted network policy")
	return nil
}

// RegisterContainerLabels registers container labels for policy matching
func (e *NetworkPolicyEnforcer) RegisterContainerLabels(ctx context.Context, containerID string, labels map[string]string) error {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	e.containerLabels[containerID] = labels
	e.logger.WithFields(logrus.Fields{
		"container_id": containerID,
		"labels_count": len(labels),
	}).Debug("Registered container labels")
	return nil
}

// UnregisterContainerLabels unregisters container labels when container is removed
func (e *NetworkPolicyEnforcer) UnregisterContainerLabels(ctx context.Context, containerID string) error {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	delete(e.containerLabels, containerID)
	e.logger.WithField("container_id", containerID).Debug("Unregistered container labels")
	return nil
}

// IsTrafficAllowed checks if traffic is allowed between two containers
func (e *NetworkPolicyEnforcer) IsTrafficAllowed(
	ctx context.Context,
	sourceContainer string,
	destinationContainer string,
	protocol Protocol,
	port uint16,
) (bool, error) {
	e.mutex.RLock()
	defer e.mutex.RUnlock()

	// Get container labels
	sourceLabels, exists := e.containerLabels[sourceContainer]
	if !exists {
		e.logger.WithField("container_id", sourceContainer).Debug("Source container not found")
		return false, nil // Source container not found
	}

	destinationLabels, exists := e.containerLabels[destinationContainer]
	if !exists {
		e.logger.WithField("container_id", destinationContainer).Debug("Destination container not found")
		return false, nil // Destination container not found
	}

	// Check against all policies
	for _, policy := range e.policies {
		// Check if policy applies to source container
		if e.labelsMatchSelector(sourceLabels, policy.BasePolicy.Selector.Labels) {
			// Check egress rules
			for _, rule := range policy.BasePolicy.EgressRules {
				// Check if the rule allows traffic to the destination
				if e.ruleAllowsTraffic(&rule, destinationLabels, protocol, port) {
					// Log the allowed traffic
					if err := e.logTraffic(
						ctx,
						sourceContainer,
						destinationContainer,
						protocol,
						0, // Source port (not known)
						port,
						100, // Dummy bytes sent
						200, // Dummy bytes received
						NetworkActionAllow,
						policy.BasePolicy.Name,
						"",
					); err != nil {
						e.logger.WithError(err).Error("Failed to log traffic")
					}

					return true, nil
				}
			}
		}

		// Check if policy applies to destination container
		if e.labelsMatchSelector(destinationLabels, policy.BasePolicy.Selector.Labels) {
			// Check ingress rules
			for _, rule := range policy.BasePolicy.IngressRules {
				// Check if the rule allows traffic from the source
				if e.ruleAllowsTraffic(&rule, sourceLabels, protocol, port) {
					// Log the allowed traffic
					if err := e.logTraffic(
						ctx,
						sourceContainer,
						destinationContainer,
						protocol,
						0, // Source port (not known)
						port,
						100, // Dummy bytes sent
						200, // Dummy bytes received
						NetworkActionAllow,
						policy.BasePolicy.Name,
						"",
					); err != nil {
						e.logger.WithError(err).Error("Failed to log traffic")
					}

					return true, nil
				}
			}
		}
	}

	// If no policy explicitly allows the traffic, deny it
	if err := e.logTraffic(
		ctx,
		sourceContainer,
		destinationContainer,
		protocol,
		0, // Source port (not known)
		port,
		0, // No bytes sent (denied)
		0, // No bytes received (denied)
		NetworkActionDeny,
		"",
		"",
	); err != nil {
		e.logger.WithError(err).Error("Failed to log traffic")
	}

	return false, nil
}

// ruleAllowsTraffic checks if a rule allows traffic to/from a container with given labels
func (e *NetworkPolicyEnforcer) ruleAllowsTraffic(
	rule *NetworkRule,
	containerLabels map[string]string,
	protocol Protocol,
	port uint16,
) bool {
	// Check if the port is allowed by any port range in the rule
	portAllowed := false
	for _, portRange := range rule.Ports {
		if portRange.Protocol == protocol &&
			port >= portRange.PortMin &&
			port <= portRange.PortMax {
			portAllowed = true
			break
		}
	}

	if !portAllowed {
		return false
	}

	// Check if the container is allowed by any peer in the rule
	for _, peer := range rule.From {
		// Check IP block if specified
		if peer.IPBlock != nil {
			// In a real implementation, we would check if the container's IP is in the CIDR block
			// For now, we'll assume it's not
			continue
		}

		// Check selector if specified
		if peer.Selector != nil {
			if e.labelsMatchSelector(containerLabels, peer.Selector.Labels) {
				return true
			}
		}
	}

	return false
}

// labelsMatchSelector checks if container labels match a selector
func (e *NetworkPolicyEnforcer) labelsMatchSelector(containerLabels map[string]string, selectorLabels map[string]string) bool {
	// All selector labels must be present in the container labels with matching values
	for key, value := range selectorLabels {
		containerValue, exists := containerLabels[key]
		if !exists || containerValue != value {
			return false
		}
	}

	return true
}

// logTraffic logs network traffic
func (e *NetworkPolicyEnforcer) logTraffic(
	ctx context.Context,
	sourceContainer string,
	destinationContainer string,
	protocol Protocol,
	sourcePort uint16,
	destinationPort uint16,
	bytesSent uint64,
	bytesReceived uint64,
	action NetworkAction,
	policyID string,
	ruleID string,
) error {
	// In a real implementation, we would get the actual IP addresses
	sourceIP := fmt.Sprintf("10.0.0.%d", len(sourceContainer)%255)
	destinationIP := fmt.Sprintf("10.0.0.%d", len(destinationContainer)%255)

	logEntry := NetworkTrafficLog{
		ID:                   fmt.Sprintf("LOG-%d-%d", time.Now().UnixNano()%10000, len(sourceContainer)),
		Timestamp:            time.Now().Unix(),
		SourceIP:             sourceIP,
		DestinationIP:        destinationIP,
		SourceContainer:      sourceContainer,
		DestinationContainer: destinationContainer,
		Protocol:             protocol,
		SourcePort:           sourcePort,
		DestinationPort:      destinationPort,
		BytesSent:            bytesSent,
		BytesReceived:        bytesReceived,
		Action:               action,
		PolicyID:             policyID,
		RuleID:               ruleID,
	}

	e.mutex.Lock()
	e.trafficLogs = append(e.trafficLogs, logEntry)
	e.mutex.Unlock()

	// In a real implementation, we would also send logs to a central logging system
	e.logger.WithFields(logrus.Fields{
		"source":      sourceContainer,
		"destination": destinationContainer,
		"protocol":    protocol,
		"port":        destinationPort,
		"action":      action,
	}).Debug("Network traffic logged")

	return nil
}

// CreateAlert creates a network security alert
func (e *NetworkPolicyEnforcer) CreateAlert(
	ctx context.Context,
	alertType NetworkAlertType,
	sourceIP string,
	destinationIP string,
	description string,
	severity AlertSeverity,
	details map[string]string,
) error {
	alert := NetworkSecurityAlert{
		ID:            fmt.Sprintf("ALERT-%d-%d", time.Now().UnixNano()%10000, len(sourceIP)),
		Timestamp:     time.Now().Unix(),
		AlertType:     alertType,
		SourceIP:      sourceIP,
		DestinationIP: destinationIP,
		Description:   description,
		Severity:      severity,
		Details:       details,
	}

	e.mutex.Lock()
	e.securityAlerts = append(e.securityAlerts, alert)
	e.mutex.Unlock()

	// In a real implementation, we would also send alerts to a notification system
	e.logger.WithFields(logrus.Fields{
		"alert_id":       alert.ID,
		"alert_type":     alertType,
		"source_ip":      sourceIP,
		"destination_ip": destinationIP,
		"severity":       severity,
	}).Info(description)

	return nil
}

// GetTrafficLogs gets network traffic logs
func (e *NetworkPolicyEnforcer) GetTrafficLogs(ctx context.Context) []NetworkTrafficLog {
	e.mutex.RLock()
	defer e.mutex.RUnlock()

	// Return a copy of the logs to avoid race conditions
	logs := make([]NetworkTrafficLog, len(e.trafficLogs))
	copy(logs, e.trafficLogs)
	return logs
}

// GetSecurityAlerts gets network security alerts
func (e *NetworkPolicyEnforcer) GetSecurityAlerts(ctx context.Context) []NetworkSecurityAlert {
	e.mutex.RLock()
	defer e.mutex.RUnlock()

	// Return a copy of the alerts to avoid race conditions
	alerts := make([]NetworkSecurityAlert, len(e.securityAlerts))
	copy(alerts, e.securityAlerts)
	return alerts
}

// SetupEncryption sets up network encryption for a container
func (e *NetworkPolicyEnforcer) SetupEncryption(ctx context.Context, containerID string, encryptionType EncryptionType) error {
	e.logger.WithFields(logrus.Fields{
		"container_id":    containerID,
		"encryption_type": encryptionType,
	}).Info("Setting up encryption")

	// In a real implementation, we would generate or load certificates and configure encryption

	// For now, just store a dummy certificate
	e.mutex.Lock()
	e.encryptionCertificates[containerID] = []byte{1, 2, 3, 4, 5}
	e.mutex.Unlock()

	return nil
}

// MonitorNetworkTraffic monitors network traffic for anomalies and policy violations
func (e *NetworkPolicyEnforcer) MonitorNetworkTraffic(ctx context.Context) error {
	e.logger.Info("Starting network traffic monitoring")

	// In a real implementation, we would continuously monitor network traffic
	// For now, we'll just simulate some monitoring

	// Check for suspicious connection patterns
	e.mutex.RLock()
	defer e.mutex.RUnlock()

	for source, destinations := range e.activeConnections {
		// Check for too many connections from a single source
		if len(destinations) > 100 {
			details := map[string]string{
				"connection_count": fmt.Sprintf("%d", len(destinations)),
			}

			if err := e.CreateAlert(
				ctx,
				AlertTypeSuspiciousTraffic,
				source,
				"multiple",
				"Excessive connection count from single source",
				AlertSeverityMedium,
				details,
			); err != nil {
				return err
			}
		}
	}

	return nil
}

// EnhancePolicy converts a base network policy to an enhanced one
func (e *NetworkPolicyEnforcer) EnhancePolicy(basePolicy NetworkPolicy, owner string) EnhancedNetworkPolicy {
	now := time.Now().Unix()

	loggingLevel := LoggingLevelMetadata
	return EnhancedNetworkPolicy{
		BasePolicy:          basePolicy,
		EncryptionRequired:  false,
		EncryptionType:      nil,
		TrafficLogging:      true,
		TrafficLoggingLevel: loggingLevel,
		IntrusionDetection:  false,
		CreatedAt:           now,
		UpdatedAt:           now,
		Owner:               owner,
	}
}
