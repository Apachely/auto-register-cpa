package registerbot

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
	log "github.com/sirupsen/logrus"
)

// Stages of one account inside a channel queue. They are stable wire values: the
// panel maps them to labels, so renames must update both sides.
const (
	StageQueued       = "queued"
	StageValidating   = "validating"
	StagePinning      = "pinning"
	StageBrowserStart = "browser_start"
	StageGoogleLogin  = "google_login"
	StageOnboarding   = "onboarding"
	StageWaitingHuman = "waiting_human"
	StageCapturingKey = "capturing_key"
	StageVerifyingKey = "verifying_key"
	StageWritingKey   = "writing_key"
	StageDone         = "done"
	StageFailed       = "failed"
	StageSkipped      = "skipped"
	StagePaused       = "paused"
	// StageNodeHealth reports the idle controller reachability sweep.
	StageNodeHealth = "node_health"
)

// Terminal stages close an account row.
func IsTerminal(stage string) bool {
	switch stage {
	case StageDone, StageFailed, StageSkipped:
		return true
	}
	return false
}

// Event is one journal entry. Reasons are stage names or sanitized causes only:
// passwords, cookies, tokens, callback parameters and raw upstream bodies never
// enter the journal, and API keys are stored as masked hints.
type Event struct {
	Seq      int64     `json:"seq"`
	At       int64     `json:"at"`
	Channel  string    `json:"channel"`
	Run      string    `json:"run,omitempty"`
	Account  string    `json:"account,omitempty"`
	Stage    string    `json:"stage"`
	Level    string    `json:"level,omitempty"`
	Message  string    `json:"message,omitempty"`
	Node     string    `json:"node,omitempty"`
	ExitIP   string    `json:"exit_ip,omitempty"`
	Browser  string    `json:"browser,omitempty"`
	Evidence string    `json:"evidence,omitempty"`
	Masked   string    `json:"masked,omitempty"`
	Counters *Counters `json:"counters,omitempty"`
}

