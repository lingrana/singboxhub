package auth

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sing-hub/panel/internal/store"
)

func sessionStore(t *testing.T) (*store.Store, *Manager, int64) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	id, err := s.CreateUser("account", "test-hash")
	if err != nil {
		t.Fatal(err)
	}
	return s, NewManager(make([]byte, 32), time.Minute, time.Hour, s), id
}

func TestDeletedUserSessionCannotReviveOnIDReuse(t *testing.T) {
	s, manager, id := sessionStore(t)
	pair, err := manager.Issue(id, "account", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(id); err != nil {
		t.Fatal(err)
	}
	reused, err := s.CreateUser("replacement", "other-hash")
	if err != nil {
		t.Fatal(err)
	}
	if reused != id {
		t.Fatal("fixture did not reuse user ID")
	}
	if _, err := manager.VerifyAccess(pair.AccessToken); err == nil {
		t.Fatal("deleted access session survived")
	}
	if _, err := manager.Rotate(pair.RefreshToken); !errors.Is(err, ErrRevoked) {
		t.Fatalf("deleted refresh: %v", err)
	}
}

func TestPasswordResetRevokesEverySession(t *testing.T) {
	s, manager, id := sessionStore(t)
	var pairs []*Pair
	for range 3 {
		pair, err := manager.Issue(id, "account", "admin")
		if err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, pair)
	}
	hash := "updated-hash"
	if err := s.UpdateUser(id, nil, &hash); err != nil {
		t.Fatal(err)
	}
	for _, pair := range pairs {
		if _, err := manager.VerifyAccess(pair.AccessToken); err == nil {
			t.Fatal("reset access survived")
		}
		if _, err := manager.Rotate(pair.RefreshToken); !errors.Is(err, ErrRevoked) {
			t.Fatalf("reset refresh: %v", err)
		}
	}
}

func TestConcurrentRefreshHasOneSuccessor(t *testing.T) {
	_, manager, id := sessionStore(t)
	pair, err := manager.Issue(id, "account", "admin")
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 10)
	for range 10 {
		go func() { _, err := manager.Rotate(pair.RefreshToken); results <- err }()
	}
	winners := 0
	for range 10 {
		err := <-results
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrRevoked) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("refresh successes = %d", winners)
	}
	if _, err := manager.VerifyAccess(pair.AccessToken); err == nil {
		t.Fatal("consumed access still valid")
	}
}
