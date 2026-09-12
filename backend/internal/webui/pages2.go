package webui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/qr"
	"github.com/sing-hub/panel/internal/httpx"
)

// ---- traffic page ----

type trafficBody struct {
	Base
	Nodes       []nodeSummary
	Selected    []string
	SelectedRaw string
	Range       string
	Chart       template.HTML
	TopNodes    []topNode
}

// IsSelected reports whether a node chip is active.
func (b trafficBody) IsSelected(id string) bool {
	for _, s := range b.Selected {
		if s == id {
			return true
		}
	}
	return false
}

// ToggleURL builds the traffic URL with the node's selection flipped.
func (b trafficBody) ToggleURL(id string) string {
	var next []string
	removed := false
	for _, s := range b.Selected {
		if s == id {
			removed = true
			continue
		}
		next = append(next, s)
	}
	if !removed {
		next = append(append([]string{}, b.Selected...), id)
	}
	if len(next) == len(b.Nodes) {
		next = nil // all selected == default view (no filter)
	}
	return url.QueryEscape(strings.Join(next, ","))
}

func (u *UI) pageTraffic(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	body := u.buildTrafficBody(r, token)
	body.Base = u.rd.base("traffic", "用量 · sing-box hub", u.usernameOf(r), u.themeOf(r))
	u.renderPage(w, r, "traffic", "用量 · sing-box hub", body)
}

type trafficTableBody struct {
	Base
	Nodes          []nodeCard
	TotalUpBPS     int64
	TotalDownBPS   int64
	TotalTodayUp   int64
	TotalTodayDown int64
	TotalAllUp     int64
	TotalAllDown   int64
	TotalConns     int
}

func (u *UI) buildTrafficBody(r *http.Request, token string) trafficTableBody {
	body := trafficTableBody{}
	var list nodeCardList
	if _, err := u.apiGetJSON(r, token, "/stats/nodes", &list); err == nil {
		body.Nodes = list.Items
		for _, n := range list.Items {
			body.TotalUpBPS += n.UpBPS
			body.TotalDownBPS += n.DownBPS
			body.TotalTodayUp += n.TodayUpBytes
			body.TotalTodayDown += n.TodayDownBytes
			body.TotalAllUp += n.TotalUpBytes
			body.TotalAllDown += n.TotalDownBytes
			body.TotalConns += n.ActiveConnections
		}
	}
	return body
}

func (u *UI) fragTrafficTable(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	body := u.buildTrafficBody(r, token)
	u.renderFrag(w, "frag_traffic_table", body)
}

func (u *UI) trafficChart(r *http.Request, token string, selected []string, rng string) template.HTML {
	hours := wideHours(rng)
	interval := "minute"
	if hours > 24 {
		interval = "hour"
	}
	from, to := isoRange(hours)
	// Merge per-node series into a combined up/down timeline (the Vue version
	// did the same client-side).
	merged := map[int64]*chartPoint{}
	for _, id := range selected {
		var list seriesList
		target := fmt.Sprintf("/nodes/%s/traffic?interval=%s&from=%s&to=%s",
			url.PathEscape(id), interval, url.QueryEscape(from), url.QueryEscape(to))
		if _, err := u.apiGetJSON(r, token, target, &list); err != nil {
			continue
		}
		for _, it := range list.Items {
			t, err := time.Parse(time.RFC3339, it.BucketStart)
			if err != nil {
				continue
			}
			p, ok := merged[t.Unix()]
			if !ok {
				p = &chartPoint{T: t.Unix()}
				merged[t.Unix()] = p
			}
			p.Up += it.UpBytes
			p.Down += it.DownBytes
		}
	}
	pts := make([]chartPoint, 0, len(merged))
	for _, p := range merged {
		pts = append(pts, *p)
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].T < pts[j].T })
	return areaChart(pts, "tr")
}

func (u *UI) trafficRank(r *http.Request, token, rng string) []topNode {
	hours := wideHours(rng)
	from, to := isoRange(hours)
	var tops struct {
		Items []topNode `json:"items"`
	}
	_, _ = u.apiGetJSON(r, token, fmt.Sprintf("/stats/top-nodes?limit=20&from=%s&to=%s", url.QueryEscape(from), url.QueryEscape(to)), &tops)
	return tops.Items
}

func normalizeWideRange(v string) string {
	switch v {
	case "hour", "6h", "7d", "30d":
		return v
	default:
		return "24h"
	}
}

func wideHours(rng string) int {
	switch rng {
	case "hour":
		return 1
	case "6h":
		return 6
	case "7d":
		return 168
	case "30d":
		return 720
	default:
		return 24
	}
}

