package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sing-hub/panel/internal/auth"
	"github.com/sing-hub/panel/internal/config"
	"github.com/sing-hub/panel/internal/sampler"
	"github.com/sing-hub/panel/internal/store"
)

type testEnv struct {
	ts     *httptest.Server
	client *http.Client
	token  string
}

func newTestEnv(t *testing.T) *testEnv {
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

	if _, err := st.CreateUser("admin", mustHash("test-password")); err != nil {
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
	t.Cleanup(func() {
		hcancel()
		hub.Stop()
	})

	srv := NewServer(cfg, slog.Default(), st, cryptoKey, authMgr, hub)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	return &testEnv{ts: ts, client: ts.Client()}
}

func mustHash(password string) string {
	hash, err := auth.HashPassword(password)
	if err != nil {
		panic(err)
	}
	return hash
}

// do performs a request and returns status + decoded JSON body.
func (e *testEnv) do(t *testing.T, method, path string, body any, headers map[string]string) (int, map[string]any, http.Header) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed, resp.Header
}

func (e *testEnv) login(t *testing.T) string {
	t.Helper()
	status, body, _ := e.do(t, "POST", "/session", map[string]string{
		"username": "admin", "password": "test-password",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("login status = %d, body=%v", status, body)
	}
	return body["access_token"].(string)
}

func (e *testEnv) authedHeaders(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{"Authorization": "Bearer " + e.token}
}

// ---- tests ----

func TestHealthNoAuth(t *testing.T) {
	e := newTestEnv(t)
	status, body, _ := e.do(t, "GET", "/health", nil, nil)
	if status != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("health = %d %v", status, body)
	}
}

func TestUnauthorizedIsProblemJSON(t *testing.T) {
	e := newTestEnv(t)
	status, body, _ := e.do(t, "GET", "/nodes", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d", status)
	}
	assertProblem(t, body, http.StatusUnauthorized)
}

func TestLoginFailuresUniform(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct{ user, pass string }{
		{"admin", "wrong"}, {"nosuchuser", "x"},
	}
	for _, c := range cases {
		status, body, _ := e.do(t, "POST", "/session", map[string]string{"username": c.user, "password": c.pass}, nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("login(%s) status = %d", c.user, status)
		}
		assertProblem(t, body, http.StatusUnauthorized)
		// 防枚举:未知用户与错误密码文案一致
		if body["detail"] != "invalid credentials" {
			t.Fatalf("detail = %v", body["detail"])
		}
	}
}

func TestNodeCrudAndValidation(t *testing.T) {
	e := newTestEnv(t)
	e.token = e.login(t)
	h := e.authedHeaders(t)

	// 创建:201 + Location
	status, node, hdr := e.do(t, "POST", "/nodes", map[string]any{
		"name": "n1", "api_url": "http://10.0.0.1:9090", "api_secret": "s3cret",
	}, h)
	if status != http.StatusCreated {
		t.Fatalf("create = %d %v", status, node)
	}
	if hdr.Get("Location") == "" {
		t.Fatal("missing Location header")
	}
	if node["api_secret_set"] != true {
		t.Fatal("secret must never be echoed")
	}
	if _, leaks := node["api_secret"]; leaks {
		t.Fatal("api_secret must not appear in response")
	}
	id := node["id"].(string)

	// 重名:409
	status, body, _ := e.do(t, "POST", "/nodes", map[string]any{
		"name": "n1", "api_url": "http://10.0.0.2:9090", "api_secret": "x",
	}, h)
	if status != http.StatusConflict {
		t.Fatalf("duplicate = %d", status)
	}
	assertProblem(t, body, http.StatusConflict)

	// 校验失败:422 + JSON Pointer
	status, body, _ = e.do(t, "POST", "/nodes", map[string]any{
		"name": "", "api_url": "ftp://bad", "api_secret": "",
	}, h)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid = %d", status)
	}
	assertProblem(t, body, http.StatusUnprocessableEntity)

	// 读取:200;不存在:404
	status, _, hdr = e.do(t, "GET", "/nodes/"+id, nil, h)
	if status != http.StatusOK {
		t.Fatalf("get = %d", status)
	}
	etag := hdr.Get("ETag")
	status, body, _ = e.do(t, "GET", "/nodes/00000000-0000-0000-0000-000000000000", nil, h)
	if status != http.StatusNotFound {
		t.Fatalf("missing get = %d", status)
	}
	assertProblem(t, body, http.StatusNotFound)

	// PATCH merge-patch:更新标签 (带 If-Match)
	patchH := map[string]string{"Authorization": h["Authorization"], "If-Match": etag}
	var patched map[string]any
	status, patched, _ = e.do(t, "PATCH", "/nodes/"+id, map[string]any{"tags": []string{"a", "b"}}, patchH)
	tags, tagsOk := patched["tags"].([]any)
	if status != http.StatusOK || !tagsOk || len(tags) != 2 {
		t.Fatalf("patch = %d %v", status, patched)
	}

	// DELETE:204;再删:404
	status, _, _ = e.do(t, "DELETE", "/nodes/"+id, nil, h)
	if status != http.StatusNoContent {
		t.Fatalf("delete = %d", status)
	}
	status, _, _ = e.do(t, "DELETE", "/nodes/"+id, nil, h)
	if status != http.StatusNotFound {
		t.Fatalf("re-delete = %d", status)
	}
}

