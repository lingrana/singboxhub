package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sing-hub/panel/internal/auth"
)

// Cookie names and session parameters.
const (
	accessCookieName  = "singhub_at"
	refreshCookieName = "singhub_rt"
	themeCookieName   = "singhub_theme"
	cookiePath        = "/"
)

// newLimiter mirrors the sliding-window policy of auth.Limiter without
// importing it (that one is constructed by api.Server for bearer logins).
func newLimiter(capacity int, window time.Duration) *windowLimiter {
	return &windowLimiter{capacity: capacity, window: window, hits: map[string][]time.Time{}}
}

type windowLimiter struct {
	mu       sync.Mutex
	capacity int
	window   time.Duration
	hits     map[string][]time.Time
}

func (l *windowLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.capacity {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	return true
}

// Session bundles the bearer token pair into cookies for browser clients.
type Session struct {
	AccessToken  string
	RefreshToken string
}

// setSessionCookies writes HttpOnly session cookies.
func setSessionCookies(w http.ResponseWriter, r *http.Request, pair *auth.Pair) {
	isSecure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name: accessCookieName, Value: pair.AccessToken, Path: cookiePath,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure,
		Expires: pair.AccessExpiresAt,
	})
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookieName, Value: pair.RefreshToken, Path: cookiePath,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure,
		Expires: pair.RefreshExpiresAt,
	})
}

// clearSessionCookies expires both session cookies.
func clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{accessCookieName, refreshCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: cookiePath,
			HttpOnly: true, SameSite: http.SameSiteLaxMode,
			MaxAge: -1,
		})
	}
}

// sessionFrom reads the current session; ok=false when no cookies present.
func sessionFrom(r *http.Request) (Session, bool) {
	at, errAt := r.Cookie(accessCookieName)
	rt, errRt := r.Cookie(refreshCookieName)
	if (errAt != nil || at.Value == "") && (errRt != nil || rt.Value == "") {
		return Session{}, false
	}
	s := Session{}
	if errAt == nil {
		s.AccessToken = at.Value
	}
	if errRt == nil {
		s.RefreshToken = rt.Value
	}
	return s, true
}

// verifyAccess validates the access token with the panel's auth manager.
func (u *UI) verifyAccess(token string) (*auth.Claims, error) {
	return u.auth.VerifyAccess(token)
}

// rotate consumes the refresh cookie and issues a fresh pair. Concurrent
// tabs rotating the same refresh token simultaneously are reconciled through
// a short-lived in-process cache: the losing request reuses the winner's pair
// instead of bouncing the user to /login.
func (u *UI) rotate(refreshToken string) (*auth.Pair, error) {
	claims, err := u.auth.VerifyRefresh(refreshToken)
	if err != nil {
		return nil, err
	}
	pair, err := u.auth.Rotate(refreshToken)
	if errors.Is(err, auth.ErrRevoked) {
		if cached, ok := u.recentPair(claims.ID); ok {
			if _, verifyErr := u.auth.VerifyAccess(cached.AccessToken); verifyErr == nil {
				return cached, nil
			}
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	u.rememberPair(claims.ID, pair)
	return pair, nil
}

const rotationCacheTTL = 15 * time.Second

func (u *UI) recentPair(jti string) (*auth.Pair, bool) {
	u.rotMu.Lock()
	defer u.rotMu.Unlock()
	e, ok := u.rotated[jti]
	if !ok || time.Since(e.at) > rotationCacheTTL {
		return nil, false
	}
	return e.pair, true
}

func (u *UI) rememberPair(jti string, pair *auth.Pair) {
	u.rotMu.Lock()
	defer u.rotMu.Unlock()
	now := time.Now()
	for k, e := range u.rotated {
		if now.Sub(e.at) > rotationCacheTTL {
			delete(u.rotated, k)
		}
	}
	u.rotated[jti] = rotEntry{pair: pair, at: now}
}

type rotEntry struct {
	pair *auth.Pair
	at   time.Time
}

// authFailureKind distinguishes redirect (page) vs HX-Redirect (fragment).
type authFailure struct{ hxFrag bool }

func (e authFailure) Error() string { return "unauthenticated" }

// resolveSession returns a valid access token, silently rotating via the
// refresh cookie when the access token expired. On success the (possibly
// refreshed) cookies are rewritten onto the response.
func (u *UI) resolveSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	sess, ok := sessionFrom(r)
	if !ok {
		return "", false
	}
	if sess.AccessToken != "" {
		if _, err := u.verifyAccess(sess.AccessToken); err == nil {
			return sess.AccessToken, true
		}
	}
	if sess.RefreshToken == "" {
		return "", false
	}
	pair, err := u.rotate(sess.RefreshToken)
	if err != nil {
		return "", false
	}
	setSessionCookies(w, r, pair)
	return pair.AccessToken, true
}

// uiAuth guards UI page/fragment routes. Pages redirect to /login; htmx
// fragment requests get HX-Redirect so the browser follows without JS glue.
func (u *UI) uiAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := u.resolveSession(w, r)
		if !ok {
			clearSessionCookies(w)
			if isHX(r) {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
			} else {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}
			return
		}
		next(w, r.WithContext(withToken(r.Context(), token)))
	})
}

// isHX reports whether the request comes from htmx.
func isHX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// sameOriginOk enforces the CSRF defense for state-changing UI requests:
// SameSite=Lax already blocks cross-site POSTs in modern browsers, and this
// explicit Origin/Referer check closes the legacy-client gap.
func (u *UI) sameOriginOk(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		referer := r.Header.Get("Referer")
		if referer == "" {
			return true // non-browser or same-origin GET-style navigation
		}
		ref, err := url.Parse(referer)
		if err != nil {
			return false
		}
		return sameHost(ref.Host, r.Host)
	}
	orig, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return sameHost(orig.Host, r.Host)
}

