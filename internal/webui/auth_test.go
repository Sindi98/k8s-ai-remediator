package webui

import (
	"testing"
	"time"
)

func TestSessionRoundTrip(t *testing.T) {
	key := sessionKey("hunter2")
	now := time.Now()
	cookie := signSession("alice", now.Add(time.Hour), key)
	if got := verifySession(cookie, key, now); got != "alice" {
		t.Fatalf("verify roundtrip: got %q want %q", got, "alice")
	}
}

func TestSessionRejectsTamperedSig(t *testing.T) {
	key := sessionKey("hunter2")
	now := time.Now()
	cookie := signSession("alice", now.Add(time.Hour), key)
	tampered := cookie[:len(cookie)-2] + "XX"
	if got := verifySession(tampered, key, now); got != "" {
		t.Fatalf("tampered cookie accepted: got user %q", got)
	}
}

func TestSessionRejectsExpired(t *testing.T) {
	key := sessionKey("hunter2")
	past := time.Now().Add(-time.Hour)
	cookie := signSession("alice", past, key)
	if got := verifySession(cookie, key, time.Now()); got != "" {
		t.Fatalf("expired cookie accepted: got user %q", got)
	}
}

func TestSessionRejectsDifferentKey(t *testing.T) {
	now := time.Now()
	cookie := signSession("alice", now.Add(time.Hour), sessionKey("hunter2"))
	if got := verifySession(cookie, sessionKey("changed"), now); got != "" {
		t.Fatalf("cookie signed with old password accepted after rotation: got %q", got)
	}
}

func TestSanitiseNextRejectsExternalRedirects(t *testing.T) {
	cases := map[string]string{
		"":                      "/",
		"/dashboard":            "/dashboard",
		"//evil.example.com":    "/",
		"https://evil.com/path": "/",
		"javascript:alert(1)":   "/",
		"   ":                   "/",
		"/config?x=1":           "/config?x=1",
	}
	for in, want := range cases {
		if got := sanitiseNext(in); got != want {
			t.Errorf("sanitiseNext(%q) = %q, want %q", in, got, want)
		}
	}
}
