package security

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// VulnerabilitySeverity represents the severity level of a vulnerability
type VulnerabilitySeverity int

const (
	// SeverityUnknown represents an unknown severity level
	SeverityUnknown VulnerabilitySeverity = iota
	// SeverityLow represents a low severity level
	SeverityLow
	// SeverityMedium represents a medium severity level
	SeverityMedium
	// SeverityHigh represents a high severity level
	SeverityHigh
	// SeverityCritical represents a critical severity level
	SeverityCritical
)

// String returns the string representation of the vulnerability severity
func (v VulnerabilitySeverity) String() string {
	switch v {
	case SeverityUnknown:
		return "Unknown"
	case SeverityLow:
		return "Low"
	case SeverityMedium:
		return "Medium"
	case SeverityHigh:
		return "High"
	case SeverityCritical:
		return "Critical"
	default:
		return "Unknown"
	}
}

// MarshalJSON implements the json.Marshaler interface
func (v VulnerabilitySeverity) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.String())
}

// UnmarshalJSON implements the json.Unmarshaler interface
func (v *VulnerabilitySeverity) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}

	switch s {
	case "Unknown":
		*v = SeverityUnknown
	case "Low":
		*v = SeverityLow
	case "Medium":
		*v = SeverityMedium
	case "High":
		*v = SeverityHigh
	case "Critical":
		*v = SeverityCritical
	default:
		*v = SeverityUnknown
	}
	return nil
}

// Vulnerability represents a vulnerability found in a container image
type Vulnerability struct {
	ID              string                `json:"id"`
	Name            string                `json:"name"`
	Description     string                `json:"description"`
	Severity        VulnerabilitySeverity `json:"severity"`
	AffectedPackage string                `json:"affected_package"`
	AffectedVersion string                `json:"affected_version"`
	FixedVersion    *string               `json:"fixed_version,omitempty"`
	CVEID           *string               `json:"cve_id,omitempty"`
	DiscoveredAt    int64                 `json:"discovered_at"`
}

// ScanStatus represents the status of a container scan
type ScanStatus string

const (
	// ScanStatusPending indicates the scan is pending
	ScanStatusPending ScanStatus = "pending"
	// ScanStatusInProgress indicates the scan is in progress
	ScanStatusInProgress ScanStatus = "in_progress"
	// ScanStatusCompleted indicates the scan is completed
	ScanStatusCompleted ScanStatus = "completed"
	// ScanStatusFailed indicates the scan has failed
	ScanStatusFailed ScanStatus = "failed"
)

// ScanResult represents the results of a container image scan
type ScanResult struct {
	ImageID         string          `json:"image_id"`
	ImageName       string          `json:"image_name"`
	ScanTime        int64           `json:"scan_time"`
	Vulnerabilities []Vulnerability `json:"vulnerabilities"`
	Compliant       bool            `json:"compliant"`
	ScanStatus      ScanStatus      `json:"scan_status"`
	ScanErrors      []string        `json:"scan_errors"`
}

// SecurityPolicy represents a security policy for container images
type SecurityPolicy struct {
	ID                string                `json:"id"`
	Name              string                `json:"name"`
	Description       string                `json:"description"`
	MaxSeverity       VulnerabilitySeverity `json:"max_severity"`
	BlockOnSeverity   VulnerabilitySeverity `json:"block_on_severity"`
	WhitelistCVEs     []string              `json:"whitelist_cves"`
	BlacklistCVEs     []string              `json:"blacklist_cves"`
	AllowedRegistries []string              `json:"allowed_registries"`
	RequiredLabels    map[string]string     `json:"required_labels"`
	ScanFrequency     time.Duration         `json:"scan_frequency"`
	CreatedAt         int64                 `json:"created_at"`
	UpdatedAt         int64                 `json:"updated_at"`
}

// SecurityEventType represents the type of a security event
type SecurityEventType string

