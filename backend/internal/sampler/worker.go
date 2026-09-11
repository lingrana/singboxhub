package sampler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sing-hub/panel/internal/clash"
	"github.com/sing-hub/panel/internal/store"
)

const (
	maxBackoff     = 30 * time.Second
	initialBackoff = time.Second
)

// Worker supervises the two sampling WebSockets of one node, maintaining
// live state and per-source-IP accumulators between flushes.
type Worker struct {
	nodeID       string
	name         string
	apiURL       string
	apiSecretEnc string
	client       *clash.Client
	logger       *slog.Logger
	connInterval time.Duration

	cancel context.CancelFunc
	done   chan struct{}

	mu          sync.Mutex
	state       LiveStatus
	perIPActive map[string]int
	lastSeen    map[string]connMark
	ipAcc       map[string]*ipAccum
	sumUp       int64
	sumDown     int64
	peakUp      int64
	peakDown    int64
	maxConn     int
}

type connMark struct {
	up, down int64
	ip       string
}

type ipAccum struct {
	up, down  int64
	connCount int
}

func newWorker(id, name, apiURL, apiSecretEnc, secret string, logger *slog.Logger) *Worker {
	client, err := clash.New(apiURL, secret)
	if err != nil {
		// api_url was validated at creation; treat as offline worker.
		logger.Error("invalid node api_url", slog.String("node", name), slog.Any("error", err))
	}
	return &Worker{
		nodeID:       id,
		name:         name,
		apiURL:       apiURL,
		apiSecretEnc: apiSecretEnc,
		client:       client,
		logger:       logger.With(slog.String("node", name)),
		connInterval: 2 * time.Second,
		perIPActive:  map[string]int{},
		lastSeen:     map[string]connMark{},
		ipAcc:        map[string]*ipAccum{},
	}
}

// Start launches the supervision loop until the hub context ends or Stop.
func (w *Worker) Start(parent context.Context, wg *sync.WaitGroup) {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	w.done = make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(w.done)
		defer cancel()
		defer w.markOffline(nil)
		backoff := initialBackoff
		for ctx.Err() == nil {
			if err := w.session(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				w.markOffline(err)
				w.logger.Warn("node session ended", slog.Any("error", err), slog.Duration("retry_in", backoff))
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < maxBackoff {
					backoff *= 2
				}
				continue
			}
			backoff = initialBackoff
		}
	}()
}

// Stop terminates the worker loops.
func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
		<-w.done
	}
}

// Snapshot returns a copy of the live status.
func (w *Worker) Snapshot() LiveStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.state
	st.ActiveConnections = w.activeConnections()
	return st
}

// SetMode updates the cached runtime mode after a control action.
func (w *Worker) SetMode(mode string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state.Mode = mode
}

// Drain returns and clears accumulated samples, rollup deltas and IP deltas.
func (w *Worker) Drain(now int64) ([]store.TrafficSample, []store.RollupRow, []store.IPDelta) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.sumUp == 0 && w.sumDown == 0 && len(w.ipAcc) == 0 && len(w.perIPActive) == 0 {
		return nil, nil, nil
	}

	sample := store.TrafficSample{
		NodeID: w.nodeID, Ts: now,
		UpBytes: w.sumUp, DownBytes: w.sumDown,
		PeakUpBps: w.peakUp, PeakDownBps: w.peakDown,
		MaxConnections: w.maxConn,
	}
	rollups := []store.RollupRow{
		w.rollup("minute", now-now%60),
		w.rollup("hour", now-now%3600),
		w.rollup("day", now-now%86400),
	}

	deltas := make([]store.IPDelta, 0, len(w.ipAcc))
	ips := w.mergeActiveIPs()
	for ip, acc := range ips {
		active := w.perIPActive[ip]
		if acc == nil {
			acc = &ipAccum{}
		}
		deltas = append(deltas, store.IPDelta{
			NodeID: w.nodeID, SourceIP: ip,
			UpDelta: acc.up, DownDelta: acc.down,
			ConnCount: acc.connCount, Active: active, SeenAt: now,
		})
	}

	w.sumUp, w.sumDown = 0, 0
	w.peakUp, w.peakDown = 0, 0
	w.maxConn = w.activeConnections()
	w.ipAcc = map[string]*ipAccum{}
	return []store.TrafficSample{sample}, rollups, deltas
}

// activeConnections is called with w.mu held. Counter baselines survive reconnects.
func (w *Worker) activeConnections() int {
	total := 0
	for _, n := range w.perIPActive {
		total += n
	}
	return total
}

func (w *Worker) rollup(interval string, bucketStart int64) store.RollupRow {
	return store.RollupRow{
		NodeID: w.nodeID, Interval: interval, BucketStart: bucketStart,
		UpBytes: w.sumUp, DownBytes: w.sumDown,
		PeakUpBps: w.peakUp, PeakDownBps: w.peakDown,
		MaxConnections: w.maxConn,
	}
}

