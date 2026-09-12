package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"

	"github.com/sing-hub/panel/internal/httpx"
)

// apiHandler is the panel's full HTTP handler (JSON API included). UI handlers
// dispatch to it in-process with the caller's bearer token, keeping the JSON
// API the single source of truth. It is set once by the api package right
// before the server starts serving; calls before that are a wiring bug.
var apiHandler atomic.Value // stores http.Handler

// SetAPIHandler wires the handler used for in-process API calls.
func SetAPIHandler(h http.Handler) { apiHandler.Store(h) }

func loadAPIHandler() http.Handler {
	if v := apiHandler.Load(); v != nil {
		return v.(http.Handler)
	}
	return nil
}

// apiResult is the outcome of one in-process API call.
type apiResult struct {
	Status int
	Body   []byte
	Header http.Header
}

// Problem parses an RFC 9457 problem+json body; returns nil for other types.
func (res apiResult) Problem() *httpx.Problem {
	ct := res.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/problem+json") || len(res.Body) == 0 {
		return nil
	}
	var p httpx.Problem
	if err := json.Unmarshal(res.Body, &p); err != nil {
		return nil
	}
	return &p
}

// ProblemDetail renders a human-readable message for any non-2xx result.
func (res apiResult) ProblemDetail() string {
	if p := res.Problem(); p != nil {
		if p.Detail != "" {
			return p.Detail
		}
		return p.Title
	}
	if res.Status == http.StatusTooManyRequests {
		if ra := res.Header.Get("Retry-After"); ra != "" {
			return "请求过于频繁,请 " + ra + " 秒后重试"
		}
	}
	if len(res.Body) > 0 && len(res.Body) < 300 {
		return string(res.Body)
	}
	return fmt.Sprintf("请求失败 (HTTP %d)", res.Status)
}

// Decode unmarshals a successful JSON body into v.
func (res apiResult) Decode(v any) error {
	if err := json.Unmarshal(res.Body, v); err != nil {
		return fmt.Errorf("decode api response: %w", err)
	}
	return nil
}

// call dispatches one request to the panel's own API handler. Client IP and
// Host are propagated so rate limiting and absolute-URL generation behave
// exactly as for external callers.
func (u *UI) call(r *http.Request, token, method, target string, body []byte, headers map[string]string) apiResult {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Host = r.Host
	req.RemoteAddr = r.RemoteAddr
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		req.Header.Set("X-Forwarded-Proto", proto)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h := loadAPIHandler()
	if h == nil {
		return apiResult{Status: http.StatusInternalServerError, Body: []byte("api handler not wired")}
	}
	h.ServeHTTP(rec, req)
	res := rec.Result()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return apiResult{Status: res.StatusCode, Header: res.Header, Body: nil}
	}
	return apiResult{Status: res.StatusCode, Body: raw, Header: res.Header}
}

// apiGetJSON performs GET and decodes the JSON body into out on success.
func (u *UI) apiGetJSON(r *http.Request, token, target string, out any) (apiResult, error) {
	res := u.call(r, token, http.MethodGet, target, nil, nil)
	if res.Status != http.StatusOK {
		return res, fmt.Errorf("api %s: %s", target, res.ProblemDetail())
	}
	return res, res.Decode(out)
}

// apiSend performs a method call with an optional JSON body and returns the
// raw result (callers inspect Status / Problem themselves).
func (u *UI) apiSend(r *http.Request, token, method, target string, payload any, headers map[string]string) apiResult {
	var body []byte
	hdrs := map[string]string{}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return apiResult{Status: http.StatusInternalServerError, Body: []byte(err.Error())}
		}
		body = raw
		hdrs["Content-Type"] = "application/json"
	}
	for k, v := range headers {
		hdrs[k] = v
	}
	return u.call(r, token, method, target, body, hdrs)
}

// ---- API response shapes (mirrors of the JSON API payloads) ----

