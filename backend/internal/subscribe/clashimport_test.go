package subscribe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sing-hub/panel/internal/kce"
)

const sampleClashYAML = `proxies:
  - name: "jp-01"
    type: vless
    server: jp.example.com
    port: 443
    uuid: 11111111-2222-3333-4444-555555555555
    flow: xtls-rprx-vision
    tls: true
    servername: jp.example.com
    network: ws
    reality-opts:
      public-key: pbk
      short-id: "0123abcd"
    ws-opts:
      path: /ws
      headers:
        Host: h.example.com
  - name: hk-02
    type: ss
    server: 1.2.3.4
    port: 8388
    cipher: aes-256-gcm
    password: "pw"
  - name: us-03
    type: hysteria2
    server: us.example.com
    port: 8443
    password: hpw
    obfs: salamander
    obfs-password: opw
    sni: us.example.com
  - name: broken
    type: snell
    server: 1.1.1.1
    port: 1
`

func TestParseClashConfig(t *testing.T) {
	src, err := ParseClashConfig([]byte(sampleClashYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Nodes) != 3 {
		t.Fatalf("want 3 nodes, got %d (skipped=%v)", len(src.Nodes), src.Skipped)
	}
	if len(src.Skipped) != 1 || src.Skipped[0] != "broken" {
		t.Fatalf("want broken skipped, got %v", src.Skipped)
	}

	byName := map[string]map[string]any{}
	for _, n := range src.Nodes {
		var m map[string]any
		if err := json.Unmarshal([]byte(n.Outbound), &m); err != nil {
			t.Fatal(err)
		}
		if m["tag"] != n.Name {
			t.Fatalf("tag %v != name %s", m["tag"], n.Name)
		}
		byName[n.Name] = m
	}

	vless := byName["jp-01"]
	if vless["type"] != "vless" || vless["server"] != "jp.example.com" || vless["server_port"] != float64(443) {
		t.Fatalf("vless base fields wrong: %v", vless)
	}
	tlsMap := vless["tls"].(map[string]any)
	if tlsMap["server_name"] != "jp.example.com" {
		t.Fatalf("tls server_name wrong: %v", tlsMap)
	}
	reality := tlsMap["reality"].(map[string]any)
	if reality["public_key"] != "pbk" || reality["short_id"] != "0123abcd" {
		t.Fatalf("reality mapping wrong: %v", reality)
	}
	tr := vless["transport"].(map[string]any)
	if tr["type"] != "ws" || tr["path"] != "/ws" {
		t.Fatalf("ws transport wrong: %v", tr)
	}
	if hdr := tr["headers"].(map[string]any); hdr["Host"] != "h.example.com" {
		t.Fatalf("ws host header wrong: %v", hdr)
	}

	ss := byName["hk-02"]
	if ss["type"] != "shadowsocks" || ss["method"] != "aes-256-gcm" || ss["password"] != "pw" {
		t.Fatalf("ss mapping wrong: %v", ss)
	}

	h2 := byName["us-03"]
	if h2["type"] != "hysteria2" {
		t.Fatalf("hysteria2 type wrong: %v", h2)
	}
	if obfs := h2["obfs"].(map[string]any); obfs["password"] != "opw" {
		t.Fatalf("hysteria2 obfs wrong: %v", obfs)
	}
	if h2["tls"] == nil {
		t.Fatal("hysteria2 must have implicit tls")
	}
}

func TestParseClashConfigDuplicateNames(t *testing.T) {
	raw := "proxies:\n  - &a\n    name: dup\n    type: ss\n    server: 1.1.1.1\n    port: 1\n    cipher: aes-128-gcm\n    password: p\n  - <<: *a\n    name: dup\n"
	src, err := ParseClashConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Nodes) != 2 || src.Nodes[1].Name != "dup #2" {
		t.Fatalf("duplicate handling wrong: %+v", src.Nodes)
	}
}

func TestParseClashConfigRejects(t *testing.T) {
	if _, err := ParseClashConfig([]byte("port: 7890")); err == nil {
		t.Fatal("expected error for yaml without proxies")
	}
}

func TestFetchConfigSource(t *testing.T) {
	yamlPayload := []byte(sampleClashYAML)
	sealed, err := kce.Encrypt(yamlPayload, "src-key")
	if err != nil {
		t.Fatal(err)
	}
	var plainServerCalled, sealedServerCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sealed":
			w.Header().Set("Content-Type", "application/octet-stream")
			sealedServerCalled = true
			_, _ = w.Write(sealed)
		case "/plain":
			w.Header().Set("Content-Type", "text/yaml")
			plainServerCalled = true
			_, _ = w.Write(yamlPayload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := srv.Client()
	ctx := context.Background()

	// Plain payload passes through.
	got, err := FetchConfigSource(ctx, client, srv.URL+"/plain", "")
	if err != nil || !strings.Contains(string(got), "proxies:") {
		t.Fatalf("plain fetch: %v %q", err, got)
	}
	// KCE1 payload decrypts with key.
	got, err = FetchConfigSource(ctx, client, srv.URL+"/sealed", "src-key")
	if err != nil || string(got) != string(yamlPayload) {
		t.Fatalf("sealed fetch: %v", err)
	}
	// Missing key on KCE1 payload fails distinctly.
	if _, err := FetchConfigSource(ctx, client, srv.URL+"/sealed", ""); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("want ErrKeyRequired, got %v", err)
	}
	// Non-http url rejected.
	if _, err := FetchConfigSource(ctx, client, "ftp://x/y", "k"); err == nil {
		t.Fatal("expected scheme rejection")
	}
	// 404 surfaces as error.
	if _, err := FetchConfigSource(ctx, client, srv.URL+"/missing", ""); err == nil {
		t.Fatal("expected status error")
	}
	if !plainServerCalled || !sealedServerCalled {
		t.Fatal("servers not exercised")
	}
}
