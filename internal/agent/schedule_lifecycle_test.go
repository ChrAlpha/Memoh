package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/lifecycle"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	acpclient "github.com/felinics/memoh/internal/agent/runtime/acp/client"
	agentpkg "github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/contextview"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/schedule"
)

// runScheduledACP drives one ACP schedule fire through the entry.
func runScheduledACP(ctx context.Context, service *Service, payload schedule.TriggerPayload) (schedule.TriggerResult, error) {
	return service.TriggerSchedule(ctx, agenttest.LifecycleBotID, payload, "")
}

type triggerLifecycleProvider struct {
	mu     sync.Mutex
	calls  int
	params sdk.Request
}

func (*triggerLifecycleProvider) Name() string { return "trigger-lifecycle" }

func (*triggerLifecycleProvider) ListModels(context.Context) ([]sdk.Model, error) { return nil, nil }

func (*triggerLifecycleProvider) Test(context.Context) error {
	return nil
}

func (*triggerLifecycleProvider) TestModel(context.Context, string) (*sdk.ModelTestResult, error) {
	return &sdk.ModelTestResult{Supported: true}, nil
}

func (p *triggerLifecycleProvider) DoGenerate(
	_ context.Context,
	params sdk.Request,
) (sdk.ModelResult, error) {
	p.mu.Lock()
	p.calls++
	p.params = params
	p.mu.Unlock()
	return sdk.ModelResult{
		Text:         directLifecycleResponse,
		FinishReason: sdk.FinishReasonStop,
	}, nil
}

func (p *triggerLifecycleProvider) DoStream(
	ctx context.Context,
	params sdk.Request,
) (<-chan sdk.StreamPart, error) {
	result, err := p.DoGenerate(ctx, params)
	if err != nil {
		return nil, err
	}
	ch := make(chan sdk.StreamPart, 8)
	ch <- &sdk.StartPart{}
	ch <- &sdk.StartStepPart{}
	ch <- &sdk.TextStartPart{ID: "answer"}
	ch <- &sdk.TextDeltaPart{ID: "answer", Text: result.Text}
	ch <- &sdk.FinishStepPart{FinishReason: result.FinishReason}
	ch <- &sdk.FinishPart{FinishReason: result.FinishReason}
	close(ch)
	return ch, nil
}

func (p *triggerLifecycleProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// triggerLifecycleContextView is the fixture option that runs the service's
// model through a recording provider, with mutate applied to every run
// configuration before the context view compiles it.
func triggerLifecycleContextView(mutate func(*agentpkg.RunConfig)) (*triggerLifecycleProvider, directLifecycleOption) {
	provider := &triggerLifecycleProvider{}
	logger := slog.New(slog.DiscardHandler)
	applier := contextview.ProviderRunConfigApplier(logger)
	return provider, withFixtureDeps(func(d *Deps) {
		d.Agent = agentpkg.New(agentpkg.Deps{
			Logger: logger,
			ContextViewApplier: func(ctx context.Context, cfg agentpkg.RunConfig) agentpkg.RunConfig {
				if mutate != nil {
					mutate(&cfg)
				}
				out := applier(ctx, cfg)
				if out.Model != nil {
					model := *out.Model
					model.Provider = provider
					out.Model = &model
				}
				return out
			},
		})
	})
}

func setDirectLifecycleContextWindow(fixture directLifecycleFixture, window int) {
	for key, model := range fixture.queries.Models {
		model.Config = []byte(`{"context_window":` + strconv.Itoa(window) + `}`)
		fixture.queries.Models[key] = model
	}
}

func assertDeferredLifecycleRow(
	t *testing.T,
	creates []sqlc.CreateContextLifecycleParams,
	runID, status, errorCode string,
) contextfrag.LifecycleSnapshot {
	t.Helper()
	if len(creates) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(creates))
	}
	row := creates[0]
	if got := runreq.PGUUIDString(row.RunID); got != runID {
		t.Fatalf("context lifecycle run ID = %q, want admitted run ID %q", got, runID)
	}
	if row.Status != status {
		t.Fatalf("context lifecycle status = %q, want %q", row.Status, status)
	}
	if row.ErrorCode.String != errorCode || row.ErrorCode.Valid != (errorCode != "") {
		t.Fatalf("context lifecycle error code = %#v, want %q", row.ErrorCode, errorCode)
	}
	for _, privateText := range []string{directLifecyclePrompt, directLifecycleResponse} {
		if bytes.Contains(row.Snapshot, []byte(privateText)) {
			t.Fatalf("content-light snapshot leaked %q: %s", privateText, row.Snapshot)
		}
	}
	var snapshot contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(row.Snapshot, &snapshot); err != nil {
		t.Fatalf("decode lifecycle snapshot: %v", err)
	}
	if snapshot.Version != contextfrag.LifecycleSnapshotVersion {
		t.Fatalf("snapshot version = %d, want 1", snapshot.Version)
	}
	return snapshot
}

