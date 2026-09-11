// Package probe checks a node's exit IP by running a local sing-box instance
// that dials through the node's outbound and queries an IP echo service.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sing-hub/panel/internal/httpx"
)

// Result is a successful probe outcome.
type Result struct {
	IP      string
	Country string
	City    string
	ASOrg   string
}

// Runner executes probe cycles with a configured sing-box binary.
type Runner struct {
	BinPath     string
	ProviderURL string
	Timeout     time.Duration
}

// OutboundError marks an unusable outbound config (missing tag, bad JSON).
type OutboundError struct{ Reason string }

func (e *OutboundError) Error() string { return e.Reason }

// Probe runs one exit-IP check through the given outbound config JSON.
func (r *Runner) Probe(ctx context.Context, outboundJSON string) (*Result, error) {
	if err := httpx.ValidateOutboundURL(r.ProviderURL); err != nil {
		return nil, errors.New("IP provider URL failed safety validation")
	}
	if r.BinPath == "" {
		return nil, errors.New("sing-box binary path is not configured")
	}
	tag, err := outboundTag(outboundJSON)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(r.BinPath); err != nil {
		return nil, fmt.Errorf("sing-box binary not found: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "singhub-probe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(tmpDir, "config.json")
	cfg, err := probeConfig(port, tag, outboundJSON)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		return nil, err
	}

	probeCtx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, r.BinPath, "run", "-c", cfgPath)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start sing-box: %w", err)
	}
	procExited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(procExited) }()

	if err := waitReachable(probeCtx, "127.0.0.1", port, procExited); err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	result, err := r.fetch(probeCtx, port)
	_ = cmd.Process.Kill()
	<-procExited
	return result, err
}

// outboundTag extracts the "tag" field of an outbound JSON object.
func outboundTag(outboundJSON string) (string, error) {
	var head struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal([]byte(outboundJSON), &head); err != nil {
		return "", &OutboundError{Reason: "outbound_json is not a valid sing-box outbound object"}
	}
	if head.Tag == "" {
		return "", &OutboundError{Reason: "outbound_json has no tag field"}
	}
	return head.Tag, nil
}

// probeConfig builds a minimal sing-box config: one mixed inbound forwarding
// everything to the node's outbound.
func probeConfig(port int, tag, outboundJSON string) ([]byte, error) {
	var outbound json.RawMessage
	if !json.Valid([]byte(outboundJSON)) {
		return nil, &OutboundError{Reason: "outbound_json is not valid JSON"}
	}
	outbound = json.RawMessage(outboundJSON)
	cfg := map[string]any{
		"log": map[string]any{"level": "silent"},
		"inbounds": []map[string]any{
			{"type": "mixed", "tag": "probe-in", "listen": "127.0.0.1", "listen_port": port},
		},
		"outbounds": []json.RawMessage{outbound},
		"route":     map[string]any{"final": tag},
	}
	return json.Marshal(cfg)
}

// fetch queries the IP echo service through the local mixed inbound.
func (r *Runner) fetch(ctx context.Context, port int) (*Result, error) {
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	hc := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.ProviderURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query ip provider through node: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ip provider returned %d", resp.StatusCode)
	}
	return parseProviderBody(body)
}

// parseProviderBody accepts both JSON (ip-api style) and plain-text responses.
func parseProviderBody(body []byte) (*Result, error) {
	text := strings.TrimSpace(string(body))
	var payload struct {
		Query   string `json:"query"`
		Country string `json:"country"`
		City    string `json:"city"`
		AS      string `json:"as"`
		IP      string `json:"ip"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		ip := payload.Query
		if ip == "" {
			ip = payload.IP
		}
		if ip == "" {
			return nil, errors.New("ip provider response has no IP field")
		}
		return &Result{IP: ip, Country: payload.Country, City: payload.City, ASOrg: payload.AS}, nil
	}
	if text != "" && !strings.ContainsAny(text, "{}") {
		return &Result{IP: text}, nil
	}
	return nil, errors.New("unrecognized ip provider response")
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitReachable polls the mixed inbound until it accepts or sing-box exits.
func waitReachable(ctx context.Context, host string, port int, procExited <-chan struct{}) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	deadline, _ := ctx.Deadline()
	for {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sing-box inbound did not open in time")
		case <-procExited:
			return errors.New("sing-box exited immediately (check outbound config and binary)")
		case <-time.After(200 * time.Millisecond):
			if time.Now().After(deadline) {
				return errors.New("timeout waiting for sing-box inbound")
			}
		}
	}
}
