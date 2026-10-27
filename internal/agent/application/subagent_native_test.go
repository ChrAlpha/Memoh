package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/step"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/contextview"
)

type childHistoryStore struct {
	*recordingMessageService
	rows  []messagepkg.Message
	steps []messagepkg.AgentStep
}

func (s *childHistoryStore) ListHistoryWithinBytes(context.Context, string, messagepkg.HistoryWindow) (messagepkg.HistoryLoad, error) {
	return messagepkg.HistoryLoad{Messages: append([]messagepkg.Message(nil), s.rows...)}, nil
}

func (s *childHistoryStore) PersistAgentStep(_ context.Context, step messagepkg.AgentStep) ([]messagepkg.Message, error) {
	s.steps = append(s.steps, step)
	var rows []messagepkg.Message
	for _, input := range step.Messages {
		rows = append(rows, messagepkg.Message{
			ID: uuid.NewString(), BotID: input.BotID, SessionID: input.SessionID,
			Role: input.Role, Content: input.Content, Metadata: input.Metadata,
		})
	}
	s.rows = append(s.rows, rows...)
	return rows, nil
}

func childHistoryRow(t *testing.T, message sdk.Message) messagepkg.Message {
	t.Helper()
	raw, err := historyfrag.MarshalStoredSDKMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	return messagepkg.Message{ID: uuid.NewString(), Role: string(message.Role), Content: raw}
}

func newChildBindingFixture(t *testing.T, inherited int) (*Service, *childHistoryStore, context.Context, tools.SpawnRunConfig) {
	t.Helper()
	store := &childHistoryStore{recordingMessageService: &recordingMessageService{}}
	for i := range inherited {
		store.rows = append(store.rows, childHistoryRow(t, sdk.UserMessage(fmt.Sprintf("source %d: %s", i, strings.Repeat("evidence ", 150)))))
	}
	task := childHistoryRow(t, sdk.UserMessage("Read twelve pages. Preserve task-marker-97 and report the evidence."))
	store.rows = append(store.rows, task)
	ctx := subagentRunContext(uuid.NewString(), uuid.NewString(), 7)
	handle, _ := SubagentRunHandleFromContext(ctx)
	service := &Service{messageService: store, checkpoints: emptyHistoryCheckpoints(), logger: slog.New(slog.DiscardHandler)}
	return service, store, ctx, tools.SpawnRunConfig{
		RunID: handle.RunID, ModelUUID: uuid.NewString(), Query: "Read twelve pages. Preserve task-marker-97 and report the evidence.",
		SessionType: "subagent", SupportsToolCall: true, ContextBudgetMaxTokens: 8000, ModelMaxOutputTokens: 1000,
		TurnRequestMessageID: task.ID,
		Identity:             tools.SpawnIdentity{BotID: handle.BotID, ChatID: handle.BotID, SessionID: handle.SessionID, IsSubagent: true},
	}
}

func TestChildBindingPreservesInjectedUserContentAndProvenance(t *testing.T) {
	s, store, ctx, task := newChildBindingFixture(t, 3)
	cfg := native.RunConfig{RunID: task.RunID, Identity: native.SessionContext{BotID: task.Identity.BotID, SessionID: task.Identity.SessionID}, ContextLifecycle: contextfrag.NewLifecycleHolder()}
	finish, err := s.BindSubagentNativeRun(ctx, task, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Messages) != 4 || len(cfg.ContextProvenance) != 4 || cfg.ContextCurrentUserMessageIndex == nil || *cfg.ContextCurrentUserMessageIndex != 3 {
		t.Fatalf("child request lost its source identities: %+v", cfg.ContextProvenance)
	}
	record := &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("opened image"), sdk.UserMessage("read_media image", sdk.ImagePart{Image: "data:image/png;base64,AA==", MediaType: "image/png"})}}
	directive, err := cfg.OnStepCommitted(ctx, 0, record)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.steps) != 1 || len(store.steps[0].Messages) != 2 || len(directive.Provenance) != 2 {
		t.Fatalf("step dropped injected user content: steps=%+v sources=%+v", store.steps, directive.Provenance)
	}
	for _, input := range store.steps[0].Messages {
		if input.TurnRequestMessageID != task.TurnRequestMessageID {
			t.Fatalf("step escaped task turn: %+v", input)
		}
	}
	if err := finish(ctx); err != nil {
		t.Fatal(err)
	}
}

