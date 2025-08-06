package security

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// QoSPolicyType represents the type of QoS policy
type QoSPolicyType string

const (
	// QoSPolicyTypeIOPS represents an IOPS-based QoS policy
	QoSPolicyTypeIOPS QoSPolicyType = "iops"
	// QoSPolicyTypeBandwidth represents a bandwidth-based QoS policy
	QoSPolicyTypeBandwidth QoSPolicyType = "bandwidth"
	// QoSPolicyTypeLatency represents a latency-based QoS policy
	QoSPolicyTypeLatency QoSPolicyType = "latency"
	// QoSPolicyTypePriority represents a priority-based QoS policy
	QoSPolicyTypePriority QoSPolicyType = "priority"
)

// QoSLimitType represents the type of QoS limit
type QoSLimitType string

const (
	// QoSLimitTypeMin represents a minimum guarantee
	QoSLimitTypeMin QoSLimitType = "min"
	// QoSLimitTypeMax represents a maximum limit
	QoSLimitTypeMax QoSLimitType = "max"
	// QoSLimitTypeBurst represents a burst limit
	QoSLimitTypeBurst QoSLimitType = "burst"
)

// QoSPriority represents the priority level for QoS
type QoSPriority int

const (
	// QoSPriorityLow represents low priority
	QoSPriorityLow QoSPriority = 0
	// QoSPriorityNormal represents normal priority
	QoSPriorityNormal QoSPriority = 50
	// QoSPriorityHigh represents high priority
	QoSPriorityHigh QoSPriority = 100
	// QoSPriorityCritical represents critical priority
	QoSPriorityCritical QoSPriority = 200
)

// QoSPolicy represents a Quality of Service policy for storage resources
type QoSPolicy struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Type        QoSPolicyType     `json:"type"`
	LimitType   QoSLimitType      `json:"limit_type"`
	Value       int64             `json:"value"`
	BurstValue  *int64            `json:"burst_value,omitempty"`
	Priority    QoSPriority       `json:"priority"`
	CreatedAt   int64             `json:"created_at"`
	UpdatedAt   int64             `json:"updated_at"`
	CreatedBy   string            `json:"created_by"`
	IsActive    bool              `json:"is_active"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// QoSAssignment represents an assignment of a QoS policy to a storage resource
type QoSAssignment struct {
	ID           string            `json:"id"`
	PolicyID     string            `json:"policy_id"`
	ResourceID   string            `json:"resource_id"`
	ResourceType string            `json:"resource_type"` // "volume", "pool", etc.
	CreatedAt    int64             `json:"created_at"`
	UpdatedAt    int64             `json:"updated_at"`
	CreatedBy    string            `json:"created_by"`
	IsActive     bool              `json:"is_active"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// QoSViolation represents a violation of a QoS policy
type QoSViolation struct {
	ID           string `json:"id"`
	PolicyID     string `json:"policy_id"`
	AssignmentID string `json:"assignment_id"`
	ResourceID   string `json:"resource_id"`
	ResourceType string `json:"resource_type"`
	Timestamp    int64  `json:"timestamp"`
	Value        int64  `json:"value"`
	Threshold    int64  `json:"threshold"`
	Duration     int64  `json:"duration"` // Duration in seconds
	Resolved     bool   `json:"resolved"`
	ResolvedAt   *int64 `json:"resolved_at,omitempty"`
}

// QoSMetric represents a QoS metric for a storage resource
type QoSMetric struct {
	ResourceID   string `json:"resource_id"`
	ResourceType string `json:"resource_type"`
	MetricType   string `json:"metric_type"` // "iops", "bandwidth", "latency"
	Value        int64  `json:"value"`
	Timestamp    int64  `json:"timestamp"`
}

// StorageQoSManager handles Quality of Service for storage resources
type StorageQoSManager struct {
	policies    map[string]QoSPolicy
	assignments map[string]QoSAssignment
	violations  map[string]QoSViolation
	metrics     []QoSMetric
	mutex       sync.RWMutex
	logger      *logrus.Logger
}

