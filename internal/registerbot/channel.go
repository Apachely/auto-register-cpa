package registerbot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/regexp"
)

// Channel kinds shipped with the registerbot.
const (
	ChannelKindFreeAIApiKey = "freeaiapikey"
)

// KnownChannelKinds lists the kinds the settings validator accepts.
var KnownChannelKinds = []string{ChannelKindFreeAIApiKey}

// ErrManualHandoff preserves a checkpoint while the operator finishes verification.
var ErrManualHandoff = errors.New("registerbot: operator action required")

func errString(err error) string {
	if err == nil {
		return ""
	}
	return "operation unavailable"
}

// KnownChannelKind reports whether a kind has a driver.
func KnownChannelKind(kind string) bool {
	for _, known := range KnownChannelKinds {
		if known == strings.TrimSpace(kind) {
			return true
		}
	}
	return false
}

// Account is one credential pair in the batch queue.
type Account struct {
	Email    string
	Password string
}

// Masked renders the identity without ever exposing the secret.
func (a Account) Masked() string {
	name, domain := splitEmail(a.Email)
	if domain == "" {
		return "[redacted]"
	}
	return name[:minInt(len(name), 2)] + "***@" + domain
}

func splitEmail(email string) (local, domain string) {
	if at := strings.Index(email, "@"); at >= 0 {
		return email[:at], email[at+1:]
	}
	return email, ""
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

// accountRow is one parsed input line.
type accountRow struct {
	line     int
	email    string
	password string
	status   string // valid | duplicate | invalid
}

var (
	emailPattern   = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`)
	keyPattern     = regexp.MustCompile(`sk-[A-Za-z0-9_\-]{16,}`)
	keyValueInline = regexp.MustCompile(`keyValue\s*=\s*["']([^"']+)["']`)
)

// MaxAccountRows bounds one submitted batch, keeping the work queue predictable.
const MaxAccountRows = 500

// parseAccounts turns the panel textarea into validated rows.
//
// Accepted format is `account|password`, one per line. Blank lines and lines
// starting with # are ignored, so a paste can carry comments. Later duplicates of
// the same address are marked instead of silently dropped, because the operator
// needs to see that the queue held a repeat.
func parseAccounts(text string) ([]accountRow, error) {
	if len(text) > MaxAccountRows*514 {
		return nil, fmt.Errorf("registerbot: batch too large (max %d rows)", MaxAccountRows)
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	seen := make(map[string]int, len(lines))
	var out []accountRow
	for index, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(out) >= MaxAccountRows {
			return out, fmt.Errorf("registerbot: batch limit %d reached", MaxAccountRows)
		}
		parts := strings.SplitN(raw, "|", 2)
		if len(parts) != 2 {
			out = append(out, accountRow{line: index + 1, status: "invalid"})
			continue
		}
		email := strings.ToLower(strings.TrimSpace(parts[0]))
		password := parts[1]
		if !emailPattern.MatchString(email) || password == "" {
			out = append(out, accountRow{line: index + 1, status: "invalid"})
			continue
		}
		if previous, dup := seen[email]; dup {
			out = append(out, accountRow{line: index + 1, email: email, status: "duplicate"})
			_ = previous
			continue
		}
		seen[email] = index + 1
		out = append(out, accountRow{line: index + 1, email: email, password: password, status: "valid"})
	}
	return out, nil
}

// Driver runs one account through one channel.
//
// Implementations report progress through the context, never through return
// values, so the panel sees each stage the moment it starts. A driver returns
// ErrManualHandoff when it must wait for the operator.
type Driver interface {
	Kind() string
	Run(ctx context.Context, task *Task) (Harvest, error)
	// Probe checks reachability of the channel entry point without registering.
	Probe(ctx context.Context) error
}

// Harvest is what a successful account run produced.
type Harvest struct {
	APIKey               string
	VerificationProxyURL string
	Account              string
	Node                 string
	ExitIP               string
	CreditConfirmed      bool
	Usable               bool
	Browser              string
	StartedAt            time.Time
	FinishedAt           time.Time
}

// Task carries everything a driver needs for one account.
type Task struct {
	Channel ChannelSettings
	Run     string
	RowID   string
	Account Account
	Attempt int

	bus      *Bus
	evidence *Evidence
	browser  *browserInstance
	settings func() Settings
	session  *Session
	onReport func(stage string)
	node     string
	exitIP   string

	mu       sync.Mutex
	lastURL  string
	lastNote string
}

// Report emits a stage event bound to this account.
func (t *Task) Report(stage, level, message string) {
	if t.onReport != nil {
		t.onReport(stage)
	}
	message = strings.ReplaceAll(message, t.Account.Email, t.Account.Masked())
	if t.Account.Password != "" {
		message = strings.ReplaceAll(message, t.Account.Password, "[redacted]")
	}
	t.mu.Lock()
	node, exitIP := t.node, t.exitIP
	t.mu.Unlock()
	t.bus.Emit(Event{
		Channel: t.Channel.ID, Run: t.Run, Account: t.Account.Masked(),
		Stage: stage, Level: level, Message: message,
		Node: node, ExitIP: exitIP, Browser: t.browserLabel(),
	})
}

func (t *Task) browserLabel() string {
	if t.browser == nil {
		return ""
	}
	return fmt.Sprintf("%s#%d", t.browser.Mode(), t.browser.ID())
}

// SetPinnedNode records the gateway decision for this account's events.
func (t *Task) SetPinnedNode(node, exitIP string) {
	t.mu.Lock()
	t.node, t.exitIP = node, exitIP
	t.mu.Unlock()
}

// StepDelay paces two UI actions so the batch looks like a human typing.
func (t *Task) StepDelay(ctx context.Context) error {
	delay := t.Channel.StepDelay()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// OpenSession starts a pooled browser tab ready for interaction.
func (t *Task) OpenSession(ctx context.Context, startURL string) (*Session, error) {
	if t.session != nil {
		return t.session, nil
	}
	if t.browser == nil {
		return nil, ErrManualHandoff
	}
	session, errOpen := t.browser.OpenPage(ctx, startURL)
	if errOpen != nil {
		return nil, errOpen
	}
	t.lastURL = startURL
	t.session = session
	return session, nil
}

// CaptureEvidence stores a screenshot plus the sanitized page state of a stall.
func (t *Task) CaptureEvidence(ctx context.Context, session *Session, reason string) string {
	if t.evidence == nil {
		return ""
	}
	name := fmt.Sprintf("%s-%s-%d-%s.json", sanitizeName(t.Channel.ID), sanitizeName(t.RowID), time.Now().Unix(), sanitizeName(reason))
	// Never persist raw authentication pages, which can contain passwords or tokens.
	payload, errMarshal := json.Marshal(map[string]string{"stage": sanitizeName(reason), "browser": t.browserLabel()})
	if errMarshal != nil {
		return ""
	}
	path, errWrite := t.evidence.Write(name, payload)
	if errWrite != nil {
		return ""
	}
	return filepath.Base(path)
}

func sanitizeName(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	out := builder.String()
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

// Evidence stores screenshots and page state for stalled accounts.
type Evidence struct {
	dir  string
	max  int
	mu   sync.Mutex
	size int
}

// NewEvidence prepares the evidence directory.
func NewEvidence(dir string) *Evidence {
	return &Evidence{dir: dir, max: 60}
}

// Enabled reports whether evidence capture is on.
func (e *Evidence) Enabled() bool {
	return e != nil && strings.TrimSpace(e.dir) != ""
}

// Write stores one artefact and returns its path. The file name never carries a
// password, and payloads are limited so a long run cannot fill the disk.
func (e *Evidence) Write(name string, payload []byte) (string, error) {
	if e == nil || !e.Enabled() {
		return "", errors.New("registerbot: evidence capture disabled")
	}
	if len(payload) == 0 {
		return "", nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if errMkdir := os.MkdirAll(e.dir, 0o750); errMkdir != nil {
		return "", fmt.Errorf("registerbot: create evidence dir: %w", errMkdir)
	}
	path := filepath.Join(e.dir, name)
	if errWrite := os.WriteFile(path, payload, 0o600); errWrite != nil {
		return "", fmt.Errorf("registerbot: write evidence: %w", errWrite)
	}
	e.size++
	e.trimLocked()
	return path, nil
}

// trimLocked keeps only the newest entries so evidence never grows without bound.
func (e *Evidence) trimLocked() {
	entries, errRead := os.ReadDir(e.dir)
	if errRead != nil || len(entries) <= e.max {
		return
	}
	// Directory entries come back sorted by name; the timestamp inside each name
	// keeps ordering meaningful enough for a bounded prune.
	drop := len(entries) - e.max
	for i := 0; i < drop; i++ {
		_ = os.Remove(filepath.Join(e.dir, entries[i].Name()))
	}
}

// List returns the newest evidence artefacts.
func (e *Evidence) List(limit int) []string {
	if e == nil || !e.Enabled() {
		return nil
	}
	if limit <= 0 {
		limit = 20
	}
	entries, errRead := os.ReadDir(e.dir)
	if errRead != nil {
		return nil
	}
	var out []string
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		if entries[i].IsDir() {
			continue
		}
		out = append(out, entries[i].Name())
	}
	return out
}

// Read returns one artefact by name, refusing anything outside the directory.
func (e *Evidence) Read(name string) ([]byte, error) {
	if e == nil || !e.Enabled() {
		return nil, errors.New("registerbot: evidence capture disabled")
	}
	clean := filepath.Base(strings.TrimSpace(name))
	if clean == "." || clean == "" || strings.Contains(clean, "..") {
		return nil, errors.New("registerbot: invalid evidence name")
	}
	data, errRead := os.ReadFile(filepath.Join(e.dir, clean))
	if errRead != nil {
		return nil, fmt.Errorf("registerbot: read evidence: %w", errRead)
	}
	return data, nil
}