func hasLifecycleMutation(snapshot contextfrag.LifecycleSnapshot, kind contextfrag.MutationKind) bool {
	for _, mutation := range snapshot.Mutations {
		if mutation.Kind == kind {
			return true
		}
	}
	return false
}

func triggerDirectSchedule(t *testing.T, service *Service) (schedule.TriggerResult, error) {
	t.Helper()
	return service.TriggerSchedule(context.Background(), agenttest.LifecycleBotID, schedule.TriggerPayload{
		FireID:      "test-fire",
		SessionID:   agenttest.LifecycleSessionID,
		Command:     directLifecyclePrompt,
		OwnerUserID: "user-1",
	}, "")
}

func TestTriggerScheduleResolveFailurePersistsAdmittedMinimalLifecycle(t *testing.T) {
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess, withFixtureDeps(func(d *Deps) { d.Settings = nil }))

	_, err := triggerDirectSchedule(t, fixture.service)
	if err == nil {
		t.Fatal("TriggerSchedule() error = nil, want model-resolution failure")
	}
	creates := fixture.lifecycles.creates()
	if len(creates) != 1 {
		t.Fatalf("lifecycle creates = %d, want 1", len(creates))
	}
	row := creates[0]
	if runreq.PGUUIDString(row.RunID) != agenttest.LifecycleRunID || row.Status != lifecycle.StatusFailedProvider {
		t.Fatalf("lifecycle terminal = (run %q, status %q), want admitted run %q failed_provider", runreq.PGUUIDString(row.RunID), row.Status, agenttest.LifecycleRunID)
	}
	var snapshot contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(row.Snapshot, &snapshot); err != nil {
		t.Fatalf("decode minimal lifecycle: %v", err)
	}
	if snapshot.Version != contextfrag.LifecycleSnapshotVersion || snapshot.View != "" || snapshot.Counts != (contextfrag.ManifestCounts{}) ||
		snapshot.AssistantMessageID != "" {
		t.Fatalf("pre-context lifecycle is not minimal: %#v", snapshot)
	}
	if len(fixture.messages.Persisted) != 0 {
		t.Fatalf("resolve failure persisted messages: %#v", fixture.messages.Persisted)
	}
	if len(fixture.runtime.Finishes) != 1 || fixture.runtime.Finishes[0].Status != sessionruntime.RunStatusErrored {
		t.Fatalf("runtime finishes = %#v, want one errored finish", fixture.runtime.Finishes)
	}
}

func TestTriggerScheduleProviderFailurePersistsFailedProviderLifecycle(t *testing.T) {
	fixture := newDirectLifecycleFixture(t, directLifecycleModelFailure)

	_, err := triggerDirectSchedule(t, fixture.service)
	if err == nil {
		t.Fatal("TriggerSchedule() error = nil, want provider failure")
	}
	assertDirectLifecycle(t, fixture.lifecycles, agenttest.LifecycleRunID, lifecycle.StatusFailedProvider, "")
	if len(fixture.runtime.Finishes) != 1 || fixture.runtime.Finishes[0].Status != sessionruntime.RunStatusErrored {
		t.Fatalf("runtime finishes = %#v, want one errored finish", fixture.runtime.Finishes)
	}
	// The provider refused the request with a 400: a background run names it
	// as the WebSocket path does, and nothing of the provider's text.
	if code := fixture.lifecycles.creates()[0].ErrorCode; code.String != string(apperror.CodeAgentProviderRequestRejected) {
		t.Fatalf("lifecycle error code = %#v, want %q", code, apperror.CodeAgentProviderRequestRejected)
	}
}

