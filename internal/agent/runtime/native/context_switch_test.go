package native

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/step"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/agent/turn"
)

func rowSources(ids ...string) []turn.ContextMessageSource {
	sources := make([]turn.ContextMessageSource, len(ids))
	for i, id := range ids {
		sources[i] = turn.ContextMessageSource{Kind: "history", ID: id}
	}
	return sources
}

// switchRequest is an armed run's request before its third step: an earlier
// summary, two history rows around a pipeline message the fold leaves alone
// and a passive message the run never rendered as a source, the run's input,
// its first step, a steer, and its latest step. The provenance describes the
// request the run compiled, up to the steer, by position.
func switchRequest() ([]sdk.Message, []MessageProvenance) {
	call := func(id string) sdk.Message {
		return sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{ToolCallID: id, ToolName: "effect", Input: toolexec.ArgumentsFromValue(map[string]any{"id": id})}}}
	}
	result := func(id string) sdk.Message {
		return sdk.ToolMessage(sdk.ToolResultPart{ToolCallID: id, ToolName: "effect", Result: toolexec.OutputFromValue(id + strings.Repeat(" r", 2000))})
	}
	messages := []sdk.Message{
		sdk.UserMessage("<summary>\nolder\n"),
		sdk.UserMessage("h1" + strings.Repeat(" h", 2000)),
		sdk.UserMessage("pipeline message"),
		sdk.UserMessage("passive message"),
		sdk.AssistantMessage("h2" + strings.Repeat(" h", 2000)),
		sdk.UserMessage("INPUT"),
		call("c1"), result("c1"),
		sdk.UserMessage("STEER"),
		call("c2"), result("c2"),
	}
	sources := [][]turn.ContextMessageSource{
		{{Kind: "summary", ID: "A0"}},
		rowSources("h1"),
		{{Kind: "external", ID: "rc1"}},
		nil, rowSources("h2"),
		rowSources("input"), rowSources("c1"), rowSources("r1"), rowSources("steer"),
	}
	provenance := make([]MessageProvenance, len(sources))
	for i, source := range sources {
		provenance[i] = MessageProvenance{Sources: source, Anchor: i == 5 || i == 8}
	}
	return messages, provenance
}

// armedRun is a run armed with event that has committed the latest step of
// switchRequest, with an allowance only the request spliced to keep fits in.
func armedRun(t *testing.T, messages []sdk.Message, provenance []MessageProvenance, keep []int, event func(context.Context, ContextSwitch) (string, error)) (RunConfig, *error) {
	t.Helper()
	var failure error
	var kept []sdk.Message
	for _, i := range keep {
		kept = append(kept, messages[i])
	}
	cfg := RunConfig{
		ContextMutations:  contextfrag.NewMutationLedger(),
		ContextManifest:   contextfrag.Manifest{BudgetPlan: &contextfrag.ContextBudgetPlan{Window: contextfrag.ProviderEnvelopeTokens("", append(kept, sdk.UserMessage("<summary>\nnew\n")), nil) + 8}},
		SwitchContext:     event,
		Messages:          messages[:len(provenance)],
		ContextProvenance: provenance,
	}
	cfg.contextSwitch = newContextSwitchState(cfg)
	cfg.contextSwitch.committed(messages[9:], []MessageProvenance{{Sources: rowSources("c2")}, {Sources: rowSources("r2")}})
	cfg.contextStepFailure = func(err error) { failure = err }
	return cfg, &failure
}

func prepareArmed(cfg RunConfig, messages []sdk.Message) *sdk.Request {
	return prepareProviderAttempt(context.Background(), cfg, newProviderAttemptHandoff(cfg), false, 6, 2, nil,
		&sdk.Request{Messages: append([]sdk.Message(nil), messages...)})
}

func switchDetails(cfg RunConfig) []string {
	var details []string
	for _, record := range cfg.ContextMutations.Records() {
		if record.Kind == contextfrag.MutationContextSwitch {
			details = append(details, record.Detail)
		}
	}
	return details
}

