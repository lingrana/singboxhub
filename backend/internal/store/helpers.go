package store

import (
	"database/sql"
	"strings"
)

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func splitCSV(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// requireAffected converts a zero affected-row count into ErrNotFound so
// UPDATE/DELETE callers can distinguish "missing" from "no-op".
func requireAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
