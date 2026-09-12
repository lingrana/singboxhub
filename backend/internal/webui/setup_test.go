package webui_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sing-hub/panel/internal/webui"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// freeAddress reserves a loopback port and releases it for the setup server.
func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func startSetup(t *testing.T, addr string) (base string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := webui.RunDatabaseSetup(ctx, addr, t.TempDir(), quietLogger())
		done <- err
	}()
	base = "http://" + addr
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/setup")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base, func() {
					cancel()
					if err := <-done; err != nil && err != context.Canceled {
						t.Errorf("setup server error: %v", err)
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	t.Fatal("setup server did not start")
	return base, func() {}
}

func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// TestSetupModeRedirectsToSetup pins the first-run behavior: before a
// database is configured, every panel path (including / and /login) lands on
// the initialization page instead of a 404.
func TestSetupModeRedirectsToSetup(t *testing.T) {
	base, stop := startSetup(t, freeAddress(t))
	defer stop()

	client := noRedirectClient()
	for _, path := range []string{"/", "/login", "/admin/overview", "/admin/nodes"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("GET %s status = %d, want %d (body=%s)", path, resp.StatusCode, http.StatusSeeOther, body)
		}
		if loc := resp.Header.Get("Location"); loc != "/setup" {
			t.Fatalf("GET %s Location = %q, want /setup", path, loc)
		}
	}

	resp, err := http.Get(base + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "初始化数据库") {
		t.Fatalf("GET /setup status=%d, want 200 with setup page", resp.StatusCode)
	}
}

// TestSetupPostgresFailureShowsDetail checks that a failed connection surfaces
// the underlying error on the page without echoing the DSN (which may carry
// the password).
func TestSetupPostgresFailureShowsDetail(t *testing.T) {
	base, stop := startSetup(t, freeAddress(t))
	defer stop()

	closed := freeAddress(t) // port with no listener
	dsn := "postgres://user:pass@" + closed + "/singhub?sslmode=disable"

	form := strings.NewReader(url.Values{"driver": {"postgres"}, "dsn": {dsn}}.Encode())
	req, err := http.NewRequest(http.MethodPost, base+"/setup", form)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /setup status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "数据库连接或初始化失败") {
		t.Fatalf("POST /setup body misses failure detail: %s", body)
	}
	if strings.Contains(string(body), dsn) {
		t.Fatalf("POST /setup body echoes the DSN: %s", body)
	}
}