// The event sees the request the run would send and what each of its
// messages renders, anchors included, and the request the run sends next is
// the one it had with the covered messages replaced by the summary at rule
// P: every other message is the same message, in its order.
func TestArmedRunFoldsItsOwnRequestInPlace(t *testing.T) {
	t.Parallel()

	messages, provenance := switchRequest()
	summary := sdk.UserMessage("<summary>\nnew\n")
	want := []sdk.Message{messages[2], messages[3], messages[5], summary, messages[8], messages[9], messages[10]}
	var seen ContextSwitch
	var spliced ContextSplice
	cfg, failure := armedRun(t, messages, provenance, []int{2, 3, 5, 8, 9, 10}, func(_ context.Context, sw ContextSwitch) (string, error) {
		seen = sw
		spliced = sw.Fold(foldCovering(sw, "A1", summary, append(rowSources("h1", "h2", "c1", "r1"), summarySource("A0"))...))
		return "trigger=in_run passes=1", nil
	})
	cfg.ContextCachePlan = contextfrag.CachePlan{StableMessageCount: 5, StablePrefixTokenEstimate: 4000}
	params := prepareArmed(cfg, messages)

	if *failure != nil || !reflect.DeepEqual(params.Messages, want) {
		t.Fatalf("failure=%v messages=%d, want the request spliced at rule P with every other message unchanged", *failure, len(params.Messages))
	}
	rendered := make([][]turn.ContextMessageSource, len(seen.Provenance))
	var anchors []int
	for i, p := range seen.Provenance {
		if p != nil {
			rendered[i] = p.Sources
			if p.Anchor {
				anchors = append(anchors, i)
			}
		}
	}
	sources := [][]turn.ContextMessageSource{
		{{Kind: "summary", ID: "A0"}},
		rowSources("h1"),
		{{Kind: "external", ID: "rc1"}},
		nil, rowSources("h2"),
		rowSources("input"), rowSources("c1"), rowSources("r1"), rowSources("steer"), rowSources("c2"), rowSources("r2"),
	}
	if !reflect.DeepEqual(seen.Request.Messages, messages) || !reflect.DeepEqual(rendered, sources) || !reflect.DeepEqual(anchors, []int{5, 8}) {
		t.Fatalf("the event saw %d messages rendering %v and anchors %v", len(seen.Request.Messages), rendered, anchors)
	}
	if seen.WindowTokens != cfg.ContextManifest.BudgetPlan.Window || seen.UsageTokens <= seen.AllowanceTokens || seen.Summarize == nil {
		t.Fatalf("window=%d usage=%d allowance=%d summarize=%v", seen.WindowTokens, seen.UsageTokens, seen.AllowanceTokens, seen.Summarize != nil)
	}
	if !reflect.DeepEqual(spliced.Request.Messages, want) || spliced.Tokens > seen.AllowanceTokens || spliced.Provenance[3].Sources[0].ID != "A1" {
		t.Fatalf("splice = %d messages, %d tokens over a %d allowance", len(spliced.Request.Messages), spliced.Tokens, seen.AllowanceTokens)
	}
	if !reflect.DeepEqual(switchDetails(cfg), []string{"trigger=in_run passes=1"}) {
		t.Fatalf("details=%q", switchDetails(cfg))
	}

	cfg.Messages = messages
	next, ok := cfg.contextSwitch.redispatch(cfg, generateDispatch{initialMessageCount: len(messages)}, *params)
	if !ok || len(next.Messages) != len(want) || next.initialProviderMessageCount != len(want) || next.ContextCachePlan.StableMessageCount != 2 {
		t.Fatalf("redispatch ok=%v messages=%d prefix=%d stable=%d, want the spliced request with its prefix and breakpoint moved", ok, len(next.Messages), next.initialProviderMessageCount, next.ContextCachePlan.StableMessageCount)
	}
	estimate := 4000 + messagesTokens(want[:2]) - messagesTokens(messages[:5])
	if plan := next.ContextCachePlan; plan.StablePrefixTokenEstimate != estimate {
		t.Fatalf("cache plan = %+v, want the estimate %d of the spliced prefix", plan, estimate)
	}
	if _, again := cfg.contextSwitch.redispatch(cfg, generateDispatch{}, *params); again {
		t.Fatal("a second redispatch without a splice")
	}
}

