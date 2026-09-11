package subscribe

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// SelectorTag is the auto-generated group clients switch within.
const SelectorTag = "PROXY"

// GenerateSingbox renders a minimal but complete sing-box client config.
func GenerateSingbox(nodes []Node) ([]byte, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no exportable nodes")
	}
	names := make([]string, 0, len(nodes))
	outbounds := make([]json.RawMessage, 0, len(nodes)+1)
	for _, n := range nodes {
		if !json.Valid([]byte(n.Outbound)) {
			continue
		}
		names = append(names, n.Name)
		outbounds = append(outbounds, json.RawMessage(n.Outbound))
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no valid outbound configs")
	}
	selector := map[string]any{
		"type":                        "selector",
		"tag":                         SelectorTag,
		"outbounds":                   names,
		"interrupt_exist_connections": false,
	}
	cfg := map[string]any{
		"log":       map[string]any{"level": "warn"},
		"outbounds": append([]json.RawMessage{mustJSON(selector)}, outbounds...),
		"route":     map[string]any{"final": SelectorTag},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return raw
}

// GenerateClash renders a mihomo/Clash.Meta YAML profile.
func GenerateClash(nodes []Node) ([]byte, error) {
	proxies := make([]map[string]any, 0, len(nodes))
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		proxy, err := clashProxy(n)
		if err != nil {
			continue
		}
		names = append(names, n.Name)
		proxies = append(proxies, proxy)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no exportable nodes")
	}
	profile := map[string]any{
		"proxies": proxies,
		"proxy-groups": []map[string]any{
			{"name": SelectorTag, "type": "select", "proxies": names},
		},
		"rules": []string{"MATCH," + SelectorTag},
	}
	return yaml.Marshal(profile)
}

// GenerateBase64 renders newline-joined share URIs, base64 encoded.
func GenerateBase64(nodes []Node) ([]byte, error) {
	uris := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if uri, err := ShareURI(n); err == nil && uri != "" {
			uris = append(uris, uri)
		}
	}
	if len(uris) == 0 {
		return nil, fmt.Errorf("no exportable nodes")
	}
	plain := strings.Join(uris, "\n")
	return []byte(base64.StdEncoding.EncodeToString([]byte(plain))), nil
}

// ShareURI converts one outbound into its share link when the type is
// supported; unsupported types yield an UnsupportedError.
func ShareURI(n Node) (string, error) {
	o, err := parseOutbound(n.Outbound)
	if err != nil {
		return "", err
	}
	switch o.Type {
	case "shadowsocks", "ss":
		return ssURI(o, n.Name)
	case "vmess":
		return vmessURI(o, n.Name)
	case "vless":
		return vlessURI(o, n.Name)
	case "trojan":
		return trojanURI(o, n.Name)
	case "hysteria2":
		return hysteria2URI(o, n.Name)
	case "tuic":
		return tuicURI(o, n.Name)
	default:
		return "", &UnsupportedError{TypeName: o.Type}
	}
}
