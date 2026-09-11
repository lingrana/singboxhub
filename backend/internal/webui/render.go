// Package webui renders the server-side panel UI: html/template pages and
// htmx fragments. All data flows through the panel's own JSON API (invoked
// in-process with the caller's bearer token), so the API stays the single
// source of truth. Browser sessions use HttpOnly cookies; the JSON API
// contract keeps bearer-token auth untouched.
package webui

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"strings"
	"sync/atomic"
	"time"
)

//go:embed templates/*.gohtml
var templateFS embed.FS

//go:embed all:static
var staticFS embed.FS

// Renderer parses the embedded templates once and renders pages/fragments.
type Renderer struct {
	tpls   *template.Template
	cssVer string
}

// NewRenderer parses all templates under templates/*.gohtml.
func NewRenderer() (*Renderer, error) {
	tpl := template.New("root").Funcs(template.FuncMap{
		// Format helpers mirror the old frontend (frontend/src/api.ts).
		"fmtBytes": fmtBytes,
		"fmtBps":   fmtBps,
		"fmtAgo":   fmtAgo,
		"fmtTime":  fmtTime,
		"shortID":  shortID,
		// Small template shorthands.
		"listOf":     listOf,
		"hostOr":     hostOr,
		"modeOr":     modeOr,
		"orStr":      orStr,
		"fieldErr":   fieldErr,
		"rangeLabel": rangeLabel,
		"addOne":     func(i int) int { return i + 1 },
		"add":        func(a, b int) int { return a + b },
		"sub":        func(a, b int) int { return a - b },
		"geoLine":    geoLine,
		"mkMap": func(pairs ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(pairs); i += 2 {
				m[pairs[i].(string)] = pairs[i+1]
			}
			return m
		},
		"nodeCardData": func() nodeCardData { return nodeCardData{} },
		"slice": func(s string, i, j int) string {
			runes := []rune(s)
			if i < 0 {
				i = 0
			}
			if j > len(runes) {
				j = len(runes)
			}
			if i > j {
				return ""
			}
			return string(runes[i:j])
		},
	})
	parsed, err := tpl.ParseFS(templateFS, "templates/*.gohtml")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	css, err := fs.ReadFile(staticFS, "static/style.css")
	if err != nil {
		return nil, fmt.Errorf("read style.css: %w", err)
	}
	return &Renderer{tpls: parsed, cssVer: contentStamp(css)}, nil
}

// Static exposes the embedded static assets (htmx, css, app.js).
func Static() (fs.FS, error) { return fs.Sub(staticFS, "static") }

// CSSVersion returns a content-derived stamp for style.css cache busting.
func (rd *Renderer) CSSVersion() string { return rd.cssVer }

// render executes the named template into w.
func (rd *Renderer) render(w io.Writer, name string, data any) error {
	return rd.tpls.ExecuteTemplate(w, name, data)
}

var requestCounter atomic.Uint64

// Base is the common payload every page and fragment receives.
type Base struct {
	// Page names the active nav item: overview|nodes|traffic|ips|subscription|settings.
	Page       string
	Title      string
	Username   string
	Theme      string // "light" | "dark" from cookie
	Nonce      string
	CSSVer     string
	FaviconURL string
	LogoURL    string
	BrandName  string
	// Path is the current request path (theme toggle redirect target).
	Path string
}

// navItem is one entry of the top navigation.
type navItem struct {
	Href   string
	Label  string
	Active bool
}

// NavItems computes the top navigation with the active marker.
func (b Base) NavItems() []navItem {
	items := []navItem{
		{Href: "/admin/overview", Label: "总览"},
		{Href: "/admin/nodes", Label: "主机"},
		{Href: "/admin/traffic", Label: "用量"},
		{Href: "/admin/subscription", Label: "订阅"},
		{Href: "/admin/users", Label: "用户"},
		{Href: "/admin/settings", Label: "设置"},
	}
	for i := range items {
		if items[i].Href == "/admin/overview" {
			items[i].Active = b.Page == "overview"
			continue
		}
		items[i].Active = strings.HasSuffix(items[i].Href, "/"+b.Page)
	}
	return items
}

