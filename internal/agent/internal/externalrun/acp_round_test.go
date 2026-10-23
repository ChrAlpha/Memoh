package externalrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agentapi/runtimeerr"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/outcome"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	acpagent "github.com/felinics/memoh/internal/agent/runtime/acp"
	acpclient "github.com/felinics/memoh/internal/agent/runtime/acp/client"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func TestACPTerminalStreamEventCarriesTranscriptAndUsage(t *testing.T) {
	t.Parallel()

	ev := runtimeTerminalStreamEvent(native.EventEnd, acpagent.DriverPromptResult(withTranscriptOutput(acpclient.PromptResult{
		Usage: &sdk.Usage{InputTokens: 2, OutputTokens: 4, TotalTokens: 6},
	}, event.StreamEvent{Type: event.TextDelta, Delta: "from transcript"}), "codex"))

	if ev.Type != native.EventEnd {
		t.Fatalf("terminal event type = %s, want %s", ev.Type, native.EventEnd)
	}
	if got := terminalAssistantText(t, ev); got != "from transcript" {
		t.Fatalf("terminal assistant text = %q, want from transcript", got)
	}
	var usage sdk.Usage
	if err := json.Unmarshal(ev.Usage, &usage); err != nil {
		t.Fatalf("decode terminal usage: %v", err)
	}
	if usage.InputTokens != 2 || usage.OutputTokens != 4 || usage.TotalTokens != 6 {
		t.Fatalf("terminal usage = %+v, want input=2 output=4 total=6", usage)
	}
}

func TestPersistACPRoundUsesDedicatedSessionMetadata(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := testHost(t, Deps{Messages: messages})

	err := resolver.persistACPRound(
		context.Background(),
		runreq.ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "inspect the project",
		},
		"codex",
		"/data/app",
		withTranscriptOutput(acpclient.PromptResult{
			Text:       "done",
			StopReason: "end_turn",
		}),
		nil,
		true,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("persistACPRound returned error: %v", err)
	}
	if len(messages.Persisted) != 2 {
		t.Fatalf("persisted %d messages, want 2", len(messages.Persisted))
	}

	assistantMeta := messages.Persisted[1].Metadata
	if assistantMeta["acp_agent_id"] != "codex" {
		t.Fatalf("acp_agent_id = %#v, want codex", assistantMeta["acp_agent_id"])
	}
	if assistantMeta["project_path"] != "/data/app" {
		t.Fatalf("project_path = %#v, want /data/app", assistantMeta["project_path"])
	}
	if assistantMeta["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %#v, want end_turn", assistantMeta["stop_reason"])
	}
	if assistantMeta["agent_turn_outcome"] != "succeeded" {
		t.Fatalf("ACP outcome metadata = %#v", assistantMeta)
	}
	if len(messages.RoundOptions) == 0 {
		t.Fatal("round persisted without atomic options")
	}
	publication := messages.RoundOptions[len(messages.RoundOptions)-1].AgentPublication
	// ACP captures no runtime snapshots: every completed turn publishes an
	// explicit reset head so warm-handle fencing still tracks history.
	if publication == nil {
		t.Fatalf("ACP publication = %#v, want reset head", publication)
	}
}