type childScriptProvider struct {
	abortAlignmentProvider
	t     *testing.T
	calls int
}

func (p *childScriptProvider) DoStream(_ context.Context, request sdk.Request) (<-chan sdk.StreamPart, error) {
	p.calls++
	if cost := contextfrag.ProviderEnvelopeTokens(request.System, request.Messages, request.Tools); cost > 7000 {
		p.t.Fatalf("oversized request dispatched: %d", cost)
	}
	var text strings.Builder
	for _, message := range request.Messages {
		for _, part := range message.Content {
			if part, ok := part.(sdk.TextPart); ok {
				text.WriteString(part.Text)
			}
		}
	}
	if !strings.Contains(text.String(), "task-marker-97") {
		p.t.Fatal("task constraint lost after fold")
	}
	if strings.Contains(text.String(), "earlier messages were trimmed") {
		p.t.Fatal("child history was silently trimmed")
	}
	parts := make(chan sdk.StreamPart, 8)
	parts <- &sdk.StartPart{}
	parts <- &sdk.StartStepPart{}
	reason := sdk.FinishReasonStop
	if p.calls <= 12 {
		reason = sdk.FinishReasonToolCalls
		parts <- &sdk.StreamToolCallPart{ToolCallID: fmt.Sprintf("lookup-%d", p.calls), ToolName: "lookup", Input: toolexec.ArgumentsFromValue(map[string]any{"page": p.calls})}
	} else {
		parts <- &sdk.TextStartPart{ID: "final"}
		parts <- &sdk.TextDeltaPart{ID: "final", Text: "completed twelve lookups task-marker-97"}
		parts <- &sdk.TextEndPart{ID: "final"}
	}
	parts <- &sdk.FinishStepPart{FinishReason: reason}
	parts <- &sdk.FinishPart{FinishReason: reason}
	close(parts)
	return parts, nil
}

type childLookupProvider struct{ calls int }

func (p *childLookupProvider) Tools(context.Context, tools.SessionContext) ([]toolexec.Tool, error) {
	return []toolexec.Tool{{Name: "lookup", Parameters: &jsonschema.Schema{Type: "object"}, Execute: func(*toolexec.ToolExecContext, sdk.ToolArguments) (sdk.ToolOutput, error) {
		p.calls++
		return sdk.TextOutput(strings.Repeat("evidence ", 900)), nil
	}}}, nil
}

// The fold boundary is scripted here; PostgreSQL checkpoint tests separately
// verify that the hidden fork rows are valid sources and remain child-owned.
type childFoldProbe struct {
	switchRunner
	t         *testing.T
	child     string
	calls     int
	failAfter int
}

func (p *childFoldProbe) Fold(_ context.Context, ev compaction.Event) ([]compaction.Pass, error) {
	p.calls++
	if ev.SessionID != p.child {
		p.t.Fatal("fold touched the parent's checkpoint")
	}
	if p.failAfter > 0 && p.calls >= p.failAfter {
		return nil, errors.New("scripted summarizer failure")
	}
	order := []int{}
	for i := range ev.Cut {
		if i < len(ev.Sources) && ev.Sources[i].Anchor {
			order = append(order, i)
		} else if i >= len(ev.Sources) || len(ev.Sources[i].Sources) == 0 {
			p.t.Fatalf("fold has an unsourced message at %d", i)
		}
	}
	order = append(order, -1)
	cut := len(order)
	for i := ev.Cut; i < len(ev.Request.Messages); i++ {
		order = append(order, i)
	}
	pass := compaction.Pass{SummaryID: uuid.NewString(), Summary: "Prior evidence is recorded. Continue the task.", Order: order, Cut: cut}
	ev.Apply(pass)
	return []compaction.Pass{pass}, nil
}

