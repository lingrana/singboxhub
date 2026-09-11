package store

import (
	"database/sql"
	"errors"
	"time"
)

// Operation is a long-running async action (e.g. exit-IP probe).
type Operation struct {
	ID        string
	NodeID    string
	Type      string
	Status    string // PENDING | RUNNING | SUCCEEDED | FAILED
	Result    string
	Error     string
	CreatedAt int64
	UpdatedAt int64
}

// CreateOperation registers a new async operation.
func (s *Store) CreateOperation(op *Operation) error {
	now := time.Now().Unix()
	op.CreatedAt = now
	op.UpdatedAt = now
	_, err := s.exec(`INSERT INTO operations (id, node_id, type, status, result, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, op.NodeID, op.Type, op.Status, op.Result, op.Error, op.CreatedAt, op.UpdatedAt)
	return err
}

// GetOperation loads one operation.
func (s *Store) GetOperation(id string) (*Operation, error) {
	row := s.queryRow(`SELECT id, node_id, type, status, result, error, created_at, updated_at FROM operations WHERE id = ?`, id)
	var op Operation
	var result, opErr sql.NullString
	err := row.Scan(&op.ID, &op.NodeID, &op.Type, &op.Status, &result, &opErr, &op.CreatedAt, &op.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	op.Result = result.String
	op.Error = opErr.String
	return &op, nil
}

// UpdateOperation transitions an operation's state.
func (s *Store) UpdateOperation(id, status, result, opErr string) error {
	_, err := s.exec(`UPDATE operations SET status = ?, result = ?, error = ?, updated_at = ? WHERE id = ?`,
		status, result, opErr, time.Now().Unix(), id)
	return err
}

// ExitIP is the most recent exit-IP probe result of a node.
type ExitIP struct {
	NodeID    string
	IP        string
	Country   string
	City      string
	ASOrg     string
	Method    string
	CheckedAt int64
}

// UpsertExitIP stores the latest probe result for a node.
func (s *Store) UpsertExitIP(e *ExitIP) error {
	_, err := s.exec(`INSERT INTO exit_ip_checks (node_id, ip, country, city, as_org, method, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET ip = excluded.ip, country = excluded.country, city = excluded.city, as_org = excluded.as_org, method = excluded.method, checked_at = excluded.checked_at`,
		e.NodeID, e.IP, e.Country, e.City, e.ASOrg, e.Method, e.CheckedAt)
	return err
}

// GetExitIP loads the latest probe result; ErrNotFound when never probed.
func (s *Store) GetExitIP(nodeID string) (*ExitIP, error) {
	row := s.queryRow(`SELECT node_id, ip, country, city, as_org, method, checked_at FROM exit_ip_checks WHERE node_id = ?`, nodeID)
	var e ExitIP
	err := row.Scan(&e.NodeID, &e.IP, &e.Country, &e.City, &e.ASOrg, &e.Method, &e.CheckedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// Subscription is the per-user subscription token row.
type Subscription struct {
	Token     string // empty = revoked/disabled
	UpdatedAt int64
}

// GetSubscription loads the subscription for a given user, inserting an empty row on first use.
func (s *Store) GetSubscription(userID int64) (*Subscription, error) {
	_, err := s.exec(`INSERT INTO user_subscriptions (user_id, token, updated_at) VALUES (?, '', ?) ON CONFLICT(user_id) DO NOTHING`, userID, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	var sub Subscription
	err = s.queryRow(`SELECT token, updated_at FROM user_subscriptions WHERE user_id = ?`, userID).Scan(&sub.Token, &sub.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &sub, err
}

// SetSubscriptionToken updates (or creates) the subscription token for a given user.
func (s *Store) SetSubscriptionToken(userID int64, token string) error {
	_, err := s.exec(`INSERT INTO user_subscriptions (user_id, token, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET token = excluded.token, updated_at = excluded.updated_at`,
		userID, token, time.Now().Unix())
	return err
}

// FindSubscriptionUser finds the user ID that owns a given subscription token.
func (s *Store) FindSubscriptionUser(token string) (int64, error) {
	var userID int64
	err := s.queryRow(`SELECT user_id FROM user_subscriptions WHERE token = ? AND token != ''`, token).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return userID, err
}

// GetResetCooldownInfo returns the last reset timestamp and cooldown minutes for a user.
// Returns (lastResetAt, cooldownMinutes, error).
func (s *Store) GetResetCooldownInfo(userID int64) (int64, int, error) {
	var lastResetAt int
	var cooldownMinutes int
	err := s.queryRow(`SELECT COALESCE(us.last_reset_at, 0), COALESCE(s.reset_cooldown_minutes, 10)
		FROM user_subscriptions us, settings s WHERE us.user_id = ? AND s.id = 1`, userID).Scan(&lastResetAt, &cooldownMinutes)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 10, nil
	}
	return int64(lastResetAt), cooldownMinutes, err
}

// SetResetNow updates last_reset_at to the current time for a user.
func (s *Store) SetResetNow(userID int64) error {
	_, err := s.exec(`UPDATE user_subscriptions SET last_reset_at = ? WHERE user_id = ?`, time.Now().Unix(), userID)
	return err
}

// Settings is the singleton panel configuration row.
type Settings struct {
	SamplerIntervalSeconds int
	RetentionDays          int
	IPLookupEnabled        bool
	IPLookupProviderURL    string
	SingboxBinPath         string
	Favicon                []byte
	Logo                   []byte
	BrandName              string
	ResetCooldownMinutes   int
	UpdatedAt              int64
	Revision               int64
}

// GetSettings loads the singleton, inserting defaults on first use.
func (s *Store) GetSettings(def SamplerDefaults) (*Settings, error) {
	_, err := s.exec(`INSERT INTO settings
		(id, sampler_interval_seconds, retention_days, ip_lookup_enabled, ip_lookup_provider_url, singbox_bin_path, updated_at)
		VALUES (1, ?, ?, 1, ?, '', ?) ON CONFLICT(id) DO NOTHING`,
		def.SamplerIntervalSeconds, def.RetentionDays, def.IPLookupProviderURL, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	var st Settings
	var enabled int
	var binPath sql.NullString
	var favicon, logo []byte
	var brandName string
	var cooldownMinutes int
	err = s.queryRow(`SELECT sampler_interval_seconds, retention_days, ip_lookup_enabled, ip_lookup_provider_url, singbox_bin_path, favicon, logo, brand_name, COALESCE(reset_cooldown_minutes, 10), updated_at, revision
		FROM settings WHERE id = 1`).Scan(&st.SamplerIntervalSeconds, &st.RetentionDays, &enabled, &st.IPLookupProviderURL, &binPath, &favicon, &logo, &brandName, &cooldownMinutes, &st.UpdatedAt, &st.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	st.IPLookupEnabled = enabled != 0
	st.SingboxBinPath = binPath.String
	st.Favicon = favicon
	st.Logo = logo
	st.BrandName = brandName
	st.ResetCooldownMinutes = cooldownMinutes
	return &st, nil
}

// ErrSettingsConflict means the settings revision has changed since it was read.
var ErrSettingsConflict = errors.New("settings revision changed")

// UpdateSettings persists mutable fields only if the loaded revision is current.
func (s *Store) UpdateSettings(st *Settings) error {
	now := time.Now().Unix()
	res, err := s.exec(`UPDATE settings SET sampler_interval_seconds = ?, retention_days = ?, ip_lookup_enabled = ?, ip_lookup_provider_url = ?, singbox_bin_path = ?, brand_name = ?, reset_cooldown_minutes = ?, updated_at = ?, revision = revision + 1 WHERE id = 1 AND revision = ?`,
		st.SamplerIntervalSeconds, st.RetentionDays, boolInt(st.IPLookupEnabled), st.IPLookupProviderURL, st.SingboxBinPath, st.BrandName, st.ResetCooldownMinutes, now, st.Revision)
	if err != nil {
		return err
	}
	if err := requireAffected(res); errors.Is(err, ErrNotFound) {
		return ErrSettingsConflict
	} else if err != nil {
		return err
	}
	st.UpdatedAt = now
	st.Revision++
	return nil
}

// UpdateSettingsLogo persists the logo image.
func (s *Store) UpdateSettingsLogo(data []byte) error {
	_, err := s.exec(`UPDATE settings SET logo = ?, updated_at = ?, revision = revision + 1 WHERE id = 1`, data, time.Now().Unix())
	return err
}

// UpdateSettingsFavicon persists the favicon image.
func (s *Store) UpdateSettingsFavicon(data []byte) error {
	_, err := s.exec(`UPDATE settings SET favicon = ?, updated_at = ?, revision = revision + 1 WHERE id = 1`, data, time.Now().Unix())
	return err
}

// SamplerDefaults seeds the settings singleton on first run.
type SamplerDefaults struct {
	SamplerIntervalSeconds int
	RetentionDays          int
	IPLookupProviderURL    string
}
