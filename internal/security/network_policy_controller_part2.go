package security

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// RunReconciliation runs the reconciliation loop to ensure policies are consistently applied
func (c *NetworkPolicyController) RunReconciliation(ctx context.Context) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	now := time.Now()

	// Only run reconciliation if enough time has passed since last run
	if now.Sub(c.lastReconciliation) < c.reconciliationInterval {
		return nil
	}

	c.logger.Info("Running network policy reconciliation...")

	// Update last reconciliation time
	c.lastReconciliation = now

	// Ensure all policy chains exist
	for _, chainName := range c.policyChains {
		if err := c.ensureChainExists(ctx, chainName); err != nil {
			return fmt.Errorf("failed to ensure chain exists: %w", err)
		}
	}

	// For each container, apply all matching policies
	containerIDs := make([]string, 0, len(c.containerLabels))
	for containerID := range c.containerLabels {
		containerIDs = append(containerIDs, containerID)
	}

	c.logger.WithField("container_count", len(containerIDs)).Debug("Reconciling policies for containers")

	appliedPolicies := 0
	for _, containerID := range containerIDs {
		if err := c.applyPoliciesToContainer(ctx, containerID); err != nil {
			c.logger.WithError(err).WithField("container_id", containerID).Error("Failed to apply policies to container")
			continue
		}
		appliedPolicies++
	}

	// Check for policy violations in system logs
	if err := c.CheckPolicyViolations(ctx); err != nil {
		c.logger.WithError(err).Error("Failed to check policy violations")
	}

	c.logger.WithField("applied_policies", appliedPolicies).Info("Network policy reconciliation completed successfully")

	return nil
}

// CheckPolicyViolations checks for policy violations in system logs
func (c *NetworkPolicyController) CheckPolicyViolations(ctx context.Context) error {
	c.logger.Debug("Checking for network policy violations in system logs")

	// Skip iptables operations in tests
	// This check was added to allow tests to run without requiring actual iptables operations
	if c.SkipIptablesOperations {
		return nil
	}

	// Get all container IPs
	c.mutex.RLock()
	ipToContainer := make(map[string]string)
	for id, ip := range c.containerIPs {
		ipToContainer[ip.String()] = id
	}
	c.mutex.RUnlock()

	// Try to read the kernel log for iptables messages
	cmd := exec.CommandContext(ctx, "grep", "-E", "HIVEMIND-POLICY-.*: ", "/var/log/kern.log")
	output, err := cmd.Output()
	if err != nil {
		// If the command fails, it might be because there are no matches or the file doesn't exist
		// This is not necessarily an error
		return nil
	}

	logContent := string(output)
	lines := strings.Split(logContent, "\n")
	violationCount := 0

	for _, line := range lines {
		if line == "" {
			continue
		}

		// Parse the log line to extract policy violation information
		policyName := c.extractPolicyName(line)
		if policyName == "" {
			continue
		}

		connectionInfo := c.extractConnectionInfo(line)
		if connectionInfo == nil {
			continue
		}

		srcIP := connectionInfo.srcIP
		dstIP := connectionInfo.dstIP
		protocol := connectionInfo.protocol
		port := connectionInfo.port

		// Find container IDs for the IPs
		srcContainerID, srcExists := ipToContainer[srcIP.String()]
		dstContainerID, dstExists := ipToContainer[dstIP.String()]

		if !srcExists {
			continue
		}

		// Get container labels to determine tenant ID
		c.mutex.RLock()
		var tenantID *string
		if labels, exists := c.containerLabels[srcContainerID]; exists {
			if tenant, exists := labels["tenant"]; exists {
				tenantID = &tenant
			}
		}
		c.mutex.RUnlock()

		// Determine severity based on policy name or other factors
		severity := ViolationSeverityInfo
		if strings.Contains(policyName, "critical") {
			severity = ViolationSeverityCritical
		} else if strings.Contains(policyName, "warning") {
			severity = ViolationSeverityWarning
		}

		// Record the violation
		var dstContainerIDPtr *string
		if dstExists {
			dstContainerIDPtr = &dstContainerID
		}

		if err := c.RecordViolation(
			ctx,
			srcContainerID,
			srcIP,
			dstContainerIDPtr,
			dstIP,
			protocol,
			port,
			policyName,
			nil, // We don't have rule ID from the log
			PolicyActionDeny,
			tenantID,
			severity,
		); err != nil {
			c.logger.WithError(err).Error("Failed to record violation")
			continue
		}

		violationCount++
	}

	if violationCount > 0 {
		c.logger.WithField("violation_count", violationCount).Info("Found network policy violations in logs")
	}

	return nil
}

