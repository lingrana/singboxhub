// Package httpx provides shared HTTP plumbing: RFC 9457 Problem Details
// responses, request-ID propagation, structured access logging, panic
// recovery, and SSRF protection utilities.
package httpx

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AllowedHTTPMethods defines the HTTP methods allowed for SSRF protection.
var AllowedHTTPMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

// IsPrivateIP reports whether ip is a loopback, private, link-local, or metadata address.
func IsPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	// Cloud metadata addresses
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 169 && ip4[1] == 254 {
			return true // link-local (169.254.0.0/16)
		}
	}
	// Docker/container metadata
	if ip.Equal(net.ParseIP("192.0.2.0")) || // TEST-NET-1
		ip.Equal(net.ParseIP("198.51.100.0")) || // TEST-NET-2
		ip.Equal(net.ParseIP("203.0.113.0")) { // TEST-NET-3
		return true
	}
	return false
}

// ValidateOutboundURL checks that a URL is safe for server-side requests:
// - Must be http(s) scheme
// - Must resolve to a non-private IP
// - Must not be a redirect to a private IP
func ValidateOutboundURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}

	if u.User != nil || u.Host != u.Hostname() && strings.Contains(u.Host, "@") {
		return &SSRFError{Detail: "userinfo is not allowed"}
	}
	if u.RawQuery != "" && strings.ContainsAny(u.RawQuery, "\r\n") {
		return &SSRFError{Detail: "invalid URL query"}
	}

	// Only allow http(s) schemes.
	if u.Scheme != "http" && u.Scheme != "https" {
		return &SSRFError{Detail: "only http(s) schemes are allowed"}
	}

	// Resolve hostname and check for private IPs
	hostname := u.Hostname()
	if hostname == "" {
		return &SSRFError{Detail: "hostname is required"}
	}

	// Reject obviously dangerous hostnames
	if isPrivateHostname(hostname) {
		return &SSRFError{Detail: "private/internal hostnames are not allowed"}
	}

	return validateResolvedIPs(hostname)
}

func validateResolvedIPs(hostname string) error {
	if ip := net.ParseIP(hostname); ip != nil {
		if IsPrivateIP(ip) {
			return &SSRFError{Detail: "private/internal IP is not allowed"}
		}
		return nil
	}
	ips, err := net.LookupIP(hostname)
	if err != nil || len(ips) == 0 {
		return &SSRFError{Detail: "failed to resolve hostname"}
	}
	for _, ip := range ips {
		if IsPrivateIP(ip) {
			return &SSRFError{Detail: "hostname resolves to a private/internal IP"}
		}
	}
	return nil
}

// isPrivateHostname checks if a hostname is obviously private/internal.
func isPrivateHostname(hostname string) bool {
	// Localhost variations
	hostname = strings.ToLower(hostname)
	if hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1" {
		return true
	}
	// Metadata services
	if hostname == "169.254.169.254" || hostname == "metadata.google.internal" {
		return true
	}
	// Internal domains
	if strings.HasSuffix(hostname, ".internal") || strings.HasSuffix(hostname, ".local") {
		return true
	}
	return false
}

// SSRFError represents an SSRF protection violation.
type SSRFError struct {
	Detail string
}

func (e *SSRFError) Error() string {
	return e.Detail
}

// SafeHTTPClient creates an HTTP client with SSRF protection.

// SafeHTTPClient returns an HTTP client that validates both redirects and the
// address actually dialed. Resolving and dialing the chosen IP in one place
// prevents a DNS answer from changing between validation and connection.
func SafeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: safeDialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return &SSRFError{Detail: "too many redirects"}
			}
			return ValidateOutboundURL(req.URL.String())
		},
	}
}

func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, &SSRFError{Detail: "invalid outbound address"}
	}
	if err := validateResolvedIPs(host); err != nil {
		return nil, err
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, &SSRFError{Detail: "failed to resolve hostname"}
	}
	var lastErr error
	for _, ip := range ips {
		if IsPrivateIP(ip) {
			continue
		}
		conn, dialErr := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network,
			net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no public address available")
	}
	return nil, lastErr
}

// SafeDo performs an HTTP request with SSRF protection.
func SafeDo(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	if err := ValidateOutboundURL(req.URL.String()); err != nil {
		return nil, err
	}
	if client == nil {
		client = SafeHTTPClient(15 * time.Second)
	}
	clone := *client
	if clone.Transport == nil {
		clone.Transport = (&http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: safeDialContext}).Clone()
	} else {
		transport, ok := clone.Transport.(*http.Transport)
		if !ok {
			return nil, &SSRFError{Detail: "unsafe custom HTTP transport"}
		}
		clonedTransport := transport.Clone()
		clonedTransport.DialContext = safeDialContext
		clone.Transport = clonedTransport
	}
	if clone.CheckRedirect == nil {
		clone.CheckRedirect = SafeHTTPClient(clone.Timeout).CheckRedirect
	}
	return clone.Do(req.WithContext(ctx))
}

// ValidateTCPAddress applies the same public-address policy to a host:port
// extracted from a stored outbound before a direct latency probe.
func ValidateTCPAddress(host string, port int) error {
	if port < 1 || port > 65535 {
		return &SSRFError{Detail: "invalid port"}
	}
	return validateResolvedIPs(strings.Trim(host, "[]"))
}

// TCPAddress formats a validated endpoint for net.Dial.
func TCPAddress(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }
