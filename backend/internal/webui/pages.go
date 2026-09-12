package webui

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// ---- shared helpers ----

// hxTrigger sets the HX-Trigger response header (JSON event map).
func hxTrigger(w http.ResponseWriter, events map[string]any) {
	w.Header().Set("HX-Trigger", string(jsonMarshal(events)))
}

// mergeEvents merges two event maps into one (b overrides a).
func mergeEvents(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// toast events consumed by static/app.js.
func toastOK(text string) map[string]any {
	return map[string]any{"ui-toast": map[string]string{"type": "ok", "text": text}}
}

func toastErr(text string) map[string]any {
	return map[string]any{"ui-toast": map[string]string{"type": "err", "text": text}}
}

// rangeHours maps the range param to a lookback window.
func rangeHours(rangeKey string) int {
	switch rangeKey {
	case "hour":
		return 1
	case "24h":
		return 24
	case "7d":
		return 168
	case "30d":
		return 720
	default:
		return 6
	}
}

// isoRange builds RFC3339 from/to covering the lookback window.
func isoRange(hours int) (from, to string) {
	now := time.Now()
	return now.Add(-time.Duration(hours) * time.Hour).UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339)
}

// fetchSeries loads a traffic series and normalizes it ascending for the chart.
func (u *UI) fetchSeries(r *http.Request, token, target string) []chartPoint {
	var list seriesList
	if _, err := u.apiGetJSON(r, token, target, &list); err != nil {
		return nil
	}
	pts := make([]chartPoint, 0, len(list.Items))
	for _, it := range list.Items {
		t, err := time.Parse(time.RFC3339, it.BucketStart)
		if err != nil {
			continue
		}
		pts = append(pts, chartPoint{T: t.Unix(), Up: it.UpBytes, Down: it.DownBytes})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].T < pts[j].T })
	return pts
}

// ---- overview ----

type overviewBody struct {
	Base
	Ov       *overview
	Nodes    []nodeSummary
	Range    string
	Chart    template.HTML
	TopNodes []topNode
	TopIps   []ipRow
}

// ---- public status page (no auth) ----

type statusBody struct {
	Ov         *overview
	Nodes      []nodeCard
	Theme      string
	CSSVer     string
	Title      string
	FaviconURL string
	LogoURL    string
	BrandName  string
	LoggedIn   bool
	Username   string
	IsAdmin    bool
}

func (u *UI) pageStatus(w http.ResponseWriter, r *http.Request) {
	token := ""
	base := u.rd.base("", "sing-box hub", "", u.themeOf(r))
	u.loadBranding(r, &base)
	body := statusBody{Theme: base.Theme, CSSVer: base.CSSVer, Title: base.Title, FaviconURL: base.FaviconURL, LogoURL: base.LogoURL, BrandName: base.BrandName}
	// Check if user is logged in.
	if tk, ok := u.resolveSession(w, r); ok {
		if claims, err := u.verifyAccess(tk); err == nil {
			body.LoggedIn = true
			body.Username = claims.Username
			body.IsAdmin = claims.Role == "admin"
			token = tk
		}
	}
	var ov overview
	if _, err := u.apiGetJSON(r, token, "/api/public/stats", &ov); err == nil {
		body.Ov = &ov
	}
	var list struct {
		Items []nodeCard `json:"items"`
	}
	if _, err := u.apiGetJSON(r, token, "/api/public/nodes", &list); err == nil {
		body.Nodes = list.Items
	}
	securityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := u.rd.render(w, "status_page", body); err != nil {
		u.logger.Error("render status page", "error", err)
	}
}

func (u *UI) fragStatusOverview(w http.ResponseWriter, r *http.Request) {
	var ov overview
	_, _ = u.apiGetJSON(r, "", "/api/public/stats", &ov)
	u.renderFrag(w, "frag_status_stats", ov)
}

func (u *UI) fragStatusNodes(w http.ResponseWriter, r *http.Request) {
	var list struct {
		Items []nodeCard `json:"items"`
	}
	_, _ = u.apiGetJSON(r, "", "/api/public/nodes", &list)
	u.renderFrag(w, "frag_status_nodes", list.Items)
}

