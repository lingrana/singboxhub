// Command mockclash is a fake sing-box Clash API server for trying the panel
// without real nodes. It serves /version, /configs, /traffic (WS),
// /connections (WS), /proxies, /rules and connection deletion.
//
// Usage: go run ./cmd/mockclash -listen 127.0.0.1:19999 -secret abc
package main

import (
	"encoding/json"
	"flag"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	secret = flag.String("secret", "abc", "bearer secret")
	listen = flag.String("listen", "127.0.0.1:19999", "listen address")
)

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

var (
	mu         sync.Mutex
	upTotal    int64
	downTotal  int64
	activeConn []*connection
	nextID     int
)

type connection struct {
	ID       string `json:"id"`
	Upload   int64  `json:"upload"`
	Download int64  `json:"download"`
	Start    string `json:"start"`
	Chains   []string
	Rule     string
	RulePay  string `json:"rulePayload"`
	Metadata metadata
}

type metadata struct {
	Network         string `json:"network"`
	Type            string `json:"type"`
	SourceIP        string `json:"sourceIP"`
	SourcePort      string `json:"sourcePort"`
	DestinationIP   string `json:"destinationIP"`
	DestinationPort string `json:"destinationPort"`
	Host            string `json:"host"`
	InboundType     string `json:"inboundType"`
}

var sources = []string{"192.168.1.23", "192.168.1.45", "10.8.0.7", "172.16.0.9"}
var hosts = []string{"example.com", "api.github.com", "video.example.net", "cdn.example.org"}

func churn() {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now().Format(time.RFC3339)
	// occasionally add a connection
	if len(activeConn) < 8 && rand.Intn(3) == 0 {
		nextID++
		activeConn = append(activeConn, &connection{
			ID:       "conn-" + strconv.Itoa(nextID),
			Start:    now,
			Chains:   []string{"PROXY", "vless-out"},
			Rule:     "MATCH",
			Metadata: metadata{Network: "tcp", Type: "vless", SourceIP: sources[rand.Intn(len(sources))], SourcePort: "5" + strconv.Itoa(1000+rand.Intn(8999)), Host: hosts[rand.Intn(len(hosts))], DestinationIP: "93.184.216.34", DestinationPort: "443", InboundType: "mixed"},
		})
	}
	// grow traffic, drop some connections
	var kept []*connection
	for _, c := range activeConn {
		up := rand.Int63n(200_000)
		down := rand.Int63n(1_500_000)
		c.Upload += up
		c.Download += down
		upTotal += up
		downTotal += down
		if rand.Intn(10) == 0 {
			continue // this connection closes
		}
		kept = append(kept, c)
	}
	activeConn = kept
}

func authed(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+*secret {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func main() {
	flag.Parse()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"version": "sing-box 1.11.0-mock", "meta": true})
	})

	mux.HandleFunc("GET /configs", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"mode": "rule", "tun": map[string]any{}})
	})

	mux.HandleFunc("GET /proxies", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
			"PROXY":     map[string]any{"name": "PROXY", "type": "Selector", "now": "vless-out", "all": []string{"vless-out", "direct"}},
			"vless-out": map[string]any{"name": "vless-out", "type": "Vless"},
		}})
	})

	mux.HandleFunc("GET /rules", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"rules": []map[string]string{{"type": "MATCH", "payload": "", "proxy": "PROXY"}}})
	})

	mux.HandleFunc("GET /traffic", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			mu.Lock()
			up := rand.Int63n(300_000)
			down := rand.Int63n(2_000_000)
			mu.Unlock()
			if err := ws.WriteJSON(map[string]int64{"up": up, "down": down}); err != nil {
				return
			}
		}
	})

	mux.HandleFunc("GET /connections", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		if r.Header.Get("Upgrade") == "" {
			writeConns(w)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			churn()
			mu.Lock()
			msg := map[string]any{
				"connections":   activeConn,
				"uploadTotal":   upTotal,
				"downloadTotal": downTotal,
			}
			mu.Unlock()
			if err := ws.WriteJSON(msg); err != nil {
				return
			}
		}
	})

	mux.HandleFunc("DELETE /connections", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		mu.Lock()
		activeConn = nil
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("DELETE /connections/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		id := r.PathValue("id")
		mu.Lock()
		var kept []*connection
		for _, c := range activeConn {
			if c.ID != id {
				kept = append(kept, c)
			}
		}
		activeConn = kept
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /memory", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if err := ws.WriteJSON(map[string]int64{"inuse": 30_000_000 + rand.Int63n(5_000_000)}); err != nil {
				return
			}
		}
	})

	log.Println("mockclash listening on", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func writeConns(w http.ResponseWriter) {
	mu.Lock()
	defer mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"connections": activeConn, "uploadTotal": upTotal, "downloadTotal": downTotal})
}