// NewStorageQoSManager creates a new storage QoS manager
func NewStorageQoSManager(logger *logrus.Logger) *StorageQoSManager {
	if logger == nil {
		logger = logrus.New()
		logger.SetLevel(logrus.InfoLevel)
	}

	return &StorageQoSManager{
		policies:    make(map[string]QoSPolicy),
		assignments: make(map[string]QoSAssignment),
		violations:  make(map[string]QoSViolation),
		metrics:     make([]QoSMetric, 0),
		logger:      logger,
	}
}

// CreateQoSPolicy creates a new QoS policy
func (m *StorageQoSManager) CreateQoSPolicy(
	ctx context.Context,
	name string,
	description string,
	policyType QoSPolicyType,
	limitType QoSLimitType,
	value int64,
	burstValue *int64,
	priority QoSPriority,
	createdBy string,
	labels map[string]string,
) (*QoSPolicy, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Validate policy type
	switch policyType {
	case QoSPolicyTypeIOPS, QoSPolicyTypeBandwidth, QoSPolicyTypeLatency, QoSPolicyTypePriority:
		// Valid policy type
	default:
		return nil, fmt.Errorf("invalid policy type: %s", policyType)
	}

	// Validate limit type
	switch limitType {
	case QoSLimitTypeMin, QoSLimitTypeMax, QoSLimitTypeBurst:
		// Valid limit type
	default:
		return nil, fmt.Errorf("invalid limit type: %s", limitType)
	}

	// Validate value
	if value <= 0 {
		return nil, errors.New("value must be positive")
	}

	// Validate burst value if provided
	if burstValue != nil && *burstValue <= value {
		return nil, errors.New("burst value must be greater than value")
	}

	// Generate a new ID
	policyID := uuid.New().String()
	now := time.Now().Unix()

	policy := QoSPolicy{
		ID:          policyID,
		Name:        name,
		Description: description,
		Type:        policyType,
		LimitType:   limitType,
		Value:       value,
		BurstValue:  burstValue,
		Priority:    priority,
		CreatedAt:   now,
		UpdatedAt:   now,
		CreatedBy:   createdBy,
		IsActive:    true,
		Labels:      labels,
	}

	// Store the policy
	m.policies[policyID] = policy

	m.logger.WithFields(logrus.Fields{
		"policy_id":   policyID,
		"policy_name": name,
		"policy_type": policyType,
		"limit_type":  limitType,
		"value":       value,
	}).Info("Created QoS policy")

	return &policy, nil
}

// UpdateQoSPolicy updates an existing QoS policy
func (m *StorageQoSManager) UpdateQoSPolicy(
	ctx context.Context,
	policyID string,
	name *string,
	description *string,
	value *int64,
	burstValue *int64,
	priority *QoSPriority,
	isActive *bool,
	labels map[string]string,
) (*QoSPolicy, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	policy, exists := m.policies[policyID]
	if !exists {
		return nil, fmt.Errorf("policy not found: %s", policyID)
	}

	// Update fields if provided
	if name != nil {
		policy.Name = *name
	}

	if description != nil {
		policy.Description = *description
	}

	if value != nil {
		if *value <= 0 {
			return nil, errors.New("value must be positive")
		}
		policy.Value = *value
	}

	if burstValue != nil {
		if *burstValue <= policy.Value {
			return nil, errors.New("burst value must be greater than value")
		}
		policy.BurstValue = burstValue
	}

	if priority != nil {
		policy.Priority = *priority
	}

	if isActive != nil {
		policy.IsActive = *isActive
	}

	if labels != nil {
		policy.Labels = labels
	}

	policy.UpdatedAt = time.Now().Unix()

	// Store the updated policy
	m.policies[policyID] = policy

	m.logger.WithField("policy_id", policyID).Info("Updated QoS policy")

	return &policy, nil
}