// ConnectionInfo represents information about a network connection
type ConnectionInfo struct {
	srcIP    net.IP
	dstIP    net.IP
	protocol Protocol
	port     uint16
}

// extractPolicyName extracts the policy name from a log line
func (c *NetworkPolicyController) extractPolicyName(logLine string) string {
	re := regexp.MustCompile(`HIVEMIND-POLICY-([^:]+):`)
	matches := re.FindStringSubmatch(logLine)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

// extractConnectionInfo extracts connection information from a log line
func (c *NetworkPolicyController) extractConnectionInfo(logLine string) *ConnectionInfo {
	// Extract source IP
	srcRe := regexp.MustCompile(`SRC=(\d+\.\d+\.\d+\.\d+)`)
	srcMatches := srcRe.FindStringSubmatch(logLine)
	if len(srcMatches) < 2 {
		return nil
	}
	srcIP := net.ParseIP(srcMatches[1])
	if srcIP == nil {
		return nil
	}

	// Extract destination IP
	dstRe := regexp.MustCompile(`DST=(\d+\.\d+\.\d+\.\d+)`)
	dstMatches := dstRe.FindStringSubmatch(logLine)
	if len(dstMatches) < 2 {
		return nil
	}
	dstIP := net.ParseIP(dstMatches[1])
	if dstIP == nil {
		return nil
	}

	// Extract protocol
	protoRe := regexp.MustCompile(`PROTO=(\w+)`)
	protoMatches := protoRe.FindStringSubmatch(logLine)
	if len(protoMatches) < 2 {
		return nil
	}
	var protocol Protocol
	switch strings.ToUpper(protoMatches[1]) {
	case "TCP":
		protocol = ProtocolTCP
	case "UDP":
		protocol = ProtocolUDP
	default:
		return nil
	}

	// Extract port
	portRe := regexp.MustCompile(`DPT=(\d+)`)
	portMatches := portRe.FindStringSubmatch(logLine)
	if len(portMatches) < 2 {
		return nil
	}
	port, err := strconv.ParseUint(portMatches[1], 10, 16)
	if err != nil {
		return nil
	}

	return &ConnectionInfo{
		srcIP:    srcIP,
		dstIP:    dstIP,
		protocol: protocol,
		port:     uint16(port),
	}
}

// IsTrafficAllowed checks if traffic is allowed between two containers
func (c *NetworkPolicyController) IsTrafficAllowed(
	ctx context.Context,
	srcContainerID string,
	dstContainerID string,
	protocol Protocol,
	port uint16,
) (bool, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	// Get container labels
	srcLabels, exists := c.containerLabels[srcContainerID]
	if !exists {
		return false, fmt.Errorf("source container not found: %s", srcContainerID)
	}

	dstLabels, exists := c.containerLabels[dstContainerID]
	if !exists {
		return false, fmt.Errorf("destination container not found: %s", dstContainerID)
	}

	// Get container IPs
	srcIP, exists := c.containerIPs[srcContainerID]
	if !exists {
		return false, fmt.Errorf("source container IP not found: %s", srcContainerID)
	}

	dstIP, exists := c.containerIPs[dstContainerID]
	if !exists {
		return false, fmt.Errorf("destination container IP not found: %s", dstContainerID)
	}

	// Check against all policies
	for _, policy := range c.policies {
		// Check if policy applies to source container (egress)
		if c.labelsMatchSelector(srcLabels, policy.Selector.Labels) {
			// Check egress rules
			for _, rule := range policy.EgressRules {
				// Check if the rule allows traffic to the destination
				for _, portRange := range rule.Ports {
					if portRange.Protocol == protocol &&
						port >= portRange.PortMin &&
						port <= portRange.PortMax {
						// Check if the destination is allowed by any peer in the rule
						for _, peer := range rule.From {
							if peer.IPBlock != nil {
								// Check if destination IP is in the CIDR block
								_, ipNet, err := net.ParseCIDR(peer.IPBlock.CIDR)
								if err == nil && ipNet.Contains(dstIP) {
									return true, nil
								}
							}

							if peer.Selector != nil && c.labelsMatchSelector(dstLabels, peer.Selector.Labels) {
								return true, nil
							}
						}
					}
				}
			}
		}

		// Check if policy applies to destination container (ingress)
		if c.labelsMatchSelector(dstLabels, policy.Selector.Labels) {
			// Check ingress rules
			for _, rule := range policy.IngressRules {
				// Check if the rule allows traffic from the source
				for _, portRange := range rule.Ports {
					if portRange.Protocol == protocol &&
						port >= portRange.PortMin &&
						port <= portRange.PortMax {
						// Check if the source is allowed by any peer in the rule
						for _, peer := range rule.From {
							if peer.IPBlock != nil {
								// Check if source IP is in the CIDR block
								_, ipNet, err := net.ParseCIDR(peer.IPBlock.CIDR)
								if err == nil && ipNet.Contains(srcIP) {
									return true, nil
								}
							}

							if peer.Selector != nil && c.labelsMatchSelector(srcLabels, peer.Selector.Labels) {
								return true, nil
							}
						}
					}
				}
			}
		}
	}

	// If no policy explicitly allows the traffic, deny it
	return false, nil
}

// GetTenantViolationLogs gets violation logs for a specific tenant
func (c *NetworkPolicyController) GetTenantViolationLogs(ctx context.Context, tenantID string) ([]ViolationLog, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	logs := make([]ViolationLog, 0)
	for _, log := range c.violations {
		if log.TenantID != nil && *log.TenantID == tenantID {
			logs = append(logs, log)
		}
	}

	return logs, nil
}

// RecordViolation records a policy violation
func (c *NetworkPolicyController) RecordViolation(
	ctx context.Context,
	sourceContainer string,
	sourceIP net.IP,
	destinationContainer *string,
	destinationIP net.IP,
	protocol Protocol,
	port uint16,
	policyName string,
	ruleID *string,
	action PolicyAction,
	tenantID *string,
	severity ViolationSeverity,
) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// Generate a unique ID for the violation
	id := fmt.Sprintf("VIOLATION-%d-%s", time.Now().UnixNano(), sourceContainer)

	// Create the violation log
	violation := ViolationLog{
		ID:                   id,
		Timestamp:            time.Now().Unix(),
		SourceContainer:      sourceContainer,
		SourceIP:             sourceIP,
		DestinationContainer: destinationContainer,
		DestinationIP:        destinationIP,
		Protocol:             protocol,
		Port:                 port,
		PolicyName:           policyName,
		RuleID:               ruleID,
		Action:               action,
		TenantID:             tenantID,
		Severity:             severity,
		Resolved:             false,
	}

	// Add to logs
	c.violations = append(c.violations, violation)

	// If we have too many logs, remove the oldest ones
	if len(c.violations) > c.maxLogEntries {
		c.violations = c.violations[len(c.violations)-c.maxLogEntries:]
	}

	// Log the violation
	c.logger.WithFields(logrus.Fields{
		"violation_id":     id,
		"source_container": sourceContainer,
		"destination_ip":   destinationIP.String(),
		"protocol":         protocol,
		"port":             port,
		"policy_name":      policyName,
		"severity":         severity,
	}).Info("Network policy violation recorded")

	// Send notification if configured
	if err := c.SendViolationNotification(ctx, &violation); err != nil {
		c.logger.WithError(err).Error("Failed to send violation notification")
	}

	return nil
}

// ResolveViolation resolves a policy violation
func (c *NetworkPolicyController) ResolveViolation(ctx context.Context, violationID string, resolutionAction string) (bool, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	for i := range c.violations {
		if c.violations[i].ID == violationID {
			c.violations[i].Resolved = true
			now := time.Now().Unix()
			c.violations[i].ResolutionTimestamp = &now
			c.violations[i].ResolutionAction = &resolutionAction

			c.logger.WithFields(logrus.Fields{
				"violation_id":      violationID,
				"resolution_action": resolutionAction,
			}).Info("Network policy violation resolved")

			return true, nil
		}
	}

	return false, fmt.Errorf("violation not found: %s", violationID)
}

// SendViolationNotification sends a notification for a policy violation
func (c *NetworkPolicyController) SendViolationNotification(ctx context.Context, violation *ViolationLog) error {
	if len(c.notificationEndpoints) == 0 {
		return nil
	}

	// Convert severity to string
	var severityStr string
	switch violation.Severity {
	case ViolationSeverityCritical:
		severityStr = "CRITICAL"
	case ViolationSeverityWarning:
		severityStr = "WARNING"
	default:
		severityStr = "INFO"
	}

	// Create notification payload
	payload := map[string]interface{}{
		"id":               violation.ID,
		"timestamp":        violation.Timestamp,
		"source_container": violation.SourceContainer,
		"source_ip":        violation.SourceIP.String(),
		"destination_ip":   violation.DestinationIP.String(),
		"protocol":         string(violation.Protocol),
		"port":             violation.Port,
		"policy_name":      violation.PolicyName,
		"action":           string(violation.Action),
		"severity":         severityStr,
	}

	if violation.DestinationContainer != nil {
		payload["destination_container"] = *violation.DestinationContainer
	}

	if violation.TenantID != nil {
		payload["tenant_id"] = *violation.TenantID
	}

	if violation.RuleID != nil {
		payload["rule_id"] = *violation.RuleID
	}

	// Convert payload to JSON
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal notification payload: %w", err)
	}

	// Send notification to all endpoints
	for _, endpoint := range c.notificationEndpoints {
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(jsonPayload)))
		if err != nil {
			c.logger.WithError(err).WithField("endpoint", endpoint).Error("Failed to create notification request")
			continue
		}

		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			c.logger.WithError(err).WithField("endpoint", endpoint).Error("Failed to send notification")
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			c.logger.WithFields(logrus.Fields{
				"endpoint":     endpoint,
				"violation_id": violation.ID,
			}).Debug("Notification sent successfully")
		} else {
			c.logger.WithFields(logrus.Fields{
				"endpoint":     endpoint,
				"violation_id": violation.ID,
				"status_code":  resp.StatusCode,
			}).Error("Failed to send notification")
		}
	}

	return nil
}

