package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sing-hub/panel/internal/auth"
	"github.com/sing-hub/panel/internal/httpx"
	"github.com/sing-hub/panel/internal/store"
)

func userBody(u *store.User) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Username, "created_at": formatRFC3339(u.CreatedAt)}
}

func (s *Server) userError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.NotFound(w, r, "user not found")
	case isUniqueViolation(err):
		httpx.Conflict(w, r, "username already exists")
	default:
		s.logger.Error("user operation failed", slog.Any("error", err))
		httpx.Internal(w, r)
	}
}

func (s *Server) userOr404(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	uid, err := strconv.ParseInt(r.PathValue("user_id"), 10, 64)
	if err != nil || uid <= 0 {
		httpx.NotFound(w, r, "user not found")
		return nil, false
	}
	u, err := s.store.GetUserByID(uid)
	if err != nil {
		s.userError(w, r, err)
		return nil, false
	}
	return u, true
}

func validUsername(name string) bool { return name != "" && utf8.RuneCountInString(name) <= 64 }

func validPassword(password string) bool {
	return utf8.RuneCountInString(password) >= 6 && len(password) <= 72
}

// handleGetUser implements GET /users/{user_id}.
func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	if u, ok := s.userOr404(w, r); ok {
		httpx.WriteJSON(w, http.StatusOK, userBody(u))
	}
}

// handleListUsers implements GET /users.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	limit, ok := parsePositiveInt(r.URL.Query().Get("page_size"), 100, 100)
	if !ok {
		httpx.Validation(w, r, "page_size must be 1..100")
		return
	}
	var after int64
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		parts, valid := decodeCursor(cursor)
		if valid && len(parts) == 1 {
			after, _ = strconv.ParseInt(parts[0], 10, 64)
		}
		if after <= 0 {
			httpx.Validation(w, r, "cursor is invalid")
			return
		}
	}
	users, err := s.store.ListUsers()
	if err != nil {
		s.userError(w, r, err)
		return
	}
	items := make([]map[string]any, 0)
	body := map[string]any{}
	var last int64
	for _, u := range users {
		if u.ID <= after {
			continue
		}
		if len(items) == limit {
			body["next_cursor"] = encodeCursor(strconv.FormatInt(last, 10))
			break
		}
		items = append(items, userBody(&u))
		last = u.ID
	}
	body["items"] = items
	httpx.WriteJSON(w, http.StatusOK, body)
}

// handleCreateUser implements POST /users.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	username := strings.TrimSpace(body.Username)
	if !validUsername(username) {
		httpx.Validation(w, r, "username must be 1-64 characters",
			httpx.FieldErr("#/username", "1-64 characters required"))
		return
	}
	if !validPassword(body.Password) {
		httpx.Validation(w, r, "password must be at least 6 characters and at most 72 UTF-8 bytes",
			httpx.FieldErr("#/password", "6 or more characters, maximum 72 bytes"))
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		s.logger.Error("hash password failed", slog.Any("error", err))
		httpx.Internal(w, r)
		return
	}
	id, err := s.store.CreateUser(username, hash)
	if err != nil {
		s.userError(w, r, err)
		return
	}
	u, err := s.store.GetUserByID(id)
	if err != nil {
		s.userError(w, r, err)
		return
	}
	w.Header().Set("Location", "/users/"+strconv.FormatInt(id, 10))
	httpx.WriteJSON(w, http.StatusCreated, userBody(u))
}

// handleDeleteUser implements DELETE /users/{user_id}.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.userOr404(w, r)
	if !ok {
		return
	}
	if u.ID == 1 {
		httpx.Validation(w, r, "cannot delete the default admin account")
		return
	}
	if err := s.store.DeleteUser(u.ID); err != nil {
		s.userError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateUser implements PATCH /users/{user_id}.
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.userOr404(w, r)
	if !ok {
		return
	}
	var body struct {
		Username *string `json:"username"`
		Password *string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if body.Username != nil {
		*body.Username = strings.TrimSpace(*body.Username)
		if !validUsername(*body.Username) {
			httpx.Validation(w, r, "username must be 1-64 characters", httpx.FieldErr("#/username", "1-64 characters required"))
			return
		}
	}
	var passwordHash *string
	if body.Password != nil {
		if !validPassword(*body.Password) {
			httpx.Validation(w, r, "password must be at least 6 characters and at most 72 UTF-8 bytes",
				httpx.FieldErr("#/password", "6 or more characters, maximum 72 bytes"))
			return
		}
		hash, err := auth.HashPassword(*body.Password)
		if err != nil {
			s.logger.Error("hash password failed", slog.Any("error", err))
			httpx.Internal(w, r)
			return
		}
		passwordHash = &hash
	}
	if err := s.store.UpdateUser(u.ID, body.Username, passwordHash); err != nil {
		s.userError(w, r, err)
		return
	}
	if body.Username != nil {
		u.Username = *body.Username
	}
	httpx.WriteJSON(w, http.StatusOK, userBody(u))
}
