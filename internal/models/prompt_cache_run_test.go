package models

import (
	"fmt"
	"slices"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

const anthropicBreakpointLimit = 4

func messageBreakpoints(messages []sdk.Message) []int {
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

func toolBreakpoints(tools []toolexec.Tool) int {
	count := 0
	for _, tool := range tools {
		if tool.CacheControl != nil {
			count++
		}
	}
	return count
}

func reasoningMessage(text string) sdk.Message {
	return sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ReasoningPart{Text: text}}}
}

// toolStep is what one step of a run appends: an assistant message with calls
// tool calls and the tool message answering them, 2*calls content blocks.
func toolStep(step, calls int) []sdk.Message {
	assistant := sdk.Message{Role: sdk.MessageRoleAssistant}
	var results []sdk.ToolResultPart
	for i := range calls {
		id := fmt.Sprintf("call-%d-%d", step, i)
		assistant.Content = append(assistant.Content, sdk.ToolCallPart{ToolCallID: id, ToolName: "read", Input: toolexec.ArgumentsFromValue(map[string]any{"n": i})})
		results = append(results, sdk.ToolResultPart{ToolCallID: id, ToolName: "read", Result: toolexec.OutputFromValue("ok")})
	}
	return []sdk.Message{assistant, sdk.ToolMessage(results...)}
}

// runTestLayout is a dispatch over four history messages (stable, with a
// mid-span breakpoint after the second) and the run's input.
func runTestLayout(model *sdk.Model, ttl, system string) PromptCacheLayout {
	messages := []sdk.Message{
		sdk.UserMessage("history one"),
		sdk.AssistantMessage("history two"),
		sdk.UserMessage("history three"),
		sdk.AssistantMessage("history four"),
		sdk.UserMessage("run input"),
	}
	plan := contextfrag.CachePlan{StableMessageCount: 4, MidStableMessageCount: 2, StablePrefixTokenEstimate: 5000}
	return ApplyPromptCacheWithPlan(model, ttl, plan, system, messages, []toolexec.Tool{{Name: "read"}})
}

func TestAdvanceRunRollsBreakpointAcrossSteps(t *testing.T) {
	t.Parallel()

	layout := runTestLayout(anthropicTestModel(), "5m", "system")
	first := layout.AdvanceRun(layout.Messages, len(layout.Messages))
	// The promoted system prompt, the mid-span and stable history
	// breakpoints, and the run's input.
	if got, want := messageBreakpoints(first), []int{0, 2, 4, 5}; !slices.Equal(got, want) {
		t.Fatalf("first call breakpoints = %v, want %v", got, want)
	}
	if toolBreakpoints(layout.Tools) != 0 {
		t.Fatal("the system breakpoint covers the tools; they must not carry their own")
	}

	// A step of twelve parallel calls appends 24 blocks, more than
	// Anthropic's 20-block lookback: the previous call's breakpoint stays so
	// the read names its entry exactly, and the mid-span one yields its slot.
	second := layout.AdvanceRun(append(slices.Clone(first), toolStep(1, 12)...), len(first)+2)
	if got, want := messageBreakpoints(second), []int{0, 4, 5, 7}; !slices.Equal(got, want) {
		t.Fatalf("second call breakpoints = %v, want %v", got, want)
	}
	results := second[7].Content
	if last, ok := results[len(results)-1].(sdk.ToolResultPart); !ok || last.CacheControl == nil {
		t.Fatalf("rolling breakpoint should sit on the step's last tool result: %#v", results[len(results)-1])
	}
	if partCacheControl(results[0]) != nil {
		t.Fatal("only the last block of the step carries the breakpoint")
	}

	third := layout.AdvanceRun(append(slices.Clone(second), toolStep(2, 1)...), len(second)+2)
	if got, want := messageBreakpoints(third), []int{0, 4, 7, 9}; !slices.Equal(got, want) {
		t.Fatalf("third call breakpoints = %v, want %v", got, want)
	}
	if got, want := messageBreakpoints(second), []int{0, 4, 5, 7}; !slices.Equal(got, want) {
		t.Fatalf("advancing mutated the previous request: breakpoints = %v, want %v", got, want)
	}
}

func TestAdvanceRunNeverExceedsBreakpointLimit(t *testing.T) {
	t.Parallel()

	for _, system := range []string{"system", ""} {
		layout := runTestLayout(anthropicTestModel(), "5m", system)
		messages := layout.AdvanceRun(layout.Messages, len(layout.Messages))
		for step := 1; step <= 8; step++ {
			messages = layout.AdvanceRun(append(messages, toolStep(step, step*3)...), len(messages)+2)
			breakpoints := messageBreakpoints(messages)
			if total := len(breakpoints) + toolBreakpoints(layout.Tools); total != anthropicBreakpointLimit {
				t.Fatalf("system=%q step %d: %d breakpoints %v, want %d", system, step, total, breakpoints, anthropicBreakpointLimit)
			}
			if breakpoints[len(breakpoints)-1] != len(messages)-1 {
				t.Fatalf("system=%q step %d: rolling breakpoint at %v, want the last message %d", system, step, breakpoints, len(messages)-1)
			}
		}
	}
}

