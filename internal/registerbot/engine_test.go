package registerbot

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
)

const testAPIKey = "sk-registerbot-test-key-1234567890123456"

func testSettings() Settings {
	settings := DefaultSettings()
	settings.Evidence = false
	settings.Browser.Mode = BrowserModeManual
	settings.Channels[0].ManualMode = true
	settings.Channels[0].IntervalSeconds = 0
	settings.Gateway.ProxyURL = "http://127.0.0.1:1"
	settings.Gateway.ControllerURL = "http://127.0.0.1:1"
	settings.Gateway.AutoDiscover = false
	settings.Gateway.VerifyExitIP = false
	settings.ProxyPool.Mode = ProxyPoolModeDisabled
	return settings
}

func newTestEngine(t *testing.T, settings Settings, importer ImportFunc) *Engine {
	t.Helper()
	engine, err := NewEngine(t.TempDir(), settings, importer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	return engine
}

func awaitRun(t *testing.T, e *Engine, id string, match func(RunView) bool) RunView {
	t.Helper()
	_, events, cancel, _ := e.Events().SubscribeAfter(e.Events().LastSeq(), "", 128)
	defer cancel()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		for _, run := range e.Snapshot().Runs {
			if run.ID == id && match(run) {
				return run
			}
		}
		select {
		case <-events:
		case <-deadline.C:
			t.Fatalf("run did not reach expected state: %+v", e.Snapshot())
			return RunView{}
		}
	}
}

