package compaction

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/models"
)

func TestFoldShrinksRejectedSummaryAndCommitsOnlyAcceptedAnswer(t *testing.T) {
	s := newFoldSession(t)
	r := s.request()
	r.messages[3] = resultMessage("call_1", strings.Repeat("parser.go:12 unsafe cast of int64 to int32\n", 12000))
	original := r.messages[3]
	ev := s.event(r, &scriptedSummarizer{})
	ev.Window, ev.Allowance, ev.MaxOutputTokens = 200000, 180000, 8000
	ev.FixedOutput = true
	ev.Request.MaxTokens = &ev.MaxOutputTokens
	calls, rejectedTokens := 0, 0
	ev.Summarizer = func(_ context.Context, req sdk.Request) (sdk.ModelResult, error) {
		calls++
		if calls == 1 {
			rejectedTokens = forkTokens(req)
			if rejectedTokens+8000 > 200000 {
				t.Fatal("first fork must pass local admission")
			}
			return sdk.ModelResult{}, &sdk.APIError{StatusCode: 400, Code: "context_length_exceeded"}
		}
		if forkTokens(req) >= rejectedTokens {
			t.Fatal("summary retry did not shrink")
		}
		if req.MaxTokens == nil || *req.MaxTokens != 8000 {
			t.Fatal("recovery changed the run's output settings")
		}
		return summaryAnswer("The parser audit found an unsafe int64 cast at parser.go:12. Fix and verify it."), nil
	}
	passes, err := s.svc.Fold(context.Background(), ev)
	if err != nil || len(passes) == 0 || calls < 2 {
		t.Fatalf("passes=%d calls=%d err=%v", len(passes), calls, err)
	}
	if !messagesEqual(original, r.messages[3]) {
		t.Fatal("recovery mutated the original tool result")
	}
}

// Exercise the runtime's measured admission with the real fold planner. An
// always-folding callback would hide the planner's summary-only skip.
func TestMeasuredUsageRewritesLocallyFittingSummary(t *testing.T) {
	s := newFoldSession(t)
	earlier := strings.Repeat("Earlier evidence. ", 6000)
	held := s.q.hold(earlier)
	summary := SummaryMessage(earlier)
	input := sdk.UserMessage("Continue this task")
	verifyMeasuredFold(t, s, []sdk.Message{summary, input}, []native.MessageProvenance{
		{Message: summary, Sources: []turn.ContextMessageSource{{Kind: "summary", ID: summaryIDs(held)[0]}}},
		{Message: input, Sources: []turn.ContextMessageSource{s.row()}, Anchor: true},
	}, "", 218601, nil)
}

func TestMeasuredUsageKeepsItsPrefixWhenFoldingAppendedResults(t *testing.T) {
	s := newFoldSession(t)
	input := sdk.UserMessage("Continue this task")
	provenance := []native.MessageProvenance{{Message: input, Sources: []turn.ContextMessageSource{s.row()}, Anchor: true}}
	appended := []sdk.Message{callMessage("a"), resultMessage("a", strings.Repeat("a", 40000)), callMessage("b"), resultMessage("b", strings.Repeat("b", 40000))}
	for _, message := range appended {
		provenance = append(provenance, native.MessageProvenance{Message: message, Sources: []turn.ContextMessageSource{s.row()}})
	}
	verifyMeasuredFold(t, s, []sdk.Message{input}, provenance, strings.Repeat("fixed system ", 9000), 170000, appended)
}

// A measured empty completion and a large protected current input exposed a
// double conversion from calibrated allowance to the planner's raw estimate.
// Exercise the real planner: a callback that blindly folds cannot catch it.
func TestEmptyContextRecoveryKeepsRoomForProtectedInput(t *testing.T) {
	s := newFoldSession(t)
	history := sdk.UserMessage(strings.Repeat("history ", 100000))
	input := sdk.UserMessage(strings.Repeat("current ", 60000))
	calls, summaries, firstBytes := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			firstBytes = len(body)
			_, _ = fmt.Fprint(w, `{"id":"exhausted","model":"usage-model","choices":[{"message":{"role":"assistant","content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":1048570,"completion_tokens":0,"total_tokens":1048570}}`)
			return
		}
		if len(body) >= firstBytes || strings.Count(string(body), "current ") != 60000 {
			t.Error("retry must shrink history while preserving the entire current input")
		}
		_, _ = fmt.Fprint(w, `{"id":"recovered","model":"usage-model","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101}}`)
	}))
	defer server.Close()
	toSources := func(provenance []*native.MessageProvenance) []Source {
		sources := make([]Source, len(provenance))
		for i, p := range provenance {
			if p != nil {
				sources[i] = Source{Sources: p.Sources, Anchor: p.Anchor}
			}
		}
		return sources
	}
	cfg := native.RunConfig{
		Model:    models.NewSDKChatModel(models.SDKModelConfig{ModelID: "usage-model", ClientType: "openai-completions", BaseURL: server.URL, HTTPClient: server.Client()}),
		Messages: []sdk.Message{history, input}, ContextBudgetMaxTokens: 1000000, ModelMaxOutputTokens: 8192,
		Identity: native.SessionContext{BotID: s.botID, SessionID: s.session},
		ContextProvenance: []native.MessageProvenance{
			{Message: history, Sources: []turn.ContextMessageSource{s.row()}},
			{Message: input, Sources: []turn.ContextMessageSource{s.row()}, Anchor: true},
		},
		SwitchContext: func(ctx context.Context, sw native.ContextSwitch) (string, error) {
			tokens := sw.UsageTokens
			_, err := s.svc.Fold(ctx, Event{
				BotID: s.botID, SessionID: s.session, Request: sw.Request, Sources: toSources(sw.Provenance),
				Window: sw.WindowTokens, MaxOutputTokens: sw.OutputTokens, Allowance: sw.AllowanceTokens, Pressure: sw.UsageTokens, FixedOutput: true,
				Summarizer: func(context.Context, sdk.Request) (sdk.ModelResult, error) {
					summaries++
					return summaryAnswer("History condensed."), nil
				},
				Apply: func(pass Pass) (sdk.Request, []Source) {
					next := sw.Fold(native.ContextFold{SummaryID: pass.SummaryID, Summary: SummaryMessage(pass.Summary), Order: pass.Order})
					tokens = next.Tokens
					return next.Request, toSources(next.Provenance)
				},
			})
			if err == nil && tokens > sw.AllowanceTokens {
				return "", fmt.Errorf("%w: splice exceeds planner allowance", contextfrag.ErrBudgetUnsatisfied)
			}
			return "", err
		},
	}
	result, err := native.New(native.Deps{}).Generate(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "done" || calls != 2 || summaries == 0 {
		t.Fatalf("answer=%q calls=%d summaries=%d", result.Text, calls, summaries)
	}
}