// nodeCard is one row of GET /stats/nodes (dashboard card aggregate).
type nodeCard struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Tags              []string `json:"tags"`
	Enabled           bool     `json:"enabled"`
	Source            string   `json:"source"`
	Online            bool     `json:"online"`
	HasOutbound       bool     `json:"has_outbound"`
	UpBPS             int64    `json:"up_bps"`
	DownBPS           int64    `json:"down_bps"`
	ActiveConnections int      `json:"active_connections"`
	TodayUpBytes      int64    `json:"today_up_bytes"`
	TodayDownBytes    int64    `json:"today_down_bytes"`
	TotalUpBytes      int64    `json:"total_up_bytes"`
	TotalDownBytes    int64    `json:"total_down_bytes"`
	MemoryBytes       *int64   `json:"memory_bytes"`
	Version           *string  `json:"version"`
	Mode              *string  `json:"mode"`
	LastOnlineAt      *string  `json:"last_online_at"`
	ParsedType        string   `json:"parsed_type"`
	ParsedName        string   `json:"parsed_name"`
	Server            string   `json:"server"`
	ServerPort        int      `json:"server_port"`
}

type nodeCardList struct {
	Items []nodeCard `json:"items"`
}

type nodeSummary struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	APIURL            string   `json:"api_url"`
	ConfigImported    bool     `json:"config_imported"`
	HostID            string   `json:"host_id"`
	Tags              []string `json:"tags"`
	Enabled           bool     `json:"enabled"`
	Source            string   `json:"source"`
	Mode              string   `json:"mode"`
	Online            bool     `json:"online"`
	HasOutbound       bool     `json:"has_outbound"`
	UpBPS             int64    `json:"up_bps"`
	DownBPS           int64    `json:"down_bps"`
	ActiveConnections int      `json:"active_connections"`
	LastOnlineAt      *string  `json:"last_online_at"`
	ParsedType        string   `json:"parsed_type"`
	ParsedName        string   `json:"parsed_name"`
	Server            string   `json:"server"`
	ServerPort        int      `json:"server_port"`
	LatencyMs         int      `json:"latency_ms"`
	LatencyFailCount  int      `json:"latency_fail_count"`
}

type nodeList struct {
	Items []nodeSummary `json:"items"`
}

type nodeFull struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	APIURL       string   `json:"api_url"`
	APISecretSet bool     `json:"api_secret_set"`
	OutboundJSON *string  `json:"outbound_json"`
	Source       string   `json:"source"`
	ConfigURL    string   `json:"config_url"`
	KCEKeySet    bool     `json:"kce_key_set"`
	Tags         []string `json:"tags"`
	Enabled      bool     `json:"enabled"`
	Remark       string   `json:"remark"`
	LastOnlineAt *string  `json:"last_online_at"`
}

type nodeStatus struct {
	NodeID            string  `json:"node_id"`
	Online            bool    `json:"online"`
	Version           *string `json:"version"`
	Mode              *string `json:"mode"`
	UpBPS             int64   `json:"up_bps"`
	DownBPS           int64   `json:"down_bps"`
	MemoryBytes       *int64  `json:"memory_bytes"`
	ActiveConnections int     `json:"active_connections"`
	LastOnlineAt      *string `json:"last_online_at"`
	LastError         string  `json:"last_error"`
}

type overview struct {
	TotalNodes     int   `json:"total_nodes"`
	OnlineNodes    int   `json:"online_nodes"`
	UpBPS          int64 `json:"up_bps"`
	DownBPS        int64 `json:"down_bps"`
	UpBytesToday   int64 `json:"up_bytes_today"`
	DownBytesToday int64 `json:"down_bytes_today"`
	TotalIPsSeen   int64 `json:"total_ips_seen"`
	OnlineIPs      int   `json:"online_ips"`
}

type topNode struct {
	NodeID     string `json:"node_id"`
	Name       string `json:"name"`
	UpBytes    int64  `json:"up_bytes"`
	DownBytes  int64  `json:"down_bytes"`
	TotalBytes int64  `json:"total_bytes"`
}

