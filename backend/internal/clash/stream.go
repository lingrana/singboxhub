package clash

import (
	"context"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

// TrafficMessage is one frame of GET /traffic: bytes per second.
// Note: These values are in bytes/sec, not bits/sec as described in the OpenAPI contract.
// The contract should be updated to reflect the actual implementation.
type TrafficMessage struct {
	Up   int64 `json:"up_bytes_per_second"`
	Down int64 `json:"down_bytes_per_second"`
}

// TrafficStream is a live /traffic WebSocket reader.
type TrafficStream struct {
	conn *websocket.Conn
}

// StreamTraffic opens the /traffic WebSocket. Frames arrive roughly once per
// second; Close the stream when done.
func (c *Client) StreamTraffic(ctx context.Context) (*TrafficStream, error) {
	conn, resp, err := c.DialWS(ctx, "/traffic")
	if err != nil {
		if resp != nil {
			return nil, &Error{StatusCode: resp.StatusCode, Detail: err.Error()}
		}
		return nil, &Error{Detail: err.Error()}
	}
	return &TrafficStream{conn: conn}, nil
}

// Read waits for the next traffic frame.
func (t *TrafficStream) Read() (TrafficMessage, error) {
	var msg TrafficMessage
	err := t.conn.ReadJSON(&msg)
	return msg, err
}

// Close shuts the underlying WebSocket.
func (t *TrafficStream) Close() error { return t.conn.Close() }

// ConnectionsStream is a live /connections WebSocket reader; each frame is a
// full snapshot of the tracked connections.
type ConnectionsStream struct {
	conn *websocket.Conn
}

// StreamConnections opens the /connections WebSocket.
func (c *Client) StreamConnections(ctx context.Context, interval time.Duration) (*ConnectionsStream, error) {
	query := url.Values{"interval": {strconv.FormatInt(interval.Milliseconds(), 10)}}
	conn, resp, err := c.DialWS(ctx, "/connections?"+query.Encode())
	if err != nil {
		if resp != nil {
			return nil, &Error{StatusCode: resp.StatusCode, Detail: err.Error()}
		}
		return nil, &Error{Detail: err.Error()}
	}
	return &ConnectionsStream{conn: conn}, nil
}

// Read waits for the next connections snapshot.
func (s *ConnectionsStream) Read() (ConnectionsMessage, error) {
	var msg ConnectionsMessage
	err := s.conn.ReadJSON(&msg)
	return msg, err
}

// Close shuts the underlying WebSocket.
func (s *ConnectionsStream) Close() error { return s.conn.Close() }
