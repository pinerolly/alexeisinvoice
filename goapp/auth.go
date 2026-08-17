// Authentication: DB-backed user accounts (admin/user roles) with
// bcrypt-hashed passwords, server-side sessions, CSRF protection, and a
// simple brute-force lockout for the login endpoint. A default admin/admin
// account is seeded on first run (see seedDefaultAdmin in db.go); the admin
// should change that password immediately via the account page.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookieName    = "invoiceapp_session"
	sessionAbsoluteTTL   = 12 * time.Hour
	loginMaxAttempts     = 5
	loginLockoutDuration = 15 * time.Minute
)

type session struct {
	username  string
	role      string
	csrfToken string
	expiresAt time.Time
}

var (
	sessionsMu sync.Mutex
	sessions   = map[string]*session{}

	loginAttemptsMu sync.Mutex
	loginAttempts   = map[string]*loginAttemptInfo{}
)

type loginAttemptInfo struct {
	failures    int
	lockedUntil time.Time
}

func bcryptHash(plain string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isLockedOut(ip string) bool {
	loginAttemptsMu.Lock()
	defer loginAttemptsMu.Unlock()
	info, ok := loginAttempts[ip]
	if !ok {
		return false
	}
	return time.Now().Before(info.lockedUntil)
}

func recordLoginFailure(ip string) {
	loginAttemptsMu.Lock()
	defer loginAttemptsMu.Unlock()
	info, ok := loginAttempts[ip]
	if !ok {
		info = &loginAttemptInfo{}
		loginAttempts[ip] = info
	}
	info.failures++
	if info.failures >= loginMaxAttempts {
		info.lockedUntil = time.Now().Add(loginLockoutDuration)
		info.failures = 0
	}
}

func clearLoginFailures(ip string) {
	loginAttemptsMu.Lock()
	defer loginAttemptsMu.Unlock()
	delete(loginAttempts, ip)
}

func createSession(username, role string) (token string, s *session, err error) {
	token, err = generateToken()
	if err != nil {
		return "", nil, err
	}
	csrfToken, err := generateToken()
	if err != nil {
		return "", nil, err
	}
	s = &session{
		username:  username,
		role:      role,
		csrfToken: csrfToken,
		expiresAt: time.Now().Add(sessionAbsoluteTTL),
	}
	sessionsMu.Lock()
	sessions[token] = s
	sessionsMu.Unlock()
	return token, s, nil
}

func getSession(r *http.Request) (string, *session) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", nil
	}
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	s, ok := sessions[cookie.Value]
	if !ok || time.Now().After(s.expiresAt) {
		return "", nil
	}
	return cookie.Value, s
}

func deleteSession(token string) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	delete(sessions, token)
}

