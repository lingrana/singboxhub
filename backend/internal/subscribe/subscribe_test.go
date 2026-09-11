package subscribe

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const vlessOutbound = `{"type":"vless","tag":"t1","server":"1.2.3.4","server_port":443,"uuid":"11111111-2222-3333-4444-555555555555","tls":{"enabled":true,"server_name":"example.com"},"transport":{"type":"ws","path":"/ws","headers":{"Host":"example.com"}}}`

func TestGenerateSingbox(t *testing.T) {
	raw, err := GenerateSingbox([]Node{{Name: "节点一", Outbound: vlessOutbound}})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var cfg struct {
		Outbounds []struct {
			Type string   `json:"type"`
			Tag  string   `json:"tag"`
			All  []string `json:"outbounds"`
		} `json:"outbounds"`
		Route map[string]any `json:"route"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(cfg.Outbounds) != 2 || cfg.Outbounds[0].Type != "selector" {
		t.Fatalf("outbounds = %+v", cfg.Outbounds)
	}
	if cfg.Route["final"] != SelectorTag {
		t.Fatalf("route.final = %v", cfg.Route["final"])
	}
}

func TestGenerateClash(t *testing.T) {
	raw, err := GenerateClash([]Node{{Name: "节点一", Outbound: vlessOutbound}})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	text := string(raw)
	for _, want := range []string{"proxies:", "type: vless", "proxy-groups:", "MATCH,PROXY", "servername: example.com", "network: ws"} {
		if !strings.Contains(text, want) {
			t.Fatalf("clash output missing %q:\n%s", want, text)
		}
	}
}

func TestGenerateBase64(t *testing.T) {
	raw, err := GenerateBase64([]Node{{Name: "节点一", Outbound: vlessOutbound}})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		t.Fatalf("not base64: %v", err)
	}
	if !strings.HasPrefix(string(decoded), "vless://") {
		t.Fatalf("decoded = %s", decoded)
	}
}

func TestShareURI(t *testing.T) {
	cases := []struct {
		name     string
		outbound string
		prefix   string
	}{
		{"vless", vlessOutbound, "vless://11111111-2222-3333-4444-555555555555@1.2.3.4:443"},
		{"ss", `{"type":"shadowsocks","tag":"s","server":"2.3.4.5","server_port":8388,"method":"aes-128-gcm","password":"pw"}`, "ss://"},
		{"trojan", `{"type":"trojan","tag":"t","server":"3.3.3.3","server_port":443,"password":"pw"}`, "trojan://pw@3.3.3.3:443"},
		{"hysteria2", `{"type":"hysteria2","tag":"h","server":"4.4.4.4","server_port":443,"password":"auth"}`, "hysteria2://auth@4.4.4.4:443"},
		{"tuic", `{"type":"tuic","tag":"u","server":"5.5.5.5","server_port":443,"uuid":"uuid-x","password":"pw"}`, "tuic://uuid-x:pw@5.5.5.5:443"},
	}
	for _, c := range cases {
		uri, err := ShareURI(Node{Name: c.name, Outbound: c.outbound})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !strings.HasPrefix(uri, c.prefix) {
			t.Fatalf("%s uri = %s, want prefix %s", c.name, uri, c.prefix)
		}
		if !strings.HasSuffix(uri, "#"+c.name) {
			t.Fatalf("%s uri missing fragment: %s", c.name, uri)
		}
	}
	// vmess 单独验证(base64 JSON)
	uri, err := ShareURI(Node{Name: "vm", Outbound: `{"type":"vmess","tag":"v","server":"6.6.6.6","server_port":443,"uuid":"uuid-y"}`})
	if err != nil || !strings.HasPrefix(uri, "vmess://") {
		t.Fatalf("vmess = %s, %v", uri, err)
	}

	// 不支持的类型
	if _, err := ShareURI(Node{Name: "x", Outbound: `{"type":"wireguard","tag":"w"}`}); err == nil {
		t.Fatal("expected UnsupportedError for wireguard")
	}
}

func TestEmptyNodesFails(t *testing.T) {
	if _, err := GenerateSingbox(nil); err == nil {
		t.Fatal("expected error for empty nodes")
	}
	if _, err := GenerateClash(nil); err == nil {
		t.Fatal("expected error for empty nodes")
	}
	if _, err := GenerateBase64(nil); err == nil {
		t.Fatal("expected error for empty nodes")
	}
}
