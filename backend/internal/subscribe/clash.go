package subscribe

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// clashProxy converts one outbound into a mihomo proxy map.
func clashProxy(n Node) (map[string]any, error) {
	o, err := parseOutbound(n.Outbound)
	if err != nil {
		return nil, err
	}
	if o.Server == "" || o.ServerPort <= 0 {
		return nil, fmt.Errorf("outbound %q missing server/port", o.Tag)
	}

	base := map[string]any{
		"name":   n.Name,
		"server": o.Server,
		"port":   o.ServerPort,
	}
	switch o.Type {
	case "shadowsocks", "ss":
		base["type"] = "ss"
		base["cipher"] = o.Method
		base["password"] = o.Password
	case "vmess":
		base["type"] = "vmess"
		base["uuid"] = o.UUID
		base["alterId"] = o.AlterID
		base["cipher"] = orDefault(o.Security, "auto")
	case "vless":
		base["type"] = "vless"
		base["uuid"] = o.UUID
		if o.Flow != "" {
			base["flow"] = o.Flow
		}
	case "trojan":
		base["type"] = "trojan"
		base["password"] = o.Password
	case "hysteria2":
		base["type"] = "hysteria2"
		base["password"] = o.Password
	case "tuic":
		base["type"] = "tuic"
		base["uuid"] = o.UUID
		if o.Password != "" {
			base["password"] = o.Password
		}
		if o.Congestion != "" {
			base["congestion-controller"] = o.Congestion
		}
	default:
		return nil, &UnsupportedError{TypeName: o.Type}
	}

	if o.TLSEnabled {
		base["tls"] = true
	}
	if o.ServerName != "" {
		base["servername"] = o.ServerName
	}
	if o.Insecure {
		base["skip-cert-verify"] = true
	}
	if len(o.ALPN) > 0 {
		base["alpn"] = o.ALPN
	}
	switch o.Transport {
	case "ws":
		base["network"] = "ws"
		opts := map[string]any{}
		if o.Path != "" {
			opts["path"] = o.Path
		}
		if o.Host != "" {
			opts["headers"] = map[string]any{"Host": o.Host}
		}
		base["ws-opts"] = opts
	case "grpc":
		base["network"] = "grpc"
		if o.ServiceName != "" {
			base["grpc-opts"] = map[string]any{"grpc-service-name": o.ServiceName}
		}
	case "http":
		base["network"] = "http"
		if o.Path != "" || o.Host != "" {
			opts := map[string]any{}
			if o.Path != "" {
				opts["path"] = []string{o.Path}
			}
			if o.Host != "" {
				opts["headers"] = map[string]any{"Host": o.Host}
			}
			base["http-opts"] = opts
		}
	}
	return base, nil
}

// marshalYAMLMap renders one proxy map as indented "key: value" lines.
func marshalYAMLMap(m map[string]any) ([]string, error) {
	raw, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		lines = append(lines, line)
	}
	return lines, nil
}

// yamlQuote quotes a scalar when needed for YAML plain style.
func yamlQuote(s string) string {
	if s == "" || strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`,") || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		return strconv.Quote(s)
	}
	return s
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
