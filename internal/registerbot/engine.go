package registerbot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

const StagePendingWrite = "pending_write"

// ImportFunc validates the key and commits it through CPA's existing config CAS.
// A successful return means both verification and the durable provider write passed.
type ImportFunc func(context.Context, ChannelSettings, Harvest) error

// ImportError exposes only a fixed, non-sensitive category to the panel.
type ImportError struct{ Reason string }

func (e *ImportError) Error() string { return "registerbot: " + e.Reason }
func importReason(err error) string {
	var failure *ImportError
	if errors.As(err, &failure) {
		switch failure.Reason {
		case "verification_failed", "provider_missing", "config_conflict", "write_failed", "import_unavailable":
			return failure.Reason
		}
	}
	return "verify_or_write_failed"
}

type StartRequest struct {
	Channel  string `json:"channel"`
	Accounts string `json:"accounts"`
}

type RowView struct {
	ID        string `json:"id"`
	Line      int    `json:"line"`
	Account   string `json:"account"`
	Stage     string `json:"stage"`
	Attempt   int    `json:"attempt"`
	MaskedKey string `json:"masked_key,omitempty"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
}

type RunView struct {
	ID        string    `json:"id"`
	Channel   string    `json:"channel"`
	Status    string    `json:"status"`
	CreatedAt int64     `json:"created_at"`
	UpdatedAt int64     `json:"updated_at"`
	Rows      []RowView `json:"rows"`
	Counters  Counters  `json:"counters"`
}

type Snapshot struct {
	Enabled      bool              `json:"enabled"`
	Channels     []ChannelSettings `json:"channels"`
	Runs         []RunView         `json:"runs"`
	Cursor       int64             `json:"cursor"`
	StorageError bool              `json:"storage_error,omitempty"`
}

type queueRow struct {
	View       RowView `json:"view"`
	Credential Account `json:"credential"`
	Result     Harvest `json:"result"`
	Continue   bool    `json:"continue,omitempty"`
	busy       bool
}

type queueRun struct {
	ID        string      `json:"id"`
	Channel   string      `json:"channel"`
	Status    string      `json:"status"`
	CreatedAt int64       `json:"created_at"`
	UpdatedAt int64       `json:"updated_at"`
	Rows      []*queueRow `json:"rows"`
}

type queueDocument struct {
	Version int         `json:"version"`
	Runs    []*queueRun `json:"runs"`
}

// Engine owns scheduling; all mutable queue state is guarded by mu.
type Engine struct {
	mu            sync.Mutex
	settings      Settings
	settingsStore *store
	state         *sealedStore
	bus           *Bus
	gateway       *Gateway
	pool          *Pool
	evidence      *Evidence
	importer      ImportFunc
	runAccount    func(context.Context, *Task, Harvest) (Harvest, error)
	drivers       map[string]Driver
	runs          []*queueRun
	tasks         map[string]*Task
	cancels       map[string]context.CancelFunc
	workers       map[string]bool
	nextStart     map[string]time.Time
	ctx           context.Context
	cancel        context.CancelFunc
	wake          chan struct{}
	wg            sync.WaitGroup
	closed        bool
	storageError  bool
}

func NewEngine(stateDir string, settings Settings, importer ImportFunc, drivers ...Driver) (*Engine, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("registerbot: state directory is required")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	settings.Normalize()
	st := newStore(filepath.Join(stateDir, "settings.yaml"))
	if _, errStat := os.Stat(st.path); errors.Is(errStat, os.ErrNotExist) {
		st.settings = cloneSettings(settings)
		if err := st.writeLocked(); err != nil {
			return nil, err
		}
	} else {
		loaded, errLoad := st.Load()
		if errLoad != nil {
			return nil, errLoad
		}
		settings = loaded
	}
	if err := validateSettings(&settings); err != nil {
		return nil, err
	}
	sealed, errState := newSealedStore(stateDir)
	if errState != nil {
		return nil, errState
	}
	var doc queueDocument
	if err := sealed.Load(&doc); err != nil {
		return nil, err
	}
	if doc.Version != 0 && doc.Version != 1 {
		return nil, errors.New("registerbot: unsupported queue version")
	}
	bus, errBus := NewBus(stateDir, 2000)
	if errBus != nil {
		return nil, errBus
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{settings: settings, settingsStore: st, state: sealed, bus: bus, importer: importer, drivers: make(map[string]Driver), runs: doc.Runs, tasks: make(map[string]*Task), cancels: make(map[string]context.CancelFunc), workers: make(map[string]bool), nextStart: make(map[string]time.Time), ctx: ctx, cancel: cancel, wake: make(chan struct{})}
	e.gateway = NewGateway(e.Settings)
	e.runAccount = e.execute
	e.pool = NewPool(e.Settings, stateDir)
	if settings.Evidence {
		e.evidence = NewEvidence(filepath.Join(stateDir, "evidence"))
	}
	if len(drivers) == 0 {
		drivers = []Driver{NewFreeAIDriver(e.Settings)}
	}
	for _, driver := range drivers {
		if driver != nil {
			e.drivers[driver.Kind()] = driver
		}
	}
	for _, run := range e.runs {
		if run.Status == "done" || run.Status == "cancelled" {
			continue
		}
		run.Status = "paused"
		for _, row := range run.Rows {
			if !IsTerminal(row.View.Stage) {
				row.View.Stage = StagePaused
				row.View.Reason = "restart_resume_required"
			}
		}
	}
	if err := e.persistLocked(); err != nil {
		cancel()
		_ = bus.Close()
		return nil, err
	}
	e.startWorkersLocked()
	return e, nil
}

func (e *Engine) Settings() Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneSettings(e.settings)
}
func (e *Engine) Events() *Bus      { return e.bus }
func (e *Engine) Gateway() *Gateway { return e.gateway }
func (e *Engine) Pool() *Pool       { return e.pool }

func (e *Engine) UpdateSettings(settings Settings) error {
	settings.Normalize()
	if err := validateSettings(&settings); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("registerbot: engine closed")
	}
	for _, run := range e.runs {
		for _, row := range run.Rows {
			if row.busy {
				return errors.New("registerbot: pause active runs before changing settings")
			}
		}
	}
	if _, err := e.settingsStore.Update(func(next *Settings) { *next = cloneSettings(settings) }); err != nil {
		return err
	}
	e.settings = cloneSettings(settings)
	e.startWorkersLocked()
	e.signalLocked()
	e.bus.Emit(Event{Stage: "settings_updated", Level: "info", Message: "settings updated"})
	return nil
}

func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot := Snapshot{Enabled: e.settings.Enabled, Channels: cloneSettings(e.settings).Channels, Runs: make([]RunView, 0, len(e.runs)), StorageError: e.storageError}
	for _, run := range e.runs {
		snapshot.Runs = append(snapshot.Runs, e.runViewLocked(run))
	}
	snapshot.Cursor = e.bus.LastSeq()
	return snapshot
}

func (e *Engine) Start(request StartRequest) (RunView, error) {
	rows, errParse := parseAccounts(request.Accounts)
	if errParse != nil {
		return RunView{}, errParse
	}
	if len(rows) == 0 {
		return RunView{}, errors.New("registerbot: no account rows")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || !e.settings.Enabled {
		return RunView{}, errors.New("registerbot: engine disabled")
	}
	channel, ok := e.settings.Channel(request.Channel)
	if !ok || !channel.Enabled {
		return RunView{}, errors.New("registerbot: channel unavailable")
	}
	if _, ok = e.drivers[channel.Kind]; !ok {
		return RunView{}, errors.New("registerbot: channel driver unavailable")
	}
	if len(e.runs) >= 100 {
		return RunView{}, errors.New("registerbot: run history limit reached")
	}
	now := time.Now().UnixMilli()
	run := &queueRun{ID: uuid.NewString(), Channel: channel.ID, Status: "queued", CreatedAt: now, UpdatedAt: now, Rows: make([]*queueRow, 0, len(rows))}
	for _, parsed := range rows {
		row := &queueRow{Credential: Account{Email: parsed.email, Password: parsed.password}, View: RowView{ID: uuid.NewString(), Line: parsed.line, Stage: StageQueued, UpdatedAt: now}}
		row.View.Account = row.Credential.Masked()
		if parsed.status != "valid" {
			row.View.Stage = StageSkipped
			row.View.Reason = parsed.status
			row.Credential.Password = ""
		}
		run.Rows = append(run.Rows, row)
	}
	e.runs = append(e.runs, run)
	e.refreshRunLocked(run)
	if err := e.persistLocked(); err != nil {
		e.runs = e.runs[:len(e.runs)-1]
		return RunView{}, err
	}
	e.emitProgressLocked(run, StageQueued, "batch accepted")
	e.signalLocked()
	return e.runViewLocked(run), nil
}

func (e *Engine) Action(runID, action string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("registerbot: engine closed")
	}
	run := e.findRunLocked(runID)
	if run == nil {
		return errors.New("registerbot: run not found")
	}
	switch action {
	case "pause":
		if run.Status == "done" || run.Status == "cancelled" {
			return errors.New("registerbot: run already terminal")
		}
		run.Status = "paused"
		for _, row := range run.Rows {
			if !IsTerminal(row.View.Stage) {
				row.View.Stage = StagePaused
				row.View.Reason = "operator_paused"
				if cancel := e.cancels[row.View.ID]; cancel != nil {
					cancel()
				}
			}
		}
	case "resume", "retry":
		if run.Status == "cancelled" {
			return errors.New("registerbot: cancelled run cannot resume")
		}
		run.Status = "queued"
		for _, row := range run.Rows {
			if row.View.Stage == StageDone || row.View.Stage == StageSkipped {
				continue
			}
			if row.View.Stage == StageFailed && action != "retry" {
				continue
			}
			if row.View.Stage == StageFailed {
				row.View.Attempt = 0
			}
			row.View.Stage = StageQueued
			row.View.Reason = ""
		}
	case "cancel":
		run.Status = "cancelled"
		for _, row := range run.Rows {
			if !IsTerminal(row.View.Stage) {
				row.View.Stage = StageSkipped
				row.View.Reason = "operator_cancelled"
			}
			row.Credential.Password = ""
			row.Result.APIKey = ""
			if cancel := e.cancels[row.View.ID]; cancel != nil {
				cancel()
			}
			if !row.busy {
				e.discardTaskLocked(row.View.ID)
			}
		}
	default:
		return errors.New("registerbot: unsupported action")
	}
	e.refreshRunLocked(run)
	run.UpdatedAt = time.Now().UnixMilli()
	if err := e.persistLocked(); err != nil {
		return err
	}
	e.emitProgressLocked(run, run.Status, "batch "+action)
	e.signalLocked()
	return nil
}

func (e *Engine) SubmitKey(runID, rowID, key string) error {
	key = strings.TrimSpace(key)
	if !keyPattern.MatchString(key) || keyPattern.FindString(key) != key || len(key) > 4096 {
		return errors.New("registerbot: invalid key format")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("registerbot: engine closed")
	}
	run := e.findRunLocked(runID)
	if run == nil {
		return errors.New("registerbot: run not found")
	}
	for _, row := range run.Rows {
		if row.View.ID == rowID {
			if row.busy || IsTerminal(row.View.Stage) || run.Status == "cancelled" {
				return errors.New("registerbot: row does not accept a key")
			}
			proxyURL := e.settings.Gateway.ProxyURL
			if channel, exists := e.settings.Channel(run.Channel); exists && channel.ProxyURL != "" {
				proxyURL = channel.ProxyURL
			}
			row.Result = Harvest{APIKey: key, Account: row.Credential.Email, VerificationProxyURL: proxyURL}
			row.View.MaskedKey = maskAPIKey(key)
			row.View.Stage = StageQueued
			row.View.Reason = ""
			row.Continue = true
			run.Status = "queued"
			if err := e.persistLocked(); err != nil {
				return err
			}
			e.emitProgressLocked(run, StageQueued, "manual key queued for verification")
			e.signalLocked()
			return nil
		}
	}
	return errors.New("registerbot: row not found")
}

func (e *Engine) startWorkersLocked() {
	for _, channel := range e.settings.Channels {
		if e.workers[channel.ID] {
			continue
		}
		e.workers[channel.ID] = true
		for i := 0; i < 8; i++ {
			e.wg.Add(1)
			go e.worker(channel.ID)
		}
	}
}

func (e *Engine) signalLocked() { close(e.wake); e.wake = make(chan struct{}) }

func (e *Engine) worker(channelID string) {
	defer e.wg.Done()
	for {
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return
		}
		channel, exists := e.settings.Channel(channelID)
		var run *queueRun
		var row *queueRow
		var delay time.Duration
		if exists && channel.Enabled && e.settings.Enabled && !e.storageError {
			running := 0
			for _, candidate := range e.runs {
				if candidate.Channel == channelID {
					for _, entry := range candidate.Rows {
						if entry.busy {
							running++
						}
					}
				}
			}
			if running < channel.Concurrency {
				delay = time.Until(e.nextStart[channelID])
				if delay <= 0 {
					for _, candidate := range e.runs {
						if candidate.Channel != channelID || candidate.Status == "paused" || candidate.Status == "cancelled" {
							continue
						}
						if channel.TargetKeys > 0 {
							reserved := 0
							for _, entry := range candidate.Rows {
								if entry.busy || entry.Result.APIKey != "" || entry.View.Stage == StageDone {
									reserved++
								}
							}
							if reserved >= channel.TargetKeys {
								// Captured keys may still need their idempotent write retry.
								for _, entry := range candidate.Rows {
									if entry.View.Stage == StageQueued && !entry.busy && entry.Result.APIKey != "" {
										run, row = candidate, entry
										break
									}
								}
								if row != nil {
									break
								}
								continue
							}
						}
						for _, entry := range candidate.Rows {
							if entry.View.Stage == StageQueued && !entry.busy {
								run, row = candidate, entry
								break
							}
						}
						if row != nil {
							break
						}
					}
				}
			}
		}
		if row == nil {
			wake := e.wake
			e.mu.Unlock()
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-e.ctx.Done():
					timer.Stop()
					return
				case <-wake:
					timer.Stop()
				case <-timer.C:
				}
			} else {
				select {
				case <-e.ctx.Done():
					return
				case <-wake:
				}
			}
			continue
		}
		channelCopy := *channel
		channelCopy.Nodes = append([]string(nil), channel.Nodes...)
		row.busy = true
		row.View.Stage = StageValidating
		row.View.UpdatedAt = time.Now().UnixMilli()
		if !row.Continue {
			row.View.Attempt++
		}
		row.Continue = false
		run.Status = "running"
		e.nextStart[channelID] = time.Now().Add(channel.Interval())
		ctx, cancel := context.WithCancel(e.ctx)
		e.cancels[row.View.ID] = cancel
		if err := e.persistLocked(); err != nil {
			row.busy = false
			row.View.Stage = StagePaused
			run.Status = "paused"
			delete(e.cancels, row.View.ID)
			cancel()
			e.mu.Unlock()
			continue
		}
		e.emitProgressLocked(run, StageValidating, "account started")
		task := e.tasks[row.View.ID]
		if task == nil {
			task = &Task{Channel: channelCopy, Run: run.ID, RowID: row.View.ID, Account: row.Credential, bus: e.bus, evidence: e.evidence, settings: e.Settings}
			e.tasks[row.View.ID] = task
		}
		task.Attempt = row.View.Attempt
		task.Channel = channelCopy
		task.onReport = func(stage string) { e.report(run, row, stage) }
		harvest := row.Result
		e.mu.Unlock()
		result, errRun := e.runAccount(ctx, task, harvest)
		cancel()
		e.mu.Lock()
		row.busy = false
		delete(e.cancels, row.View.ID)
		switch {
		case errRun == nil:
			row.View.Stage = StageDone
			row.View.Reason = ""
			row.View.MaskedKey = maskAPIKey(result.APIKey)
			row.Result = Harvest{}
			row.Credential.Password = ""
			e.discardTaskLocked(row.View.ID)
		case run.Status == "cancelled":
			row.View.Stage = StageSkipped
			row.View.Reason = "operator_cancelled"
			row.Result = Harvest{}
			row.Credential.Password = ""
			e.discardTaskLocked(row.View.ID)
		case errors.Is(errRun, context.Canceled):
			if run.Status == "paused" || e.closed {
				row.View.Stage = StagePaused
			} else {
				row.View.Stage = StageQueued
			}
			row.Continue = true
		case result.APIKey != "":
			row.Result = result
			row.View.MaskedKey = maskAPIKey(result.APIKey)
			row.View.Stage = StagePendingWrite
			row.View.Reason = importReason(errRun)
			row.Continue = true
		case errors.Is(errRun, ErrManualHandoff):
			row.View.Stage = StageWaitingHuman
			row.View.Reason = "manual_action_required"
			row.Continue = true
		default:
			row.View.Reason = "attempt_failed"
			if row.View.Attempt < channelCopy.MaxAttempts {
				row.View.Stage = StageQueued
			} else {
				row.View.Stage = StageFailed
				e.discardTaskLocked(row.View.ID)
			}
		}
		row.View.UpdatedAt = time.Now().UnixMilli()
		e.refreshRunLocked(run)
		if channelCopy.TargetKeys > 0 && e.countersLocked(run).Done >= channelCopy.TargetKeys {
			for _, pending := range run.Rows {
				if !IsTerminal(pending.View.Stage) && !pending.busy && pending.Result.APIKey == "" {
					pending.View.Stage = StageSkipped
					pending.View.Reason = "target_reached"
					pending.Credential.Password = ""
					e.discardTaskLocked(pending.View.ID)
				}
			}
			e.refreshRunLocked(run)
		}
		_ = e.persistLocked()
		e.emitProgressLocked(run, row.View.Stage, row.View.Reason)
		e.signalLocked()
		e.mu.Unlock()
	}
}

func (e *Engine) execute(ctx context.Context, task *Task, harvest Harvest) (Harvest, error) {
	if harvest.APIKey == "" {
		if task.Channel.ManualMode {
			return Harvest{}, ErrManualHandoff
		}
		task.Report(StagePinning, "info", "acquiring channel gateway")
		status := e.gateway.Probe(ctx, false)
		if status.Mode != GatewayModeController {
			task.Report("gateway_degraded", "warning", status.Mode)
		}
		pinChannel := task.Channel
		if task.session != nil && task.node != "" {
			pinChannel.Nodes = []string{task.node}
			pinChannel.RotateNode = false
		}
		pin, errPin := e.gateway.Acquire(ctx, pinChannel, task.RowID)
		if errPin == nil {
			defer e.gateway.Release(task.Channel.Group, task.RowID)
		}
		if ctx.Err() != nil {
			return Harvest{}, ctx.Err()
		}
		if errPin != nil {
			task.Report("gateway_degraded", "warning", "gateway unavailable; continuing with fallback")
		} else {
			task.SetPinnedNode(pin.Node, pin.ExitIP)
			if pin.Degraded != "" {
				task.Report("gateway_degraded", "warning", pin.Degraded)
			}
		}
		e.pool.SetProxyURL(e.gateway.EffectiveProxyURL())
		if task.session == nil {
			task.Report(StageBrowserStart, "info", "acquiring pooled browser")
			instance, errAcquire := e.pool.Acquire(ctx)
			mode, reason := e.pool.Mode()
			if mode != BrowserModeEmbedded {
				task.Report("browser_degraded", "warning", mode+": "+reason)
			}
			if errAcquire != nil {
				if ctx.Err() != nil {
					return Harvest{}, ctx.Err()
				}
				return Harvest{}, ErrManualHandoff
			}
			task.browser = instance
			defer e.pool.Release(instance)
		}
		driver := e.drivers[task.Channel.Kind]
		var errDriver error
		harvest, errDriver = driver.Run(ctx, task)
		if errDriver != nil {
			return harvest, errDriver
		}
		if harvest.APIKey == "" {
			return Harvest{}, errors.New("registerbot: driver returned no key")
		}
		harvest.VerificationProxyURL = e.gateway.EffectiveProxyURL()
		// Persist the key before the external commit. A restart retries only the
		// idempotent import, never repeats registration for an already harvested key.
		e.mu.Lock()
		run := e.findRunLocked(task.Run)
		if run != nil {
			for _, row := range run.Rows {
				if row.View.ID == task.RowID {
					row.Result = harvest
					row.View.MaskedKey = maskAPIKey(harvest.APIKey)
					break
				}
			}
		}
		errSave := e.persistLocked()
		e.mu.Unlock()
		if errSave != nil {
			return harvest, errSave
		}
	} else {
		// Imported manual keys and pending writes follow the same forwarding
		// fallback as registration; an unavailable proxy must not strand a key.
		status := e.gateway.Probe(ctx, false)
		if status.Mode != GatewayModeController {
			task.Report("gateway_degraded", "warning", status.Mode)
		}
		harvest.VerificationProxyURL = e.gateway.EffectiveProxyURL()
	}
	if ctx.Err() != nil {
		return harvest, ctx.Err()
	}
	task.Report(StageVerifyingKey, "info", "verifying captured key before provider write")
	if e.importer == nil {
		return harvest, errors.New("registerbot: importer unavailable")
	}
	if errImport := e.importer(ctx, task.Channel, harvest); errImport != nil {
		return harvest, errImport
	}
	task.Report(StageWritingKey, "info", "provider key committed")
	return harvest, nil
}

func (e *Engine) report(run *queueRun, row *queueRow, stage string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !row.busy || run.Status == "paused" || run.Status == "cancelled" {
		return
	}
	if strings.HasPrefix(stage, "onboarding_") {
		stage = StageOnboarding
	}
	switch stage {
	case StageValidating, StagePinning, StageBrowserStart, StageGoogleLogin, StageOnboarding, StageWaitingHuman, StageCapturingKey, StageVerifyingKey, StageWritingKey:
		row.View.Stage = stage
	case "email", "password", "consent", "google", "speedbump":
		row.View.Stage = StageGoogleLogin
	case "key", "reveal", "dashboard":
		row.View.Stage = StageCapturingKey
	case "manual", "blocked", "challenge":
		row.View.Stage = StageWaitingHuman
	}
	row.View.UpdatedAt = time.Now().UnixMilli()
	run.UpdatedAt = row.View.UpdatedAt
}

func (e *Engine) persistLocked() error {
	err := e.state.Save(queueDocument{Version: 1, Runs: e.runs})
	if err != nil {
		e.storageError = true
		log.WithField("component", "registerbot").Error("queue persistence failed; scheduling paused")
		e.bus.Emit(Event{Stage: "storage_error", Level: "error", Message: "queue persistence failed; scheduling paused"})
		return errors.New("registerbot: queue persistence failed")
	}
	e.storageError = false
	return nil
}

func (e *Engine) countersLocked(run *queueRun) Counters {
	c := Counters{ChannelID: run.Channel, Total: len(run.Rows), Stage: run.Status, UpdatedAt: run.UpdatedAt}
	if channel, ok := e.settings.Channel(run.Channel); ok {
		c.Target = channel.TargetKeys
	}
	for _, row := range run.Rows {
		switch row.View.Stage {
		case StageQueued:
			c.Queued++
		case StageDone:
			c.Done++
		case StageFailed:
			c.Failed++
		case StageSkipped:
			c.Skipped++
		case StageWaitingHuman:
			c.Waiting++
		case StagePendingWrite:
			c.Pending++
		case StagePaused:
		default:
			c.Running++
		}
		if row.Result.APIKey != "" || row.View.Stage == StageDone {
			c.Got++
		}
	}
	c.Stalled = c.Waiting > 0 || c.Pending > 0
	return c
}

func (e *Engine) refreshRunLocked(run *queueRun) {
	run.UpdatedAt = time.Now().UnixMilli()
	if run.Status == "paused" || run.Status == "cancelled" {
		return
	}
	c := e.countersLocked(run)
	switch {
	case c.Running > 0:
		run.Status = "running"
	case c.Queued > 0:
		run.Status = "queued"
	case c.Waiting > 0 || c.Pending > 0:
		run.Status = "waiting_human"
	case c.Failed > 0 && c.Done > 0:
		run.Status = "partial"
	case c.Failed > 0:
		run.Status = "failed"
	default:
		run.Status = "done"
	}
}

func (e *Engine) emitProgressLocked(run *queueRun, stage, message string) {
	c := e.countersLocked(run)
	e.bus.Emit(Event{Channel: run.Channel, Run: run.ID, Stage: stage, Message: message, Counters: &c})
}

func (e *Engine) runViewLocked(run *queueRun) RunView {
	v := RunView{ID: run.ID, Channel: run.Channel, Status: run.Status, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, Rows: make([]RowView, 0, len(run.Rows)), Counters: e.countersLocked(run)}
	for _, row := range run.Rows {
		v.Rows = append(v.Rows, row.View)
	}
	return v
}

func (e *Engine) findRunLocked(id string) *queueRun {
	for _, run := range e.runs {
		if run.ID == id {
			return run
		}
	}
	return nil
}
func maskAPIKey(key string) string {
	if len(key) < 12 {
		return "[redacted]"
	}
	return key[:3] + "…" + key[len(key)-4:]
}

func (e *Engine) discardTaskLocked(rowID string) {
	task := e.tasks[rowID]
	delete(e.tasks, rowID)
	if task == nil || task.session == nil {
		return
	}
	session := task.session
	task.session = nil
	task.Account.Password = ""
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		if err := session.CloseTarget(e.ctx); err != nil {
			log.WithField("component", "registerbot").Debug("browser context cleanup deferred to pool shutdown")
		}
	}()
}

func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.cancel()
	e.signalLocked()
	e.mu.Unlock()
	e.wg.Wait()
	e.mu.Lock()
	errSave := e.persistLocked()
	e.mu.Unlock()
	errPool := e.pool.Shutdown()
	errBus := e.bus.Close()
	return errors.Join(errSave, errPool, errBus)
}