func TestCreateNodeIdempotencyKey(t *testing.T) {
	e := newTestEnv(t)
	e.token = e.login(t)
	h := map[string]string{"Authorization": "Bearer " + e.token, "Idempotency-Key": "key-1"}
	payload := map[string]any{"name": "idem", "api_url": "http://10.0.0.9:9090", "api_secret": "s"}

	status1, body1, _ := e.do(t, "POST", "/nodes", payload, h)
	status2, body2, _ := e.do(t, "POST", "/nodes", payload, h)
	if status1 != http.StatusCreated || status2 != http.StatusCreated {
		t.Fatalf("idempotent statuses: %d %d", status1, status2)
	}
	if body1["id"] != body2["id"] {
		t.Fatalf("idempotent replay returned different node: %v vs %v", body1["id"], body2["id"])
	}
}

func TestNodePaginationKeyset(t *testing.T) {
	e := newTestEnv(t)
	e.token = e.login(t)
	h := e.authedHeaders(t)

	for i := 0; i < 25; i++ {
		status, _, _ := e.do(t, "POST", "/nodes", map[string]any{
			"name": fmt.Sprintf("node-%02d", i), "api_url": "http://10.0.0.1:9090", "api_secret": "s",
		}, h)
		if status != http.StatusCreated {
			t.Fatalf("seed %d = %d", i, status)
		}
	}

	// page_size 上限校验
	status, body, _ := e.do(t, "GET", "/nodes?page_size=500", nil, h)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("oversize page_size = %d %v", status, body)
	}

	seen := map[string]bool{}
	pages := 0
	cursor := ""
	for {
		path := "/nodes?page_size=10"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		status, body, _ := e.do(t, "GET", path, nil, h)
		if status != http.StatusOK {
			t.Fatalf("list = %d", status)
		}
		items := body["items"].([]any)
		if pages < 2 && len(items) != 10 {
			t.Fatalf("page %d items = %d, want 10", pages, len(items))
		}
		for _, it := range items {
			name := it.(map[string]any)["name"].(string)
			if seen[name] {
				t.Fatalf("duplicate node across pages: %s", name)
			}
			seen[name] = true
		}
		pages++
		next, ok := body["next_cursor"].(string)
		if !ok || next == "" {
			break
		}
		cursor = next
	}
	if pages != 3 || len(seen) != 25 {
		t.Fatalf("pages=%d seen=%d, want 3/25", pages, len(seen))
	}
}

func TestSettingsPreconditionFlow(t *testing.T) {
	e := newTestEnv(t)
	e.token = e.login(t)
	h := e.authedHeaders(t)

	// 缺 If-Match → 428
	status, body, _ := e.do(t, "PATCH", "/settings", map[string]any{"retention_days": 60}, h)
	if status != http.StatusPreconditionRequired {
		t.Fatalf("no if-match = %d %v", status, body)
	}

	// 读取 ETag
	status, _, hdr := e.do(t, "GET", "/settings", nil, h)
	etag := hdr.Get("ETag")
	if status != http.StatusOK || etag == "" {
		t.Fatalf("get settings = %d etag=%q", status, etag)
	}

	// 错误 ETag → 412
	bad := map[string]string{"Authorization": h["Authorization"], "If-Match": `"stale"`}
	status, _, _ = e.do(t, "PATCH", "/settings", map[string]any{"retention_days": 60}, bad)
	if status != http.StatusPreconditionFailed {
		t.Fatalf("bad etag = %d", status)
	}

	// 正确 ETag → 200,字段越界 → 422
	ok := map[string]string{"Authorization": h["Authorization"], "If-Match": etag}
	status, updated, savedHeader := e.do(t, "PATCH", "/settings", map[string]any{"retention_days": 60}, ok)
	if status != http.StatusOK || updated["retention_days"].(float64) != 60 {
		t.Fatalf("update = %d %v", status, updated)
	}
	ok["If-Match"] = savedHeader.Get("ETag")
	status, _, _ = e.do(t, "PATCH", "/settings", map[string]any{"retention_days": 100000}, ok)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid retention = %d", status)
	}
}