func TestNativeChildFoldsAtStepZeroAndThroughoutOneTask(t *testing.T) {
	for _, inherited := range []int{0, 100} {
		t.Run(strconv.Itoa(inherited), func(t *testing.T) {
			s, store, ctx, task := newChildBindingFixture(t, inherited)
			fold := &childFoldProbe{t: t, child: task.Identity.SessionID}
			s.compactionService = fold
			provider := &childScriptProvider{t: t}
			task.Model = &sdk.Model{ID: "scripted", Provider: provider}
			lookup := &childLookupProvider{}
			agent := native.New(native.Deps{ContextViewApplier: contextview.ProviderRunConfigApplier(s.logger)})
			agent.SetToolProviders([]tools.ToolProvider{lookup})
			adapter := native.NewSpawnAdapter(agent)
			adapter.SetRunBindingFactory(s.BindSubagentNativeRun)
			result, err := adapter.GenerateWithWatchdog(ctx, task, func() {})
			if err != nil {
				t.Fatal(err)
			}
			if result == nil || !result.Persisted || !strings.Contains(result.Text, "completed twelve") || lookup.calls != 12 || len(store.steps) != 13 || fold.calls < 2 {
				t.Fatalf("result=%+v calls=%d steps=%d folds=%d", result, lookup.calls, len(store.steps), fold.calls)
			}
		})
	}
}

func TestChildWatchdogPausesForBoundedCompaction(t *testing.T) {
	ctx, watchdog := NewSubagentWatchdog(t.Context(), 30*time.Millisecond, slog.New(slog.DiscardHandler))
	defer watchdog.Stop()
	watchdog.Pause()
	time.Sleep(80 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("outer watchdog expired during compaction")
	}
	watchdog.Resume()
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), tools.ErrWatchdogTimedOut) {
			t.Fatal(context.Cause(ctx))
		}
	case <-time.After(time.Second):
		t.Fatal("resumed watchdog did not expire")
	}
}

func TestNativeChildFoldFailureKeepsCommittedEffectsWithoutReplay(t *testing.T) {
	s, store, ctx, task := newChildBindingFixture(t, 0)
	fold := &childFoldProbe{t: t, child: task.Identity.SessionID, failAfter: 1}
	s.compactionService = fold
	task.Model = &sdk.Model{ID: "scripted", Provider: &childScriptProvider{t: t}}
	lookup := &childLookupProvider{}
	agent := native.New(native.Deps{ContextViewApplier: contextview.ProviderRunConfigApplier(s.logger)})
	agent.SetToolProviders([]tools.ToolProvider{lookup})
	adapter := native.NewSpawnAdapter(agent)
	adapter.SetRunBindingFactory(s.BindSubagentNativeRun)
	result, err := adapter.GenerateWithWatchdog(ctx, task, func() {})
	if err == nil || result == nil || !result.Persisted || lookup.calls == 0 || len(store.steps) != lookup.calls {
		t.Fatalf("fold failure lost committed effects: err=%v result=%+v calls=%d steps=%d", err, result, lookup.calls, len(store.steps))
	}
	if got := subagentAttemptDisposition(ctx, err, result.Persisted, true); got != tools.SpawnAttemptFailure {
		t.Fatalf("fold error would replay effects: %v", got)
	}
}

func TestChildFailureBeforeModelUsageRemainsUnknown(t *testing.T) {
	s, _, ctx, task := newChildBindingFixture(t, 100)
	s.compactionService = &childFoldProbe{t: t, child: task.Identity.SessionID, failAfter: 1}
	task.Model = &sdk.Model{ID: "scripted", Provider: &childScriptProvider{t: t}}
	agent := native.New(native.Deps{ContextViewApplier: contextview.ProviderRunConfigApplier(s.logger)})
	adapter := native.NewSpawnAdapter(agent)
	adapter.SetRunBindingFactory(s.BindSubagentNativeRun)
	result, err := adapter.GenerateWithWatchdog(ctx, task, func() {})
	if err == nil || (result != nil && result.Usage != nil) {
		t.Fatalf("pre-dispatch failure reported usage: result=%+v err=%v", result, err)
	}
}
