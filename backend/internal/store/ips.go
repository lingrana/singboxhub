package store

import (
	"database/sql"
	"errors"
	"strconv"
)

// IPDelta is one sampling-round change for a (node, source_ip) pair.
type IPDelta struct {
	NodeID    string
	SourceIP  string
	UpDelta   int64
	DownDelta int64
	ConnCount int
	Active    int
	SeenAt    int64
}

// IPStat is the aggregated per-source-IP row of one node.
type IPStat struct {
	NodeID            string
	SourceIP          string
	UploadBytes       int64
	DownloadBytes     int64
	TotalBytes        int64
	ConnectionsTotal  int64
	ActiveConnections int
	FirstSeenAt       int64
	LastSeenAt        int64
	NodesCount        int64 // only set by cross-node aggregates
}

// Geo is a cached IP geolocation result.
type Geo struct {
	IP        string
	Country   string
	Region    string
	City      string
	ASOrg     string
	CheckedAt int64
}

// ClearActiveIPs resets stale runtime state without changing historical counters.
// An empty nodeID clears all nodes at service startup or shutdown.
func (s *Store) ClearActiveIPs(nodeID string) error {
	if nodeID == "" {
		_, err := s.exec(`UPDATE ip_stats SET active_connections = 0 WHERE active_connections != 0`)
		return err
	}
	_, err := s.exec(`UPDATE ip_stats SET active_connections = 0 WHERE node_id = ?`, nodeID)
	return err
}

