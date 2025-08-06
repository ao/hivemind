package security

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// NetworkPolicyManager handles network policies using the NetworkPolicyController
type NetworkPolicyManager struct {
	controller              *NetworkPolicyController
	mutex                   sync.RWMutex
	dynamicPolicyAdjustment bool
	violationThreshold      uint32
	adjustmentCooldown      time.Duration
	lastAdjustment          map[string]time.Time
	logger                  *logrus.Logger
}

// NewNetworkPolicyManager creates a new network policy manager
func NewNetworkPolicyManager(logger *logrus.Logger) *NetworkPolicyManager {
	if logger == nil {
		logger = logrus.New()
		logger.SetLevel(logrus.InfoLevel)
	}

	return &NetworkPolicyManager{
		controller:              NewNetworkPolicyController(logger),
		dynamicPolicyAdjustment: false,
		violationThreshold:      5,         // Default: adjust policy after 5 violations
		adjustmentCooldown:      time.Hour, // Default: 1 hour cooldown between adjustments
		lastAdjustment:          make(map[string]time.Time),
		logger:                  logger,
	}
}

// WithNetworkPolicyController sets the network policy controller for the manager
func (m *NetworkPolicyManager) WithNetworkPolicyController(controller *NetworkPolicyController) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.controller = controller
	m.logger.Debug("Network policy controller has been set")
}

// AddNotificationEndpoint adds a notification endpoint for policy violation alerts
func (m *NetworkPolicyManager) AddNotificationEndpoint(ctx context.Context, endpoint string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.controller.AddNotificationEndpoint(endpoint)
	m.logger.WithField("endpoint", endpoint).Info("Added notification endpoint")
	return nil
}

// SetDynamicPolicyAdjustment enables or disables dynamic policy adjustment
func (m *NetworkPolicyManager) SetDynamicPolicyAdjustment(enabled bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.dynamicPolicyAdjustment = enabled
	m.logger.WithField("enabled", enabled).Info("Set dynamic policy adjustment")
}

// SetViolationThreshold sets the violation threshold for dynamic policy adjustment
func (m *NetworkPolicyManager) SetViolationThreshold(threshold uint32) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.violationThreshold = threshold
	m.logger.WithField("threshold", threshold).Info("Set violation threshold")
}

// SetAdjustmentCooldown sets the cooldown period between policy adjustments
func (m *NetworkPolicyManager) SetAdjustmentCooldown(cooldown time.Duration) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.adjustmentCooldown = cooldown
	m.logger.WithField("cooldown", cooldown).Info("Set adjustment cooldown")
}

// RegisterContainerLabels registers container labels for policy matching
func (m *NetworkPolicyManager) RegisterContainerLabels(ctx context.Context, containerID string, labels map[string]string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Check if we already have an IP for this container
	ip, exists := m.controller.GetContainerIP(containerID)
	if exists {
		// If we have both IP and labels, register the container
		return m.controller.RegisterContainer(ctx, containerID, ip, labels)
	}

	// Otherwise just store the labels
	return m.controller.StoreContainerLabels(ctx, containerID, labels)
}

// RegisterContainerIP registers a container IP address
func (m *NetworkPolicyManager) RegisterContainerIP(ctx context.Context, containerID string, ip net.IP) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Check if we already have labels for this container
	labels, exists := m.controller.GetContainerLabels(containerID)
	if exists {
		// If we have both IP and labels, register the container
		return m.controller.RegisterContainer(ctx, containerID, ip, labels)
	}

	// Otherwise just store the IP
	return m.controller.StoreContainerIP(ctx, containerID, ip)
}

// UnregisterContainer unregisters a container when it's removed
func (m *NetworkPolicyManager) UnregisterContainer(ctx context.Context, containerID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	return m.controller.UnregisterContainer(ctx, containerID)
}

// ApplyPolicy applies a network policy
func (m *NetworkPolicyManager) ApplyPolicy(ctx context.Context, policy NetworkPolicy) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.logger.WithField("policy_name", policy.Name).Info("Applying network policy")

	// Apply the policy using the controller
	if err := m.controller.ApplyPolicy(ctx, policy); err != nil {
		return fmt.Errorf("failed to apply policy: %w", err)
	}

	return nil
}