// A window smaller than the system prompt, tools and message is no local
// failure: the request goes to the provider as it is, and the turn is written
// to history like any other.
func TestTriggerScheduleSendsARequestLargerThanTheWindowAsIs(t *testing.T) {
	provider, contextView := triggerLifecycleContextView(nil)
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess, contextView)
	setDirectLifecycleContextWindow(fixture, 1)

	result, err := triggerDirectSchedule(t, fixture.service)
	if err != nil || result.Status != "ok" {
		t.Fatalf("TriggerSchedule() = %q, %v; want the run to complete", result.Status, err)
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want the request sent once", provider.callCount())
	}
	if len(fixture.messages.Persisted) == 0 {
		t.Fatal("no history written: the turn must be stored like any completed one")
	}
	assertDirectLifecycle(t, fixture.lifecycles, agenttest.LifecycleRunID, lifecycle.StatusCompleted, "message-id")
	snapshot := assertDeferredLifecycleRow(t, fixture.lifecycles.creates(), agenttest.LifecycleRunID, lifecycle.StatusCompleted, "")
	if !contextview.PlanOverflows(snapshot.BudgetPlan) || snapshot.BudgetPlan.Window != 1 {
		t.Fatalf("budget plan = %#v, want the window-1 plan recorded as overflowing", snapshot.BudgetPlan)
	}
	if hasLifecycleMutation(snapshot, contextfrag.MutationContextBudgetFailure) {
		t.Fatalf("lifecycle mutations = %#v, want no budget failure", snapshot.Mutations)
	}
}

func TestTriggerScheduleContextViewFallbackPersistsFallbackAndReachesProvider(t *testing.T) {
	provider, contextView := triggerLifecycleContextView(func(cfg *agentpkg.RunConfig) {
		duplicate := func(role sdk.MessageRole, text string) contextfrag.ContextFrag {
			return contextfrag.MessageFrag(contextfrag.MessageFragInput{
				ID:      "forced-duplicate",
				Message: sdk.Message{Role: role, Content: []sdk.MessagePart{sdk.TextPart{Text: text}}},
				Kind:    contextfrag.KindConversationEvent,
				Slot:    contextfrag.SlotHistory,
				Source:  contextfrag.SourceRunConfig,
				Scope:   cfg.ContextScope,
			})
		}
		cfg.ContextSourceFrags = append(
			cfg.ContextSourceFrags,
			duplicate(sdk.MessageRoleUser, "first"),
			duplicate(sdk.MessageRoleAssistant, "second"),
		)
	})
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess, contextView)

	result, err := triggerDirectSchedule(t, fixture.service)
	if err != nil {
		t.Fatalf("TriggerSchedule() error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("TriggerSchedule() status = %q, want ok", result.Status)
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want fallback payload to reach provider once", provider.callCount())
	}
	assertDirectLifecycle(
		t,
		fixture.lifecycles,
		agenttest.LifecycleRunID,
		lifecycle.StatusFallback,
		"message-id",
	)
	snapshot := assertDeferredLifecycleRow(
		t,
		fixture.lifecycles.creates(),
		agenttest.LifecycleRunID,
		lifecycle.StatusFallback,
		"",
	)
	if !hasLifecycleMutation(snapshot, contextfrag.MutationContextViewFallback) {
		t.Fatalf("lifecycle mutations = %#v, want context-view fallback", snapshot.Mutations)
	}
}

func TestTriggerScheduleLifecycleStoreFailureDoesNotFailSuccessfulTrigger(t *testing.T) {
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess)
	fixture.lifecycles.mu.Lock()
	// A fenced run's lifecycle row is written when the runtime ends the run.
	fixture.lifecycles.store.CreateErr = errors.New("context lifecycle store unavailable")
	fixture.lifecycles.store.TerminalUpsertErr = fixture.lifecycles.store.CreateErr
	fixture.lifecycles.mu.Unlock()

	result, err := triggerDirectSchedule(t, fixture.service)
	if err != nil {
		t.Fatalf("TriggerSchedule() error = %v, want nil despite lifecycle store failure", err)
	}
	if result.Status != "ok" {
		t.Fatalf("TriggerSchedule() status = %q, want ok", result.Status)
	}
	if got := fixture.service.lifecycle.PersistenceErrors(); got == 0 {
		t.Fatal("lifecycle persistence error count = 0, want at least one recorded failure")
	}
	if len(fixture.runtime.Finishes) != 1 || fixture.runtime.Finishes[0].Status != sessionruntime.RunStatusCompleted {
		t.Fatalf("runtime finishes = %#v, want one completed finish", fixture.runtime.Finishes)
	}
}

