package scheduler

import (
	"context"
	"errors"
	"sync"

	"github.com/sirupsen/logrus"
)

// TaskState represents the state of a scheduled task
type TaskState string

const (
	// TaskPending indicates the task is waiting to be scheduled
	TaskPending TaskState = "pending"
	// TaskScheduled indicates the task has been scheduled to a node
	TaskScheduled TaskState = "scheduled"
	// TaskRunning indicates the task is running on a node
	TaskRunning TaskState = "running"
	// TaskCompleted indicates the task has completed successfully
	TaskCompleted TaskState = "completed"
	// TaskFailed indicates the task has failed
	TaskFailed TaskState = "failed"
)

// Task represents a unit of work to be scheduled
type Task struct {
	ID          string
	Name        string
	Image       string
	Command     []string
	Resources   ResourceRequirements
	State       TaskState
	AssignedTo  string
	Constraints []Constraint
	Affinity    []AffinityRule
}

// ResourceRequirements specifies the resources required by a task
type ResourceRequirements struct {
	CPU    float64
	Memory int64
	Disk   int64
}

// Node represents a node in the cluster
type Node struct {
	ID        string
	Name      string
	Address   string
	Available ResourceRequirements
	Used      ResourceRequirements
	Labels    map[string]string
}

// Constraint represents a scheduling constraint
type Constraint interface {
	Satisfied(node *Node) bool
}

// AffinityOperator represents an operator for affinity rules
type AffinityOperator string

const (
	// AffinityOperatorIn represents the "in" operator
	AffinityOperatorIn AffinityOperator = "In"
	// AffinityOperatorNotIn represents the "not in" operator
	AffinityOperatorNotIn AffinityOperator = "NotIn"
	// AffinityOperatorExists represents the "exists" operator
	AffinityOperatorExists AffinityOperator = "Exists"
	// AffinityOperatorDoesNotExist represents the "does not exist" operator
	AffinityOperatorDoesNotExist AffinityOperator = "DoesNotExist"
)

// AffinityRule represents a rule for node affinity
type AffinityRule struct {
	Key      string
	Operator AffinityOperator
	Values   []string
}

// Scheduler is responsible for scheduling tasks to nodes
type Scheduler struct {
	nodes      map[string]*Node
	tasks      map[string]*Task
	nodesMutex sync.RWMutex
	tasksMutex sync.RWMutex
	logger     *logrus.Logger
}

// NewScheduler creates a new scheduler
func NewScheduler(logger *logrus.Logger) *Scheduler {
	return &Scheduler{
		nodes:  make(map[string]*Node),
		tasks:  make(map[string]*Task),
		logger: logger,
	}
}

// RegisterNode registers a node with the scheduler
func (s *Scheduler) RegisterNode(node *Node) {
	s.nodesMutex.Lock()
	defer s.nodesMutex.Unlock()
	s.nodes[node.ID] = node
	s.logger.Infof("Node registered: %s (%s)", node.Name, node.ID)
}

// UnregisterNode removes a node from the scheduler
func (s *Scheduler) UnregisterNode(nodeID string) {
	s.nodesMutex.Lock()
	defer s.nodesMutex.Unlock()
	if node, exists := s.nodes[nodeID]; exists {
		delete(s.nodes, nodeID)
		s.logger.Infof("Node unregistered: %s (%s)", node.Name, node.ID)
	}
}

// SubmitTask submits a task to be scheduled
func (s *Scheduler) SubmitTask(task *Task) {
	s.tasksMutex.Lock()
	defer s.tasksMutex.Unlock()
	task.State = TaskPending
	s.tasks[task.ID] = task
	s.logger.Infof("Task submitted: %s (%s)", task.Name, task.ID)
}

// Schedule attempts to schedule pending tasks to available nodes
func (s *Scheduler) Schedule(ctx context.Context) error {
	s.tasksMutex.Lock()
	defer s.tasksMutex.Unlock()

	pendingTasks := []*Task{}
	for _, task := range s.tasks {
		if task.State == TaskPending {
			pendingTasks = append(pendingTasks, task)
		}
	}

	if len(pendingTasks) == 0 {
		return nil
	}

	s.nodesMutex.RLock()
	defer s.nodesMutex.RUnlock()

	if len(s.nodes) == 0 {
		return errors.New("no nodes available for scheduling")
	}

	for _, task := range pendingTasks {
		bestNode := s.findBestNodeForTask(task)
		if bestNode != nil {
			task.State = TaskScheduled
			task.AssignedTo = bestNode.ID

			// Update node resources
			bestNode.Used.CPU += task.Resources.CPU
			bestNode.Used.Memory += task.Resources.Memory
			bestNode.Used.Disk += task.Resources.Disk

			s.logger.Infof("Task %s (%s) scheduled to node %s (%s)",
				task.Name, task.ID, bestNode.Name, bestNode.ID)
		}
	}

	return nil
}

// findBestNodeForTask finds the best node to run a task based on available resources
// and constraints
func (s *Scheduler) findBestNodeForTask(task *Task) *Node {
	var bestNode *Node
	var bestScore float64 = -1

	for _, node := range s.nodes {
		// Check if node satisfies all constraints
		constraintsSatisfied := true
		for _, constraint := range task.Constraints {
			if !constraint.Satisfied(node) {
				constraintsSatisfied = false
				break
			}
		}

		if !constraintsSatisfied {
			continue
		}

		// Check if node has enough resources
		if node.Available.CPU-node.Used.CPU < task.Resources.CPU ||
			node.Available.Memory-node.Used.Memory < task.Resources.Memory ||
			node.Available.Disk-node.Used.Disk < task.Resources.Disk {
			continue
		}

		// Calculate score (simple implementation - could be more sophisticated)
		// Lower score is better (less resource usage)
		cpuScore := (node.Used.CPU + task.Resources.CPU) / node.Available.CPU
		memScore := float64(node.Used.Memory+task.Resources.Memory) / float64(node.Available.Memory)
		diskScore := float64(node.Used.Disk+task.Resources.Disk) / float64(node.Available.Disk)

		score := (cpuScore + memScore + diskScore) / 3

		if bestNode == nil || score < bestScore {
			bestNode = node
			bestScore = score
		}
	}

	return bestNode
}

// GetTask returns a task by ID
func (s *Scheduler) GetTask(taskID string) (*Task, bool) {
	s.tasksMutex.RLock()
	defer s.tasksMutex.RUnlock()
	task, exists := s.tasks[taskID]
	return task, exists
}

// GetNode returns a node by ID
func (s *Scheduler) GetNode(nodeID string) (*Node, bool) {
	s.nodesMutex.RLock()
	defer s.nodesMutex.RUnlock()
	node, exists := s.nodes[nodeID]
	return node, exists
}
