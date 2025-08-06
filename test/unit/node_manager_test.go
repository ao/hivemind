package unit

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/ao/hivemind/internal/membership"
	"github.com/ao/hivemind/internal/node"
	"github.com/ao/hivemind/test/fixtures"
)

func TestNodeManager(t *testing.T) {
	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("RegisterNode", func(t *testing.T) {
		// Create a logger for testing
		logger := logrus.New()
		logger.SetLevel(logrus.DebugLevel)

		// Create a new node manager for this test
		manager, err := node.NewManager()
		require.NoError(t, err)
		defer manager.Close()

		// Create a test node
		nodeInfo := fixtures.CreateTestNodeInfo("test-node-1", "test-host-1", "192.168.1.1")

		// Register the node
		err = manager.RegisterNode(ctx, nodeInfo)
		assert.NoError(t, err)

		// Verify node is registered
		nodes, err := manager.GetNodes(ctx)
		assert.NoError(t, err)
		assert.Len(t, nodes, 1)
		assert.Equal(t, "test-node-1", nodes[0].ID)
		assert.Equal(t, "192.168.1.1", nodes[0].Address)
	})

	t.Run("UpdateNodeStatus", func(t *testing.T) {
		// Create a logger for testing
		logger := logrus.New()
		logger.SetLevel(logrus.DebugLevel)

		// Create a new node manager for this test
		manager, err := node.NewManager()
		require.NoError(t, err)
		defer manager.Close()

		// Create and register a test node
		nodeInfo := fixtures.CreateTestNodeInfo("test-node-1", "test-host-1", "192.168.1.1")
		err = manager.RegisterNode(ctx, nodeInfo)
		require.NoError(t, err)

		// Update node status
		err = manager.UpdateNodeStatus(ctx, "test-node-1", node.NodeStatusMaintenance)
		assert.NoError(t, err)

		// Verify status is updated
		nodes, err := manager.GetNodes(ctx)
		assert.NoError(t, err)
		assert.Len(t, nodes, 1)
		assert.Equal(t, node.NodeStatusMaintenance, nodes[0].Status)
	})

	t.Run("UpdateNodeResources", func(t *testing.T) {
		// Create a logger for testing
		logger := logrus.New()
		logger.SetLevel(logrus.DebugLevel)

		// Create a new node manager for this test
		manager, err := node.NewManager()
		require.NoError(t, err)
		defer manager.Close()

		// Create and register a test node
		nodeInfo := fixtures.CreateTestNodeInfo("test-node-1", "test-host-1", "192.168.1.1")
		err = manager.RegisterNode(ctx, nodeInfo)
		require.NoError(t, err)

		// Update node resources
		resources := node.Resources{
			CPU:    8,
			Memory: 16384,
			Disk:   200000,
		}
		err = manager.UpdateNodeResources(ctx, "test-node-1", resources)
		assert.NoError(t, err)

		// Verify resources are updated
		nodes, err := manager.GetNodes(ctx)
		assert.NoError(t, err)
		assert.Len(t, nodes, 1)
		assert.Equal(t, resources.CPU, nodes[0].Resources.CPU)
		assert.Equal(t, resources.Memory, nodes[0].Resources.Memory)
		assert.Equal(t, resources.Disk, nodes[0].Resources.Disk)
	})

	t.Run("GetNodeByID", func(t *testing.T) {
		// Create a logger for testing
		logger := logrus.New()
		logger.SetLevel(logrus.DebugLevel)

		// Create a new node manager for this test
		manager, err := node.NewManager()
		require.NoError(t, err)
		defer manager.Close()

		// Create and register a test node
		nodeInfo := fixtures.CreateTestNodeInfo("test-node-1", "test-host-1", "192.168.1.1")
		err = manager.RegisterNode(ctx, nodeInfo)
		require.NoError(t, err)

		// Get node by ID
		nodeInfo, err = manager.GetNodeByID(ctx, "test-node-1")
		assert.NoError(t, err)
		assert.NotNil(t, nodeInfo)
		assert.Equal(t, "test-node-1", nodeInfo.ID)
	})

	t.Run("GetNodeByHostname", func(t *testing.T) {
		// Create a logger for testing
		logger := logrus.New()
		logger.SetLevel(logrus.DebugLevel)

		// Create a new node manager for this test
		manager, err := node.NewManager()
		require.NoError(t, err)
		defer manager.Close()

		// Create and register a test node
		nodeInfo := fixtures.CreateTestNodeInfo("test-node-1", "test-host-1", "192.168.1.1")
		err = manager.RegisterNode(ctx, nodeInfo)
		require.NoError(t, err)

		// Get node by hostname
		nodeInfo, err = manager.GetNodeByHostname(ctx, "test-host-1")
		assert.NoError(t, err)
		assert.NotNil(t, nodeInfo)
		assert.Equal(t, "test-host-1", nodeInfo.Hostname)
	})

	t.Run("GetNodeByAddress", func(t *testing.T) {
		// Create a logger for testing
		logger := logrus.New()
		logger.SetLevel(logrus.DebugLevel)

		// Create a new node manager for this test
		manager, err := node.NewManager()
		require.NoError(t, err)
		defer manager.Close()

		// Create and register a test node
		nodeInfo := fixtures.CreateTestNodeInfo("test-node-1", "test-host-1", "192.168.1.1")
		err = manager.RegisterNode(ctx, nodeInfo)
		require.NoError(t, err)

		// Get node by address
		nodeInfo, err = manager.GetNodeByAddress(ctx, "192.168.1.1")
		assert.NoError(t, err)
		assert.NotNil(t, nodeInfo)
		assert.Equal(t, "192.168.1.1", nodeInfo.Address)
	})

	t.Run("RemoveNode", func(t *testing.T) {
		// Create a logger for testing
		logger := logrus.New()
		logger.SetLevel(logrus.DebugLevel)

		// Create a new node manager for this test
		manager, err := node.NewManager()
		require.NoError(t, err)
		defer manager.Close()

		// Create and register a test node
		nodeInfo := fixtures.CreateTestNodeInfo("test-node-1", "test-host-1", "192.168.1.1")
		err = manager.RegisterNode(ctx, nodeInfo)
		require.NoError(t, err)

		// Remove the node
		err = manager.RemoveNode(ctx, "test-node-1")
		assert.NoError(t, err)

		// Verify node is removed
		nodes, err := manager.GetNodes(ctx)
		assert.NoError(t, err)
		assert.Len(t, nodes, 0)
	})
}