func TestAdvanceRunWithoutSystemKeepsToolBreakpoint(t *testing.T) {
	t.Parallel()

	layout := runTestLayout(anthropicTestModel(), "5m", "")
	if toolBreakpoints(layout.Tools) != 1 {
		t.Fatal("without a system prompt the last tool carries the head breakpoint")
	}
	first := layout.AdvanceRun(layout.Messages, len(layout.Messages))
	if got, want := messageBreakpoints(first), []int{1, 3, 4}; !slices.Equal(got, want) {
		t.Fatalf("first call breakpoints = %v, want %v", got, want)
	}
	second := layout.AdvanceRun(append(slices.Clone(first), toolStep(1, 2)...), len(first)+2)
	if got, want := messageBreakpoints(second), []int{3, 4, 6}; !slices.Equal(got, want) {
		t.Fatalf("second call breakpoints = %v, want %v", got, want)
	}
}

func TestAdvanceRunLeavesPerCallCarriersUncovered(t *testing.T) {
	t.Parallel()

	layout := runTestLayout(anthropicTestModel(), "5m", "system")
	messages := append(slices.Clone(layout.Messages), sdk.UserMessage("[Background tasks]\nnone"))

	got := layout.AdvanceRun(messages, len(messages)-1)

	if breakpoints := messageBreakpoints(got); breakpoints[len(breakpoints)-1] != 5 {
		t.Fatalf("breakpoints = %v, want the rolling one on the input before the carrier", breakpoints)
	}
}

func TestAdvanceRunStaysOutOfThePlanPrefix(t *testing.T) {
	t.Parallel()

	layout := runTestLayout(anthropicTestModel(), "5m", "system")
	messages := append(slices.Clone(layout.Messages[:5]), reasoningMessage("thinking"))

	got := layout.AdvanceRun(messages, len(messages))

	if breakpoints, want := messageBreakpoints(got), []int{0, 2, 4}; !slices.Equal(breakpoints, want) {
		t.Fatalf("breakpoints = %v, want the plan's own %v when nothing after it can carry one", breakpoints, want)
	}
}

func TestAdvanceRunLeavesUncachedRequestsUnchanged(t *testing.T) {
	t.Parallel()

	for name, layout := range map[string]PromptCacheLayout{
		"ttl off":       runTestLayout(anthropicTestModel(), PromptCacheTTLOff, "system"),
		"openai family": runTestLayout(newOpenAITestModel(t), "5m", "system"),
	} {
		messages := append(slices.Clone(layout.Messages), toolStep(1, 2)...)
		got := layout.AdvanceRun(messages, len(messages))
		if breakpoints := messageBreakpoints(got); len(breakpoints) != 0 {
			t.Fatalf("%s: breakpoints = %v, want none", name, breakpoints)
		}
	}
}

func TestClearCacheControlsClearsEveryPartKind(t *testing.T) {
	t.Parallel()

	cc := &sdk.CacheControl{Type: "ephemeral"}
	messages := []sdk.Message{
		{Role: sdk.MessageRoleUser, Content: []sdk.MessagePart{
			sdk.TextPart{Text: "a", CacheControl: cc},
			sdk.ImagePart{Image: "data:image/png;base64,AA==", CacheControl: cc},
			sdk.FilePart{Data: "AA==", CacheControl: cc},
		}},
		{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{ToolCallID: "c", ToolName: "read", CacheControl: cc}}},
		sdk.ToolMessage(sdk.ToolResultPart{ToolCallID: "c", ToolName: "read", CacheControl: cc}),
	}

	ClearCacheControls(messages)

	if breakpoints := messageBreakpoints(messages); len(breakpoints) != 0 {
		t.Fatalf("breakpoints left at %v", breakpoints)
	}
}

// TestApplyPromptCacheClearsInheritedBreakpoints covers a forked run whose
// messages carry its parent's breakpoints, rolling ones included: the layout owns every
// breakpoint of the request, so they are cleared before it places its own.
func TestApplyPromptCacheClearsInheritedBreakpoints(t *testing.T) {
	t.Parallel()

	cc := &sdk.CacheControl{Type: "ephemeral"}
	parent := runTestLayout(anthropicTestModel(), "5m", "system")
	inherited := slices.Clone(parent.Messages[1:5])
	for _, step := range [][]sdk.Message{toolStep(1, 2), toolStep(2, 2)} {
		results := slices.Clone(step[1].Content)
		last := results[len(results)-1].(sdk.ToolResultPart)
		last.CacheControl = cc
		results[len(results)-1] = last
		step[1].Content = results
		inherited = append(inherited, step...)
	}
	inherited = append(inherited, sdk.UserMessage("forked task"))
	plan := contextfrag.CachePlan{StableMessageCount: len(inherited) - 1, MidStableMessageCount: 2, StablePrefixTokenEstimate: 5000}

	before := messageBreakpoints(inherited)

	layout := ApplyPromptCacheWithPlan(anthropicTestModel(), "5m", plan, "system", inherited, nil)
	got := layout.AdvanceRun(layout.Messages, len(layout.Messages))

	if breakpoints, want := messageBreakpoints(got), []int{0, 2, len(got) - 2, len(got) - 1}; !slices.Equal(breakpoints, want) {
		t.Fatalf("breakpoints = %v, want %v", breakpoints, want)
	}
	if after := messageBreakpoints(inherited); !slices.Equal(after, before) {
		t.Fatalf("the input was mutated: breakpoints %v, were %v", after, before)
	}
}
