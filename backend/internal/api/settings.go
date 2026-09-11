package api

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/sing-hub/panel/internal/httpx"
	"github.com/sing-hub/panel/internal/store"
)

// settingsETag identifies a committed settings revision.
func settingsETag(revision int64) string {
	return `"settings-r` + strconv.FormatInt(revision, 10) + `"`
}

// handleGetSettings implements GET /settings.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	st := s.currentSettings()
	if st == nil {
		httpx.Internal(w, r)
		return
	}
	w.Header().Set("ETag", settingsETag(st.Revision))
	httpx.WriteJSON(w, http.StatusOK, settingsBody(st))
}

// handleUpdateSettings implements PATCH /settings with mandatory If-Match.
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	st := s.currentSettings()
	if st == nil {
		httpx.Internal(w, r)
		return
	}
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		httpx.WriteProblem(w, r, http.StatusPreconditionRequired,
			"https://sing-hub.dev/probs/precondition-required", "If-Match header is required")
		return
	}
	if ifMatch != settingsETag(st.Revision) {
		httpx.WriteProblem(w, r, http.StatusPreconditionFailed,
			httpx.ProbPreconditionFailed, "settings were modified concurrently; GET /settings and retry")
		return
	}

	var patch map[string]any
	if !httpx.DecodeJSON(w, r, &patch) {
		return
	}
	if errs := validateSettingsPatch(patch); len(errs) > 0 {
		httpx.Validation(w, r, "settings failed validation", errs...)
		return
	}
	if v, ok := patch["sampler_interval_seconds"]; ok {
		st.SamplerIntervalSeconds = int(v.(float64))
	}
	if v, ok := patch["retention_days"]; ok {
		st.RetentionDays = int(v.(float64))
	}
	if v, ok := patch["ip_lookup_enabled"]; ok {
		st.IPLookupEnabled = v.(bool)
	}
	if v, ok := patch["ip_lookup_provider_url"]; ok {
		st.IPLookupProviderURL = v.(string)
	}
	if v, ok := patch["singbox_bin_path"]; ok {
		if v == nil {
			st.SingboxBinPath = ""
		} else {
			st.SingboxBinPath = v.(string)
		}
	}
	if v, ok := patch["brand_name"]; ok {
		if v == nil {
			st.BrandName = ""
		} else if s, isStr := v.(string); isStr {
			st.BrandName = s
		} else {
			httpx.Validation(w, r, "field brand_name must be a string or null")
			return
		}
	}
	if v, ok := patch["reset_cooldown_minutes"]; ok {
		if n, isNum := v.(float64); isNum {
			st.ResetCooldownMinutes = int(n)
		} else {
			httpx.Validation(w, r, "field reset_cooldown_minutes must be a number")
			return
		}
	}

	if err := s.store.UpdateSettings(st); errors.Is(err, store.ErrSettingsConflict) {
		httpx.WriteProblem(w, r, http.StatusPreconditionFailed,
			httpx.ProbPreconditionFailed, "settings were modified concurrently; GET /settings and retry")
		return
	} else if err != nil {
		s.logger.Error("update settings failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	if err := s.hub.Reload(); err != nil {
		s.logger.Error("hub reload failed", slog.Any("error", err))
	}
	w.Header().Set("ETag", settingsETag(st.Revision))
	httpx.WriteJSON(w, http.StatusOK, settingsBody(st))
}

func settingsBody(st *store.Settings) map[string]any {
	body := map[string]any{
		"sampler_interval_seconds": st.SamplerIntervalSeconds,
		"retention_days":           st.RetentionDays,
		"ip_lookup_enabled":        st.IPLookupEnabled,
		"ip_lookup_provider_url":   st.IPLookupProviderURL,
		"singbox_bin_path":         st.SingboxBinPath,
		"brand_name":               st.BrandName,
		"reset_cooldown_minutes":   st.ResetCooldownMinutes,
		"updated_at":               formatRFC3339(st.UpdatedAt),
	}
	if len(st.Favicon) > 0 {
		body["favicon_url"] = "/settings/favicon"
	}
	if len(st.Logo) > 0 {
		body["logo_url"] = "/settings/logo"
	}
	return body
}

// handleGetBrand implements GET /brand (public, no auth).
func (s *Server) handleGetBrand(w http.ResponseWriter, r *http.Request) {
	st := s.currentSettings()
	if st == nil {
		httpx.Internal(w, r)
		return
	}
	body := map[string]any{
		"brand_name": st.BrandName,
	}
	if len(st.Favicon) > 0 {
		body["favicon_url"] = "/settings/favicon"
	}
	if len(st.Logo) > 0 {
		body["logo_url"] = "/settings/logo"
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

// validateSettingsPatch enforces field types and bounds without mutating.
func validateSettingsPatch(patch map[string]any) []httpx.ValidationError {
	var errs []httpx.ValidationError
	if v, ok := patch["sampler_interval_seconds"]; ok {
		n, isNum := v.(float64)
		if !isNum || n < 2 || n > 60 || math.Trunc(n) != n {
			errs = append(errs, httpx.FieldErr("#/sampler_interval_seconds", "must be an integer 2..60"))
		}
	}
	if v, ok := patch["retention_days"]; ok {
		n, isNum := v.(float64)
		if !isNum || n < 1 || n > 365 || math.Trunc(n) != n {
			errs = append(errs, httpx.FieldErr("#/retention_days", "must be an integer 1..365"))
		}
	}
	if v, ok := patch["ip_lookup_enabled"]; ok {
		if _, isBool := v.(bool); !isBool {
			errs = append(errs, httpx.FieldErr("#/ip_lookup_enabled", "must be a boolean"))
		}
	}
	if v, ok := patch["ip_lookup_provider_url"]; ok {
		s, isStr := v.(string)
		if !isStr || !(len(s) > 8 && (s[:7] == "http://" || len(s) > 8 && s[:8] == "https://")) {
			errs = append(errs, httpx.FieldErr("#/ip_lookup_provider_url", "must be an http(s) URL"))
		}
	}
	if v, ok := patch["singbox_bin_path"]; ok && v != nil {
		if _, isStr := v.(string); !isStr {
			errs = append(errs, httpx.FieldErr("#/singbox_bin_path", "must be a string or null"))
		}
	}
	return errs
}