func (u *UI) pageOverview(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	rng := normalizeRange(r.URL.Query().Get("range"))
	body, err := u.buildOverview(r, token, rng)
	if err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: "加载总览失败:" + err.Error()})
		return
	}
	body.Base = u.rd.base("overview", "总览 · sing-box hub", u.usernameOf(r), u.themeOf(r))
	u.renderPage(w, r, "overview", "总览 · sing-box hub", body)
}

func (u *UI) buildOverview(r *http.Request, token, rng string) (overviewBody, error) {
	body := overviewBody{Range: rng}
	res, err := u.apiGetJSON(r, token, "/stats/overview", &body.Ov)
	if err != nil {
		return body, fmt.Errorf("%s", res.ProblemDetail())
	}
	var list nodeList
	if _, err := u.apiGetJSON(r, token, "/nodes?page_size=100", &list); err != nil {
		return body, fmt.Errorf("%s", res.ProblemDetail())
	}
	body.Nodes = list.Items
	from, to := isoRange(rangeHours(rng))
	var tops struct {
		Items []topNode `json:"items"`
	}
	if _, err := u.apiGetJSON(r, token, fmt.Sprintf("/stats/top-nodes?limit=5&from=%s&to=%s", url.QueryEscape(from), url.QueryEscape(to)), &tops); err == nil {
		body.TopNodes = tops.Items
	}
	var topIps struct {
		Items []ipRow `json:"items"`
	}
	if _, err := u.apiGetJSON(r, token, fmt.Sprintf("/stats/top-ips?limit=5&from=%s&to=%s", url.QueryEscape(from), url.QueryEscape(to)), &topIps); err == nil {
		body.TopIps = topIps.Items
	}
	series := u.fetchSeries(r, token, fmt.Sprintf("/stats/traffic-series?interval=minute&from=%s&to=%s", url.QueryEscape(from), url.QueryEscape(to)))
	body.Chart = areaChart(series, "ov")
	return body, nil
}

func (u *UI) fragOverviewStats(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	var ov overview
	res, err := u.apiGetJSON(r, token, "/stats/overview", &ov)
	if err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: "统计数据暂不可用"})
		u.logger.Error("overview stats", "status", res.Status)
		return
	}
	u.renderFrag(w, "frag_ov_stats", ov)
}

func (u *UI) fragOverviewChart(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	rng := normalizeRange(r.URL.Query().Get("range"))
	from, to := isoRange(rangeHours(rng))
	series := u.fetchSeries(r, token, fmt.Sprintf("/stats/traffic-series?interval=minute&from=%s&to=%s", url.QueryEscape(from), url.QueryEscape(to)))
	u.renderFrag(w, "frag_ov_chart", ovChart{Range: rng, Chart: areaChart(series, "ov")})
}

type ovChart struct {
	Range string
	Chart template.HTML
}

func (u *UI) fragOverviewTops(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	rng := normalizeRange(r.URL.Query().Get("range"))
	from, to := isoRange(rangeHours(rng))
	var tops struct {
		Items []topNode `json:"items"`
	}
	_, _ = u.apiGetJSON(r, token, fmt.Sprintf("/stats/top-nodes?limit=5&from=%s&to=%s", url.QueryEscape(from), url.QueryEscape(to)), &tops)
	var topIps struct {
		Items []ipRow `json:"items"`
	}
	_, _ = u.apiGetJSON(r, token, fmt.Sprintf("/stats/top-ips?limit=5&from=%s&to=%s", url.QueryEscape(from), url.QueryEscape(to)), &topIps)
	u.renderFrag(w, "frag_ov_tops", ovTops{Range: rng, TopNodes: tops.Items, TopIps: topIps.Items})
}

type ovTops struct {
	Range    string
	TopNodes []topNode
	TopIps   []ipRow
}

func normalizeRange(v string) string {
	switch v {
	case "hour", "24h":
		return v
	default:
		return "6h"
	}
}

// ---- node dashboard cards (overview) ----

