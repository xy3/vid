package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "vid_session"
	oauthCookie   = "vid_oauth"
	sessionTTL    = 90 * 24 * time.Hour
)

// ---------- per-IP rate limiting on the OAuth callback ----------
//
// The callback verifies a `state` and does two upstream calls; without a cap,
// a hostile client can make us hammer Google's token endpoint.

type limiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

const (
	maxAttempts = 20
	attemptWin  = 10 * time.Minute
)

func newLimiter() *limiter { return &limiter{attempts: map[string][]time.Time{}} }

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-attemptWin)
	kept := l.attempts[key][:0]
	for _, t := range l.attempts[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= maxAttempts {
		l.attempts[key] = kept
		return false
	}
	l.attempts[key] = append(kept, time.Now())
	return true
}

// clientIP trusts Cloudflare's header because every real request arrives
// through the tunnel and would otherwise look like localhost. Safe only
// because nothing but the tunnel can reach this process.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- Google OAuth (authorization code + PKCE) ----------
//
// Ported from ~/apps/trip/server/auth.js. The flow is a redirect, a form POST
// and a JSON GET — not worth a dependency. A single-use `state` cookie ties
// the callback to the browser that started it; PKCE covers the code in transit.

const (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserinfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

func s256(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *server) redirectURI() string { return s.cfg.BaseURL + "/auth/google/callback" }

func (s *server) handleGoogleStart(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	state := randomToken()
	verifier := randomToken()

	q := url.Values{
		"client_id":             {s.cfg.GoogleClientID},
		"redirect_uri":          {s.redirectURI()},
		"response_type":         {"code"},
		"scope":                 {"openid email profile"},
		"state":                 {state},
		"code_challenge":        {s256(verifier)},
		"code_challenge_method": {"S256"},
		"access_type":           {"online"},
		"prompt":                {"select_account"},
	}

	http.SetCookie(w, &http.Cookie{
		Name:     oauthCookie,
		Value:    state + ":" + verifier,
		Path:     "/auth/google",
		MaxAge:   600,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, googleAuthURL+"?"+q.Encode(), http.StatusFound)
}

func (s *server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		s.logf("oauth: rate limited %s", ip)
		s.renderNotice(w, http.StatusTooManyRequests, "Too many attempts", "Give it a few minutes and try again.")
		return
	}

	// Clear the state cookie on every exit path — it is single use.
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Path: "/auth/google", MaxAge: -1})

	if e := r.URL.Query().Get("error"); e != "" {
		s.renderNotice(w, http.StatusBadRequest, "Sign-in cancelled", "You can close this tab, or head back and try again.")
		return
	}

	c, err := r.Cookie(oauthCookie)
	if err != nil {
		s.renderNotice(w, http.StatusBadRequest, "The sign-in link expired", "Start again from the home page.")
		return
	}
	savedState, verifier, ok := strings.Cut(c.Value, ":")
	gotState := r.URL.Query().Get("state")
	if !ok || savedState == "" || subtle.ConstantTimeCompare([]byte(savedState), []byte(gotState)) != 1 {
		s.renderNotice(w, http.StatusBadRequest, "The sign-in link expired", "Start again from the home page.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	prof, err := s.exchangeCode(ctx, r.URL.Query().Get("code"), verifier)
	if err != nil {
		s.logf("oauth: exchange failed for %s: %v", ip, err)
		s.renderNotice(w, http.StatusBadGateway, "Sign-in failed", "Google didn't complete the handshake. Try again in a moment.")
		return
	}

	if !prof.EmailVerified || prof.Email == "" {
		s.renderNotice(w, http.StatusForbidden, "Unverified account", "That Google account has no verified email address.")
		return
	}
	if !s.cfg.allowed(prof.Email) {
		s.logf("oauth: %q not on the allowlist", prof.Email)
		s.renderNotice(w, http.StatusForbidden, "Not on the list",
			"This is a private instance. "+prof.Email+" hasn't been granted access.")
		return
	}

	u, err := s.store.upsertUser(prof.Sub, prof.Email, prof.Name, prof.Picture)
	if err != nil {
		s.logf("oauth: upsert user: %v", err)
		s.renderNotice(w, http.StatusInternalServerError, "Something broke", "Couldn't create your session. Try again.")
		return
	}

	token := randomToken()
	if err := s.store.createSession(token, u.ID, sessionTTL); err != nil {
		s.logf("oauth: create session: %v", err)
		s.renderNotice(w, http.StatusInternalServerError, "Something broke", "Couldn't create your session. Try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

type googleProfile struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

func (s *server) exchangeCode(ctx context.Context, code, verifier string) (*googleProfile, error) {
	if code == "" {
		return nil, errors.New("no code in callback")
	}
	form := url.Values{
		"client_id":     {s.cfg.GoogleClientID},
		"client_secret": {s.cfg.GoogleClientSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {s.redirectURI()},
		"code_verifier": {verifier},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var tok struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&tok); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		if tok.Error != "" {
			return nil, fmt.Errorf("token endpoint: %s: %s", tok.Error, tok.ErrorDescription)
		}
		return nil, fmt.Errorf("token endpoint returned no access_token (%d)", res.StatusCode)
	}

	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, googleUserinfoURL, nil)
	req2.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	res2, err := httpClient.Do(req2)
	if err != nil {
		return nil, err
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo returned %d", res2.StatusCode)
	}
	var prof googleProfile
	if err := json.NewDecoder(io.LimitReader(res2.Body, 1<<20)).Decode(&prof); err != nil {
		return nil, fmt.Errorf("decode userinfo: %w", err)
	}
	if prof.Sub == "" {
		return nil, errors.New("userinfo had no subject")
	}
	return &prof, nil
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.deleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	if r.Method == http.MethodGet {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "signed out"})
}

// ---------- gates ----------

func (s *server) currentUser(r *http.Request) *user {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	u, err := s.store.sessionUser(c.Value)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.logf("session lookup: %v", err)
		}
		return nil
	}
	return u
}

func (s *server) requireUser(h func(http.ResponseWriter, *http.Request, *user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			writeErr(w, http.StatusUnauthorized, "sign in required")
			return
		}
		// A state-changing request must come from our own origin. Combined with
		// the SameSite=Lax session cookie this stops cross-site writes.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.sameOrigin(r) {
			writeErr(w, http.StatusForbidden, "bad origin")
			return
		}
		h(w, r, u)
	}
}

func (s *server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	if o == s.cfg.BaseURL {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}