func TestSubscriptionLifecycle(t *testing.T) {
	e := newTestEnv(t)
	e.token = e.login(t)
	h := e.authedHeaders(t)

	// 生成节点(含 outbound)→ 令牌 → 订阅可用
	status, _, _ := e.do(t, "POST", "/nodes", map[string]any{
		"name": "sub-node", "api_url": "http://10.0.0.3:9090", "api_secret": "s",
		"outbound_json": `{"type":"vless","tag":"t1","server":"1.2.3.4","server_port":443,"uuid":"11111111-2222-3333-4444-555555555555"}`,
	}, h)
	if status != http.StatusCreated {
		t.Fatalf("seed node = %d", status)
	}
	status, sub, _ := e.do(t, "POST", "/subscription/token", nil, h)
	if status != http.StatusOK {
		t.Fatalf("reset token = %d", status)
	}
	token := sub["token"].(string)
	if token == "" || sub["url"] == nil {
		t.Fatalf("subscription incomplete: %v", sub)
	}

	// 公开端点:有效令牌 200;无效令牌 404
	resp, err := e.client.Get(e.ts.URL + "/sub/" + token + "?format=singbox")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("sub singbox = %v/%d", err, resp.StatusCode)
	}
	resp.Body.Close()
	status, body, _ := e.do(t, "GET", "/sub/invalid-token", nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("bad token = %d", status)
	}
	assertProblem(t, body, http.StatusNotFound)

	// 吊销后立即失效
	status, _, _ = e.do(t, "DELETE", "/subscription", nil, h)
	if status != http.StatusNoContent {
		t.Fatalf("revoke = %d", status)
	}
	resp, err = e.client.Get(e.ts.URL + "/sub/" + token)
	if err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after revoke = %v/%d", err, resp.StatusCode)
	}
	if resp != nil {
		resp.Body.Close()
	}
}

func TestTrafficValidation(t *testing.T) {
	e := newTestEnv(t)
	e.token = e.login(t)
	h := e.authedHeaders(t)

	status, node, _ := e.do(t, "POST", "/nodes", map[string]any{
		"name": "traffic-node", "api_url": "http://10.0.0.4:9090", "api_secret": "s",
	}, h)
	id := node["id"].(string)

	status, body, _ := e.do(t, "GET", "/nodes/"+id+"/traffic?interval=weekly", nil, h)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("bad interval = %d", status)
	}
	assertProblem(t, body, http.StatusUnprocessableEntity)

	status, body, _ = e.do(t, "GET", "/nodes/"+id+"/traffic?from=not-a-time", nil, h)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("bad from = %d", status)
	}
}

func TestCheckUnreachableNodeReturnsOnlineFalse(t *testing.T) {
	e := newTestEnv(t)
	e.token = e.login(t)
	h := e.authedHeaders(t)

	status, node, _ := e.do(t, "POST", "/nodes", map[string]any{
		"name": "dead-node", "api_url": "http://127.0.0.1:1", "api_secret": "s",
	}, h)
	id := node["id"].(string)

	status, result, _ := e.do(t, "POST", "/nodes/"+id+"/check", nil, h)
	if status != http.StatusOK {
		t.Fatalf("check = %d", status)
	}
	if result["online"] != false {
		t.Fatalf("online = %v, want false", result["online"])
	}
}

// assertProblem verifies RFC 9457 shape: status field agrees with HTTP status.
func assertProblem(t *testing.T, body map[string]any, wantStatus int) {
	t.Helper()
	if body == nil {
		t.Fatal("empty problem body")
	}
	if body["type"] == nil || body["title"] == nil {
		t.Fatalf("problem missing type/title: %v", body)
	}
	got, ok := body["status"].(float64)
	if !ok || int(got) != wantStatus {
		t.Fatalf("problem.status = %v, want %d", body["status"], wantStatus)
	}
	if !strings.Contains(body["type"].(string), "probs/") && body["type"].(string) != "about:blank" {
		t.Fatalf("problem.type = %v", body["type"])
	}
}
