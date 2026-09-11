// Package api implements the panel's HTTP API handlers and routing.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/sing-hub/panel/internal/auth"
	"github.com/sing-hub/panel/internal/config"
	"github.com/sing-hub/panel/internal/geo"
	"github.com/sing-hub/panel/internal/httpx"
	"github.com/sing-hub/panel/internal/probe"
	"github.com/sing-hub/panel/internal/sampler"
	"github.com/sing-hub/panel/internal/store"
	"github.com/sing-hub/panel/internal/webui"
)

// Server wires all handler dependencies.
type Server struct {
	cfg       config.Config
	logger    *slog.Logger
	store     *store.Store
	cryptoKey []byte
	auth      *auth.Manager
	limiter   *auth.Limiter
	hub       *sampler.Hub
	idem      *idempotencyStore
	geoSvc    *geo.Service
	geoOnce   sync.Once
	probeSem  chan struct{} // semaphore to limit concurrent probes
}

// NewServer builds the API server.
func NewServer(cfg config.Config, logger *slog.Logger, st *store.Store, cryptoKey []byte, authMgr *auth.Manager, hub *sampler.Hub) *Server {
	s := &Server{
		cfg:       cfg,
		logger:    logger,
		store:     st,
		cryptoKey: cryptoKey,
		auth:      authMgr,
		limiter:   auth.NewLimiter(10, 5*60*time.Second),
		hub:       hub,
		idem:      newIdempotencyStore(24 * time.Hour),
		probeSem:  make(chan struct{}, 5),
	}
	// Start limiter GC goroutine to prevent memory leak.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.limiter.GC()
		}
	}()
	return s
}