// isRequestSecure reports whether the client's connection is HTTPS, either
// directly or via a reverse proxy (e.g. Caddy) that sets X-Forwarded-Proto.
func isRequestSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isRequestSecure(r),
		SameSite: http.SameSiteLaxMode,
		// Persist across browser restarts up to the session's absolute TTL,
		// so reopening the app goes straight to New Invoice instead of login.
		MaxAge: int(sessionAbsoluteTTL.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isRequestSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// requireAuth protects a handler behind a valid session, redirecting to the
// login page (preserving the originally requested path) when absent/expired.
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, s := getSession(r)
		if s == nil {
			http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// requireAdmin protects a handler behind a valid session with the "admin"
// role, returning 403 for logged-in non-admins.
func requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		_, s := getSession(r)
		user, err := getUserByUsername(s.username)
		if err != nil || user.Role != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

// requireSignature blocks access to invoice pages until the logged-in user
// has saved a signature on their account, redirecting them there otherwise.
func requireSignature(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, s := getSession(r)
		if s == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		user, err := getUserByUsername(s.username)
		if err != nil || user.Signature == "" {
			http.Redirect(w, r, "/account/signature?missing=1", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// requireCSRF validates the csrf_token form field on state-changing POST
// requests against the current session's token.
func requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, s := getSession(r)
		if s == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
			if strings.HasPrefix(contentType, "multipart/form-data") {
				if err := r.ParseMultipartForm(32 << 20); err != nil {
					http.Error(w, "invalid form", http.StatusBadRequest)
					return
				}
			} else {
				if err := r.ParseForm(); err != nil {
					http.Error(w, "invalid form", http.StatusBadRequest)
					return
				}
			}
			if r.FormValue("csrf_token") != s.csrfToken {
				http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		next := r.URL.Query().Get("next")
		if err := mustTemplate("login.html").Execute(w, map[string]string{
			"Next":  next,
			"Error": "",
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	ip := clientIP(r)
	if isLockedOut(ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		mustTemplate("login.html").Execute(w, map[string]string{
			"Next":  r.FormValue("next"),
			"Error": "Too many failed attempts. Try again in 15 minutes.",
		})
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := getUserByUsername(username)
	valid := err == nil && bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) == nil
	if !valid {
		recordLoginFailure(ip)
		w.WriteHeader(http.StatusUnauthorized)
		mustTemplate("login.html").Execute(w, map[string]string{
			"Next":  r.FormValue("next"),
			"Error": "Invalid username or password.",
		})
		return
	}

	clearLoginFailures(ip)
	token, _, err := createSession(user.Username, user.Role)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, r, token)

	http.Redirect(w, r, "/admin/service-requests", http.StatusSeeOther)
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if token, s := getSession(r); s != nil {
		deleteSession(token)
	}
	clearSessionCookie(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// isAdmin reports whether the current request's session belongs to an admin.
func isAdmin(r *http.Request) bool {
	_, s := getSession(r)
	if s == nil {
		return false
	}
	user, err := getUserByUsername(s.username)
	return err == nil && user.Role == "admin"
}

// currentUsername returns the logged-in user's username, or "" if none.
func currentUsername(r *http.Request) string {
	_, s := getSession(r)
	if s == nil {
		return ""
	}
	return s.username
}

// currentDisplayName returns the logged-in user's full name (falling back to
// their username), used for the nav bar badge.
func currentDisplayName(r *http.Request) string {
	username := currentUsername(r)
	if username == "" {
		return ""
	}
	user, err := getUserByUsername(username)
	if err != nil {
		return username
	}
	return user.FullName()
}

// accountPasswordHandler updates the logged-in admin's own password, then
// redirects back to the Workers page (which now hosts the "My Account" panel).
func accountPasswordHandler(w http.ResponseWriter, r *http.Request) {
	_, s := getSession(r)
	currentPassword := r.FormValue("current_password")
	newPassword := r.FormValue("new_password")
	confirmPassword := r.FormValue("confirm_password")

	user, err := getUserByUsername(s.username)
	var acctErr string
	switch {
	case err != nil:
		acctErr = "Account not found."
	case bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(currentPassword)) != nil:
		acctErr = "Current password is incorrect."
	case len(newPassword) < 4:
		acctErr = "New password must be at least 4 characters."
	case newPassword != confirmPassword:
		acctErr = "New password and confirmation do not match."
	default:
		if err := updateUserPassword(user.ID, newPassword); err != nil {
			acctErr = err.Error()
		}
	}

	if acctErr != "" {
		http.Redirect(w, r, "/admin/users?accterror="+url.QueryEscape(acctErr), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/users?acctsuccess=1", http.StatusSeeOther)
}

// UsersPageView is the template data for the admin Workers page, which also
// hosts the "My Account" (change own password) panel.
type UsersPageView struct {
	Users       []User
	CSRFToken   string
	Error       string
	Username    string
	DisplayName string
	AcctError   string
	AcctSuccess bool
	IsAdmin     bool
}

func usersListHandler(w http.ResponseWriter, r *http.Request) {
	users, err := listUsers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view := UsersPageView{
		Users:       users,
		CSRFToken:   currentCSRFToken(r),
		Error:       r.URL.Query().Get("error"),
		Username:    currentUsername(r),
		DisplayName: currentDisplayName(r),
		AcctError:   r.URL.Query().Get("accterror"),
		AcctSuccess: r.URL.Query().Get("acctsuccess") == "1",
		IsAdmin:     isAdmin(r),
	}
	if err := mustTemplate("users.html").Execute(w, view); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// usersCreateHandler adds a brand-new worker, or, if the given username
// already belongs to an existing account, updates that account's name,
// role, and (if provided) password instead of erroring out on a duplicate.
func usersCreateHandler(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	role := r.FormValue("role")
	firstName := r.FormValue("first_name")
	lastName := r.FormValue("last_name")
	if role != "admin" {
		role = "user"
	}

	if username == "" {
		http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("Username is required."), http.StatusSeeOther)
		return
	}

	if existing, err := getUserByUsername(username); err == nil {
		if existing.Role == "admin" && role != "admin" {
			adminCount, err := countAdmins()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if adminCount <= 1 {
				http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("Cannot demote the last remaining admin."), http.StatusSeeOther)
				return
			}
		}
		if err := updateUserName(existing.ID, firstName, lastName); err != nil {
			http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("Could not update worker: "+err.Error()), http.StatusSeeOther)
			return
		}
		if err := updateUserRole(existing.ID, role); err != nil {
			http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("Could not update worker: "+err.Error()), http.StatusSeeOther)
			return
		}
		if password != "" {
			if len(password) < 4 {
				http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("Password must be at least 4 characters."), http.StatusSeeOther)
				return
			}
			if err := updateUserPassword(existing.ID, password); err != nil {
				http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("Could not update worker: "+err.Error()), http.StatusSeeOther)
				return
			}
		}
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	if len(password) < 4 {
		http.Redirect(w, r, "/admin/users?error=Username+required+and+password+must+be+at+least+4+characters.", http.StatusSeeOther)
		return
	}
	if err := createUser(username, password, role, firstName, lastName); err != nil {
		http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("Could not create worker: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func usersDeleteHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	user, err := getUserByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if user.Username == currentUsername(r) {
		http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("You cannot delete your own account."), http.StatusSeeOther)
		return
	}
	if user.Role == "admin" {
		adminCount, err := countAdmins()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if adminCount <= 1 {
			http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("Cannot delete the last remaining admin."), http.StatusSeeOther)
			return
		}
	}
	if err := deleteUser(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// usersRoleHandler promotes/demotes a worker between "admin" and "user".
func usersRoleHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	user, err := getUserByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	newRole := r.FormValue("role")
	if newRole != "admin" && newRole != "user" {
		http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("Invalid role."), http.StatusSeeOther)
		return
	}
	if user.Username == currentUsername(r) {
		http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("You cannot change your own role."), http.StatusSeeOther)
		return
	}
	if user.Role == "admin" && newRole != "admin" {
		adminCount, err := countAdmins()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if adminCount <= 1 {
			http.Redirect(w, r, "/admin/users?error="+url.QueryEscape("Cannot demote the last remaining admin."), http.StatusSeeOther)
			return
		}
	}
	if err := updateUserRole(id, newRole); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// usersNameHandler updates a worker's first/last name.
func usersNameHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := getUserByID(id); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := updateUserName(id, r.FormValue("first_name"), r.FormValue("last_name")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}
