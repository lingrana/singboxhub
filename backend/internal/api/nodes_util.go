package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sing-hub/panel/internal/clash"
	"github.com/sing-hub/panel/internal/cryptox"
	"github.com/sing-hub/panel/internal/httpx"
	"github.com/sing-hub/panel/internal/store"
)

// newUUID returns a RFC 4122 v4 UUID string.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// isUniqueViolation reports SQLite UNIQUE constraint failures.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// validateNodeInput checks required fields and URL shape of a node payload.
func validateNodeInput(input *struct {
	Name         string   `json:"name"`
	APIURL       string   `json:"api_url"`
	APISecret    string   `json:"api_secret"`
	OutboundJSON *string  `json:"outbound_json"`
	ConfigURL    string   `json:"config_url"`
	KCEKey       string   `json:"kce_key"`
	Tags         []string `json:"tags"`
	Enabled      *bool    `json:"enabled"`
	Remark       string   `json:"remark"`
}, forCreate bool) []httpx.ValidationError {
	var errs []httpx.ValidationError
	if forCreate || input.Name != "" {
		name := strings.TrimSpace(input.Name)
		if name == "" || len(name) > 64 {
			errs = append(errs, httpx.FieldErr("#/name", "required, 1..64 characters"))
		}
	}
	if forCreate || input.APIURL != "" {
		u := strings.TrimRight(input.APIURL, "/")
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			errs = append(errs, httpx.FieldErr("#/api_url", "must be an http(s) URL of the node Clash API"))
		}
	}
	if forCreate && strings.TrimSpace(input.APISecret) == "" {
		errs = append(errs, httpx.FieldErr("#/api_secret", "required"))
	}
	if input.OutboundJSON != nil && strings.TrimSpace(*input.OutboundJSON) != "" {
		if !json.Valid([]byte(*input.OutboundJSON)) {
			errs = append(errs, httpx.FieldErr("#/outbound_json", "must be a valid JSON object"))
		}
	}
	if u := strings.TrimRight(strings.TrimSpace(input.ConfigURL), "/"); u != "" {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			errs = append(errs, httpx.FieldErr("#/config_url", "must be an http(s) URL of the encrypted config"))
		}
	}
	return errs
}

// applyNodePatch merges a merge-patch body into a node entity, encrypting
// secrets as needed.
func applyNodePatch(node *store.Node, patch map[string]json.RawMessage, key []byte) error {
	if v, ok := patch["name"]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &node.Name); err != nil {
			return fmt.Errorf("field name: %w", err)
		}
		node.Name = strings.TrimSpace(node.Name)
		if node.Name == "" || len(node.Name) > 64 {
			return errors.New("field name must be 1..64 characters")
		}
	}
	if v, ok := patch["api_url"]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &node.APIURL); err != nil {
			return fmt.Errorf("field api_url: %w", err)
		}
		if !strings.HasPrefix(node.APIURL, "http://") && !strings.HasPrefix(node.APIURL, "https://") {
			return errors.New("field api_url must be an http(s) URL")
		}
		node.APIURL = strings.TrimRight(node.APIURL, "/")
	}
	if v, ok := patch["api_secret"]; ok && string(v) != "null" {
		var secret string
		if err := json.Unmarshal(v, &secret); err != nil {
			return fmt.Errorf("field api_secret: %w", err)
		}
		enc, err := cryptox.Encrypt(key, secret)
		if err != nil {
			return err
		}
		node.APISecretEnc = enc
	}
	if v, ok := patch["outbound_json"]; ok {
		if string(v) == "null" {
			node.OutboundEnc = ""
		} else {
			var raw string
			if err := json.Unmarshal(v, &raw); err != nil {
				return fmt.Errorf("field outbound_json: %w", err)
			}
			enc, err := cryptox.Encrypt(key, strings.TrimSpace(raw))
			if err != nil {
				return err
			}
			node.OutboundEnc = enc
		}
	}
	if v, ok := patch["tags"]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &node.Tags); err != nil {
			return fmt.Errorf("field tags: %w", err)
		}
	}
	if v, ok := patch["enabled"]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &node.Enabled); err != nil {
			return fmt.Errorf("field enabled: %w", err)
		}
	}
	if v, ok := patch["remark"]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &node.Remark); err != nil {
			return fmt.Errorf("field remark: %w", err)
		}
	}
	return nil
}

// checkNodeNow performs a synchronous reachability check.
func (s *Server) checkNodeNow(r *http.Request, node *store.Node) map[string]any {
	secret, err := cryptox.Decrypt(s.cryptoKey, node.APISecretEnc)
	if err != nil {
		s.logger.Error("decrypt secret failed", slog.String("node", node.Name), slog.Any("error", err))
		return checkResult(node.ID, false, nil, nil, "internal decryption failure", time.Now().Unix())
	}
	client, err := clash.New(node.APIURL, secret)
	if err != nil {
		return checkResult(node.ID, false, nil, nil, err.Error(), time.Now().Unix())
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	vres, err := client.Version(ctx)
	if err != nil {
		return checkResult(node.ID, false, nil, nil, nodeErrDetail(err), time.Now().Unix())
	}
	return checkResult(node.ID, true, &vres.LatencyMS, &vres.Version, "", time.Now().Unix())
}

func checkResult(id string, online bool, latency *int64, version *string, detail string, at int64) map[string]any {
	body := map[string]any{
		"node_id":    id,
		"online":     online,
		"checked_at": formatRFC3339(at),
	}
	if latency != nil {
		body["latency_ms"] = *latency
	} else {
		body["latency_ms"] = nil
	}
	body["version"] = stringPtr(version)
	body["detail"] = detail
	return body
}

func stringPtr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func nodeErrDetail(err error) string {
	var ce *clash.Error
	if errors.As(err, &ce) {
		if ce.StatusCode == 0 {
			return "node unreachable"
		}
		return "node returned non-success status"
	}
	return "node unreachable"
}
