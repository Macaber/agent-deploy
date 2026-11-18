package main

// auth.go adds a self-contained login gate to the migrate-server.
//
// The tool can delete Workspace CRs, PVCs and PVs, so exposing it on a NodePort
// with no authentication is unsafe. This file adds a single-user login with no
// third-party dependencies, so the binary keeps its single-binary + embedded-UI
// design.
//
// Model:
//   - Credentials come from the environment (MIGRATE_AUTH_USER / MIGRATE_AUTH_PASSWORD).
//   - When MIGRATE_AUTH_PASSWORD is empty, auth is DISABLED (backward compatible;
//     a warning is logged) so an existing deployment keeps working until the
//     operator sets a password.
//   - On successful POST /login we set an HMAC-signed, HttpOnly, SameSite=Lax,
//     expiring session cookie. The signing key is derived from the password, so
//     only MIGRATE_AUTH_USER + MIGRATE_AUTH_PASSWORD are needed; changing the
//     password invalidates existing sessions.
//   - Every route except /health and the login page/endpoint requires a valid
//     session. UI requests (GET, not under /api) are redirected to the login
//     page; API requests get 401 JSON so the frontend can bounce to login.
//   - The login page is embedded (auth-ui.html) and shares the console's look.
//   - Failed logins are throttled in memory per client IP to slow brute force.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
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
	sessionCookieName = "migrate_session"
	defaultSessionTTL = 12 * time.Hour
	loginThrottleMax  = 8               // failures allowed ...
	loginThrottleWin  = 5 * time.Minute // ... within this window per IP
)

// authenticator holds the resolved auth configuration and session signing key.
type authenticator struct {
	enabled  bool
	user     string
	passHash [32]byte // sha256 of the configured password
	secret   []byte
	ttl      time.Duration

	mu    sync.Mutex
	fails map[string]*failRecord
}

type failRecord struct {
	count   int
	resetAt time.Time
}

// newAuthenticator resolves configuration from the environment. It logs (but
// never crashes) when auth is disabled.
func newAuthenticator() *authenticator {
	user := strings.TrimSpace(os.Getenv("MIGRATE_AUTH_USER"))
	if user == "" {
		user = "admin"
	}
	pass := os.Getenv("MIGRATE_AUTH_PASSWORD")

	ttl := defaultSessionTTL
	if v := os.Getenv("MIGRATE_AUTH_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			ttl = d
		}
	}

	// The session-signing key is derived from the password itself, so only
	// username+password are needed. Changing the password invalidates all
	// existing sessions (a sensible, safe side effect).
	secret := sha256.Sum256([]byte("migrate-session-v1|" + pass))

	a := &authenticator{
		enabled: pass != "",
		user:    user,
		secret:  secret[:],
		ttl:     ttl,
		fails:   map[string]*failRecord{},
	}
	a.passHash = sha256.Sum256([]byte(pass))
	if !a.enabled {
		log.Printf("auth: WARNING MIGRATE_AUTH_PASSWORD is empty — authentication is DISABLED; the console is open to anyone who can reach it")
	} else {
		log.Printf("auth: login enabled for user %q (session TTL %s)", user, ttl)
	}
	return a
}

// checkCredentials compares user+password in constant time.
func (a *authenticator) checkCredentials(user, pass string) bool {
	uh := sha256.Sum256([]byte(user))
	refU := sha256.Sum256([]byte(a.user))
	ph := sha256.Sum256([]byte(pass))
	userOK := subtle.ConstantTimeCompare(uh[:], refU[:]) == 1
	passOK := subtle.ConstantTimeCompare(a.passHash[:], ph[:]) == 1
	return userOK && passOK
}

// sign builds the cookie value: base64(exp||hmac(exp)).
func (a *authenticator) sign(exp int64) string {
	mac := hmac.New(sha256.New, a.secret)
	fmt.Fprintf(mac, "v1|%d", exp)
	sum := mac.Sum(nil)
	raw := fmt.Sprintf("%d|%s", exp, hex.EncodeToString(sum))
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// verify validates a cookie value and reports whether it is authentic and unexpired.
func (a *authenticator) verify(v string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return false
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, a.secret)
	fmt.Fprintf(mac, "v1|%d", exp)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(parts[1]))
}

// authenticated reports whether the request carries a valid session.
func (a *authenticator) authenticated(r *http.Request) bool {
	if !a.enabled {
		return true
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	return a.verify(c.Value)
}

// throttled reports whether the client IP has too many recent failures.
func (a *authenticator) throttled(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := a.fails[ip]
	if rec == nil {
		return false
	}
	if time.Now().After(rec.resetAt) {
		delete(a.fails, ip)
		return false
	}
	return rec.count >= loginThrottleMax
}

func (a *authenticator) recordFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := a.fails[ip]
	if rec == nil || time.Now().After(rec.resetAt) {
		rec = &failRecord{resetAt: time.Now().Add(loginThrottleWin)}
		a.fails[ip] = rec
	}
	rec.count++
}

