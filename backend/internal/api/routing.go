package api

import (
	"net/http"

	"github.com/sing-hub/panel/internal/httpx"
)

// Preserve standard-library routing and Range handling while formatting generated errors.
type apiErrorWriter struct {
	http.ResponseWriter
	request       *http.Request
	onlyUnmatched bool
	failed        bool
}

func (w *apiErrorWriter) WriteHeader(status int) {
	if status >= 400 && (!w.onlyUnmatched || w.request.Pattern == "") {
		w.failed = true
		w.Header().Del("Content-Length")
		httpx.WriteProblem(w.ResponseWriter, w.request, status, "about:blank", http.StatusText(status))
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *apiErrorWriter) Write(data []byte) (int, error) {
	if w.failed {
		return len(data), nil
	}
	return w.ResponseWriter.Write(data)
}
