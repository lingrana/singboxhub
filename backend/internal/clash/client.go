// Package clash implements a client for the Clash API embedded in sing-box
// (experimental.clash_api). It covers the endpoints the panel relies on:
// version, configs, proxies, rules, connections, traffic and memory.
package clash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Error distinguishes transport failures from HTTP errors returned by the node.
type Error struct {
	StatusCode int // 0 when the node could not be reached at all
	Detail     string
}

func (e *Error) Error() string {
	if e.StatusCode == 0 {
		return "node unreachable: " + e.Detail
	}
	return fmt.Sprintf("node returned %d: %s", e.StatusCode, e.Detail)
}

// Client talks to one node's Clash API.
type Client struct {
	baseURL *url.URL
	secret  string
	hc      *http.Client
}

// New builds a client for the given API base URL and bearer secret.
func New(baseURL, secret string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid node api_url: %w", err)
	}
	// DisableKeepAlives works around servers (e.g. Python webapp relays) that
	// only handle the first HTTP request on a keep-alive connection correctly.
	transport := &http.Transport{DisableKeepAlives: true}
	return &Client{
		baseURL: u,
		secret:  secret,
		hc:      &http.Client{Timeout: 5 * time.Second, Transport: transport},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL.String()+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, &Error{StatusCode: 0, Detail: "node unreachable"}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, &Error{StatusCode: resp.StatusCode, Detail: "failed to read response"}
	}
	return resp.StatusCode, data, nil
}

// VersionResult carries the node version and round-trip latency.
type VersionResult struct {
	Version   string
	LatencyMS int64
}

// Version calls GET /version.
func (c *Client) Version(ctx context.Context) (*VersionResult, error) {
	start := time.Now()
	status, data, err := c.do(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &Error{StatusCode: status, Detail: "node returned non-200 status"}
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, &Error{StatusCode: status, Detail: "unexpected response format"}
	}
	return &VersionResult{
		Version:   strings.TrimPrefix(payload.Version, "sing-box "),
		LatencyMS: time.Since(start).Milliseconds(),
	}, nil
}

// ConfigsResult mirrors GET /configs (subset).
type ConfigsResult struct {
	Mode string `json:"mode"`
}

// Configs calls GET /configs.
func (c *Client) Configs(ctx context.Context) (*ConfigsResult, error) {
	status, data, err := c.do(ctx, http.MethodGet, "/configs", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &Error{StatusCode: status, Detail: "node returned non-200 status"}
	}
	var cfg ConfigsResult
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, &Error{StatusCode: status, Detail: "unexpected response format"}
	}
	return &cfg, nil
}

// PatchMode switches the node runtime mode.
func (c *Client) PatchMode(ctx context.Context, mode string) error {
	body, _ := json.Marshal(map[string]string{"mode": mode})
	status, _, err := c.do(ctx, http.MethodPatch, "/configs", body)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return &Error{StatusCode: status, Detail: "node returned non-success status"}
	}
	return nil
}

// Proxy is one entry of GET /proxies (subset of fields the panel shows).
type Proxy struct {
	Name string   `json:"name"`
	Type string   `json:"type"`
	Now  string   `json:"now"`
	All  []string `json:"all"`
}

// Proxies calls GET /proxies.
func (c *Client) Proxies(ctx context.Context) (map[string]Proxy, error) {
	status, data, err := c.do(ctx, http.MethodGet, "/proxies", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &Error{StatusCode: status, Detail: "node returned non-200 status"}
	}
	var payload struct {
		Proxies map[string]Proxy `json:"proxies"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, &Error{StatusCode: status, Detail: "unexpected response format"}
	}
	return payload.Proxies, nil
}

// SelectProxy switches a selector group's active outbound.
func (c *Client) SelectProxy(ctx context.Context, group, name string) error {
	body, _ := json.Marshal(map[string]string{"name": name})
	path := "/proxies/" + url.PathEscape(group)
	status, _, err := c.do(ctx, http.MethodPut, path, body)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return &Error{StatusCode: status, Detail: "node returned non-success status"}
	}
	return nil
}

// Rule is one entry of GET /rules.
type Rule struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
	Proxy   string `json:"proxy"`
}

// Rules calls GET /rules.
func (c *Client) Rules(ctx context.Context) ([]Rule, error) {
	status, data, err := c.do(ctx, http.MethodGet, "/rules", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &Error{StatusCode: status, Detail: "node returned non-200 status"}
	}
	var payload struct {
		Rules []Rule `json:"rules"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, &Error{StatusCode: status, Detail: "unexpected response format"}
	}
	return payload.Rules, nil
}

// CloseConnection deletes one tracked connection.
func (c *Client) CloseConnection(ctx context.Context, id string) error {
	status, _, err := c.do(ctx, http.MethodDelete, "/connections/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return &Error{StatusCode: status, Detail: "node returned non-success status"}
	}
	return nil
}

// CloseAllConnections deletes every tracked connection.
func (c *Client) CloseAllConnections(ctx context.Context) error {
	status, _, err := c.do(ctx, http.MethodDelete, "/connections", nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return &Error{StatusCode: status, Detail: "node returned non-success status"}
	}
	return nil
}

// wsURL converts the API base URL into a WebSocket URL for the given path.
func (c *Client) wsURL(path string) string {
	scheme := "ws"
	if c.baseURL.Scheme == "https" {
		scheme = "wss"
	}
	return scheme + "://" + c.baseURL.Host + c.baseURL.Path + path
}

// DialWS opens a WebSocket to the given Clash API path with auth headers.
func (c *Client) DialWS(ctx context.Context, path string) (*websocket.Conn, *http.Response, error) {
	dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+c.secret)
	conn, resp, err := dialer.DialContext(ctx, c.wsURL(path), hdr)
	if err != nil {
		return conn, resp, err
	}
	// Limit incoming frame size to 512KB to prevent memory exhaustion.
	conn.SetReadLimit(512 << 10)
	return conn, resp, nil
}

// ErrNoUpgrade is returned when a WebSocket endpoint cannot be upgraded.
var ErrNoUpgrade = errors.New("websocket upgrade failed")

// FetchClientPortMap queries the node's webapp /connections endpoint to get
// the real client IP mapping (port → IP). This works around sing-box only
// seeing 127.0.0.1 when behind the Python webapp relay.
func (c *Client) FetchClientPortMap(ctx context.Context) (map[string]string, error) {
	rootURL := &url.URL{
		Scheme: c.baseURL.Scheme,
		Host:   c.baseURL.Host,
		Path:   "/connections",
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rootURL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{StatusCode: resp.StatusCode, Detail: "node returned non-200 status"}
	}
	var portMap map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&portMap); err != nil {
		return nil, err
	}
	return portMap, nil
}