// CleanupOldViolations removes old violation logs
func (c *NetworkPolicyController) CleanupOldViolations(ctx context.Context, maxAgeDays int) (int, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if maxAgeDays <= 0 {
		return 0, fmt.Errorf("max age must be positive")
	}

	cutoff := time.Now().AddDate(0, 0, -maxAgeDays).Unix()
	newLogs := make([]ViolationLog, 0, len(c.violations))
	removed := 0

	for _, log := range c.violations {
		if log.Timestamp >= cutoff {
			newLogs = append(newLogs, log)
		} else {
			removed++
		}
	}

	c.violations = newLogs
	c.logger.WithField("removed_count", removed).Info("Cleaned up old violation logs")

	return removed, nil
}

// CreateDefaultTenantPolicies creates default network policies for a tenant
func (c *NetworkPolicyController) CreateDefaultTenantPolicies(ctx context.Context, tenantID string) error {
	c.logger.WithField("tenant_id", tenantID).Info("Creating default network policies for tenant")

	// Create a default isolation policy
	isolationPolicy := NetworkPolicy{
		Name: fmt.Sprintf("tenant-%s-isolation", tenantID),
		Selector: NetworkSelector{
			Labels: map[string]string{
				"tenant": tenantID,
			},
		},
		IngressRules: []NetworkRule{
			{
				Ports: []PortRange{
					{
						Protocol: ProtocolTCP,
						PortMin:  1,
						PortMax:  65535,
					},
					{
						Protocol: ProtocolUDP,
						PortMin:  1,
						PortMax:  65535,
					},
				},
				From: []NetworkPeer{
					{
						Selector: &NetworkSelector{
							Labels: map[string]string{
								"tenant": tenantID,
							},
						},
					},
				},
				Log:         true,
				Description: "Allow traffic from same tenant",
				ID:          "default-ingress-1",
			},
		},
		EgressRules: []NetworkRule{
			{
				Ports: []PortRange{
					{
						Protocol: ProtocolTCP,
						PortMin:  1,
						PortMax:  65535,
					},
					{
						Protocol: ProtocolUDP,
						PortMin:  1,
						PortMax:  65535,
					},
				},
				From:        []NetworkPeer{},
				Log:         true,
				Description: "Allow all outbound traffic",
				ID:          "default-egress-1",
			},
		},
		Priority:  100,
		Namespace: "",
		TenantID:  tenantID,
		Labels: map[string]string{
			"type":    "default",
			"tenant":  tenantID,
			"managed": "true",
		},
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
	}

	// Apply the policy
	if err := c.ApplyPolicy(ctx, isolationPolicy); err != nil {
		return fmt.Errorf("failed to apply isolation policy: %w", err)
	}

	return nil
}
