package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/sing-hub/panel/internal/cryptox"
	"github.com/sing-hub/panel/internal/geo"
	"github.com/sing-hub/panel/internal/httpx"
	"github.com/sing-hub/panel/internal/probe"
	"github.com/sing-hub/panel/internal/store"
	"github.com/sing-hub/panel/internal/subscribe"
)

// handleNodeStatus implements GET /nodes/{node_id}/status.
func (s *Server) handleNodeStatus(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeOr404(w, r)
	if !ok {
		return
	}
	status, known := s.hub.Status(node.ID)
	if !known {
		status.LastError = "node is disabled or not sampled"
	}
	body := map[string]any{
		"node_id":            node.ID,
		"online":             status.Online,
		"version":            nilEmpty(status.Version),
		"mode":               nilEmpty(status.Mode),
		"up_bps":             status.UpBPS,
		"down_bps":           status.DownBPS,
		"memory_bytes":       nil,
		"active_connections": status.ActiveConnections,
		"last_online_at":     nullableTime(node.LastOnline, node.HasLastOnl),
		"last_error":         status.LastError,
	}
	if status.HasMemory {
		body["memory_bytes"] = status.MemoryBytes
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

// nilEmpty maps empty strings to JSON null.
func nilEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// handleNodeTraffic implements GET /nodes/{node_id}/traffic.
func (s *Server) handleNodeTraffic(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeOr404(w, r)
	if !ok {
		return
	}
	interval := r.URL.Query().Get("interval")
	if interval == "" {
		interval = "minute"
	}
	if interval != "minute" && interval != "hour" && interval != "day" {
		httpx.Validation(w, r, "interval must be minute|hour|day",
			httpx.FieldErr("#interval", "one of minute, hour, day"))
		return
	}
	from, to, ok := parseTimeRange(r, interval)
	if !ok {
		httpx.Validation(w, r, "from/to must be RFC 3339 and from <= to")
		return
	}
	after := int64(0)
	if c := r.URL.Query().Get("cursor"); c != "" {
		parts, valid := decodeCursor(c)
		if !valid || len(parts) != 1 {
			httpx.Validation(w, r, "cursor is invalid", httpx.FieldErr("#cursor", "malformed cursor"))
			return
		}
		after, _ = strconv.ParseInt(parts[0], 10, 64)
	}

	rows, err := s.store.QueryRollup(node.ID, interval, from, to, after, 1000)
	if err != nil {
		s.logger.Error("query traffic failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{
			"bucket_start":    formatRFC3339(row.BucketStart),
			"interval":        row.Interval,
			"up_bytes":        row.UpBytes,
			"down_bytes":      row.DownBytes,
			"peak_up_bps":     row.PeakUpBps,
			"peak_down_bps":   row.PeakDownBps,
			"max_connections": row.MaxConnections,
		})
	}
	body := map[string]any{"items": items}
	if len(rows) == 1000 {
		next := encodeCursor(strconv.FormatInt(rows[len(rows)-1].BucketStart, 10))
		body["next_cursor"] = next
		// Preserve original query parameters in pagination links
		query := r.URL.Query()
		query.Set("cursor", next)
		body["links"] = map[string]string{"self": absoluteURL(r), "next": absoluteURL(r) + "?" + query.Encode()}
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

// handleNodeIps implements GET /nodes/{node_id}/ips.
func (s *Server) handleNodeIps(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeOr404(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	pageSize, ok := parsePositiveInt(q.Get("page_size"), 20, 100)
	if !ok {
		httpx.Validation(w, r, "page_size must be 1..100", httpx.FieldErr("#page_size", "must be 1..100"))
		return
	}
	sort := q.Get("sort")
	switch sort {
	case "", "total_bytes", "-total_bytes", "last_seen_at", "-last_seen_at":
	default:
		httpx.Validation(w, r, "sort must be total_bytes|last_seen_at with optional - prefix",
			httpx.FieldErr("#sort", "unsupported sort field"))
		return
	}
	if sort == "" {
		sort = "-total_bytes"
	}
	cursor := ""
	if c := q.Get("cursor"); c != "" {
		parts, valid := decodeCursor(c)
		if !valid || len(parts) != 2 {
			httpx.Validation(w, r, "cursor is invalid", httpx.FieldErr("#cursor", "malformed cursor"))
			return
		}
		cursor = parts[0] + "|" + parts[1]
	}

	rows, err := s.store.ListIPStats(node.ID, sort, q.Get("q"), cursor, pageSize+1)
	if err != nil {
		s.logger.Error("query ips failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	hasMore := len(rows) > pageSize
	if hasMore {
		rows = rows[:pageSize]
	}

	settings := s.currentSettings()
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, s.ipStatItem(r.Context(), row, settings))
	}
	body := map[string]any{"items": items}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		var key int64
		if sort == "last_seen_at" || sort == "-last_seen_at" {
			key = last.LastSeenAt
		} else {
			key = last.TotalBytes
		}
		next := encodeCursor(strconv.FormatInt(key, 10), last.SourceIP)
		body["next_cursor"] = next
		// Preserve original query parameters in pagination links
		query := r.URL.Query()
		query.Set("cursor", next)
		body["links"] = map[string]string{"self": absoluteURL(r), "next": absoluteURL(r) + "?" + query.Encode()}
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

func (s *Server) ipStatItem(ctx context.Context, row store.IPStat, settings *store.Settings) map[string]any {
	item := map[string]any{
		"node_id":            row.NodeID,
		"source_ip":          row.SourceIP,
		"upload_bytes":       row.UploadBytes,
		"download_bytes":     row.DownloadBytes,
		"total_bytes":        row.TotalBytes,
		"connections_total":  row.ConnectionsTotal,
		"active_connections": row.ActiveConnections,
		"first_seen_at":      formatRFC3339(row.FirstSeenAt),
		"last_seen_at":       formatRFC3339(row.LastSeenAt),
		"geo":                nil,
	}
	if settings != nil && settings.IPLookupEnabled {
		if g, err := s.geo().Lookup(ctx, row.SourceIP); err == nil && g != nil {
			item["geo"] = map[string]any{
				"country": g.Country, "region": g.Region,
				"city": g.City, "as_org": g.ASOrg,
			}
		}
	}
	return item
}

// handleNodeExitIP implements GET /nodes/{node_id}/exit-ip.
func (s *Server) handleNodeExitIP(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeOr404(w, r)
	if !ok {
		return
	}
	e, err := s.store.GetExitIP(node.ID)
	if errors.Is(err, store.ErrNotFound) {
		httpx.NotFound(w, r, "node has never been probed; POST /nodes/{node_id}/probe first")
		return
	}
	if err != nil {
		s.logger.Error("get exit ip failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"node_id":    e.NodeID,
		"ip":         e.IP,
		"country":    e.Country,
		"city":       e.City,
		"as_org":     e.ASOrg,
		"checked_at": formatRFC3339(e.CheckedAt),
		"method":     e.Method,
	})
}

// handleProbeNode implements POST /nodes/{node_id}/probe (202 + operation).
func (s *Server) handleProbeNode(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeOr404(w, r)
	if !ok {
		return
	}
	outbound, err := cryptox.Decrypt(s.cryptoKey, node.OutboundEnc)
	if err != nil || outbound == "" {
		httpx.Validation(w, r, "node has no outbound config; PATCH the node with outbound_json first")
		return
	}

	op := &store.Operation{ID: newUUID(), NodeID: node.ID, Type: "probe_exit_ip", Status: "PENDING"}
	if err := s.store.CreateOperation(op); err != nil {
		s.logger.Error("create operation failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}

	settings := s.currentSettings()
	runner := s.probeRunner(settings)

	// Use semaphore to limit concurrent probes
	select {
	case s.probeSem <- struct{}{}:
		go func() {
			defer func() { <-s.probeSem }()
			s.runProbe(node.ID, outbound, runner, op.ID)
		}()
	default:
		// Too many concurrent probes
		_ = s.store.UpdateOperation(op.ID, "FAILED", "", "too many concurrent probes")
	}

	w.Header().Set("Location", "/operations/"+op.ID)
	httpx.WriteJSON(w, http.StatusAccepted, operationBody(op))
}

func (s *Server) runProbe(nodeID, outbound string, runner *probe.Runner, opID string) {
	_ = s.store.UpdateOperation(opID, "RUNNING", "", "")
	result, err := runner.Probe(context.Background(), outbound)
	if err != nil {
		_ = s.store.UpdateOperation(opID, "FAILED", "", err.Error())
		return
	}
	exit := &store.ExitIP{
		NodeID: nodeID, IP: result.IP,
		Country: result.Country, City: result.City, ASOrg: result.ASOrg,
		Method: "singbox-probe", CheckedAt: time.Now().Unix(),
	}
	if err := s.store.UpsertExitIP(exit); err != nil {
		_ = s.store.UpdateOperation(opID, "FAILED", "", "persist result: "+err.Error())
		return
	}
	_ = s.store.UpdateOperation(opID, "SUCCEEDED", result.IP, "")
}

// handleGetOperation implements GET /operations/{operation_id}.
func (s *Server) handleGetOperation(w http.ResponseWriter, r *http.Request) {
	op, err := s.store.GetOperation(r.PathValue("operation_id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.NotFound(w, r, "operation not found")
		return
	}
	if err != nil {
		s.logger.Error("get operation failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, operationBody(op))
}

func operationBody(op *store.Operation) map[string]any {
	var result, opErr any
	if op.Result != "" {
		result = op.Result
	}
	if op.Error != "" {
		opErr = op.Error
	}
	return map[string]any{
		"id":         op.ID,
		"node_id":    op.NodeID,
		"type":       op.Type,
		"status":     op.Status,
		"result":     result,
		"error":      opErr,
		"created_at": formatRFC3339(op.CreatedAt),
		"updated_at": formatRFC3339(op.UpdatedAt),
	}
}

// ---- stats ----

// handleStatsOverview implements GET /stats/overview.
func (s *Server) handleStatsOverview(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.ListNodes()
	if err != nil {
		s.logger.Error("overview nodes failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	up, down, online, total := s.hub.Aggregate()

	now := time.Now()
	todayStart := now.UTC().Truncate(24 * time.Hour).Unix()
	var todayUp, todayDown int64
	for i := range nodes {
		u, d, err := s.store.SumRange(nodes[i].ID, todayStart, now.Unix())
		if err == nil {
			todayUp += u
			todayDown += d
		}
	}
	ipCount, err := s.store.CountDistinctIPs()
	if err != nil {
		s.logger.Error("overview ip count failed", slog.Any("error", err))
	}
	onlineIPs, err := s.store.CountOnlineIPs()
	if err != nil {
		s.logger.Error("overview online ip count failed", slog.Any("error", err))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"total_nodes":      total,
		"online_nodes":     online,
		"up_bps":           up,
		"down_bps":         down,
		"up_bytes_today":   todayUp,
		"down_bytes_today": todayDown,
		"total_ips_seen":   ipCount,
		"online_ips":       onlineIPs,
		"updated_at":       formatRFC3339(now.Unix()),
	})
}

// handleStatsNodes implements GET /stats/nodes: per-node dashboard cards
// (live rates, today's traffic, memory, version/mode) in one call.
func (s *Server) handleStatsNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.ListNodes()
	if err != nil {
		s.logger.Error("stats nodes failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	now := time.Now()
	todayStart := now.UTC().Truncate(24 * time.Hour).Unix()
	items := make([]map[string]any, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		status, known := s.hub.Status(n.ID)
		var todayUp, todayDown int64
		if u, d, err := s.store.SumRange(n.ID, todayStart, now.Unix()); err == nil {
			todayUp, todayDown = u, d
		}
		var totalUp, totalDown int64
		if u, d, err := s.store.SumAll(n.ID); err == nil {
			totalUp, totalDown = u, d
		}
		var memory any
		if known && status.HasMemory {
			memory = status.MemoryBytes
		}
		var version any
		if known && status.Version != "" {
			version = status.Version
		}
		var mode any
		if known && status.Mode != "" {
			mode = status.Mode
		}
		items = append(items, map[string]any{
			"id":                 n.ID,
			"name":               n.Name,
			"tags":               orEmpty(n.Tags),
			"enabled":            n.Enabled,
			"online":             status.Online,
			"has_outbound":       n.OutboundEnc != "",
			"up_bps":             status.UpBPS,
			"down_bps":           status.DownBPS,
			"active_connections": status.ActiveConnections,
			"today_up_bytes":     todayUp,
			"today_down_bytes":   todayDown,
			"total_up_bytes":     totalUp,
			"total_down_bytes":   totalDown,
			"memory_bytes":       memory,
			"version":            version,
			"mode":               mode,
			"last_online_at":     nullableTime(n.LastOnline, n.HasLastOnl),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "updated_at": formatRFC3339(now.Unix())})
}

// handleStatsTopNodes implements GET /stats/top-nodes.
func (s *Server) handleStatsTopNodes(w http.ResponseWriter, r *http.Request) {
	from, to, ok := parseTimeRange(r, "hour")
	if !ok {
		httpx.Validation(w, r, "from/to must be RFC 3339 and from <= to")
		return
	}
	limit, ok := parsePositiveInt(r.URL.Query().Get("limit"), 10, 100)
	if !ok {
		httpx.Validation(w, r, "limit must be 1..100", httpx.FieldErr("#limit", "must be 1..100"))
		return
	}
	rows, err := s.store.TopNodesByTraffic(from, to, limit)
	if err != nil {
		s.logger.Error("top nodes failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	names := map[string]string{}
	if nodes, err := s.store.ListNodes(); err == nil {
		for i := range nodes {
			names[nodes[i].ID] = nodes[i].Name
		}
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{
			"node_id":     row.NodeID,
			"name":        names[row.NodeID],
			"up_bytes":    row.UpBytes,
			"down_bytes":  row.DownBytes,
			"total_bytes": row.UpBytes + row.DownBytes,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleStatsTopIps implements GET /stats/top-ips.
func (s *Server) handleStatsTopIps(w http.ResponseWriter, r *http.Request) {
	from, to, ok := parseTimeRange(r, "hour")
	if !ok {
		httpx.Validation(w, r, "from/to must be RFC 3339 and from <= to")
		return
	}
	limit, ok := parsePositiveInt(r.URL.Query().Get("limit"), 10, 100)
	if !ok {
		httpx.Validation(w, r, "limit must be 1..100", httpx.FieldErr("#limit", "must be 1..100"))
		return
	}
	fromDay, toDay := dayOf(from), dayOf(to)
	rows, err := s.store.TopIPsByTraffic(fromDay, toDay, limit)
	if err != nil {
		s.logger.Error("top ips failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	settings := s.currentSettings()
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		row.TotalBytes = row.UploadBytes + row.DownloadBytes
		item := s.ipStatItem(r.Context(), row, settings)
		item["nodes_count"] = row.NodesCount
		items = append(items, item)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func dayOf(ts int64) int64 { return ts - ts%86400 }

// handleNodeShareLink implements GET /nodes/{node_id}/share-link.
func (s *Server) handleNodeShareLink(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeOr404(w, r)
	if !ok {
		return
	}
	outbound, err := cryptox.Decrypt(s.cryptoKey, node.OutboundEnc)
	if err != nil || outbound == "" {
		httpx.Validation(w, r, "node has no outbound config; PATCH the node with outbound_json first")
		return
	}
	uri, err := subscribe.ShareURI(subscribe.Node{Name: node.Name, Outbound: outbound})
	if err != nil {
		var ue *subscribe.UnsupportedError
		if !errors.As(err, &ue) {
			s.logger.Error("share link failed", slog.Any("error", err))
			httpx.Internal(w, r)
			return
		}
		uri = ""
	}
	var uriField any
	if uri != "" {
		uriField = uri
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"node_id":       node.ID,
		"name":          node.Name,
		"uri":           uriField,
		"outbound_json": outbound,
	})
}

// currentSettings loads settings; on failure returns nil (callers degrade).
func (s *Server) currentSettings() *store.Settings {
	st, err := s.store.GetSettings(store.SamplerDefaults{
		SamplerIntervalSeconds: int(s.cfg.Sampler.ConnInterval.Seconds()),
		RetentionDays:          s.cfg.Sampler.RetentionDays,
		IPLookupProviderURL:    s.cfg.Probe.IPProviderURL,
	})
	if err != nil {
		s.logger.Error("load settings failed", slog.Any("error", err))
		return nil
	}
	return st
}

func (s *Server) geo() *geo.Service {
	s.geoOnce.Do(func() {
		s.geoSvc = geo.New(s.store, s.currentSettings)
	})
	return s.geoSvc
}

// handlePublicStats implements GET /api/public/stats (no auth).
func (s *Server) handlePublicStats(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.ListNodes()
	if err != nil {
		httpx.Internal(w, r)
		return
	}
	up, down, online, total := s.hub.Aggregate()
	now := time.Now()
	todayStart := now.UTC().Truncate(24 * time.Hour).Unix()
	var todayUp, todayDown int64
	for i := range nodes {
		if u, d, err := s.store.SumRange(nodes[i].ID, todayStart, now.Unix()); err == nil {
			todayUp += u
			todayDown += d
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"total_nodes":      total,
		"online_nodes":     online,
		"up_bps":           up,
		"down_bps":         down,
		"up_bytes_today":   todayUp,
		"down_bytes_today": todayDown,
	})
}

// handlePublicNodes implements GET /api/public/nodes (no auth).
func (s *Server) handlePublicNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.ListNodes()
	if err != nil {
		httpx.Internal(w, r)
		return
	}
	now := time.Now()
	todayStart := now.UTC().Truncate(24 * time.Hour).Unix()
	items := make([]map[string]any, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		status, known := s.hub.Status(n.ID)
		var todayUp, todayDown int64
		if u, d, err := s.store.SumRange(n.ID, todayStart, now.Unix()); err == nil {
			todayUp, todayDown = u, d
		}
		var memory any
		if known && status.HasMemory {
			memory = status.MemoryBytes
		}
		var version any
		if known && status.Version != "" {
			version = status.Version
		}
		exitIP := ""
		if e, err := s.store.GetExitIP(n.ID); err == nil {
			exitIP = e.IP
		}
		items = append(items, map[string]any{
			"id":                 n.ID,
			"name":               n.Name,
			"tags":               orEmpty(n.Tags),
			"enabled":            n.Enabled,
			"online":             status.Online,
			"up_bps":             status.UpBPS,
			"down_bps":           status.DownBPS,
			"active_connections": status.ActiveConnections,
			"today_up_bytes":     todayUp,
			"today_down_bytes":   todayDown,
			"memory_bytes":       memory,
			"version":            version,
			"exit_ip":            exitIP,
			"last_online_at":     nullableTime(n.LastOnline, n.HasLastOnl),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
