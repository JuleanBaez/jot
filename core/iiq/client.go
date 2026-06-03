// Package iiq is jot's Incident IQ client. It wraps the /api/v1.0 HTTP
// surface with typed request/response helpers so the dash, the CLI, and
// future subcommands can speak to IIQ without each one re-implementing the
// auth headers and base-URL normalization.
//
// The legacy helpers in ad.go (iiqBase, iiqGET, iiqPOST, iiqPATCH) will
// migrate to this package in a follow-up. For now both code paths coexist —
// they hit the same endpoints with the same headers, so results are
// identical.
package iiq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"jot/core/config"
)

// Client talks to a single IIQ tenant. Construct with New.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client bound to the IIQ settings in cfg. Returns an error
// (rather than panicking) if the bare minimum fields are missing, so the
// dash can surface "IIQ not configured" gracefully.
func New(cfg config.Jot) (*Client, error) {
	if cfg.IncidentIQ.BaseURL == "" {
		return nil, fmt.Errorf("incident_iq.base_url not set")
	}
	if cfg.IncidentIQ.Token == "" {
		return nil, fmt.Errorf("incident_iq.token not set")
	}
	base := strings.TrimRight(cfg.IncidentIQ.BaseURL, "/")
	base = strings.TrimSuffix(base, "/api/v1.0")
	return &Client{
		base:  base + "/api/v1.0",
		token: cfg.IncidentIQ.Token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// do executes a request with IIQ auth headers and returns the raw response.
// Callers are responsible for closing the body.
func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Client", "ApiClient")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

// GetJSON performs a GET and decodes the response body into out.
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	return c.requestJSON(ctx, http.MethodGet, path, nil, out)
}

// PostJSON performs a POST with a JSON body and decodes the response into out.
func (c *Client) PostJSON(ctx context.Context, path string, body, out any) error {
	return c.requestJSON(ctx, http.MethodPost, path, body, out)
}

// PatchJSON performs a PATCH and, if out != nil, decodes the response.
func (c *Client) PatchJSON(ctx context.Context, path string, body, out any) error {
	return c.requestJSON(ctx, http.MethodPatch, path, body, out)
}

func (c *Client) requestJSON(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