// An armed run whose event cannot make the request fit ends the run with the
// event's reason recorded and sends nothing: no step is dropped or trimmed
// to rescue it.
func TestArmedRunFailsClosedWhenTheEventDoesNotFit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		fold   []turn.ContextMessageSource
		err    error
		want   error
		detail string
	}{
		{name: "summarizer down", err: fmt.Errorf("%w: summarizer_failed", contextfrag.ErrBudgetUnsatisfied), want: contextfrag.ErrBudgetUnsatisfied, detail: "trigger=in_run passes=0 reason=summarizer_failed"},
		{name: "protected overflow", err: fmt.Errorf("%w: protected_overflow", contextfrag.ErrProtectedContextOverflow), want: contextfrag.ErrProtectedContextOverflow, detail: "trigger=in_run passes=0 reason=protected_overflow"},
		{name: "result without its call", fold: rowSources("h1", "h2", "r1"), want: contextfrag.ErrBudgetUnsatisfied, detail: "trigger=in_run reason=postcondition"},
		{name: "call without its result", fold: rowSources("h1", "h2", "c1"), want: contextfrag.ErrBudgetUnsatisfied, detail: "trigger=in_run reason=postcondition"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			messages, provenance := switchRequest()
			cfg, failure := armedRun(t, messages, provenance, []int{2, 3, 5, 8, 9, 10}, func(_ context.Context, sw ContextSwitch) (string, error) {
				if tc.fold != nil {
					sw.Fold(foldCovering(sw, "A1", sdk.UserMessage("<summary>\nnew\n"), tc.fold...))
					return "", nil
				}
				return tc.detail, tc.err
			})
			prepareArmed(cfg, messages)
			details := switchDetails(cfg)
			if !errors.Is(*failure, tc.want) || len(details) != 1 || !strings.HasPrefix(details[0], tc.detail) {
				t.Fatalf("failure=%v details=%q, want %v recorded as %q", *failure, details, tc.want, tc.detail)
			}
		})
	}
}

// A call whose run ended before its answer is closed by a result the read
// path writes from the call's row. A fold that covers the call removes the
// closure with it; a closure that named no source would stay behind, answer
// a call the request no longer carries, and fail the event instead of being
// sent.
func TestArmedRunFoldsAClosureWithItsCall(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		sources []turn.ContextMessageSource
		want    error
	}{
		{name: "closure renders from the call row", sources: rowSources("c0")},
		{name: "closure without a source", want: contextfrag.ErrBudgetUnsatisfied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			messages, provenance := switchRequest()
			abandoned := sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{ToolCallID: "c0", ToolName: "ask_user", Input: toolexec.ArgumentsFromValue(map[string]any{})}}}
			closure := sdk.ToolMessage(sdk.ToolResultPart{ToolCallID: "c0", ToolName: "ask_user", Result: toolexec.OutputFromValue("interrupted"), IsError: true})
			messages = slices.Insert(messages, 2, abandoned, closure)
			provenance = slices.Insert(provenance, 2, MessageProvenance{Sources: rowSources("c0")}, MessageProvenance{Sources: tc.sources})
			summary := sdk.UserMessage("<summary>\nnew\n")
			want := []sdk.Message{messages[4], messages[5], messages[7], summary, messages[10], messages[11], messages[12]}
			// The window also holds a closure left behind, as an orphan
			// small enough to send would be.
			window := contextfrag.ProviderEnvelopeTokens("", append(slices.Clone(want), closure), nil) + 8
			var failure error
			cfg := RunConfig{
				ContextMutations: contextfrag.NewMutationLedger(),
				ContextManifest:  contextfrag.Manifest{BudgetPlan: &contextfrag.ContextBudgetPlan{Window: window}},
				SwitchContext: func(_ context.Context, sw ContextSwitch) (string, error) {
					sw.Fold(foldCovering(sw, "A1", summary, append(rowSources("h1", "c0", "h2", "c1", "r1"), summarySource("A0"))...))
					return "trigger=in_run passes=1", nil
				},
				Messages:          messages[:len(provenance)],
				ContextProvenance: provenance,
			}
			cfg.contextSwitch = newContextSwitchState(cfg)
			cfg.contextSwitch.committed(messages[11:], []MessageProvenance{{Sources: rowSources("c2")}, {Sources: rowSources("r2")}})
			cfg.contextStepFailure = func(err error) { failure = err }
			params := prepareProviderAttempt(context.Background(), cfg, newProviderAttemptHandoff(cfg), false, 8, 2, nil,
				&sdk.Request{Messages: slices.Clone(messages)})

			if tc.want != nil {
				details := switchDetails(cfg)
				if !errors.Is(failure, tc.want) || len(details) != 1 || !strings.HasPrefix(details[0], "trigger=in_run reason=postcondition") {
					t.Fatalf("failure=%v details=%q, want the event to fail on the result it left without its call", failure, details)
				}
				return
			}
			if failure != nil || !reflect.DeepEqual(params.Messages, want) {
				t.Fatalf("failure=%v messages=%d, want the call and its closure replaced by the summary and every other message unchanged", failure, len(params.Messages))
			}
		})
	}
}

