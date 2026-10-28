package native

import (
	"context"
	"fmt"
	"slices"
	"testing"

	anthropicmessages "github.com/felinics/twilight/provider/anthropic/messages"
	sdk "github.com/felinics/twilight/sdk"

	agenttools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/models"
)

// toolBatchProvider answers each call with the next batch of parallel tool
// calls, then a final answer, recording every request.
type toolBatchProvider struct {
	recordingPromptCacheProvider
	batches []int
}

func (p *toolBatchProvider) DoGenerate(_ context.Context, params sdk.Request) (sdk.ModelResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.params = append(p.params, cloneGenerateParams(params))
	if p.calls > len(p.batches) {
		return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
	}
	var calls []sdk.ToolCall
	for i := range p.batches[p.calls-1] {
		calls = append(calls, sdk.ToolCall{
			ToolCallID: fmt.Sprintf("call-%d-%d", p.calls, i),
			ToolName:   "noop",
			Input:      toolexec.ArgumentsFromValue(map[string]any{"i": i}),
		})
	}
	return sdk.ModelResult{FinishReason: sdk.FinishReasonToolCalls, ToolCalls: calls}, nil
}

func sentBreakpoints(messages []sdk.Message) []int {
	var out []int
	for i, message := range messages {
		for _, part := range message.Content {
			if partCacheControl(part) != nil {
				out = append(out, i)
			}
		}
	}
	return out
}

func partCacheControl(part sdk.MessagePart) *sdk.CacheControl {
	switch p := part.(type) {
	case sdk.TextPart:
		return p.CacheControl
	case sdk.ImagePart:
		return p.CacheControl
	case sdk.FilePart:
		return p.CacheControl
	case sdk.ToolCallPart:
		return p.CacheControl
	case sdk.ToolResultPart:
		return p.CacheControl
	default:
		return nil
	}
}

// TestAgentGenerateRollsAnthropicCacheBreakpointAcrossSteps pins what a run
// sends Anthropic: every call ends in a breakpoint, so the step it appended is
// written to the cache, and keeps the previous call's one, so the read names
// that entry exactly even when a step appends more blocks than the 20-block
// lookback reaches.
func TestAgentGenerateRollsAnthropicCacheBreakpointAcrossSteps(t *testing.T) {
	t.Parallel()

	provider := &toolBatchProvider{batches: []int{12, 1}}
	model := anthropicmessages.New(anthropicmessages.WithAPIKey("test")).ChatModel("claude-test")
	model.Provider = provider
	cfg := RunConfig{
		Model:            model,
		Messages:         []sdk.Message{sdk.UserMessage("run")},
		System:           "base system",
		PromptCacheTTL:   models.PromptCacheTTL5m,
		SupportsToolCall: true,
		Identity:         SessionContext{BotID: "bot-1", SessionID: "session-1"},
	}
	a := New(Deps{})
	a.SetToolProviders([]agenttools.ToolProvider{staticToolProvider{tools: []toolexec.Tool{{
		Name: "noop",
		Execute: func(*toolexec.ToolExecContext, sdk.ToolArguments) (sdk.ToolOutput, error) {
			return toolexec.OutputFromValue("ok"), nil
		},
	}}}})

	result, err := a.Generate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	params := provider.snapshotParams()
	want := [][]int{
		{0, 1},    // system, run input
		{0, 1, 3}, // + the 24-block step's last tool result
		{0, 3, 5},
	}
	if len(params) != len(want) {
		t.Fatalf("calls = %d, want %d", len(params), len(want))
	}
	for i, p := range params {
		if got := sentBreakpoints(p.Messages); !slices.Equal(got, want[i]) {
			t.Fatalf("call %d breakpoints = %v, want %v", i+1, got, want[i])
		}
		for _, tool := range p.Tools {
			if tool.CacheControl != nil {
				t.Fatalf("call %d: tool %s carries a breakpoint the system one already covers", i+1, tool.Name)
			}
		}
	}
	if got := sentBreakpoints(result.Messages); len(got) != 0 {
		t.Fatalf("result messages carry breakpoints at %v; they are request decoration, not history", got)
	}
}