func (u *UI) fragOverviewNodes(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	var list nodeCardList
	res, err := u.apiGetJSON(r, token, "/stats/nodes", &list)
	if err != nil {
		u.renderFrag(w, "frag_ov_nodes", nodeCardData{Cards: nil, Error: res.ProblemDetail()})
		return
	}
	sorted := make([]nodeCard, len(list.Items))
	copy(sorted, list.Items)
	sort.Slice(sorted, func(i, j int) bool {
		return (sorted[i].TodayUpBytes + sorted[i].TodayDownBytes) > (sorted[j].TodayUpBytes + sorted[j].TodayDownBytes)
	})
	n := 4
	if len(sorted) < n {
		n = len(sorted)
	}
	u.renderFrag(w, "frag_ov_nodes", nodeCardData{Cards: sorted[:n]})
}

type nodeCardData struct {
	Cards []nodeCard
	Error string
}

// ---- nodes list ----

type nodesBody struct {
	Base
	Nodes     []nodeSummary
	HostNodes []hostNodeView
}

type hostNodeView struct {
	nodeSummary
	UpBPS             int64
	DownBPS           int64
	ActiveConnections int
	TodayUpBytes      int64
	TodayDownBytes    int64
	TotalUpBytes      int64
	TotalDownBytes    int64
	ParsedNodes       []nodeCard
}

func (u *UI) pageNodes(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	var list nodeList
	if _, err := u.apiGetJSON(r, token, "/nodes?page_size=100", &list); err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: "主机列表加载失败:" + err.Error()})
		return
	}
	body := nodesBody{Nodes: rootNodes(list.Items), HostNodes: u.buildHostNodes(r, token, list.Items)}
	body.Base = u.rd.base("nodes", "主机 · sing-box hub", u.usernameOf(r), u.themeOf(r))
	u.renderPage(w, r, "nodes", "主机 · sing-box hub", body)
}

func (u *UI) fragNodeList(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	var list nodeList
	if _, err := u.apiGetJSON(r, token, "/nodes?page_size=100", &list); err != nil {
		u.renderFrag(w, "frag_node_list", nodesBody{Nodes: nil})
		return
	}
	u.renderFrag(w, "frag_node_list", nodesBody{Nodes: rootNodes(list.Items), HostNodes: u.buildHostNodes(r, token, list.Items)})
}

func (u *UI) fragNodeManage(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	var list nodeList
	if _, err := u.apiGetJSON(r, token, "/nodes?page_size=100", &list); err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: "主机列表加载失败"})
		return
	}
	u.renderFrag(w, "frag_node_manage", nodesBody{Nodes: rootNodes(list.Items), HostNodes: u.buildHostNodes(r, token, list.Items)})
}

func rootNodes(nodes []nodeSummary) []nodeSummary {
	roots := make([]nodeSummary, 0, len(nodes))
	for _, n := range nodes {
		if n.Source == "" {
			roots = append(roots, n)
		}
	}
	return roots
}

func (u *UI) buildHostNodes(r *http.Request, token string, nodes []nodeSummary) []hostNodeView {
	var stats nodeCardList
	if _, err := u.apiGetJSON(r, token, "/stats/nodes", &stats); err != nil {
		stats.Items = nil
	}
	byID := make(map[string]nodeCard, len(stats.Items))
	for _, item := range stats.Items {
		byID[item.ID] = item
	}
	hosts := make([]hostNodeView, 0)
	byHost := make(map[string]int)
	for _, n := range nodes {
		if n.Source != "" {
			continue
		}
		h := hostNodeView{nodeSummary: n}
		h.addStats(byID[n.ID])
		if n.ConfigImported && (n.ParsedName != "" || n.ParsedType != "" || n.Server != "") {
			h.ParsedNodes = append(h.ParsedNodes, nodeCard{
				ID: n.ID, Name: n.ParsedName, ParsedName: n.ParsedName,
				ParsedType: n.ParsedType, Server: n.Server, ServerPort: n.ServerPort,
			})
		}
		byHost[n.ID] = len(hosts)
		hosts = append(hosts, h)
	}
	for _, n := range nodes {
		if n.Source == "" || n.HostID == "" {
			continue
		}
		i, ok := byHost[n.HostID]
		if !ok {
			continue
		}
		child := byID[n.ID]
		child.ID = n.ID
		child.Name = n.Name
		child.Source = n.Source
		child.ParsedName = n.ParsedName
		child.ParsedType = n.ParsedType
		child.Server = n.Server
		child.ServerPort = n.ServerPort
		hosts[i].ParsedNodes = append(hosts[i].ParsedNodes, child)
		hosts[i].addStats(child)
	}
	return hosts
}