// BodyName is the template name rendered inside the layout.
func (b Base) BodyName() string {
	if b.Page == "" {
		return "page_empty"
	}
	return "page_" + b.Page
}

func (rd *Renderer) base(page, title, username, theme string) Base {
	return Base{
		Page:     page,
		Title:    title,
		Username: username,
		Theme:    theme,
		Nonce:    fmt.Sprintf("n%d", requestCounter.Add(1)),
		CSSVer:   rd.cssVer,
	}
}

// ---- template format helpers ----

func fmtBytes(n any) string {
	v, ok := toFloat(n)
	if !ok {
		return "—"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	abs := v
	if abs < 0 {
		abs = -abs
	}
	i := 0
	for abs >= 1024 && i < len(units)-1 {
		abs /= 1024
		i++
	}
	digits := 1
	if i == 0 {
		digits = 0
	}
	return fmt.Sprintf("%.*f %s", digits, abs, units[i])
}

func fmtBps(n any) string {
	v, ok := toFloat(n)
	if !ok {
		return "—"
	}
	units := []string{"bps", "Kbps", "Mbps", "Gbps"}
	i := 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	digits := 1
	if i == 0 {
		digits = 0
	}
	return fmt.Sprintf("%.*f %s", digits, v, units[i])
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case *int:
		if n == nil {
			return 0, false
		}
		return float64(*n), true
	case *int64:
		if n == nil {
			return 0, false
		}
		return float64(*n), true
	case *float64:
		if n == nil {
			return 0, false
		}
		return *n, true
	default:
		return 0, false
	}
}

func fmtAgo(v any) string {
	iso := derefString(v)
	if iso == "" {
		return "从未"
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}
	diff := time.Since(t)
	switch {
	case diff < time.Minute:
		return "刚刚"
	case diff < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(diff.Minutes()))
	case diff < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(diff.Hours()))
	default:
		return fmt.Sprintf("%d 天前", int(diff.Hours()/24))
	}
}

func fmtTime(v any) string {
	iso := derefString(v)
	if iso == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// shortID renders a UUID's first segment for compact node titles.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

// derefString accepts string / *string / nil from templates.
func derefString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case *string:
		if s == nil {
			return ""
		}
		return *s
	case nil:
		return ""
	default:
		return ""
	}
}

// listOf builds []string for template range over dynamic items.
func listOf(items ...string) []string { return items }

// hostOr renders "host:port" preferring the sniffed host.
func hostOr(host, destIP string) string {
	if host != "" {
		return host
	}
	return destIP
}

// modeOr renders the runtime mode with a fallback dash.
func modeOr(mode *string) string {
	if mode == nil || *mode == "" {
		return "—"
	}
	return *mode
}

// orStr renders v (string or *string) or the fallback.
func orStr(v any, fallback string) string {
	s := derefString(v)
	if s == "" {
		return fallback
	}
	return s
}

// fieldErr looks up a per-field validation message in templates.
func fieldErr(errs map[string]string, field string) string { return errs[field] }

// rangeLabel maps a range key to its Chinese display label.
func rangeLabel(key string) string {
	switch key {
	case "hour":
		return "最近 1 小时"
	case "6h":
		return "最近 6 小时"
	case "24h":
		return "最近 24 小时"
	case "7d":
		return "最近 7 天"
	case "30d":
		return "最近 30 天"
	default:
		return key
	}
}

// geoLine renders "country city [as_org]" for an IP row.
func geoLine(g *geoInfo) string {
	if g == nil {
		return "—"
	}
	parts := strings.TrimSpace(g.Country + " " + g.City)
	if g.ASOrg != "" {
		parts = strings.TrimSpace(parts + " " + g.ASOrg)
	}
	if parts == "" {
		return "—"
	}
	return parts
}

// contentStamp derives a short deterministic stamp for cache busting.
func contentStamp(b []byte) string {
	const hexDigits = "0123456789abcdef"
	var acc uint64
	for i, c := range b {
		acc = acc*31 + uint64(c)
		if i%97 == 96 {
			acc ^= acc >> 29
		}
	}
	out := make([]byte, 0, 12)
	for i := 0; i < 12; i++ {
		out = append(out, hexDigits[acc&0xf])
		acc >>= 4
	}
	return string(out)
}
