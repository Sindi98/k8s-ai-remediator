package webui

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie = "ai-remediator-session"
	sessionTTL    = 12 * time.Hour
)

// sessionKey derives the HMAC key used to sign session cookies from the
// configured password. Changing the password invalidates every existing
// session — a useful side effect for emergency rotation. The salt
// suffix exists so the cookie HMAC key is never identical to the bare
// SHA-256 of the password (defence in depth).
func sessionKey(password string) []byte {
	sum := sha256.Sum256([]byte(password + "|ai-remediator-session-v1"))
	return sum[:]
}

// signSession builds a cookie value of the form "<base64(payload)>.<base64(hmac)>"
// where payload is "<username>|<expiry-unix>". Decoding rejects tampered
// cookies and expired tokens.
func signSession(username string, expiresAt time.Time, key []byte) string {
	payload := username + "|" + strconv.FormatInt(expiresAt.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// verifySession returns the username encoded in the cookie, or empty string
// if the cookie is missing, malformed, signed with a different key, or
// expired.
func verifySession(cookieValue string, key []byte, now time.Time) string {
	parts := strings.SplitN(cookieValue, ".", 2)
	if len(parts) != 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ""
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), sig) {
		return ""
	}
	fields := strings.SplitN(string(payload), "|", 2)
	if len(fields) != 2 {
		return ""
	}
	expiresAt, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return ""
	}
	if now.Unix() >= expiresAt {
		return ""
	}
	return fields[0]
}

// requireAuth wraps next so that requests are accepted only if they carry
// a valid session cookie or correct HTTP Basic credentials. The latter
// keeps curl-based scripting working unchanged.
//
// Unauthenticated requests are redirected to /login (HTML) or answered
// with 401 (when the path is /api/* or the client asks for JSON), so
// EventSource and fetch() get a clean failure rather than a redirect to
// HTML they cannot parse.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	expectedUser := []byte(s.opts.Username)
	expectedPass := []byte(s.opts.Password)
	key := sessionKey(s.opts.Password)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			if user := verifySession(c.Value, key, time.Now()); user != "" {
				next.ServeHTTP(w, r)
				return
			}
		}
		if u, p, ok := r.BasicAuth(); ok {
			userMatch := subtle.ConstantTimeCompare([]byte(u), expectedUser) == 1
			passMatch := subtle.ConstantTimeCompare([]byte(p), expectedPass) == 1
			if userMatch && passMatch {
				next.ServeHTTP(w, r)
				return
			}
		}
		s.unauthorized(w, r)
	})
}

func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request) {
	if isAPIRequest(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="ai-remediator", charset="UTF-8"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Browser navigation: redirect to /login, preserving the original URL
	// so the user lands back on the page they wanted after signing in.
	next := r.URL.RequestURI()
	http.Redirect(w, r, "/login?next="+url_QueryEscape(next), http.StatusSeeOther)
}

// isAPIRequest detects fetch/EventSource/curl-style callers so we return
// 401 + WWW-Authenticate (compatible with basic-auth scripting) instead
// of a redirect to HTML they cannot follow.
func isAPIRequest(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") || strings.Contains(accept, "text/event-stream") {
		return true
	}
	return false
}

// url_QueryEscape avoids importing net/url just for one call. Equivalent
// behaviour to net/url.QueryEscape but inlined to keep this file
// dependency-light.
func url_QueryEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0F])
		}
	}
	return b.String()
}
