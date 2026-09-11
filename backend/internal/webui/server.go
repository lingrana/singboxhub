package webui

import (
	"bytes"
	"html/template"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/sing-hub/panel/internal/auth"
	"github.com/sing-hub/panel/internal/store"
)

// UI serves the server-rendered panel on top of the JSON API.
type UI struct {
	rd           *Renderer
	auth         *auth.Manager
	store        *store.Store
	logger       *slog.Logger
	accessTTL    time.Duration
	refreshTTL   time.Duration
	loginLimiter *windowLimiter

	rotMu   sync.Mutex
	rotated map[string]rotEntry
}

// New builds the UI handler set.
func New(authMgr *auth.Manager, st *store.Store, logger *slog.Logger, accessTTL, refreshTTL time.Duration) (*UI, error) {
	rd, err := NewRenderer()
	if err != nil {
		return nil, err
	}
	return &UI{
		rd:           rd,
		auth:         authMgr,
		store:        st,
		logger:       logger,
		accessTTL:    accessTTL,
		refreshTTL:   refreshTTL,
		loginLimiter: newLimiter(10, 5*time.Minute),
		rotated:      map[string]rotEntry{},
	}, nil
}

// SetAPIHandler wires the in-process API client target. Must be called with
// the panel's full handler before the server starts serving.
func (u *UI) SetAPIHandler(h http.Handler) { apiHandler.Store(h) }

// Register mounts every UI route. The mux is the panel's shared ServeMux;
// admin paths live under /admin/* plus /login and /logout.
func (u *UI) Register(mux *http.ServeMux) {
	static, err := Static()
	if err == nil {
		mux.Handle("GET /admin/assets/{path...}", cacheStatic(http.StripPrefix("/admin/assets/", http.FileServer(http.FS(static)))))
	} else {
		u.logger.Error("static assets unavailable", "error", err)
	}

	// Use the portable root pattern here. Older Go runtimes treat the newer
	// "/{$}" pattern as a literal path, which leaves the public home page at
	// 404 even though the API is healthy.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		u.pageStatus(w, r)
	})

	mux.HandleFunc("GET /status", u.pageStatus)
	mux.HandleFunc("GET /status/overview", u.fragStatusOverview)
	mux.HandleFunc("GET /status/nodes", u.fragStatusNodes)

	mux.HandleFunc("GET /login", u.handleLoginPage)
	mux.HandleFunc("POST /login", u.handleLoginSubmit)
	mux.Handle("POST /logout", u.uiAuth(u.handleLogout))

	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/overview", http.StatusSeeOther)
	})
	mux.Handle("GET /admin/overview", u.uiAuth(u.pageOverview))
	mux.Handle("GET /admin/overview/stats", u.uiAuth(u.fragOverviewStats))
	mux.Handle("GET /admin/overview/chart", u.uiAuth(u.fragOverviewChart))
	mux.Handle("GET /admin/overview/tops", u.uiAuth(u.fragOverviewTops))
	mux.Handle("GET /admin/overview/nodes", u.uiAuth(u.fragOverviewNodes))

	mux.Handle("GET /admin/nodes", u.uiAuth(u.pageNodes))
	mux.Handle("GET /admin/nodes/list", u.uiAuth(u.fragNodeList))
	mux.Handle("GET /admin/nodes/manage", u.uiAuth(u.fragNodeManage))
	mux.Handle("GET /admin/nodes/new", u.uiAuth(u.fragNodeFormNew))
	mux.Handle("POST /admin/nodes/new", u.uiAuth(u.handleNodeCreate))
	mux.Handle("GET /admin/nodes/{node_id}/edit", u.uiAuth(u.fragNodeFormEdit))
	mux.Handle("POST /admin/nodes/{node_id}/edit", u.uiAuth(u.handleNodeUpdate))
	mux.Handle("POST /admin/nodes/{node_id}/delete", u.uiAuth(u.handleNodeDelete))
	mux.Handle("POST /admin/nodes/{node_id}/toggle", u.uiAuth(u.handleNodeToggle))
	mux.Handle("POST /admin/nodes/{node_id}/check", u.uiAuth(u.handleNodeCheck))

	mux.Handle("GET /admin/traffic", u.uiAuth(u.pageTraffic))
	mux.Handle("GET /admin/traffic/table", u.uiAuth(u.fragTrafficTable))
	mux.Handle("GET /admin/ips", u.uiAuth(u.pageIps))
	mux.Handle("GET /admin/ips/table", u.uiAuth(u.fragIpsTable))

	mux.Handle("GET /admin/subscription", u.uiAuth(u.pageSubscription))
	mux.Handle("GET /admin/subscription/card", u.uiAuth(u.fragSubscriptionCard))
	mux.Handle("POST /admin/subscription/parse", u.uiAuth(u.handleSubParse))
	mux.Handle("POST /admin/subscription/token", u.uiAuth(u.handleSubscriptionReset))
	mux.Handle("GET /admin/subscription/qrcode", u.uiAuth(u.handleSubscriptionQR))
	mux.Handle("POST /admin/subscription/revoke", u.uiAuth(u.handleSubscriptionRevoke))
	mux.Handle("GET /admin/subscription/nodes", u.uiAuth(u.fragSubNodes))
	mux.Handle("POST /admin/nodes/{node_id}/latency", u.uiAuth(u.handleSubNodeLatency))

	mux.Handle("GET /admin/settings", u.uiAuth(u.pageSettings))
	mux.Handle("POST /admin/settings", u.uiAuth(u.handleSettingsSave))

	mux.Handle("GET /admin/users", u.uiAuth(u.pageUsers))
	mux.Handle("GET /admin/users/list", u.uiAuth(u.fragUserList))
	mux.Handle("GET /admin/users/new", u.uiAuth(u.fragUserFormNew))
	mux.Handle("POST /admin/users/new", u.uiAuth(u.handleUserCreate))
	mux.Handle("GET /admin/users/{user_id}/password", u.uiAuth(u.fragUserFormPassword))
	mux.Handle("POST /admin/users/{user_id}/delete", u.uiAuth(u.handleUserDelete))
	mux.Handle("POST /admin/users/{user_id}/password", u.uiAuth(u.handleUserPassword))
	mux.Handle("POST /admin/users/{user_id}/reset", u.uiAuth(u.handleUserReset))

	mux.HandleFunc("POST /admin/theme", u.handleThemeToggle)

	mux.Handle("POST /admin/settings/favicon", u.uiAuth(u.handleFaviconUpload))
	mux.Handle("POST /admin/settings/logo", u.uiAuth(u.handleLogoUpload))
	mux.Handle("POST /admin/branding", u.uiAuth(u.handleBrandingSave))

	// User profile modal (for status page)
	mux.Handle("GET /user/profile", u.uiAuth(u.fragUserProfile))
	mux.Handle("GET /user/profile/edit", u.uiAuth(u.fragUserProfileEdit))
	mux.Handle("POST /user/profile/edit", u.uiAuth(u.handleUserProfileEdit))
	mux.Handle("GET /user/profile/qr", u.uiAuth(u.fragUserProfileQR))
	mux.Handle("GET /user/profile/qr/image", u.uiAuth(u.handleUserProfileQRImage))
	mux.Handle("GET /user/profile/reset-confirm", u.uiAuth(u.fragUserProfileResetConfirm))
	mux.Handle("POST /user/subscription/token", u.uiAuth(u.handleUserProfileSubscriptionToken))
	mux.Handle("POST /user/subscription/revoke", u.uiAuth(u.handleUserProfileSubscriptionRevoke))
}

