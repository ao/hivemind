//go:build !linux && !darwin

package router

// openFileLimit is 0: the limit is not read here.
func openFileLimit() int { return 0 }