func verifyMeasuredFold(t *testing.T, s *foldSession, messages []sdk.Message, provenance []native.MessageProvenance, system string, actual int, appended []sdk.Message) {
	t.Helper()
	calls, summaries := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"response","model":"usage-model","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":1,"total_tokens":%d}}`, actual, actual+1)
	}))
	defer server.Close()
	model := models.NewSDKChatModel(models.SDKModelConfig{ModelID: "usage-model", ClientType: "openai-completions", BaseURL: server.URL, HTTPClient: server.Client()})
	cfg := native.RunConfig{
		Model: model, System: system, Messages: messages, ContextBudgetMaxTokens: 200000, ModelMaxOutputTokens: 8000,
		Identity:          native.SessionContext{BotID: s.botID, SessionID: s.session},
		ContextProvenance: provenance,
		SwitchContext: func(ctx context.Context, sw native.ContextSwitch) (string, error) {
			tokens := sw.UsageTokens
			sources := make([]Source, len(sw.Provenance))
			for i, p := range sw.Provenance {
				if p != nil {
					sources[i] = Source{Sources: p.Sources, Anchor: p.Anchor}
				}
			}
			_, err := s.svc.Fold(ctx, Event{
				BotID: s.botID, SessionID: s.session, Request: sw.Request, Sources: sources,
				Window: sw.WindowTokens, MaxOutputTokens: sw.OutputTokens, Allowance: sw.AllowanceTokens, FixedOutput: true,
				Summarizer: func(context.Context, sdk.Request) (sdk.ModelResult, error) {
					summaries++
					return summaryAnswer("Earlier evidence condensed."), nil
				},
				Apply: func(pass Pass) (sdk.Request, []Source) {
					next := sw.Fold(native.ContextFold{SummaryID: pass.SummaryID, Summary: SummaryMessage(pass.Summary), Order: pass.Order})
					tokens = next.Tokens
					out := make([]Source, len(next.Provenance))
					for i, p := range next.Provenance {
						if p != nil {
							out[i] = Source{Sources: p.Sources, Anchor: p.Anchor}
						}
					}
					return next.Request, out
				},
			})
			if err == nil && tokens > sw.AllowanceTokens {
				return "", fmt.Errorf("%w: splice tokens=%d planner allowance=%d", contextfrag.ErrBudgetUnsatisfied, tokens, sw.AllowanceTokens)
			}
			return "", err
		},
	}
	agent := native.New(native.Deps{})
	if _, err := agent.Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Messages = append(cfg.Messages, sdk.AssistantMessage("done"))
	cfg.Messages = append(cfg.Messages, appended...)
	cfg.Messages = append(cfg.Messages, sdk.UserMessage("Continue"))
	if _, err := agent.Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if summaries != 1 || calls != 2 {
		t.Fatalf("summaries=%d calls=%d", summaries, calls)
	}
}

func TestFoldStopsRepeatedSummaryOverflowWithoutCommitting(t *testing.T) {
	s := newFoldSession(t)
	r := s.request()
	r.messages[3] = resultMessage("call_1", strings.Repeat("parser.go:12 unsafe cast of int64 to int32\n", 12000))
	ev := s.event(r, &scriptedSummarizer{})
	ev.Window, ev.Allowance, ev.MaxOutputTokens = 200000, 180000, 8000
	calls, lastInput := 0, 0
	ev.Summarizer = func(_ context.Context, req sdk.Request) (sdk.ModelResult, error) {
		calls++
		input := forkTokens(req)
		if lastInput > 0 && input >= lastInput {
			t.Fatal("repeated the same rejected summary")
		}
		lastInput = input
		return sdk.ModelResult{}, &sdk.APIError{Code: "context_length_exceeded"}
	}
	ev.Apply = func(Pass) (sdk.Request, []Source) { t.Fatal("committed a rejected summary"); return sdk.Request{}, nil }
	passes, err := s.svc.Fold(context.Background(), ev)
	if err == nil || len(passes) != 0 || calls < 2 || calls > 4 {
		t.Fatalf("passes=%d calls=%d err=%v", len(passes), calls, err)
	}
}
