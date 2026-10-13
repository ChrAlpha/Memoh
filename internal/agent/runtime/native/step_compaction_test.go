package native

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/felinics/memoh/internal/agent/background"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/step"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/models"
)

// compactionRun drives a run of four model calls (three tool steps, then an
// answer) and records what the compactor was asked and what each call sent.
type compactionRun struct {
	mu       sync.Mutex
	sent     [][]sdk.Message
	request  sdk.Request
	inputs   []StepContextInput
	replaced bool
}

func (r *compactionRun) config(t *testing.T, a *Agent, replace func(call int) []sdk.Message) RunConfig {
	t.Helper()
	provider := &atomicMockProvider{
		handler: func(call int, params sdk.Request) (sdk.ModelResult, error) {
			r.mu.Lock()
			r.sent = append(r.sent, append([]sdk.Message(nil), params.Messages...))
			r.request = params
			r.mu.Unlock()
			if call == 4 {
				return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
			}
			return sdk.ModelResult{
				FinishReason: sdk.FinishReasonToolCalls,
				ToolCalls: []sdk.ToolCall{{
					ToolCallID: fmt.Sprintf("call-%d", call),
					ToolName:   "lookup",
					Input:      toolexec.ArgumentsFromValue(map[string]any{"q": call}),
				}},
			}, nil
		},
	}
	a.SetToolProviders([]agenttools.ToolProvider{staticToolProvider{tools: []toolexec.Tool{{
		Name:       "lookup",
		Parameters: &jsonschema.Schema{Type: "object"},
		Execute: func(_ *toolexec.ToolExecContext, _ sdk.ToolArguments) (sdk.ToolOutput, error) {
			return toolexec.OutputFromValue(map[string]any{"answer": strings.Repeat("tool-result ", 64)}), nil
		},
	}}}})
	return RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: provider},
		Messages:         []sdk.Message{sdk.UserMessage("start")},
		SupportsToolCall: true,
		Identity:         SessionContext{BotID: "bot-1"},
		StepContextCompactor: func(_ context.Context, in StepContextInput) []sdk.Message {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.inputs = append(r.inputs, in)
			if replace == nil {
				return nil
			}
			out := replace(len(r.inputs))
			r.replaced = r.replaced || out != nil
			return out
		},
	}
}

func drainStream(a *Agent, cfg RunConfig) {
	for range a.Stream(context.Background(), cfg) { //nolint:revive // the events are not under test
	}
}

