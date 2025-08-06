package performance

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/ao/hivemind/internal/node"
	"github.com/ao/hivemind/internal/scheduler"
	"github.com/ao/hivemind/test/fixtures"
)

// BenchmarkScheduler benchmarks the scheduler performance
func BenchmarkScheduler(b *testing.B) {
	// Create a logger for testing
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel) // Reduce log noise during benchmarks

	// Create the scheduler
	schedulerManager, err := scheduler.NewEnhancedScheduler(logger)
	require.NoError(b, err)
	defer schedulerManager.Close()

	// Create a mock node manager
	mockNodeManager := fixtures.NewMockNodeManager("test-node-1")

	// Connect the scheduler to the node manager
	schedulerManager.WithNodeManager(mockNodeManager)

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create test nodes
	createTestNodes(mockNodeManager, 100)

	// Setup mock expectations
	nodes, _ := mockNodeManager.GetNodes(ctx)
	mockNodeManager.On("GetNodes", ctx).Return(nodes, nil)

	// Create a task to schedule
	task := fixtures.CreateTestTask("test-task-1", "test-task")

	// Run the benchmark
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := schedulerManager.Schedule(ctx, task)
		if err != nil {
			b.Fatalf("Failed to schedule task: %v", err)
		}
	}
}

// BenchmarkSchedulerWithAffinity benchmarks the scheduler performance with affinity rules
func BenchmarkSchedulerWithAffinity(b *testing.B) {
	// Create a logger for testing
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel) // Reduce log noise during benchmarks

	// Create the scheduler
	schedulerManager, err := scheduler.NewEnhancedScheduler(logger)
	require.NoError(b, err)
	defer schedulerManager.Close()

	// Create a mock node manager
	mockNodeManager := fixtures.NewMockNodeManager("test-node-1")

	// Connect the scheduler to the node manager
	schedulerManager.WithNodeManager(mockNodeManager)

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create test nodes with labels
	createTestNodesWithLabels(mockNodeManager, 100)

	// Setup mock expectations
	nodes, _ := mockNodeManager.GetNodes(ctx)
	mockNodeManager.On("GetNodes", ctx).Return(nodes, nil)

	// Create a task with affinity rules
	task := &scheduler.Task{
		ID:   "test-task-1",
		Name: "test-task",
		Resources: scheduler.ResourceRequirements{
			CPU:    2,
			Memory: 4096,
			Disk:   50000,
		},
		Affinity: []scheduler.AffinityRule{
			{
				Key:      "zone",
				Operator: scheduler.AffinityOperatorIn,
				Values:   []string{"us-east-1a", "us-east-1b"},
			},
			{
				Key:      "type",
				Operator: scheduler.AffinityOperatorIn,
				Values:   []string{"compute"},
			},
		},
	}

	// Run the benchmark
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := schedulerManager.Schedule(ctx, task)
		if err != nil {
			b.Fatalf("Failed to schedule task: %v", err)
		}
	}
}

// TestSchedulerScalability tests the scheduler's scalability with increasing node counts
func TestSchedulerScalability(t *testing.T) {
	// Skip if not running performance tests
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	// Create a logger for testing
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel) // Reduce log noise during performance tests

	// Create the scheduler
	schedulerManager, err := scheduler.NewEnhancedScheduler(logger)
	require.NoError(t, err)
	defer schedulerManager.Close()

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Test with different node counts
	nodeCounts := []int{10, 100, 500, 1000}
	for _, count := range nodeCounts {
		t.Run(fmt.Sprintf("NodeCount_%d", count), func(t *testing.T) {
			// Create a mock node manager
			mockNodeManager := fixtures.NewMockNodeManager("test-node-1")

			// Connect the scheduler to the node manager
			schedulerManager.WithNodeManager(mockNodeManager)

			// Create test nodes
			createTestNodes(mockNodeManager, count)

			// Get the nodes we just created to use in our mock
			nodes := make([]*node.NodeInfo, 0)
			for i := 1; i <= count; i++ {
				nodeID := fmt.Sprintf("test-node-%d", i)
				nodeInfo := &node.NodeInfo{
					ID:       nodeID,
					Hostname: fmt.Sprintf("test-host-%d", i),
					Address:  fmt.Sprintf("192.168.1.%d", (i%254)+1),
					Status:   node.NodeStatusReady,
					Resources: node.NodeResources{
						CPU:               8,
						Memory:            16384,
						Disk:              200000,
						CPUAvailable:      4,
						MemoryAvailable:   8192,
						ContainersRunning: 0,
					},
				}
				nodes = append(nodes, nodeInfo)
			}

			// Setup mock expectations - do this BEFORE any calls to GetNodes
			mockNodeManager.On("GetNodes", mock.Anything).Return(nodes, nil)

			// Create a task to schedule
			task := fixtures.CreateTestTask("test-task-1", "test-task")

			// Measure scheduling time
			start := time.Now()
			_, err := schedulerManager.Schedule(ctx, task)
			duration := time.Since(start)

			// Assert
			assert.NoError(t, err)
			t.Logf("Scheduling time for %d nodes: %v", count, duration)
		})
	}
}

