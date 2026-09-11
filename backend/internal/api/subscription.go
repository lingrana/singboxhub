package api

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/sing-hub/panel/internal/cryptox"
	"github.com/sing-hub/panel/internal/httpx"
	"github.com/sing-hub/panel/internal/subscribe"
)

// exportableNodes loads enabled nodes with outbound configs for subscription.
func (s *Server) exportableNodes() ([]subscribe.Node, error) {
	nodes, err := s.store.ListNodes()
	if err != nil {
		return nil, err
	}
	out := make([]subscribe.Node, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if !n.Enabled || n.OutboundEnc == "" {
			continue
		}
		plain, err := cryptox.Decrypt(s.cryptoKey, n.OutboundEnc)
		if err != nil {
			s.logger.Error("decrypt outbound failed", slog.String("node", n.Name), slog.Any("error", err))
			continue
		}
		// Rename the outbound tag to the panel node name for consistent display.
		plain = renameOutboundTag(plain, n.Name)
		out = append(out, subscribe.Node{Name: n.Name, Outbound: plain})
	}
	return out, nil
}

// renameOutboundTag replaces the outbound "tag" with the panel node name.
func renameOutboundTag(outbound, name string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(outbound), &m); err != nil {
		return outbound
	}
	if _, has := m["tag"]; has {
		m["tag"] = name
		if raw, err := json.Marshal(m); err == nil {
			return string(raw)
		}
	}
	return outbound
}

// handleGetSubscription implements GET /subscription.
func (s *Server) handleGetSubscription(w http.ResponseWriter, r *http.Request) {
	userID, ok := ctxUserID(r)
	if !ok {
		httpx.Unauthorized(w, r, "unauthorized")
		return
	}
	sub, err := s.store.GetSubscription(userID)
	if err != nil {
		s.logger.Error("load subscription failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	body := map[string]any{
		"enabled":    sub.Token != "",
		"token":      nil,
		"url":        nil,
		"formats":    []string{"singbox", "clash", "base64"},
		"updated_at": formatRFC3339(sub.UpdatedAt),
	}
	if sub.Token != "" {
		body["token"] = sub.Token
		body["url"] = subURL(r, sub.Token)
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

// handleResetSubscriptionToken implements POST /subscription/token.
func (s *Server) handleResetSubscriptionToken(w http.ResponseWriter, r *http.Request) {
	userID, ok := ctxUserID(r)
	if !ok {
		httpx.Unauthorized(w, r, "unauthorized")
		return
	}
	token, err := randomToken(24)
	if err != nil {
		s.logger.Error("generate token failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	if err := s.store.SetSubscriptionToken(userID, token); err != nil {
		s.logger.Error("save subscription token failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	sub, err := s.store.GetSubscription(userID)
	if err != nil {
		httpx.Internal(w, r)
		return
	}
	w.Header().Set("Location", "/subscription")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"enabled":    true,
		"token":      sub.Token,
		"url":        subURL(r, sub.Token),
		"formats":    []string{"singbox", "clash", "base64"},
		"updated_at": formatRFC3339(sub.UpdatedAt),
	})
}

// handleDeleteSubscription implements DELETE /subscription (revoke).
func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	userID, ok := ctxUserID(r)
	if !ok {
		httpx.Unauthorized(w, r, "unauthorized")
		return
	}
	if err := s.store.SetSubscriptionToken(userID, ""); err != nil {
		s.logger.Error("revoke subscription failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSubscriptionContent implements GET /sub/{token} (public capability URL).
func (s *Server) handleSubscriptionContent(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")

	// Rate limit by IP for all subscription requests (valid or invalid)
	limitKey := "sub:" + clientIP(r)
	if !s.limiter.Allow(limitKey) {
		w.Header().Set("Retry-After", "60")
		httpx.WriteProblem(w, r, http.StatusTooManyRequests, httpx.ProbRateLimited, "subscription rate limited")
		return
	}

	// Look up which user owns this token
	userID, err := s.store.FindSubscriptionUser(token)
	if err != nil || userID == 0 {
		httpx.NotFound(w, r, "subscription not found")
		return
	}
	sub, err := s.store.GetSubscription(userID)
	if err != nil || sub.Token == "" || !constantTimeEqual(token, sub.Token) {
		httpx.NotFound(w, r, "subscription not found")
		return
	}

	format := r.URL.Query().Get("format")
	if format == "" {
		format = "singbox"
	}
	nodes, err := s.exportableNodes()
	if err != nil {
		httpx.Internal(w, r)
		return
	}

	s.logger.Info("subscription fetched",
		slog.String("format", format), slog.Int("nodes", len(nodes)), slog.String("remote", clientIP(r)))

	var (
		body        []byte
		contentType string
	)
	switch format {
	case "singbox":
		contentType = "application/json; charset=utf-8"
		body, err = subscribe.GenerateSingbox(nodes)
	case "clash":
		contentType = "text/yaml; charset=utf-8"
		body, err = subscribe.GenerateClash(nodes)
	case "base64":
		contentType = "text/plain; charset=utf-8"
		body, err = subscribe.GenerateBase64(nodes)
	default:
		httpx.Validation(w, r, "format must be singbox|clash|base64",
			httpx.FieldErr("#format", "one of singbox, clash, base64"))
		return
	}
	if err != nil {
		s.logger.Error("generate subscription failed", slog.Any("error", err))
		httpx.WriteProblem(w, r, http.StatusServiceUnavailable, "https://sing-hub.dev/probs/subscription-empty",
			"no exportable nodes are configured")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Profile-Update-Interval", "6")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func constantTimeEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func subURL(r *http.Request, token string) string {
	// Use configured external URL if available, otherwise derive from request
	// with strict validation of proxy headers
	scheme := "http"

	// Only trust X-Forwarded-Proto when behind a known proxy
	if r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}

	// Use the Host header but sanitize it
	host := r.Host
	if host == "" {
		host = "localhost:9090"
	}

	// Validate host to prevent injection
	if strings.ContainsAny(host, "\n\r\t") {
		host = "localhost:9090"
	}

	return scheme + "://" + host + "/sub/" + token
}

func clientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}
