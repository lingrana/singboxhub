package sampler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sing-hub/panel/internal/clash"
	"github.com/sing-hub/panel/internal/cryptox"
	"github.com/sing-hub/panel/internal/store"
)

func samplerStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "sampler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func silentLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestClosedAndOfflineIPsPersistZeroWithoutTraffic(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "offline"}[offline], func(t *testing.T) {
			s := samplerStore(t)
			if err := s.CreateNode(&store.Node{ID: "node", Name: "node", APIURL: "http://127.0.0.1:1"}); err != nil {
				t.Fatal(err)
			}
			w := newWorker("node", "node", "http://127.0.0.1:1", "", "", silentLogger())
			h := NewHub(s, make([]byte, 32), silentLogger(), time.Hour)
			snapshot := []clash.Connection{{ID: "conn", Upload: 100, Metadata: clash.Metadata{SourceIP: "192.0.2.1"}}}
			w.applySnapshot(snapshot)
			h.flushWorker(w, time.Now().Unix())
			if offline {
				w.markOffline(errors.New("disconnected"))
			} else {
				w.applySnapshot(nil)
			}
			h.flushWorker(w, time.Now().Unix()+1)
			n, err := s.CountOnlineIPs()
			if err != nil || n != 0 {
				t.Fatalf("online IPs=%d, error=%v", n, err)
			}
			if w.Snapshot().ActiveConnections != 0 {
				t.Fatal("live state still active")
			}
			if offline {
				w.applySnapshot(snapshot)
				h.flushWorker(w, time.Now().Unix()+2)
				rows, err := s.ListIPStats("node", "-total_bytes", "", "", 10)
				if err != nil || len(rows) != 1 || rows[0].UploadBytes != 100 || rows[0].ConnectionsTotal != 1 {
					t.Fatalf("reconnect counted old bytes twice: %+v %v", rows, err)
				}
			}
		})
	}
}

func TestSamplerIntervalReloadAndDisable(t *testing.T) {
	s := samplerStore(t)
	settings, err := s.GetSettings(store.SamplerDefaults{SamplerIntervalSeconds: 7, RetentionDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	intervals := make(chan string, 4)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"version":"test"}`)
			return
		case "/configs":
			io.WriteString(w, `{"mode":"rule"}`)
			return
		case "/memory":
			io.WriteString(w, `{"inuse":1}`)
			return
		}
		upgrader := websocket.Upgrader{}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		payload := `{"up":0,"down":0}`
		if r.URL.Path == "/connections" {
			intervals <- r.URL.Query().Get("interval")
			payload = `{"connections":[{"id":"conn","upload":100,"metadata":{"sourceIP":"192.0.2.1"}}]}`
		}
		if err := ws.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			return
		}
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer mock.Close()
	key := make([]byte, 32)
	secret, err := cryptox.Encrypt(key, "mock")
	if err != nil {
		t.Fatal(err)
	}
	node := &store.Node{ID: "node", Name: "node", APIURL: mock.URL, APISecretEnc: secret, Enabled: true}
	if err := s.CreateNode(node); err != nil {
		t.Fatal(err)
	}
	h := NewHub(s, key, silentLogger(), time.Hour)
	h.Start(context.Background())
	defer h.Stop()
	expectInterval := func(want string) {
		t.Helper()
		select {
		case got := <-intervals:
			if got != want {
				t.Fatalf("interval=%s want=%s", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("connection subscription did not start")
		}
	}
	expectInterval("7000")
	waitActive := func() {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if st, _ := h.Status("node"); st.ActiveConnections == 1 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("snapshot did not arrive")
	}
	waitActive()
	settings.SamplerIntervalSeconds = 11
	if err := s.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := h.Reload(); err != nil {
		t.Fatal(err)
	}
	expectInterval("11000")
	waitActive()
	node.Enabled = false
	if err := s.UpdateNode(node); err != nil {
		t.Fatal(err)
	}
	if err := h.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Status("node"); ok {
		t.Fatal("disabled worker remained")
	}
	n, err := s.CountOnlineIPs()
	if err != nil || n != 0 {
		t.Fatalf("disabled IP count %d: %v", n, err)
	}
	rows, err := s.ListIPStats("node", "-total_bytes", "", "", 10)
	if err != nil || len(rows) != 1 || rows[0].UploadBytes != 100 {
		t.Fatalf("reload lost or duplicated pending bytes: %+v %v", rows, err)
	}
}

func TestStartupMaintenanceClearsStaleStateAndUsesRetention(t *testing.T) {
	s := samplerStore(t)
	now := time.Now().Unix()
	if err := s.CreateNode(&store.Node{ID: "node", Name: "node", APIURL: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	settings, err := s.GetSettings(store.SamplerDefaults{SamplerIntervalSeconds: 2, RetentionDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertIPStats([]store.IPDelta{{NodeID: "node", SourceIP: "192.0.2.1", Active: 1, SeenAt: now}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRollups([]store.RollupRow{{NodeID: "node", Interval: "minute", BucketStart: now - 8*86400}, {NodeID: "node", Interval: "minute", BucketStart: now - 3*86400}}); err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateUser("admin", "test-hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRefreshToken("expired", id, now-1); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRefreshToken("live", id, now+3600); err != nil {
		t.Fatal(err)
	}
	h := NewHub(s, make([]byte, 32), silentLogger(), time.Hour)
	h.Start(context.Background())
	defer h.Stop()
	if n, err := s.CountOnlineIPs(); err != nil || n != 0 {
		t.Fatalf("startup stale IPs %d %v", n, err)
	}
	if _, _, err := s.GetRefreshToken("expired"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired token remained: %v", err)
	}
	if _, _, err := s.GetRefreshToken("live"); err != nil {
		t.Fatalf("live token removed: %v", err)
	}
	rows, err := s.QueryRollup("node", "minute", 0, now, 0, 100)
	if err != nil || len(rows) != 1 {
		t.Fatalf("startup cleanup: %+v %v", rows, err)
	}
	settings.RetentionDays = 1
	if err := s.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	h.maintain(now)
	rows, err = s.QueryRollup("node", "minute", 0, now, 0, 100)
	if err != nil || len(rows) != 0 {
		t.Fatalf("new retention not applied: %+v %v", rows, err)
	}
}
