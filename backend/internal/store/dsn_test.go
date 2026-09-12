package store

import "testing"

func TestNormalizeDSNPostgresURLInjectsSSLModePrefer(t *testing.T) {
	got := normalizeDSN("postgres", "postgres://user:password@127.0.0.1:5432/singhub")
	want := "postgres://user:password@127.0.0.1:5432/singhub?sslmode=prefer"
	if got != want {
		t.Fatalf("normalizeDSN = %q, want %q", got, want)
	}
}

func TestNormalizeDSNPostgresURLScheme(t *testing.T) {
	got := normalizeDSN("postgres", "postgresql://user:password@127.0.0.1:5432/singhub")
	want := "postgresql://user:password@127.0.0.1:5432/singhub?sslmode=prefer"
	if got != want {
		t.Fatalf("normalizeDSN = %q, want %q", got, want)
	}
}

func TestNormalizeDSNPostgresKeepsExplicitSSLMode(t *testing.T) {
	const dsn = "postgres://user:password@127.0.0.1:5432/singhub?sslmode=require"
	if got := normalizeDSN("postgres", dsn); got != dsn {
		t.Fatalf("normalizeDSN = %q, want %q", got, dsn)
	}
	const kw = "host=127.0.0.1 user=postgres password=secret dbname=singhub sslmode=disable"
	if got := normalizeDSN("postgres", kw); got != kw {
		t.Fatalf("normalizeDSN = %q, want %q", got, kw)
	}
}

func TestNormalizeDSNPostgresKeywordFormAppendsSSLMode(t *testing.T) {
	got := normalizeDSN("postgres", "host=127.0.0.1 user=postgres password=secret dbname=singhub")
	want := "host=127.0.0.1 user=postgres password=secret dbname=singhub sslmode=prefer"
	if got != want {
		t.Fatalf("normalizeDSN = %q, want %q", got, want)
	}
}

func TestNormalizeDSNSQLiteStripsFileScheme(t *testing.T) {
	if got := normalizeDSN("sqlite", "file:data/panel.db"); got != "data/panel.db" {
		t.Fatalf("normalizeDSN = %q, want data/panel.db", got)
	}
	if got := normalizeDSN("sqlite", "data/panel.db"); got != "data/panel.db" {
		t.Fatalf("normalizeDSN = %q, want data/panel.db", got)
	}
}