// UpsertIPStats applies one batch of per-connection deltas.
func (s *Store) UpsertIPStats(deltas []IPDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := s.begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	statStmt, err := s.txPrepare(tx, `INSERT INTO ip_stats
		(node_id, source_ip, upload_bytes, download_bytes, connections_total, active_connections, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id, source_ip) DO UPDATE SET
		  upload_bytes       = upload_bytes + excluded.upload_bytes,
		  download_bytes     = download_bytes + excluded.download_bytes,
		  connections_total  = connections_total + excluded.connections_total,
		  active_connections = excluded.active_connections,
		  last_seen_at       = excluded.last_seen_at`)
	if err != nil {
		return err
	}
	defer statStmt.Close()

	dailyStmt, err := s.txPrepare(tx, `INSERT INTO ip_daily (node_id, source_ip, day, up_bytes, down_bytes)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(node_id, source_ip, day) DO UPDATE SET
		  up_bytes   = up_bytes + excluded.up_bytes,
		  down_bytes = down_bytes + excluded.down_bytes`)
	if err != nil {
		return err
	}
	defer dailyStmt.Close()

	for _, d := range deltas {
		if _, err := statStmt.Exec(d.NodeID, d.SourceIP, d.UpDelta, d.DownDelta, d.ConnCount, d.Active, d.SeenAt, d.SeenAt); err != nil {
			return err
		}
		if _, err := dailyStmt.Exec(d.NodeID, d.SourceIP, dayStart(d.SeenAt), d.UpDelta, d.DownDelta); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListIPStats returns one node's IP rows, newest-first by the chosen sort.
// cursor is "<sortValue>|<source_ip>" from the previous page (keyset).
func (s *Store) ListIPStats(nodeID, sort string, q string, cursor string, limit int) ([]IPStat, error) {
	desc := sort == "-total_bytes" || sort == "-last_seen_at"
	col := "total_bytes"
	if sort == "last_seen_at" || sort == "-last_seen_at" {
		col = "last_seen_at"
	}
	dir := "DESC"
	if !desc {
		dir = "ASC"
	}

	where := `node_id = ?`
	args := []any{nodeID}
	if q != "" {
		where += ` AND source_ip LIKE ?`
		args = append(args, q+"%")
	}
	if cursor != "" {
		val, ip, ok := splitCursor(cursor)
		if !ok {
			return nil, errors.New("invalid cursor")
		}
		op := "<"
		if !desc {
			op = ">"
		}
		where += ` AND (` + col + ` ` + op + ` ? OR (` + col + ` = ? AND source_ip ` + op + ` ?))`
		args = append(args, toInt64(val), toInt64(val), ip)
	}

	query := `SELECT source_ip, upload_bytes, download_bytes, upload_bytes + download_bytes AS total_bytes,
		connections_total, active_connections, first_seen_at, last_seen_at
		FROM ip_stats WHERE ` + where + ` ORDER BY ` + col + ` ` + dir + `, source_ip ` + dir + ` LIMIT ?`
	args = append(args, limit)

	rows, err := s.query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IPStat{}
	for rows.Next() {
		var st IPStat
		if err := rows.Scan(&st.SourceIP, &st.UploadBytes, &st.DownloadBytes, &st.TotalBytes, &st.ConnectionsTotal, &st.ActiveConnections, &st.FirstSeenAt, &st.LastSeenAt); err != nil {
			return nil, err
		}
		st.NodeID = nodeID
		out = append(out, st)
	}
	return out, rows.Err()
}

// CountDistinctIPs returns how many different source IPs were ever seen.
func (s *Store) CountDistinctIPs() (int, error) {
	var n int
	err := s.queryRow(`SELECT COUNT(DISTINCT source_ip) FROM ip_stats`).Scan(&n)
	return n, err
}

// CountOnlineIPs returns how many distinct source IPs currently have active
// connections (the sampler refreshes active_connections every round).
func (s *Store) CountOnlineIPs() (int, error) {
	var n int
	err := s.queryRow(`SELECT COUNT(DISTINCT source_ip) FROM ip_stats WHERE active_connections > 0`).Scan(&n)
	return n, err
}

// TopIPsByTraffic aggregates ip_daily across nodes in a day range.
func (s *Store) TopIPsByTraffic(fromDay, toDay int64, limit int) ([]IPStat, error) {
	rows, err := s.query(`SELECT source_ip,
			SUM(up_bytes) AS up, SUM(down_bytes) AS down,
			COUNT(DISTINCT node_id) AS nodes_count
		FROM ip_daily WHERE day >= ? AND day <= ?
		GROUP BY source_ip ORDER BY up + down DESC LIMIT ?`, fromDay, toDay, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IPStat{}
	for rows.Next() {
		var st IPStat
		if err := rows.Scan(&st.SourceIP, &st.UploadBytes, &st.DownloadBytes, &st.NodesCount); err != nil {
			return nil, err
		}
		st.TotalBytes = st.UploadBytes + st.DownloadBytes
		out = append(out, st)
	}
	return out, rows.Err()
}

// GetGeo reads a cached geolocation entry.
func (s *Store) GetGeo(ip string) (*Geo, error) {
	row := s.queryRow(`SELECT ip, country, region, city, as_org, checked_at FROM ip_geo_cache WHERE ip = ?`, ip)
	var g Geo
	err := row.Scan(&g.IP, &g.Country, &g.Region, &g.City, &g.ASOrg, &g.CheckedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// PutGeo caches a geolocation result.
func (s *Store) PutGeo(g Geo) error {
	_, err := s.exec(`INSERT INTO ip_geo_cache (ip, country, region, city, as_org, checked_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(ip) DO UPDATE SET country = excluded.country, region = excluded.region, city = excluded.city, as_org = excluded.as_org, checked_at = excluded.checked_at`, g.IP, g.Country, g.Region, g.City, g.ASOrg, g.CheckedAt)
	return err
}

// dayStart truncates a unix timestamp to its UTC day boundary.
func dayStart(ts int64) int64 {
	return ts - ts%86400
}

// splitCursor decodes "value|ip" cursors.
func splitCursor(cursor string) (value, ip string, ok bool) {
	for i := len(cursor) - 1; i >= 0; i-- {
		if cursor[i] == '|' {
			return cursor[:i], cursor[i+1:], true
		}
	}
	return "", "", false
}

// toInt64 parses a cursor number component.
func toInt64(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
