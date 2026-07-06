package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/memohai/twilight-ai/sdk"

	agenttools "github.com/memohai/memoh/internal/agent/tools"
	"github.com/memohai/memoh/internal/contextfrag"
)

type staticToolProvider struct {
	tools []sdk.Tool
}

func (p staticToolProvider) Tools(context.Context, agenttools.SessionContext) ([]sdk.Tool, error) {
	return p.tools, nil
}

type atomicMockProvider struct {
	calls   atomic.Int32
	handler func(call int, params sdk.GenerateParams) (*sdk.GenerateResult, error)
	stream  func(ctx context.Context, params sdk.GenerateParams) (*sdk.StreamResult, error)
}

func (*atomicMockProvider) Name() string {
	return "mock"
}

func (*atomicMockProvider) ListModels(context.Context) ([]sdk.Model, error) {
	return nil, nil
}

func (*atomicMockProvider) Test(context.Context) *sdk.ProviderTestResult {
	return &sdk.ProviderTestResult{Status: sdk.ProviderStatusOK, Message: "ok"}
}

func (*atomicMockProvider) TestModel(context.Context, string) (*sdk.ModelTestResult, error) {
	return &sdk.ModelTestResult{Supported: true, Message: "supported"}, nil
}

func (m *atomicMockProvider) DoGenerate(_ context.Context, params sdk.GenerateParams) (*sdk.GenerateResult, error) {
	call := int(m.calls.Add(1))
	return m.handler(call, params)
}

func (m *atomicMockProvider) DoStream(ctx context.Context, params sdk.GenerateParams) (*sdk.StreamResult, error) {
	if m.stream != nil {
		return m.stream(ctx, params)
	}

	result, err := m.DoGenerate(ctx, params)
	if err != nil {
		return nil, err
	}
	ch := make(chan sdk.StreamPart, 8)
	go func() {
		defer close(ch)
		ch <- &sdk.StartPart{}
		ch <- &sdk.StartStepPart{}
		if result.Text != "" {
			ch <- &sdk.TextStartPart{ID: "mock"}
			ch <- &sdk.TextDeltaPart{ID: "mock", Text: result.Text}
			ch <- &sdk.TextEndPart{ID: "mock"}
		}
		for _, tc := range result.ToolCalls {
			ch <- &sdk.StreamToolCallPart{
				ToolCallID: tc.ToolCallID,
				ToolName:   tc.ToolName,
				Input:      tc.Input,
			}
		}
		ch <- &sdk.FinishStepPart{
			FinishReason: result.FinishReason,
			Usage:        result.Usage,
			Response:     result.Response,
		}
		ch <- &sdk.FinishPart{
			FinishReason: result.FinishReason,
			TotalUsage:   result.Usage,
		}
	}()
	return &sdk.StreamResult{Stream: ch}, nil
}

func TestAgentGenerateStopsOnTerminalTextLoopAbort(t *testing.T) {
	t.Parallel()

	repeatedText := "abcdefghijklmnopqrstuvwxyz0123456789 repeated text chunk for loop detection"
	modelProvider := &atomicMockProvider{
		handler: func(call int, _ sdk.GenerateParams) (*sdk.GenerateResult, error) {
			finishReason := sdk.FinishReasonToolCalls
			var toolCalls []sdk.ToolCall
			if call < 4 {
				toolCalls = []sdk.ToolCall{{
					ToolCallID: "call-terminal",
					ToolName:   "noop_tool",
					Input:      map[string]any{"step": call},
				}}
			} else {
				finishReason = sdk.FinishReasonStop
			}
			return &sdk.GenerateResult{
				Text:         repeatedText,
				FinishReason: finishReason,
				ToolCalls:    toolCalls,
			}, nil
		},
	}

	a := New(Deps{})
	a.SetToolProviders([]agenttools.ToolProvider{
		staticToolProvider{
			tools: []sdk.Tool{{
				Name:       "noop_tool",
				Parameters: &jsonschema.Schema{Type: "object"},
				Execute: func(_ *sdk.ToolExecContext, _ any) (any, error) {
					return map[string]any{"ok": true}, nil
				},
			}},
		},
	})

	_, err := a.Generate(context.Background(), RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: modelProvider},
		Messages:         []sdk.Message{sdk.UserMessage("loop text terminal")},
		SupportsToolCall: true,
		Identity:         SessionContext{BotID: "bot-1"},
		LoopDetection:    LoopDetectionConfig{Enabled: true},
	})
	if !errors.Is(err, ErrTextLoopDetected) {
		t.Fatalf("expected ErrTextLoopDetected, got %v", err)
	}
	if modelProvider.calls.Load() != 4 {
		t.Fatalf("expected terminal text loop to abort on final step, got %d provider calls", modelProvider.calls.Load())
	}
}