func runCompactionLoops(t *testing.T, run func(a *Agent, cfg RunConfig)) {
	t.Helper()
	summary := sdk.UserMessage("<summary>earlier steps</summary>")
	t.Run("a replacement carries the next call and the steps after it", func(t *testing.T) {
		t.Parallel()
		var r compactionRun
		a := New(Deps{})
		cfg := r.config(t, a, func(call int) []sdk.Message {
			if call == 3 {
				return []sdk.Message{summary, sdk.UserMessage("start")}
			}
			return nil
		})
		run(a, cfg)
		if len(r.sent) != 4 {
			t.Fatalf("provider calls = %d, want 4", len(r.sent))
		}
		if len(r.inputs) != 4 {
			t.Fatalf("compactor asked %d times, want once per call", len(r.inputs))
		}
		if len(r.inputs[0].Messages) != 1 || len(r.inputs[1].Messages) != 3 || len(r.inputs[2].Messages) != 5 {
			t.Fatalf("compactor saw %d, %d and %d messages, want the conversation as the loop holds it (1, 3, 5)",
				len(r.inputs[0].Messages), len(r.inputs[1].Messages), len(r.inputs[2].Messages))
		}
		if len(r.sent[1]) != 3 {
			t.Fatalf("call 2 sent %d messages, want start, call, result", len(r.sent[1]))
		}
		if want := []sdk.Message{summary, sdk.UserMessage("start")}; !reflect.DeepEqual(r.sent[2], want) {
			t.Fatalf("call 3 sent %#v, want the replacement as it is", r.sent[2])
		}
		if len(r.sent[3]) != 4 || !reflect.DeepEqual(r.sent[3][:2], r.sent[2]) {
			t.Fatalf("call 4 sent %d messages, want the replacement plus the step after it", len(r.sent[3]))
		}
	})
	t.Run("the first call is offered with the whole request", func(t *testing.T) {
		t.Parallel()
		var r compactionRun
		a := New(Deps{})
		cfg := r.config(t, a, func(call int) []sdk.Message {
			if call == 1 {
				return []sdk.Message{summary, sdk.UserMessage("start")}
			}
			return nil
		})
		run(a, cfg)
		if len(r.inputs) == 0 || len(r.inputs[0].Messages) != 1 || r.inputs[0].FixedTokens == 0 {
			t.Fatalf("first offer = %+v, want the compiled conversation and its tools", r.inputs)
		}
		if want := []sdk.Message{summary, sdk.UserMessage("start")}; len(r.sent) == 0 || !reflect.DeepEqual(r.sent[0], want) {
			t.Fatalf("call 1 sent %#v, want the replacement", r.sent)
		}
		if len(r.sent) < 2 || len(r.sent[1]) != 4 || !reflect.DeepEqual(r.sent[1][:2], r.sent[0]) {
			t.Fatalf("call 2 sent %#v, want the replacement plus the step after it", r.sent)
		}
	})
	t.Run("a system prompt moved into the messages is not the compactor's", func(t *testing.T) {
		t.Parallel()
		var r compactionRun
		a := New(Deps{})
		cfg := r.config(t, a, func(call int) []sdk.Message {
			if call == 3 {
				return []sdk.Message{summary, sdk.UserMessage("start")}
			}
			return nil
		})
		cfg.Model = &sdk.Model{ID: "claude-test", Provider: anthropicNamedProvider{cfg.Model.Provider.(*atomicMockProvider)}}
		cfg.System = "base system"
		cfg.PromptCacheTTL = models.PromptCacheTTL5m
		run(a, cfg)
		if len(r.sent) != 4 || len(r.sent[1]) != 4 || r.sent[1][0].Role != sdk.MessageRoleSystem {
			t.Fatalf("call 2 = %#v, want the system prompt as the first message", r.sent)
		}
		if got := r.inputs[1].Messages; len(got) != 3 || got[0].Role == sdk.MessageRoleSystem {
			t.Fatalf("compactor saw %#v, want the conversation without the system prompt", got)
		}
		if want := contextfrag.ProviderEnvelopeTokens(r.request.System, r.sent[1][:1], r.request.Tools); r.inputs[1].FixedTokens != want {
			t.Fatalf("fixed tokens = %d, want %d: the moved system prompt and the tools", r.inputs[1].FixedTokens, want)
		}
		if want := append([]sdk.Message{r.sent[1][0]}, summary, sdk.UserMessage("start")); !reflect.DeepEqual(r.sent[2], want) {
			t.Fatalf("call 3 sent %#v, want the system prompt ahead of the replacement", r.sent[2])
		}
	})
	t.Run("a replacement is prefix the background summary refresh keeps", func(t *testing.T) {
		t.Parallel()
		bgMgr := background.New(nil)
		sessionID := "sess-" + strings.ReplaceAll(t.Name(), "/", "-")
		_, release := spawnBlockedBackgroundTask(t, bgMgr, "bot-1", sessionID, "long build")
		t.Cleanup(func() { close(release) })
		carried := backgroundSummaryMessage("carried over by the compactor")
		var r compactionRun
		a := New(Deps{})
		cfg := r.config(t, a, func(call int) []sdk.Message {
			if call == 3 {
				return []sdk.Message{summary, carried, sdk.UserMessage("start")}
			}
			return nil
		})
		cfg.Identity.SessionID = sessionID
		cfg.BackgroundManager = bgMgr
		run(a, cfg)
		if len(r.sent) != 4 {
			t.Fatalf("provider calls = %d, want 4", len(r.sent))
		}
		for call := 3; call <= 4; call++ {
			sent := r.sent[call-1]
			if len(sent) < 4 || !reflect.DeepEqual(sent[:3], []sdk.Message{summary, carried, sdk.UserMessage("start")}) {
				t.Fatalf("call %d sent %#v, want the replacement kept whole as the prefix", call, sent)
			}
			fresh := 0
			for _, message := range sent[3:] {
				if contextfrag.IsBackgroundSummaryCarrier(message) {
					fresh++
				}
			}
			if fresh != 1 || !contextfrag.IsBackgroundSummaryCarrier(sent[len(sent)-1]) {
				t.Fatalf("call %d carries %d summaries after the prefix, want one fresh summary last: %#v", call, fresh, sent)
			}
		}
	})
	t.Run("a compactor that keeps the conversation leaves it untouched", func(t *testing.T) {
		t.Parallel()
		var kept, untouched compactionRun
		for _, r := range []*compactionRun{&kept, &untouched} {
			a := New(Deps{})
			cfg := r.config(t, a, nil)
			if r == &untouched {
				cfg.StepContextCompactor = nil
			}
			run(a, cfg)
		}
		if !reflect.DeepEqual(kept.sent, untouched.sent) {
			t.Fatalf("messages differ with a compactor that declines:\n%#v\n%#v", kept.sent, untouched.sent)
		}
	})
}