// An armed run whose next step does not fit goes on from the request the
// event spliced, in both loops: the summary replaces the steps before the
// latest one, the input stays where it was, and the steps after the event
// are appended to the spliced request.
func TestArmedRunContinuesOnItsSplicedRequest(t *testing.T) {
	t.Parallel()

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Parallel()
			var requests []sdk.Request
			provider := &atomicMockProvider{handler: func(call int, params sdk.Request) (sdk.ModelResult, error) {
				requests = append(requests, params)
				if call <= 4 {
					return sdk.ModelResult{FinishReason: sdk.FinishReasonToolCalls, ToolCalls: []sdk.ToolCall{{
						ToolCallID: fmt.Sprintf("c%d", call), ToolName: "effect", Input: toolexec.ArgumentsFromValue(map[string]any{"n": call}),
					}}}, nil
				}
				return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
			}}
			a := New(Deps{})
			a.SetToolProviders([]agenttools.ToolProvider{staticToolProvider{tools: []toolexec.Tool{{
				Name: "effect", Parameters: &jsonschema.Schema{Type: "object"},
				Execute: func(*toolexec.ToolExecContext, sdk.ToolArguments) (sdk.ToolOutput, error) {
					return toolexec.OutputFromValue(strings.Repeat(" r", 2000)), nil
				},
			}}}})
			input := sdk.UserMessage("INPUT")
			summary := sdk.UserMessage("<summary>\nsteps\n")
			var events []ContextSwitch
			cfg := RunConfig{
				Model: &sdk.Model{ID: "mock-model", Provider: provider}, Messages: []sdk.Message{input}, SupportsToolCall: true,
				Identity: SessionContext{BotID: "bot-1", SessionID: "sess-1"}, ContextMutations: contextfrag.NewMutationLedger(),
				ContextManifest:   contextfrag.Manifest{BudgetPlan: &contextfrag.ContextBudgetPlan{Window: 2600}},
				ContextProvenance: []MessageProvenance{{Sources: rowSources("input"), Anchor: true}},
				OnStepCommitted: func(_ context.Context, index int, record *step.Record) (StepDirective, error) {
					provenance := make([]MessageProvenance, len(record.Messages))
					for i := range provenance {
						provenance[i].Sources = rowSources(fmt.Sprintf("s%d.%d", index, i))
					}
					return StepDirective{Provenance: provenance}, nil
				},
				SwitchContext: func(_ context.Context, sw ContextSwitch) (string, error) {
					events = append(events, sw)
					sw.Fold(foldCovering(sw, "a1", summary, rowSources("s0.0", "s0.1", "s1.0", "s1.1")...))
					return "trigger=in_run passes=1", nil
				},
			}
			if stream {
				for event := range a.Stream(context.Background(), cfg) {
					if event.Type == EventError {
						t.Fatalf("stream failed: %v", event.Cause)
					}
				}
			} else if _, err := a.Generate(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 5 || len(events) != 1 || !reflect.DeepEqual(events[0].Provenance[5].Sources, rowSources("s2.0")) {
				t.Fatalf("provider calls=%d events=%d, want one event before the fourth call that knows the third step", len(requests), len(events))
			}
			spliced, next := requests[3].Messages, requests[4].Messages
			call, _ := spliced[min(2, len(spliced)-1)].Content[0].(sdk.ToolCallPart)
			if len(spliced) != 4 || !reflect.DeepEqual(spliced[0], input) || !reflect.DeepEqual(spliced[1], summary) || call.ToolCallID != "c3" || spliced[3].Role != sdk.MessageRoleTool {
				t.Fatalf("fourth call sent %d messages, want the input, the summary and the latest step", len(spliced))
			}
			if len(next) != 6 || !reflect.DeepEqual(next[:4], spliced) {
				t.Fatalf("fifth call sent %d messages, want the spliced request with the fourth step appended", len(next))
			}
		})
	}
}

