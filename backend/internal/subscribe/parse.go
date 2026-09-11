// Package subscribe converts stored sing-box outbound configs into client
// consumable formats: a sing-box config JSON, Clash/mihomo YAML, and base64
// share-link URI lists.
package subscribe

import (
	"encoding/json"
	"fmt"
)

// Node is one exportable node.
type Node struct {
	Name     string
	Outbound string // sing-box outbound JSON object
}

// UnsupportedError reports an outbound type the exporter cannot convert yet.
type UnsupportedError struct{ TypeName string }

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("outbound type %q is not supported for this format", e.TypeName)
}

// outbound is the normalized view of a sing-box outbound object.
type outbound struct {
	Type       string
	Tag        string
	Server     string
	ServerPort int
	UUID       string
	Password   string
	Method     string
	Security   string
	AlterID    int
	Flow       string

	TLSEnabled bool
	ServerName string
	Insecure   bool
	ALPN       []string

	Transport   string
	Path        string
	Host        string
	ServiceName string

	Congestion string
	Obfs       string
}

func parseOutbound(raw string) (*outbound, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("outbound is not valid JSON: %w", err)
	}
	o := &outbound{
		Type:       getStr(m, "type"),
		Tag:        getStr(m, "tag"),
		Server:     getStr(m, "server"),
		ServerPort: getInt(m, "server_port"),
		UUID:       getStr(m, "uuid"),
		Password:   getStr(m, "password"),
		Method:     getStr(m, "method"),
		Security:   getStr(m, "security"),
		AlterID:    getInt(m, "alter_id"),
		Flow:       getStr(m, "flow"),
		Congestion: getStr(m, "congestion_control"),
	}
	if o.Type == "" {
		return nil, fmt.Errorf("outbound has no type field")
	}

	if tls := getMap(m, "tls"); tls != nil {
		o.TLSEnabled = getBool(tls, "enabled")
		o.ServerName = getStr(tls, "server_name")
		o.Insecure = getBool(tls, "insecure")
		o.ALPN = getStrSlice(tls, "alpn")
	}
	if tr := getMap(m, "transport"); tr != nil {
		o.Transport = getStr(tr, "type")
		o.Path = getStr(tr, "path")
		o.ServiceName = getStr(tr, "service_name")
		if headers := getMap(tr, "headers"); headers != nil {
			o.Host = getStr(headers, "Host")
		}
	}
	if obfs := getMap(m, "obfs"); obfs != nil {
		o.Obfs = getStr(obfs, "type")
	}
	return o, nil
}

// ---- lenient JSON field helpers (values may be any JSON type) ----

func getStr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func getBool(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

func getMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func getStrSlice(m map[string]any, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
