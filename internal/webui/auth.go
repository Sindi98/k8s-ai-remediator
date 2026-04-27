package webui

import (
	"crypto/subtle"
	"net/http"
)

// basicAuth wraps next with HTTP Basic authentication. Credentials are
// compared in constant time so that a network attacker cannot recover the
// expected username/password by timing the response.
//
// The webui is meant to be exposed only behind HTTPS (Ingress TLS
// termination): basic-auth credentials are sent in clear text and only
// HTTPS keeps them confidential on the wire.
func (s *Server) basicAuth(next http.Handler) http.Handler {
	expectedUser := []byte(s.opts.Username)
	expectedPass := []byte(s.opts.Password)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			s.unauthorized(w)
			return
		}
		userMatch := subtle.ConstantTimeCompare([]byte(user), expectedUser) == 1
		passMatch := subtle.ConstantTimeCompare([]byte(pass), expectedPass) == 1
		if !userMatch || !passMatch {
			s.unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="ai-remediator", charset="UTF-8"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
