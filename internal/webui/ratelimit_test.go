package webui

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginThrottle_LocksAfterMaxFailures(t *testing.T) {
	now := time.Now()
	tr := newLoginThrottle()
	tr.now = func() time.Time { return now }

	// First max-1 failures stay allowed.
	for i := 0; i < tr.max-1; i++ {
		tr.recordFailure("1.2.3.4")
		if !tr.allowed("1.2.3.4") {
			t.Fatalf("should still be allowed after %d failures", i+1)
		}
	}
	// The max-th failure locks the key.
	tr.recordFailure("1.2.3.4")
	if tr.allowed("1.2.3.4") {
		t.Fatal("expected lockout after reaching max failures")
	}
	// An unrelated key is unaffected.
	if !tr.allowed("9.9.9.9") {
		t.Fatal("unrelated key should not be locked")
	}
}

func TestLoginThrottle_UnlocksAfterLockout(t *testing.T) {
	now := time.Now()
	tr := newLoginThrottle()
	tr.now = func() time.Time { return now }

	for i := 0; i < tr.max; i++ {
		tr.recordFailure("1.2.3.4")
	}
	if tr.allowed("1.2.3.4") {
		t.Fatal("expected lockout")
	}
	// Advance past the lockout window.
	now = now.Add(tr.lockout + time.Second)
	if !tr.allowed("1.2.3.4") {
		t.Fatal("expected key to be allowed again after lockout expires")
	}
}

func TestLoginThrottle_SuccessResets(t *testing.T) {
	now := time.Now()
	tr := newLoginThrottle()
	tr.now = func() time.Time { return now }

	for i := 0; i < tr.max-1; i++ {
		tr.recordFailure("1.2.3.4")
	}
	tr.recordSuccess("1.2.3.4")
	// After a success the counter is cleared, so one more failure must not lock.
	tr.recordFailure("1.2.3.4")
	if !tr.allowed("1.2.3.4") {
		t.Fatal("a single failure after success should not lock the key")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.7:54321"
	if got := clientIP(r); got != "203.0.113.7" {
		t.Errorf("clientIP = %q, want 203.0.113.7", got)
	}
	// No port: fall back to the raw value.
	r.RemoteAddr = "203.0.113.7"
	if got := clientIP(r); got != "203.0.113.7" {
		t.Errorf("clientIP without port = %q, want 203.0.113.7", got)
	}
}
