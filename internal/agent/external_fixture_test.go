package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	acpagent "github.com/felinics/memoh/internal/agent/runtime/acp"
	acpclient "github.com/felinics/memoh/internal/agent/runtime/acp/client"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/bots"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

type noticeTestDriver struct {
	kind   string
	prompt func(context.Context, external.PromptInput) (external.PromptResult, error)
}

func (d noticeTestDriver) RuntimeType() string { return d.kind }

func (d noticeTestDriver) Prompt(ctx context.Context, input external.PromptInput) (external.PromptResult, error) {
	return d.prompt(ctx, input)
}

func newACPLifecycleService(
	t *testing.T,
	pool acpagent.Prompter,
	messages messagepkg.Service,
	lifecycles lifecycle.Store,
	opts ...testOption,
) *Service {
	t.Helper()
	sessions := &agenttest.Sessions{
		GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
			return session.Thread{
				ID:    sessionID,
				BotID: agenttest.LifecycleBotID,
				Type:  session.TypeACPAgent,
				Metadata: map[string]any{
					"acp_agent_id":             "codex",
					"project_path":             "/data/app",
					"runtime_owner_account_id": "user-1",
				},
			}, nil
		},
		UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
			return session.Thread{ID: sessionID}, nil
		},
	}
	return newTestService(t, Deps{
		Messages:       messages,
		BotPermissions: allowWorkspaceExecForBot(agenttest.LifecycleBotID, "user-1"),
		Sessions:       runtimeTypedSessions{SessionService: sessions, runtimeType: session.RuntimeACPAgent},
	}, append([]testOption{
		withLifecycleRecorder(lifecycles),
		withACPPool(pool),
		withTurnAdmitter(&agenttest.LifecycleAdmitter{Admission: webTestAdmission(lifecycleWebRequest())}),
	}, opts...)...)
}

// lifecycleWebRequest is the Web send the lifecycle fixtures admit: the bot,
// session and run the lifecycle stand-ins name.
func lifecycleWebRequest() runreq.ChatRequest {
	return runreq.ChatRequest{BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID}
}

// withExternalDriver registers driver for the sessions of the service, which
// then run on its runtime type.
func withExternalDriver(driver external.Driver) testOption {
	return func(b *testBuild) {
		b.deps.ExternalRuntimes = append(b.deps.ExternalRuntimes, driver)
		b.deps.Sessions = runtimeTypedSessions{SessionService: b.deps.Sessions, runtimeType: driver.RuntimeType()}
	}
}

func requireACPLifecycle(
	t *testing.T,
	store *agenttest.RecordingLifecycleStore,
	wantRunID, wantStatus string,
) (sqlc.CreateContextLifecycleParams, contextfrag.LifecycleSnapshot) {
	t.Helper()
	if len(store.Creates) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(store.Creates))
	}
	row := store.Creates[0]
	if got := runreq.PGUUIDString(row.RunID); got != wantRunID {
		t.Fatalf("lifecycle RunID = %q, want %q", got, wantRunID)
	}
	if row.Status != wantStatus {
		t.Fatalf("lifecycle status = %q, want %q", row.Status, wantStatus)
	}
	var snapshot contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(row.Snapshot, &snapshot); err != nil {
		t.Fatalf("decode lifecycle snapshot: %v", err)
	}
	return row, snapshot
}

type recordingACPPrompter struct {
	calls        int
	input        acpagent.PromptInput
	result       acpclient.PromptResult
	err          error
	promptFn     func(context.Context, acpagent.PromptInput) (acpclient.PromptResult, error)
	onPrompt     func()
	streamEvents []event.StreamEvent
	afterEvents  func()
	closed       []string
	closeErr     error
}

func flowTestUUID(value string) pgtype.UUID {
	var out pgtype.UUID
	if err := out.Scan(value); err != nil {
		panic(err)
	}
	return out
}

func (p *recordingACPPrompter) Prompt(ctx context.Context, input acpagent.PromptInput) (acpclient.PromptResult, error) {
	p.calls++
	p.input = input
	if p.promptFn != nil {
		return p.promptFn(ctx, input)
	}
	if p.onPrompt != nil {
		p.onPrompt()
	}
	if input.Sink != nil {
		events := p.streamEvents
		if len(events) == 0 && p.err == nil {
			events = []event.StreamEvent{{Type: event.TextDelta, Delta: "streamed from acp"}}
		}
		for _, ev := range events {
			input.Sink.EmitStreamEvent(ev)
		}
	}
	if p.afterEvents != nil {
		p.afterEvents()
	}
	return p.result, p.err
}

func (p *recordingACPPrompter) CloseSession(sessionID string) error {
	p.closed = append(p.closed, sessionID)
	return p.closeErr
}

