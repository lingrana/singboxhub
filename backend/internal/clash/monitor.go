package clash

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gorilla/websocket"
)

// Metadata mirrors the Clash connection metadata (subset).
type Metadata struct {
	Network         string `json:"network"`
	Type            string `json:"type"`
	SourceIP        string `json:"sourceIP"`
	SourcePort      string `json:"sourcePort"`
	DestinationIP   string `json:"destinationIP"`
	DestinationPort string `json:"destinationPort"`
	Host            string `json:"host"`
	InboundType     string `json:"inboundType"`
}

// Connection mirrors one Clash tracked connection (subset).
type Connection struct {
	ID       string   `json:"id"`
	Upload   int64    `json:"upload"`
	Download int64    `json:"download"`
	Start    string   `json:"start"`
	Chains   []string `json:"chains"`
	Rule     string   `json:"rule"`
	RulePay  string   `json:"rulePayload"`
	Metadata Metadata `json:"metadata"`
}

// ConnectionsMessage is the payload of GET /connections and the WS stream.
type ConnectionsMessage struct {
	Connections   []Connection `json:"connections"`
	UploadTotal   int64        `json:"uploadTotal"`
	DownloadTotal int64        `json:"downloadTotal"`
}

// ConnectionsSnapshot fetches the current connections. Newer sing-box builds
// answer plain GET; otherwise we read one frame from the WS stream.
func (c *Client) ConnectionsSnapshot(ctx context.Context) (*ConnectionsMessage, error) {
	status, data, err := c.do(ctx, http.MethodGet, "/connections", nil)
	if err == nil && status == http.StatusOK {
		var msg ConnectionsMessage
		if jsonErr := json.Unmarshal(data, &msg); jsonErr == nil {
			return &msg, nil
		}
	}
	// Fall back to one WS frame.
	conn, resp, dialErr := c.DialWS(ctx, "/connections")
	if dialErr != nil {
		if resp != nil {
			return nil, &Error{StatusCode: resp.StatusCode, Detail: dialErr.Error()}
		}
		return nil, &Error{Detail: dialErr.Error()}
	}
	defer conn.Close()
	var msg ConnectionsMessage
	if readErr := conn.ReadJSON(&msg); readErr != nil {
		return nil, &Error{Detail: "read /connections frame: " + readErr.Error()}
	}
	return &msg, nil
}

// MemoryMessage is one frame of GET /memory.
type MemoryMessage struct {
	Inuse       int64 `json:"inuse"`
	OSLimit     int64 `json:"oslimit,omitempty"`
	HeapObjects int64 `json:"heapobjects,omitempty"`
}

// MemorySnapshot reads one /memory frame (WS-only endpoint).
func (c *Client) MemorySnapshot(ctx context.Context) (*MemoryMessage, error) {
	conn, resp, err := c.DialWS(ctx, "/memory")
	if err != nil {
		if resp != nil {
			return nil, &Error{StatusCode: resp.StatusCode, Detail: err.Error()}
		}
		return nil, &Error{Detail: err.Error()}
	}
	defer conn.Close()
	var msg MemoryMessage
	if readErr := conn.ReadJSON(&msg); readErr != nil {
		if websocket.IsCloseError(readErr, websocket.CloseNormalClosure) {
			return nil, &Error{Detail: "/memory closed early"}
		}
		return nil, &Error{Detail: "read /memory frame: " + readErr.Error()}
	}
	return &msg, nil
}
