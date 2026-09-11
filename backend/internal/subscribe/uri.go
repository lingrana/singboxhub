package subscribe

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// ssURI renders a SIP002 share link.
func ssURI(o *outbound, name string) (string, error) {
	if o.Server == "" || o.Method == "" {
		return "", fmt.Errorf("shadowsocks outbound missing server/method")
	}
	userinfo := base64.RawURLEncoding.EncodeToString([]byte(o.Method + ":" + o.Password))
	return fmt.Sprintf("ss://%s@%s:%d#%s", userinfo, o.Server, o.ServerPort, url.QueryEscape(name)), nil
}

// vmessURI renders the base64-JSON v2 format.
func vmessURI(o *outbound, name string) (string, error) {
	if o.Server == "" || o.UUID == "" {
		return "", fmt.Errorf("vmess outbound missing server/uuid")
	}
	security := o.Security
	if security == "" {
		security = "auto"
	}
	payload := map[string]any{
		"v":    "2",
		"ps":   name,
		"add":  o.Server,
		"port": o.ServerPort,
		"id":   o.UUID,
		"aid":  o.AlterID,
		"scy":  security,
		"net":  transportName(o.Transport),
		"host": o.Host,
		"path": o.Path,
		"tls":  boolToFlag(o.TLSEnabled),
		"sni":  o.ServerName,
		"alpn": strings.Join(o.ALPN, ","),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(raw), nil
}

// vlessURI renders the standard vless share link.
func vlessURI(o *outbound, name string) (string, error) {
	if o.Server == "" || o.UUID == "" {
		return "", fmt.Errorf("vless outbound missing server/uuid")
	}
	q := url.Values{}
	q.Set("encryption", "none")
	if o.Flow != "" {
		q.Set("flow", o.Flow)
	}
	addTLSParams(q, o)
	addTransportParams(q, o)
	return fmt.Sprintf("vless://%s@%s:%d?%s#%s", o.UUID, o.Server, o.ServerPort, q.Encode(), url.QueryEscape(name)), nil
}

// trojanURI renders the trojan-golang share link.
func trojanURI(o *outbound, name string) (string, error) {
	if o.Server == "" || o.Password == "" {
		return "", fmt.Errorf("trojan outbound missing server/password")
	}
	q := url.Values{}
	addTLSParams(q, o)
	addTransportParams(q, o)
	return fmt.Sprintf("trojan://%s@%s:%d?%s#%s",
		url.QueryEscape(o.Password), o.Server, o.ServerPort, q.Encode(), url.QueryEscape(name)), nil
}

// hysteria2URI renders the hysteria2 share link.
func hysteria2URI(o *outbound, name string) (string, error) {
	if o.Server == "" {
		return "", fmt.Errorf("hysteria2 outbound missing server")
	}
	q := url.Values{}
	if o.ServerName != "" {
		q.Set("sni", o.ServerName)
	}
	if o.Insecure {
		q.Set("insecure", "1")
	}
	if o.Obfs != "" {
		q.Set("obfs", o.Obfs)
	}
	if len(o.ALPN) > 0 {
		q.Set("alpn", strings.Join(o.ALPN, ","))
	}
	auth := o.Password
	return fmt.Sprintf("hysteria2://%s@%s:%d?%s#%s",
		url.QueryEscape(auth), o.Server, o.ServerPort, q.Encode(), url.QueryEscape(name)), nil
}

// tuicURI renders the TUIC v5 share link.
func tuicURI(o *outbound, name string) (string, error) {
	if o.Server == "" || o.UUID == "" {
		return "", fmt.Errorf("tuic outbound missing server/uuid")
	}
	q := url.Values{}
	if o.Congestion != "" {
		q.Set("congestion_control", o.Congestion)
	}
	if len(o.ALPN) > 0 {
		q.Set("alpn", strings.Join(o.ALPN, ","))
	}
	if o.ServerName != "" {
		q.Set("sni", o.ServerName)
	}
	if o.Insecure {
		q.Set("allow_insecure", "1")
	}
	auth := o.UUID
	if o.Password != "" {
		// TUIC v5 userinfo: uuid:password,冒号是分隔符,不转义
		auth = o.UUID + ":" + o.Password
	}
	return fmt.Sprintf("tuic://%s@%s:%d?%s#%s",
		auth, o.Server, o.ServerPort, q.Encode(), url.QueryEscape(name)), nil
}

func addTLSParams(q url.Values, o *outbound) {
	if o.TLSEnabled {
		q.Set("security", "tls")
	} else {
		q.Set("security", "none")
	}
	if o.ServerName != "" {
		q.Set("sni", o.ServerName)
	}
	if o.Insecure {
		q.Set("allowInsecure", "1")
	}
	if len(o.ALPN) > 0 {
		q.Set("alpn", strings.Join(o.ALPN, ","))
	}
}

func addTransportParams(q url.Values, o *outbound) {
	if o.Transport == "" {
		return
	}
	q.Set("type", transportName(o.Transport))
	if o.Host != "" {
		q.Set("host", o.Host)
	}
	if o.Path != "" {
		q.Set("path", o.Path)
	}
	if o.ServiceName != "" {
		q.Set("serviceName", o.ServiceName)
	}
}

func transportName(t string) string {
	if t == "" {
		return "tcp"
	}
	return t
}

func boolToFlag(b bool) string {
	if b {
		return "tls"
	}
	return ""
}