// A run whose first request does not fit folds it before its first dispatch,
// in both loops: the event sees the request the run would send, its input an
// anchor, and the run dispatches the spliced request once.
func TestArmedRunFoldsBeforeItsFirstStep(t *testing.T) {
	t.Parallel()

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Parallel()
			var requests []sdk.Request
			provider := &atomicMockProvider{handler: func(_ int, params sdk.Request) (sdk.ModelResult, error) {
				requests = append(requests, params)
				return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
			}}
			older := sdk.UserMessage("older" + strings.Repeat(" h", 2000))
			answer := sdk.AssistantMessage("answer" + strings.Repeat(" h", 2000))
			input := sdk.UserMessage("INPUT")
			summary := sdk.UserMessage("<summary>\nolder\n")
			request := turn.ContextMessageSource{Kind: "history", Current: true}
			current := 2
			var events []ContextSwitch
			cfg := RunConfig{
				Model: &sdk.Model{ID: "mock-model", Provider: provider}, Messages: []sdk.Message{older, answer, input},
				Identity: SessionContext{BotID: "bot-1", SessionID: "sess-1"}, ContextMutations: contextfrag.NewMutationLedger(),
				ContextManifest:                contextfrag.Manifest{BudgetPlan: &contextfrag.ContextBudgetPlan{Window: 1200}},
				ContextCurrentUserMessageIndex: &current,
				ContextProvenance: []MessageProvenance{
					{Sources: rowSources("older")},
					{Sources: rowSources("answer")},
					{Sources: []turn.ContextMessageSource{request}, Anchor: true},
				},
				SwitchContext: func(_ context.Context, sw ContextSwitch) (string, error) {
					events = append(events, sw)
					sw.Fold(foldCovering(sw, "a1", summary, rowSources("older", "answer")...))
					return "trigger=in_run passes=1", nil
				},
			}
			if stream {
				for event := range New(Deps{}).Stream(context.Background(), cfg) {
					if event.Type == EventError {
						t.Fatalf("stream failed: %v", event.Cause)
					}
				}
			} else if _, err := New(Deps{}).Generate(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || len(requests) != 1 {
				t.Fatalf("events=%d provider calls=%d, want one event before the only call", len(events), len(requests))
			}
			sw := events[0]
			if sw.Request.Model != "mock-model" || len(sw.Request.Messages) != 3 || !sw.Provenance[2].Anchor {
				t.Fatalf("event model=%q messages=%d, want the run's whole first request with its input an anchor", sw.Request.Model, len(sw.Request.Messages))
			}
			if got := requests[0].Messages; len(got) != 2 || !reflect.DeepEqual(got[0], summary) || !reflect.DeepEqual(got[1], input) {
				t.Fatalf("first call sent %d messages, want the summary and the input", len(got))
			}
		})
	}
}