func parseSelected(raw string, nodes []nodeSummary) []string {
	if raw == "" {
		ids := make([]string, 0, len(nodes))
		for _, n := range nodes {
			ids = append(ids, n.ID)
		}
		return ids
	}
	ids := strings.Split(raw, ",")
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// ---- ips page ----

type ipsBody struct {
	Base
	NodeID     string
	Nodes      []nodeSummary
	Rows       []ipRow
	PerNode    bool
	Tab        string
	Query      string
	Page       int
	TotalCount int
	TotalPages int
}

func (u *UI) pageIps(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	var list nodeList
	if _, err := u.apiGetJSON(r, token, "/nodes?page_size=100", &list); err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: "节点列表加载失败:" + err.Error()})
		return
	}
	body := u.buildIps(r, token, list.Items, query, page)
	body.Base = u.rd.base("users", "用户 & IP · sing-box hub", u.usernameOf(r), u.themeOf(r))
	body.Tab = "ip"
	body.Query = query
	body.Page = page
	u.renderPage(w, r, "users_ip", "用户 & IP · sing-box hub", body)
}

func (u *UI) buildIps(r *http.Request, token string, nodes []nodeSummary, query string, page int) ipsBody {
	nodeID := r.URL.Query().Get("node")
	body := ipsBody{Nodes: nodes}
	pageSize := 20
	var allRows []ipRow
	if nodeID == "" || nodeID == "all" {
		var top struct {
			Items []ipRow `json:"items"`
		}
		if _, err := u.apiGetJSON(r, token, "/stats/top-ips?limit=100", &top); err == nil {
			allRows = top.Items
		}
	} else {
		body.NodeID = nodeID
		body.PerNode = true
		var list struct {
			Items []ipRow `json:"items"`
		}
		if _, err := u.apiGetJSON(r, token, "/nodes/"+url.PathEscape(nodeID)+"/ips?page_size=100&sort=-total_bytes", &list); err == nil {
			allRows = list.Items
		}
	}
	if query != "" {
		q := strings.ToLower(query)
		filtered := make([]ipRow, 0)
		for _, row := range allRows {
			if strings.Contains(strings.ToLower(row.SourceIP), q) {
				filtered = append(filtered, row)
			}
		}
		allRows = filtered
	}
	body.TotalCount = len(allRows)
	body.TotalPages = (body.TotalCount + pageSize - 1) / pageSize
	if body.TotalPages < 1 {
		body.TotalPages = 1
	}
	start := (page - 1) * pageSize
	if start >= len(allRows) {
		start = len(allRows)
	}
	end := start + pageSize
	if end > len(allRows) {
		end = len(allRows)
	}
	body.Rows = allRows[start:end]
	return body
}

func (u *UI) fragIpsTable(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	var list nodeList
	if _, err := u.apiGetJSON(r, token, "/nodes?page_size=100", &list); err != nil {
		u.renderFrag(w, "frag_ips_table", ipsBody{})
		return
	}
	body := u.buildIps(r, token, list.Items, query, page)
	body.Query = query
	body.Page = page
	u.renderFrag(w, "frag_ips_table", body)
}

// ---- subscription page ----

type subBody struct {
	Base
	Sub       *subscription
	Nodes     subNodesBody
	Format    string
	URL       string
	QRDataURI string
}

// subNodeRow is one manageable entry of the user-available node list.
type subNodeRow struct {
	ID     string
	Name   string
	Source bool
	Online bool
	Mode   string
}

// ModeGroup buckets a node into the subscription-page classification.
// Imported proxies without a Clash runtime mode use the default Rule group.
func (r subNodeRow) ModeGroup() string {
	switch r.Mode {
	case "rule", "global", "direct":
		return r.Mode
	default:
		// Imported proxies have no Clash API and therefore no runtime mode.
		// Rule is the panel's default subscription group for those nodes.
		return "rule"
	}
}

// subNodeGroups is the availability card payload grouped for display.
type subNodesBody struct {
	Rule   []subNodeRow
	Global []subNodeRow
	Direct []subNodeRow
	None   []subNodeRow
}

// NodeGroups exposes the groups to templates as a map keyed by group name.
func (g subNodesBody) NodeGroups() map[string][]subNodeRow {
	return map[string][]subNodeRow{
		"rule":   g.Rule,
		"global": g.Global,
		"direct": g.Direct,
		"none":   g.None,
	}
}

func buildSubNodeGroups(rows []subNodeRow) subNodesBody {
	var g subNodesBody
	for _, r := range rows {
		switch r.ModeGroup() {
		case "rule":
			g.Rule = append(g.Rule, r)
		case "global":
			g.Global = append(g.Global, r)
		case "direct":
			g.Direct = append(g.Direct, r)
		}
	}
	return g
}

func (u *UI) pageSubscription(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	body, err := u.buildSubscription(r, token, r.URL.Query().Get("format"))
	if err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: "订阅加载失败:" + err.Error()})
		return
	}
	body.Nodes = buildSubNodeGroups(u.fetchSubNodes(r, token))
	body.Base = u.rd.base("subscription", "订阅 · sing-box hub", u.usernameOf(r), u.themeOf(r))
	u.renderPage(w, r, "subscription", "订阅 · sing-box hub", body)
}

