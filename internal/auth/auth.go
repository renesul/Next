package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"next/internal/logger"
)

// AuthUser represents a user in the users table.
type AuthUser struct {
	ID                 int       `json:"id"`
	Username           string    `json:"username"`
	PasswordHash       string    `json:"-"`
	Role               string    `json:"role"`
	MustChangePassword bool      `json:"must_change_password,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

// AuthSession holds session data for a logged-in user.
type AuthSession struct {
	UserID             int
	Username           string
	Role               string
	MustChangePassword bool
	Expiry             int64 // Unix timestamp
}

// Auth manages user authentication and sessions.
type Auth struct {
	db       *sql.DB
	logger   *logger.Logger
	mu       sync.RWMutex
	sessions map[string]*AuthSession // token -> session
}

type ctxKey string

const ctxSession ctxKey = "session"

const (
	authCookieName   = "next_session"
	authSessionTTL   = 24 * time.Hour
	authCleanupEvery = 30 * time.Minute
)

// BcryptCost controls bcrypt hashing cost. Lowered in tests for speed.
var BcryptCost = 12

// jsonResponse writes a JSON response with the correct Content-Type header.
func jsonResponse(rw http.ResponseWriter, data any) {
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(data)
}

// jsonError writes a JSON error response with the given status code.
func jsonError(rw http.ResponseWriter, message string, statusCode int) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(statusCode)
	json.NewEncoder(rw).Encode(map[string]string{"error": message})
}

// requireJSON returns true if the request has a JSON Content-Type, otherwise sends 415.
func requireJSON(rw http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		jsonError(rw, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// NewAuth creates the users/sessions tables and starts the session cleanup goroutine.
func NewAuth(db *sql.DB, l *logger.Logger) (*Auth, error) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS users (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		username      TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		role          TEXT NOT NULL DEFAULT 'user',
		must_change_password INTEGER NOT NULL DEFAULT 0,
		created_at    DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	if err == nil {
		// Migration: add column if it doesn't exist (idempotent)
		db.Exec("ALTER TABLE users ADD COLUMN must_change_password INTEGER NOT NULL DEFAULT 0")
	}
	if err != nil {
		return nil, fmt.Errorf("create users table: %w", err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		token      TEXT PRIMARY KEY,
		user_id    INTEGER NOT NULL,
		username   TEXT NOT NULL,
		role       TEXT NOT NULL,
		expiry     INTEGER NOT NULL,
		created_at INTEGER NOT NULL DEFAULT (unixepoch()),
		FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
	)`)
	if err != nil {
		return nil, fmt.Errorf("create sessions table: %w", err)
	}
	a := &Auth{db: db, logger: l, sessions: make(map[string]*AuthSession)}
	a.loadSessions()
	if !a.HasUsers() {
		user, err := a.CreateUser("admin", "admin123", "admin")
		if err != nil {
			return nil, fmt.Errorf("create default admin: %w", err)
		}
		// Force password change on first login
		db.Exec("UPDATE users SET must_change_password = 1 WHERE id = ?", user.ID)
		log.Println("auth: default admin user created (admin/admin123) — change password after first login")
	}
	go a.cleanupLoop()
	return a, nil
}

// loadSessions populates the in-memory session map from the database.
func (a *Auth) loadSessions() {
	rows, err := a.db.Query("SELECT token, user_id, username, role, expiry FROM sessions WHERE expiry > unixepoch()")
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var token, username, role string
		var userID int
		var expiry int64
		if err := rows.Scan(&token, &userID, &username, &role, &expiry); err != nil {
			continue
		}
		a.sessions[token] = &AuthSession{
			UserID:   userID,
			Username: username,
			Role:     role,
			Expiry:   expiry,
		}
	}
}

// logEvent is a nil-safe helper that logs only when logger is set.
func (a *Auth) logEvent(event, chatID string, data map[string]any) {
	if a.logger != nil {
		a.logger.Log(event, chatID, data)
	}
}

// HasUsers returns true if at least one user exists.
func (a *Auth) HasUsers() bool {
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	return count > 0
}

