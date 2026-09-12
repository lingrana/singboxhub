package store

import "time"

// UpdateNodeLatency updates the latency fields for a node.
func (s *Store) UpdateNodeLatency(nodeID string, latencyMs int, failCount int) error {
	now := time.Now().Unix()
	_, err := s.exec(
		`UPDATE nodes SET latency_ms = ?, latency_checked_at = ?, latency_fail_count = ?, updated_at = ? WHERE id = ?`,
		latencyMs, now, failCount, now, nodeID)
	return err
}

// ListEnabledSubscriptionNodes returns all enabled nodes with non-empty source (imported nodes).
func (s *Store) ListEnabledSubscriptionNodes() ([]Node, error) {
	rows, err := s.query(`SELECT `+nodeColumns+` FROM nodes WHERE enabled = 1 AND source != '' ORDER BY name`)
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

// ListSubscriptionNodesWithServer returns server addresses for ICMP monitoring.
func (s *Store) ListSubscriptionNodesWithServer() ([]ServerNode, error) {
	// Use JSON extraction to get server field from outbound_enc after decryption
	// Since we can't decrypt in SQL, we'll return nodes with outbound_enc and let caller decrypt
	query := `
		SELECT id, name, outbound_enc, enabled, latency_ms, latency_fail_count
		FROM nodes
		WHERE enabled = 1 AND source != '' AND outbound_enc != ''
		ORDER BY name
	`
	rows, err := s.query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ServerNode
	for rows.Next() {
		var n ServerNode
		var outboundEnc string
		if err := rows.Scan(&n.ID, &n.Name, &outboundEnc, &n.Enabled, &n.LatencyMs, &n.LatencyFailCount); err != nil {
			return nil, err
		}
		// Store encrypted outbound in Server field temporarily - caller will decrypt and parse
		n.Server = outboundEnc
		out = append(out, n)
	}
	return out, rows.Err()
}