// fetchSubNodes loads the node pool with runtime mode for the availability
// card (mode comes from each node's Clash API: rule/global/direct).
func (u *UI) fetchSubNodes(r *http.Request, token string) []subNodeRow {
	var stats nodeCardList
	_, statsErr := u.apiGetJSON(r, token, "/stats/nodes", &stats)
	byID := make(map[string]nodeCard, len(stats.Items))
	for _, n := range stats.Items {
		byID[n.ID] = n
	}
	var list nodeList
	if _, err := u.apiGetJSON(r, token, "/nodes?page_size=100", &list); err != nil {
		return nil
	}
	rows := make([]subNodeRow, 0, len(list.Items))
	for _, n := range list.Items {
		if !n.Enabled || !n.HasOutbound {
			continue
		}
		stat := byID[n.ID]
		mode := ""
		if stat.Mode != nil {
			mode = *stat.Mode
		}
		rows = append(rows, subNodeRow{
			ID: n.ID, Name: n.Name, Source: n.Source != "",
			Online: statsErr == nil && stat.Online, Mode: mode,
		})
	}
	return rows
}

// fragSubNodes renders the user-available nodes card: every panel node with
// mode badge, latency test and disable controls; enabled nodes are exactly
// what /sub/{token} serves.
func (u *UI) fragSubNodes(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	u.renderFrag(w, "frag_sub_nodes", buildSubNodeGroups(u.fetchSubNodes(r, token)))
}

// handleSubNodeLatency runs a latency test from the subscription page and
// reports the result as a toast.
func (u *UI) handleSubNodeLatency(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	nodeID := r.PathValue("node_id")
	res := u.apiSend(r, token, http.MethodPost, "/nodes/"+url.PathEscape(nodeID)+"/latency", nil, nil)
	if res.Status != http.StatusOK {
		hxTrigger(w, toastErr("延迟测试失败:"+res.ProblemDetail()))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var out struct {
		LatencyMS int64 `json:"latency_ms"`
	}
	if err := res.Decode(&out); err != nil {
		hxTrigger(w, toastErr("延迟测试响应异常"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	hxTrigger(w, toastOK(fmt.Sprintf("延迟 %d ms", out.LatencyMS)))
	w.WriteHeader(http.StatusNoContent)
}

func (u *UI) buildSubscription(r *http.Request, token, format string) (subBody, error) {
	body := subBody{Format: normalizeSubFormat(format)}
	var sub subscription
	res, err := u.apiGetJSON(r, token, "/subscription", &sub)
	if err != nil {
		return body, fmt.Errorf("%s", res.ProblemDetail())
	}
	body.Sub = &sub
	if sub.Enabled && sub.Token != nil {
		body.URL = publicOrigin(r) + "/sub/" + *sub.Token + "?format=" + body.Format
		body.QRDataURI = qrDataURI(body.URL)
	}
	return body, nil
}

// publicOrigin reconstructs the externally visible origin.
func publicOrigin(r *http.Request) string {
	scheme := "http"

	// Only trust X-Forwarded-Proto when behind a known proxy
	if r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}

	// Use the Host header but sanitize it
	host := r.Host
	if host == "" {
		host = "localhost:42501"
	}

	// Validate host to prevent injection
	if strings.ContainsAny(host, "\n\r\t") {
		host = "localhost:42501"
	}

	return scheme + "://" + host
}

func normalizeSubFormat(v string) string {
	switch v {
	case "clash", "base64":
		return v
	default:
		return "singbox"
	}
}

// qrDataURI renders the subscription URL as a QR PNG data URI.
func qrDataURI(content string) string {
	code, err := qr.Encode(content, qr.M, qr.Auto)
	if err != nil {
		return ""
	}
	scaled, err := barcode.Scale(code, 220, 220)
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, scaled); err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func (u *UI) fragSubscriptionCard(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	body, err := u.buildSubscription(r, token, r.URL.Query().Get("format"))
	if err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: err.Error()})
		return
	}
	u.renderFrag(w, "frag_sub_card", body)
}

// parsedNode is one proxy node extracted from a subscription.
type parsedNode struct {
	Name   string
	Type   string
	Server string
	Port   int
	Remark string
}

// parsedSubBody is the template payload for parsed subscription results.
type parsedSubBody struct {
	Nodes []parsedNode
	Error string
}

