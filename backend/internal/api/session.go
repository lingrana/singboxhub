package api

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/sing-hub/panel/internal/auth"
	"github.com/sing-hub/panel/internal/httpx"
	"github.com/sing-hub/panel/internal/store"
)

// handleCreateSession implements POST /session (login).
// Failures are indistinguishable between "unknown user" and "wrong password"
// to prevent user enumeration (contract 2.10-8).
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	key := "login:" + ip
	if !s.limiter.Allow(key) {
		retry := int(s.limiter.RetryAfter(key).Seconds())
		if retry < 1 {
			retry = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		httpx.WriteProblem(w, r, http.StatusTooManyRequests, httpx.ProbRateLimited, "too many login attempts, retry later")
		return
	}

	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &input) {
		return
	}
	if input.Username == "" || input.Password == "" {
		httpx.Validation(w, r, "username and password are required",
			httpx.FieldErr("#/username", "must not be empty"),
			httpx.FieldErr("#/password", "must not be empty"))
		return
	}

	user, err := s.store.GetUserByUsername(input.Username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("lookup user failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	valid := false
	if user != nil {
		valid = auth.ComparePassword(user.PasswordHash, input.Password)
	} else {
		// Burn a bcrypt round for unknown users to even out timing.
		auth.ComparePassword("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5B0X0FkF0mQ5oZ4S1lGqRzCkU3SNe", "dummy-password")
	}
	if !valid {
		httpx.Unauthorized(w, r, "invalid credentials")
		return
	}

	pair, err := s.auth.Issue(user.ID, user.Username, user.Role)
	if err != nil {
		s.logger.Error("issue session failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	w.Header().Set("Location", "/me")
	httpx.WriteJSON(w, http.StatusCreated, sessionBody(pair))
}

// handleRefreshSession implements POST /session/refresh.
func (s *Server) handleRefreshSession(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	key := "refresh:" + ip
	if !s.limiter.Allow(key) {
		retry := int(s.limiter.RetryAfter(key).Seconds())
		if retry < 1 {
			retry = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		httpx.WriteProblem(w, r, http.StatusTooManyRequests, httpx.ProbRateLimited, "too many refresh attempts, retry later")
		return
	}

	var input struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !httpx.DecodeJSON(w, r, &input) {
		return
	}
	pair, err := s.auth.Rotate(input.RefreshToken)
	if errors.Is(err, auth.ErrRevoked) {
		httpx.Unauthorized(w, r, "refresh token has been revoked")
		return
	}
	if err != nil {
		httpx.Unauthorized(w, r, "invalid refresh token")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sessionBody(pair))
}

// handleDeleteSession implements DELETE /session: revokes every refresh
// token of the authenticated user (single-admin panel).
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok {
		httpx.Unauthorized(w, r, "missing bearer token")
		return
	}
	claims, err := s.auth.VerifyAccess(token)
	if err != nil {
		httpx.Unauthorized(w, r, "invalid or expired token")
		return
	}
	if err := s.store.RevokeAllRefreshTokens(claims.UserID()); err != nil {
		s.logger.Error("revoke refresh tokens failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGetMe implements GET /me.
func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	token, _ := bearerToken(r)
	claims, err := s.auth.VerifyAccess(token)
	if err != nil {
		httpx.Unauthorized(w, r, "invalid or expired token")
		return
	}
	user, err := s.store.GetUserByUsername(claims.Username)
	createdAt := ""
	if err == nil {
		createdAt = formatRFC3339(user.CreatedAt)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"username":   claims.Username,
		"created_at": createdAt,
	})
}

func sessionBody(pair *auth.Pair) map[string]any {
	return map[string]any{
		"access_token":       pair.AccessToken,
		"token_type":         "Bearer",
		"expires_in_seconds": int(time.Until(pair.AccessExpiresAt).Seconds()),
		"refresh_token":      pair.RefreshToken,
	}
}