func (a *authenticator) clearFailures(ip string) {
	a.mu.Lock()
	delete(a.fails, ip)
	a.mu.Unlock()
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host
}

// isAPIRequest reports whether the request targets the JSON API (vs. the UI).
func isAPIRequest(r *http.Request) bool {
	_, rest := splitNodePrefix(r.URL.Path)
	return r.URL.Path == "/api/" || strings.HasPrefix(r.URL.Path, "/api/") ||
		strings.HasPrefix(rest, "/api/")
}

// requireAuth wraps h, enforcing a valid session. Unauthenticated UI requests
// are redirected to the login page; API requests get 401 JSON.
func (a *authenticator) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.authenticated(r) {
			h(w, r)
			return
		}
		if isAPIRequest(r) {
			writeErr(w, http.StatusUnauthorized, "login required")
			return
		}
		// UI: bounce to login, preserving where to return after sign-in.
		target := r.URL.RequestURI()
		http.Redirect(w, r, loginBasePath(r)+"?next="+urlQueryEscape(target), http.StatusSeeOther)
	}
}

// loginBasePath returns the path of the login page, keeping the /node/<self>/
// prefix when the console is opened under one.
func loginBasePath(r *http.Request) string {
	node, _ := splitNodePrefix(r.URL.Path)
	if node != "" {
		return "/node/" + node + "/login"
	}
	return "/login"
}

// urlQueryEscape escapes a value for use in a query string (minimal, no import).
func urlQueryEscape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' || c == '/' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

// safeNext only allows same-origin absolute paths to avoid open redirects.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return ""
	}
	return next
}

// handleLoginPage serves the embedded login form.
func (a *authenticator) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if a.authenticated(r) {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		if safeNext(r.URL.Query().Get("next")) == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		}
		return
	}
	b, err := authFS.ReadFile("auth-ui.html")
	if err != nil {
		http.Error(w, "login page not embedded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// handleLogin processes the form POST (and JSON) and sets the session cookie.
func (a *authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, loginBasePath(r), http.StatusSeeOther)
		return
	}
	ip := clientIP(r)
	if a.throttled(ip) {
		a.loginFail(w, r, "尝试过于频繁，请稍后再试")
		return
	}
	if err := r.ParseForm(); err != nil {
		a.loginFail(w, r, "请求格式错误")
		return
	}
	user := r.FormValue("username")
	pass := r.FormValue("password")
	if !a.checkCredentials(user, pass) {
		a.recordFailure(ip)
		log.Printf("auth: failed login for user %q from %s", user, ip)
		a.loginFail(w, r, "用户名或密码错误")
		return
	}
	a.clearFailures(ip)

	exp := time.Now().Add(a.ttl).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    a.sign(exp),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		Expires:  time.Unix(exp, 0),
	})
	next := safeNext(r.FormValue("next"))
	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// loginFail re-renders the login page with an error message (simple HTML).
func (a *authenticator) loginFail(w http.ResponseWriter, r *http.Request, msg string) {
	next := safeNext(r.FormValue("next"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w, `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/><title>登录 · OSS 迁移控制台</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0b0f17;color:#e8edf6;font-family:-apple-system,"PingFang SC",sans-serif}
.box{width:340px;padding:28px;background:#111827;border:1px solid #233047;border-radius:14px;box-shadow:0 12px 40px -16px #000}
h1{font-size:16px;margin:0 0 18px}.err{background:rgba(255,97,105,.12);color:#ffb4b6;border:1px solid rgba(255,97,105,.4);padding:9px 12px;border-radius:9px;font-size:13px;margin-bottom:14px}
label{display:block;font-size:12px;color:#8494ad;margin:12px 0 6px}input{width:100%%;box-sizing:border-box;padding:9px 12px;border-radius:9px;border:1px solid #233047;background:#161f30;color:#e8edf6;font-size:14px}
button{width:100%%;margin-top:18px;padding:10px;border-radius:9px;border:1px solid #3f7bf0;background:linear-gradient(180deg,#6aa0ff,#3f7bf0);color:#fff;font-size:14px;font-weight:600;cursor:pointer}</style></head>
<body><form class="box" method="POST" action="%s">
<h1>local-path → OSS 迁移控制台</h1>
<div class="err">%s</div>
<input type="hidden" name="next" value="%s"/>
<label>用户名</label><input name="username" autocomplete="username" autofocus/>
<label>密码</label><input name="password" type="password" autocomplete="current-password"/>
<button type="submit">登录</button></form></body></html>`,
		htmlEscape(loginBasePath(r)), htmlEscape(msg), htmlEscape(next))
}

// handleLogout clears the session cookie.
func (a *authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(0, 0),
	})
	if isAPIRequest(r) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
		return
	}
	http.Redirect(w, r, loginBasePath(r), http.StatusSeeOther)
}

// isHTTPS reports whether the request reached us over TLS (directly or via a
// terminating proxy that set X-Forwarded-Proto).
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// htmlEscape is a tiny escaper for the login-fail template.
func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}