const (
	// EventTypeVulnerabilityFound indicates a vulnerability was found
	EventTypeVulnerabilityFound SecurityEventType = "vulnerability_found"
	// EventTypePolicyViolation indicates a policy violation
	EventTypePolicyViolation SecurityEventType = "policy_violation"
	// EventTypeScanCompleted indicates a scan was completed
	EventTypeScanCompleted SecurityEventType = "scan_completed"
	// EventTypeScanFailed indicates a scan has failed
	EventTypeScanFailed SecurityEventType = "scan_failed"
	// EventTypeRuntimeViolation indicates a runtime violation
	EventTypeRuntimeViolation SecurityEventType = "runtime_violation"
	// EventTypeUnauthorizedAccess indicates unauthorized access
	EventTypeUnauthorizedAccess SecurityEventType = "unauthorized_access"
)

// SecurityEventSeverity represents the severity of a security event
type SecurityEventSeverity string

const (
	// EventSeverityInfo represents an informational security event
	EventSeverityInfo SecurityEventSeverity = "info"
	// EventSeverityWarning represents a warning security event
	EventSeverityWarning SecurityEventSeverity = "warning"
	// EventSeverityError represents an error security event
	EventSeverityError SecurityEventSeverity = "error"
	// EventSeverityCritical represents a critical security event
	EventSeverityCritical SecurityEventSeverity = "critical"
)

// SecurityEvent represents a security event for logging and alerting
type SecurityEvent struct {
	ID        string                `json:"id"`
	EventType SecurityEventType     `json:"event_type"`
	Source    string                `json:"source"`
	Timestamp int64                 `json:"timestamp"`
	Message   string                `json:"message"`
	Details   map[string]string     `json:"details"`
	Severity  SecurityEventSeverity `json:"severity"`
}

// RuntimeViolation represents a runtime security violation
type RuntimeViolation struct {
	RuleID      string                `json:"rule_id"`
	Description string                `json:"description"`
	Severity    SecurityEventSeverity `json:"severity"`
	Details     map[string]string     `json:"details"`
	Timestamp   int64                 `json:"timestamp"`
}

// RuntimeCheckResult represents a container runtime security check result
type RuntimeCheckResult struct {
	ContainerID string             `json:"container_id"`
	Timestamp   int64              `json:"timestamp"`
	Violations  []RuntimeViolation `json:"violations"`
	Compliant   bool               `json:"compliant"`
}

// ContainerScanner manages image scanning and runtime security checks
type ContainerScanner struct {
	scanResults      map[string]ScanResult
	securityPolicies map[string]SecurityPolicy
	securityEvents   []SecurityEvent
	runtimeChecks    map[string]RuntimeCheckResult
	mutex            sync.RWMutex
	logger           *logrus.Logger
}

// NewContainerScanningManager creates a new container scanning manager (alias for NewContainerScanner for backward compatibility)
func NewContainerScanningManager(logger *logrus.Logger) *ContainerScanner {
	return NewContainerScanner(logger)
}

// NewContainerScanner creates a new container scanner
func NewContainerScanner(logger *logrus.Logger) *ContainerScanner {
	if logger == nil {
		logger = logrus.New()
		logger.SetLevel(logrus.InfoLevel)
	}

	return &ContainerScanner{
		scanResults:      make(map[string]ScanResult),
		securityPolicies: make(map[string]SecurityPolicy),
		securityEvents:   make([]SecurityEvent, 0),
		runtimeChecks:    make(map[string]RuntimeCheckResult),
		logger:           logger,
	}
}