// AdjustPolicyForViolations dynamically adjusts a network policy based on violation patterns
func (m *NetworkPolicyManager) AdjustPolicyForViolations(ctx context.Context, policyName string) (bool, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Skip if dynamic adjustment is disabled
	if !m.dynamicPolicyAdjustment {
		return false, nil
	}

	// Check cooldown period
	now := time.Now()
	lastTime, exists := m.lastAdjustment[policyName]
	if exists && now.Sub(lastTime) < m.adjustmentCooldown {
		m.logger.WithField("policy_name", policyName).Debug("Skipping policy adjustment due to cooldown period")
		return false, nil
	}

	// Get violation logs
	violations := m.controller.GetViolationLogs(ctx)

	// Count recent violations for this policy
	recentViolations := 0
	for _, v := range violations {
		if v.PolicyName == policyName && time.Since(time.Unix(v.Timestamp, 0)) < 24*time.Hour {
			recentViolations++
		}
	}

	// If violations exceed threshold, adjust the policy
	if uint32(recentViolations) >= m.violationThreshold {
		m.logger.WithFields(logrus.Fields{
			"policy_name":     policyName,
			"violation_count": recentViolations,
		}).Info("Detected violations exceeding threshold, adjusting policy")

		// Get the current policy
		policy, err := m.controller.GetPolicy(ctx, policyName)
		if err != nil {
			return false, fmt.Errorf("failed to get policy: %w", err)
		}

		// Adjust the policy based on violation patterns
		if err := m.strengthenPolicy(ctx, policy, violations); err != nil {
			return false, fmt.Errorf("failed to strengthen policy: %w", err)
		}

		// Apply the updated policy
		if err := m.controller.ApplyPolicy(ctx, *policy); err != nil {
			return false, fmt.Errorf("failed to apply adjusted policy: %w", err)
		}

		// Update last adjustment time
		m.lastAdjustment[policyName] = now

		m.logger.WithFields(logrus.Fields{
			"policy_name":     policyName,
			"violation_count": recentViolations,
		}).Info("Policy adjusted due to violations")

		return true, nil
	}

	return false, nil
}

// strengthenPolicy strengthens a policy based on violation patterns
func (m *NetworkPolicyManager) strengthenPolicy(ctx context.Context, policy *NetworkPolicy, violations []ViolationLog) error {
	// Group violations by source/destination patterns
	violationPatterns := make(map[string][]ViolationLog)

	for _, violation := range violations {
		if violation.PolicyName == policy.Name {
			srcPattern := violation.SourceContainer
			dstPattern := "unknown"
			if violation.DestinationContainer != nil {
				dstPattern = *violation.DestinationContainer
			}
			pattern := fmt.Sprintf("%s->%s", srcPattern, dstPattern)

			violationPatterns[pattern] = append(violationPatterns[pattern], violation)
		}
	}

	// Find the most common violation patterns
	type patternCount struct {
		pattern string
		count   int
	}
	patternCounts := make([]patternCount, 0, len(violationPatterns))
	for pattern, violations := range violationPatterns {
		patternCounts = append(patternCounts, patternCount{
			pattern: pattern,
			count:   len(violations),
		})
	}

	// Sort by count in descending order
	sort.Slice(patternCounts, func(i, j int) bool {
		return patternCounts[i].count > patternCounts[j].count
	})

	// Add or strengthen rules for the top violation patterns
	for i, pc := range patternCounts {
		if i >= 3 || pc.count < 3 {
			break
		}

		pattern := pc.pattern
		count := pc.count

		m.logger.WithFields(logrus.Fields{
			"pattern": pattern,
			"count":   count,
		}).Debug("Adding stricter rule for pattern")

		// Extract source pattern from the combined pattern
		parts := strings.Split(pattern, "->")
		srcPattern := parts[0]

		// Create a new rule or strengthen existing rules
		ruleAdded := false

		for i := range policy.IngressRules {
			rule := &policy.IngressRules[i]
			// If we find a matching rule, make it stricter
			for _, peer := range rule.From {
				if peer.Selector != nil {
					for _, v := range peer.Selector.Labels {
						if strings.Contains(v, srcPattern) {
							// Make the rule stricter
							denyAction := PolicyActionDeny
							rule.Action = &denyAction
							rule.Log = true
							ruleAdded = true
							break
						}
					}
				}
				if ruleAdded {
					break
				}
			}
			if ruleAdded {
				break
			}
		}

		// If no matching rule found, add a new one
		if !ruleAdded {
			// Create a new selector for the source pattern
			selectorLabels := make(map[string]string)

			// Try to extract meaningful labels from the pattern
			if strings.Contains(srcPattern, "frontend") {
				selectorLabels["app"] = "frontend"
			} else if strings.Contains(srcPattern, "backend") {
				selectorLabels["app"] = "backend"
			} else {
				// Use a generic label based on the pattern
				selectorLabels["name"] = srcPattern
			}

			// Create a new rule
			denyAction := PolicyActionDeny
			newRule := NetworkRule{
				Ports: []PortRange{}, // Empty means all ports
				From: []NetworkPeer{
					{
						Selector: &NetworkSelector{
							Labels: selectorLabels,
						},
					},
				},
				Action:      &denyAction,
				Log:         true,
				Description: fmt.Sprintf("Auto-generated rule based on %d violation patterns", count),
				ID:          fmt.Sprintf("auto-%s", uuid.New().String()),
			}

			// Add the new rule
			policy.IngressRules = append(policy.IngressRules, newRule)
		}
	}

	// Increase policy priority to ensure it takes precedence
	policy.Priority += 10

	return nil
}

