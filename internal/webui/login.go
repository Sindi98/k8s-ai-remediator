package webui

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"
)

// handleLoginPage renders the login form. Already-authenticated users are
// bounced back to the dashboard so refreshing /login mid-session does
// not show a stale form.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl, ok := s.pages["login.html"]
	if !ok {
		http.Error(w, "login template missing", http.StatusInternalServerError)
		return
	}
	data := loginPageData{
		Error: r.URL.Query().Get("error"),
		Next:  sanitiseNext(r.URL.Query().Get("next")),
	}
	if err := tmpl.ExecuteTemplate(w, "login.html", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// handleLoginSubmit validates the credentials posted from the login form,
// sets the session cookie and redirects to the originally-requested page
// (or "/" by default). Wrong credentials re-render the form with an
// error in the URL so a refresh does not resubmit the password.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	user := strings.TrimSpace(r.FormValue("username"))
	pass := r.FormValue("password")
	next := sanitiseNext(r.FormValue("next"))

	expectedUser := []byte(s.opts.Username)
	expectedPass := []byte(s.opts.Password)
	userMatch := subtle.ConstantTimeCompare([]byte(user), expectedUser) == 1
	passMatch := subtle.ConstantTimeCompare([]byte(pass), expectedPass) == 1
	if !userMatch || !passMatch {
		http.Redirect(w, r, "/login?error=invalid&next="+url_QueryEscape(next), http.StatusSeeOther)
		return
	}

	expiry := time.Now().Add(sessionTTL)
	value := signSession(user, expiry, sessionKey(s.opts.Password))
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Secure intentionally NOT set: the GUI may be reached over plain
		// HTTP on local clusters (port-forward). Production deployments
		// MUST front the GUI with TLS at the Ingress, at which point the
		// proxy adds the Secure attribute automatically when forwarding.
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// handleLogout clears the session cookie and bounces the user to /login.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// currentUser returns the username from a valid session cookie, or empty
// string. Used to render the navbar with a "logout" button only when
// signed in.
func (s *Server) currentUser(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return verifySession(c.Value, sessionKey(s.opts.Password), time.Now())
}

// sanitiseNext keeps only same-origin relative paths in the post-login
// redirect target, so that /login?next=//evil.example.com cannot be
// abused as an open redirect.
func sanitiseNext(next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return "/"
	}
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

type loginPageData struct {
	Error string
	Next  string
}