func (h *hostNodeView) addStats(s nodeCard) {
	h.UpBPS += s.UpBPS
	h.DownBPS += s.DownBPS
	h.ActiveConnections += s.ActiveConnections
	h.TodayUpBytes += s.TodayUpBytes
	h.TodayDownBytes += s.TodayDownBytes
	h.TotalUpBytes += s.TotalUpBytes
	h.TotalDownBytes += s.TotalDownBytes
}

// parseTags splits a comma-separated tag field.
func parseTags(raw string) []string {
	parts := strings.Split(raw, ",")
	tags := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

func (u *UI) fragNodeFormNew(w http.ResponseWriter, r *http.Request) {
	u.renderFrag(w, "frag_node_form", nodeForm{
		Mode:    "create",
		Enabled: true,
	})
}

type nodeForm struct {
	Mode         string // "create" | "edit"
	NodeID       string
	Name         string
	APIURL       string
	APISecretSet bool
	OutboundJSON string
	ConfigURL    string
	KCEKeySet    bool
	Tags         string
	Enabled      bool
	FieldErrors  map[string]string
	FormError    string
}

func (u *UI) fragNodeFormEdit(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	nodeID := r.PathValue("node_id")
	var node nodeFull
	res, err := u.apiGetJSON(r, token, "/nodes/"+url.PathEscape(nodeID)+"?include=outbound", &node)
	if err != nil {
		u.renderFrag(w, "frag_node_form", nodeForm{Mode: "edit", FormError: res.ProblemDetail()})
		return
	}
	outbound := ""
	if node.OutboundJSON != nil {
		outbound = *node.OutboundJSON
	}
	u.renderFrag(w, "frag_node_form", nodeForm{
		Mode:         "edit",
		NodeID:       node.ID,
		Name:         node.Name,
		APIURL:       node.APIURL,
		APISecretSet: node.APISecretSet,
		OutboundJSON: outbound,
		ConfigURL:    node.ConfigURL,
		KCEKeySet:    node.KCEKeySet,
		Tags:         strings.Join(node.Tags, ", "),
		Enabled:      node.Enabled,
	})
}

// formFieldErrors maps problem validation pointers to form field names.
func formFieldErrors(res apiResult) map[string]string {
	errs := map[string]string{}
	if p := res.Problem(); p != nil {
		for _, fe := range p.Errors {
			field := strings.TrimPrefix(strings.TrimPrefix(fe.Pointer, "#/"), "#")
			field = strings.TrimPrefix(field, "/")
			if field == "" {
				continue
			}
			if _, dup := errs[field]; !dup {
				errs[field] = fe.Detail
			}
		}
	}
	return errs
}

func (u *UI) handleNodeCreate(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderFrag(w, "frag_node_form", nodeForm{Mode: "create", FormError: "表单解析失败", Enabled: true})
		return
	}
	enabled := r.PostFormValue("enabled") == "on"
	payload := map[string]any{
		"name":       strings.TrimSpace(r.PostFormValue("name")),
		"api_url":    strings.TrimSpace(r.PostFormValue("api_url")),
		"api_secret": r.PostFormValue("api_secret"),
		"tags":       parseTags(r.PostFormValue("tags")),
		"enabled":    enabled,
		"remark":     "",
	}
	if ob := strings.TrimSpace(r.PostFormValue("outbound_json")); ob != "" {
		payload["outbound_json"] = ob
	} else {
		payload["outbound_json"] = nil
	}
	if cu := strings.TrimSpace(r.PostFormValue("config_url")); cu != "" {
		payload["config_url"] = cu
	}
	if key := r.PostFormValue("kce_key"); key != "" {
		payload["kce_key"] = key
	}
	res := u.apiSend(r, token, http.MethodPost, "/nodes", payload, nil)
	switch {
	case res.Status == http.StatusCreated:
		hxTrigger(w, mergeEvents(toastOK("主机已创建"+importToastSuffix(res)), map[string]any{"refresh-nodes": ""}))
		w.WriteHeader(http.StatusOK)
		return
	}
	form := nodeForm{Mode: "create", Name: payload["name"].(string), APIURL: payload["api_url"].(string),
		OutboundJSON: r.PostFormValue("outbound_json"), ConfigURL: r.PostFormValue("config_url"),
		Tags: r.PostFormValue("tags"), Enabled: enabled,
		FieldErrors: formFieldErrors(res)}
	if len(form.FieldErrors) == 0 {
		form.FormError = res.ProblemDetail()
	}
	u.renderFrag(w, "frag_node_form", form)
}