type geoInfo struct {
	Country string `json:"country"`
	Region  string `json:"region"`
	City    string `json:"city"`
	ASOrg   string `json:"as_org"`
}

type ipRow struct {
	NodeID            string   `json:"node_id"`
	SourceIP          string   `json:"source_ip"`
	UploadBytes       int64    `json:"upload_bytes"`
	DownloadBytes     int64    `json:"download_bytes"`
	TotalBytes        int64    `json:"total_bytes"`
	ConnectionsTotal  int      `json:"connections_total"`
	ActiveConnections int      `json:"active_connections"`
	LastSeenAt        string   `json:"last_seen_at"`
	Geo               *geoInfo `json:"geo"`
	NodesCount        int      `json:"nodes_count"`
}

type seriesPoint struct {
	BucketStart string `json:"bucket_start"`
	UpBytes     int64  `json:"up_bytes"`
	DownBytes   int64  `json:"down_bytes"`
}

type seriesList struct {
	Items []seriesPoint `json:"items"`
}

type connMeta struct {
	Network         string `json:"network"`
	SourceIP        string `json:"source_ip"`
	SourcePort      any    `json:"source_port"`
	DestinationIP   string `json:"destination_ip"`
	DestinationPort any    `json:"destination_port"`
	Host            string `json:"host"`
}

type connRow struct {
	ID            string   `json:"id"`
	UploadBytes   int64    `json:"upload_bytes"`
	DownloadBytes int64    `json:"download_bytes"`
	Start         string   `json:"start"`
	Outbound      string   `json:"outbound"`
	Metadata      connMeta `json:"metadata"`
}

type connList struct {
	Items []connRow `json:"items"`
}

type proxyRow struct {
	Name string   `json:"name"`
	Type string   `json:"type"`
	Now  string   `json:"now"`
	All  []string `json:"all"`
}

type proxyList struct {
	Items []proxyRow `json:"items"`
}

type exitIP struct {
	IP        string `json:"ip"`
	Country   string `json:"country"`
	City      string `json:"city"`
	ASOrg     string `json:"as_org"`
	CheckedAt string `json:"checked_at"`
}

type checkResult struct {
	Online    bool    `json:"online"`
	LatencyMS *int64  `json:"latency_ms"`
	Version   *string `json:"version"`
	Detail    *string `json:"detail"`
}

type operation struct {
	ID     string  `json:"id"`
	Status string  `json:"status"`
	Result *string `json:"result"`
	Error  *string `json:"error"`
}

type subscription struct {
	Enabled   bool     `json:"enabled"`
	Token     *string  `json:"token"`
	URL       *string  `json:"url"`
	Formats   []string `json:"formats"`
	UpdatedAt string   `json:"updated_at"`
}

type settingsView struct {
	SamplerIntervalSeconds     int    `json:"sampler_interval_seconds"`
	RetentionDays              int    `json:"retention_days"`
	IPLookupEnabled            bool   `json:"ip_lookup_enabled"`
	IPLookupProviderURL        string `json:"ip_lookup_provider_url"`
	SingboxBinPath             string `json:"singbox_bin_path"`
	BrandName                  string `json:"brand_name"`
	ResetCooldownMinutes       int    `json:"reset_cooldown_minutes"`
	ICMPMonitorEnabled         bool   `json:"icmp_monitor_enabled"`
	ICMPMonitorTarget          string `json:"icmp_monitor_target"`
	ICMPMonitorIntervalSeconds int    `json:"icmp_monitor_interval_seconds"`
	ICMPAutoDisableThresholdMs int    `json:"icmp_auto_disable_threshold_ms"`
	ICMPAutoDisableConsecutive int    `json:"icmp_auto_disable_consecutive"`
	FaviconURL                 string `json:"favicon_url,omitempty"`
	LogoURL                    string `json:"logo_url,omitempty"`
	UpdatedAt                  string `json:"updated_at"`
}

type sessionTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}