func TestPersistACPRoundStoresACPEventsAsNativeToolMessages(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := testHost(t, Deps{Messages: messages})

	err := resolver.persistACPRound(
		context.Background(),
		runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "inspect"},
		"codex",
		"/data/app",
		withTranscriptOutput(acpclient.PromptResult{StopReason: "end_turn"}, []event.StreamEvent{
			{Type: event.TextDelta, Delta: "Before"},
			{
				Type:       event.ToolCallStart,
				ToolCallID: "read-1",
				ToolName:   "read",
				Input:      map[string]any{"path": "README.md"},
			},
			{
				Type:       event.ToolCallEnd,
				ToolCallID: "read-1",
				ToolName:   "read",
				Result:     map[string]any{"ok": true},
			},
			{Type: event.TextDelta, Delta: "After"},
		}...),
		nil,
		true,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("persistACPRound returned error: %v", err)
	}
	if len(messages.Persisted) != 4 {
		t.Fatalf("persisted %d messages, want user + assistant + tool + assistant", len(messages.Persisted))
	}
	roles := []string{
		messages.Persisted[0].Role,
		messages.Persisted[1].Role,
		messages.Persisted[2].Role,
		messages.Persisted[3].Role,
	}
	if strings.Join(roles, ",") != "user,assistant,tool,assistant" {
		t.Fatalf("persisted roles = %v", roles)
	}

	before := persistedModelMessage(t, messages.Persisted[1].Content)
	if got := before.TextContent(); got != "Before" {
		t.Fatalf("first assistant text = %q, want Before", got)
	}
	calls := runreq.ExtractAssistantToolCallParts(before)
	if len(calls) != 1 || calls[0].ToolCallID != "read-1" || calls[0].ToolName != "read" {
		t.Fatalf("assistant tool calls = %#v, want read-1/read", calls)
	}
	tool := persistedModelMessage(t, messages.Persisted[2].Content)
	results := runreq.ExtractToolResultParts(tool)
	if len(results) != 1 || results[0].ToolCallID != "read-1" || results[0].ToolName != "read" {
		t.Fatalf("tool results = %#v, want read-1/read", results)
	}
	after := persistedModelMessage(t, messages.Persisted[3].Content)
	if got := after.TextContent(); got != "After" {
		t.Fatalf("last assistant text = %q, want After", got)
	}
	if messages.Persisted[1].Metadata["agent_turn_outcome"] != nil {
		t.Fatalf("intermediate assistant unexpectedly claims the turn outcome: %#v", messages.Persisted[1].Metadata)
	}
	if messages.Persisted[3].Metadata["agent_turn_outcome"] != "succeeded" {
		t.Fatalf("final assistant outcome metadata = %#v", messages.Persisted[3].Metadata)
	}
	publication := messages.RoundOptions[len(messages.RoundOptions)-1].AgentPublication
	// ACP captures no runtime snapshots: every completed turn publishes an
	// explicit reset head so warm-handle fencing still tracks history.
	if publication == nil {
		t.Fatalf("ACP publication = %#v, want reset head", publication)
	}
}

func TestPersistACPRoundAttachesLifecycleOnlyToFinalAssistant(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	service := testHost(t, Deps{Messages: messages})
	holder := contextfrag.NewLifecycleHolder()
	holder.SetManifest(contextfrag.BuildManifest(nil))

	err := service.persistACPRound(
		context.Background(),
		runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "inspect"},
		"codex",
		"/data/app",
		withTranscriptOutput(acpclient.PromptResult{}, []event.StreamEvent{
			{Type: event.TextDelta, Delta: "Before"},
			{Type: event.ToolCallStart, ToolCallID: "read-1", ToolName: "read"},
			{Type: event.ToolCallEnd, ToolCallID: "read-1", ToolName: "read", Result: map[string]any{"ok": true}},
			{Type: event.TextDelta, Delta: "After"},
		}...),
		nil,
		true,
		holder,
		nil,
	)
	if err != nil {
		t.Fatalf("persistACPRound() error = %v", err)
	}
	if len(messages.Persisted) != 4 {
		t.Fatalf("persisted messages = %d, want user + assistant + tool + assistant", len(messages.Persisted))
	}
	if _, ok := messages.Persisted[1].Metadata[contextfrag.MetadataContextLifecycleKey]; ok {
		t.Fatalf("first assistant lifecycle metadata leaked from final assistant: %#v", messages.Persisted[1].Metadata)
	}
	if _, ok := messages.Persisted[3].Metadata[contextfrag.MetadataContextLifecycleKey].(contextfrag.LifecycleSnapshot); !ok {
		t.Fatalf("final assistant lifecycle metadata = %#v, want snapshot", messages.Persisted[3].Metadata)
	}
}

func TestPersistACPRoundStoresACPThoughtsAsReasoningParts(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := testHost(t, Deps{Messages: messages})

	err := resolver.persistACPRound(
		context.Background(),
		runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "inspect"},
		"codex",
		"/data/app",
		withTranscriptOutput(acpclient.PromptResult{StopReason: "end_turn"}, []event.StreamEvent{
			{Type: event.ReasoningDelta, Delta: "I should inspect first."},
			{Type: event.TextDelta, Delta: "Done"},
		}...),
		nil,
		true,
		nil,
		[]messagepkg.ReasoningTimingSegment{{
			DurationMS: 2000,
			State:      "completed",
		}},
	)
	if err != nil {
		t.Fatalf("persistACPRound returned error: %v", err)
	}
	if len(messages.Persisted) != 2 {
		t.Fatalf("persisted %d messages, want user + assistant", len(messages.Persisted))
	}
	assistant := persistedModelMessage(t, messages.Persisted[1].Content)
	if got := assistant.TextContent(); got != "Done" {
		t.Fatalf("assistant text = %q, want Done", got)
	}
	parts := assistant.ContentParts()
	if len(parts) < 2 || parts[0].Type != "reasoning" || parts[0].Text != "I should inspect first." {
		t.Fatalf("assistant parts = %#v, want leading reasoning part", parts)
	}
	segments := messagepkg.ReasoningTimingFromMetadata(messages.Persisted[1].Metadata)
	if len(segments) != 1 || segments[0].DurationMS != 2000 {
		t.Fatalf("assistant reasoning timing = %#v", segments)
	}
}

