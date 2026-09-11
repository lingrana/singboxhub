package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/sing-hub/panel/internal/cryptox"
	"github.com/sing-hub/panel/internal/httpx"
	"github.com/sing-hub/panel/internal/subscribe"
)

// latencyTimeout bounds one TCP dial to the outbound server.
const latencyTimeout = 5 * time.Second

// handleNodeLatency implements POST /nodes/{node_id}/latency: a TCP connect
// probe against the node's outbound server:port, giving an availability and
// round-trip signal that works for subscription-imported nodes without a
// Clash API.
func (s *Server) handleNodeLatency(w http.ResponseWriter, r *http.Request) {
	node, ok := s.nodeOr404(w, r)
	if !ok {
		return
	}
	if node.OutboundEnc == "" {
		httpx.Validation(w, r, "node has no outbound to probe")
		return
	}
	plain, err := cryptox.Decrypt(s.cryptoKey, node.OutboundEnc)
	if err != nil {
		s.logger.Error("decrypt outbound failed", slog.String("node", node.Name), slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	server, port, err := subscribe.ParseOutboundServer(plain)
	if err != nil {
		httpx.Validation(w, r, err.Error())
		return
	}

	// Validate server address for SSRF protection
	if err := httpx.ValidateOutboundURL("http://" + net.JoinHostPort(server, strconv.Itoa(port))); err != nil {
		httpx.Validation(w, r, "server address failed SSRF validation")
		return
	}

	result := map[string]any{"node_id": node.ID, "checked_at": formatRFC3339(time.Now().Unix())}
	start := time.Now()
	dialer := net.Dialer{Timeout: latencyTimeout}
	ctx, cancel := context.WithTimeout(r.Context(), latencyTimeout)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(server, strconv.Itoa(port)))
	latency := time.Since(start).Milliseconds()
	if err != nil {
		result["ok"] = false
		result["latency_ms"] = nil
		detail := err.Error()
		if len(detail) > 200 {
			detail = detail[:200]
		}
		result["detail"] = detail
	} else {
		_ = conn.Close()
		result["ok"] = true
		result["latency_ms"] = latency
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}