// A pass that covers an input with images, such as an older input of a
// Discuss batch, records them omitted, as no summary carries an image.
func TestArmedRunRecordsTheImagesOfAFoldedInput(t *testing.T) {
	t.Parallel()

	var requests []sdk.Request
	provider := &atomicMockProvider{handler: func(_ int, params sdk.Request) (sdk.ModelResult, error) {
		requests = append(requests, params)
		return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
	}}
	pictured := sdk.UserMessage("older input"+strings.Repeat(" o", 1000), sdk.ImagePart{Image: "data:image/png;base64,AAAA", MediaType: "image/png"})
	answered := sdk.AssistantMessage("answered" + strings.Repeat(" a", 1000))
	newest := sdk.UserMessage("NEWEST")
	summary := sdk.UserMessage("<summary>\nolder\n")
	var events []ContextSwitch
	cfg := RunConfig{
		Model: &sdk.Model{ID: "mock-model", Provider: provider}, Messages: []sdk.Message{pictured, answered, newest},
		Identity: SessionContext{BotID: "bot-1", SessionID: "sess-1"}, ContextMutations: contextfrag.NewMutationLedger(),
		ContextManifest: contextfrag.Manifest{BudgetPlan: &contextfrag.ContextBudgetPlan{Window: 600}},
		ContextProvenance: []MessageProvenance{
			{Sources: []turn.ContextMessageSource{{Kind: "external", ID: "older"}}, Anchor: true},
			{Sources: rowSources("answered")},
			{Sources: []turn.ContextMessageSource{{Kind: "external", ID: "newest"}}, Anchor: true},
		},
		SwitchContext: func(_ context.Context, sw ContextSwitch) (string, error) {
			events = append(events, sw)
			sw.Fold(foldCovering(sw, "a1", summary, append(rowSources("answered"), turn.ContextMessageSource{Kind: "external", ID: "older"})...))
			return "trigger=in_run passes=1", nil
		},
	}
	if _, err := New(Deps{}).Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || len(events[0].Request.Messages) != 3 {
		t.Fatalf("events=%d, want one event over the whole request", len(events))
	}
	if got := requests[0].Messages; len(got) != 2 || !reflect.DeepEqual(got[0], summary) || !reflect.DeepEqual(got[1], newest) {
		t.Fatalf("first call sent %d messages, want the summary and the newest input", len(got))
	}
	var omitted []string
	for _, record := range cfg.ContextMutations.Records() {
		if record.Kind == contextfrag.MutationCurrentInputImagesOmitted {
			omitted = append(omitted, record.Detail)
		}
	}
	if !reflect.DeepEqual(omitted, []string{"sources=older"}) {
		t.Fatalf("omitted images = %q, want the folded input's", omitted)
	}
}

// foldCovering is the fold an event commits when its summary stands for
// covered: every message whose sources it all covers goes, and the summary
// takes its place, as the engine orders a pass.
func foldCovering(sw ContextSwitch, summaryID string, summary sdk.Message, covered ...turn.ContextMessageSource) ContextFold {
	set := make(map[turn.ContextMessageSource]bool, len(covered))
	for _, source := range covered {
		set[turn.ContextMessageSource{Kind: source.Kind, ID: source.ID}] = true
	}
	order := make([]int, len(sw.Request.Messages))
	for i := range order {
		order[i] = i
	}
	order, _ = turn.PlaceSummary(order, -1, func(i int) bool {
		if i < 0 || sw.Provenance[i] == nil || len(sw.Provenance[i].Sources) == 0 {
			return false
		}
		for _, source := range sw.Provenance[i].Sources {
			if !set[turn.ContextMessageSource{Kind: source.Kind, ID: source.ID}] {
				return false
			}
		}
		return true
	})
	return ContextFold{SummaryID: summaryID, Summary: summary, Order: order}
}

func summarySource(id string) turn.ContextMessageSource {
	return turn.ContextMessageSource{Kind: "summary", ID: id}
}

// The run knows its compiled request by position, not by its bytes: two
// stored messages with the same text keep their own rows, and runtime text
// that reads like a stored message stays runtime context. A request its
// provenance does not describe fails the fold rather than guess.
func TestArmedRunKnowsItsCompiledRequestByPosition(t *testing.T) {
	t.Parallel()

	ok := sdk.UserMessage("OK" + strings.Repeat(" o", 1000))
	messages := []sdk.Message{ok, sdk.AssistantMessage("noted"), ok, ok, sdk.UserMessage("INPUT")}
	provenance := []MessageProvenance{
		{Sources: rowSources("first")},
		{Sources: rowSources("noted")},
		{Sources: rowSources("second")},
		{},
		{Sources: rowSources("input"), Anchor: true},
	}
	var seen []*MessageProvenance
	cfg := RunConfig{
		ContextMutations:  contextfrag.NewMutationLedger(),
		ContextManifest:   contextfrag.Manifest{BudgetPlan: &contextfrag.ContextBudgetPlan{Window: 64}},
		Messages:          messages,
		ContextProvenance: provenance,
		SwitchContext: func(_ context.Context, sw ContextSwitch) (string, error) {
			seen = sw.Provenance
			return "", fmt.Errorf("%w: stop", contextfrag.ErrBudgetUnsatisfied)
		},
	}
	cfg.contextSwitch = newContextSwitchState(cfg)
	params := sdk.Request{Messages: slices.Clone(messages)}
	if _, err := cfg.contextSwitch.fold(context.Background(), cfg, &params, 0, 32); !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("fold err = %v", err)
	}
	want := [][]turn.ContextMessageSource{rowSources("first"), rowSources("noted"), rowSources("second"), nil, rowSources("input")}
	if len(seen) != len(want) {
		t.Fatalf("event saw provenance for %d messages, want %d", len(seen), len(want))
	}
	for i, w := range want {
		var got []turn.ContextMessageSource
		if seen[i] != nil {
			got = seen[i].Sources
		}
		if !reflect.DeepEqual(got, w) {
			t.Fatalf("message %d renders %v, want %v", i, got, w)
		}
	}

	cfg.Messages = messages[1:]
	cfg.contextSwitch = newContextSwitchState(cfg)
	seen = nil
	if _, err := cfg.contextSwitch.fold(context.Background(), cfg, &params, 0, 32); !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) || seen != nil {
		t.Fatalf("fold of a request its provenance does not describe: err=%v event=%v", err, seen != nil)
	}
}