// CreateUser adds a new user with bcrypt-hashed password.
func (a *Auth) CreateUser(username, password, role string) (*AuthUser, error) {
	username = strings.TrimSpace(username)
	if len(username) < 3 {
		return nil, errors.New("username deve ter pelo menos 3 caracteres")
	}
	if len(password) < 6 {
		return nil, errors.New("senha deve ter pelo menos 6 caracteres")
	}
	if role != "admin" && role != "user" {
		return nil, errors.New("role deve ser 'admin' ou 'user'")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	res, err := a.db.Exec("INSERT INTO users (username, password_hash, role) VALUES (?, ?, ?)", username, string(hash), role)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, errors.New("username ja existe")
		}
		return nil, fmt.Errorf("insert user: %w", err)
	}

	id, _ := res.LastInsertId()
	return &AuthUser{ID: int(id), Username: username, Role: role, CreatedAt: time.Now()}, nil
}

// Authenticate checks username and password, returning the user on success.
func (a *Auth) Authenticate(username, password string) (*AuthUser, error) {
	var u AuthUser
	var hash string
	var ts string
	var mustChange int
	err := a.db.QueryRow("SELECT id, username, password_hash, role, COALESCE(must_change_password, 0), created_at FROM users WHERE username = ?", username).
		Scan(&u.ID, &u.Username, &hash, &u.Role, &mustChange, &ts)
	if err != nil {
		return nil, errors.New("usuario ou senha incorretos")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, errors.New("usuario ou senha incorretos")
	}
	u.MustChangePassword = mustChange == 1
	u.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ts)
	return &u, nil
}

// ListUsers returns all users without password hashes.
func (a *Auth) ListUsers() ([]AuthUser, error) {
	rows, err := a.db.Query("SELECT id, username, role, created_at FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []AuthUser
	for rows.Next() {
		var u AuthUser
		var ts string
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &ts); err != nil {
			return nil, err
		}
		u.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ts)
		users = append(users, u)
	}
	if users == nil {
		users = []AuthUser{}
	}
	return users, rows.Err()
}

// UpdatePassword changes a user's password.
func (a *Auth) UpdatePassword(userID int, newPassword string) error {
	if len(newPassword) < 6 {
		return errors.New("senha deve ter pelo menos 6 caracteres")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), BcryptCost)
	if err != nil {
		return err
	}
	res, err := a.db.Exec("UPDATE users SET password_hash = ? WHERE id = ?", string(hash), userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("usuario nao encontrado")
	}
	return nil
}

// UpdateRole changes a user's role, preventing removal of the last admin.
func (a *Auth) UpdateRole(userID int, role string) error {
	if role != "admin" && role != "user" {
		return errors.New("role deve ser 'admin' ou 'user'")
	}
	if role == "user" {
		// Check if this is the last admin
		var currentRole string
		a.db.QueryRow("SELECT role FROM users WHERE id = ?", userID).Scan(&currentRole)
		if currentRole == "admin" {
			var adminCount int
			a.db.QueryRow("SELECT COUNT(*) FROM users WHERE role = 'admin'").Scan(&adminCount)
			if adminCount <= 1 {
				return errors.New("nao pode remover o ultimo admin")
			}
		}
	}
	res, err := a.db.Exec("UPDATE users SET role = ? WHERE id = ?", role, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("usuario nao encontrado")
	}
	return nil
}

// DeleteUser removes a user, preventing deletion of the last admin.
func (a *Auth) DeleteUser(userID int) error {
	var role string
	err := a.db.QueryRow("SELECT role FROM users WHERE id = ?", userID).Scan(&role)
	if err != nil {
		return errors.New("usuario nao encontrado")
	}
	if role == "admin" {
		var adminCount int
		a.db.QueryRow("SELECT COUNT(*) FROM users WHERE role = 'admin'").Scan(&adminCount)
		if adminCount <= 1 {
			return errors.New("nao pode deletar o ultimo admin")
		}
	}
	a.db.Exec("DELETE FROM users WHERE id = ?", userID)
	a.DestroyUserSessions(userID)
	return nil
}

