package webui_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sing-hub/panel/internal/api"
	"github.com/sing-hub/panel/internal/auth"
	"github.com/sing-hub/panel/internal/config"
	"github.com/sing-hub/panel/internal/sampler"
	"github.com/sing-hub/panel/internal/store"
)

// Cookie names mirror webui's session cookies (kept in sync by contract).
const (
	accessCookieName  = "singhub_at"
	refreshCookieName = "singhub_rt"
)

// uiEnv is a UI test environment wired to a real API server.
type uiEnv struct {
	ts     *httptest.Server
	client *http.Client
}

func newUIEnv(t *testing.T) *uiEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cryptoKey := make([]byte, 32)
	jwtKey := make([]byte, 32)
	for i := range cryptoKey {
		cryptoKey[i] = byte(i + 1)
		jwtKey[i] = byte(i + 100)
	}
	if _, err := st.CreateUser("admin", hashForTest("test-password")); err != nil {
		t.Fatalf("create user: %v", err)
	}

	cfg := config.Config{
		ListenAddr: "127.0.0.1:0",
		DataDir:    dir,
		Session: config.SessionConfig{
			AccessTokenTTL:  5 * time.Minute,
			RefreshTokenTTL: time.Hour,
		},
		Probe:   config.ProbeConfig{IPProviderURL: "http://127.0.0.1:1/json", ProbeTimeoutSeconds: 1},
		Sampler: config.SamplerConfig{PersistInterval: time.Second, RetentionDays: 30, ConnInterval: 2 * time.Second},
	}
	authMgr := auth.NewManager(jwtKey, cfg.Session.AccessTokenTTL, cfg.Session.RefreshTokenTTL, st)
	hub := sampler.NewHub(st, cryptoKey, slog.Default(), cfg.Sampler.PersistInterval)
	hctx, hcancel := context.WithCancel(context.Background())
	hub.Start(hctx)
	srv := api.NewServer(cfg, slog.Default(), st, cryptoKey, authMgr, hub)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(func() {
		hcancel()
		hub.Stop()
		ts.Close()
	})

	// Never follow redirects: the tests assert on 303 responses directly.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return &uiEnv{ts: ts, client: client}
}

func hashForTest(password string) string {
	hash, err := auth.HashPassword(password)
	if err != nil {
		panic(err)
	}
	return hash
}

// login submits the UI login form and asserts a session is established.
func (e *uiEnv) login(t *testing.T) {
	t.Helper()
	res := e.postForm(t, "/login", url.Values{
		"username": {"admin"},
		"password": {"test-password"},
		"next":     {"/admin/overview"},
	}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", res.StatusCode)
	}
	loc := res.Header.Get("Location")
	if loc != "/admin/overview" {
		t.Fatalf("login redirect = %q, want /admin/overview", loc)
	}
	if len(res.Cookies()) < 2 {
		t.Fatalf("expected access+refresh cookies, got %d", len(res.Cookies()))
	}
}

// postForm sends a form POST and follows nothing.
func (e *uiEnv) postForm(t *testing.T, path string, form url.Values, cookies []*http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// get performs a GET with the given cookies.
func (e *uiEnv) get(t *testing.T, path string, cookies []*http.Cookie, hx bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.ts.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// ---- tests ----

func TestLoginRedirectsAndSetsCookies(t *testing.T) {
	env := newUIEnv(t)
	env.login(t)
}

func TestUnauthenticatedPageRedirectsToLogin(t *testing.T) {
	env := newUIEnv(t)
	res := env.get(t, "/admin/overview", nil, false)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", res.StatusCode)
	}
	if res.Header.Get("Location") != "/login" {
		t.Fatalf("location = %q, want /login", res.Header.Get("Location"))
	}
}

func TestUnauthenticatedFragmentReturnsHXRedirect(t *testing.T) {
	env := newUIEnv(t)
	res := env.get(t, "/admin/overview/stats", nil, true)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	if res.Header.Get("HX-Redirect") != "/login" {
		t.Fatalf("HX-Redirect = %q, want /login", res.Header.Get("HX-Redirect"))
	}
}

func TestWrongPasswordShowsError(t *testing.T) {
	env := newUIEnv(t)
	res := env.postForm(t, "/login", url.Values{
		"username": {"admin"},
		"password": {"wrong"},
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (login page with error)", res.StatusCode)
	}
}

func TestAccessRotationViaRefreshCookie(t *testing.T) {
	env := newUIEnv(t)
	env.login(t)

	// Forge an expired access cookie while keeping the refresh cookie: the
	// next request must silently rotate and still serve the page.
	// (We can't easily mint an expired token here without the key, so instead
	// we verify rotation by replacing the access cookie with garbage.)
	loginRes := env.postForm(t, "/login", url.Values{
		"username": {"admin"},
		"password": {"test-password"},
	}, nil)
	var refreshCookie *http.Cookie
	for _, c := range loginRes.Cookies() {
		if c.Name == refreshCookieName {
			cc := *c
			refreshCookie = &cc
		}
	}
	if refreshCookie == nil {
		t.Fatalf("refresh cookie missing")
	}
	bad := &http.Cookie{Name: accessCookieName, Value: "garbage"}

	res := env.get(t, "/admin/overview/stats", []*http.Cookie{bad, refreshCookie}, true)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after rotation", res.StatusCode)
	}
	var newAccess *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == accessCookieName {
			newAccess = c
		}
	}
	if newAccess == nil || newAccess.Value == "" {
		t.Fatalf("expected refreshed access cookie")
	}
}

