// Package httpx provides shared HTTP plumbing: RFC 9457 Problem Details
// responses, request-ID propagation, structured access logging, and panic
// recovery.
package httpx

import (
	"errors"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

// Problem type URIs defined by the panel API contract.
const (
	ProbUnauthorized       = "https://sing-hub.dev/probs/unauthorized"
	ProbNotFound           = "https://sing-hub.dev/probs/not-found"
	ProbConflict           = "https://sing-hub.dev/probs/conflict"
	ProbValidation         = "https://sing-hub.dev/probs/validation-error"
	ProbRateLimited        = "https://sing-hub.dev/probs/rate-limited"
	ProbNodeUnreachable    = "https://sing-hub.dev/probs/node-unreachable"
	ProbPreconditionFailed = "https://sing-hub.dev/probs/precondition-failed"
	ProbInternal           = "https://sing-hub.dev/probs/internal-error"
	ProbUpstream           = "https://sing-hub.dev/probs/upstream-unavailable"
)

// ValidationError is one field-level error located by JSON Pointer (RFC 6901).
type ValidationError struct {
	Detail  string `json:"detail"`
	Pointer string `json:"pointer"`
}

// Problem is the RFC 9457 payload rendered as application/problem+json.
type Problem struct {
	Type     string            `json:"type"`
	Title    string            `json:"title"`
	Status   int               `json:"status"`
	Detail   string            `json:"detail,omitempty"`
	Instance string            `json:"instance,omitempty"`
	Errors   []ValidationError `json:"errors,omitempty"`
}

// FieldErr builds a ValidationError for the given JSON Pointer.
func FieldErr(pointer, detail string) ValidationError {
	return ValidationError{Detail: detail, Pointer: pointer}
}

// WriteProblem renders a Problem response with the matching Content-Type.
// status must agree with problem.Status (contract 2.5).
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, probType, detail string, errs ...ValidationError) {
	title := http.StatusText(status)
	if title == "" {
		title = "Unknown Error"
	}
	body := Problem{
		Type:     probType,
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: r.URL.Path,
		Errors:   errs,
	}
	if body.Type == "" {
		body.Type = "about:blank"
	}
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Convenience helpers for the most common status codes.

// Unauthorized writes 401 with WWW-Authenticate.
func Unauthorized(w http.ResponseWriter, r *http.Request, detail string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="sing-hub"`)
	WriteProblem(w, r, http.StatusUnauthorized, ProbUnauthorized, detail)
}

// NotFound writes 404.
func NotFound(w http.ResponseWriter, r *http.Request, detail string) {
	WriteProblem(w, r, http.StatusNotFound, ProbNotFound, detail)
}

// Conflict writes 409.
func Conflict(w http.ResponseWriter, r *http.Request, detail string) {
	WriteProblem(w, r, http.StatusConflict, ProbConflict, detail)
}

// Validation writes 422 with optional field errors.
func Validation(w http.ResponseWriter, r *http.Request, detail string, errs ...ValidationError) {
	WriteProblem(w, r, http.StatusUnprocessableEntity, ProbValidation, detail, errs...)
}

// NodeUnreachable writes 502 when the panel cannot reach a node's Clash API.
func NodeUnreachable(w http.ResponseWriter, r *http.Request, detail string) {
	WriteProblem(w, r, http.StatusBadGateway, ProbNodeUnreachable, detail)
}

// Internal writes 500 without leaking internals.
func Internal(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, http.StatusInternalServerError, ProbInternal, "")
}

// WriteJSON renders v as application/json with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteRawJSON emits pre-serialized JSON (idempotent replay).
func WriteRawJSON(w http.ResponseWriter, status int, raw []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// DecodeJSON strictly decodes a JSON request body into v.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		WriteProblem(w, r, http.StatusUnsupportedMediaType, "about:blank",
			"Content-Type must be application/json")
		return false
	}

	// Limit request body size
	bodyReader := http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(bodyReader)
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		var syntaxErr *json.SyntaxError
		switch {
		case errors.As(err, &tooLarge):
			WriteProblem(w, r, http.StatusRequestEntityTooLarge, "about:blank",
				"request body exceeds size limit")
		case errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &syntaxErr):
			WriteProblem(w, r, http.StatusBadRequest, ProbValidation,
				"request body is truncated or invalid JSON")
		default:
			Validation(w, r, "request body does not match the expected schema",
				FieldErr("", "request body has an invalid value or shape"))
		}
		return false
	}

	// A request contains exactly one JSON value. Unknown object fields remain
	// forward-compatible; a second value or malformed trailing bytes do not.
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		WriteProblem(w, r, http.StatusBadRequest, ProbValidation,
			"request body contains trailing content after JSON object")
		return false
	}

	return true
}