func TestAgentGenerateRunsStepReselectorBeforeNextProviderCall(t *testing.T) {
	t.Parallel()

	ledger := contextfrag.NewMutationLedger()
	var secondCallMessages []sdk.Message
	modelProvider := &atomicMockProvider{
		handler: func(call int, params sdk.GenerateParams) (*sdk.GenerateResult, error) {
			switch call {
			case 1:
				return &sdk.GenerateResult{
					FinishReason: sdk.FinishReasonToolCalls,
					ToolCalls: []sdk.ToolCall{{
						ToolCallID: "call-1",
						ToolName:   "lookup",
						Input:      map[string]any{"q": "one"},
					}},
				}, nil
			case 2:
				secondCallMessages = append([]sdk.Message(nil), params.Messages...)
				return &sdk.GenerateResult{
					Text:         "ok",
					FinishReason: sdk.FinishReasonStop,
				}, nil
			default:
				return nil, errors.New("unexpected provider call")
			}
		},
	}

	a := New(Deps{})
	a.SetToolProviders([]agenttools.ToolProvider{
		staticToolProvider{
			tools: []sdk.Tool{{
				Name:       "lookup",
				Parameters: &jsonschema.Schema{Type: "object"},
				Execute: func(_ *sdk.ToolExecContext, _ any) (any, error) {
					return map[string]any{"answer": strings.Repeat("tool-result ", 64)}, nil
				},
			}},
		},
	})

	var reselectorCalls atomic.Int32
	recentProtect := 4096
	_, err := a.Generate(context.Background(), RunConfig{
		Model:                      &sdk.Model{ID: "mock-model", Provider: modelProvider},
		Messages:                   []sdk.Message{sdk.UserMessage("start")},
		SupportsToolCall:           true,
		Identity:                   SessionContext{BotID: "bot-1"},
		ContextMutations:           ledger,
		ContextRecentProtectTokens: &recentProtect,
		ContextStepReselector: func(_ context.Context, input ContextStepSelectionInput) ContextStepSelectionResult {
			reselectorCalls.Add(1)
			if input.InitialMessageCount != 1 {
				t.Fatalf("InitialMessageCount = %d, want 1", input.InitialMessageCount)
			}
			if len(input.Messages) != 3 {
				t.Fatalf("selector input messages = %d, want 3", len(input.Messages))
			}
			// Finding [7]: the resolved recent-protect window travels with the
			// step reselection input.
			if input.RecentProtectTokens == nil || *input.RecentProtectTokens != recentProtect {
				t.Fatalf("RecentProtectTokens = %v, want %d", input.RecentProtectTokens, recentProtect)
			}
			return ContextStepSelectionResult{
				Messages:    append([]sdk.Message(nil), input.Messages[:input.InitialMessageCount]...),
				Dropped:     len(input.Messages) - input.InitialMessageCount,
				DropReasons: map[string]int{"test": len(input.Messages) - input.InitialMessageCount},
			}
		},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if reselectorCalls.Load() != 1 {
		t.Fatalf("step reselector calls = %d, want 1", reselectorCalls.Load())
	}
	if len(secondCallMessages) != 1 {
		t.Fatalf("second provider call messages = %d, want 1", len(secondCallMessages))
	}
	if secondCallMessages[0].Role != sdk.MessageRoleUser {
		t.Fatalf("second provider call first role = %q, want user", secondCallMessages[0].Role)
	}
	records := ledger.Records()
	if len(records) != 1 || records[0].Kind != contextfrag.MutationLoopStepReselection {
		t.Fatalf("mutation records = %#v, want one loop_step_reselection", records)
	}
	if got := ledger.FinalInputHash(); got == "" {
		t.Fatal("final input hash was not updated after step reselection")
	}
}

func TestAgentGenerateFallsBackToMidTaskPruneWhenStepReselectorNoops(t *testing.T) {
	t.Parallel()

	var providerParams []sdk.GenerateParams
	modelProvider := &atomicMockProvider{
		handler: func(call int, params sdk.GenerateParams) (*sdk.GenerateResult, error) {
			providerParams = append(providerParams, sdk.GenerateParams{
				System:   params.System,
				Messages: append([]sdk.Message(nil), params.Messages...),
				Tools:    append([]sdk.Tool(nil), params.Tools...),
			})
			if call <= 4 {
				return &sdk.GenerateResult{
					FinishReason: sdk.FinishReasonToolCalls,
					ToolCalls: []sdk.ToolCall{{
						ToolCallID: "call-prune",
						ToolName:   "lookup",
						Input:      map[string]any{"step": call},
					}},
				}, nil
			}
			return &sdk.GenerateResult{Text: "ok", FinishReason: sdk.FinishReasonStop}, nil
		},
	}

	a := New(Deps{})
	a.SetToolProviders([]agenttools.ToolProvider{
		staticToolProvider{tools: []sdk.Tool{{
			Name:       "lookup",
			Parameters: &jsonschema.Schema{Type: "object"},
			Execute: func(_ *sdk.ToolExecContext, _ any) (any, error) {
				return strings.Repeat("large tool result ", 80), nil
			},
		}}},
	})

	_, err := a.Generate(context.Background(), RunConfig{
		Model:                 &sdk.Model{ID: "mock-model", Provider: modelProvider},
		Messages:              []sdk.Message{sdk.UserMessage("start")},
		SupportsToolCall:      true,
		Identity:              SessionContext{BotID: "bot-1"},
		ContextMutations:      contextfrag.NewMutationLedger(),
		MidTaskPruneThreshold: 4,
		MidTaskPruneKeepSteps: 1,
		ContextStepReselector: func(context.Context, ContextStepSelectionInput) ContextStepSelectionResult {
			return ContextStepSelectionResult{}
		},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if len(providerParams) < 3 {
		t.Fatalf("provider calls = %d, want at least 3", len(providerParams))
	}
	if !hasPrunedToolResult(providerParams[len(providerParams)-1].Messages) {
		t.Fatalf("final provider messages did not include a pruned tool result: %#v", providerParams[len(providerParams)-1].Messages)
	}
}

func TestAgentGeneratePassesRemainingBudgetToStepReselector(t *testing.T) {
	t.Parallel()

	modelProvider := &atomicMockProvider{
		handler: func(call int, _ sdk.GenerateParams) (*sdk.GenerateResult, error) {
			if call == 1 {
				return &sdk.GenerateResult{
					FinishReason: sdk.FinishReasonToolCalls,
					ToolCalls: []sdk.ToolCall{{
						ToolCallID: "call-budget",
						ToolName:   "lookup",
						Input:      map[string]any{"q": "one"},
					}},
				}, nil
			}
			return &sdk.GenerateResult{Text: "ok", FinishReason: sdk.FinishReasonStop}, nil
		},
	}

	a := New(Deps{})
	a.SetToolProviders([]agenttools.ToolProvider{
		staticToolProvider{tools: []sdk.Tool{{
			Name:       "lookup",
			Parameters: &jsonschema.Schema{Type: "object"},
			Execute: func(_ *sdk.ToolExecContext, _ any) (any, error) {
				return map[string]any{"answer": "ok"}, nil
			},
		}}},
	})

	var seenBudget int
	_, err := a.Generate(context.Background(), RunConfig{
		Model:                  &sdk.Model{ID: "mock-model", Provider: modelProvider},
		Messages:               []sdk.Message{sdk.UserMessage(strings.Repeat("prefix ", 80))},
		SupportsToolCall:       true,
		Identity:               SessionContext{BotID: "bot-1"},
		ContextMutations:       contextfrag.NewMutationLedger(),
		ContextBudgetMaxTokens: 20,
		ContextStepReselector: func(_ context.Context, input ContextStepSelectionInput) ContextStepSelectionResult {
			seenBudget = input.BudgetMaxTokens
			return ContextStepSelectionResult{}
		},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if seenBudget <= 0 || seenBudget >= 20 {
		t.Fatalf("step budget = %d, want remaining budget below full run budget", seenBudget)
	}
}

func TestAgentGenerateRejectsNonPrefixPreservingStepSelectionWithoutMutationRecord(t *testing.T) {
	t.Parallel()

	ledger := contextfrag.NewMutationLedger()
	var secondCallMessages []sdk.Message
	modelProvider := &atomicMockProvider{
		handler: func(call int, params sdk.GenerateParams) (*sdk.GenerateResult, error) {
			if call == 1 {
				return &sdk.GenerateResult{
					FinishReason: sdk.FinishReasonToolCalls,
					ToolCalls: []sdk.ToolCall{{
						ToolCallID: "call-guard",
						ToolName:   "lookup",
						Input:      map[string]any{"q": "one"},
					}},
				}, nil
			}
			secondCallMessages = append([]sdk.Message(nil), params.Messages...)
			return &sdk.GenerateResult{Text: "ok", FinishReason: sdk.FinishReasonStop}, nil
		},
	}

	a := New(Deps{})
	a.SetToolProviders([]agenttools.ToolProvider{
		staticToolProvider{tools: []sdk.Tool{{
			Name:       "lookup",
			Parameters: &jsonschema.Schema{Type: "object"},
			Execute: func(_ *sdk.ToolExecContext, _ any) (any, error) {
				return map[string]any{"answer": "ok"}, nil
			},
		}}},
	})

	_, err := a.Generate(context.Background(), RunConfig{
		Model:                 &sdk.Model{ID: "mock-model", Provider: modelProvider},
		Messages:              []sdk.Message{sdk.UserMessage("start")},
		SupportsToolCall:      true,
		Identity:              SessionContext{BotID: "bot-1"},
		ContextMutations:      ledger,
		MidTaskPruneThreshold: 100,
		ContextStepReselector: func(context.Context, ContextStepSelectionInput) ContextStepSelectionResult {
			return ContextStepSelectionResult{
				Messages: []sdk.Message{
					sdk.UserMessage("modified prefix"),
				},
				Dropped: 2,
			}
		},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if len(secondCallMessages) == 0 || textOfMessage(secondCallMessages[0]) != "start" {
		t.Fatalf("second provider call prefix = %#v, want original prefix", secondCallMessages)
	}
	if hasMutationKind(ledger.Records(), contextfrag.MutationLoopStepReselection) {
		t.Fatalf("mutation records = %#v, want no loop_step_reselection when prefix guard rejects selection", ledger.Records())
	}
}

func TestAgentStreamStopsOnToolLoopAbort(t *testing.T) {
	t.Parallel()

	modelProvider := &atomicMockProvider{
		handler: func(call int, _ sdk.GenerateParams) (*sdk.GenerateResult, error) {
			if call >= 20 {
				return &sdk.GenerateResult{
					Text:         "unexpected-final-step",
					FinishReason: sdk.FinishReasonStop,
				}, nil
			}
			return &sdk.GenerateResult{
				FinishReason: sdk.FinishReasonToolCalls,
				ToolCalls: []sdk.ToolCall{{
					ToolCallID: "call-stream",
					ToolName:   "loop_tool",
					Input:      map[string]any{"query": "same"},
				}},
			}, nil
		},
	}

	a := New(Deps{})
	a.SetToolProviders([]agenttools.ToolProvider{
		staticToolProvider{
			tools: []sdk.Tool{{
				Name:       "loop_tool",
				Parameters: &jsonschema.Schema{Type: "object"},
				Execute: func(_ *sdk.ToolExecContext, _ any) (any, error) {
					return map[string]any{"ok": true}, nil
				},
			}},
		},
	})

	var terminal StreamEvent
	for event := range a.Stream(context.Background(), RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: modelProvider},
		Messages:         []sdk.Message{sdk.UserMessage("loop stream")},
		SupportsToolCall: true,
		Identity:         SessionContext{BotID: "bot-1"},
		LoopDetection:    LoopDetectionConfig{Enabled: true},
	}) {
		if event.IsTerminal() {
			terminal = event
		}
	}

	if terminal.Type != EventAgentAbort {
		t.Fatalf("expected EventAgentAbort, got %q", terminal.Type)
	}
}

func TestAgentStreamMarksTerminalTextLoopAsAbort(t *testing.T) {
	t.Parallel()

	repeatedChunk := strings.Repeat("abcd", 64)
	var observedCancel atomic.Bool
	modelProvider := &atomicMockProvider{
		stream: func(ctx context.Context, _ sdk.GenerateParams) (*sdk.StreamResult, error) {
			ch := make(chan sdk.StreamPart, 16)
			go func() {
				defer close(ch)
				send := func(part sdk.StreamPart) bool {
					select {
					case <-ctx.Done():
						observedCancel.Store(true)
						return false
					case ch <- part:
						return true
					}
				}
				if !send(&sdk.StartPart{}) {
					return
				}
				if !send(&sdk.StartStepPart{}) {
					return
				}
				if !send(&sdk.TextStartPart{ID: "mock"}) {
					return
				}
				for i := 0; i < 4; i++ {
					if !send(&sdk.TextDeltaPart{ID: "mock", Text: repeatedChunk}) {
						return
					}
				}
				select {
				case <-ctx.Done():
					observedCancel.Store(true)
					return
				case <-time.After(50 * time.Millisecond):
				}
				if !send(&sdk.TextEndPart{ID: "mock"}) {
					return
				}
				if !send(&sdk.FinishStepPart{FinishReason: sdk.FinishReasonStop}) {
					return
				}
				_ = send(&sdk.FinishPart{FinishReason: sdk.FinishReasonStop})
			}()
			return &sdk.StreamResult{Stream: ch}, nil
		},
	}

	a := New(Deps{})

	var terminal StreamEvent
	for event := range a.Stream(context.Background(), RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: modelProvider},
		Messages:         []sdk.Message{sdk.UserMessage("loop stream text")},
		SupportsToolCall: true,
		Identity:         SessionContext{BotID: "bot-1"},
		LoopDetection:    LoopDetectionConfig{Enabled: true},
	}) {
		if event.IsTerminal() {
			terminal = event
		}
	}

	if !observedCancel.Load() {
		t.Fatal("expected stream provider to observe context cancellation from text-loop abort")
	}
	if terminal.Type != EventAgentAbort {
		t.Fatalf("expected EventAgentAbort, got %q", terminal.Type)
	}
}

func TestAgentStreamMarksRetryTextLoopAsAbort(t *testing.T) {
	t.Parallel()

	repeatedChunk := strings.Repeat("abcd", 64)
	var streamCalls atomic.Int32
	var observedCancel atomic.Bool
	modelProvider := &atomicMockProvider{
		stream: func(ctx context.Context, _ sdk.GenerateParams) (*sdk.StreamResult, error) {
			call := streamCalls.Add(1)
			ch := make(chan sdk.StreamPart, 16)
			go func() {
				defer close(ch)
				send := func(part sdk.StreamPart) bool {
					select {
					case <-ctx.Done():
						observedCancel.Store(true)
						return false
					case ch <- part:
						return true
					}
				}

				if !send(&sdk.StartPart{}) {
					return
				}
				if !send(&sdk.StartStepPart{}) {
					return
				}

				if call == 1 {
					_ = send(&sdk.ErrorPart{Error: errors.New("api error 500")})
					return
				}

				if !send(&sdk.TextStartPart{ID: "mock-retry"}) {
					return
				}
				for i := 0; i < 4; i++ {
					if !send(&sdk.TextDeltaPart{ID: "mock-retry", Text: repeatedChunk}) {
						return
					}
				}
				select {
				case <-ctx.Done():
					observedCancel.Store(true)
					return
				case <-time.After(50 * time.Millisecond):
				}
				if !send(&sdk.TextEndPart{ID: "mock-retry"}) {
					return
				}
				if !send(&sdk.FinishStepPart{FinishReason: sdk.FinishReasonStop}) {
					return
				}
				_ = send(&sdk.FinishPart{FinishReason: sdk.FinishReasonStop})
			}()
			return &sdk.StreamResult{Stream: ch}, nil
		},
	}

	a := New(Deps{})

	var terminal StreamEvent
	for event := range a.Stream(context.Background(), RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: modelProvider},
		Messages:         []sdk.Message{sdk.UserMessage("loop stream retry text")},
		SupportsToolCall: true,
		Identity:         SessionContext{BotID: "bot-1"},
		LoopDetection:    LoopDetectionConfig{Enabled: true},
	}) {
		if event.IsTerminal() {
			terminal = event
		}
	}

	if streamCalls.Load() != 2 {
		t.Fatalf("expected one retry stream attempt, got %d stream calls", streamCalls.Load())
	}
	if !observedCancel.Load() {
		t.Fatal("expected retry stream provider to observe context cancellation from text-loop abort")
	}
	if terminal.Type != EventAgentAbort {
		t.Fatalf("expected EventAgentAbort after retry text-loop abort, got %q", terminal.Type)
	}
}

func TestRunMidStreamRetryMarksTextLoopCancellationAsAborted(t *testing.T) {
	t.Parallel()

	repeatedChunk := strings.Repeat("abcd", 64)
	var observedCancel atomic.Bool
	modelProvider := &atomicMockProvider{
		stream: func(ctx context.Context, _ sdk.GenerateParams) (*sdk.StreamResult, error) {
			ch := make(chan sdk.StreamPart)
			go func() {
				defer close(ch)
				send := func(part sdk.StreamPart) bool {
					select {
					case <-ctx.Done():
						observedCancel.Store(true)
						return false
					case ch <- part:
						return true
					}
				}

				if !send(&sdk.StartPart{}) {
					return
				}
				if !send(&sdk.StartStepPart{}) {
					return
				}
				if !send(&sdk.TextStartPart{ID: "mock-retry-only"}) {
					return
				}
				for i := 0; i < 4; i++ {
					if !send(&sdk.TextDeltaPart{ID: "mock-retry-only", Text: repeatedChunk}) {
						return
					}
				}
				select {
				case <-ctx.Done():
					observedCancel.Store(true)
					return
				case <-time.After(200 * time.Millisecond):
					t.Error("expected text-loop detection to cancel retry stream before any extra part was sent")
					return
				}
			}()
			return &sdk.StreamResult{Stream: ch}, nil
		},
	}

	a := New(Deps{})
	streamCtx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	textLoopGuard := NewTextLoopGuard(LoopDetectedStreakThreshold, LoopDetectedMinNewGramsPerChunk, SentialOptions{})
	textLoopProbeBuffer := NewTextLoopProbeBuffer(LoopDetectedProbeChars, func(text string) {
		result := textLoopGuard.Inspect(text)
		if result.Abort {
			cancel(ErrTextLoopDetected)
		}
	})

	retryResult, aborted := a.runMidStreamRetry(
		context.Background(),
		streamCtx,
		cancel,
		newToolAbortRegistry(),
		make(chan StreamEvent, 32),
		RunConfig{
			Model:         &sdk.Model{ID: "mock-model", Provider: modelProvider},
			Messages:      []sdk.Message{sdk.UserMessage("retry text loop")},
			Identity:      SessionContext{BotID: "bot-1"},
			LoopDetection: LoopDetectionConfig{Enabled: true},
		},
		nil,
		nil,
		nil,
		&sdk.StreamResult{Messages: []sdk.Message{sdk.UserMessage("previous step")}},
		0,
		"api error 500",
		&strings.Builder{},
		textLoopProbeBuffer,
	)

	if retryResult == nil {
		t.Fatal("expected retry result")
	}
	if !observedCancel.Load() {
		t.Fatal("expected retry stream provider to observe context cancellation from text-loop abort")
	}
	if !errors.Is(context.Cause(streamCtx), ErrTextLoopDetected) {
		t.Fatalf("expected stream context cause ErrTextLoopDetected, got %v", context.Cause(streamCtx))
	}
	if !aborted {
		t.Fatal("expected runMidStreamRetry to report aborted when retry stream hit text-loop cancellation")
	}
}

func hasPrunedToolResult(messages []sdk.Message) bool {
	for _, msg := range messages {
		if msg.Role != sdk.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			result, ok := part.(sdk.ToolResultPart)
			if !ok {
				continue
			}
			if strings.Contains(fmt.Sprint(result.Result), "[tool result pruned:") {
				return true
			}
		}
	}
	return false
}

func textOfMessage(msg sdk.Message) string {
	if len(msg.Content) == 0 {
		return ""
	}
	text, _ := msg.Content[0].(sdk.TextPart)
	return text.Text
}

func hasMutationKind(records []contextfrag.MutationRecord, kind contextfrag.MutationKind) bool {
	for _, record := range records {
		if record.Kind == kind {
			return true
		}
	}
	return false
}