func TestEngineManualQueueImportAndRedaction(t *testing.T) {
	var imported atomic.Int32
	e := newTestEngine(t, testSettings(), func(ctx context.Context, ch ChannelSettings, h Harvest) error {
		if h.APIKey != testAPIKey || ch.Provider != "FreeAi" {
			t.Errorf("wrong import inputs")
		}
		imported.Add(1)
		return nil
	})
	run, err := e.Start(StartRequest{Channel: "freeaiapikey", Accounts: "alice@example.invalid|sensitive-pass\nalice@example.invalid|duplicate\ninvalid"})
	if err != nil {
		t.Fatal(err)
	}
	waiting := awaitRun(t, e, run.ID, func(v RunView) bool { return v.Counters.Waiting == 1 })
	if waiting.Counters.Skipped != 2 {
		t.Fatalf("expected duplicate and malformed rows skipped: %+v", waiting.Counters)
	}
	if err = e.SubmitKey(run.ID, waiting.Rows[0].ID, testAPIKey); err != nil {
		t.Fatal(err)
	}
	done := awaitRun(t, e, run.ID, func(v RunView) bool { return v.Status == "done" })
	if done.Counters.Done != 1 || imported.Load() != 1 {
		t.Fatal("import completion mismatch")
	}
	if err = e.Action(run.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().Runs[0].Status != "done" {
		t.Fatal("resuming terminal run corrupted status")
	}
	payload, err := json.Marshal(e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sensitive-pass", testAPIKey, "alice@example.invalid"} {
		if bytes.Contains(payload, []byte(secret)) {
			t.Fatalf("snapshot leaked %q", secret)
		}
	}
	journal, err := os.ReadFile(filepath.Join(filepath.Dir(e.state.path), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(journal, []byte(testAPIKey)) || bytes.Contains(journal, []byte("sensitive-pass")) {
		t.Fatal("journal leaked credentials")
	}
}

func TestEnginePublishesUsableAccountAndRouteMetadata(t *testing.T) {
	e := newTestEngine(t, testSettings(), nil)
	e.runAccount = func(_ context.Context, task *Task, _ Harvest) (Harvest, error) {
		return Harvest{
			APIKey: testAPIKey, Node: "node-a", ExitIP: "203.0.113.30",
			CreditConfirmed: true, Usable: true,
		}, nil
	}
	run, errStart := e.Start(StartRequest{Channel: "freeaiapikey", Accounts: "usable@example.invalid|pw"})
	if errStart != nil {
		t.Fatal(errStart)
	}
	done := awaitRun(t, e, run.ID, func(view RunView) bool { return view.Status == "done" })
	if done.Counters.Usable != 1 || len(done.Rows) != 1 {
		t.Fatalf("usable counter missing: %+v", done)
	}
	row := done.Rows[0]
	if !row.Usable || !row.CreditConfirmed || row.Node != "node-a" || row.ExitIP != "203.0.113.30" {
		t.Fatalf("route metadata missing: %+v", row)
	}
}

func TestEnginePendingWriteRetryDoesNotRegisterAgain(t *testing.T) {
	var calls atomic.Int32
	e := newTestEngine(t, testSettings(), func(context.Context, ChannelSettings, Harvest) error {
		if calls.Add(1) == 1 {
			return &ImportError{Reason: "config_conflict"}
		}
		return nil
	})
	run, err := e.Start(StartRequest{Channel: "freeaiapikey", Accounts: "bob@example.invalid|pw"})
	if err != nil {
		t.Fatal(err)
	}
	waiting := awaitRun(t, e, run.ID, func(v RunView) bool { return v.Counters.Waiting == 1 })
	if err = e.SubmitKey(run.ID, waiting.Rows[0].ID, testAPIKey); err != nil {
		t.Fatal(err)
	}
	pending := awaitRun(t, e, run.ID, func(v RunView) bool { return v.Counters.Pending == 1 })
	if pending.Rows[0].Reason != "config_conflict" {
		t.Fatal("safe import category was lost")
	}
	if err = e.Action(run.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, e, run.ID, func(v RunView) bool { return v.Counters.Done == 1 })
	if calls.Load() != 2 {
		t.Fatalf("unexpected import calls: %d", calls.Load())
	}
}

func TestEngineRestartRequiresResumeAndPreservesCapturedKey(t *testing.T) {
	dir := t.TempDir()
	settings := testSettings()
	e, err := NewEngine(dir, settings, func(context.Context, ChannelSettings, Harvest) error { return errors.New("pending") })
	if err != nil {
		t.Fatal(err)
	}
	run, err := e.Start(StartRequest{Channel: "freeaiapikey", Accounts: "carol@example.invalid|private-password"})
	if err != nil {
		t.Fatal(err)
	}
	waiting := awaitRun(t, e, run.ID, func(v RunView) bool { return v.Counters.Waiting == 1 })
	if err = e.SubmitKey(run.ID, waiting.Rows[0].ID, testAPIKey); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, e, run.ID, func(v RunView) bool { return v.Counters.Pending == 1 })
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "queue.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(testAPIKey)) || bytes.Contains(data, []byte("private-password")) {
		t.Fatal("plaintext credential persisted")
	}
	var imports atomic.Int32
	restarted, err := NewEngine(dir, settings, func(_ context.Context, _ ChannelSettings, h Harvest) error {
		if h.APIKey != testAPIKey {
			t.Error("checkpoint key lost")
		}
		imports.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	if restarted.Snapshot().Runs[0].Status != "paused" || imports.Load() != 0 {
		t.Fatal("restart must require deliberate resume")
	}
	if err = restarted.Action(run.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, restarted, run.ID, func(v RunView) bool { return v.Counters.Done == 1 })
	if imports.Load() != 1 {
		t.Fatal("checkpoint did not import exactly once")
	}
}

func TestEngineChannelsContinueWhileAnotherWaits(t *testing.T) {
	settings := testSettings()
	second := settings.Channels[0]
	second.ID = "second"
	second.Provider = "Second"
	settings.Channels = append(settings.Channels, second)
	e := newTestEngine(t, settings, func(context.Context, ChannelSettings, Harvest) error { return nil })
	first, err := e.Start(StartRequest{Channel: "freeaiapikey", Accounts: "a@example.invalid|pw"})
	if err != nil {
		t.Fatal(err)
	}
	secondRun, err := e.Start(StartRequest{Channel: "second", Accounts: "b@example.invalid|pw"})
	if err != nil {
		t.Fatal(err)
	}
	awaitRun(t, e, first.ID, func(v RunView) bool { return v.Counters.Waiting == 1 })
	waiting := awaitRun(t, e, secondRun.ID, func(v RunView) bool { return v.Counters.Waiting == 1 })
	if err = e.SubmitKey(secondRun.ID, waiting.Rows[0].ID, testAPIKey); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, e, secondRun.ID, func(v RunView) bool { return v.Counters.Done == 1 })
	if e.Snapshot().Runs[0].Counters.Waiting != 1 {
		t.Fatal("another channel's state changed")
	}
}

func TestEnginePauseCancelAndBoundConcurrency(t *testing.T) {
	settings := testSettings()
	settings.Channels[0].Concurrency = 2
	entered := make(chan struct{}, 2)
	var current atomic.Int32
	var maxConcurrent atomic.Int32
	e := newTestEngine(t, settings, func(ctx context.Context, _ ChannelSettings, _ Harvest) error {
		n := current.Add(1)
		defer current.Add(-1)
		for old := maxConcurrent.Load(); n > old; old = maxConcurrent.Load() {
			if maxConcurrent.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	run, err := e.Start(StartRequest{Channel: "freeaiapikey", Accounts: "a@example.invalid|pw\nb@example.invalid|pw\nc@example.invalid|pw"})
	if err != nil {
		t.Fatal(err)
	}
	waiting := awaitRun(t, e, run.ID, func(v RunView) bool { return v.Counters.Waiting == 3 })
	for _, row := range waiting.Rows {
		if err = e.SubmitKey(run.ID, row.ID, testAPIKey); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent imports not started")
		}
	}
	if err = e.Action(run.ID, "pause"); err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().Runs[0].Status != "paused" {
		t.Fatal("pause not reflected")
	}
	if maxConcurrent.Load() != 2 {
		t.Fatal("configured concurrency not honored")
	}
	if err = e.Action(run.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().Runs[0].Status != "cancelled" {
		t.Fatal("cancel not reflected")
	}
	if err = e.Action(run.ID, "resume"); err == nil {
		t.Fatal("cancelled batch resumed")
	}
}

func TestBusConcurrentReplayAndCancellation(t *testing.T) {
	dir := t.TempDir()
	bus, err := NewBus(dir, 64)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 80; n++ {
				_, cancel := bus.Subscribe(1)
				bus.Emit(Event{Stage: StageQueued, Message: testAPIKey})
				cancel()
			}
		}()
	}
	wg.Wait()
	latest := bus.LastSeq()
	events, live, cancel, reset := bus.SubscribeAfter(latest-5, "", 2)
	if reset || len(events) != 5 {
		t.Fatalf("unexpected replay %d reset %v", len(events), reset)
	}
	bus.Emit(Event{Stage: StageDone})
	if event := <-live; event.Seq != latest+1 {
		t.Fatal("live event gap")
	}
	cancel()
	for _, event := range events {
		if strings.Contains(event.Message, testAPIKey) {
			t.Fatal("key leaked in events")
		}
	}
	if err = bus.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewBus(dir, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.LastSeq() != latest+1 {
		t.Fatal("sequence changed after restart")
	}
	_, _, unsubscribe, reset := reopened.SubscribeAfter(1, "", 1)
	unsubscribe()
	if !reset {
		t.Fatal("expired cursor must reset")
	}
	previous := int64(0)
	for _, event := range reopened.Tail(1000, "") {
		if event.Seq <= previous {
			t.Fatal("journal is not ordered")
		}
		previous = event.Seq
	}
}

func TestSealedStoreRejectsTampering(t *testing.T) {
	store, err := newSealedStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(map[string]string{"password": "never-plaintext"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err = os.WriteFile(store.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err = store.Load(&result); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestSchedulerRetriesFailedAccountsAndStopsAtLimit(t *testing.T) {
	settings := testSettings()
	settings.Channels[0].MaxAttempts = 3
	e := newTestEngine(t, settings, nil)
	var attempts sync.Map
	e.runAccount = func(_ context.Context, task *Task, _ Harvest) (Harvest, error) {
		attempts.Store(task.Account.Email, task.Attempt)
		if strings.HasPrefix(task.Account.Email, "recover") && task.Attempt == 2 {
			return Harvest{APIKey: testAPIKey}, nil
		}
		return Harvest{}, errors.New("transient driver failure")
	}
	run, err := e.Start(StartRequest{Channel: "freeaiapikey", Accounts: "recover@example.invalid|pw\nfail@example.invalid|pw"})
	if err != nil {
		t.Fatal(err)
	}
	finished := awaitRun(t, e, run.ID, func(v RunView) bool { return v.Status == "partial" })
	if finished.Counters.Done != 1 || finished.Counters.Failed != 1 {
		t.Fatalf("unexpected retry outcome: %+v", finished.Counters)
	}
	if count, _ := attempts.Load("recover@example.invalid"); count != 2 {
		t.Fatalf("recovered account attempts %v", count)
	}
	if count, _ := attempts.Load("fail@example.invalid"); count != 3 {
		t.Fatalf("failed account attempts %v", count)
	}
}

func TestParseAccountsPreservesPasswordWhitespace(t *testing.T) {
	rows, err := parseAccounts("  alice@example.invalid | leading and trailing  \n")
	if err != nil || len(rows) != 1 || rows[0].password != " leading and trailing  " {
		t.Fatal("password bytes changed")
	}
}

func TestSchedulerTargetBoundsConcurrentRegistration(t *testing.T) {
	settings := testSettings()
	settings.Channels[0].Concurrency = 3
	settings.Channels[0].TargetKeys = 1
	e := newTestEngine(t, settings, nil)
	var called atomic.Int32
	e.runAccount = func(context.Context, *Task, Harvest) (Harvest, error) {
		called.Add(1)
		return Harvest{APIKey: testAPIKey}, nil
	}
	run, err := e.Start(StartRequest{Channel: "freeaiapikey", Accounts: "a@example.invalid|pw\nb@example.invalid|pw\nc@example.invalid|pw"})
	if err != nil {
		t.Fatal(err)
	}
	finished := awaitRun(t, e, run.ID, func(v RunView) bool { return v.Status == "done" })
	if called.Load() != 1 || finished.Counters.Skipped != 2 {
		t.Fatal("target allowed excess registrations")
	}
}

func TestEngineCancelledSuccessfulGatewayAcquireReleasesLease(t *testing.T) {
	settings := testSettings()
	settings.Channels[0].ManualMode = false
	settings.Gateway.ProxyURL = "http://127.0.0.1:1"
	settings.Gateway.ControllerURL = "http://127.0.0.1:1"
	settings.Gateway.AutoDiscover = false
	settings.Gateway.VerifyExitIP = false
	e := newTestEngine(t, settings, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	task := &Task{Channel: settings.Channels[0], Run: "cancel-test", RowID: "cancel-row", Account: Account{Email: "cancel@example.invalid"}, bus: e.Events(), settings: e.Settings}
	if _, err := e.execute(ctx, task, Harvest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	e.gateway.mu.Lock()
	leases := len(e.gateway.leases)
	e.gateway.mu.Unlock()
	if leases != 0 {
		t.Fatal("cancelled task retained exclusive gateway lease")
	}
}

func TestEngineManualKeyFallsBackAndTargetSkipsWaitingRows(t *testing.T) {
	settings := testSettings()
	settings.Channels[0].TargetKeys = 1
	e := newTestEngine(t, settings, func(_ context.Context, _ ChannelSettings, harvest Harvest) error {
		if harvest.VerificationProxyURL != "" {
			t.Error("disabled proxy did not degrade to direct verification")
		}
		return nil
	})
	run, err := e.Start(StartRequest{Channel: "freeaiapikey", Accounts: "a@example.invalid|pw\nb@example.invalid|pw"})
	if err != nil {
		t.Fatal(err)
	}
	waiting := awaitRun(t, e, run.ID, func(v RunView) bool { return v.Counters.Waiting == 2 })
	if err = e.SubmitKey(run.ID, waiting.Rows[0].ID, testAPIKey); err != nil {
		t.Fatal(err)
	}
	finished := awaitRun(t, e, run.ID, func(v RunView) bool { return v.Status == "done" })
	if finished.Counters.Done != 1 || finished.Counters.Skipped != 1 || finished.Rows[1].Reason != "target_reached" {
		t.Fatal("waiting row survived a satisfied target")
	}
}
