// Package geo enriches source IPs with geolocation data using a configurable
// provider, caching results in the store for 30 days.
package geo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sing-hub/panel/internal/httpx"
	"github.com/sing-hub/panel/internal/store"
)

const cacheTTL = 30 * 24 * time.Hour

// SettingsFn loads current settings (nil = defaults).
type SettingsFn func() *store.Settings

// Service resolves IP geolocation with caching and in-flight dedup.
type Service struct {
	store    *store.Store
	settings SettingsFn
	hc       *http.Client

	mu       sync.Mutex
	inflight map[string]*call
	window   time.Time
	count    int
}

type call struct {
	done chan struct{}
	geo  *store.Geo
	err  error
}

// New builds the service.
func New(st *store.Store, settings SettingsFn) *Service {
	return &Service{
		store:    st,
		settings: settings,
		hc:       httpx.SafeHTTPClient(4 * time.Second),
		inflight: map[string]*call{},
	}
}

// maxLookupsPerMinute guards against free-provider rate limits.
const maxLookupsPerMinute = 40

// Lookup returns cached-or-fetched geolocation; nil when disabled or on
// transient failure (callers must treat geo as optional).
func (s *Service) Lookup(ctx context.Context, ip string) (*store.Geo, error) {
	if g, err := s.store.GetGeo(ip); err == nil && time.Since(time.Unix(g.CheckedAt, 0)) < cacheTTL {
		return g, nil
	}
	if !s.allow() {
		return nil, nil
	}

	s.mu.Lock()
	if c, ok := s.inflight[ip]; ok {
		s.mu.Unlock()
		select {
		case <-c.done:
			return c.geo, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	s.inflight[ip] = c
	s.mu.Unlock()

	g, err := s.fetch(ctx, ip)
	c.geo, c.err = g, err
	close(c.done)

	s.mu.Lock()
	delete(s.inflight, ip)
	s.mu.Unlock()
	return g, err
}

func (s *Service) allow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Sub(s.window) >= time.Minute {
		s.window = now
		s.count = 0
	}
	if s.count >= maxLookupsPerMinute {
		return false
	}
	s.count++
	return true
}

func (s *Service) fetch(ctx context.Context, ip string) (*store.Geo, error) {
	provider := "http://ip-api.com/json/?fields=query,country,city,as"
	if st := s.settings(); st != nil && st.IPLookupProviderURL != "" {
		provider = st.IPLookupProviderURL
	}

	// Validate provider URL for SSRF protection
	if err := httpx.ValidateOutboundURL(provider); err != nil {
		return nil, err
	}

	u, err := url.Parse(provider)
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + url.PathEscape(ip)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpx.SafeDo(ctx, s.hc, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("ip provider status " + resp.Status)
	}
	var payload struct {
		Query   string `json:"query"`
		Country string `json:"country"`
		City    string `json:"city"`
		AS      string `json:"as"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	g := &store.Geo{
		IP:        ip,
		Country:   payload.Country,
		City:      payload.City,
		ASOrg:     payload.AS,
		CheckedAt: time.Now().Unix(),
	}
	if err := s.store.PutGeo(*g); err != nil {
		slog.Debug("geo cache write failed", slog.Any("error", err))
	}
	return g, nil
}
