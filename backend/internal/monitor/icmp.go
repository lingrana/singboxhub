// Package monitor provides ICMP latency monitoring for subscription nodes.
package monitor

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/sing-hub/panel/internal/cryptox"
	"github.com/sing-hub/panel/internal/store"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// Monitor periodically pings subscription nodes and auto-disables high-latency ones.
type Monitor struct {
	store     *store.Store
	logger    *slog.Logger
	cryptoKey []byte

	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	icmpDisabled bool
	mu           sync.Mutex
}

// NewMonitor creates a latency monitor.
func NewMonitor(st *store.Store, cryptoKey []byte, logger *slog.Logger) *Monitor {
	return &Monitor{
		store:     st,
		logger:    logger,
		cryptoKey: cryptoKey,
	}
}

// Start begins the monitoring loop.
func (m *Monitor) Start(ctx context.Context) {
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.wg.Add(1)
	go m.monitorLoop()
}

// Stop cancels the monitoring loop.
func (m *Monitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
}

func (m *Monitor) monitorLoop() {
	defer m.wg.Done()

	// Wait a bit before first check
	select {
	case <-m.ctx.Done():
		return
	case <-time.After(10 * time.Second):
	}

	for {
		settings, err := m.store.GetSettings(store.SamplerDefaults{SamplerIntervalSeconds: 2})
		if err != nil {
			m.logger.Error("load monitor settings", "error", err)
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(60 * time.Second):
				continue
			}
		}

		if !settings.ICMPMonitorEnabled || settings.ICMPMonitorTarget == "" {
			// Monitoring disabled, check again in 1 minute
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(60 * time.Second):
				continue
			}
		}

		interval := time.Duration(settings.ICMPMonitorIntervalSeconds) * time.Second
		if interval < 5*time.Second {
			interval = 5 * time.Second
		}

		m.checkAllNodes(settings)

		select {
		case <-m.ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (m *Monitor) checkAllNodes(settings *store.Settings) {
	nodes, err := m.store.ListSubscriptionNodesWithServer()
	if err != nil {
		m.logger.Error("list nodes for monitoring", "error", err)
		return
	}

	for _, node := range nodes {
		// Decrypt outbound to extract server address
		outboundJSON, err := cryptox.Decrypt(m.cryptoKey, node.Server)
		if err != nil {
			m.logger.Warn("decrypt outbound failed", "node", node.Name, "error", err)
			continue
		}

		var outbound struct {
			Server string `json:"server"`
		}
		if err := json.Unmarshal([]byte(outboundJSON), &outbound); err != nil {
			m.logger.Warn("parse outbound failed", "node", node.Name, "error", err)
			continue
		}

		if outbound.Server == "" {
			continue
		}

		latency, err := m.ping(outbound.Server, settings.ICMPMonitorTarget)
		if err != nil {
			m.logger.Warn("ping failed", "node", node.Name, "server", outbound.Server, "error", err)
			latency = -1
		}

		failCount := node.LatencyFailCount
		if latency < 0 || latency >= int64(settings.ICMPAutoDisableThresholdMs) {
			failCount++
		} else {
			failCount = 0
		}

		// Update latency in database
		if err := m.store.UpdateNodeLatency(node.ID, int(latency), failCount); err != nil {
			m.logger.Error("update node latency", "node", node.Name, "error", err)
			continue
		}

		// Auto-disable if threshold exceeded
		if failCount >= settings.ICMPAutoDisableConsecutive && node.Enabled {
			fullNode, err := m.store.GetNode(node.ID)
			if err != nil {
				m.logger.Error("fetch node for disable", "node", node.Name, "error", err)
				continue
			}
			fullNode.Enabled = false
			if err := m.store.UpdateNode(fullNode); err != nil {
				m.logger.Error("auto-disable node", "node", node.Name, "error", err)
			} else {
				m.logger.Info("auto-disabled node", "node", node.Name, "latency", latency, "fails", failCount)
			}
		}

		// Auto-enable if latency is good and node was auto-disabled
		if latency >= 0 && latency < int64(settings.ICMPAutoDisableThresholdMs) && !node.Enabled && failCount == 0 {
			fullNode, err := m.store.GetNode(node.ID)
			if err != nil {
				m.logger.Error("fetch node for enable", "node", node.Name, "error", err)
				continue
			}
			fullNode.Enabled = true
			if err := m.store.UpdateNode(fullNode); err != nil {
				m.logger.Error("auto-enable node", "node", node.Name, "error", err)
			} else {
				m.logger.Info("auto-enabled node", "node", node.Name, "latency", latency)
			}
		}
	}
}

func (m *Monitor) ping(server, target string) (int64, error) {
	m.mu.Lock()
	disabled := m.icmpDisabled
	m.mu.Unlock()

	if disabled {
		return -1, nil
	}

	// Resolve server to IP
	ips, err := net.LookupIP(server)
	if err != nil || len(ips) == 0 {
		return -1, err
	}

	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		// ICMP requires special permissions - disable monitoring if unavailable
		m.mu.Lock()
		if !m.icmpDisabled {
			m.icmpDisabled = true
			m.logger.Warn("ICMP monitoring disabled: insufficient permissions (requires CAP_NET_RAW or root)", "error", err)
		}
		m.mu.Unlock()
		return -1, nil
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(3 * time.Second))

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   1,
			Seq:  1,
			Data: []byte("singbox-hub-ping"),
		},
	}

	msgBytes, err := msg.Marshal(nil)
	if err != nil {
		return -1, err
	}

	start := time.Now()
	if _, err := conn.WriteTo(msgBytes, &net.IPAddr{IP: ips[0]}); err != nil {
		return -1, err
	}

	reply := make([]byte, 1500)
	n, _, err := conn.ReadFrom(reply)
	if err != nil {
		return -1, err
	}
	duration := time.Since(start)

	parsedMsg, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), reply[:n])
	if err != nil {
		return -1, err
	}

	if parsedMsg.Type == ipv4.ICMPTypeEchoReply {
		return duration.Milliseconds(), nil
	}

	return -1, nil
}
