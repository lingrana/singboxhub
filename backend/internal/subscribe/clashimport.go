package subscribe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/sing-hub/panel/internal/httpx"
	"github.com/sing-hub/panel/internal/kce"
	"gopkg.in/yaml.v3"
)

// fetchSizeLimit bounds subscription payloads (KCE1 caps plaintext at 4 MiB,
// so 8 MiB of ciphertext is a generous ceiling).
const fetchSizeLimit = 8 << 20

// ErrKeyRequired reports a KCE1 payload fetched without decryption key.
var ErrKeyRequired = errors.New("subscription payload is KCE1 encrypted; a decryption key is required")

// ParsedSource is the outcome of decoding one subscription payload.
type ParsedSource struct {
	Nodes   []Node   // convertible proxies as panel export nodes
	Skipped []string // names of proxies dropped (unsupported type/plugin/bad fields)
}

// ParseClashConfig decodes a Clash/mihomo YAML document into exportable
// nodes. Each proxy becomes a sing-box outbound JSON whose tag is the proxy
// name; unsupported entries are skipped and reported in Skipped.
func ParseClashConfig(raw []byte) (*ParsedSource, error) {
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("not a Clash config: %w", err)
	}
	if len(doc.Proxies) == 0 {
		return nil, errors.New("Clash config has no proxies")
	}
	out := &ParsedSource{Nodes: make([]Node, 0, len(doc.Proxies))}
	seen := map[string]bool{}
	for _, p := range doc.Proxies {
		name := asString(p["name"])
		if name == "" {
			out.Skipped = append(out.Skipped, "<unnamed>")
			continue
		}
		unique := uniqueName(name, seen)
		ob, err := clashToOutbound(p, unique)
		if err != nil {
			out.Skipped = append(out.Skipped, name)
			continue
		}
		raw, err := json.Marshal(ob)
		if err != nil {
			out.Skipped = append(out.Skipped, name)
			continue
		}
		seen[unique] = true
		out.Nodes = append(out.Nodes, Node{Name: unique, Outbound: string(raw)})
	}
	return out, nil
}

// uniqueName suffixes duplicates inside one document so panel node names
// (which are globally unique) stay importable.
func uniqueName(name string, seen map[string]bool) string {
	if !seen[name] {
		return name
	}
	for i := 2; ; i++ {
		candidate := name + " #" + strconv.Itoa(i)
		if !seen[candidate] {
			return candidate
		}
	}
}

// clashToOutbound converts one Clash proxy map into a sing-box outbound map.
func clashToOutbound(p map[string]any, tag string) (map[string]any, error) {
	typ := asString(p["type"])
	server := asString(p["server"])
	port := asInt(p["port"])
	if server == "" || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("proxy %q missing server/port", tag)
	}
	ob := map[string]any{
		"type":        singboxType(typ),
		"tag":         tag,
		"server":      server,
		"server_port": port,
	}
	switch typ {
	case "ss":
		if asString(p["plugin"]) != "" {
			return nil, fmt.Errorf("ss plugin %q not supported", asString(p["plugin"]))
		}
		ob["method"] = asString(p["cipher"])
		ob["password"] = asString(p["password"])
	case "vmess":
		ob["uuid"] = asString(p["uuid"])
		ob["alter_id"] = asInt(p["alterId"])
		ob["security"] = orDefaultStr(asString(p["cipher"]), "auto")
	case "vless":
		ob["uuid"] = asString(p["uuid"])
		if flow := asString(p["flow"]); flow != "" {
			ob["flow"] = flow
		}
	case "trojan":
		ob["password"] = asString(p["password"])
	case "hysteria2":
		ob["password"] = asString(p["password"])
		// TLS is implicit in hysteria2.
		p["tls"] = true
		if sni := asString(p["sni"]); sni != "" {
			p["servername"] = sni
		}
		if obfs := asString(p["obfs"]); obfs == "salamander" {
			ob["obfs"] = map[string]any{"type": "salamander", "password": asString(p["obfs-password"])}
		}
	case "tuic":
		ob["uuid"] = asString(p["uuid"])
		if pw := asString(p["password"]); pw != "" {
			ob["password"] = pw
		}
		if cc := asString(p["congestion-controller"]); cc != "" {
			ob["congestion_control"] = cc
		}
		p["tls"] = true
		if sni := asString(p["sni"]); sni != "" {
			p["servername"] = sni
		}
	default:
		return nil, &UnsupportedError{TypeName: typ}
	}

	applyClashTLS(ob, p)
	applyClashTransport(ob, p)
	return ob, nil
}