func TestPersistACPRoundEmptyTextLeavesAssistantBlank(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := testHost(t, Deps{Messages: messages})
	if err := resolver.persistACPRound(
		context.Background(),
		runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "run"},
		"codex",
		"/data/app",
		acpclient.PromptResult{},
		nil,
		true,
		nil,
		nil,
	); err != nil {
		t.Fatalf("persistACPRound() error = %v", err)
	}
	if len(messages.Persisted) != 2 {
		t.Fatalf("persisted %d messages, want 2", len(messages.Persisted))
	}
	if got := persistedText(t, messages.Persisted[1].Content); got != "" {
		t.Fatalf("assistant text = %q, want empty", got)
	}
}

func TestPersistACPRoundEmptyOutputKeepsUsage(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := testHost(t, Deps{Messages: messages})
	if err := resolver.persistACPRound(
		context.Background(),
		runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "run"},
		"codex",
		"/data/app",
		acpclient.PromptResult{
			Usage: &sdk.Usage{
				InputTokens:  9,
				OutputTokens: 4,
			},
		},
		nil,
		true,
		nil,
		nil,
	); err != nil {
		t.Fatalf("persistACPRound() error = %v", err)
	}
	if len(messages.Persisted) != 2 {
		t.Fatalf("persisted %d messages, want 2", len(messages.Persisted))
	}
	var usage sdk.Usage
	if err := json.Unmarshal(messages.Persisted[1].Usage, &usage); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	if usage.InputTokens != 9 || usage.OutputTokens != 4 {
		t.Fatalf("usage = %+v, want input=9 output=4", usage)
	}
}

// persistACPRound persists one ACP round the way the runtime executor does:
// the pool result is mapped as the ACP driver maps it, and a prompt error
// becomes the failure the round records.
func (s *Host) persistACPRound(
	ctx context.Context,
	req runreq.ChatRequest,
	agentID, projectPath string,
	result acpclient.PromptResult,
	promptErr error,
	turnCompleted bool,
	contextLifecycle *contextfrag.LifecycleHolder,
	reasoningTiming []messagepkg.ReasoningTimingSegment,
) error {
	return s.persistRound(ctx, req, acpagent.RuntimeType, projectPath, acpagent.DriverPromptResult(result, agentID), externalRoundFailure(promptErr), turnCompleted, contextLifecycle, reasoningTiming)
}

// externalRoundFailure is the failure a round records for an error the runtime's
// prompt returned, named the way the executor names it.
func externalRoundFailure(promptErr error) *outcome.RunFailure {
	if promptErr == nil {
		return nil
	}
	code, args := outcome.RunFailureCode(outcome.NewExternalPromptFailure(runtimeerr.ExternalAgentError(promptErr)), outcome.PhaseExecution, false)
	return &outcome.RunFailure{Code: code, Args: args, Cause: promptErr, Phase: outcome.PhaseExecution}
}

// withTranscriptOutput fills PromptResult.Output from streamed events.
func withTranscriptOutput(result acpclient.PromptResult, events ...event.StreamEvent) acpclient.PromptResult {
	result.Output = acpclient.TranscriptFromEvents(events, result.Text)
	return result
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

func persistedModelMessage(t *testing.T, content json.RawMessage) runreq.ModelMessage {
	t.Helper()
	var msg runreq.ModelMessage
	if err := json.Unmarshal(content, &msg); err != nil {
		t.Fatalf("decode persisted content: %v", err)
	}
	return msg
}

func persistedText(t *testing.T, content json.RawMessage) string {
	t.Helper()
	return persistedModelMessage(t, content).TextContent()
}
