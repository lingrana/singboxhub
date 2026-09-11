package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSettingsMigrationPreservesExistingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:2] {
		if _, err := db.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (2);
		INSERT INTO settings (id, sampler_interval_seconds, retention_days, updated_at, logo) VALUES (1, 8, 90, 123, X'010203')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settings, err := s.GetSettings(SamplerDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Revision != 1 || settings.SamplerIntervalSeconds != 8 || settings.RetentionDays != 90 || string(settings.Logo) != "\x01\x02\x03" {
		t.Fatalf("migration changed existing settings: %+v", settings)
	}
	settings.RetentionDays = 60
	if err := s.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	if settings.Revision != 2 {
		t.Fatal("revision did not advance")
	}
}

func TestConcurrentSettingsUpdatesHaveOneWinner(t *testing.T) {
	s := testStore(t)
	settings, err := s.GetSettings(SamplerDefaults{SamplerIntervalSeconds: 2, RetentionDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 12)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copy := *settings
			copy.RetentionDays = 40 + i
			results <- s.UpdateSettings(&copy)
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrSettingsConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("successful writers = %d", winners)
	}
}

func TestUpdateUserRollsBackWhenSessionRevocationFails(t *testing.T) {
	s := testStore(t)
	id, err := s.CreateUser("original", "original-hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRefreshToken("session", id, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_revoke BEFORE UPDATE ON refresh_tokens BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	name, hash := "changed", "changed-hash"
	if err := s.UpdateUser(id, &name, &hash); err == nil {
		t.Fatal("expected revocation failure")
	}
	user, err := s.GetUserByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "original" || user.PasswordHash != "original-hash" {
		t.Fatal("partial user update survived rollback")
	}
}

func TestMaintenanceRetentionBoundaries(t *testing.T) {
	s := testStore(t)
	const now int64 = 1800000000
	const day int64 = 86400
	if err := s.CreateNode(&Node{ID: "node", Name: "node", APIURL: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	for _, interval := range []string{"minute", "hour", "day"} {
		keep := int64(7)
		if interval == "day" {
			keep = 365
		}
		if err := s.UpsertRollups([]RollupRow{
			{NodeID: "node", Interval: interval, BucketStart: now - keep*day - 1, UpBytes: 10},
			{NodeID: "node", Interval: interval, BucketStart: now - keep*day, UpBytes: 20},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertTrafficSamples([]TrafficSample{{NodeID: "node", Ts: now - 3*day - 1}, {NodeID: "node", Ts: now - 3*day}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneTraffic(now, 7); err != nil {
		t.Fatal(err)
	}
	for _, interval := range []string{"minute", "hour", "day"} {
		rows, err := s.QueryRollup("node", interval, 0, now, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].UpBytes != 20 {
			t.Fatalf("retention for %s: %+v", interval, rows)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM traffic_samples`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("raw retention left %d rows", n)
	}
}
