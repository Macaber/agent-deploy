package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed auth-ui.html
var authFS embed.FS

const (
	sessionCookieName = "oss_migrate_session"
	defaultSessionTTL = 12 * time.Hour
	loginThrottleMax  = 8
	loginThrottleWin  = 5 * time.Minute
)

type authenticator struct {
	enabled  bool
	user     string
	passHash [32]byte
	secret   []byte
	ttl      time.Duration

	mu        sync.Mutex
	throttles map[string][]time.Time
}

func newAuthenticator() *authenticator {
	user := os.Getenv("OSS_MIGRATE_AUTH_USER")
	if user == "" {
		user = os.Getenv("MIGRATE_AUTH_USER")
	}
	if user == "" {
		user = "admin"
	}
	pass := os.Getenv("OSS_MIGRATE_AUTH_PASSWORD")
	if pass == "" {
		pass = os.Getenv("MIGRATE_AUTH_PASSWORD")
	}
	if pass == "" {
		log.Println("WARN: OSS_MIGRATE_AUTH_PASSWORD is empty; authentication is DISABLED")
		return &authenticator{
			enabled:   false,
			user:      user,
			throttles: make(map[string][]time.Time),
		}
	}

	passHash := sha256.Sum256([]byte(pass))
	h := hmac.New(sha256.New, []byte("oss-migrate-server-cookie-key-v1"))
	h.Write(passHash[:])
	secret := h.Sum(nil)

	log.Printf("INFO: authentication ENABLED for user %q", user)
	return &authenticator{
		enabled:   true,
		user:      user,
		passHash:  passHash,
		secret:    secret,
		ttl:       defaultSessionTTL,
		throttles: make(map[string][]time.Time),
	}
}

func (a *authenticator) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.enabled {
			next(w, r)
			return
		}

		if r.URL.Path == "/health" {
			next(w, r)
			return
		}

		if a.hasValidSession(r) {
			next(w, r)
			return
		}

		// API requests get 401 JSON
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized","loginRequired":true}`))
			return
		}

		// Web requests redirect to /login
		loginURL := "/login"
		if p := getForwardedPrefix(r); p != "" {
			loginURL = strings.TrimSuffix(p, "/") + "/login"
		}
		http.Redirect(w, r, loginURL, http.StatusFound)
	}
}

func (a *authenticator) hasValidSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return false
	}
	user := parts[0]
	expStr := parts[1]
	sigHex := parts[2]

	if subtle.ConstantTimeCompare([]byte(user), []byte(a.user)) != 1 {
		return false
	}

	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}

	expectedSig := a.sign(user + "." + expStr)
	actualSig, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}

	return hmac.Equal(actualSig, expectedSig)
}

func (a *authenticator) sign(msg string) []byte {
	h := hmac.New(sha256.New, a.secret)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

func (a *authenticator) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if a.hasValidSession(r) {
		home := "/"
		if p := getForwardedPrefix(r); p != "" {
			home = strings.TrimSuffix(p, "/") + "/"
		}
		http.Redirect(w, r, home, http.StatusFound)
		return
	}
	b, err := authFS.ReadFile("auth-ui.html")
	if err != nil {
		http.Error(w, "login page template missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

func (a *authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := clientIP(r)
	if a.isThrottled(ip) {
		writeErr(w, http.StatusTooManyRequests, "登录尝试过于频繁，请稍后再试")
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(a.user)) == 1
	passHash := sha256.Sum256([]byte(req.Password))
	passOK := subtle.ConstantTimeCompare(passHash[:], a.passHash[:]) == 1

	if !userOK || !passOK {
		a.recordFailure(ip)
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}

	exp := time.Now().Add(a.ttl).Unix()
	payload := fmt.Sprintf("%s.%d", a.user, exp)
	sig := hex.EncodeToString(a.sign(payload))
	token := fmt.Sprintf("%s.%s", payload, sig)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  time.Unix(exp, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok", "user": a.user})
}

func (a *authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	home := "/login"
	if p := getForwardedPrefix(r); p != "" {
		home = strings.TrimSuffix(p, "/") + "/login"
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	http.Redirect(w, r, home, http.StatusFound)
}

func (a *authenticator) isThrottled(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	times := a.throttles[ip]
	now := time.Now()
	var recent []time.Time
	for _, t := range times {
		if now.Sub(t) < loginThrottleWin {
			recent = append(recent, t)
		}
	}
	a.throttles[ip] = recent
	return len(recent) >= loginThrottleMax
}

func (a *authenticator) recordFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.throttles[ip] = append(a.throttles[ip], time.Now())
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if rip := r.Header.Get("X-Real-IP"); rip != "" {
		return strings.TrimSpace(rip)
	}
	idx := strings.LastIndex(r.RemoteAddr, ":")
	if idx != -1 {
		return r.RemoteAddr[:idx]
	}
	return r.RemoteAddr
}

func getForwardedPrefix(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Prefix"); p != "" {
		return p
	}
	if p := os.Getenv("OSS_MIGRATE_URL_PREFIX"); p != "" {
		return p
	}
	return os.Getenv("URL_PREFIX")
}