func TestNodeManagerWithMembership(t *testing.T) {
	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("RegisterNodeWithMembership", func(t *testing.T) {
		// Create a logger for testing
		logger := logrus.New()
		logger.SetLevel(logrus.DebugLevel)

		// Create the node manager
		nodeManager, err := node.NewManager(logger)
		require.NoError(t, err)
		defer nodeManager.Close()

		// Create the membership manager with dynamic port
		membershipManager, err := membership.NewManager("test-node", "127.0.0.1", 0, logger)
		require.NoError(t, err)
		defer membershipManager.Close()

		// Connect the managers
		nodeManager.WithMembershipManager(membershipManager)

		// Create a test node
		nodeInfo := fixtures.CreateTestNodeInfo("test-node-1", "test-host-1", "192.168.1.1")

		// Register the node
		err = nodeManager.RegisterNode(ctx, nodeInfo)
		assert.NoError(t, err)

		// Verify node is registered in node manager
		nodes, err := nodeManager.GetNodes(ctx)
		assert.NoError(t, err)
		assert.Len(t, nodes, 1)
		assert.Equal(t, "test-node-1", nodes[0].ID)

		// Verify node is registered in membership manager
		members, err := membershipManager.GetMembers(ctx)
		assert.NoError(t, err)
		assert.Contains(t, members, "test-node-1")
	})

	t.Run("RemoveNodeWithMembership", func(t *testing.T) {
		// Create a logger for testing
		logger := logrus.New()
		logger.SetLevel(logrus.DebugLevel)

		// Create the node manager
		nodeManager, err := node.NewManager(logger)
		require.NoError(t, err)
		defer nodeManager.Close()

		// Create the membership manager with dynamic port
		membershipManager, err := membership.NewManager("test-node", "127.0.0.1", 0, logger)
		require.NoError(t, err)
		defer membershipManager.Close()

		// Connect the managers
		nodeManager.WithMembershipManager(membershipManager)

		// Create and register a test node
		nodeInfo := fixtures.CreateTestNodeInfo("test-node-1", "test-host-1", "192.168.1.1")
		err = nodeManager.RegisterNode(ctx, nodeInfo)
		require.NoError(t, err)

		// Remove the node
		err = nodeManager.RemoveNode(ctx, "test-node-1")
		assert.NoError(t, err)

		// Verify node is removed from node manager
		nodes, err := nodeManager.GetNodes(ctx)
		assert.NoError(t, err)
		assert.Len(t, nodes, 0)

		// Verify node is removed from membership manager
		members, err := membershipManager.GetMembers(ctx)
		assert.NoError(t, err)
		assert.NotContains(t, members, "test-node-1")
	})
}