// CreateSession generates a random token and stores the session in memory and DB.
func (a *Auth) CreateSession(user *AuthUser) string {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	expiry := time.Now().Add(authSessionTTL).Unix()

	a.mu.Lock()
	a.sessions[token] = &AuthSession{
		UserID:             user.ID,
		Username:           user.Username,
		Role:               user.Role,
		MustChangePassword: user.MustChangePassword,
		Expiry:             expiry,
	}
	a.mu.Unlock()

	a.db.Exec("INSERT OR REPLACE INTO sessions (token, user_id, username, role, expiry) VALUES (?, ?, ?, ?, ?)",
		token, user.ID, user.Username, user.Role, expiry)
	return token
}

// GetSession returns the session for a token, or nil if expired/missing.
// Falls back to DB lookup if not in memory (e.g., after restart).
func (a *Auth) GetSession(token string) *AuthSession {
	now := time.Now().Unix()
	a.mu.RLock()
	s, ok := a.sessions[token]
	a.mu.RUnlock()
	if ok {
		if s.Expiry < now {
			a.mu.Lock()
			delete(a.sessions, token)
			a.mu.Unlock()
			a.db.Exec("DELETE FROM sessions WHERE token = ?", token)
			return nil
		}
		return s
	}
	// Fallback: check DB (session created before restart)
	var userID int
	var username, role string
	var expiry int64
	err := a.db.QueryRow("SELECT user_id, username, role, expiry FROM sessions WHERE token = ?", token).
		Scan(&userID, &username, &role, &expiry)
	if err != nil || expiry < now {
		if err == nil {
			a.db.Exec("DELETE FROM sessions WHERE token = ?", token)
		}
		return nil
	}
	s = &AuthSession{UserID: userID, Username: username, Role: role, Expiry: expiry}
	a.mu.Lock()
	a.sessions[token] = s
	a.mu.Unlock()
	return s
}

// DestroySession removes a single session from memory and DB.
func (a *Auth) DestroySession(token string) {
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
	a.db.Exec("DELETE FROM sessions WHERE token = ?", token)
}

// DestroyUserSessions removes all sessions for a given user from memory and DB.
func (a *Auth) DestroyUserSessions(userID int) {
	a.mu.Lock()
	for token, s := range a.sessions {
		if s.UserID == userID {
			delete(a.sessions, token)
		}
	}
	a.mu.Unlock()
	a.db.Exec("DELETE FROM sessions WHERE user_id = ?", userID)
}

// Middleware checks auth on every request.
// Exempt paths: /login, /api/login, /api/auth/status.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// Exempt paths
		path := r.URL.Path
		if path == "/login" || path == "/api/login" || path == "/api/auth/status" || path == "/health" || strings.HasPrefix(path, "/mcp/") || strings.HasPrefix(path, "/static/") {
			next.ServeHTTP(rw, r)
			return
		}

		// Check cookie
		cookie, err := r.Cookie(authCookieName)
		if err != nil {
			a.denyAccess(rw, r)
			return
		}
		session := a.GetSession(cookie.Value)
		if session == nil {
			a.denyAccess(rw, r)
			return
		}

		// If must change password, only allow change-password and logout
		if session.MustChangePassword && path != "/api/auth/change-password" && path != "/api/logout" && path != "/api/auth/status" {
			if strings.HasPrefix(path, "/api/") {
				jsonError(rw, "must_change_password", 403)
				return
			}
			// Redirect to login page for non-API requests
			http.Redirect(rw, r, "/login", 302)
			return
		}

		// Inject session into context
		ctx := context.WithValue(r.Context(), ctxSession, session)
		next.ServeHTTP(rw, r.WithContext(ctx))
	})
}

func (a *Auth) denyAccess(rw http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		jsonError(rw, "unauthorized", 401)
		return
	}
	http.Redirect(rw, r, "/login", 302)
}

