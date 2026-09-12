package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Node is a registered sing-box instance. APISecretEnc and OutboundEnc hold
// AES-GCM base64 ciphertext; encryption/decryption happens in the service
// layer which owns the key. Source marks provenance (” manual,
// 'sub:<id>' imported from an upstream subscription); ConfigURL/KCEKeyEnc
// keep the encrypted-config import settings for later re-imports.
type Node struct {
	ID           string
	Name         string
	APIURL       string
	APISecretEnc string
	OutboundEnc  string
	Source       string
	ConfigURL    string
	KCEKeyEnc    string
	Tags         []string
	Enabled      bool
	Remark       string
	Mode         string // rule/global/direct from Clash proxy-groups
	CreatedAt    int64
	UpdatedAt    int64
	LastOnline   int64 // 0 = never
	HasLastOnl   bool
}

const nodeColumns = `id, name, api_url, api_secret_enc, outbound_enc, source, config_url, kce_key_enc, tags, enabled, remark, mode, created_at, updated_at, last_online_at`

// CreateNode inserts a new node. Name uniqueness is enforced by the schema.
func (s *Store) CreateNode(n *Node) error {
	now := time.Now().Unix()
	n.CreatedAt = now
	n.UpdatedAt = now
	_, err := s.exec(
		`INSERT INTO nodes (id, name, api_url, api_secret_enc, outbound_enc, source, config_url, kce_key_enc, tags, enabled, remark, mode, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.Name, n.APIURL, n.APISecretEnc, n.OutboundEnc, n.Source, n.ConfigURL, n.KCEKeyEnc, strings.Join(n.Tags, ","), boolInt(n.Enabled), n.Remark, n.Mode, now, now)
	return err
}

// GetNode loads one node by id.
func (s *Store) GetNode(id string) (*Node, error) {
	row := s.queryRow(`SELECT `+nodeColumns+` FROM nodes WHERE id = ?`, id)
	return scanNode(row)
}

// GetNodeByName loads a node by unique name.
func (s *Store) GetNodeByName(name string) (*Node, error) {
	row := s.queryRow(`SELECT `+nodeColumns+` FROM nodes WHERE name = ?`, name)
	return scanNode(row)
}

// ListNodes returns all nodes ordered by name.
func (s *Store) ListNodes() ([]Node, error) {
	rows, err := s.query(`SELECT ` + nodeColumns + ` FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

// UpdateNode persists mutable fields of an existing node.
func (s *Store) UpdateNode(n *Node) error {
	n.UpdatedAt = time.Now().Unix()
	res, err := s.exec(
		`UPDATE nodes SET name = ?, api_url = ?, api_secret_enc = ?, outbound_enc = ?, source = ?, config_url = ?, kce_key_enc = ?, tags = ?, enabled = ?, remark = ?, mode = ?, updated_at = ? WHERE id = ?`,
		n.Name, n.APIURL, n.APISecretEnc, n.OutboundEnc, n.Source, n.ConfigURL, n.KCEKeyEnc, strings.Join(n.Tags, ","), boolInt(n.Enabled), n.Remark, n.Mode, n.UpdatedAt, n.ID)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

// DeleteNode removes a node; dependent rows cascade.
func (s *Store) DeleteNode(id string) error {
	res, err := s.exec(`DELETE FROM nodes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return requireAffected(res)
}

// SetLastOnline records the latest time a node answered.
func (s *Store) SetLastOnline(id string, ts int64) error {
	_, err := s.exec(`UPDATE nodes SET last_online_at = ? WHERE id = ?`, ts, id)
	return err
}

// ListNodesBySource returns all nodes imported from one upstream source.
func (s *Store) ListNodesBySource(source string) ([]Node, error) {
	rows, err := s.query(`SELECT `+nodeColumns+` FROM nodes WHERE source = ? ORDER BY name`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanNode(r rowScanner) (*Node, error) {
	var n Node
	var tags string
	var enabled int
	var lastOnline sql.NullInt64
	err := r.Scan(&n.ID, &n.Name, &n.APIURL, &n.APISecretEnc, &n.OutboundEnc, &n.Source, &n.ConfigURL, &n.KCEKeyEnc, &tags, &enabled, &n.Remark, &n.Mode, &n.CreatedAt, &n.UpdatedAt, &lastOnline)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	n.Tags = splitCSV(tags)
	n.Enabled = enabled != 0
	n.HasLastOnl = lastOnline.Valid
	n.LastOnline = lastOnline.Int64
	return &n, nil
}
