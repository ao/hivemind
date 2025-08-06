package scheduler

// WithNodeManager sets the node manager
func (s *EnhancedScheduler) WithNodeManager(nodeManager interface{}) *EnhancedScheduler {
	// Store the node manager for use in scheduling
	s.nodeManager = nodeManager
	s.logger.Info("Node manager set")
	return s
}

// getRandomInt64 returns a random int64 value
func (s *EnhancedScheduler) getRandomInt64() int64 {
	// In a real implementation, this would use a proper random number generator
	// For now, we'll just use a simple counter
	return int64(len(s.tasks))
}

// No duplicate Close method needed as it's already defined in enhanced_scheduler.go
