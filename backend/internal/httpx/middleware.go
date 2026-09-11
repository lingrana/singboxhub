package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = 1

// RequestID returns the request ID attached by Middleware.
func RequestID(r *http.Request) string {
	if v, ok := r.Context().Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Middleware builds the shared handler chain: request ID, panic recovery,
// and structured access logging. Authorization values are never logged.
func Middleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = newRequestID()
		}
		w.Header().Set("X-Request-Id", reqID)
		r = r.WithContext(contextWithRequestID(r.Context(), reqID))

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				logger.Error("panic recovered",
					slog.Any("panic", p), slog.String("request_id", reqID),
					slog.String("method", r.Method), slog.String("path", r.URL.Path))
				if rec.status == http.StatusOK {
					Internal(rec, r)
				}
			}
			// Sanitize sensitive paths: /sub/{token} -> /sub/{redacted}
			path := r.URL.Path
			if strings.HasPrefix(path, "/sub/") && len(path) > 5 {
				path = "/sub/{redacted}"
			}
			logger.Info("request",
				slog.String("request_id", reqID),
				slog.String("method", r.Method),
				slog.String("path", path),
				slog.Int("status", rec.status),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr))
		}()
		next.ServeHTTP(rec, r)
	})
}