func TestGenerateStepContextCompactor(t *testing.T) {
	t.Parallel()
	runCompactionLoops(t, func(a *Agent, cfg RunConfig) {
		if _, err := a.Generate(context.Background(), cfg); err != nil {
			t.Fatalf("Generate() error = %v", err)
		}
	})
}

func TestStreamStepContextCompactor(t *testing.T) {
	t.Parallel()
	runCompactionLoops(t, func(a *Agent, cfg RunConfig) { drainStream(a, cfg) })
}

// A spawned run compacts its thread only when its steps are committed as they
// complete; the compactor is built from the run's own identity.
func TestSpawnAdapterInstallsACompactorOnlyForCommittedRuns(t *testing.T) {
	t.Parallel()

	commit := func(context.Context, int, *step.Record) error { return nil }
	cfg := agenttools.SpawnRunConfig{
		ModelUUID:            "model-1",
		TurnRequestMessageID: "task-1",
		Identity:             agenttools.SpawnIdentity{BotID: "bot-1", SessionID: "sub-1", UserID: "user-1"},
	}
	for _, tc := range []struct {
		name      string
		committed bool
	}{{"committed steps", true}, {"terminal snapshot only", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			adapter := NewSpawnAdapter(newTestAgent())
			adapter.SetStepCommitFactory(func(context.Context, string, string, string, string, *contextfrag.LifecycleHolder, func()) (
				func(context.Context, int, *step.Record) error,
				func(context.Context, int, *step.Record) error,
			) {
				if !tc.committed {
					return nil, nil
				}
				return commit, commit
			})
			var got SpawnCompaction
			adapter.SetCompactorFactory(func(_ context.Context, run SpawnCompaction) StepContextCompactor {
				got = run
				return func(context.Context, StepContextInput) []sdk.Message { return nil }
			})
			rc := runConfigFromSpawnRunConfig(cfg)
			adapter.installStepCommit(context.Background(), cfg, &rc)

			if installed := rc.StepContextCompactor != nil; installed != tc.committed {
				t.Fatalf("compactor installed = %v, want %v", installed, tc.committed)
			}
			if tc.committed && (got.BotID != "bot-1" || got.SessionID != "sub-1" || got.UserID != "user-1" ||
				got.ModelUUID != "model-1" || got.TurnRequestMessageID != "task-1") {
				t.Fatalf("compaction run = %+v, want the spawned run's identity", got)
			}
		})
	}
}

// A mid-stream retry resends the failed attempt's own request, which the
// compactor was offered before it went out; it is not offered again.
func TestStreamRetryDoesNotOfferTheResentRequestAgain(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	provider := &atomicMockProvider{
		stream: func(_ context.Context, _ sdk.Request) (<-chan sdk.StreamPart, error) {
			call := calls.Add(1)
			ch := make(chan sdk.StreamPart, 8)
			go func() {
				defer close(ch)
				ch <- &sdk.StartPart{}
				ch <- &sdk.StartStepPart{}
				if call == 1 {
					ch <- &sdk.ErrorPart{Error: serverErr()}
					return
				}
				ch <- &sdk.TextStartPart{ID: "retried"}
				ch <- &sdk.TextDeltaPart{ID: "retried", Text: "ok"}
				ch <- &sdk.TextEndPart{ID: "retried"}
				ch <- &sdk.FinishStepPart{FinishReason: sdk.FinishReasonStop}
				ch <- &sdk.FinishPart{FinishReason: sdk.FinishReasonStop}
			}()
			return ch, nil
		},
	}
	var offers atomic.Int32
	drainStream(New(Deps{}), RunConfig{
		Model:    &sdk.Model{ID: "mock-model", Provider: provider},
		Messages: []sdk.Message{sdk.UserMessage("start")},
		Identity: SessionContext{BotID: "bot-1"},
		StepContextCompactor: func(context.Context, StepContextInput) []sdk.Message {
			offers.Add(1)
			return nil
		},
	})
	if calls.Load() != 2 || offers.Load() != 1 {
		t.Fatalf("provider calls = %d, compactor offers = %d; want the first call offered and its retry not", calls.Load(), offers.Load())
	}
}
