// Package notify sends short human-readable reports about remediation
// decisions. The only concrete implementation today is SMTP (PLAIN auth
// over STARTTLS, tested with iCloud Mail). When credentials are missing
// the factory returns a no-op notifier so the agent keeps working without
// extra configuration.
package notify

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/smtp"
	"sort"
	"strings"
	"time"

	"github.com/tuo-user/k8s-ai-remediator/internal/model"
)

// DecisionResult captures everything the notifier needs to summarise a
// single remediation cycle: the triggering event, the LLM decision and
// the outcome of execute.
type DecisionResult struct {
	Namespace    string
	Kind         string
	Name         string
	EventReason  string
	EventMessage string
	Decision     model.Decision
	ExecutionErr error
}

// Notifier is implemented by SMTP and the no-op fallback.
type Notifier interface {
	NotifyDecision(ctx context.Context, r DecisionResult)
}

// SMTPConfig captures everything the SMTP notifier needs. When Host,
// User or To are empty the factory returns a no-op notifier.
type SMTPConfig struct {
	Host        string
	Port        int
	User        string
	Password    string
	From        string
	To          string
	MinSeverity model.Severity
}

// New returns an SMTP notifier when configuration is complete and a no-op
// notifier otherwise. Callers never need to branch on the underlying
// implementation.
func New(cfg SMTPConfig) Notifier {
	if strings.TrimSpace(cfg.Host) == "" ||
		strings.TrimSpace(cfg.User) == "" ||
		strings.TrimSpace(cfg.To) == "" {
		slog.Info("notify: SMTP not configured, notifications disabled")
		return noopNotifier{}
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.From == "" {
		cfg.From = cfg.User
	}
	if cfg.MinSeverity == "" {
		cfg.MinSeverity = model.SeverityMedium
	}
	slog.Info("notify: SMTP configured",
		"host", cfg.Host, "port", cfg.Port,
		"from", cfg.From, "to", cfg.To,
		"minSeverity", string(cfg.MinSeverity))
	return &smtpNotifier{
		cfg:  cfg,
		send: sendViaSMTP,
		sem:  make(chan struct{}, maxConcurrentSends),
	}
}

// maxConcurrentSends caps in-flight SMTP goroutines so that an event storm
// (many decisions per poll) cannot spawn unbounded dispatchers and exhaust
// goroutines or overwhelm the SMTP server. Excess notifications are dropped
// with a warning rather than queued, since the poll loop is the authority
// on what is actionable.
const maxConcurrentSends = 16

type noopNotifier struct{}

func (noopNotifier) NotifyDecision(_ context.Context, _ DecisionResult) {}

// sendFunc is the swappable entry-point used by smtp.SendMail in production
// and by a stub in tests. Keeping it as a field lets us verify the message
// body without hitting a real SMTP server.
type sendFunc func(addr string, auth smtp.Auth, from string, to []string, msg []byte) error

func sendViaSMTP(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	return smtp.SendMail(addr, auth, from, to, msg)
}

type smtpNotifier struct {
	cfg  SMTPConfig
	send sendFunc
	// sem bounds concurrent dispatch goroutines (non-blocking acquire:
	// overflow is dropped with a log line, not queued).
	sem chan struct{}
}

// NotifyDecision is fire-and-forget from the caller's perspective: it
// dispatches the email in a goroutine with its own 30s timeout, so slow
// SMTP servers never block the poll loop. The goroutine pool is bounded
// via n.sem; when saturated the notification is dropped (rare in practice
// because poll dedup/caps already throttle upstream).
func (n *smtpNotifier) NotifyDecision(ctx context.Context, r DecisionResult) {
	severity := model.ParseSeverity(r.Decision.Severity)
	if !severity.MeetsMinimum(n.cfg.MinSeverity) {
		return
	}

	// Tests construct smtpNotifier literals without a semaphore; only
	// apply the cap when the production factory wired one up.
	if n.sem != nil {
		select {
		case n.sem <- struct{}{}:
		default:
			slog.Warn("notify: SMTP send dropped (in-flight cap reached)",
				"cap", cap(n.sem),
				"ns", r.Namespace, "name", r.Name,
				"action", r.Decision.Action)
			return
		}
	}

	go func() {
		if n.sem != nil {
			defer func() { <-n.sem }()
		}
		// Detach from the poll context so the email survives poll-level
		// timeouts, but cap it to avoid hanging forever.
		sendCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := n.dispatch(sendCtx, r); err != nil {
			slog.Error("notify: SMTP send failed",
				"ns", r.Namespace, "name", r.Name,
				"action", r.Decision.Action, "error", err)
		}
	}()
	_ = ctx // kept for signature consistency / future cancellation wiring
}

func (n *smtpNotifier) dispatch(ctx context.Context, r DecisionResult) error {
	msg := buildMessage(n.cfg.From, n.cfg.To,
		buildSubject(r), buildBody(r))

	addr := fmt.Sprintf("%s:%d", n.cfg.Host, n.cfg.Port)
	auth := smtp.PlainAuth("", n.cfg.User, n.cfg.Password, n.cfg.Host)

	done := make(chan error, 1)
	go func() {
		done <- n.send(addr, auth, n.cfg.From, []string{n.cfg.To}, msg)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func buildSubject(r DecisionResult) string {
	return fmt.Sprintf("[ai-remediator] %s on %s/%s", r.Decision.Action, r.Namespace, r.Name)
}

func buildBody(r DecisionResult) string {
	var b bytes.Buffer

	b.WriteString("Situazione anomala\n")
	b.WriteString(fmt.Sprintf("  Namespace: %s\n", r.Namespace))
	b.WriteString(fmt.Sprintf("  Kind: %s\n", r.Kind))
	b.WriteString(fmt.Sprintf("  Name: %s\n", r.Name))
	b.WriteString(fmt.Sprintf("  Reason: %s\n", r.EventReason))
	if r.EventMessage != "" {
		b.WriteString(fmt.Sprintf("  Message: %s\n", r.EventMessage))
	}
	if r.Decision.Severity != "" {
		b.WriteString(fmt.Sprintf("  Severity: %s\n", r.Decision.Severity))
	}
	b.WriteString("\n")

	b.WriteString("Decisione presa\n")
	b.WriteString(fmt.Sprintf("  Action: %s\n", r.Decision.Action))
	if r.Decision.ProbableCause != "" {
		b.WriteString(fmt.Sprintf("  Probable cause: %s\n", r.Decision.ProbableCause))
	}
	if r.Decision.Summary != "" {
		b.WriteString(fmt.Sprintf("  Summary: %s\n", r.Decision.Summary))
	}
	b.WriteString(fmt.Sprintf("  Confidence: %.2f\n", r.Decision.Confidence))
	if len(r.Decision.Parameters) > 0 {
		keys := make([]string, 0, len(r.Decision.Parameters))
		for k := range r.Decision.Parameters {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("  Parameters:\n")
		for _, k := range keys {
			b.WriteString(fmt.Sprintf("    %s: %s\n", k, r.Decision.Parameters[k]))
		}
	}
	b.WriteString("\n")

	b.WriteString("Situazione post intervento\n")
	if r.ExecutionErr == nil {
		b.WriteString("  Azione applicata con successo.\n")
	} else {
		b.WriteString(fmt.Sprintf("  Azione non applicata. Errore: %s\n", r.ExecutionErr.Error()))
	}

	return b.String()
}

func buildMessage(from, to, subject, body string) []byte {
	// RFC 5322 requires CRLF line endings in SMTP DATA.
	headers := []string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=\"utf-8\"",
	}
	return []byte(strings.Join(headers, "\r\n") + "\r\n\r\n" + body)
}