// Counters is the live progress snapshot carried by progress events.
type Counters struct {
	ChannelID string `json:"channel"`
	Total     int    `json:"total"`
	Queued    int    `json:"queued"`
	Running   int    `json:"running"`
	Done      int    `json:"done"`
	Failed    int    `json:"failed"`
	Skipped   int    `json:"skipped"`
	Waiting   int    `json:"waiting_human"`
	Pending   int    `json:"pending_write"`
	Target    int    `json:"target_keys,omitempty"`
	Got       int    `json:"harvested_keys"`
	Usable    int    `json:"usable_accounts"`
	Stage     string `json:"stage,omitempty"`
	Stalled   bool   `json:"stalled,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
}

// Bus stores the journal and fans events out to live subscribers.
//
// Replay works by sequence number, exactly like the log cursor design: a client
// that reconnects asks for "after seq" and continues where it stopped, so nothing
// is lost when the panel closes or CPA restarts. Entries are also appended to disk.
type Bus struct {
	dir  string
	file *os.File

	mu        sync.Mutex
	entries   []Event
	ringLimit int
	bySeq     int64
	subs      map[int]chan Event
	nextSub   int
	// written counts journal lines appended to disk for rotation decisions.
	written int
}

// NewBus opens the journal below dir and rebuilds the sequence counter.
func NewBus(dir string, ringLimit int) (*Bus, error) {
	if ringLimit <= 0 {
		ringLimit = 2000
	}
	if errMkdir := os.MkdirAll(dir, 0o750); errMkdir != nil {
		return nil, fmt.Errorf("registerbot: create journal dir: %w", errMkdir)
	}
	path := filepath.Join(dir, "events.jsonl")
	file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if errOpen != nil {
		return nil, fmt.Errorf("registerbot: open journal: %w", errOpen)
	}
	bus := &Bus{dir: dir, file: file, ringLimit: ringLimit, subs: make(map[int]chan Event)}
	if errReplay := bus.replay(path); errReplay != nil {
		log.WithError(errReplay).Warn("registerbot: journal replay skipped part of the history")
	}
	return bus, nil
}

// replay loads the tail of the on-disk journal so seq keeps monotonic after restart.
func (b *Bus) replay(path string) error {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return errOpen
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Warn("registerbot journal close failed")
		}
	}()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event Event
		if errUnmarshal := json.Unmarshal([]byte(line), &event); errUnmarshal != nil {
			continue
		}
		b.entries = append(b.entries, event)
		if event.Seq > b.bySeq {
			b.bySeq = event.Seq
		}
		b.written++
		if len(b.entries) > b.ringLimit*2 {
			b.entries = append([]Event(nil), b.entries[len(b.entries)-b.ringLimit:]...)
		}
	}
	if len(b.entries) > b.ringLimit {
		b.entries = append([]Event(nil), b.entries[len(b.entries)-b.ringLimit:]...)
	}
	return scanner.Err()
}

// NextSeq returns the sequence number the next event will take.
func (b *Bus) NextSeq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bySeq + 1
}

// Emit appends one event and notifies subscribers. The journal never blocks a run:
// a subscriber whose buffer is full is dropped, and live viewers resync by cursor.
func (b *Bus) Emit(event Event) Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bySeq++
	event.Seq = b.bySeq
	event.Message = keyPattern.ReplaceAllString(event.Message, "[redacted-key]")
	if len(event.Message) > 1024 {
		event.Message = event.Message[:1024]
	}
	if event.At == 0 {
		event.At = time.Now().UnixMilli()
	}
	b.entries = append(b.entries, event)
	if len(b.entries) > b.ringLimit*2 {
		b.entries = append([]Event(nil), b.entries[len(b.entries)-b.ringLimit:]...)
	}
	b.written++
	if b.file != nil {
		if payload, errMarshal := json.Marshal(event); errMarshal == nil {
			if _, errWrite := b.file.Write(append(payload, '\n')); errWrite != nil {
				log.Warn("registerbot journal write failed")
			}
		}
	}
	if b.written > b.ringLimit*4 {
		b.rotateLocked()
	}
	for id, ch := range b.subs {
		select {
		case ch <- event:
		default:
			delete(b.subs, id)
			close(ch)
		}
	}
	return event
}

// rotate archives the journal file and continues in a fresh one.
func (b *Bus) rotateLocked() {
	path := filepath.Join(b.dir, "events.jsonl")
	var data []byte
	for _, event := range b.entries {
		payload, errMarshal := json.Marshal(event)
		if errMarshal != nil {
			return
		}
		data = append(data, payload...)
		data = append(data, '\n')
	}
	if b.file != nil {
		if errClose := b.file.Close(); errClose != nil {
			log.Warn("registerbot journal rotation close failed")
		}
		b.file = nil
	}
	if errWrite := atomicWrite(path, data); errWrite != nil {
		log.Warn("registerbot journal rotation failed")
	}
	file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen == nil {
		b.file = file
		b.written = len(b.entries)
	}
}

// Since returns events with a sequence greater than after, newest last.
func (b *Bus) Since(after int64, limit int, channel string) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit <= 0 || limit > b.ringLimit {
		limit = b.ringLimit
	}
	var out []Event
	for _, event := range b.entries {
		if event.Seq <= after {
			continue
		}
		if channel != "" && event.Channel != channel {
			continue
		}
		out = append(out, event)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// Tail returns the most recent events for an initial paint.
func (b *Bus) Tail(limit int, channel string) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit <= 0 {
		limit = 200
	}
	out := make([]Event, 0, limit)
	for i := len(b.entries) - 1; i >= 0 && len(out) < limit; i-- {
		event := b.entries[i]
		if channel != "" && event.Channel != channel {
			continue
		}
		out = append(out, event)
	}
	for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
		out[left], out[right] = out[right], out[left]
	}
	return out
}

// LastSeq reports the newest stored sequence number.
func (b *Bus) LastSeq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bySeq
}

// Subscribe registers a live viewer and returns its cancel function.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 128
	}
	b.mu.Lock()
	id := b.nextSub
	b.nextSub++
	ch := make(chan Event, buffer)
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
		b.mu.Unlock()
	}
}

// Replay resolves a client cursor into a safe starting point.
//
// A cursor the journal no longer holds (trimmed by rotation, or newer than the
// last event after a restart) falls back to the oldest retained event, so the
// viewer always continues from real history instead of silently missing rows.
func (b *Bus) Replay(after int64, channel string) (events []Event, cursor int64) {
	b.mu.Lock()
	latest, limit := b.bySeq, b.ringLimit
	b.mu.Unlock()
	if after < 0 || after > latest {
		if tail := b.Tail(limit, channel); len(tail) > 0 {
			after = tail[0].Seq - 1
		}
	}
	events = b.Since(after, limit, channel)
	cursor = after
	if len(events) > 0 {
		cursor = events[len(events)-1].Seq
	}
	return events, cursor
}

// Close releases the journal file.
func (b *Bus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
	if b.file == nil {
		return nil
	}
	errClose := b.file.Close()
	b.file = nil
	return errClose
}

// SubscribeAfter captures replay and subscription under one lock, avoiding gaps.
func (b *Bus) SubscribeAfter(after int64, channel string, buffer int) ([]Event, <-chan Event, func(), bool) {
	if buffer <= 0 {
		buffer = 256
	}
	b.mu.Lock()
	reset := after > b.bySeq || (len(b.entries) > 0 && after < b.entries[0].Seq-1)
	if reset {
		after = 0
	}
	replay := make([]Event, 0)
	for _, event := range b.entries {
		if event.Seq > after && (channel == "" || event.Channel == channel) {
			replay = append(replay, event)
		}
	}
	id := b.nextSub
	b.nextSub++
	ch := make(chan Event, buffer)
	b.subs[id] = ch
	b.mu.Unlock()
	return replay, ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
	}, reset
}
