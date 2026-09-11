package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sing-hub/panel/internal/auth"
	"github.com/sing-hub/panel/internal/config"
	"github.com/sing-hub/panel/internal/store"
)

func TestFreshInstallAndRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "data")
	cfg := config.Config{DataDir: dir, DBPath: filepath.Join(dir, "db", "panel.db")}
	if err := prepareDataDirs(cfg); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	created, err := initializeAdmin(st, dir)
	if err != nil || !created {
		t.Fatalf("first initialization: created=%v err=%v", created, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "initial-admin-password.txt"))
	if err != nil {
		t.Fatal(err)
	}
	user, err := st.GetUserByUsername("admin")
	if err != nil || !auth.ComparePassword(user.PasswordHash, extractPassword(string(raw))) {
		t.Fatal("generated password does not authenticate default admin")
	}
	if err := os.Remove(filepath.Join(dir, "initial-admin-password.txt")); err != nil {
		t.Fatal(err)
	}
	created, err = initializeAdmin(st, dir)
	if err != nil || created {
		t.Fatalf("restart without password file: created=%v err=%v", created, err)
	}
	again, err := st.GetUserByUsername("admin")
	if err != nil || again.PasswordHash != user.PasswordHash {
		t.Fatal("restart replaced the existing password")
	}
}

func TestExistingInitialPasswordFileIsReused(t *testing.T) {
	dir := t.TempDir()
	password, err := initialPassword(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := initializeAdmin(st, dir); err != nil {
		t.Fatal(err)
	}
	user, err := st.GetUserByUsername("admin")
	if err != nil || !auth.ComparePassword(user.PasswordHash, password) {
		t.Fatal("existing initial password was not preserved")
	}
}