func (u *UI) handleNodeUpdate(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	nodeID := r.PathValue("node_id")
	if !u.requireForm(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderFrag(w, "frag_node_form", nodeForm{Mode: "edit", NodeID: nodeID, FormError: "表单解析失败"})
		return
	}
	enabled := r.PostFormValue("enabled") == "on"
	payload := map[string]any{
		"name":    strings.TrimSpace(r.PostFormValue("name")),
		"tags":    parseTags(r.PostFormValue("tags")),
		"enabled": enabled,
	}
	if v := strings.TrimSpace(r.PostFormValue("api_url")); v != "" {
		payload["api_url"] = v
	}
	if v := r.PostFormValue("api_secret"); v != "" {
		payload["api_secret"] = v
	}
	if v := strings.TrimSpace(r.PostFormValue("outbound_json")); v != "" {
		payload["outbound_json"] = v
	}
	if v := strings.TrimSpace(r.PostFormValue("config_url")); v != "" {
		payload["config_url"] = v
	}
	if v := r.PostFormValue("kce_key"); v != "" {
		payload["kce_key"] = v
	}
	res := u.apiSend(r, token, http.MethodPatch, "/nodes/"+url.PathEscape(nodeID), payload, nil)
	if res.Status == http.StatusOK {
		hxTrigger(w, mergeEvents(toastOK("主机已更新"+importToastSuffix(res)), map[string]any{"refresh-nodes": ""}))
		// 200 with empty body so htmx clears the modal slot.
		w.WriteHeader(http.StatusOK)
		return
	}
	form := nodeForm{Mode: "edit", NodeID: nodeID, Name: payload["name"].(string),
		OutboundJSON: r.PostFormValue("outbound_json"), Tags: r.PostFormValue("tags"), Enabled: enabled,
		FieldErrors: formFieldErrors(res)}
	if len(form.FieldErrors) == 0 {
		form.FormError = res.ProblemDetail()
	}
	u.renderFrag(w, "frag_node_form", form)
}

// importStatsView mirrors the optional decrypt-import statistics attached to
// node create/update responses.
type importStatsView struct {
	Found    int `json:"found"`
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
}

// importToastSuffix renders the decrypt-import note for success toasts; ""
// when the mutation did not import anything.
func importToastSuffix(res apiResult) string {
	if len(res.Body) == 0 {
		return ""
	}
	var body struct {
		Import *importStatsView `json:"import"`
	}
	if json.Unmarshal(res.Body, &body) != nil || body.Import == nil {
		return ""
	}
	switch {
	case body.Import.Found > 1:
		return fmt.Sprintf(" · 解密到 %d 个代理,已全部导入为节点", body.Import.Found)
	case body.Import.Found == 1:
		return " · 出站已从加密配置导入"
	default:
		return ""
	}
}

func (u *UI) handleNodeDelete(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	nodeID := r.PathValue("node_id")
	res := u.apiSend(r, token, http.MethodDelete, "/nodes/"+url.PathEscape(nodeID), nil, nil)
	switch res.Status {
	case http.StatusNoContent:
		hxTrigger(w, mergeEvents(toastOK("主机已删除"), map[string]any{"refresh-nodes": ""}))
		w.WriteHeader(http.StatusNoContent)
	default:
		hxTrigger(w, toastErr(res.ProblemDetail()))
		w.WriteHeader(http.StatusNoContent)
	}
}