// mergeActiveIPs unions accumulator keys with currently active IPs.
func (w *Worker) mergeActiveIPs() map[string]*ipAccum {
	out := make(map[string]*ipAccum, len(w.ipAcc)+len(w.perIPActive))
	for ip, acc := range w.ipAcc {
		out[ip] = acc
	}
	for ip := range w.perIPActive {
		if _, ok := out[ip]; !ok {
			out[ip] = nil
		}
	}
	return out
}

// session performs one connect-to-failure cycle for both streams.
func (w *Worker) session(ctx context.Context) error {
	if w.client == nil {
		return context.Canceled
	}
	vres, err := w.client.Version(ctx)
	if err != nil {
		return err
	}
	cfg, err := w.client.Configs(ctx)
	if err != nil {
		return err
	}
	mem, _ := w.client.MemorySnapshot(ctx)
	w.markOnline(vres.Version, cfg.Mode, mem)

	traffic, err := w.client.StreamTraffic(ctx)
	if err != nil {
		return err
	}
	defer traffic.Close()
	conns, err := w.client.StreamConnections(ctx, w.connInterval)
	if err != nil {
		return err
	}
	defer conns.Close()

	// Unblock blocked readers when the session ends.
	unblock := make(chan struct{})
	defer close(unblock)
	go func() {
		select {
		case <-ctx.Done():
			_ = traffic.Close()
			_ = conns.Close()
		case <-unblock:
		}
	}()

	errCh := make(chan error, 2)
	go func() { errCh <- w.readTraffic(traffic) }()
	go func() { errCh <- w.readConnections(conns) }()

	var sessionErr error
	remaining := 2
	select {
	case <-ctx.Done():
		sessionErr = ctx.Err()
	case err := <-errCh:
		sessionErr = err
		remaining--
	}
	// Wait for both readers before clearing state or starting another session.
	_ = traffic.Close()
	_ = conns.Close()
	for range remaining {
		<-errCh
	}
	return sessionErr
}

func (w *Worker) readTraffic(t *clash.TrafficStream) error {
	for {
		msg, err := t.Read()
		if err != nil {
			return err
		}
		w.accumulateTraffic(msg)
	}
}

func (w *Worker) readConnections(s *clash.ConnectionsStream) error {
	for {
		msg, err := s.Read()
		if err != nil {
			return err
		}
		w.applySnapshot(msg.Connections)
	}
}

func (w *Worker) accumulateTraffic(msg clash.TrafficMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state.UpBPS = msg.Up
	w.state.DownBPS = msg.Down
	w.sumUp += msg.Up
	w.sumDown += msg.Down
	if msg.Up > w.peakUp {
		w.peakUp = msg.Up
	}
	if msg.Down > w.peakDown {
		w.peakDown = msg.Down
	}
}

// applySnapshot diffs one /connections frame into per-IP accumulators.
// Bytes of connections that already closed are counted at their last sight
// (best-effort, see plan assumption A4).
func (w *Worker) applySnapshot(conns []clash.Connection) {
	w.mu.Lock()
	defer w.mu.Unlock()

	cur := make(map[string]connMark, len(conns))
	perIP := make(map[string]int, len(conns))
	for _, c := range conns {
		ip := c.Metadata.SourceIP
		if ip == "" {
			continue
		}
		cur[c.ID] = connMark{up: c.Upload, down: c.Download, ip: ip}
		perIP[ip]++

		last, seen := w.lastSeen[c.ID]
		if !seen {
			acc := w.accFor(ip)
			acc.connCount++
			acc.up += c.Upload
			acc.down += c.Download
			continue
		}
		dUp, dDown := c.Upload-last.up, c.Download-last.down
		if dUp < 0 {
			dUp = c.Upload
		}
		if dDown < 0 {
			dDown = c.Download
		}
		if dUp > 0 || dDown > 0 {
			acc := w.accFor(ip)
			acc.up += dUp
			acc.down += dDown
		}
	}
	// Persist zero-active transitions even when no more bytes were transferred.
	for ip, count := range w.perIPActive {
		if perIP[ip] != count {
			w.accFor(ip)
		}
	}
	w.lastSeen = cur
	w.perIPActive = perIP
	if len(conns) > w.maxConn {
		w.maxConn = len(conns)
	}
}

func (w *Worker) accFor(ip string) *ipAccum {
	acc, ok := w.ipAcc[ip]
	if !ok {
		acc = &ipAccum{}
		w.ipAcc[ip] = acc
	}
	return acc
}

func (w *Worker) markOnline(version, mode string, mem *clash.MemoryMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state.Online = true
	w.state.Version = version
	w.state.Mode = mode
	w.state.LastError = ""
	w.state.LastOnlineAt = time.Now().Unix()
	if mem != nil {
		w.state.MemoryBytes = mem.Inuse
		w.state.HasMemory = true
	}
}

func (w *Worker) markOffline(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state.Online = false
	w.state.UpBPS = 0
	w.state.DownBPS = 0
	if err != nil {
		w.state.LastError = err.Error()
	}
	for ip := range w.perIPActive {
		w.accFor(ip)
	}
	w.perIPActive = map[string]int{}
}
