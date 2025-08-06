package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ao/hivemind/pkg/api"
)

// Client is a Hivemind API client
type Client struct {
	baseURL    string
	httpClient *http.Client
	token      string
}

// ClientOption is a function that configures a Client
type ClientOption func(*Client)

// WithTimeout sets the timeout for the HTTP client
func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		c.httpClient.Timeout = timeout
	}
}

// WithToken sets the authentication token
func WithToken(token string) ClientOption {
	return func(c *Client) {
		c.token = token
	}
}

// NewClient creates a new Hivemind API client
func NewClient(baseURL string, options ...ClientOption) *Client {
	client := &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	for _, option := range options {
		option(client)
	}

	return client
}

// ListContainers returns a list of containers
func (c *Client) ListContainers() ([]api.Container, error) {
	resp, err := c.doRequest("GET", "/containers", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var containers []api.Container
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, err
	}

	return containers, nil
}

// GetContainer returns a container by ID
func (c *Client) GetContainer(id string) (*api.Container, error) {
	resp, err := c.doRequest("GET", fmt.Sprintf("/containers/%s", id), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var container api.Container
	if err := json.NewDecoder(resp.Body).Decode(&container); err != nil {
		return nil, err
	}

	return &container, nil
}

// CreateContainer creates a new container
func (c *Client) CreateContainer(container *api.Container) (*api.Container, error) {
	data, err := json.Marshal(container)
	if err != nil {
		return nil, err
	}

	resp, err := c.doRequest("POST", "/containers", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var createdContainer api.Container
	if err := json.NewDecoder(resp.Body).Decode(&createdContainer); err != nil {
		return nil, err
	}

	return &createdContainer, nil
}

// DeleteContainer deletes a container by ID
func (c *Client) DeleteContainer(id string) error {
	resp, err := c.doRequest("DELETE", fmt.Sprintf("/containers/%s", id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// ListNodes returns a list of nodes
func (c *Client) ListNodes() ([]api.Node, error) {
	resp, err := c.doRequest("GET", "/nodes", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var nodes []api.Node
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return nil, err
	}

	return nodes, nil
}

// GetNode returns a node by ID
func (c *Client) GetNode(id string) (*api.Node, error) {
	resp, err := c.doRequest("GET", fmt.Sprintf("/nodes/%s", id), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var node api.Node
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		return nil, err
	}

	return &node, nil
}

// doRequest performs an HTTP request
func (c *Client) doRequest(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var apiErr api.Error
		if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil {
			return nil, fmt.Errorf("HTTP error: %s", resp.Status)
		}
		return nil, fmt.Errorf("API error: %d - %s", apiErr.Code, apiErr.Message)
	}

	return resp, nil
}