func singboxType(clashType string) string {
	if clashType == "ss" {
		return "shadowsocks"
	}
	return clashType
}

// applyClashTLS maps tls/servername/skip-cert-verify/alpn/reality-opts onto
// the sing-box tls object.
func applyClashTLS(ob map[string]any, p map[string]any) {
	reality := getMapVal(p, "reality-opts")
	if !asBool(p["tls"]) && len(reality) == 0 {
		return
	}
	tls := map[string]any{"enabled": true}
	if sni := asString(p["servername"]); sni != "" {
		tls["server_name"] = sni
	}
	if asBool(p["skip-cert-verify"]) {
		tls["insecure"] = true
	}
	if alpn := asStringSlice(p["alpn"]); len(alpn) > 0 {
		tls["alpn"] = alpn
	}
	if len(reality) > 0 {
		tls["reality"] = map[string]any{
			"enabled":    true,
			"public_key": asString(reality["public-key"]),
			"short_id":   asString(reality["short-id"]),
		}
	}
	if fp := asString(p["client-fingerprint"]); fp != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	}
	ob["tls"] = tls
}

// applyClashTransport maps network/ws-opts/grpc-opts/http-opts/h2-opts onto
// the sing-box transport object.
func applyClashTransport(ob map[string]any, p map[string]any) {
	network := asString(p["network"])
	switch network {
	case "ws":
		tr := map[string]any{"type": "ws"}
		opts := getMapVal(p, "ws-opts")
		if path := asString(opts["path"]); path != "" {
			tr["path"] = path
		}
		if headers := getMapVal(opts, "headers"); len(headers) > 0 {
			tr["headers"] = headers
		}
		if ed := asInt(opts["max-early-data"]); ed > 0 {
			tr["max_early_data"] = ed
		}
		if edh := asString(opts["early-data-header-name"]); edh != "" {
			tr["early_data_header_name"] = edh
		}
		ob["transport"] = tr
	case "grpc":
		tr := map[string]any{"type": "grpc"}
		opts := getMapVal(p, "grpc-opts")
		if sn := asString(opts["grpc-service-name"]); sn != "" {
			tr["service_name"] = sn
		}
		ob["transport"] = tr
	case "http", "h2":
		tr := map[string]any{"type": "http"}
		opts := getMapVal(p, "http-opts")
		if network == "h2" {
			opts = getMapVal(p, "h2-opts")
		}
		switch path := opts["path"].(type) {
		case string:
			tr["path"] = path
		case []any:
			if len(path) > 0 {
				tr["path"] = asString(path[0])
			}
		}
		if hosts := asStringSlice(opts["host"]); len(hosts) > 0 {
			tr["host"] = hosts
		} else if headers := getMapVal(opts, "headers"); len(headers) > 0 {
			tr["headers"] = headers
		}
		ob["transport"] = tr
	}
}

// ---- lenient YAML field helpers (Clash values may be any scalar shape) ----

func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case int:
		return strconv.Itoa(s)
	case int64:
		return strconv.FormatInt(s, 10)
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	}
	return ""
}

func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func getMapVal(m map[string]any, key string) map[string]any {
	if sub, ok := m[key].(map[string]any); ok {
		return sub
	}
	return nil
}

func asStringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s := asString(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// FetchConfigSource downloads a subscription URL and returns plaintext config
// bytes: KCE1 payloads are decrypted with keyMaterial, anything else passes
// through unchanged (plain Clash YAML sources).
func FetchConfigSource(ctx context.Context, client *http.Client, url, keyMaterial string) ([]byte, error) {
	if err := httpx.ValidateOutboundURL(url); err != nil {
		return nil, errors.New("config url failed safety validation")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpx.SafeDo(ctx, client, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("config url returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchSizeLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > fetchSizeLimit {
		return nil, errors.New("config payload exceeds size limit")
	}
	if kce.IsBlob(body) {
		if keyMaterial == "" {
			return nil, ErrKeyRequired
		}
		return kce.Decrypt(body, keyMaterial)
	}
	return body, nil
}