// DeleteQoSPolicy deletes a QoS policy
func (m *StorageQoSManager) DeleteQoSPolicy(ctx context.Context, policyID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if _, exists := m.policies[policyID]; !exists {
		return fmt.Errorf("policy not found: %s", policyID)
	}

	// Check if the policy is assigned to any resources
	for _, assignment := range m.assignments {
		if assignment.PolicyID == policyID && assignment.IsActive {
			return fmt.Errorf("policy is assigned to resource: %s", assignment.ResourceID)
		}
	}

	// Delete the policy
	delete(m.policies, policyID)

	m.logger.WithField("policy_id", policyID).Info("Deleted QoS policy")

	return nil
}

// GetQoSPolicy gets a QoS policy by ID
func (m *StorageQoSManager) GetQoSPolicy(ctx context.Context, policyID string) (*QoSPolicy, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	policy, exists := m.policies[policyID]
	if !exists {
		return nil, fmt.Errorf("policy not found: %s", policyID)
	}

	return &policy, nil
}

// ListQoSPolicies lists all QoS policies
func (m *StorageQoSManager) ListQoSPolicies(ctx context.Context) []QoSPolicy {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	policies := make([]QoSPolicy, 0, len(m.policies))
	for _, policy := range m.policies {
		policies = append(policies, policy)
	}

	return policies
}

// AssignQoSPolicy assigns a QoS policy to a storage resource
func (m *StorageQoSManager) AssignQoSPolicy(
	ctx context.Context,
	policyID string,
	resourceID string,
	resourceType string,
	createdBy string,
	labels map[string]string,
) (*QoSAssignment, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Check if the policy exists
	if _, exists := m.policies[policyID]; !exists {
		return nil, fmt.Errorf("policy not found: %s", policyID)
	}

	// Check if the resource already has this policy assigned
	for _, assignment := range m.assignments {
		if assignment.ResourceID == resourceID && assignment.PolicyID == policyID && assignment.IsActive {
			return nil, fmt.Errorf("policy already assigned to resource: %s", resourceID)
		}
	}

	// Generate a new ID
	assignmentID := uuid.New().String()
	now := time.Now().Unix()

	assignment := QoSAssignment{
		ID:           assignmentID,
		PolicyID:     policyID,
		ResourceID:   resourceID,
		ResourceType: resourceType,
		CreatedAt:    now,
		UpdatedAt:    now,
		CreatedBy:    createdBy,
		IsActive:     true,
		Labels:       labels,
	}

	// Store the assignment
	m.assignments[assignmentID] = assignment

	m.logger.WithFields(logrus.Fields{
		"assignment_id": assignmentID,
		"policy_id":     policyID,
		"resource_id":   resourceID,
		"resource_type": resourceType,
	}).Info("Assigned QoS policy to resource")

	return &assignment, nil
}

// UnassignQoSPolicy unassigns a QoS policy from a storage resource
func (m *StorageQoSManager) UnassignQoSPolicy(
	ctx context.Context,
	assignmentID string,
) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	assignment, exists := m.assignments[assignmentID]
	if !exists {
		return fmt.Errorf("assignment not found: %s", assignmentID)
	}

	// Deactivate the assignment
	assignment.IsActive = false
	assignment.UpdatedAt = time.Now().Unix()
	m.assignments[assignmentID] = assignment

	m.logger.WithFields(logrus.Fields{
		"assignment_id": assignmentID,
		"policy_id":     assignment.PolicyID,
		"resource_id":   assignment.ResourceID,
	}).Info("Unassigned QoS policy from resource")

	return nil
}

// GetQoSAssignment gets a QoS assignment by ID
func (m *StorageQoSManager) GetQoSAssignment(ctx context.Context, assignmentID string) (*QoSAssignment, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	assignment, exists := m.assignments[assignmentID]
	if !exists {
		return nil, fmt.Errorf("assignment not found: %s", assignmentID)
	}

	return &assignment, nil
}

