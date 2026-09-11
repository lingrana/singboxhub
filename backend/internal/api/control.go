package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/sing-hub/panel/internal/clash"
	"github.com/sing-hub/panel/internal/cryptox"
	"github.com/sing-hub/panel/internal/httpx"
)

// resolveClient loads the node from the path and builds its Clash client,
// writing the appropriate problem response when it cannot. ok=false means the
// response has already been written.
func (s *Server) resolveClient(w http.ResponseWriter, r *http.Request) (*clash.Client, bool) {
	node, ok := s.nodeOr404(w, r)
	if !ok {
		return nil, false
	}
	secret, err := cryptox.Decrypt(s.cryptoKey, node.APISecretEnc)
	if err != nil {
		s.logger.Error("decrypt secret failed", "node", node.Name, "error", err)
		httpx.Internal(w, r)
		return nil, false
	}
	client, err := clash.New(node.APIURL, secret)
	if err != nil {
		httpx.NodeUnreachable(w, r, err.Error())
		return nil, false
	}
	return client, true
}

func ctx6s(r *http.Request) (context.Context, func()) {
	return context.WithTimeout(r.Context(), 6*time.Second)
}

// mapNodeErr converts node-side errors into 502 problem responses.
func mapNodeErr(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		return
	}
	httpx.NodeUnreachable(w, r, nodeErrDetail(err))
}

// handleListConnections implements GET /nodes/{node_id}/connections.
func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	client, ok := s.resolveClient(w, r)
	if !ok {
		return
	}
	ctx, cancel := ctx6s(r)
	defer cancel()
	msg, err := client.ConnectionsSnapshot(ctx)
	if err != nil {
		mapNodeErr(w, r, err)
		return
	}
	portMap, _ := client.FetchClientPortMap(ctx)
	items := make([]map[string]any, 0, len(msg.Connections))
	for _, c := range msg.Connections {
		item := connectionItem(c)
		if realIP, ok := portMap[c.Metadata.SourcePort]; ok {
			item["metadata"].(map[string]any)["source_ip"] = realIP
		}
		items = append(items, item)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func connectionItem(c clash.Connection) map[string]any {
	outbound := ""
	if len(c.Chains) > 0 {
		outbound = c.Chains[0]
	}
	return map[string]any{
		"id":             c.ID,
		"upload_bytes":   c.Upload,
		"download_bytes": c.Download,
		"start":          c.Start,
		"chains":         orEmpty(c.Chains),
		"rule":           c.Rule,
		"rule_payload":   c.RulePay,
		"outbound":       outbound,
		"metadata": map[string]any{
			"network":          c.Metadata.Network,
			"type":             c.Metadata.Type,
			"source_ip":        c.Metadata.SourceIP,
			"source_port":      c.Metadata.SourcePort,
			"destination_ip":   c.Metadata.DestinationIP,
			"destination_port": c.Metadata.DestinationPort,
			"host":             c.Metadata.Host,
			"inbound_type":     c.Metadata.InboundType,
		},
	}
}

// handleCloseAllConnections implements DELETE /nodes/{node_id}/connections.
func (s *Server) handleCloseAllConnections(w http.ResponseWriter, r *http.Request) {
	client, ok := s.resolveClient(w, r)
	if !ok {
		return
	}
	ctx, cancel := ctx6s(r)
	defer cancel()
	if err := client.CloseAllConnections(ctx); err != nil {
		mapNodeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCloseConnection implements DELETE /nodes/{node_id}/connections/{cid}.
func (s *Server) handleCloseConnection(w http.ResponseWriter, r *http.Request) {
	client, ok := s.resolveClient(w, r)
	if !ok {
		return
	}
	ctx, cancel := ctx6s(r)
	defer cancel()
	if err := client.CloseConnection(ctx, r.PathValue("connection_id")); err != nil {
		var ce *clash.Error
		if errors.As(err, &ce) && ce.StatusCode == http.StatusNotFound {
			httpx.NotFound(w, r, "connection not found on node")
			return
		}
		mapNodeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListProxies implements GET /nodes/{node_id}/proxies.
func (s *Server) handleListProxies(w http.ResponseWriter, r *http.Request) {
	client, ok := s.resolveClient(w, r)
	if !ok {
		return
	}
	ctx, cancel := ctx6s(r)
	defer cancel()
	proxies, err := client.Proxies(ctx)
	if err != nil {
		mapNodeErr(w, r, err)
		return
	}
	items := []map[string]any{}
	for _, p := range proxies {
		items = append(items, map[string]any{
			"name": p.Name,
			"type": p.Type,
			"now":  p.Now,
			"all":  p.All,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleSelectProxy implements PUT /nodes/{node_id}/proxies/{proxy_name}.
func (s *Server) handleSelectProxy(w http.ResponseWriter, r *http.Request) {
	client, ok := s.resolveClient(w, r)
	if !ok {
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if !httpx.DecodeJSON(w, r, &input) {
		return
	}
	if input.Name == "" {
		httpx.Validation(w, r, "name is required", httpx.FieldErr("#/name", "must not be empty"))
		return
	}
	ctx, cancel := ctx6s(r)
	defer cancel()
	if err := client.SelectProxy(ctx, r.PathValue("proxy_name"), input.Name); err != nil {
		mapNodeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListRules implements GET /nodes/{node_id}/rules.
func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	client, ok := s.resolveClient(w, r)
	if !ok {
		return
	}
	ctx, cancel := ctx6s(r)
	defer cancel()
	rules, err := client.Rules(ctx)
	if err != nil {
		mapNodeErr(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(rules))
	for _, ru := range rules {
		items = append(items, map[string]any{"type": ru.Type, "payload": ru.Payload, "proxy": ru.Proxy})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handlePatchRuntime implements PATCH /nodes/{node_id}/runtime.
func (s *Server) handlePatchRuntime(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("node_id")
	client, ok := s.resolveClient(w, r)
	if !ok {
		return
	}
	var input struct {
		Mode string `json:"mode"`
	}
	if !httpx.DecodeJSON(w, r, &input) {
		return
	}
	switch input.Mode {
	case "rule", "global", "direct":
	default:
		httpx.Validation(w, r, "mode must be rule|global|direct",
			httpx.FieldErr("#/mode", "one of rule, global, direct"))
		return
	}
	ctx, cancel := ctx6s(r)
	defer cancel()
	if err := client.PatchMode(ctx, input.Mode); err != nil {
		mapNodeErr(w, r, err)
		return
	}
	s.hub.SetNodeMode(nodeID, input.Mode)
	status, known := s.hub.Status(nodeID)
	if !known {
		status.Mode = input.Mode
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"node_id":            nodeID,
		"online":             status.Online,
		"mode":               status.Mode,
		"up_bps":             status.UpBPS,
		"down_bps":           status.DownBPS,
		"active_connections": status.ActiveConnections,
	})
}