// After a splice the run compiles the spliced request up to where its
// compiled prefix ended, past a promoted system message, and knows what
// follows by what its steps committed: a background message the loop strips
// and re-adds, and the steps after the splice, keep every position right.
func TestArmedRunKnowsItsRequestAfterASplice(t *testing.T) {
	t.Parallel()

	long := func(text string) string { return text + strings.Repeat(" x", 1000) }
	system := sdk.SystemMessage("system")
	compiled := []sdk.Message{sdk.UserMessage(long("older")), sdk.AssistantMessage(long("answer")), sdk.UserMessage("INPUT")}
	step := []sdk.Message{sdk.AssistantMessage(long("step one"))}
	summary := sdk.UserMessage("<summary>\nolder\n")
	folds := 0
	var seen []*MessageProvenance
	cfg := RunConfig{
		ContextMutations: contextfrag.NewMutationLedger(),
		ContextManifest:  contextfrag.Manifest{BudgetPlan: &contextfrag.ContextBudgetPlan{Window: 1200}},
		Messages:         compiled,
		ContextProvenance: []MessageProvenance{
			{Sources: rowSources("older")}, {Sources: rowSources("answer")}, {Sources: rowSources("input"), Anchor: true},
		},
		SwitchContext: func(_ context.Context, sw ContextSwitch) (string, error) {
			folds++
			seen = sw.Provenance
			if folds == 1 {
				sw.Fold(foldCovering(sw, "a1", summary, rowSources("older", "answer")...))
			}
			return "", nil
		},
	}
	cfg.contextSwitch = newContextSwitchState(cfg)
	cfg.contextSwitch.committed(step, []MessageProvenance{{Sources: rowSources("s0")}})
	background := backgroundSummaryMessage("task running")
	params := sdk.Request{Messages: append(append(append([]sdk.Message{system}, compiled...), step...), background)}
	if switched, err := cfg.contextSwitch.fold(context.Background(), cfg, &params, 1, 600); err != nil || !switched {
		t.Fatalf("first fold switched=%v err=%v", switched, err)
	}

	// The next boundary strips the background message, appends the next
	// step and the message again, and the request still does not fit.
	messages := removeBackgroundSummaryMessages(params.Messages, 1)
	next := sdk.AssistantMessage(long("step two"))
	cfg.contextSwitch.committed([]sdk.Message{next}, []MessageProvenance{{Sources: rowSources("s1")}})
	params.Messages = append(append(messages, next), background)
	if _, err := cfg.contextSwitch.fold(context.Background(), cfg, &params, 1, 400); err != nil || folds != 2 {
		t.Fatalf("second fold folds=%d err=%v", folds, err)
	}
	want := [][]turn.ContextMessageSource{nil, {summarySource("a1")}, rowSources("input"), rowSources("s0"), rowSources("s1"), nil}
	if len(seen) != len(want) {
		t.Fatalf("second event saw provenance for %d messages, want %d", len(seen), len(want))
	}
	for i, w := range want {
		var got []turn.ContextMessageSource
		if seen[i] != nil {
			got = seen[i].Sources
		}
		if !reflect.DeepEqual(got, w) {
			t.Fatalf("message %d renders %v, want %v", i, got, w)
		}
	}
}