// ScanImage scans a container image for vulnerabilities
func (c *ContainerScanner) ScanImage(ctx context.Context, imageName, imageID string) (*ScanResult, error) {
	c.logger.WithFields(logrus.Fields{
		"image_name": imageName,
		"image_id":   imageID,
	}).Info("Scanning image")

	// Create a pending scan result
	scanResult := ScanResult{
		ImageID:         imageID,
		ImageName:       imageName,
		ScanTime:        time.Now().Unix(),
		Vulnerabilities: make([]Vulnerability, 0),
		Compliant:       false,
		ScanStatus:      ScanStatusPending,
		ScanErrors:      make([]string, 0),
	}

	// Update status to in progress
	scanResult.ScanStatus = ScanStatusInProgress

	// In a real implementation, we would integrate with a vulnerability scanner like Trivy, Clair, or Anchore
	// For now, we'll simulate a scan with some sample vulnerabilities

	// Simulate scanning delay
	select {
	case <-ctx.Done():
		scanResult.ScanStatus = ScanStatusFailed
		scanResult.ScanErrors = append(scanResult.ScanErrors, "scan cancelled")
		return &scanResult, ctx.Err()
	case <-time.After(500 * time.Millisecond):
		// Continue with the scan
	}

	// Generate some sample vulnerabilities based on the image name
	vulnerabilities := c.simulateVulnerabilities(imageName)
	scanResult.Vulnerabilities = vulnerabilities

	// Check if the image is compliant with security policies
	compliant, err := c.checkCompliance(ctx, &scanResult)
	if err != nil {
		c.logger.WithError(err).Error("Failed to check compliance")
		scanResult.ScanStatus = ScanStatusFailed
		scanResult.ScanErrors = append(scanResult.ScanErrors, fmt.Sprintf("compliance check failed: %v", err))
		return &scanResult, err
	}
	scanResult.Compliant = compliant

	// Update status to completed
	scanResult.ScanStatus = ScanStatusCompleted

	// Store the scan result
	c.mutex.Lock()
	c.scanResults[imageID] = scanResult
	c.mutex.Unlock()

	// Generate security events for any vulnerabilities found
	if err := c.generateVulnerabilityEvents(ctx, &scanResult); err != nil {
		c.logger.WithError(err).Error("Failed to generate vulnerability events")
	}

	return &scanResult, nil
}

// simulateVulnerabilities simulates vulnerabilities for testing purposes
func (c *ContainerScanner) simulateVulnerabilities(imageName string) []Vulnerability {
	vulnerabilities := make([]Vulnerability, 0)

	// Generate a deterministic number of vulnerabilities based on the image name
	var sum int
	for _, b := range imageName {
		sum += int(b)
	}
	vulnCount := sum % 10

	for i := 0; i < vulnCount; i++ {
		var severity VulnerabilitySeverity
		switch i % 5 {
		case 0:
			severity = SeverityCritical
		case 1:
			severity = SeverityHigh
		case 2:
			severity = SeverityMedium
		case 3:
			severity = SeverityLow
		default:
			severity = SeverityUnknown
		}

		fixedVersion := fmt.Sprintf("1.%d.1", i)
		cveID := fmt.Sprintf("CVE-2023-%d", 1000+i)

		vulnerabilities = append(vulnerabilities, Vulnerability{
			ID:              fmt.Sprintf("VULN-%d-%d", i, len(imageName)),
			Name:            fmt.Sprintf("Sample Vulnerability %d", i),
			Description:     fmt.Sprintf("This is a simulated vulnerability for testing in image %s", imageName),
			Severity:        severity,
			AffectedPackage: fmt.Sprintf("package-%d", i),
			AffectedVersion: fmt.Sprintf("1.%d.0", i),
			FixedVersion:    &fixedVersion,
			CVEID:           &cveID,
			DiscoveredAt:    time.Now().Unix(),
		})
	}

	return vulnerabilities
}