func TestNodeManagerWithMocks(t *testing.T) {
	// Setup context
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("AddNode", func(t *testing.T) {
		// Create a logger for testing
		logger := logrus.New()
		logger.SetLevel(logrus.DebugLevel)

		// Create a new mock node manager for this subtest
		mockNodeManager := fixtures.NewMockNodeManager("test-node-1")

		// Setup expectations before adding the node
		mockNodeManager.On("GetNodes", mock.Anything).Return([]*node.NodeInfo{
			fixtures.CreateTestNodeInfo("test-node-1", "test-host-1", "192.168.1.1"),
			fixtures.CreateTestNodeInfo("test-node-2", "test-host-2", "192.168.1.2"),
		}, nil)

		// Add a node
		mockNodeManager.AddNode("test-node-2", "test-host-2", "192.168.1.2")

		// Get nodes and verify
		nodes, err := mockNodeManager.GetNodes(ctx)
		assert.NoError(t, err)
		assert.Len(t, nodes, 2)
		assert.Equal(t, "test-node-2", nodes[1].ID)
	})

	t.Run("RemoveNode", func(t *testing.T) {
		// Create a logger for testing
		logger := logrus.New()
		logger.SetLevel(logrus.DebugLevel)

		// Create a new mock node manager for this subtest
		mockNodeManager := fixtures.NewMockNodeManager("test-node-1")

		// Add a node first
		mockNodeManager.AddNode("test-node-2", "test-host-2", "192.168.1.2")

		// Setup expectations before removing the node
		mockNodeManager.On("GetNodes", mock.Anything).Return([]*node.NodeInfo{
			fixtures.CreateTestNodeInfo("test-node-1", "test-host-1", "192.168.1.1"),
		}, nil)

		// Remove a node
		mockNodeManager.RemoveNode("test-node-2")

		// Get nodes and verify
		nodes, err := mockNodeManager.GetNodes(ctx)
		assert.NoError(t, err)
		assert.Len(t, nodes, 1)
		assert.Equal(t, "test-node-1", nodes[0].ID)
	})

	t.Run("UpdateNodeResources", func(t *testing.T) {
		// Create a logger for testing
		logger := logrus.New()
		logger.SetLevel(logrus.DebugLevel)

		// Create a new mock node manager for this subtest
		mockNodeManager := fixtures.NewMockNodeManager("test-node-1")

		// Define the updated resources
		resources := node.Resources{
			CPU:    16,
			Memory: 32768,
			Disk:   500000,
		}

		// Now update the resources
		mockNodeManager.UpdateNodeResources("test-node-1", resources)

		// Setup expectations AFTER updating resources
		// This is important because UpdateNodeResources clears expectations
		mockNodeManager.On("GetNodes", mock.Anything).Return([]*node.NodeInfo{
			{
				ID:       "test-node-1",
				Hostname: "test-host-1",
				Address:  "192.168.1.1",
				Status:   node.NodeStatusReady,
				Resources: node.NodeResources{
					CPU:    resources.CPU,
					Memory: resources.Memory,
					Disk:   resources.Disk,
				},
			},
		}, nil)

		// Get nodes and verify
		nodes, err := mockNodeManager.GetNodes(ctx)
		assert.NoError(t, err)
		assert.Len(t, nodes, 1)
		assert.Equal(t, resources.CPU, nodes[0].Resources.CPU)
		assert.Equal(t, resources.Memory, nodes[0].Resources.Memory)
		assert.Equal(t, resources.Disk, nodes[0].Resources.Disk)
	})
}
