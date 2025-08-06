package app

import (
	"strings"
)

// splitCSV splits a comma-separated string into a slice of strings
func splitCSV(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}

// splitKeyValue splits a key-value string into a slice of strings
func splitKeyValue(s string) []string {
	return strings.Split(s, ":")
}

// generateID generates a unique ID for an application
func generateID(name, tenantID string) string {
	return name + "-" + tenantID + "-" + randomString(8)
}

// randomString generates a random string of the specified length
func randomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, length)
	for i := range result {
		result[i] = charset[i%len(charset)]
	}
	return string(result)
}
