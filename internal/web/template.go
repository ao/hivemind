package web

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/ao/hivemind/pkg/api"
)

// TemplateFuncs returns a map of template functions
func TemplateFuncs() template.FuncMap {
	return template.FuncMap{
		"formatTime":      formatTime,
		"formatDuration":  formatDuration,
		"formatBytes":     formatBytes,
		"lower":           strings.ToLower,
		"upper":           strings.ToUpper,
		"statusClass":     statusClass,
		"containerStatus": containerStatus,
		"nodeStatus":      nodeStatus,
		"volumeStatus":    volumeStatus,
		"div":             div,
		"len":             length,
	}
}

// length returns the length of a slice, array, map, or string
func length(v interface{}) int {
	switch val := v.(type) {
	case []interface{}:
		return len(val)
	case []string:
		return len(val)
	case string:
		return len(val)
	case map[string]interface{}:
		return len(val)
	case map[string]string:
		return len(val)
	default:
		return 0
	}
}

// div divides two integers
func div(a, b int64) int64 {
	return a / b
}

// formatTime formats a time.Time value
func formatTime(t time.Time) string {
	return t.Format("2006-01-02 15:04:05")
}

// formatDuration formats a duration
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	} else if d < time.Hour {
		return fmt.Sprintf("%.1fm", d.Minutes())
	} else if d < 24*time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return fmt.Sprintf("%.1fd", d.Hours()/24)
}

// formatBytes formats a byte count
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// statusClass returns a CSS class for a status
func statusClass(status string) string {
	status = strings.ToLower(status)
	switch status {
	case "running", "ready", "available", "active":
		return "status-running"
	case "failed", "error", "unhealthy", "terminated":
		return "status-failed"
	case "pending", "starting", "stopping", "draining":
		return "status-pending"
	default:
		return ""
	}
}

// containerStatus returns a human-readable container status
func containerStatus(status api.ContainerStatus) string {
	switch status {
	case api.ContainerRunning:
		return "Running"
	case api.ContainerStopped:
		return "Stopped"
	case api.ContainerPaused:
		return "Paused"
	case api.ContainerFailed:
		return "Failed"
	case api.ContainerCreated:
		return "Created"
	default:
		return string(status)
	}
}

// nodeStatus returns a human-readable node status
func nodeStatus(status api.NodeStatus) string {
	switch status {
	case api.NodeReady:
		return "Ready"
	case api.NodeNotReady:
		return "Not Ready"
	case api.NodeMaintenance:
		return "Maintenance"
	case api.NodeDraining:
		return "Draining"
	default:
		return string(status)
	}
}

// volumeStatus returns a human-readable volume status
func volumeStatus(status api.VolumeStatus) string {
	switch status {
	case api.VolumeAvailable:
		return "Available"
	case api.VolumeInUse:
		return "In Use"
	case api.VolumeFailed:
		return "Failed"
	default:
		return string(status)
	}
}