// HandleLogin handles GET /login (serve page) and POST /api/login (authenticate).
func (a *Auth) HandleLogin(rw http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		tmpl, err := template.ParseFiles("templates/login.html")
		if err != nil {
			http.Error(rw, "Template error: "+err.Error(), 500)
			return
		}
		tmpl.Execute(rw, nil)
		return
	}

	if r.Method != "POST" {
		jsonError(rw, "Method not allowed", 405)
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !requireJSON(rw, r) {
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(rw, "JSON invalido", 400)
		return
	}

	user, err := a.Authenticate(req.Username, req.Password)
	if err != nil {
		a.logEvent("auth_login_failed", "", map[string]any{"username": req.Username, "reason": err.Error()})
		jsonError(rw, err.Error(), 401)
		return
	}

	a.logEvent("auth_login_ok", "", map[string]any{"username": user.Username, "role": user.Role})
	token := a.CreateSession(user)
	http.SetCookie(rw, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(authSessionTTL.Seconds()),
	})

	resp := map[string]any{"ok": true, "role": user.Role}
	if user.MustChangePassword {
		resp["must_change_password"] = true
	}
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(resp)
}

// HandleLogout destroys the session and clears the cookie.
func (a *Auth) HandleLogout(rw http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(rw, "Method not allowed", 405)
		return
	}
	cookie, err := r.Cookie(authCookieName)
	if err == nil {
		if s := a.GetSession(cookie.Value); s != nil {
			a.logEvent("auth_logout", "", map[string]any{"username": s.Username})
		}
		a.DestroySession(cookie.Value)
	}
	http.SetCookie(rw, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]any{"ok": true})
}

// HandleChangePassword handles POST /api/auth/change-password.
func (a *Auth) HandleChangePassword(rw http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(rw, "Method not allowed", 405)
		return
	}

	s := GetSessionFromCtx(r)
	if s == nil {
		jsonError(rw, "unauthorized", 401)
		return
	}

	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !requireJSON(rw, r) {
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(rw, "JSON invalido", 400)
		return
	}

	// Verify current password
	var hash string
	a.db.QueryRow("SELECT password_hash FROM users WHERE id = ?", s.UserID).Scan(&hash)
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.CurrentPassword)) != nil {
		jsonError(rw, "senha atual incorreta", 400)
		return
	}

	if err := a.UpdatePassword(s.UserID, req.NewPassword); err != nil {
		jsonError(rw, err.Error(), 400)
		return
	}

	// Clear force-change flag
	a.db.Exec("UPDATE users SET must_change_password = 0 WHERE id = ?", s.UserID)
	s.MustChangePassword = false

	a.logEvent("auth_password_changed", "", map[string]any{"username": s.Username})
	jsonResponse(rw, map[string]any{"ok": true})
}

func (a *Auth) cleanupLoop() {
	ticker := time.NewTicker(authCleanupEvery)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now().Unix()
		a.mu.Lock()
		for token, s := range a.sessions {
			if s.Expiry < now {
				delete(a.sessions, token)
			}
		}
		a.mu.Unlock()
		a.db.Exec("DELETE FROM sessions WHERE expiry <= ?", now)
	}
}

// GetSessionFromCtx extracts the session from the request context.
func GetSessionFromCtx(r *http.Request) *AuthSession {
	s, _ := r.Context().Value(ctxSession).(*AuthSession)
	return s
}

// RequireAdmin checks if the current user is admin, sending 403 if not.
// Returns true if admin, false otherwise.
func RequireAdmin(rw http.ResponseWriter, r *http.Request) bool {
	s := GetSessionFromCtx(r)
	if s != nil && s.Role == "admin" {
		return true
	}
	jsonError(rw, "forbidden", 403)
	return false
}

// HandleAuthStatus returns auth state and current user info.
func (a *Auth) HandleAuthStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	// Try context first (middleware ran), then cookie directly (exempt route)
	s := GetSessionFromCtx(r)
	if s == nil {
		if cookie, err := r.Cookie(authCookieName); err == nil {
			s = a.GetSession(cookie.Value)
		}
	}
	if s == nil {
		json.NewEncoder(rw).Encode(map[string]any{"enabled": true, "user": nil})
		return
	}
	userData := map[string]any{
		"id":       s.UserID,
		"username": s.Username,
		"role":     s.Role,
	}
	if s.MustChangePassword {
		userData["must_change_password"] = true
	}
	json.NewEncoder(rw).Encode(map[string]any{
		"enabled": true,
		"user":    userData,
	})
}