// cacheStatic adds conservative caching for embedded assets.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// requireForm validates the CSRF origin check for state-changing handlers.
// Returns false when the response was already written.
func (u *UI) requireForm(w http.ResponseWriter, r *http.Request) bool {
	if !u.sameOriginOk(r) {
		http.Error(w, "cross-origin form submission rejected", http.StatusForbidden)
		return false
	}
	return true
}

// ---- two-pass rendering: page content -> body frame -> layout ----

// fragErr is the generic error card payload.
type fragErr struct {
	Message string
}

// frameData is the header+main+toast wrapper payload.
type frameData struct {
	Base
	BodyHTML template.HTML
}

// layoutData is the full-document payload.
type layoutData struct {
	Base
	BodyFrame template.HTML
}

// securityHeaders apply to every UI response.
func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
}

// renderPage renders a full page. For htmx page navigations the response is
// the body frame (nav included) so active states stay correct; plain browser
// requests get the full document.
func (u *UI) renderPage(w http.ResponseWriter, r *http.Request, page, title string, body any) {
	securityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	var content bytes.Buffer
	if err := u.rd.render(&content, "page_"+page, body); err != nil {
		u.logger.Error("render page content", "page", page, "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	// Extract the nav-active page name from the body's Base field if available,
	// so the sidebar OOB swap highlights the correct nav item (e.g. detail
	// pages inherit their parent section's page name).
	navPage := page
	if body != nil {
		v := reflect.ValueOf(body)
		if v.Kind() == reflect.Ptr {
			v = v.Elem()
		}
		if v.Kind() == reflect.Struct {
			if f := v.FieldByName("Base"); f.IsValid() {
				if pf := f.FieldByName("Page"); pf.IsValid() && pf.Kind() == reflect.String {
					if s := pf.String(); s != "" {
						navPage = s
					}
				}
			}
		}
	}
	frame := frameData{
		Base:     u.rd.base(navPage, title, u.usernameOf(r), u.themeOf(r)),
		BodyHTML: template.HTML(content.String()),
	}
	frame.Path = r.URL.Path
	u.loadBranding(r, &frame.Base)

	if isHX(r) {
		_, _ = w.Write(content.Bytes())
		// OOB swap: append the updated sidebar nav so active state refreshes.
		var navBuf bytes.Buffer
		if err := u.rd.render(&navBuf, "sidebar_nav_oob", frame); err == nil {
			_, _ = w.Write(navBuf.Bytes())
		}
		return
	}
	var frameBuf bytes.Buffer
	if err := u.rd.render(&frameBuf, "body_frame", frame); err != nil {
		u.logger.Error("render body frame", "page", page, "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	doc := layoutData{
		Base:      frame.Base,
		BodyFrame: template.HTML(frameBuf.String()),
	}
	var out bytes.Buffer
	if err := u.rd.render(&out, "layout", doc); err != nil {
		u.logger.Error("render layout", "page", page, "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(out.Bytes())
}

// renderFrag renders an htmx fragment.
func (u *UI) renderFrag(w http.ResponseWriter, frag string, data any) {
	securityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := u.rd.render(w, frag, data); err != nil {
		u.logger.Error("render fragment", "fragment", frag, "error", err)
	}
}

// usernameOf resolves the display name from the session claims.
func (u *UI) usernameOf(r *http.Request) string {
	token, ok := ctxToken(r.Context())
	if !ok {
		return ""
	}
	claims, err := u.verifyAccess(token)
	if err != nil {
		return ""
	}
	return claims.Username
}