func (u *UI) handleNodeToggle(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	nodeID := r.PathValue("node_id")
	if err := r.ParseForm(); err != nil {
		hxTrigger(w, toastErr("表单解析失败"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	enable := r.PostFormValue("enable") == "true"
	res := u.apiSend(r, token, http.MethodPatch, "/nodes/"+url.PathEscape(nodeID),
		map[string]any{"enabled": enable}, nil)
	switch res.Status {
	case http.StatusOK:
		msg := "主机已停用"
		if enable {
			msg = "主机已启用"
		}
		hxTrigger(w, mergeEvents(toastOK(msg), map[string]any{"refresh-nodes": ""}))
		w.WriteHeader(http.StatusNoContent)
	default:
		hxTrigger(w, toastErr(res.ProblemDetail()))
		w.WriteHeader(http.StatusNoContent)
	}
}

func (u *UI) handleNodeCheck(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	nodeID := r.PathValue("node_id")
	res := u.apiSend(r, token, http.MethodPost, "/nodes/"+url.PathEscape(nodeID)+"/check", nil, nil)
	if res.Status != http.StatusOK {
		hxTrigger(w, toastErr("检查失败:"+res.ProblemDetail()))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var cr checkResult
	if err := res.Decode(&cr); err != nil {
		hxTrigger(w, mergeEvents(toastErr("检查响应异常"), map[string]any{"refresh-nodes": ""}))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var events map[string]any
	if cr.Online {
		ver := ""
		if cr.Version != nil {
			ver = " · " + *cr.Version
		}
		lat := ""
		if cr.LatencyMS != nil {
			lat = fmt.Sprintf(" · %dms", *cr.LatencyMS)
		}
		events = mergeEvents(toastOK("在线"+ver+lat), map[string]any{"refresh-nodes": ""})
	} else {
		detail := ""
		if cr.Detail != nil {
			detail = *cr.Detail
		}
		events = mergeEvents(toastErr("离线 · "+detail), map[string]any{"refresh-nodes": ""})
	}
	hxTrigger(w, events)
	w.WriteHeader(http.StatusNoContent)
}

// jsonMarshal marshals v with non-ASCII runes escaped so the HX-Trigger
// header survives ISO-8859-1 header transport (Chinese toast text).
func jsonMarshal(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	var b strings.Builder
	for _, r := range string(raw) {
		if r < 0x80 {
			b.WriteRune(r)
			continue
		}
		for _, u := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&b, `\u%04x`, u)
		}
	}
	return []byte(b.String())
}

// ---- user management ----

type userItem struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	CreatedAt string `json:"created_at"`
}

type mergedRow struct {
	IsUser     bool
	User       *userItem
	SourceIP   string
	Geo        *geoInfo
	TotalBytes int64
	NodesCount int
}

type usersBody struct {
	Base
	Users      []userItem
	Error      string
	Tab        string
	Query      string
	Page       int
	TotalCount int
	TotalPages int
	MergedRows []mergedRow
}

func (u *UI) pageUsers(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	body := u.buildUsers(r, token, query, page)
	body.Base = u.rd.base("users", "用户 · sing-box hub", u.usernameOf(r), u.themeOf(r))
	body.Tab = "users"
	body.Query = query
	body.Page = page
	u.renderPage(w, r, "users_ip", "用户 · sing-box hub", body)
}

func (u *UI) buildUsers(r *http.Request, token, query string, page int) usersBody {
	body := usersBody{}
	pageSize := 20

	// Fetch users
	cursor := ""
	fetched := make([]userItem, 0)
	for {
		var list struct {
			Items      []userItem `json:"items"`
			NextCursor string     `json:"next_cursor"`
		}
		if _, err := u.apiGetJSON(r, token, "/users?page_size=100&cursor="+url.QueryEscape(cursor), &list); err != nil {
			body.Error = err.Error()
			return body
		}
		fetched = append(fetched, list.Items...)
		if list.NextCursor == "" || list.NextCursor == cursor {
			break
		}
		cursor = list.NextCursor
	}

	// Fetch top IPs
	type ipRow struct {
		SourceIP   string   `json:"source_ip"`
		TotalBytes int64    `json:"total_bytes"`
		Geo        *geoInfo `json:"geo"`
		NodesCount int      `json:"nodes_count"`
	}
	var topIPs struct {
		Items []ipRow `json:"items"`
	}
	_, _ = u.apiGetJSON(r, token, "/stats/top-ips?limit=100", &topIPs)

	// Build merged rows
	var rows []mergedRow
	for i := range fetched {
		rows = append(rows, mergedRow{IsUser: true, User: &fetched[i]})
	}
	for i := range topIPs.Items {
		ip := &topIPs.Items[i]
		rows = append(rows, mergedRow{
			IsUser:     false,
			SourceIP:   ip.SourceIP,
			Geo:        ip.Geo,
			TotalBytes: ip.TotalBytes,
			NodesCount: ip.NodesCount,
		})
	}

	// Filter
	if query != "" {
		q := strings.ToLower(query)
		filtered := make([]mergedRow, 0)
		for _, row := range rows {
			if row.IsUser && strings.Contains(strings.ToLower(row.User.Username), q) {
				filtered = append(filtered, row)
			} else if !row.IsUser && strings.Contains(strings.ToLower(row.SourceIP), q) {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}

	body.TotalCount = len(rows)
	body.TotalPages = (body.TotalCount + pageSize - 1) / pageSize
	if body.TotalPages < 1 {
		body.TotalPages = 1
	}
	start := (page - 1) * pageSize
	if start >= len(rows) {
		start = len(rows)
	}
	end := start + pageSize
	if end > len(rows) {
		end = len(rows)
	}
	body.MergedRows = rows[start:end]
	body.Users = fetched
	return body
}

func (u *UI) fragUserList(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	body := u.buildUsers(r, token, query, page)
	body.Query = query
	body.Page = page
	u.renderFrag(w, "frag_user_list", body)
}

type userForm struct {
	UserID   string
	Username string
	Error    string
}

func (u *UI) fragUserFormNew(w http.ResponseWriter, r *http.Request) {
	u.renderFrag(w, "frag_user_form", userForm{})
}

func (u *UI) fragUserFormPassword(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	var user userItem
	id := r.PathValue("user_id")
	if _, err := u.apiGetJSON(r, token, "/users/"+url.PathEscape(id), &user); err != nil {
		u.renderFrag(w, "frag_error", fragErr{Message: err.Error()})
		return
	}
	u.renderFrag(w, "frag_user_form", userForm{UserID: id, Username: user.Username})
}

func (u *UI) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderFrag(w, "frag_user_form", userForm{Error: "表单解析失败"})
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	res := u.apiSend(r, token, http.MethodPost, "/users", map[string]string{
		"username": username, "password": password,
	}, nil)
	if res.Status == http.StatusCreated {
		hxTrigger(w, mergeEvents(toastOK("用户已创建"), map[string]any{"refresh-users": ""}))
	} else {
		u.renderFrag(w, "frag_user_form", userForm{Username: username, Error: res.ProblemDetail()})
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (u *UI) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	userID := r.PathValue("user_id")
	res := u.apiSend(r, token, http.MethodDelete, "/users/"+userID, nil, nil)
	if res.Status == http.StatusNoContent {
		hxTrigger(w, mergeEvents(toastOK("用户已删除"), map[string]any{"refresh-users": ""}))
	} else {
		hxTrigger(w, toastErr(res.ProblemDetail()))
	}
	w.WriteHeader(http.StatusNoContent)
}

func (u *UI) handleUserPassword(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		hxTrigger(w, toastErr("表单解析失败"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	userID := r.PathValue("user_id")
	claims, _ := u.verifyAccess(token)
	password := r.PostFormValue("password")
	res := u.apiSend(r, token, http.MethodPatch, "/users/"+userID, map[string]string{
		"password": password,
	}, nil)
	if res.Status == http.StatusOK {
		if claims != nil && claims.Subject == userID {
			clearSessionCookies(w)
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusOK)
			return
		}
		hxTrigger(w, toastOK("密码已修改"))
	} else {
		u.renderFrag(w, "frag_user_form", userForm{UserID: userID, Username: r.PostFormValue("username"), Error: res.ProblemDetail()})
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (u *UI) handleUserReset(w http.ResponseWriter, r *http.Request) {
	token, _ := ctxToken(r.Context())
	if !u.requireForm(w, r) {
		return
	}
	userID := r.PathValue("user_id")
	res := u.apiSend(r, token, http.MethodPatch, "/users/"+userID, map[string]string{
		"password": "a123456",
	}, nil)
	if res.Status == http.StatusOK {
		hxTrigger(w, toastOK("密码已重置为 a123456"))
	} else {
		hxTrigger(w, toastErr("重置失败: "+res.ProblemDetail()))
	}
	w.WriteHeader(http.StatusOK)
}
