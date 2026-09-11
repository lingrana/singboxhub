package api

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/sing-hub/panel/internal/httpx"
)

const maxBrandingBytes = 2 << 20
const maxBrandingDimension = 4096

func (s *Server) handleUploadFavicon(w http.ResponseWriter, r *http.Request) {
	s.uploadBranding(w, r, s.store.UpdateSettingsFavicon)
}

func (s *Server) handleUploadLogo(w http.ResponseWriter, r *http.Request) {
	s.uploadBranding(w, r, s.store.UpdateSettingsLogo)
}

func (s *Server) uploadBranding(w http.ResponseWriter, r *http.Request, save func([]byte) error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/octet-stream" && mediaType != "image/png" && mediaType != "image/jpeg" && mediaType != "image/gif") {
		httpx.WriteProblem(w, r, http.StatusUnsupportedMediaType, "about:blank", "expected PNG, JPEG or GIF image")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBrandingBytes+1))
	if len(data) > maxBrandingBytes {
		httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "about:blank", "image exceeds 2 MiB")
		return
	}
	if err != nil || len(data) == 0 {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "about:blank", "empty or unreadable body")
		return
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxBrandingDimension || cfg.Height > maxBrandingDimension {
		httpx.Validation(w, r, "expected a valid image no larger than 4096x4096 pixels")
		return
	}
	if mediaType != "application/octet-stream" && mediaType != "image/"+format {
		httpx.WriteProblem(w, r, http.StatusUnsupportedMediaType, "about:blank", "Content-Type does not match the image")
		return
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		httpx.Validation(w, r, "image data is incomplete or invalid")
		return
	}
	if s.currentSettings() == nil {
		httpx.Internal(w, r)
		return
	}
	if err := save(data); err != nil {
		s.logger.Error("save branding image", "error", err)
		httpx.Internal(w, r)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleServeFavicon(w http.ResponseWriter, r *http.Request) {
	s.serveBranding(w, r, true)
}

func (s *Server) handleServeLogo(w http.ResponseWriter, r *http.Request) {
	s.serveBranding(w, r, false)
}

func (s *Server) serveBranding(w http.ResponseWriter, r *http.Request, favicon bool) {
	st := s.currentSettings()
	if st == nil {
		httpx.Internal(w, r)
		return
	}
	data := st.Logo
	if favicon {
		data = st.Favicon
	}
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		httpx.NotFound(w, r, "image has not been configured; upload a valid image")
		return
	}
	w.Header().Set("Content-Type", "image/"+format)
	w.Header().Set("Cache-Control", "public, no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(data)))
	http.ServeContent(&apiErrorWriter{ResponseWriter: w, request: r}, r, "", time.Time{}, bytes.NewReader(data))
}