// ListQoSAssignments lists all QoS assignments
func (m *StorageQoSManager) ListQoSAssignments(ctx context.Context) []QoSAssignment {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	assignments := make([]QoSAssignment, 0, len(m.assignments))
	for _, assignment := range m.assignments {
		assignments = append(assignments, assignment)
	}

	return assignments
}

// ListQoSAssignmentsByResource lists QoS assignments for a specific resource
func (m *StorageQoSManager) ListQoSAssignmentsByResource(
	ctx context.Context,
	resourceID string,
) []QoSAssignment {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	assignments := make([]QoSAssignment, 0)
	for _, assignment := range m.assignments {
		if assignment.ResourceID == resourceID && assignment.IsActive {
			assignments = append(assignments, assignment)
		}
	}

	return assignments
}

// ListQoSAssignmentsByPolicy lists QoS assignments for a specific policy
func (m *StorageQoSManager) ListQoSAssignmentsByPolicy(
	ctx context.Context,
	policyID string,
) []QoSAssignment {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	assignments := make([]QoSAssignment, 0)
	for _, assignment := range m.assignments {
		if assignment.PolicyID == policyID && assignment.IsActive {
			assignments = append(assignments, assignment)
		}
	}

	return assignments
}

// RecordQoSMetric records a QoS metric for a storage resource
func (m *StorageQoSManager) RecordQoSMetric(
	ctx context.Context,
	resourceID string,
	resourceType string,
	metricType string,
	value int64,
) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	now := time.Now().Unix()

	metric := QoSMetric{
		ResourceID:   resourceID,
		ResourceType: resourceType,
		MetricType:   metricType,
		Value:        value,
		Timestamp:    now,
	}

	// Store the metric
	m.metrics = append(m.metrics, metric)

	// Check for violations
	m.checkViolations(resourceID, resourceType, metricType, value)

	// Limit the number of metrics stored
	if len(m.metrics) > 10000 {
		m.metrics = m.metrics[len(m.metrics)-10000:]
	}
}

// checkViolations checks for QoS policy violations
func (m *StorageQoSManager) checkViolations(
	resourceID string,
	resourceType string,
	metricType string,
	value int64,
) {
	// Get active assignments for this resource
	var activeAssignments []QoSAssignment
	for _, assignment := range m.assignments {
		if assignment.ResourceID == resourceID && assignment.IsActive {
			activeAssignments = append(activeAssignments, assignment)
		}
	}

	// Check each assignment for violations
	for _, assignment := range activeAssignments {
		policy, exists := m.policies[assignment.PolicyID]
		if !exists || !policy.IsActive {
			continue
		}

		// Skip if policy type doesn't match metric type
		if (policy.Type == QoSPolicyTypeIOPS && metricType != "iops") ||
			(policy.Type == QoSPolicyTypeBandwidth && metricType != "bandwidth") ||
			(policy.Type == QoSPolicyTypeLatency && metricType != "latency") {
			continue
		}

		// Check for violation based on limit type
		var violated bool
		var threshold int64

		switch policy.LimitType {
		case QoSLimitTypeMin:
			violated = value < policy.Value
			threshold = policy.Value
		case QoSLimitTypeMax:
			violated = value > policy.Value
			threshold = policy.Value
		case QoSLimitTypeBurst:
			if policy.BurstValue != nil {
				violated = value > *policy.BurstValue
				threshold = *policy.BurstValue
			}
		}

		if violated {
			// Create a new violation
			violationID := uuid.New().String()
			now := time.Now().Unix()

			violation := QoSViolation{
				ID:           violationID,
				PolicyID:     policy.ID,
				AssignmentID: assignment.ID,
				ResourceID:   resourceID,
				ResourceType: resourceType,
				Timestamp:    now,
				Value:        value,
				Threshold:    threshold,
				Duration:     0,
				Resolved:     false,
			}

			// Store the violation
			m.violations[violationID] = violation

			m.logger.WithFields(logrus.Fields{
				"violation_id":  violationID,
				"policy_id":     policy.ID,
				"resource_id":   resourceID,
				"resource_type": resourceType,
				"metric_type":   metricType,
				"value":         value,
				"threshold":     threshold,
			}).Warn("QoS policy violation detected")
		}
	}
}