func allowWorkspaceExecForBot(botID, accountID string) *agenttest.PermissionChecker {
	return &agenttest.PermissionChecker{Values: map[string]bool{
		strings.TrimSpace(botID) + ":" + strings.TrimSpace(accountID) + ":" + bots.PermissionWorkspaceExec: true,
	}}
}

func acpRuntimeSessionServiceForTest(runtimeOwnerAccountID string) *agenttest.Sessions {
	return &agenttest.Sessions{
		GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
			return session.Thread{
				ID:    sessionID,
				BotID: "bot-1",
				Type:  session.TypeACPAgent,
				Metadata: map[string]any{
					"acp_agent_id":             "codex",
					"project_path":             "/data/app",
					"runtime_owner_account_id": strings.TrimSpace(runtimeOwnerAccountID),
				},
			}, nil
		},
	}
}

func drainAgentEvents(t *testing.T, eventCh <-chan runreq.WSStreamEvent) []native.StreamEvent {
	t.Helper()
	events := make([]native.StreamEvent, 0, len(eventCh))
	for len(eventCh) > 0 {
		var event native.StreamEvent
		if err := json.Unmarshal(<-eventCh, &event); err != nil {
			t.Fatalf("decode stream event: %v", err)
		}
		events = append(events, event)
	}
	return events
}

func containsStreamEvent(events []native.StreamEvent, eventType native.StreamEventType) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func terminalAssistantText(t *testing.T, event native.StreamEvent) string {
	t.Helper()
	var messages []runreq.ModelMessage
	if err := json.Unmarshal(event.Messages, &messages); err != nil {
		t.Fatalf("decode terminal messages: %v", err)
	}
	for _, msg := range messages {
		if msg.Role == "assistant" {
			return strings.TrimSpace(msg.TextContent())
		}
	}
	t.Fatalf("terminal messages = %#v, want assistant message", messages)
	return ""
}

func persistedText(t *testing.T, content json.RawMessage) string {
	t.Helper()
	return persistedModelMessage(t, content).TextContent()
}

func persistedModelMessage(t *testing.T, content json.RawMessage) runreq.ModelMessage {
	t.Helper()
	var msg runreq.ModelMessage
	if err := json.Unmarshal(content, &msg); err != nil {
		t.Fatalf("decode persisted content: %v", err)
	}
	return msg
}

// runtimeTypedSessions serves the session the fixture describes as one that runs
// on runtimeType, which is the driver the test registered.
type runtimeTypedSessions struct {
	runreq.SessionService
	runtimeType string
}

func (r runtimeTypedSessions) Get(ctx context.Context, sessionID string) (session.Thread, error) {
	thread, err := r.SessionService.Get(ctx, sessionID)
	if err == nil {
		thread.RuntimeType = r.runtimeType
	}
	return thread, err
}

// streamExternalWebSend admits and runs req as a Web send, the way
// the WebSocket does, and hands back what the run delivered. abortCh stops the
// run the way a routed Stop does. A run that failed reports its failure in the
// outcome.
func streamExternalWebSend(ctx context.Context, s *Service, req runreq.ChatRequest, eventCh chan<- runreq.WSStreamEvent, abortCh <-chan struct{}) (outcome.RunOutcome, error) {
	admission, err := admitWebRun(ctx, s, agentapi.WebAdmitInput{
		BotID:        req.BotID,
		SessionID:    req.ThreadID,
		InvocationID: "web-invocation",
		Payload:      []byte(`{"kind":"message"}`),
	})
	if err != nil {
		return outcome.RunOutcome{}, err
	}
	runEnded := make(chan struct{})
	defer close(runEnded)
	go func() {
		select {
		case <-abortCh:
			admission.Stop(outcome.ErrRunStopped)
		case <-runEnded:
		}
	}()
	delivery := newWebTestDelivery()
	s.web.RunSend(admission, req, delivery) //nolint:contextcheck // The run's context is the one admission derived from ctx.
	select {
	case <-delivery.done:
	case <-time.After(10 * time.Second):
		return outcome.RunOutcome{}, errors.New("the Web run did not end")
	}
	delivery.mu.Lock()
	defer delivery.mu.Unlock()
	for _, ev := range delivery.events {
		eventCh <- ev.Payload
	}
	if delivery.ended.Failure != nil {
		return outcome.RunOutcome{Status: sessionruntime.RunStatusErrored, Cause: delivery.ended.Failure}, nil
	}
	return outcome.RunOutcome{}, nil
}

// ignoreOutcome keeps the error of a WebSocket stream call.
func ignoreOutcome(_ outcome.RunOutcome, err error) error {
	return err
}
