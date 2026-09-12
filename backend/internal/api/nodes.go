package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sing-hub/panel/internal/cryptox"
	"github.com/sing-hub/panel/internal/httpx"
	"github.com/sing-hub/panel/internal/store"
	"github.com/sing-hub/panel/internal/subscribe"
)

// nodeView is the JSON projection of a node (secret never leaves the server).
type nodeView struct {
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
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	LastOnlineAt *string  `json:"last_online_at"`
}

func (s *Server) viewNode(n *store.Node, withOutbound bool) nodeView {
	v := nodeView{
		ID:           n.ID,
		Name:         n.Name,
		APIURL:       n.APIURL,
		APISecretSet: n.APISecretEnc != "",
		Source:       n.Source,
		ConfigURL:    n.ConfigURL,
		KCEKeySet:    n.KCEKeyEnc != "",
		Tags:         n.Tags,
		Enabled:      n.Enabled,
		Remark:       n.Remark,
		CreatedAt:    formatRFC3339(n.CreatedAt),
		UpdatedAt:    formatRFC3339(n.UpdatedAt),
	}
	if n.HasLastOnl && n.LastOnline > 0 {
		t := formatRFC3339(n.LastOnline)
		v.LastOnlineAt = &t
	}
	if withOutbound && n.OutboundEnc != "" {
		if plain, err := cryptox.Decrypt(s.cryptoKey, n.OutboundEnc); err == nil {
			v.OutboundJSON = &plain
		}
	}
	return v
}