// GetQoSViolation gets a QoS violation by ID
func (m *StorageQoSManager) GetQoSViolation(ctx context.Context, violationID string) (*QoSViolation, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	violation, exists := m.violations[violationID]
	if !exists {
		return nil, fmt.Errorf("violation not found: %s", violationID)
	}

	return &violation, nil
}

// ListQoSViolations lists all QoS violations
func (m *StorageQoSManager) ListQoSViolations(ctx context.Context, includeResolved bool) []QoSViolation {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	violations := make([]QoSViolation, 0, len(m.violations))
	for _, violation := range m.violations {
		if includeResolved || !violation.Resolved {
			violations = append(violations, violation)
		}
	}

	return violations
}

// ListQoSViolationsByResource lists QoS violations for a specific resource
func (m *StorageQoSManager) ListQoSViolationsByResource(
	ctx context.Context,
	resourceID string,
	includeResolved bool,
) []QoSViolation {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	violations := make([]QoSViolation, 0)
	for _, violation := range m.violations {
		if violation.ResourceID == resourceID && (includeResolved || !violation.Resolved) {
			violations = append(violations, violation)
		}
	}

	return violations
}

// ResolveQoSViolation marks a QoS violation as resolved
func (m *StorageQoSManager) ResolveQoSViolation(ctx context.Context, violationID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	violation, exists := m.violations[violationID]
	if !exists {
		return fmt.Errorf("violation not found: %s", violationID)
	}

	if violation.Resolved {
		return fmt.Errorf("violation already resolved: %s", violationID)
	}

	// Mark as resolved
	now := time.Now().Unix()
	violation.Resolved = true
	violation.ResolvedAt = &now
	violation.Duration = now - violation.Timestamp
	m.violations[violationID] = violation

	m.logger.WithFields(logrus.Fields{
		"violation_id":  violationID,
		"policy_id":     violation.PolicyID,
		"resource_id":   violation.ResourceID,
		"resource_type": violation.ResourceType,
		"duration":      violation.Duration,
	}).Info("QoS policy violation resolved")

	return nil
}

// GetQoSMetrics gets QoS metrics for a specific resource
func (m *StorageQoSManager) GetQoSMetrics(
	ctx context.Context,
	resourceID string,
	metricType string,
	startTime int64,
	endTime int64,
) []QoSMetric {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	metrics := make([]QoSMetric, 0)
	for _, metric := range m.metrics {
		if metric.ResourceID == resourceID &&
			(metricType == "" || metric.MetricType == metricType) &&
			metric.Timestamp >= startTime &&
			metric.Timestamp <= endTime {
			metrics = append(metrics, metric)
		}
	}

	return metrics
}

// EnforceQoSPolicies enforces QoS policies for all resources
func (m *StorageQoSManager) EnforceQoSPolicies(ctx context.Context) error {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	// In a real implementation, this would enforce QoS policies
	// For now, we'll just log that enforcement is happening

	activeAssignments := 0
	for _, assignment := range m.assignments {
		if assignment.IsActive {
			activeAssignments++
		}
	}

	m.logger.WithField("active_assignments", activeAssignments).Info("Enforcing QoS policies")

	return nil
}

// CleanupOldMetrics removes old metrics
func (m *StorageQoSManager) CleanupOldMetrics(ctx context.Context, olderThan int64) int {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	now := time.Now().Unix()
	cutoff := now - olderThan

	newMetrics := make([]QoSMetric, 0)
	removed := 0

	for _, metric := range m.metrics {
		if metric.Timestamp >= cutoff {
			newMetrics = append(newMetrics, metric)
		} else {
			removed++
		}
	}

	m.metrics = newMetrics

	m.logger.WithField("removed_count", removed).Info("Cleaned up old QoS metrics")

	return removed
}
