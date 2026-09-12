package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sing-hub/panel/internal/auth"
	"github.com/sing-hub/panel/internal/config"
	"github.com/sing-hub/panel/internal/store"
)

func prepareDataDirs(cfg config.Config) error {
	for _, dir := range []string{cfg.DataDir, filepath.Dir(cfg.DBPath)} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	return nil
}

func initializeAdmin(st *store.Store, dir string, setupCredentials ...string) (bool, error) {
	n, err := st.CountUsers()
	if err != nil || n > 0 {
		return false, err
	}
	username := "admin"
	password := ""
	if len(setupCredentials) >= 2 {
		username = setupCredentials[0]
		password = setupCredentials[1]
	}
	if password == "" {
		password, err = initialPassword(dir)
		if err != nil {
			return false, err
		}
	}
	if len(password) < 6 || len(password) > 72 {
		return false, errors.New("initial password must contain 6-72 bytes")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return false, err
	}
	_, err = st.CreateUser(username, hash)
	return err == nil, err
}

func initialPassword(dir string) (string, error) {
	path := filepath.Join(dir, "initial-admin-password.txt")
	if raw, err := os.ReadFile(path); err == nil {
		return extractPassword(string(raw)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	password := base64.RawURLEncoding.EncodeToString(raw)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return initialPassword(dir)
	}
	if err != nil {
		return "", err
	}
	_, writeErr := fmt.Fprintln(f, password)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return "", err
	}
	return password, nil
}
