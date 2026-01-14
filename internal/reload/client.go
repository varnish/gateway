package reload

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// ReloadPath is the magic endpoint that ghost uses for config reloads.
	ReloadPath = "/.varnish-ghost/reload"

	// DefaultTimeout is the default timeout for reload requests.
	DefaultTimeout = 5 * time.Second
)

// Response represents the JSON response from ghost's reload endpoint.
type Response struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// Client is an HTTP client for triggering ghost config reloads.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a new reload client.
// varnishAddr should be the address where Varnish is listening (e.g., "localhost:80").
func NewClient(varnishAddr string) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: DefaultTimeout,
		},
		baseURL: fmt.Sprintf("http://%s", varnishAddr),
	}
}

// TriggerReload sends a reload request to ghost and waits for the response.
// Returns nil on success, or an error if the reload failed.
func (c *Client) TriggerReload(ctx context.Context) error {
	url := c.baseURL + ReloadPath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("http.NewRequestWithContext: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http.Do(%s): %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("io.ReadAll: %w", err)
	}

	var reloadResp Response
	if err := json.Unmarshal(body, &reloadResp); err != nil {
		return fmt.Errorf("json.Unmarshal: %w (body: %s)", err, string(body))
	}

	if reloadResp.Status != "ok" {
		return fmt.Errorf("ghost reload failed: %s", reloadResp.Message)
	}

	return nil
}

// TriggerReloadSimple is a convenience function that creates a client and triggers a reload.
func TriggerReloadSimple(ctx context.Context, varnishAddr string) error {
	client := NewClient(varnishAddr)
	return client.TriggerReload(ctx)
}
