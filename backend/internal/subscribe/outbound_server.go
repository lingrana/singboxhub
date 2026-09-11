package subscribe

import "fmt"

// ParseOutboundServer extracts server host and port from one sing-box
// outbound JSON, for TCP latency probes without full re-parsing.
func ParseOutboundServer(outboundJSON string) (string, int, error) {
	o, err := parseOutbound(outboundJSON)
	if err != nil {
		return "", 0, err
	}
	if o.Server == "" || o.ServerPort <= 0 {
		return "", 0, fmt.Errorf("outbound %q missing server/port", o.Tag)
	}
	return o.Server, o.ServerPort, nil
}
