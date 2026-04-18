package notify

import (
	"context"
	"errors"
	"fmt"
	"net/smtp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuo-user/k8s-ai-remediator/internal/model"
)

func sampleResult() DecisionResult {
	return DecisionResult{
		Namespace:    "incident-lab",
		Kind:         "Pod",
		Name:         "memory-hog-xxxx",
		EventReason:  "BackOff",
		EventMessage: "Back-off restarting failed container",
		Decision: model.Decision{
			Action:        model.ActionPatchResources,
			Severity:      "high",
			Confidence:    0.95,
			Summary:       "Container OOMKilled, raise memory_limit",
			ProbableCause: "Insufficient memory limit vs workload",
			Parameters: map[string]string{
				"container":      "app",
				"memory_limit":   "256Mi",
				"memory_request": "128Mi",
			},
		},
	}
}

func TestBuildSubjectAndBody(t *testing.T) {
	r := sampleResult()
	subj := buildSubject(r)
	if !strings.Contains(subj, "patch_resources") || !strings.Contains(subj, "incident-lab/memory-hog-xxxx") {
		t.Errorf("unexpected subject: %s", subj)
	}

	body := buildBody(r)
	for _, want := range []string{
		"Situazione anomala",
		"Namespace: incident-lab",
		"Reason: BackOff",
		"Decisione presa",
		"Action: patch_resources",
		"memory_limit: 256Mi",
		"Situazione post intervento",
		"Azione applicata con successo",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestBuildBody_ReportsExecutionError(t *testing.T) {
	r := sampleResult()
	r.ExecutionErr = errors.New("patch_resources blocked: disabled by policy")
	body := buildBody(r)
	if !strings.Contains(body, "Azione non applicata") || !strings.Contains(body, "disabled by policy") {
		t.Errorf("body missing failure details:\n%s", body)
	}
}

func TestBuildMessage_HasHeaders(t *testing.T) {
	msg := buildMessage("alice@icloud.com", "bob@icloud.com", "hi", "body\n")
	s := string(msg)
	for _, want := range []string{
		"From: alice@icloud.com",
		"To: bob@icloud.com",
		"Subject: hi",
		"MIME-Version: 1.0",
		"\r\n\r\nbody",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("message missing %q:\n%s", want, s)
		}
	}
}

func TestNew_FallsBackToNoop(t *testing.T) {
	cases := []SMTPConfig{
		{},                                                 // all empty
		{Host: "smtp.mail.me.com", User: "u@icloud.com"},   // missing To
		{Host: "smtp.mail.me.com", To: "r@icloud.com"},     // missing User
		{User: "u@icloud.com", To: "r@icloud.com"},         // missing Host
	}
	for i, c := range cases {
		n := New(c)
		if _, ok := n.(noopNotifier); !ok {
			t.Errorf("case %d: expected noop, got %T", i, n)
		}
		// Must not panic or block.
		n.NotifyDecision(context.Background(), sampleResult())
	}
}

func TestNew_ConfiguresDefaults(t *testing.T) {
	n := New(SMTPConfig{
		Host:     "smtp.mail.me.com",
		User:     "user@icloud.com",
		Password: "secret",
		To:       "recipient@icloud.com",
	})
	sm, ok := n.(*smtpNotifier)
	if !ok {
		t.Fatalf("expected *smtpNotifier, got %T", n)
	}
	if sm.cfg.Port != 587 {
		t.Errorf("expected default port 587, got %d", sm.cfg.Port)
	}
	if sm.cfg.From != "user@icloud.com" {
		t.Errorf("expected From to fallback to User, got %q", sm.cfg.From)
	}
	if sm.cfg.MinSeverity != model.SeverityMedium {
		t.Errorf("expected default MinSeverity medium, got %s", sm.cfg.MinSeverity)
	}
}

func TestSMTPNotifier_SkipsBelowMinSeverity(t *testing.T) {
	var called int32
	n := &smtpNotifier{
		cfg: SMTPConfig{Host: "x", User: "u", Password: "p", From: "a", To: "b", Port: 587, MinSeverity: model.SeverityHigh},
		send: func(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
			atomic.AddInt32(&called, 1)
			return nil
		},
	}
	r := sampleResult()
	r.Decision.Severity = "low"
	n.NotifyDecision(context.Background(), r)
	time.Sleep(50 * time.Millisecond) // allow goroutine to run (it shouldn't)
	if atomic.LoadInt32(&called) != 0 {
		t.Errorf("send should be skipped below minimum severity, got %d calls", called)
	}
}

func TestSMTPNotifier_SendsAboveMinSeverity(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []byte
		done = make(chan struct{})
	)
	n := &smtpNotifier{
		cfg: SMTPConfig{Host: "smtp.test", User: "u", Password: "p", From: "a@x", To: "b@x", Port: 587, MinSeverity: model.SeverityMedium},
		send: func(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
			mu.Lock()
			seen = append([]byte(nil), msg...)
			mu.Unlock()
			close(done)
			return nil
		},
	}
	n.NotifyDecision(context.Background(), sampleResult())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("send goroutine did not complete in time")
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(string(seen), "Subject: [ai-remediator] patch_resources") {
		t.Errorf("unexpected message:\n%s", seen)
	}
}

func TestSMTPNotifier_WrapsSendError(t *testing.T) {
	// Directly exercise dispatch to observe the error path without goroutine
	// timing complications.
	n := &smtpNotifier{
		cfg: SMTPConfig{Host: "smtp.test", User: "u", From: "a@x", To: "b@x", Port: 587},
		send: func(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
			return fmt.Errorf("connection refused")
		},
	}
	err := n.dispatch(context.Background(), sampleResult())
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("expected send error to be surfaced, got %v", err)
	}
}