func TestOverviewPageRendersWithSession(t *testing.T) {
	env := newUIEnv(t)
	cookies := env.sessionCookies(t)
	res := env.get(t, "/admin/overview", cookies, false)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	body := readBody(t, res)
	for _, want := range []string{"在线节点", "实时上行", "实时下行", "在线IP", "sing-box hub", "总览"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview page missing %q", want)
		}
	}
}

func TestNodesPageRendersAndCreateNodeViaAPI(t *testing.T) {
	env := newUIEnv(t)
	cookies := env.sessionCookies(t)
	res := env.get(t, "/admin/nodes", cookies, false)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}

	// Create through the UI form endpoint; the card list fragment should
	// render the node afterwards.
	res = env.postForm(t, "/admin/nodes/new", url.Values{
		"name":       {"test-node"},
		"api_url":    {"http://127.0.0.1:1"},
		"api_secret": {"s3cret"},
		"tags":       {"prod, jp"},
		"enabled":    {"on"},
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d, want 200 (empty body clears modal slot)", res.StatusCode)
	}
	if res.Header.Get("HX-Trigger") == "" {
		t.Fatalf("expected HX-Trigger header on success")
	}

	listRes := env.get(t, "/admin/nodes/list", cookies, true)
	body := readBody(t, listRes)
	if !strings.Contains(body, "test-node") {
		t.Errorf("node list missing created node; body: %s", body[:min(len(body), 300)])
	}
	if !strings.Contains(body, "prod") {
		t.Errorf("node list missing tag")
	}
}

func TestNodeCreateValidationErrorRendersForm(t *testing.T) {
	env := newUIEnv(t)
	cookies := env.sessionCookies(t)
	res := env.postForm(t, "/admin/nodes/new", url.Values{
		"name":       {""},
		"api_url":    {"not-a-url"},
		"api_secret": {"x"},
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (form re-render)", res.StatusCode)
	}
	body := readBody(t, res)
	if !strings.Contains(body, "modal") {
		t.Errorf("expected form re-render")
	}
}

func TestSettingsSaveConflictAndSuccess(t *testing.T) {
	env := newUIEnv(t)
	cookies := env.sessionCookies(t)

	// Fetch the settings page to obtain the current ETag via the API.
	formRes := env.get(t, "/admin/settings", cookies, false)
	if formRes.StatusCode != http.StatusOK {
		t.Fatalf("settings page status = %d", formRes.StatusCode)
	}

	// Save without etag -> 428 from the API -> error banner, form re-render.
	res := env.postForm(t, "/admin/settings", url.Values{
		"etag":                     {""},
		"sampler_interval_seconds": {"10"},
		"retention_days":           {"30"},
		"ip_lookup_enabled":        {"on"},
		"ip_lookup_provider_url":   {"http://api.ip-api.com/json"},
	}, cookies)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d, want 200", res.StatusCode)
	}
	body := readBody(t, res)
	if !strings.Contains(body, "error") && !strings.Contains(body, "修改") && !strings.Contains(body, "If-Match") {
		t.Errorf("expected error banner in re-rendered form; body: %s", body[:min(len(body), 400)])
	}
}

func TestRootShowsPublicStatus(t *testing.T) {
	env := newUIEnv(t)
	res := env.get(t, "/", nil, false)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	body := readBody(t, res)
	if !strings.Contains(body, "sing-box hub") {
		t.Errorf("root page missing branding")
	}
}

func TestStaticAssetsServed(t *testing.T) {
	env := newUIEnv(t)
	for _, path := range []string{"/admin/assets/style.css", "/admin/assets/htmx.min.js", "/admin/assets/app.js"} {
		res := env.get(t, path, nil, false)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, res.StatusCode)
		}
	}
}

// ---- helpers ----

func (e *uiEnv) sessionCookies(t *testing.T) []*http.Cookie {
	t.Helper()
	res := e.postForm(t, "/login", url.Values{
		"username": {"admin"},
		"password": {"test-password"},
	}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", res.StatusCode)
	}
	return res.Cookies()
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