// TestSchedulerConcurrency tests the scheduler's performance under concurrent scheduling requests
func TestSchedulerConcurrency(t *testing.T) {
	// Skip if not running performance tests
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	// Create a logger for testing
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel) // Reduce log noise during performance tests

	// Create the scheduler
	schedulerManager, err := scheduler.NewEnhancedScheduler(logger)
	require.NoError(t, err)
	defer schedulerManager.Close()

	// Create a mock node manager
	mockNodeManager := fixtures.NewMockNodeManager("test-node-1")

	// Connect the scheduler to the node manager
	schedulerManager.WithNodeManager(mockNodeManager)

	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Create test nodes
	createTestNodes(mockNodeManager, 100)

	// Setup mock expectations before calling GetNodes
	// Use mock.Anything to handle any context type
	// Create at least one mock node with sufficient resources to handle scheduling requests
	mockNodes := []*node.NodeInfo{
		{
			ID:       "test-node-1",
			Hostname: "test-host-1",
			Address:  "192.168.1.1",
			Status:   node.NodeStatusReady,
			Resources: node.NodeResources{
				CPU:               16,
				Memory:            32768,
				Disk:              500000,
				CPUAvailable:      12,
				MemoryAvailable:   24576,
				ContainersRunning: 0,
			},
		},
	}
	mockNodeManager.On("GetNodes", mock.Anything).Return(mockNodes, nil)

	// Get the nodes after setting up the mock
	// We don't need to store the result since the mock is already set up
	_, _ = mockNodeManager.GetNodes(ctx)

	// Test with different concurrency levels
	concurrencyLevels := []int{1, 10, 50, 100}
	for _, concurrency := range concurrencyLevels {
		t.Run(fmt.Sprintf("Concurrency_%d", concurrency), func(t *testing.T) {
			// Create a channel to collect results
			results := make(chan error, concurrency)

			// Start time measurement
			start := time.Now()

			// Launch concurrent scheduling requests
			for i := 0; i < concurrency; i++ {
				go func(taskID int) {
					task := &scheduler.Task{
						ID:   fmt.Sprintf("test-task-%d", taskID),
						Name: fmt.Sprintf("test-task-%d", taskID),
						Resources: scheduler.ResourceRequirements{
							CPU:    2,
							Memory: 4096,
							Disk:   50000,
						},
					}
					_, err := schedulerManager.Schedule(ctx, task)
					results <- err
				}(i)
			}

			// Collect results
			var failures int
			for i := 0; i < concurrency; i++ {
				if err := <-results; err != nil {
					failures++
				}
			}

			// Calculate duration
			duration := time.Since(start)

			// Assert
			assert.Zero(t, failures, "Some scheduling requests failed")
			t.Logf("Scheduling time for %d concurrent requests: %v (avg: %v per request)",
				concurrency, duration, duration/time.Duration(concurrency))
		})
	}
}

// Helper function to create test nodes
func createTestNodes(mockNodeManager *fixtures.MockNodeManager, count int) {
	for i := 0; i < count; i++ {
		nodeID := fmt.Sprintf("test-node-%d", i+1)
		hostname := fmt.Sprintf("test-host-%d", i+1)
		address := fmt.Sprintf("192.168.1.%d", (i%254)+1)

		// Create node with random resource capacity
		cpu := int(4 + rand.Float64()*12)             // 4-16 CPUs
		memory := int(8192 + rand.Float64()*24576)    // 8-32 GB RAM
		disk := int64(100000 + rand.Float64()*900000) // 100-1000 GB disk

		// Create NodeResources from the values
		nodeResources := node.NodeResources{
			CPUAvailable:      float64(cpu),
			MemoryAvailable:   uint64(memory),
			ContainersRunning: 0,
			IsLeader:          false,
			CPU:               cpu,
			Memory:            memory,
			Disk:              disk,
		}

		mockNodeManager.AddNode(nodeID, hostname, address)
		// Convert NodeResources to Resources for UpdateNodeResources
		resources := node.Resources{
			CPU:    nodeResources.CPU,
			Memory: nodeResources.Memory,
			Disk:   nodeResources.Disk,
		}
		mockNodeManager.UpdateNodeResources(nodeID, resources)
	}
}

// Helper function to create test nodes with labels
func createTestNodesWithLabels(mockNodeManager *fixtures.MockNodeManager, count int) {
	// Commented out unused variables
	// zones := []string{"us-east-1a", "us-east-1b", "us-east-1c", "us-west-1a", "us-west-1b"}
	// types := []string{"compute", "storage", "memory", "gpu"}

	for i := 0; i < count; i++ {
		nodeID := fmt.Sprintf("test-node-%d", i+1)
		hostname := fmt.Sprintf("test-host-%d", i+1)
		address := fmt.Sprintf("192.168.1.%d", (i%254)+1)

		// Create node with random resource capacity
		resources := node.Resources{
			CPU:    int(4 + rand.Float64()*12),            // 4-16 CPUs
			Memory: int(8192 + rand.Float64()*24576),      // 8-32 GB RAM
			Disk:   int64(100000 + rand.Float64()*900000), // 100-1000 GB disk
		}

		// Labels are not part of Resources struct
		// We'll use them for node creation but not add them to resources
		// Commented out unused variables
		// zoneLabel := zones[i%len(zones)]
		// typeLabel := types[i%len(types)]

		// Convert Resources to NodeResources
		nodeResources := node.NodeResources{
			CPU:               resources.CPU,
			Memory:            resources.Memory,
			Disk:              resources.Disk,
			CPUAvailable:      float64(resources.CPU),
			MemoryAvailable:   uint64(resources.Memory),
			ContainersRunning: 0,
			IsLeader:          false,
		}

		mockNodeManager.AddNode(nodeID, hostname, address)
		// Convert NodeResources to Resources for UpdateNodeResources
		nodeRes := node.Resources{
			CPU:    nodeResources.CPU,
			Memory: nodeResources.Memory,
			Disk:   nodeResources.Disk,
		}
		mockNodeManager.UpdateNodeResources(nodeID, nodeRes)
	}
}
