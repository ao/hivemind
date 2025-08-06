package web

import (
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error   string `json:"error"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ErrorHandler is a middleware that handles errors
func ErrorHandler(logger *logrus.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Process request
		c.Next()

		// Handle errors
		if len(c.Errors) > 0 {
			err := c.Errors.Last().Err
			logger.Errorf("Error handling request: %v", err)

			// Check if it's an API request
			if isAPIRequest(c.Request.URL.Path) {
				c.JSON(http.StatusInternalServerError, ErrorResponse{
					Error:   "internal_server_error",
					Code:    http.StatusInternalServerError,
					Message: err.Error(),
				})
			} else {
				// Render error page for HTML requests
				c.HTML(http.StatusInternalServerError, "error.html", gin.H{
					"error":    err.Error(),
					"back_url": "/",
				})
			}
		}
	}
}

// RecoveryHandler is a middleware that recovers from panics
func RecoveryHandler(logger *logrus.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("Panic recovered: %v\n%s", r, debug.Stack())

				// Check if it's an API request
				if isAPIRequest(c.Request.URL.Path) {
					c.JSON(http.StatusInternalServerError, ErrorResponse{
						Error:   "internal_server_error",
						Code:    http.StatusInternalServerError,
						Message: fmt.Sprintf("Internal server error: %v", r),
					})
				} else {
					// Render error page for HTML requests
					c.HTML(http.StatusInternalServerError, "error.html", gin.H{
						"error":    fmt.Sprintf("Internal server error: %v", r),
						"back_url": "/",
					})
				}

				c.Abort()
			}
		}()

		c.Next()
	}
}

// LoggingMiddleware is a middleware that logs requests
func LoggingMiddleware(logger *logrus.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Start timer
		start := logger.WithFields(logrus.Fields{
			"method": c.Request.Method,
			"path":   c.Request.URL.Path,
			"ip":     c.ClientIP(),
		})

		start.Info("Request started")

		// Process request
		c.Next()

		// End timer
		end := logger.WithFields(logrus.Fields{
			"method":   c.Request.Method,
			"path":     c.Request.URL.Path,
			"ip":       c.ClientIP(),
			"status":   c.Writer.Status(),
			"size":     c.Writer.Size(),
			"duration": c.Writer.Header().Get("X-Response-Time"),
		})

		if len(c.Errors) > 0 {
			end.Error("Request completed with errors")
		} else {
			end.Info("Request completed")
		}
	}
}

// isAPIRequest checks if the request is an API request
func isAPIRequest(path string) bool {
	return len(path) >= 4 && path[:4] == "/api"
}
