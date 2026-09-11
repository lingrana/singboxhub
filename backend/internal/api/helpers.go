package api

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sing-hub/panel/internal/httpx"
	"github.com/sing-hub/panel/internal/store"
)

// ---- bearer token ----

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	return token, token != ""
}

// ---- cursors ----

// encodeCursor makes an opaque keyset cursor from components.
func encodeCursor(parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "\x1f")))
}

// decodeCursor splits an opaque cursor back into components.
func decodeCursor(cursor string) ([]string, bool) {
	if cursor == "" {
		return nil, true
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, false
	}
	return strings.Split(string(raw), "\x1f"), true
}

// ---- idempotency ----

type idemEntry struct {
	created  time.Time
	status   int
	location string
	body     []byte
	userID   int64  // bind to user
	opHash   string // operation hash for dedup
}

// idempotencyStore deduplicates POST /nodes by Idempotency-Key.
type idempotencyStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]idemEntry
}

func newIdempotencyStore(ttl time.Duration) *idempotencyStore {
	return &idempotencyStore{ttl: ttl, entries: map[string]idemEntry{}}
}

func (s *idempotencyStore) get(key string) (idemEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || time.Since(e.created) > s.ttl {
		return idemEntry{}, false
	}
	return e, true
}

func (s *idempotencyStore) put(key string, e idemEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// opportunistic GC
	for k, v := range s.entries {
		if time.Since(v.created) > s.ttl {
			delete(s.entries, k)
		}
	}
	s.entries[key] = e
}

// putIfAbsent atomically inserts only if key does not exist.
// Returns the existing entry if present, or the new entry if inserted.
func (s *idempotencyStore) putIfAbsent(key string, e idemEntry) (idemEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// opportunistic GC
	for k, v := range s.entries {
		if time.Since(v.created) > s.ttl {
			delete(s.entries, k)
		}
	}
	if existing, ok := s.entries[key]; ok {
		return existing, true
	}
	s.entries[key] = e
	return e, false
}

// ---- time range parsing ----

// parseTimeRange resolves from/to with interval-dependent defaults.
func parseTimeRange(r *http.Request, interval string) (from, to int64, ok bool) {
	now := time.Now().Unix()
	to = now
	switch interval {
	case "hour":
		from = now - 7*86400
	case "day":
		from = now - 30*86400
	default:
		from = now - 86400
	}
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return 0, 0, false
		}
		from = t.Unix()
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return 0, 0, false
		}
		to = t.Unix()
	}
	if from > to {
		return 0, 0, false
	}
	return from, to, true
}

// ---- shared lookups ----

// nodeOr404 loads a node by path id.
func (s *Server) nodeOr404(w http.ResponseWriter, r *http.Request) (*store.Node, bool) {
	id := r.PathValue("node_id")
	node, err := s.store.GetNode(id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.NotFound(w, r, "node not found")
		return nil, false
	}
	if err != nil {
		s.logger.Error("get node failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return nil, false
	}
	return node, true
}

// formatRFC3339 renders unix seconds as RFC 3339 UTC.
func formatRFC3339(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}

// parsePositiveInt parses a bounded integer query parameter.
func parsePositiveInt(raw string, def, max int) (int, bool) {
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}

// randomToken generates a URL-safe random token.
func randomToken(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
