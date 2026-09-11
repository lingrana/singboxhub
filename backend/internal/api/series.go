package api

import (
	"log/slog"
	"net/http"
	"sort"

	"github.com/sing-hub/panel/internal/httpx"
)

// handleStatsTrafficSeries implements GET /stats/traffic-series: minute/hour
// rollups aggregated across every node, ordered oldest→newest for charting.
func (s *Server) handleStatsTrafficSeries(w http.ResponseWriter, r *http.Request) {
	interval := r.URL.Query().Get("interval")
	if interval == "" {
		interval = "minute"
	}
	if interval != "minute" && interval != "hour" && interval != "day" {
		httpx.Validation(w, r, "interval must be minute|hour|day",
			httpx.FieldErr("#interval", "one of minute, hour, day"))
		return
	}
	from, to, ok := parseTimeRange(r, interval)
	if !ok {
		httpx.Validation(w, r, "from/to must be RFC 3339 and from <= to")
		return
	}

	nodes, err := s.store.ListNodes()
	if err != nil {
		s.logger.Error("series nodes failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}

	type bucket struct {
		up, down, peakUp, peakDown, maxConn int64
	}
	agg := map[int64]*bucket{}
	for i := range nodes {
		rows, err := s.store.QueryRollupAsc(nodes[i].ID, interval, from, to, 10000)
		if err != nil {
			s.logger.Error("series query failed", slog.Any("error", err))
			httpx.Internal(w, r)
			return
		}
		for _, row := range rows {
			b, exists := agg[row.BucketStart]
			if !exists {
				b = &bucket{}
				agg[row.BucketStart] = b
			}
			b.up += row.UpBytes
			b.down += row.DownBytes
			if row.PeakUpBps > b.peakUp {
				b.peakUp = row.PeakUpBps
			}
			if row.PeakDownBps > b.peakDown {
				b.peakDown = row.PeakDownBps
			}
			if int64(row.MaxConnections) > b.maxConn {
				b.maxConn = int64(row.MaxConnections)
			}
		}
	}

	stamps := make([]int64, 0, len(agg))
	for ts := range agg {
		stamps = append(stamps, ts)
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i] < stamps[j] })

	items := make([]map[string]any, 0, len(stamps))
	for _, ts := range stamps {
		b := agg[ts]
		items = append(items, map[string]any{
			"bucket_start":    formatRFC3339(ts),
			"interval":        interval,
			"up_bytes":        b.up,
			"down_bytes":      b.down,
			"peak_up_bps":     b.peakUp,
			"peak_down_bps":   b.peakDown,
			"max_connections": b.maxConn,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