// checkCompliance checks if an image is compliant with security policies
func (c *ContainerScanner) checkCompliance(ctx context.Context, scanResult *ScanResult) (bool, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	// If no policies are defined, default to non-compliant
	if len(c.securityPolicies) == 0 {
		return false, nil
	}

	// Check against each policy
	for _, policy := range c.securityPolicies {
		// Check for critical or high severity vulnerabilities
		for _, vuln := range scanResult.Vulnerabilities {
			if vuln.Severity >= policy.BlockOnSeverity {
				// Check if the vulnerability is whitelisted
				whitelisted := false
				if vuln.CVEID != nil {
					for _, whiteCVE := range policy.WhitelistCVEs {
						if whiteCVE == *vuln.CVEID {
							whitelisted = true
							break
						}
					}
				}

				if !whitelisted {
					return false, nil
				}
			}
		}

		// Check for blacklisted CVEs
		for _, vuln := range scanResult.Vulnerabilities {
			if vuln.CVEID != nil {
				for _, blackCVE := range policy.BlacklistCVEs {
					if blackCVE == *vuln.CVEID {
						return false, nil
					}
				}
			}
		}

		// Check for allowed registries
		if len(policy.AllowedRegistries) > 0 {
			// Extract registry from image name
			registry := ""
			for i, c := range scanResult.ImageName {
				if c == '/' {
					registry = scanResult.ImageName[:i]
					break
				}
			}

			allowed := false
			for _, r := range policy.AllowedRegistries {
				if len(registry) >= len(r) && registry[:len(r)] == r {
					allowed = true
					break
				}
			}

			if !allowed {
				return false, nil
			}
		}
	}

	return true, nil
}

// CreatePolicy creates a new security policy
func (c *ContainerScanner) CreatePolicy(ctx context.Context, policy SecurityPolicy) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.securityPolicies[policy.ID] = policy
	c.logger.WithFields(logrus.Fields{
		"policy_id":   policy.ID,
		"policy_name": policy.Name,
	}).Info("Created security policy")
	return nil
}

// GetPolicy gets a security policy by ID
func (c *ContainerScanner) GetPolicy(ctx context.Context, policyID string) (*SecurityPolicy, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	policy, exists := c.securityPolicies[policyID]
	if !exists {
		return nil, fmt.Errorf("policy not found: %s", policyID)
	}
	return &policy, nil
}

// ListPolicies lists all security policies
func (c *ContainerScanner) ListPolicies(ctx context.Context) []SecurityPolicy {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	policies := make([]SecurityPolicy, 0, len(c.securityPolicies))
	for _, policy := range c.securityPolicies {
		policies = append(policies, policy)
	}
	return policies
}

// DeletePolicy deletes a security policy
func (c *ContainerScanner) DeletePolicy(ctx context.Context, policyID string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if _, exists := c.securityPolicies[policyID]; !exists {
		return fmt.Errorf("policy not found: %s", policyID)
	}

	delete(c.securityPolicies, policyID)
	c.logger.WithField("policy_id", policyID).Info("Deleted security policy")
	return nil
}

// CheckContainerRuntime performs a runtime security check on a running container
func (c *ContainerScanner) CheckContainerRuntime(ctx context.Context, containerID string) (*RuntimeCheckResult, error) {
	c.logger.WithField("container_id", containerID).Info("Performing runtime security check")

	// In a real implementation, we would integrate with a runtime security tool like Falco
	// For now, we'll simulate a runtime check

	now := time.Now().Unix()

	// Simulate some violations based on container ID
	violations := c.simulateRuntimeViolations(containerID)

	result := RuntimeCheckResult{
		ContainerID: containerID,
		Timestamp:   now,
		Violations:  violations,
		Compliant:   len(violations) == 0,
	}

	// Store the result
	c.mutex.Lock()
	c.runtimeChecks[containerID] = result
	c.mutex.Unlock()

	// Generate security events for any violations
	for _, violation := range result.Violations {
		if err := c.LogSecurityEvent(
			ctx,
			EventTypeRuntimeViolation,
			containerID,
			violation.Description,
			violation.Severity,
			violation.Details,
		); err != nil {
			c.logger.WithError(err).Error("Failed to log security event")
		}
	}

	return &result, nil
}