// HandleUsers handles GET (list) and POST (create) for users.
func (a *Auth) HandleUsers(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		if !RequireAdmin(rw, r) {
			return
		}
		users, err := a.ListUsers()
		if err != nil {
			jsonError(rw, err.Error(), 500)
			return
		}
		jsonResponse(rw, users)

	case "POST":
		if !RequireAdmin(rw, r) {
			return
		}

		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if !requireJSON(rw, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(rw, "JSON invalido", 400)
			return
		}
		user, err := a.CreateUser(req.Username, req.Password, req.Role)
		if err != nil {
			jsonError(rw, err.Error(), 400)
			return
		}

		by := ""
		if s := GetSessionFromCtx(r); s != nil {
			by = s.Username
		}
		a.logEvent("auth_user_created", "", map[string]any{"username": user.Username, "role": user.Role, "by": by})
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(201)
		json.NewEncoder(rw).Encode(map[string]any{"ok": true, "id": user.ID})

	default:
		jsonError(rw, "Method not allowed", 405)
	}
}

// HandleUserByID handles PUT (update) and DELETE for /api/users/{id}.
func (a *Auth) HandleUserByID(rw http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/users/")
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		jsonError(rw, "invalid id", 400)
		return
	}

	switch r.Method {
	case "PUT":
		var req struct {
			Password        string `json:"password"`
			CurrentPassword string `json:"current_password"`
			Role            string `json:"role"`
		}
		if !requireJSON(rw, r) {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(rw, "JSON invalido", 400)
			return
		}

		s := GetSessionFromCtx(r)

		// Change password
		if req.Password != "" {
			// Users can change their own password (with current_password)
			// Admins can change anyone's password
			if s != nil && s.UserID == id {
				// Changing own password — verify current
				var hash string
				a.db.QueryRow("SELECT password_hash FROM users WHERE id = ?", id).Scan(&hash)
				if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.CurrentPassword)) != nil {
					jsonError(rw, "senha atual incorreta", 400)
					return
				}
			} else if s == nil || s.Role != "admin" {
				jsonError(rw, "forbidden", 403)
				return
			}
			if err := a.UpdatePassword(id, req.Password); err != nil {
				jsonError(rw, err.Error(), 400)
				return
			}
			jsonResponse(rw, map[string]any{"ok": true})
			return
		}

		// Change role (admin only)
		if req.Role != "" {
			if !RequireAdmin(rw, r) {
				return
			}
			// Get old role for logging
			var oldRole string
			a.db.QueryRow("SELECT role FROM users WHERE id = ?", id).Scan(&oldRole)

			if err := a.UpdateRole(id, req.Role); err != nil {
				jsonError(rw, err.Error(), 400)
				return
			}
			by := ""
			if s := GetSessionFromCtx(r); s != nil {
				by = s.Username
			}
			a.logEvent("auth_user_role_changed", "", map[string]any{"user_id": id, "old_role": oldRole, "new_role": req.Role, "by": by})
			// Invalidate user sessions so role takes effect
			a.DestroyUserSessions(id)
			jsonResponse(rw, map[string]any{"ok": true})
			return
		}

		jsonError(rw, "nothing to update", 400)

	case "DELETE":
		if !RequireAdmin(rw, r) {
			return
		}
		s := GetSessionFromCtx(r)
		if s != nil && s.UserID == id {
			jsonError(rw, "nao pode deletar a si mesmo", 400)
			return
		}
		// Get username before deletion for logging
		var deletedUsername string
		a.db.QueryRow("SELECT username FROM users WHERE id = ?", id).Scan(&deletedUsername)

		if err := a.DeleteUser(id); err != nil {
			jsonError(rw, err.Error(), 400)
			return
		}
		by := ""
		if s != nil {
			by = s.Username
		}
		a.logEvent("auth_user_deleted", "", map[string]any{"user_id": id, "username": deletedUsername, "by": by})
		jsonResponse(rw, map[string]any{"ok": true})

	default:
		jsonError(rw, "Method not allowed", 405)
	}
}