func TestTriggerScheduleACPPersistsCompletedLifecycle(t *testing.T) {
	pool := &recordingACPPrompter{result: acpclient.PromptResult{Text: "done", StopReason: "end_turn"}}
	messages := &agenttest.RecordingMessages{}
	lifecycles := &agenttest.RecordingLifecycleStore{}
	service, _ := scheduleLifecycleService(t, pool, messages, lifecycles)

	result, err := runScheduledACP(context.Background(), service, schedule.TriggerPayload{
		FireID:          "test-fire",
		SessionID:       agenttest.LifecycleSessionID,
		Command:         "run scheduled task",
		OwnerUserID:     "user-1",
		RuntimeModelID:  "test-model",
		ReasoningEffort: "medium",
	})
	if err != nil {
		t.Fatalf("scheduled ACP run error = %v", err)
	}
	if result.Status != "ok" || result.Text != "done" {
		t.Fatalf("scheduled ACP result = %#v, want completed output", result)
	}
	if pool.input.RunID != agenttest.LifecycleRunID || pool.input.SessionID != agenttest.LifecycleSessionID {
		t.Fatalf("ACP prompt identity = (run %q, session %q), want (%q, %q)", pool.input.RunID, pool.input.SessionID, agenttest.LifecycleRunID, agenttest.LifecycleSessionID)
	}
	row, snapshot := requireACPLifecycle(t, lifecycles, agenttest.LifecycleRunID, lifecycle.StatusCompleted)
	if row.ErrorCode.Valid {
		t.Fatalf("completed lifecycle error code = %#v, want none", row.ErrorCode)
	}
	if snapshot.AssistantMessageID != "message-id" {
		t.Fatalf("assistant message ID = %q, want message-id", snapshot.AssistantMessageID)
	}
}

// A scheduled runtime that returns without completing its turn is a stopped
// run: the partial transcript is kept, the lifecycle records the abort, and
// nobody was asked to answer a decision.
func TestTriggerScheduleRuntimeRejectsIncompleteTurn(t *testing.T) {
	driver := &incompleteScheduleDriver{}
	messages := &agenttest.RecordingMessages{}
	lifecycles := &agenttest.RecordingLifecycleStore{}
	service, _ := scheduleLifecycleService(t, &recordingACPPrompter{}, messages, lifecycles, withScheduleDriver(driver))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runScheduledACP(ctx, service, schedule.TriggerPayload{FireID: "test-fire", ID: "schedule-1", SessionID: agenttest.LifecycleSessionID, Command: "run scheduled task", OwnerUserID: "user-1"})
	if err == nil {
		t.Fatal("scheduled run error = nil, want incomplete-turn failure")
	}
	if driver.input.CanRequestUserInput {
		t.Fatal("scheduled external runtime was marked interactive")
	}
	_, snapshot := requireACPLifecycle(t, lifecycles, agenttest.LifecycleRunID, lifecycle.StatusAborted)
	if snapshot.AssistantMessageID != "message-id" {
		t.Fatalf("aborted lifecycle assistant message ID = %q, want message-id", snapshot.AssistantMessageID)
	}
}