// RunMaintenance runs maintenance tasks for network policies
func (m *NetworkPolicyManager) RunMaintenance(ctx context.Context) error {
	m.logger.Info("Running network policy maintenance tasks")

	// Run reconciliation to ensure policies are consistently applied
	if err := m.RunReconciliation(ctx); err != nil {
		return fmt.Errorf("failed to run reconciliation: %w", err)
	}

	// Get violation logs
	violations := m.controller.GetSecurityEvents(ctx)

	// Count violations by policy
	policyViolations := make(map[string]int)
	for _, violation := range violations {
		if violation.EventType == EventTypePolicyViolation {
			policyName, exists := violation.Details["policy_name"]
			if exists {
				policyViolations[policyName]++
			}
		}
	}

	// Adjust policies with high violation counts
	for policyName, count := range policyViolations {
		if uint32(count) >= m.violationThreshold {
			if _, err := m.AdjustPolicyForViolations(ctx, policyName); err != nil {
				m.logger.WithError(err).WithField("policy_name", policyName).Error("Failed to adjust policy")
			}
		}
	}

	// Clean up old violation logs (older than 30 days)
	removed, err := m.controller.CleanupOldViolations(ctx, 30)
	if err != nil {
		m.logger.WithError(err).Error("Failed to clean up old violation logs")
	} else if removed > 0 {
		m.logger.WithField("removed_count", removed).Debug("Cleaned up old violation logs")
	}

	m.logger.Info("Network policy maintenance completed")

	return nil
}

// RunReconciliation runs the reconciliation loop to ensure policies are consistently applied
func (m *NetworkPolicyManager) RunReconciliation(ctx context.Context) error {
	return m.controller.RunReconciliation(ctx)
}

// CreateDefaultTenantPolicies creates default network policies for a new tenant
func (m *NetworkPolicyManager) CreateDefaultTenantPolicies(ctx context.Context, tenantID string) error {
	return m.controller.CreateDefaultTenantPolicies(ctx, tenantID)
}

// IsTrafficAllowed checks if traffic is allowed between containers
func (m *NetworkPolicyManager) IsTrafficAllowed(ctx context.Context, srcContainerID, dstContainerID string, protocol Protocol, port uint16) (bool, error) {
	return m.controller.IsTrafficAllowed(ctx, srcContainerID, dstContainerID, protocol, port)
}

// GetViolationLogs gets policy violation logs
func (m *NetworkPolicyManager) GetViolationLogs(ctx context.Context, tenantID string) ([]ViolationLog, error) {
	return m.controller.GetTenantViolationLogs(ctx, tenantID)
}
