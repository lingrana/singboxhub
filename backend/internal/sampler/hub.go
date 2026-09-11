// Package sampler keeps one WebSocket worker pair per enabled node: /traffic
// for realtime rates and /connections for per-source-IP accounting. Samples
// are flushed to the store every persist interval.
package sampler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/sing-hub/panel/internal/cryptox"
	"github.com/sing-hub/panel/internal/store"
)

// LiveStatus is the in-memory realtime state of one node.
type LiveStatus struct {
	Online            bool   `json:"online"`
	Version           string `json:"version,omitempty"`
	Mode              string `json:"mode,omitempty"`
	UpBPS             int64  `json:"up_bps"`
	DownBPS           int64  `json:"down_bps"`
	MemoryBytes       int64  `json:"memory_bytes,omitempty"`
	HasMemory         bool   `json:"-"`
	ActiveConnections int    `json:"active_connections"`
	LastError         string `json:"last_error,omitempty"`
	LastOnlineAt      int64  `json:"last_online_at,omitempty"`
}

// Hub owns all node workers and the flush loop.
type Hub struct {
	store  *store.Store
	key    []byte
	logger *slog.Logger

	persistInterval time.Duration

	mu      sync.RWMutex
	workers map[string]*Worker

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewHub builds a hub; call Start to begin sampling.
func NewHub(st *store.Store, key []byte, logger *slog.Logger, persistInterval time.Duration) *Hub {
	return &Hub{
		store:           st,
		key:             key,
		logger:          logger,
		persistInterval: persistInterval,
		workers:         map[string]*Worker{},
	}
}

// Start begins sampling all enabled nodes and the flush loop.
func (h *Hub) Start(ctx context.Context) {
	h.ctx, h.cancel = context.WithCancel(ctx)
	if err := h.store.ClearActiveIPs(""); err != nil {
		h.logger.Error("clear stale active IPs", "error", err)
	}
	h.maintain(time.Now().Unix())
	if err := h.Reload(); err != nil {
		h.logger.Error("initial node load failed", slog.Any("error", err))
	}
	h.wg.Add(2)
	go h.flushLoop()
	go h.maintenanceLoop()
}

// Stop cancels all workers and waits for them.
func (h *Hub) Stop() {
	h.mu.Lock()
	if h.cancel != nil {
		h.cancel()
	}
	for id, w := range h.workers {
		w.Stop()
		h.flushWorker(w, time.Now().Unix())
		delete(h.workers, id)
	}
	if err := h.store.ClearActiveIPs(""); err != nil {
		h.logger.Error("clear active IPs on shutdown", "error", err)
	}
	h.mu.Unlock()
	h.wg.Wait()
}

// Reload re-syncs the worker set with enabled nodes (diffing, not restarting
// unchanged workers). Call after node CRUD or settings changes.
func (h *Hub) Reload() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx == nil || h.ctx.Err() != nil {
		return nil
	}
	settings, err := h.settings()
	if err != nil {
		return err
	}
	connInterval := time.Duration(settings.SamplerIntervalSeconds) * time.Second
	nodes, err := h.store.ListNodes()
	if err != nil {
		return err
	}
	desired := map[string]*store.Node{}
	for i := range nodes {
		n := &nodes[i]
		// Nodes imported from upstream subscriptions have no Clash API;
		// there is nothing to sample, so never start workers for them.
		if n.Enabled && n.APIURL != "" {
			desired[n.ID] = n
		}
	}

	for id, w := range h.workers {
		want, ok := desired[id]
		if !ok || want.APIURL != w.apiURL || want.APISecretEnc != w.apiSecretEnc {
			w.Stop()
			if _, err := h.store.GetNode(id); err == nil {
				h.flushWorker(w, time.Now().Unix())
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err := h.store.ClearActiveIPs(id); err != nil {
				return err
			}
			delete(h.workers, id)
		} else if w.connInterval != connInterval {
			w.Stop()
			h.flushWorker(w, time.Now().Unix())
			w.connInterval = connInterval
			w.Start(h.ctx, &h.wg)
		}
	}
	for id, n := range desired {
		if _, running := h.workers[id]; running {
			continue
		}
		secret, err := cryptox.Decrypt(h.key, n.APISecretEnc)
		if err != nil {
			h.logger.Error("decrypt node secret failed", slog.String("node", n.Name), slog.Any("error", err))
			continue
		}
		w := newWorker(n.ID, n.Name, n.APIURL, n.APISecretEnc, secret, h.logger)
		w.connInterval = connInterval
		h.workers[id] = w
		w.Start(h.ctx, &h.wg)
	}
	return nil
}

// Status returns the live status of one node.
func (h *Hub) Status(nodeID string) (LiveStatus, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	w, ok := h.workers[nodeID]
	if !ok {
		return LiveStatus{}, false
	}
	return w.Snapshot(), true
}

// SetNodeMode updates the cached runtime mode after a control action.
func (h *Hub) SetNodeMode(nodeID, mode string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if w, ok := h.workers[nodeID]; ok {
		w.SetMode(mode)
	}
}

// Aggregate sums live rates across all workers.
func (h *Hub) Aggregate() (upBPS, downBPS int64, onlineCount, totalWorkers int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, w := range h.workers {
		st := w.Snapshot()
		totalWorkers++
		if st.Online {
			onlineCount++
			upBPS += st.UpBPS
			downBPS += st.DownBPS
		}
	}
	return upBPS, downBPS, onlineCount, totalWorkers
}

func (h *Hub) flushLoop() {
	defer h.wg.Done()
	ticker := time.NewTicker(h.persistInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			h.flushOnce()
		}
	}
}

func (h *Hub) flushOnce() {
	now := time.Now().Unix()
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, w := range h.workers {
		h.flushWorker(w, now)
	}
}

func (h *Hub) flushWorker(w *Worker, now int64) {
	samples, rollups, ipDeltas := w.Drain(now)
	if w.Snapshot().Online {
		if err := h.store.SetLastOnline(w.nodeID, now); err != nil {
			h.logger.Error("update last online failed", "node", w.nodeID, "error", err)
		}
	}
	if len(samples) == 0 && len(ipDeltas) == 0 {
		return
	}
	if err := h.store.InsertTrafficSamples(samples); err != nil {
		h.logger.Error("flush samples failed", slog.String("node", w.nodeID), slog.Any("error", err))
	}
	if err := h.store.UpsertRollups(rollups); err != nil {
		h.logger.Error("flush rollups failed", slog.String("node", w.nodeID), slog.Any("error", err))
	}
	if err := h.store.UpsertIPStats(ipDeltas); err != nil {
		h.logger.Error("flush ip stats failed", slog.String("node", w.nodeID), slog.Any("error", err))
	}
}

func (h *Hub) settings() (*store.Settings, error) {
	return h.store.GetSettings(store.SamplerDefaults{SamplerIntervalSeconds: 2, RetentionDays: 30})
}

func (h *Hub) maintain(now int64) {
	settings, err := h.settings()
	if err != nil {
		h.logger.Error("load retention settings", "error", err)
	} else if err := h.store.PruneTraffic(now, settings.RetentionDays); err != nil {
		h.logger.Error("prune traffic history", "error", err)
	}
	if err := h.store.DeleteExpiredTokens(now); err != nil {
		h.logger.Error("prune expired sessions", "error", err)
	}
}

func (h *Hub) maintenanceLoop() {
	defer h.wg.Done()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case now := <-ticker.C:
			h.maintain(now.Unix())
		}
	}
}
