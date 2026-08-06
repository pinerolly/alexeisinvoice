// Authentication: single hardcoded username with a bcrypt-hashed password
// read from the environment, server-side sessions, CSRF protection, and a
// simple brute-force lockout for the login endpoint.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os"
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

var (
	authUsername     string
	authPasswordHash []byte
)

// loadAuthConfig reads AUTH_USERNAME (default "alexies") and the required
// AUTH_PASSWORD_HASH (a bcrypt hash) from the environment. It fails fast if
// the hash is missing or invalid so the app never starts without auth.
func loadAuthConfig() error {
	authUsername = os.Getenv("AUTH_USERNAME")
	if authUsername == "" {
		authUsername = "alexies"
	}

	hash := os.Getenv("AUTH_PASSWORD_HASH")
	if hash == "" {
		return errors.New(
			"AUTH_PASSWORD_HASH environment variable is not set.\n" +
				"Generate one with:  invoiceapp.exe -hashpassword\n" +
				"then set AUTH_PASSWORD_HASH to the printed value before starting the server.",
		)
	}
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		return errors.New("AUTH_PASSWORD_HASH is not a valid bcrypt hash: " + err.Error())
	}
	authPasswordHash = []byte(hash)
	return nil
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

func createSession(username string) (token string, s *session, err error) {
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

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		// No MaxAge/Expires: session cookie, cleared when the browser closes.
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
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
			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form", http.StatusBadRequest)
				return
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

	valid := username == authUsername && bcrypt.CompareHashAndPassword(authPasswordHash, []byte(password)) == nil
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
	token, _, err := createSession(username)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, token)

	next := r.FormValue("next")
	if next == "" || next == "/login" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if token, s := getSession(r); s != nil {
		deleteSession(token)
	}
	clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