// A completed scheduled run reports the final assistant text and the run's
// usage to the schedule's log, and its round is written to history by the step
// committer as the user's turn followed by the answer. The run is fenced over a
// store that persists steps, which is how every scheduled run is written.
func TestTriggerScheduleReportsTheFinalTextAndUsage(t *testing.T) {
	_, contextView := triggerLifecycleContextView(nil)
	steps := &stepMessages{RecordingMessages: &agenttest.RecordingMessages{}}
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess, contextView,
		withFixtureMessages(steps), driveRequestRuntime(t))

	result, err := triggerDirectSchedule(t, fixture.service)
	if err != nil || result.Status != "ok" {
		t.Fatalf("TriggerSchedule() = %q, %v; want the run to complete", result.Status, err)
	}
	if steps.stepCalls() == 0 {
		t.Fatal("no step was committed; the run did not go through the step committer")
	}
	if result.Text != directLifecycleResponse {
		t.Fatalf("result.Text = %q, want the final assistant text %q", result.Text, directLifecycleResponse)
	}
	if len(result.UsageBytes) == 0 {
		t.Fatal("result.UsageBytes is empty, want the run's usage payload")
	}
	var roles []string
	steps.mu.Lock()
	for _, step := range steps.steps {
		for _, m := range step.Messages {
			roles = append(roles, m.Role)
		}
	}
	steps.mu.Unlock()
	if len(roles) != 2 || roles[0] != "user" || roles[1] != "assistant" {
		t.Fatalf("committed roles = %q, want user then assistant", roles)
	}
}

// fakeTriggeredAdmitter captures the Admit input so tests can drive the
// admission hook exactly the way the runtime manager would at activation.
type fakeTriggeredAdmitter struct {
	admitInput sessionruntime.AdmitInput
	admitErr   error
}

func (f *fakeTriggeredAdmitter) Admit(_ context.Context, input sessionruntime.AdmitInput) (sessionruntime.Admission, error) {
	f.admitInput = input
	if f.admitErr != nil {
		return sessionruntime.Admission{}, f.admitErr
	}
	return sessionruntime.Admission{
		Started:      true,
		RunID:        "run-1",
		TurnID:       "turn-1",
		TurnPosition: 1,
		Handle: sessionruntime.RunHandle{
			BotID:        input.BotID,
			SessionID:    input.SessionID,
			RunID:        "run-1",
			TurnID:       "turn-1",
			FencingToken: 1,
		},
	}, nil
}

func (*fakeTriggeredAdmitter) FinishRunWithErrorCode(context.Context, sessionruntime.RunHandle, string, string) (sessionruntime.TerminalRun, error) {
	return sessionruntime.TerminalRun{}, nil
}

func (*fakeTriggeredAdmitter) FinishRunWithFailure(context.Context, sessionruntime.RunHandle, string, string, map[string]string) (sessionruntime.TerminalRun, error) {
	return sessionruntime.TerminalRun{}, nil
}

func (*fakeTriggeredAdmitter) MarkInlineDecisionRun(string, string, string) {}

// A fire that finds the session busy reports the schedule's busy sentinel,
// which its unit treats as a skip. A failure before admission has no run
// record, so it is not marked recorded.
func TestTriggerScheduleBusyIsTheScheduleSentinel(t *testing.T) {
	t.Parallel()
	svc := newTestService(t, Deps{}, withTurnAdmitter(&fakeTriggeredAdmitter{admitErr: sessionruntime.ErrSessionBusy}))

	_, err := svc.TriggerSchedule(context.Background(), "bot-1", schedule.TriggerPayload{FireID: "fire-1", ID: "schedule-1", SessionID: "session-1", Command: "run"}, "token")

	if !errors.Is(err, schedule.ErrSessionBusy) || !errors.Is(err, sessionruntime.ErrSessionBusy) {
		t.Fatalf("err = %v, want the schedule busy sentinel over the runtime's", err)
	}
	if errs.Analyze(context.Background(), err).Recorded {
		t.Fatal("a fire refused before admission must not be marked recorded")
	}
}

// A failure after admission ends the run, whose result record carries it at
// its level; the fire returns it marked recorded so its own record is a WARN.
func TestTriggerScheduleFailureAfterAdmissionIsRecorded(t *testing.T) {
	t.Parallel()
	svc := newTestService(t, Deps{}, withTurnAdmitter(&fakeTriggeredAdmitter{}))

	_, err := svc.TriggerSchedule(context.Background(), "bot-1", schedule.TriggerPayload{FireID: "fire-1", ID: "schedule-1", SessionID: "session-1", Command: "run"}, "token")

	if err == nil {
		t.Fatal("TriggerSchedule() error = nil, want the run's failure")
	}
	if !errs.Analyze(context.Background(), err).Recorded {
		t.Fatalf("err = %v, want it marked recorded", err)
	}
}
