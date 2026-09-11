package store

import "database/sql"

// TrafficSample is one raw sampler row (persist_interval granularity).
type TrafficSample struct {
	NodeID         string
	Ts             int64
	UpBytes        int64
	DownBytes      int64
	PeakUpBps      int64
	PeakDownBps    int64
	MaxConnections int
}

// RollupRow is one aggregated bucket (minute/hour/day).
type RollupRow struct {
	NodeID         string
	Interval       string
	BucketStart    int64
	UpBytes        int64
	DownBytes      int64
	PeakUpBps      int64
	PeakDownBps    int64
	MaxConnections int
}

// InsertTrafficSamples upserts raw samples for one persist batch.
func (s *Store) InsertTrafficSamples(samples []TrafficSample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := s.txPrepare(tx, `INSERT INTO traffic_samples
		(node_id, ts, up_bytes, down_bytes, peak_up_bps, peak_down_bps, max_connections)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id, ts) DO UPDATE SET
		  up_bytes = up_bytes + excluded.up_bytes,
		  down_bytes = down_bytes + excluded.down_bytes,
		  peak_up_bps = MAX(peak_up_bps, excluded.peak_up_bps),
		  peak_down_bps = MAX(peak_down_bps, excluded.peak_down_bps),
		  max_connections = MAX(max_connections, excluded.max_connections)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range samples {
		if _, err := stmt.Exec(m.NodeID, m.Ts, m.UpBytes, m.DownBytes, m.PeakUpBps, m.PeakDownBps, m.MaxConnections); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpsertRollups accumulates deltas into rollup buckets; peaks take the max.
func (s *Store) UpsertRollups(rows []RollupRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := s.txPrepare(tx, `INSERT INTO traffic_rollup
		(node_id, interval, bucket_start, up_bytes, down_bytes, peak_up_bps, peak_down_bps, max_connections)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id, interval, bucket_start) DO UPDATE SET
		  up_bytes        = up_bytes + excluded.up_bytes,
		  down_bytes      = down_bytes + excluded.down_bytes,
		  peak_up_bps     = MAX(peak_up_bps, excluded.peak_up_bps),
		  peak_down_bps   = MAX(peak_down_bps, excluded.peak_down_bps),
		  max_connections = MAX(max_connections, excluded.max_connections)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.Exec(r.NodeID, r.Interval, r.BucketStart, r.UpBytes, r.DownBytes, r.PeakUpBps, r.PeakDownBps, r.MaxConnections); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// QueryRollup returns one node's buckets in [from,to], newest first.
// afterTs is the keyset cursor (0 = start).
func (s *Store) QueryRollup(nodeID, interval string, from, to, afterTs int64, limit int) ([]RollupRow, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if afterTs > 0 {
		rows, err = s.query(`SELECT bucket_start, up_bytes, down_bytes, peak_up_bps, peak_down_bps, max_connections
			FROM traffic_rollup
			WHERE node_id = ? AND interval = ? AND bucket_start >= ? AND bucket_start <= ? AND bucket_start < ?
			ORDER BY bucket_start DESC LIMIT ?`,
			nodeID, interval, from, to, afterTs, limit)
	} else {
		rows, err = s.query(`SELECT bucket_start, up_bytes, down_bytes, peak_up_bps, peak_down_bps, max_connections
			FROM traffic_rollup
			WHERE node_id = ? AND interval = ? AND bucket_start >= ? AND bucket_start <= ?
			ORDER BY bucket_start DESC LIMIT ?`,
			nodeID, interval, from, to, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RollupRow{}
	for rows.Next() {
		var r RollupRow
		if err := rows.Scan(&r.BucketStart, &r.UpBytes, &r.DownBytes, &r.PeakUpBps, &r.PeakDownBps, &r.MaxConnections); err != nil {
			return nil, err
		}
		r.NodeID = nodeID
		r.Interval = interval
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueryRollupAsc returns one node's buckets in [from,to], oldest first
// (chart order). afterTs is ignored; pass 0.
func (s *Store) QueryRollupAsc(nodeID, interval string, from, to, limit int64) ([]RollupRow, error) {
	rows, err := s.query(`SELECT bucket_start, up_bytes, down_bytes, peak_up_bps, peak_down_bps, max_connections
		FROM traffic_rollup
		WHERE node_id = ? AND interval = ? AND bucket_start >= ? AND bucket_start <= ?
		ORDER BY bucket_start ASC LIMIT ?`,
		nodeID, interval, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RollupRow{}
	for rows.Next() {
		var r RollupRow
		if err := rows.Scan(&r.BucketStart, &r.UpBytes, &r.DownBytes, &r.PeakUpBps, &r.PeakDownBps, &r.MaxConnections); err != nil {
			return nil, err
		}
		r.NodeID = nodeID
		r.Interval = interval
		out = append(out, r)
	}
	return out, rows.Err()
}

// SumRange totals traffic over minute rollups for one node in [from,to].
func (s *Store) SumRange(nodeID string, from, to int64) (up, down int64, err error) {
	err = s.queryRow(`SELECT COALESCE(SUM(up_bytes),0), COALESCE(SUM(down_bytes),0)
		FROM traffic_rollup WHERE node_id = ? AND interval = 'minute' AND bucket_start >= ? AND bucket_start <= ?`,
		nodeID, from, to).Scan(&up, &down)
	return up, down, err
}

// SumAll returns total traffic for a node across all time.
func (s *Store) SumAll(nodeID string) (up, down int64, err error) {
	err = s.queryRow(`SELECT COALESCE(SUM(up_bytes),0), COALESCE(SUM(down_bytes),0)
		FROM traffic_rollup WHERE node_id = ? AND interval = 'minute'`,
		nodeID).Scan(&up, &down)
	return up, down, err
}

// TopNodesByTraffic ranks nodes by total bytes in [from,to].
func (s *Store) TopNodesByTraffic(from, to int64, limit int) ([]RollupRow, error) {
	rows, err := s.query(`SELECT node_id,
			SUM(up_bytes) AS up, SUM(down_bytes) AS down
		FROM traffic_rollup
		WHERE interval = 'minute' AND bucket_start >= ? AND bucket_start <= ?
		GROUP BY node_id ORDER BY up + down DESC LIMIT ?`, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RollupRow{}
	for rows.Next() {
		var r RollupRow
		if err := rows.Scan(&r.NodeID, &r.UpBytes, &r.DownBytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneTraffic removes expired raw samples and rollups.
// samples: newest first 3 days; minute/hour: retentionDays; day: 365.
func (s *Store) PruneTraffic(now int64, retentionDays int) error {
	day := int64(86400)
	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM traffic_samples WHERE ts < ?`, []any{now - 3*day}},
		{`DELETE FROM traffic_rollup WHERE interval = 'minute' AND bucket_start < ?`, []any{now - int64(retentionDays)*day}},
		{`DELETE FROM traffic_rollup WHERE interval = 'hour' AND bucket_start < ?`, []any{now - int64(retentionDays)*day}},
		{`DELETE FROM traffic_rollup WHERE interval = 'day' AND bucket_start < ?`, []any{now - 365*day}},
		{`DELETE FROM ip_daily WHERE day < ?`, []any{now - 365*day}},
		{`DELETE FROM operations WHERE updated_at < ?`, []any{now - 7*day}},
		{`DELETE FROM ip_geo_cache WHERE checked_at < ?`, []any{now - 30*day}},
	}
	for _, st := range statements {
		if _, err := s.exec(st.query, st.args...); err != nil {
			return err
		}
	}
	return nil
}