func sameHost(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	// Host may carry no port in either side behind proxies; compare loosely
	// on host names when ports are absent.
	ha, pa := splitHostPort(a)
	hb, pb := splitHostPort(b)
	if ha != hb {
		return false
	}
	return pa == "" || pb == "" || pa == pb
}

func splitHostPort(host string) (string, string) {
	if h, p, err := net.SplitHostPort(host); err == nil {
		return h, p
	}
	return host, ""
}

// handleLoginPage renders GET /login.
func (u *UI) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// Already signed in: redirect based on role.
	if token, ok := u.resolveSession(w, r); ok {
		if claims, err := u.verifyAccess(token); err == nil {
			if claims.Role == "admin" {
				http.Redirect(w, r, "/admin/overview", http.StatusSeeOther)
			} else {
				http.Redirect(w, r, "/", http.StatusSeeOther)
			}
			return
		}
	}
	u.renderLogin(w, r, "")
}

func (u *UI) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	securityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	data := struct {
		Base
		Error string
		Path  string
	}{
		Base:  u.rd.base("", "登录 · sing-box hub", "", u.themeOf(r)),
		Error: errMsg,
		Path:  r.URL.Query().Get("next"),
	}
	u.loadBranding(r, &data.Base)
	if err := u.rd.render(w, "login", data); err != nil {
		u.logger.Error("render login", "error", err)
	}
}

// handleLoginSubmit processes POST /login form submissions.
func (u *UI) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if !u.sameOriginOk(r) {
		u.renderLogin(w, r, "请求来源校验失败,请重试。")
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if !u.loginLimiter.allow("login:" + ip) {
		w.Header().Set("Retry-After", "30")
		u.renderLogin(w, r, "尝试过于频繁,请稍后再试。")
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderLogin(w, r, "表单解析失败。")
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	if username == "" || password == "" {
		u.renderLogin(w, r, "请输入用户名与密码。")
		return
	}

	res := u.call(r, "", http.MethodPost, "/session", mustJSON(map[string]string{
		"username": username, "password": password,
	}), nil)
	if res.Status != http.StatusCreated {
		msg := res.ProblemDetail()
		if res.Status == http.StatusUnauthorized {
			// Keep the enumeration-resistant wording of the API.
			msg = "用户名或密码错误。"
		}
		u.renderLogin(w, r, msg)
		return
	}
	var tokens sessionTokens
	if err := res.Decode(&tokens); err != nil {
		u.renderLogin(w, r, "登录响应异常,请重试。")
		return
	}
	pair := &auth.Pair{
		AccessToken:      tokens.AccessToken,
		RefreshToken:     tokens.RefreshToken,
		AccessExpiresAt:  time.Now().Add(u.accessTTL),
		RefreshExpiresAt: time.Now().Add(u.refreshTTL),
	}
	setSessionCookies(w, r, pair)
	// Redirect: use "next" param if provided, otherwise stay on current page.
	target := r.PostFormValue("next")
	if target == "" {
		target = "/"
	}
	if !isSafeRedirect(target) {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleLogout processes POST /logout: revoke refresh tokens and clear cookies.
func (u *UI) handleLogout(w http.ResponseWriter, r *http.Request) {
	if u.sameOriginOk(r) {
		if token, ok := ctxToken(r.Context()); ok {
			// DELETE /session revokes every refresh token of the user.
			u.call(r, token, http.MethodDelete, "/session", nil, nil)
		}
	}
	clearSessionCookies(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// themeOf reads the theme preference cookie ("dark" or "light").
func (u *UI) themeOf(r *http.Request) string {
	c, err := r.Cookie(themeCookieName)
	if err != nil || (c.Value != "dark" && c.Value != "light") {
		return "light"
	}
	return c.Value
}

// handleThemeToggle flips the theme cookie and redirects back to the page
// the user came from (plain form post, no JS required).
func (u *UI) handleThemeToggle(w http.ResponseWriter, r *http.Request) {
	current := u.themeOf(r)
	next := "light"
	if current == "light" {
		next = "dark"
	}
	http.SetCookie(w, &http.Cookie{
		Name: themeCookieName, Value: next, Path: cookiePath,
		HttpOnly: false, SameSite: http.SameSiteLaxMode,
		MaxAge: 365 * 24 * 3600,
	})
	target := "/"
	if err := r.ParseForm(); err == nil {
		if v := r.PostFormValue("next"); v != "" {
			target = v
		}
	}
	if !isSafeRedirect(target) {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// mustJSON marshals v or panics; only used with statically-known shapes.
func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// isSafeRedirect checks that a redirect target is a safe relative path
// (no scheme, no host, no protocol-relative URL) to prevent open redirect.
func isSafeRedirect(target string) bool {
	if target == "" {
		return true
	}
	// Must start with / and not //
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		return false
	}
	// Must not contain a scheme separator
	if strings.Contains(target, "://") {
		return false
	}
	// Must not contain backslash (path traversal)
	if strings.Contains(target, "\\") {
		return false
	}
	return true
}

// ---- small context helpers ----

type ctxKey int

const ctxTokenKey ctxKey = 1

func withToken(parent context.Context, token string) context.Context {
	return context.WithValue(parent, ctxTokenKey, token)
}

func ctxToken(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxTokenKey).(string)
	return v, ok
}
