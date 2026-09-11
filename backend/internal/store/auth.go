package store

import (
	"database/sql"
	"errors"
	"time"
)

// User is a panel administrator account.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	CreatedAt    int64
}

// CountUsers reports whether any account exists (first-run initialization).
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.queryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts an account and returns its id.
func (s *Store) CreateUser(username, passwordHash string) (int64, error) {
	return s.createUser(username, passwordHash, "admin")
}

// CreateUserWithRole inserts an account with a specified role and returns its id.
func (s *Store) CreateUserWithRole(username, passwordHash, role string) (int64, error) {
	return s.createUser(username, passwordHash, role)
}

func (s *Store) createUser(username, passwordHash, role string) (int64, error) {
	now := time.Now().Unix()
	if s.driver == DriverPostgres {
		var id int64
		err := s.queryRow(`INSERT INTO users (username, password_hash, role, created_at) VALUES (?, ?, ?, ?) RETURNING id`, username, passwordHash, role, now).Scan(&id)
		return id, err
	}
	res, err := s.exec(`INSERT INTO users (username, password_hash, role, created_at) VALUES (?, ?, ?, ?)`, username, passwordHash, role, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetUserByUsername loads one account.
func (s *Store) GetUserByUsername(username string) (*User, error) {
	row := s.queryRow(`SELECT id, username, password_hash, role, created_at FROM users WHERE username = ?`, username)
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

// InsertRefreshToken registers a refresh token jti for revocation checks.
func (s *Store) InsertRefreshToken(jti string, userID int64, expiresAt int64) error {
	_, err := s.exec(
		`INSERT INTO refresh_tokens (jti, user_id, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		jti, userID, expiresAt, time.Now().Unix())
	return err
}

// GetRefreshToken returns the revocation state of a refresh token jti.
func (s *Store) GetRefreshToken(jti string) (revoked bool, expiresAt int64, err error) {
	row := s.queryRow(`SELECT revoked, expires_at FROM refresh_tokens WHERE jti = ?`, jti)
	var revokedInt int
	err = row.Scan(&revokedInt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, ErrNotFound
	}
	return revokedInt != 0, expiresAt, err
}

// RevokeRefreshToken marks a jti as revoked (logout).
func (s *Store) RevokeRefreshToken(jti string) error {
	_, err := s.exec(`UPDATE refresh_tokens SET revoked = 1 WHERE jti = ?`, jti)
	return err
}

// ReplaceRefreshToken atomically consumes one live session and registers its successor.
func (s *Store) ReplaceRefreshToken(oldJTI, newJTI string, userID, expiresAt int64) (bool, error) {
	tx, err := s.begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	res, err := s.txExec(tx, `UPDATE refresh_tokens SET revoked = 1
		WHERE jti = ? AND user_id = ? AND revoked = 0 AND expires_at > ?`, oldJTI, userID, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if _, err := s.txExec(tx, `INSERT INTO refresh_tokens (jti, user_id, expires_at, created_at)
		VALUES (?, ?, ?, ?)`, newJTI, userID, expiresAt, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// RevokeAllRefreshTokens revokes every live refresh token of a user.
func (s *Store) RevokeAllRefreshTokens(userID int64) error {
	_, err := s.exec(`UPDATE refresh_tokens SET revoked = 1 WHERE user_id = ? AND revoked = 0`, userID)
	return err
}

// DeleteExpiredTokens prunes stale refresh token rows.
func (s *Store) DeleteExpiredTokens(now int64) error {
	_, err := s.exec(`DELETE FROM refresh_tokens WHERE expires_at < ?`, now)
	return err
}

// ListUsers returns all admin accounts.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.query(`SELECT id, username, password_hash, role, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUserByID loads one account by id.
func (s *Store) GetUserByID(id int64) (*User, error) {
	row := s.queryRow(`SELECT id, username, password_hash, role, created_at FROM users WHERE id = ?`, id)
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

// UpdateUser changes supplied account fields and revokes its sessions in one transaction.
func (s *Store) UpdateUser(id int64, username, passwordHash *string) error {
	tx, err := s.begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := s.txExec(tx, `UPDATE users SET username = COALESCE(?, username),
		password_hash = COALESCE(?, password_hash) WHERE id = ?`, username, passwordHash, id)
	if err != nil {
		return err
	}
	if err := requireAffected(res); err != nil {
		return err
	}
	if username != nil || passwordHash != nil {
		if _, err := s.txExec(tx, `UPDATE refresh_tokens SET revoked = 1 WHERE user_id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteUser removes an account and cascades its refresh tokens.
func (s *Store) DeleteUser(id int64) error {
	res, err := s.exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return requireAffected(res)
}