// Routes registers every endpoint of the OpenAPI contract plus the
// server-rendered UI (HTML surface under /admin/* and /login, which is not part
// of the API contract).
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Public
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /session", s.handleCreateSession)
	mux.HandleFunc("POST /session/refresh", s.handleRefreshSession)
	mux.HandleFunc("GET /sub/{token}", s.handleSubscriptionContent)

	// Public status (no auth required)
	mux.HandleFunc("GET /api/public/stats", s.handlePublicStats)
	mux.HandleFunc("GET /api/public/nodes", s.handlePublicNodes)

	// Authenticated
	mux.Handle("DELETE /session", s.requireAuth(http.HandlerFunc(s.handleDeleteSession)))
	mux.Handle("GET /me", s.requireAuth(http.HandlerFunc(s.handleGetMe)))

	mux.Handle("GET /nodes", s.requireAuth(http.HandlerFunc(s.handleListNodes)))
	mux.Handle("POST /nodes", s.requireAuth(http.HandlerFunc(s.handleCreateNode)))
	mux.Handle("GET /nodes/{node_id}", s.requireAuth(http.HandlerFunc(s.handleGetNode)))
	mux.Handle("PATCH /nodes/{node_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateNode)))
	mux.Handle("DELETE /nodes/{node_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteNode)))
	mux.Handle("POST /nodes/{node_id}/check", s.requireAuth(http.HandlerFunc(s.handleCheckNode)))

	mux.Handle("GET /nodes/{node_id}/status", s.requireAuth(http.HandlerFunc(s.handleNodeStatus)))
	mux.Handle("GET /nodes/{node_id}/traffic", s.requireAuth(http.HandlerFunc(s.handleNodeTraffic)))
	mux.Handle("GET /nodes/{node_id}/ips", s.requireAuth(http.HandlerFunc(s.handleNodeIps)))
	mux.Handle("GET /nodes/{node_id}/exit-ip", s.requireAuth(http.HandlerFunc(s.handleNodeExitIP)))
	mux.Handle("POST /nodes/{node_id}/probe", s.requireAuth(http.HandlerFunc(s.handleProbeNode)))

	mux.Handle("GET /nodes/{node_id}/connections", s.requireAuth(http.HandlerFunc(s.handleListConnections)))
	mux.Handle("DELETE /nodes/{node_id}/connections", s.requireAuth(http.HandlerFunc(s.handleCloseAllConnections)))
	mux.Handle("DELETE /nodes/{node_id}/connections/{connection_id}", s.requireAuth(http.HandlerFunc(s.handleCloseConnection)))
	mux.Handle("GET /nodes/{node_id}/proxies", s.requireAuth(http.HandlerFunc(s.handleListProxies)))
	mux.Handle("PUT /nodes/{node_id}/proxies/{proxy_name}", s.requireAuth(http.HandlerFunc(s.handleSelectProxy)))
	mux.Handle("GET /nodes/{node_id}/rules", s.requireAuth(http.HandlerFunc(s.handleListRules)))
	mux.Handle("PATCH /nodes/{node_id}/runtime", s.requireAuth(http.HandlerFunc(s.handlePatchRuntime)))

	mux.Handle("GET /nodes/{node_id}/share-link", s.requireAuth(http.HandlerFunc(s.handleNodeShareLink)))

	mux.Handle("GET /operations/{operation_id}", s.requireAuth(http.HandlerFunc(s.handleGetOperation)))

	mux.Handle("GET /subscription", s.requireAuth(http.HandlerFunc(s.handleGetSubscription)))
	mux.Handle("POST /subscription/token", s.requireAuth(http.HandlerFunc(s.handleResetSubscriptionToken)))
	mux.Handle("DELETE /subscription", s.requireAuth(http.HandlerFunc(s.handleDeleteSubscription)))

	mux.Handle("POST /nodes/{node_id}/latency", s.requireAuth(http.HandlerFunc(s.handleNodeLatency)))

	mux.Handle("GET /stats/overview", s.requireAuth(http.HandlerFunc(s.handleStatsOverview)))
	mux.Handle("GET /stats/nodes", s.requireAuth(http.HandlerFunc(s.handleStatsNodes)))
	mux.Handle("GET /stats/traffic-series", s.requireAuth(http.HandlerFunc(s.handleStatsTrafficSeries)))
	mux.Handle("GET /stats/top-nodes", s.requireAuth(http.HandlerFunc(s.handleStatsTopNodes)))
	mux.Handle("GET /stats/top-ips", s.requireAuth(http.HandlerFunc(s.handleStatsTopIps)))

	mux.Handle("GET /settings", s.requireAuth(http.HandlerFunc(s.handleGetSettings)))
	mux.Handle("PATCH /settings", s.requireAuth(http.HandlerFunc(s.handleUpdateSettings)))
	mux.Handle("POST /settings/favicon", s.requireAuth(http.HandlerFunc(s.handleUploadFavicon)))
	mux.Handle("POST /settings/logo", s.requireAuth(http.HandlerFunc(s.handleUploadLogo)))
	mux.HandleFunc("GET /settings/favicon", s.handleServeFavicon)
	mux.HandleFunc("GET /settings/logo", s.handleServeLogo)
	mux.HandleFunc("GET /brand", s.handleGetBrand)

	// User management (admin only)
	mux.Handle("GET /users", s.requireAuth(http.HandlerFunc(s.handleListUsers)))
	mux.Handle("POST /users", s.requireAuth(http.HandlerFunc(s.handleCreateUser)))
	mux.Handle("GET /users/{user_id}", s.requireAuth(http.HandlerFunc(s.handleGetUser)))
	mux.Handle("DELETE /users/{user_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteUser)))
	mux.Handle("PATCH /users/{user_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateUser)))

	// Server-rendered UI: cookie-session pages backed by this JSON API.
	ui, err := webui.New(s.auth, s.store, s.logger, s.cfg.Session.AccessTokenTTL, s.cfg.Session.RefreshTokenTTL)
	if err != nil {
		// Template parsing is embedded and compile-time validated; failure is
		// a programming error, not a runtime condition.
		panic("webui init: " + err.Error())
	}
	ui.Register(mux)

	handler := httpx.Middleware(s.logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(&apiErrorWriter{ResponseWriter: w, request: r, onlyUnmatched: true}, r)
	}))
	ui.SetAPIHandler(handler)
	return handler
}

// requireAuth guards handlers with the bearer token.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		ctx := context.WithValue(r.Context(), ctxUserIDKey, claims.UserID())
		ctx = context.WithValue(ctx, ctxClaimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type ctxKey int

const (
	ctxUserIDKey ctxKey = iota
	ctxClaimsKey
)

// ctxUserID extracts the user ID from the request context.
func ctxUserID(r *http.Request) (int64, bool) {
	id, ok := r.Context().Value(ctxUserIDKey).(int64)
	return id, ok
}

// ctxClaims extracts the claims from the request context.
func ctxClaims(r *http.Request) (*auth.Claims, bool) {
	c, ok := r.Context().Value(ctxClaimsKey).(*auth.Claims)
	return c, ok
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// probeRunner builds a Runner from current settings.
func (s *Server) probeRunner(settings *store.Settings) *probe.Runner {
	binPath := settings.SingboxBinPath
	if binPath == "" {
		binPath = s.cfg.Probe.SingboxBinPath
	}
	provider := settings.IPLookupProviderURL
	if provider == "" {
		provider = s.cfg.Probe.IPProviderURL
	}
	timeout := time.Duration(s.cfg.Probe.ProbeTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &probe.Runner{BinPath: binPath, ProviderURL: provider, Timeout: timeout}
}