// handleListNodes implements GET /nodes (keyset pagination by name).
func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pageSize, ok := parsePositiveInt(q.Get("page_size"), 20, 100)
	if !ok {
		httpx.Validation(w, r, "page_size must be 1..100", httpx.FieldErr("#page_size", "must be 1..100"))
		return
	}

	nodes, err := s.store.ListNodes()
	if err != nil {
		s.logger.Error("list nodes failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}

	nameFilter := strings.ToLower(q.Get("q"))
	tagFilter := q.Get("tag")
	enabledFilter := q.Get("enabled")

	afterName := ""
	if cursor := q.Get("cursor"); cursor != "" {
		parts, ok := decodeCursor(cursor)
		if !ok || len(parts) != 1 {
			httpx.Validation(w, r, "cursor is invalid", httpx.FieldErr("#cursor", "malformed cursor"))
			return
		}
		afterName = parts[0]
	}

	items := []map[string]any{}
	nextName := ""
	lastName := ""
	for i := range nodes {
		n := &nodes[i]
		if afterName != "" && n.Name <= afterName {
			continue
		}
		if nameFilter != "" && !strings.Contains(strings.ToLower(n.Name), nameFilter) {
			continue
		}
		if tagFilter != "" && !containsTag(n.Tags, tagFilter) {
			continue
		}
		if enabledFilter != "" {
			if (enabledFilter == "true") != n.Enabled {
				continue
			}
		}
		if len(items) >= pageSize {
			// 本页已满:游标指向最后一条已返回的记录,下页从其后继续
			nextName = lastName
			break
		}
		items = append(items, s.summaryItem(n))
		lastName = n.Name
	}

	body := map[string]any{"items": items}
	if nextName != "" {
		body["next_cursor"] = encodeCursor(nextName)
		// Preserve original query parameters in pagination links
		query := r.URL.Query()
		query.Set("cursor", encodeCursor(nextName))
		body["links"] = map[string]string{
			"self": absoluteURL(r),
			"next": absoluteURL(r) + "?" + query.Encode(),
		}
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

func (s *Server) summaryItem(n *store.Node) map[string]any {
	status, _ := s.hub.Status(n.ID)
	item := map[string]any{
		"id":                 n.ID,
		"name":               n.Name,
		"api_url":            n.APIURL,
		"config_imported":    n.ConfigURL != "",
		"tags":               n.Tags,
		"enabled":            n.Enabled,
		"source":             n.Source,
		"online":             status.Online,
		"has_outbound":       n.OutboundEnc != "",
		"up_bps":             status.UpBPS,
		"down_bps":           status.DownBPS,
		"active_connections": status.ActiveConnections,
		"last_online_at":     nullableTime(n.LastOnline, n.HasLastOnl),
	}
	if parsed := s.parsedOutboundSummary(n); parsed != nil {
		for key, value := range parsed {
			item[key] = value
		}
	}
	if strings.HasPrefix(n.Source, "node:") {
		item["host_id"] = strings.TrimPrefix(n.Source, "node:")
	} else {
		item["host_id"] = n.ID
	}
	return item
}

// parsedOutboundSummary exposes only the non-sensitive identity fields needed
// by the server-rendered node list. Credentials remain encrypted and private.
func (s *Server) parsedOutboundSummary(n *store.Node) map[string]any {
	if n.OutboundEnc == "" {
		return nil
	}
	plain, err := cryptox.Decrypt(s.cryptoKey, n.OutboundEnc)
	if err != nil {
		return nil
	}
	var outbound struct {
		Tag        string `json:"tag"`
		Type       string `json:"type"`
		Server     string `json:"server"`
		ServerPort int    `json:"server_port"`
	}
	if json.Unmarshal([]byte(plain), &outbound) != nil {
		return nil
	}
	return map[string]any{
		"parsed_name": outbound.Tag,
		"parsed_type": outbound.Type,
		"server":      outbound.Server,
		"server_port": outbound.ServerPort,
	}
}

func containsTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func nullableTime(ts int64, valid bool) any {
	if !valid || ts <= 0 {
		return nil
	}
	return formatRFC3339(ts)
}

func absoluteURL(r *http.Request) string {
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

	return scheme + "://" + host + r.URL.Path
}

// nodeImportResponse attaches decrypt-import statistics to a node payload.
type nodeImportResponse struct {
	nodeView
	Import *importStats `json:"import,omitempty"`
}

// importStats summarizes one decrypt-import run.
type importStats struct {
	Found    int `json:"found"`
	Imported int `json:"imported"`
	Updated  int `json:"updated"`
	Removed  int `json:"removed"`
	Skipped  int `json:"skipped"`
}

// upstreamClient fetches remote config payloads with a hard timeout.
func (s *Server) upstreamClient() *http.Client {
	return httpx.SafeHTTPClient(15 * time.Second)
}

// uniqueNodeName returns base when free, otherwise an unused "base #N".
func (s *Server) uniqueNodeName(base string) string {
	if _, err := s.store.GetNodeByName(base); errors.Is(err, store.ErrNotFound) {
		return base
	}
	for i := 2; ; i++ {
		candidate := base + " #" + strconv.Itoa(i)
		if _, err := s.store.GetNodeByName(candidate); errors.Is(err, store.ErrNotFound) {
			return candidate
		} else if err != nil {
			return candidate
		}
	}
}

// handleCreateNode implements POST /nodes. When config_url is provided the
// payload is fetched, decrypted (KCE1 when needed) and every parsed proxy is
// imported as a panel node automatically; the created node anchors the set.
func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	userID, _ := ctxUserID(r)
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		// Create a unique key binding user + idempotency key
		idemKey := fmt.Sprintf("create-node:%d:%s", userID, key)
		if entry, ok := s.idem.get(idemKey); ok {
			w.Header().Set("Location", entry.location)
			httpx.WriteRawJSON(w, entry.status, entry.body)
			return
		}
	}

	var input struct {
		Name         string   `json:"name"`
		APIURL       string   `json:"api_url"`
		APISecret    string   `json:"api_secret"`
		OutboundJSON *string  `json:"outbound_json"`
		ConfigURL    string   `json:"config_url"`
		KCEKey       string   `json:"kce_key"`
		Tags         []string `json:"tags"`
		Enabled      *bool    `json:"enabled"`
		Remark       string   `json:"remark"`
	}
	if !httpx.DecodeJSON(w, r, &input) {
		return
	}
	if errs := validateNodeInput(&input, true); len(errs) > 0 {
		httpx.Validation(w, r, "node input failed validation", errs...)
		return
	}

	secretEnc, err := cryptox.Encrypt(s.cryptoKey, input.APISecret)
	if err != nil {
		httpx.Internal(w, r)
		return
	}
	outboundEnc := ""
	if input.OutboundJSON != nil && strings.TrimSpace(*input.OutboundJSON) != "" {
		if outboundEnc, err = cryptox.Encrypt(s.cryptoKey, strings.TrimSpace(*input.OutboundJSON)); err != nil {
			httpx.Internal(w, r)
			return
		}
	}
	configURL := strings.TrimRight(strings.TrimSpace(input.ConfigURL), "/")
	kceKeyEnc := ""
	var imported *importStats
	if configURL != "" {
		if kceKeyEnc, err = cryptox.Encrypt(s.cryptoKey, input.KCEKey); err != nil {
			httpx.Internal(w, r)
			return
		}
	}

	node := &store.Node{
		ID:           newUUID(),
		Name:         input.Name,
		APIURL:       strings.TrimRight(input.APIURL, "/"),
		APISecretEnc: secretEnc,
		OutboundEnc:  outboundEnc,
		ConfigURL:    configURL,
		KCEKeyEnc:    kceKeyEnc,
		Tags:         orEmpty(input.Tags),
		Enabled:      input.Enabled == nil || *input.Enabled,
		Remark:       input.Remark,
	}
	if err := s.store.CreateNode(node); err != nil {
		if isUniqueViolation(err) {
			httpx.Conflict(w, r, "a node with this name already exists")
			return
		}
		s.logger.Error("create node failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	// Auto-import: every proxy in the decrypted config becomes a node
	// anchored to this one (source "node:<id>"); the created node itself
	// takes the first proxy as its outbound when none was given manually.
	if configURL != "" {
		stats, importErr := s.importProxiesForNode(r.Context(), node, input.KCEKey, outboundEnc == "")
		if importErr != nil {
			httpx.WriteProblem(w, r, http.StatusBadGateway, httpx.ProbUpstream, importErr.Error())
			return
		}
		imported = stats
	}
	if err := s.hub.Reload(); err != nil {
		s.logger.Error("hub reload failed", slog.Any("error", err))
	}

	created := nodeImportResponse{nodeView: s.viewNode(node, false), Import: imported}
	raw, _ := json.Marshal(created)
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		idemKey := fmt.Sprintf("create-node:%d:%s", userID, key)
		s.idem.put(idemKey, idemEntry{
			created: time.Now(), status: http.StatusCreated,
			location: "/nodes/" + node.ID, body: raw,
			userID: userID,
		})
	}
	w.Header().Set("Location", "/nodes/"+node.ID)
	httpx.WriteRawJSON(w, http.StatusCreated, raw)
}

// nodeSource is the provenance marker for proxies auto-imported from one
// anchor node's encrypted config.
func nodeSource(anchorID string) string { return "node:" + anchorID }

// importProxiesForNode downloads the anchor node's config, decrypts it and
// syncs every parsed proxy as a node: the first proxy fills the anchor's own
// outbound when takeFirst is set, further proxies become anchored child nodes
// (replacing previously anchored ones, keeping manual nodes untouched).
func (s *Server) importProxiesForNode(ctx context.Context, anchor *store.Node, kceKey string, takeFirst bool) (*importStats, error) {
	// SSRF protection: validate config URL before fetching
	if err := httpx.ValidateOutboundURL(anchor.ConfigURL); err != nil {
		return nil, err
	}
	raw, err := subscribe.FetchConfigSource(ctx, s.upstreamClient(), anchor.ConfigURL, kceKey)
	if err != nil {
		if errors.Is(err, subscribe.ErrKeyRequired) {
			return nil, errors.New("payload is KCE1 encrypted; a decryption key is required")
		}
		return nil, errors.New("fetch failed: " + err.Error())
	}
	parsed, err := subscribe.ParseClashConfig(raw)
	if err != nil {
		return nil, err
	}
	stats := &importStats{Found: len(parsed.Nodes), Skipped: len(parsed.Skipped)}
	if len(parsed.Nodes) == 0 {
		return stats, nil
	}

	// Track created node IDs for rollback on failure
	var createdNodeIDs []string

	if takeFirst {
		enc, err := cryptox.Encrypt(s.cryptoKey, parsed.Nodes[0].Outbound)
		if err != nil {
			return nil, err
		}
		anchor.OutboundEnc = enc
		if err := s.store.UpdateNode(anchor); err != nil {
			return nil, err
		}
	}

	// Replace the previously anchored children in one pass.
	previous, err := s.store.ListNodesBySource(nodeSource(anchor.ID))
	if err != nil {
		return nil, err
	}
	prevByName := make(map[string]*store.Node, len(previous))
	for i := range previous {
		prevByName[previous[i].Name] = &previous[i]
	}
	seen := map[string]bool{}
	for i, proxy := range parsed.Nodes {
		if i == 0 && takeFirst {
			continue // already stored on the anchor itself
		}
		seen[proxy.Name] = true
		enc, err := cryptox.Encrypt(s.cryptoKey, proxy.Outbound)
		if err != nil {
			// Rollback created nodes on failure
			s.rollbackCreatedNodes(createdNodeIDs)
			return nil, err
		}
		if cur, ok := prevByName[proxy.Name]; ok {
			if curPlain, derr := cryptox.Decrypt(s.cryptoKey, cur.OutboundEnc); derr != nil || curPlain != proxy.Outbound {
				cur.OutboundEnc = enc
				if err := s.store.UpdateNode(cur); err != nil {
					// Rollback created nodes on failure
					s.rollbackCreatedNodes(createdNodeIDs)
					return nil, err
				}
				stats.Updated++
			}
			continue
		}
		child := &store.Node{
			ID:          newUUID(),
			Name:        s.uniqueNodeName(proxy.Name),
			OutboundEnc: enc,
			Source:      nodeSource(anchor.ID),
			Enabled:     anchor.Enabled,
			Tags:        append([]string{}, anchor.Tags...),
		}
		if err := s.store.CreateNode(child); err != nil {
			// Rollback created nodes on failure
			s.rollbackCreatedNodes(createdNodeIDs)
			return nil, err
		}
		createdNodeIDs = append(createdNodeIDs, child.ID)
		stats.Imported++
	}
	for name, cur := range prevByName {
		if seen[name] {
			continue
		}
		if err := s.store.DeleteNode(cur.ID); err != nil {
			// Rollback created nodes on failure
			s.rollbackCreatedNodes(createdNodeIDs)
			return nil, err
		}
		stats.Removed++
	}
	return stats, nil
}

// rollbackCreatedNodes deletes nodes that were created during a failed import.
func (s *Server) rollbackCreatedNodes(nodeIDs []string) {
	for _, id := range nodeIDs {
		if err := s.store.DeleteNode(id); err != nil {
			s.logger.Error("rollback created node failed", slog.String("node_id", id), slog.Any("error", err))
		}
	}
}

// handleGetNode implements GET /nodes/{node_id}.
func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeOr404(w, r)
	if !ok {
		return
	}
	withOutbound := r.URL.Query().Get("include") == "outbound"
	w.Header().Set("ETag", nodeETag(node.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, s.viewNode(node, withOutbound))
}

// handleUpdateNode implements PATCH /nodes/{node_id} (merge-patch).
func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeOr404(w, r)
	if !ok {
		return
	}
	etag := nodeETag(node.UpdatedAt)
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		httpx.WriteProblem(w, r, http.StatusPreconditionRequired,
			"https://sing-hub.dev/probs/precondition-required", "If-Match header is required")
		return
	}
	if ifMatch != etag {
		httpx.WriteProblem(w, r, http.StatusPreconditionFailed, httpx.ProbPreconditionFailed, "etag mismatch; refresh and retry")
		return
	}

	var patch map[string]json.RawMessage
	if !httpx.DecodeJSON(w, r, &patch) {
		return
	}
	// config_url/kce_key drive a decrypt-import instead of direct storage;
	// extract them before the generic merge-patch.
	importRequested := false
	var kceKey string
	if raw, ok := patch["config_url"]; ok {
		importRequested = true
		delete(patch, "config_url")
		if string(raw) == "null" {
			node.ConfigURL = ""
			node.KCEKeyEnc = ""
		} else if err := json.Unmarshal(raw, &node.ConfigURL); err != nil {
			httpx.Validation(w, r, "field config_url: "+err.Error())
			return
		} else {
			node.ConfigURL = strings.TrimRight(strings.TrimSpace(node.ConfigURL), "/")
			if node.ConfigURL != "" && !isHTTPURL(node.ConfigURL) {
				httpx.Validation(w, r, "field config_url must be an http(s) URL")
				return
			}
		}
	}
	if raw, ok := patch["kce_key"]; ok {
		importRequested = true
		delete(patch, "kce_key")
		if err := json.Unmarshal(raw, &kceKey); err != nil {
			httpx.Validation(w, r, "field kce_key: "+err.Error())
			return
		}
		enc, err := cryptox.Encrypt(s.cryptoKey, kceKey)
		if err != nil {
			httpx.Internal(w, r)
			return
		}
		node.KCEKeyEnc = enc
	}
	if err := applyNodePatch(node, patch, s.cryptoKey); err != nil {
		httpx.Validation(w, r, err.Error())
		return
	}
	_, manualOutbound := patch["outbound_json"]

	if err := s.store.UpdateNode(node); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.NotFound(w, r, "node not found")
			return
		}
		if isUniqueViolation(err) {
			httpx.Conflict(w, r, "a node with this name already exists")
			return
		}
		s.logger.Error("update node failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	// Re-import on config_url/kce_key edits: refreshes the anchor's outbound
	// (when not manually overridden) and the anchored child node set.
	var imported *importStats
	if importRequested && node.ConfigURL != "" {
		stats, importErr := s.importProxiesForNode(r.Context(), node, kceKey, !manualOutbound)
		if importErr != nil {
			httpx.WriteProblem(w, r, http.StatusBadGateway, httpx.ProbUpstream, importErr.Error())
			return
		}
		imported = stats
	}
	if err := s.hub.Reload(); err != nil {
		s.logger.Error("hub reload failed", slog.Any("error", err))
	}
	updated, _ := s.store.GetNode(node.ID)
	w.Header().Set("ETag", nodeETag(updated.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, nodeImportResponse{nodeView: s.viewNode(updated, false), Import: imported})
}

// isHTTPURL reports whether raw is an http(s) URL.
func isHTTPURL(raw string) bool {
	return strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
}

// handleDeleteNode implements DELETE /nodes/{node_id}. Deleting an anchor
// node also removes the child nodes auto-imported from its config.
func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeOr404(w, r)
	if !ok {
		return
	}
	children, err := s.store.ListNodesBySource(nodeSource(node.ID))
	if err == nil {
		for i := range children {
			if derr := s.store.DeleteNode(children[i].ID); derr != nil {
				s.logger.Error("delete imported child failed", slog.String("node", children[i].Name), slog.Any("error", derr))
			}
		}
	} else {
		s.logger.Error("list imported children failed", slog.Any("error", err))
	}
	if err := s.store.DeleteNode(node.ID); err != nil {
		s.logger.Error("delete node failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	if err := s.hub.Reload(); err != nil {
		s.logger.Error("hub reload failed", slog.Any("error", err))
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCheckNode implements POST /nodes/{node_id}/check.
func (s *Server) handleCheckNode(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeOr404(w, r)
	if !ok {
		return
	}
	result := s.checkNodeNow(r, node)
	httpx.WriteJSON(w, http.StatusOK, result)
}

func nodeETag(updatedAt int64) string {
	return `"node-v1-` + strconv.FormatInt(updatedAt, 10) + `"`
}
