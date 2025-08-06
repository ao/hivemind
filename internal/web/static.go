package web

import (
	"net/http"
	"path/filepath"

	"github.com/gin-gonic/gin"
)

// StaticHandler handles static file requests
type StaticHandler struct {
	fs     http.FileSystem
	prefix string
}

// NewStaticHandler creates a new static file handler
func NewStaticHandler(dir string, prefix string) *StaticHandler {
	return &StaticHandler{
		fs:     http.Dir(dir),
		prefix: prefix,
	}
}

// Handle handles static file requests
func (h *StaticHandler) Handle(c *gin.Context) {
	path := c.Param("path")
	if path == "" {
		c.Status(http.StatusNotFound)
		return
	}

	// Clean the path to prevent directory traversal
	path = filepath.Clean(path)
	if path == "." {
		path = ""
	}

	// Serve the file
	http.FileServer(h.fs).ServeHTTP(c.Writer, c.Request)
}

// ServeStaticFiles serves static files from the given directory
func (ws *WebServer) ServeStaticFiles(dir string, urlPrefix string) {
	handler := NewStaticHandler(dir, urlPrefix)
	ws.router.GET(urlPrefix+"/*path", handler.Handle)
}

// ServeCSS serves a CSS file with the given content
func (ws *WebServer) ServeCSS(c *gin.Context) {
	c.Header("Content-Type", "text/css")
	c.String(http.StatusOK, `
		body {
			font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
			line-height: 1.6;
			color: #333;
			margin: 0;
			padding: 0;
			background-color: #f5f7fa;
		}
		.container {
			width: 90%;
			max-width: 1200px;
			margin: 0 auto;
			padding: 20px;
		}
		.navbar {
			background-color: #2c3e50;
			color: white;
			padding: 1rem;
			margin-bottom: 2rem;
		}
		.navbar a {
			color: white;
			text-decoration: none;
			margin-right: 1rem;
		}
		.navbar a:hover {
			text-decoration: underline;
		}
		.card {
			background: white;
			border-radius: 5px;
			box-shadow: 0 2px 5px rgba(0,0,0,0.1);
			padding: 20px;
			margin-bottom: 20px;
		}
		.btn {
			display: inline-block;
			background: #3498db;
			color: white;
			padding: 0.5rem 1rem;
			border: none;
			border-radius: 3px;
			cursor: pointer;
			text-decoration: none;
		}
		.btn:hover {
			background: #2980b9;
		}
		.btn-danger {
			background: #e74c3c;
		}
		.btn-danger:hover {
			background: #c0392b;
		}
		.btn-success {
			background: #2ecc71;
		}
		.btn-success:hover {
			background: #27ae60;
		}
		table {
			width: 100%;
			border-collapse: collapse;
		}
		table, th, td {
			border: 1px solid #ddd;
		}
		th, td {
			padding: 12px;
			text-align: left;
		}
		th {
			background-color: #f2f2f2;
		}
		tr:nth-child(even) {
			background-color: #f9f9f9;
		}
		.status-running {
			color: #2ecc71;
		}
		.status-failed {
			color: #e74c3c;
		}
		.status-pending {
			color: #f39c12;
		}
		.form-group {
			margin-bottom: 1rem;
		}
		label {
			display: block;
			margin-bottom: 0.5rem;
		}
		input, select {
			width: 100%;
			padding: 8px;
			border: 1px solid #ddd;
			border-radius: 4px;
		}
	`)
}

// ServeJS serves a JavaScript file with the given content
func (ws *WebServer) ServeJS(c *gin.Context) {
	c.Header("Content-Type", "application/javascript")
	c.String(http.StatusOK, `
		// Dashboard refresh
		function refreshData() {
			const refreshElements = document.querySelectorAll('[data-refresh-url]');
			refreshElements.forEach(el => {
				fetch(el.dataset.refreshUrl)
					.then(response => response.json())
					.then(data => {
						if (el.dataset.refreshTemplate === 'table') {
							refreshTable(el, data);
						} else if (el.dataset.refreshTemplate === 'count') {
							el.textContent = data.length || 0;
						}
					});
			});
		}

		function refreshTable(tableEl, data) {
			const tbody = tableEl.querySelector('tbody');
			const template = tableEl.dataset.refreshRowTemplate;

			if (!tbody || !template) return;

			let html = '';
			data.forEach(item => {
				let row = template;
				Object.keys(item).forEach(key => {
					row = row.replace(new RegExp("\\{\\{" + key + "\\}\\}", 'g'), item[key]);
				});
				html += row;
			});

			tbody.innerHTML = html;
		}

		// Set up periodic refresh
		if (document.querySelector('[data-refresh-url]')) {
			refreshData();
			setInterval(refreshData, 5000);
		}
	`)
}