// handleSubParse fetches a subscription URL and parses its nodes.
func (u *UI) handleSubParse(w http.ResponseWriter, r *http.Request) {
	if !u.requireForm(w, r) {
		return
	}
	subURL := strings.TrimSpace(r.PostFormValue("sub_url"))
	if subURL == "" {
		u.renderFrag(w, "frag_sub_parsed", parsedSubBody{Error: "请输入订阅链接"})
		return
	}
	if !strings.HasPrefix(subURL, "http://") && !strings.HasPrefix(subURL, "https://") {
		u.renderFrag(w, "frag_sub_parsed", parsedSubBody{Error: "链接必须以 http:// 或 https:// 开头"})
		return
	}

	// Validate URL for SSRF protection
	if err := httpx.ValidateOutboundURL(subURL); err != nil {
		u.renderFrag(w, "frag_sub_parsed", parsedSubBody{Error: "订阅链接安全校验失败: " + err.Error()})
		return
	}

	client := httpx.SafeHTTPClient(15 * time.Second)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, subURL, nil)
	if err != nil {
		u.renderFrag(w, "frag_sub_parsed", parsedSubBody{Error: "订阅链接无效"})
		return
	}
	resp, err := httpx.SafeDo(r.Context(), client, req)
	if err != nil {
		u.renderFrag(w, "frag_sub_parsed", parsedSubBody{Error: "获取订阅失败,请检查链接"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		u.renderFrag(w, "frag_sub_parsed", parsedSubBody{Error: fmt.Sprintf("订阅服务器返回 %d", resp.StatusCode)})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		u.renderFrag(w, "frag_sub_parsed", parsedSubBody{Error: "读取订阅内容失败"})
		return
	}
	nodes := parseSubscriptionContent(raw)
	u.renderFrag(w, "frag_sub_parsed", parsedSubBody{Nodes: nodes})
}

// parseSubscriptionContent detects format and extracts nodes.
func parseSubscriptionContent(raw []byte) []parsedNode {
	content := strings.TrimSpace(string(raw))
	// Try singbox JSON (outbounds array)
	if strings.HasPrefix(content, "{") {
		return parseSingboxJSON(content)
	}
	// Try Clash YAML (proxies array)
	if strings.Contains(content, "proxies:") {
		return parseClashYAML(content)
	}
	// Try base64 (line-based URI list)
	if nodes := parseBase64URIs(content); len(nodes) > 0 {
		return nodes
	}
	// Try plain URI list
	return parsePlainURIs(content)
}

func parseSingboxJSON(content string) []parsedNode {
	var cfg struct {
		Outbounds []struct {
			Type string `json:"type"`
			Tag  string `json:"tag"`
			// common fields for display
			Server string `json:"server"`
			Port   int    `json:"server_port"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(content), &cfg); err != nil {
		return nil
	}
	skip := map[string]bool{
		"direct": true, "block": true, "dns": true, "selector": true,
		"urltest": true, "loopback": true,
	}
	var nodes []parsedNode
	for _, o := range cfg.Outbounds {
		if skip[o.Type] {
			continue
		}
		nodes = append(nodes, parsedNode{
			Name:   o.Tag,
			Type:   o.Type,
			Server: o.Server,
			Port:   o.Port,
		})
	}
	return nodes
}

func parseClashYAML(content string) []parsedNode {
	// Simple line-based parser for Clash YAML proxies
	var nodes []parsedNode
	lines := strings.Split(content, "\n")
	inProxies := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "proxies:" {
			inProxies = true
			continue
		}
		if inProxies {
			// New top-level key ends proxies section
			if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && !strings.HasPrefix(trimmed, "-") {
				break
			}
			if strings.HasPrefix(trimmed, "- name:") || strings.HasPrefix(trimmed, "- {") {
				// Simple proxy entry
				name := extractYAMLValue(trimmed, "name")
				pType := extractYAMLValue(trimmed, "type")
				server := extractYAMLValue(trimmed, "server")
				port := extractYAMLInt(trimmed, "port")
				if name != "" {
					nodes = append(nodes, parsedNode{Name: name, Type: pType, Server: server, Port: port})
				}
			}
		}
	}
	return nodes
}

func extractYAMLValue(line, key string) string {
	idx := strings.Index(line, key+":")
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(key)+1:]
	rest = strings.TrimSpace(rest)
	// Handle quoted values
	if len(rest) > 1 && (rest[0] == '"' || rest[0] == '\'') {
		end := strings.Index(rest[1:], string(rest[0]))
		if end >= 0 {
			return rest[1 : end+1]
		}
		return rest[1:]
	}
	// Handle inline object {key: val}
	if rest == "{" {
		return ""
	}
	sp := strings.Fields(rest)
	if len(sp) > 0 {
		return strings.Trim(sp[0], ",")
	}
	return ""
}

func extractYAMLInt(line, key string) int {
	v := extractYAMLValue(line, key)
	n, _ := strconv.Atoi(v)
	return n
}

func parseBase64URIs(content string) []parsedNode {
	// Try to decode as base64
	decoded, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		// Try raw std encoding
		decoded, err = base64.RawStdEncoding.DecodeString(content)
		if err != nil {
			return nil
		}
	}
	return parsePlainURIs(string(decoded))
}

func parsePlainURIs(content string) []parsedNode {
	var nodes []parsedNode
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		node := parseSingleURI(line)
		if node.Name != "" {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

func parseSingleURI(line string) parsedNode {
	// Extract remark (last part after #)
	remark := ""
	if idx := strings.LastIndex(line, "#"); idx >= 0 {
		remark = line[idx+1:]
		line = line[:idx]
	}
	// URL-decode remark
	if decoded, err := url.QueryUnescape(remark); err == nil {
		remark = decoded
	}

	switch {
	case strings.HasPrefix(line, "vmess://"):
		return parseVmessURI(line, remark)
	case strings.HasPrefix(line, "trojan://"):
		return parseTrojanURI(line, remark)
	case strings.HasPrefix(line, "ss://"):
		return parseShadowsocksURI(line, remark)
	case strings.HasPrefix(line, "ssr://"):
		return parseShadowsocksRURI(line, remark)
	case strings.HasPrefix(line, "vless://"):
		return parseVlessURI(line, remark)
	case strings.HasPrefix(line, "hysteria://") || strings.HasPrefix(line, "hysteria2://") || strings.HasPrefix(line, "hy2://"):
		return parseHysteriaURI(line, remark)
	case strings.HasPrefix(line, "tuic://"):
		return parseTUICURI(line, remark)
	case strings.HasPrefix(line, "wg://") || strings.HasPrefix(line, "wireguard://"):
		return parseWireGuardURI(line, remark)
	default:
		return parsedNode{Name: remark, Type: "unknown"}
	}
}

func parseVmessURI(line, remark string) parsedNode {
	// vmess://base64(json)
	data := strings.TrimPrefix(line, "vmess://")
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(data)
	}
	if err != nil {
		return parsedNode{Name: remark, Type: "vmess"}
	}
	var v struct {
		Ps   string `json:"ps"`
		Add  string `json:"add"`
		Port int    `json:"port"`
		Type string `json:"net"`
	}
	json.Unmarshal(decoded, &v)
	name := v.Ps
	if name == "" {
		name = remark
	}
	return parsedNode{Name: name, Type: "vmess/" + v.Type, Server: v.Add, Port: v.Port, Remark: remark}
}

func parseTrojanURI(line, remark string) parsedNode {
	// trojan://password@host:port?params#remark
	line = strings.TrimPrefix(line, "trojan://")
	return parseGenericURI(line, "trojan", remark)
}

func parseShadowsocksURI(line, remark string) parsedNode {
	// ss://base64(method:password)@host:port#remark
	// or ss://base64(method:password@host:port)#remark
	line = strings.TrimPrefix(line, "ss://")
	return parseGenericURI(line, "ss", remark)
}

func parseShadowsocksRURI(line, remark string) parsedNode {
	line = strings.TrimPrefix(line, "ssr://")
	decoded, err := base64.RawStdEncoding.DecodeString(line)
	if err != nil {
		return parsedNode{Name: remark, Type: "ssr"}
	}
	parts := strings.Split(string(decoded), ":")
	if len(parts) >= 2 {
		server := parts[0]
		port := 0
		fmt.Sscanf(parts[1], "%d", &port)
		return parsedNode{Name: remark, Type: "ssr", Server: server, Port: port}
	}
	return parsedNode{Name: remark, Type: "ssr"}
}

func parseVlessURI(line, remark string) parsedNode {
	line = strings.TrimPrefix(line, "vless://")
	return parseGenericURI(line, "vless", remark)
}

func parseHysteriaURI(line, remark string) parsedNode {
	pType := "hysteria"
	if strings.HasPrefix(line, "hysteria2://") || strings.HasPrefix(line, "hy2://") {
		pType = "hysteria2"
	}
	line = strings.TrimPrefix(line, "hysteria2://")
	line = strings.TrimPrefix(line, "hy2://")
	line = strings.TrimPrefix(line, "hysteria://")
	return parseGenericURI(line, pType, remark)
}

func parseTUICURI(line, remark string) parsedNode {
	line = strings.TrimPrefix(line, "tuic://")
	return parseGenericURI(line, "tuic", remark)
}

func parseWireGuardURI(line, remark string) parsedNode {
	line = strings.TrimPrefix(line, "wg://")
	line = strings.TrimPrefix(line, "wireguard://")
	return parseGenericURI(line, "wireguard", remark)
}

func parseGenericURI(line, pType, remark string) parsedNode {
	// password@host:port?params or host:port?params
	atIdx := strings.LastIndex(line, "@")
	hostPort := line
	if atIdx >= 0 {
		hostPort = line[atIdx+1:]
	}
	// Remove query params
	if qIdx := strings.Index(hostPort, "?"); qIdx >= 0 {
		hostPort = hostPort[:qIdx]
	}
	// Remove fragment
	if fIdx := strings.Index(hostPort, "#"); fIdx >= 0 {
		hostPort = hostPort[:fIdx]
	}
	parts := strings.Split(hostPort, ":")
	server := ""
	port := 0
	if len(parts) >= 2 {
		server = parts[0]
		fmt.Sscanf(parts[len(parts)-1], "%d", &port)
	} else if len(parts) == 1 {
		server = parts[0]
	}
	name := remark
	if name == "" {
		name = server
	}
	return parsedNode{Name: name, Type: pType, Server: server, Port: port, Remark: remark}
}

func (u *UI) handleSubscriptionReset(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	res := u.apiSend(r, token, http.MethodPost, "/subscription/token", nil, nil)
	if res.Status != http.StatusOK {
		hxTrigger(w, toastErr(res.ProblemDetail()))
	} else {
		hxTrigger(w, toastOK("订阅令牌已生成,旧链接已失效"))
	}
	u.renderSubAfter(w, r, token)
}

func (u *UI) handleSubscriptionRevoke(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	res := u.apiSend(r, token, http.MethodDelete, "/subscription", nil, nil)
	if res.Status != http.StatusNoContent {
		hxTrigger(w, toastErr(res.ProblemDetail()))
	} else {
		hxTrigger(w, toastOK("订阅已吊销"))
	}
	u.renderSubAfter(w, r, token)
}

// handleSubscriptionQR serves the QR PNG bytes directly (img-src 'self').
func (u *UI) handleSubscriptionQR(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	body, err := u.buildSubscription(r, token, r.URL.Query().Get("format"))
	if err != nil || body.QRDataURI == "" {
		http.NotFound(w, r)
		return
	}
	const prefix = "data:image/png;base64,"
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(body.QRDataURI, prefix))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

func (u *UI) renderSubAfter(w http.ResponseWriter, r *http.Request, token string) {
	body, err := u.buildSubscription(r, token, "")
	if err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: err.Error()})
		return
	}
	u.renderFrag(w, "frag_sub_card", body)
}

// ---- settings page ----

type settingsBody struct {
	Base
	Settings *settingsView
	ETag     string
	Saved    bool
	Error    string
	FieldErr map[string]string
}

func (u *UI) pageSettings(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	body, err := u.buildSettings(r, token)
	if err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: err.Error()})
		return
	}
	body.Base = u.rd.base("settings", "设置 · sing-box hub", u.usernameOf(r), u.themeOf(r))
	u.renderPage(w, r, "settings", "设置 · sing-box hub", body)
}

func (u *UI) buildSettings(r *http.Request, token string) (settingsBody, error) {
	body := settingsBody{}
	res := u.call(r, token, http.MethodGet, "/settings", nil, nil)
	if res.Status != http.StatusOK {
		return body, fmt.Errorf("%s", res.ProblemDetail())
	}
	var sv settingsView
	if err := res.Decode(&sv); err != nil {
		return body, err
	}
	body.Settings = &sv
	body.ETag = res.Header.Get("ETag")
	return body, nil
}

func (u *UI) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderSettingsError(w, r, token, "表单解析失败", nil)
		return
	}
	etag := r.PostFormValue("etag")
	payload := map[string]any{
		"sampler_interval_seconds": atoiOr(r.PostFormValue("sampler_interval_seconds"), 5),
		"retention_days":           atoiOr(r.PostFormValue("retention_days"), 30),
		"ip_lookup_enabled":        r.PostFormValue("ip_lookup_enabled") == "on",
		"ip_lookup_provider_url":   strings.TrimSpace(r.PostFormValue("ip_lookup_provider_url")),
		"brand_name":               strings.TrimSpace(r.PostFormValue("brand_name")),
		"reset_cooldown_minutes":   atoiOr(r.PostFormValue("reset_cooldown_minutes"), 10),
	}
	if v := strings.TrimSpace(r.PostFormValue("singbox_bin_path")); v != "" {
		payload["singbox_bin_path"] = v
	} else {
		payload["singbox_bin_path"] = nil
	}
	res := u.apiSend(r, token, http.MethodPatch, "/settings", payload,
		map[string]string{"If-Match": etag, "Content-Type": "application/merge-patch+json"})
	switch {
	case res.Status == http.StatusOK:
		hxTrigger(w, toastOK("设置已保存,采集器已重载"))
		body, err := u.buildSettings(r, token)
		if err != nil {
			u.renderFrag(w, "frag_error", fragErr{Message: err.Error()})
			return
		}
		body.Saved = true
		securityHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		// Render settings form + sidebar brand OOB
		var buf bytes.Buffer
		if err := u.rd.render(&buf, "frag_settings_form", body); err != nil {
			u.logger.Error("render settings form", "error", err)
		}
		// OOB: update sidebar brand
		base := u.rd.base("settings", "设置 · sing-box hub", u.usernameOf(r), u.themeOf(r))
		u.loadBranding(r, &base)
		var oobBuf bytes.Buffer
		if err := u.rd.render(&oobBuf, "sidebar_brand_oob", base); err == nil {
			buf.Write(oobBuf.Bytes())
		}
		_, _ = w.Write(buf.Bytes())
	case res.Status == http.StatusPreconditionFailed:
		u.renderSettingsError(w, r, token, "设置已被其他会话修改,请基于最新内容重试。", nil)
	default:
		u.renderSettingsError(w, r, token, res.ProblemDetail(), formFieldErrors(res))
	}
}

// renderSettingsError reloads current settings and renders the form with an
// error banner (so the ETag the user retries against is fresh).
func (u *UI) renderSettingsError(w http.ResponseWriter, r *http.Request, token, msg string, fieldErrs map[string]string) {
	body, err := u.buildSettings(r, token)
	if err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: err.Error()})
		return
	}
	body.Error = msg
	body.FieldErr = fieldErrs
	u.renderFrag(w, "frag_settings_form", body)
}

func atoiOr(raw string, def int) int {
	n := 0
	if raw == "" {
		return def
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return def
	}
	return n
}

// handleFaviconUpload processes the favicon upload form.
func (u *UI) handleFaviconUpload(w http.ResponseWriter, r *http.Request) {
	u.handleBrandingUpload(w, r, "favicon")
}

// handleLogoUpload processes the logo upload form.
func (u *UI) handleLogoUpload(w http.ResponseWriter, r *http.Request) {
	u.handleBrandingUpload(w, r, "logo")
}

func (u *UI) handleBrandingUpload(w http.ResponseWriter, r *http.Request, field string) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	const maxImageBytes = 2 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxImageBytes+(64<<10))
	if err := r.ParseMultipartForm(maxImageBytes); err != nil {
		hxTrigger(w, toastErr("上传失败,请选择不超过 2 MiB 的图片"))
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, _, err := r.FormFile(field)
	if err != nil {
		hxTrigger(w, toastErr("请选择文件"))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxImageBytes+1))
	if err != nil || len(data) == 0 {
		hxTrigger(w, toastErr("文件为空或读取失败"))
		return
	}
	if len(data) > maxImageBytes {
		hxTrigger(w, toastErr("文件过大,最大 2 MiB"))
		return
	}
	res := u.call(r, token, http.MethodPost, "/settings/"+field, data,
		map[string]string{"Content-Type": "application/octet-stream"})
	if res.Status != http.StatusOK {
		hxTrigger(w, toastErr(res.ProblemDetail()))
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

func (u *UI) handleBrandingSave(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		hxTrigger(w, toastErr("表单解析失败"))
		return
	}
	defer r.MultipartForm.RemoveAll()

	brandName := strings.TrimSpace(r.FormValue("brand_name"))
	etag := r.FormValue("etag")

	// Save brand name via PATCH
	res := u.apiSend(r, token, http.MethodPatch, "/settings", map[string]any{
		"brand_name": brandName,
	}, map[string]string{"If-Match": etag, "Content-Type": "application/merge-patch+json"})
	if res.Status != http.StatusOK {
		u.renderSettingsError(w, r, token, "品牌名称保存失败: "+res.ProblemDetail(), nil)
		return
	}

	// Upload favicon if provided
	const maxImageBytes = 2 << 20
	if file, _, err := r.FormFile("favicon"); err == nil {
		defer file.Close()
		data, _ := io.ReadAll(io.LimitReader(file, maxImageBytes+1))
		if len(data) > 0 && len(data) <= maxImageBytes {
			u.call(r, token, http.MethodPost, "/settings/favicon", data,
				map[string]string{"Content-Type": "application/octet-stream"})
		}
	}

	// Upload logo if provided
	if file, _, err := r.FormFile("logo"); err == nil {
		defer file.Close()
		data, _ := io.ReadAll(io.LimitReader(file, maxImageBytes+1))
		if len(data) > 0 && len(data) <= maxImageBytes {
			u.call(r, token, http.MethodPost, "/settings/logo", data,
				map[string]string{"Content-Type": "application/octet-stream"})
		}
	}

	hxTrigger(w, toastOK("品牌设置已保存"))
	body, err := u.buildSettings(r, token)
	if err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: err.Error()})
		return
	}
	body.Saved = true
	// Render settings form + sidebar brand OOB
	securityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var buf bytes.Buffer
	if err := u.rd.render(&buf, "frag_settings_form", body); err != nil {
		u.logger.Error("render settings form", "error", err)
	}
	base := u.rd.base("settings", "设置 · sing-box hub", u.usernameOf(r), u.themeOf(r))
	u.loadBranding(r, &base)
	var oobBuf bytes.Buffer
	if err := u.rd.render(&oobBuf, "sidebar_brand_oob", base); err == nil {
		buf.Write(oobBuf.Bytes())
	}
	_, _ = w.Write(buf.Bytes())
}

// ---- user profile modal ----

type userProfileBody struct {
	Username               string
	IsAdmin                bool
	SubEnabled             bool
	SubURL                 string
	SubURLMask             string
	ResetCooldownRemaining string
}

// maskURL masks the middle part of the URL token for display.
// e.g. "https://example.com/sub/abc123xyz?format=clash" -> "https://example.com/sub/abc***xyz?format=clash"
func maskURL(url string) string {
	// Find the token part (between /sub/ and ?)
	subIdx := strings.Index(url, "/sub/")
	if subIdx == -1 {
		return url
	}
	subPart := url[subIdx+5:]
	qIdx := strings.Index(subPart, "?")
	var token string
	if qIdx == -1 {
		token = subPart
	} else {
		token = subPart[:qIdx]
	}
	if len(token) <= 6 {
		return url
	}
	masked := token[:3] + "***" + token[len(token)-3:]
	if qIdx == -1 {
		return url[:subIdx+5] + masked
	}
	return url[:subIdx+5] + masked + subPart[qIdx:]
}

func (u *UI) fragUserProfile(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	claims, err := u.verifyAccess(token)
	if err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: "获取用户信息失败"})
		return
	}

	body := userProfileBody{
		Username: claims.Username,
		IsAdmin:  claims.Role == "admin",
	}

	// Fetch subscription info
	var sub subscription
	if _, err := u.apiGetJSON(r, token, "/subscription", &sub); err == nil {
		body.SubEnabled = sub.Enabled
		if sub.Enabled && sub.Token != nil {
			body.SubURL = publicOrigin(r) + "/sub/" + *sub.Token + "?format=clash"
			body.SubURLMask = maskURL(body.SubURL)
		}
	}

	// Check reset cooldown
	userID := claims.UserID()
	lastResetAt, cooldownMinutes, err := u.store.GetResetCooldownInfo(userID)
	if err == nil && cooldownMinutes > 0 && lastResetAt > 0 {
		elapsed := time.Now().Unix() - lastResetAt
		remaining := int64(cooldownMinutes)*60 - elapsed
		if remaining > 0 {
			minutes := remaining / 60
			seconds := remaining % 60
			body.ResetCooldownRemaining = fmt.Sprintf("%d分%d秒", minutes, seconds)
		}
	}

	u.renderFrag(w, "frag_user_profile", body)
}

func (u *UI) handleUserProfileSubscriptionToken(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	// Check cooldown
	claims, err := u.auth.VerifyAccess(token)
	if err != nil {
		hxTrigger(w, toastErr("认证失败"))
		return
	}
	userID := claims.UserID()
	lastResetAt, cooldownMinutes, err := u.store.GetResetCooldownInfo(userID)
	if err != nil {
		hxTrigger(w, toastErr("查询冷却状态失败"))
		return
	}
	if cooldownMinutes > 0 && lastResetAt > 0 {
		elapsed := time.Now().Unix() - lastResetAt
		remaining := int64(cooldownMinutes)*60 - elapsed
		if remaining > 0 {
			minutes := remaining / 60
			seconds := remaining % 60
			hxTrigger(w, toastErr(fmt.Sprintf("冷却中,还需 %d分%d秒", minutes, seconds)))
			return
		}
	}
	// Update last_reset_at
	_ = u.store.SetResetNow(userID)
	res := u.apiSend(r, token, http.MethodPost, "/subscription/token", nil, nil)
	if res.Status != http.StatusOK {
		hxTrigger(w, toastErr(res.ProblemDetail()))
	} else {
		hxTrigger(w, toastOK("订阅令牌已生成,旧链接已失效"))
	}
	// Re-render the profile modal
	u.fragUserProfile(w, r)
}

func (u *UI) handleUserProfileSubscriptionRevoke(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	res := u.apiSend(r, token, http.MethodDelete, "/subscription", nil, nil)
	if res.Status != http.StatusNoContent {
		hxTrigger(w, toastErr(res.ProblemDetail()))
	} else {
		hxTrigger(w, toastOK("订阅已吊销"))
	}
	// Re-render the profile modal
	u.fragUserProfile(w, r)
}

func (u *UI) fragUserProfileEdit(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	claims, err := u.verifyAccess(token)
	if err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: "获取用户信息失败"})
		return
	}
	u.renderFrag(w, "frag_user_profile_edit", struct {
		Username string
		Error    string
	}{Username: claims.Username})
}

func (u *UI) handleUserProfileEdit(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		hxTrigger(w, toastErr("表单解析失败"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	claims, _ := u.verifyAccess(token)
	if claims == nil {
		hxTrigger(w, toastErr("用户信息获取失败"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	newPassword := r.PostFormValue("new_password")
	if username == "" {
		hxTrigger(w, toastErr("用户名不能为空"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	payload := map[string]any{}
	if username != claims.Username {
		payload["username"] = username
	}
	if newPassword != "" {
		payload["password"] = newPassword
	}
	if len(payload) == 0 {
		hxTrigger(w, toastErr("未修改任何内容"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	res := u.apiSend(r, token, http.MethodPatch, "/users/"+claims.Subject, payload, nil)
	if res.Status == http.StatusOK {
		var updated struct {
			Username string `json:"username"`
		}
		if err := res.Decode(&updated); err != nil || updated.Username == "" {
			hxTrigger(w, toastErr("个人信息更新响应异常"))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Updating a user revokes every previous refresh token. Issue a new
		// browser session immediately so a username/password change does not
		// strand the current page on an invalidated session.
		pair, err := u.auth.Issue(claims.UserID(), updated.Username, claims.Role)
		if err != nil {
			hxTrigger(w, toastErr("会话刷新失败,请重新登录"))
			clearSessionCookies(w)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		setSessionCookies(w, r, pair)
		hxTrigger(w, mergeEvents(toastOK("个人信息已更新"), map[string]any{"profile-updated": ""}))
		w.WriteHeader(http.StatusOK)
		return
	}
	hxTrigger(w, toastErr(res.ProblemDetail()))
	w.WriteHeader(http.StatusNoContent)
}

func (u *UI) fragUserProfileQR(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	var sub subscription
	if _, err := u.apiGetJSON(r, token, "/subscription", &sub); err != nil || !sub.Enabled || sub.Token == nil {
		u.renderFrag(w, "frag_error", fragErr{Message: "订阅未启用"})
		return
	}
	u.renderFrag(w, "frag_user_profile_qr", nil)
}

func (u *UI) fragUserProfileResetConfirm(w http.ResponseWriter, r *http.Request) {
	u.renderFrag(w, "frag_user_profile_reset_confirm", nil)
}

func (u *UI) handleUserProfileQRImage(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	var sub subscription
	if _, err := u.apiGetJSON(r, token, "/subscription", &sub); err != nil || !sub.Enabled || sub.Token == nil {
		http.NotFound(w, r)
		return
	}
	url := publicOrigin(r) + "/sub/" + *sub.Token + "?format=clash"
	code, err := qr.Encode(url, qr.M, qr.Auto)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	scaled, err := barcode.Scale(code, 220, 220)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	png.Encode(w, scaled)
}