// simulateRuntimeViolations simulates runtime violations for testing purposes
func (c *ContainerScanner) simulateRuntimeViolations(containerID string) []RuntimeViolation {
	violations := make([]RuntimeViolation, 0)

	// Generate a deterministic number of violations based on the container ID
	var sum int
	for _, b := range containerID {
		sum += int(b)
	}
	violationCount := sum % 3

	for i := 0; i < violationCount; i++ {
		var severity SecurityEventSeverity
		switch i % 3 {
		case 0:
			severity = EventSeverityCritical
		case 1:
			severity = EventSeverityError
		default:
			severity = EventSeverityWarning
		}

		details := map[string]string{
			"process": fmt.Sprintf("/bin/suspicious%d", i),
			"user":    "root",
		}

		violations = append(violations, RuntimeViolation{
			RuleID:      fmt.Sprintf("RULE-%d", i),
			Description: fmt.Sprintf("Suspicious activity detected in container %s", containerID),
			Severity:    severity,
			Details:     details,
			Timestamp:   time.Now().Unix(),
		})
	}

	return violations
}

// generateVulnerabilityEvents generates security events for vulnerabilities found in a scan
func (c *ContainerScanner) generateVulnerabilityEvents(ctx context.Context, scanResult *ScanResult) error {
	for _, vulnerability := range scanResult.Vulnerabilities {
		if vulnerability.Severity >= SeverityHigh {
			details := map[string]string{
				"image_id":   scanResult.ImageID,
				"image_name": scanResult.ImageName,
				"package":    vulnerability.AffectedPackage,
				"version":    vulnerability.AffectedVersion,
			}

			if vulnerability.CVEID != nil {
				details["cve_id"] = *vulnerability.CVEID
			}

			var severity SecurityEventSeverity
			switch vulnerability.Severity {
			case SeverityCritical:
				severity = EventSeverityCritical
			case SeverityHigh:
				severity = EventSeverityError
			default:
				severity = EventSeverityWarning
			}

			if err := c.LogSecurityEvent(
				ctx,
				EventTypeVulnerabilityFound,
				scanResult.ImageID,
				fmt.Sprintf("%s: %s", vulnerability.Name, vulnerability.Description),
				severity,
				details,
			); err != nil {
				return err
			}
		}
	}

	return nil
}

// LogSecurityEvent logs a security event
func (c *ContainerScanner) LogSecurityEvent(
	ctx context.Context,
	eventType SecurityEventType,
	source string,
	message string,
	severity SecurityEventSeverity,
	details map[string]string,
) error {
	event := SecurityEvent{
		ID:        fmt.Sprintf("EVENT-%d-%d", time.Now().UnixNano()%10000, len(source)),
		EventType: eventType,
		Source:    source,
		Timestamp: time.Now().Unix(),
		Message:   message,
		Details:   details,
		Severity:  severity,
	}

	c.mutex.Lock()
	c.securityEvents = append(c.securityEvents, event)
	c.mutex.Unlock()

	// Log the event
	c.logger.WithFields(logrus.Fields{
		"event_id":   event.ID,
		"event_type": event.EventType,
		"source":     event.Source,
		"severity":   event.Severity,
	}).Info(event.Message)

	// In a real implementation, we would also send alerts for critical events

	return nil
}

// GetSecurityEvents gets all security events
func (c *ContainerScanner) GetSecurityEvents(ctx context.Context) []SecurityEvent {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	// Return a copy of the events to avoid race conditions
	events := make([]SecurityEvent, len(c.securityEvents))
	copy(events, c.securityEvents)
	return events
}

// GetScanResult gets a scan result for an image
func (c *ContainerScanner) GetScanResult(ctx context.Context, imageID string) (*ScanResult, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	result, exists := c.scanResults[imageID]
	if !exists {
		return nil, fmt.Errorf("scan result not found for image: %s", imageID)
	}
	return &result, nil
}

// GetRuntimeCheckResult gets a runtime check result for a container
func (c *ContainerScanner) GetRuntimeCheckResult(ctx context.Context, containerID string) (*RuntimeCheckResult, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	result, exists := c.runtimeChecks[containerID]
	if !exists {
		return nil, fmt.Errorf("runtime check result not found for container: %s", containerID)
	}
	return &result, nil
}
