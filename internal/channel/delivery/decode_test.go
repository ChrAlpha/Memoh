package delivery

import (
	"encoding/json"
	"testing"

	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/i18n"
)

func TestDecode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		chunk         string
		wantType      channel.StreamEventType
		wantDelta     string
		wantPhase     channel.StreamPhase
		wantToolName  string
		wantAttCount  int
		wantError     string
		wantErrorCode string
		wantNilEvents bool
	}{
		{
			name:      "text_delta",
			chunk:     `{"type":"text_delta","delta":"hello"}`,
			wantType:  channel.StreamEventDelta,
			wantDelta: "hello",
			wantPhase: channel.StreamPhaseText,
		},
		{
			name:          "text_delta empty",
			chunk:         `{"type":"text_delta","delta":""}`,
			wantNilEvents: true,
		},
		{
			name:      "reasoning_delta",
			chunk:     `{"type":"reasoning_delta","delta":"thinking"}`,
			wantType:  channel.StreamEventDelta,
			wantDelta: "thinking",
			wantPhase: channel.StreamPhaseReasoning,
		},
		{
			name:          "reasoning_delta empty",
			chunk:         `{"type":"reasoning_delta","delta":""}`,
			wantNilEvents: true,
		},
		{
			name:      "reasoning_start",
			chunk:     `{"type":"reasoning_start"}`,
			wantType:  channel.StreamEventPhaseStart,
			wantPhase: channel.StreamPhaseReasoning,
		},
		{
			name:      "reasoning_end",
			chunk:     `{"type":"reasoning_end"}`,
			wantType:  channel.StreamEventPhaseEnd,
			wantPhase: channel.StreamPhaseReasoning,
		},
		{
			name:      "text_start",
			chunk:     `{"type":"text_start"}`,
			wantType:  channel.StreamEventPhaseStart,
			wantPhase: channel.StreamPhaseText,
		},
		{
			name:      "text_end",
			chunk:     `{"type":"text_end"}`,
			wantType:  channel.StreamEventPhaseEnd,
			wantPhase: channel.StreamPhaseText,
		},
		{
			name:         "tool_call_start",
			chunk:        `{"type":"tool_call_start","toolName":"search_web","toolCallId":"tc_1","input":{"query":"test"}}`,
			wantType:     channel.StreamEventToolCallStart,
			wantToolName: "search_web",
		},
		{
			name:         "tool_call_end",
			chunk:        `{"type":"tool_call_end","toolName":"search_web","toolCallId":"tc_1","input":{"query":"test"},"result":{"ok":true}}`,
			wantType:     channel.StreamEventToolCallEnd,
			wantToolName: "search_web",
		},
		{
			name:         "attachment_delta",
			chunk:        `{"type":"attachment_delta","attachments":[{"type":"image","url":"https://example.com/img.png"}]}`,
			wantType:     channel.StreamEventAttachment,
			wantAttCount: 1,
		},
		{
			name:          "attachment_delta empty",
			chunk:         `{"type":"attachment_delta","attachments":[]}`,
			wantNilEvents: true,
		},
		{
			name:          "uncoded error shows the failed run copy",
			chunk:         `{"type":"error","error":"something failed"}`,
			wantType:      channel.StreamEventError,
			wantError:     "The response could not be completed. Please try again.",
			wantErrorCode: "runtime_run_failed",
		},
		{
			name:          "uncoded error with a message shows the failed run copy",
			chunk:         `{"type":"error","message":"fallback msg"}`,
			wantType:      channel.StreamEventError,
			wantError:     "The response could not be completed. Please try again.",
			wantErrorCode: "runtime_run_failed",
		},
		{
			name:          "error with a catalogued code shows its copy",
			chunk:         `{"type":"error","code":"agent.provider_overloaded","error":"upstream 529 overloaded_error"}`,
			wantType:      channel.StreamEventError,
			wantError:     "The model provider is unavailable or overloaded right now. Please try again in a moment.",
			wantErrorCode: "agent.provider_overloaded",
		},
		{
			name:          "error with an unknown code shows the failed run copy",
			chunk:         `{"type":"error","code":"not.in_catalog","error":"something failed"}`,
			wantType:      channel.StreamEventError,
			wantError:     "The response could not be completed. Please try again.",
			wantErrorCode: "runtime_run_failed",
		},
		{
			name:     "agent_start",
			chunk:    `{"type":"agent_start","input":{"agent":"planner"}}`,
			wantType: channel.StreamEventAgentStart,
		},
		{
			name:     "agent_end",
			chunk:    `{"type":"agent_end","result":{"ok":true}}`,
			wantType: channel.StreamEventAgentEnd,
		},
		{
			name:     "retry",
			chunk:    `{"type":"retry","attempt":1,"maxAttempt":3,"retryError":"stream cut"}`,
			wantType: channel.StreamEventReset,
		},
		{
			name:     "processing_started",
			chunk:    `{"type":"processing_started"}`,
			wantType: channel.StreamEventProcessingStarted,
		},
		{
			name:     "processing_completed",
			chunk:    `{"type":"processing_completed"}`,
			wantType: channel.StreamEventProcessingCompleted,
		},
		{
			name:          "empty chunk",
			chunk:         ``,
			wantNilEvents: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			events, _, err := Decode(json.RawMessage(tt.chunk), i18n.New("en"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantNilEvents {
				if len(events) > 0 {
					t.Fatalf("expected nil/empty events, got %d", len(events))
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("expected 1 event, got %d", len(events))
			}
			ev := events[0]
			if ev.Type != tt.wantType {
				t.Fatalf("expected type %q, got %q", tt.wantType, ev.Type)
			}
			if tt.wantDelta != "" && ev.Delta != tt.wantDelta {
				t.Fatalf("expected delta %q, got %q", tt.wantDelta, ev.Delta)
			}
			if tt.wantPhase != "" && ev.Phase != tt.wantPhase {
				t.Fatalf("expected phase %q, got %q", tt.wantPhase, ev.Phase)
			}
			if tt.wantToolName != "" {
				if ev.ToolCall == nil {
					t.Fatal("expected non-nil ToolCall")
				}
				if ev.ToolCall.Name != tt.wantToolName {
					t.Fatalf("expected tool name %q, got %q", tt.wantToolName, ev.ToolCall.Name)
				}
			}
			if tt.wantAttCount > 0 && len(ev.Attachments) != tt.wantAttCount {
				t.Fatalf("expected %d attachments, got %d", tt.wantAttCount, len(ev.Attachments))
			}
			if tt.wantError != "" && ev.Error != tt.wantError {
				t.Fatalf("expected error %q, got %q", tt.wantError, ev.Error)
			}
			if ev.ErrorCode != tt.wantErrorCode {
				t.Fatalf("expected error code %q, got %q", tt.wantErrorCode, ev.ErrorCode)
			}
		})
	}
}

func TestDecode_ToolCallFields(t *testing.T) {
	t.Parallel()

	chunk := `{"type":"tool_call_end","toolName":"calc","toolCallId":"c1","input":{"x":1},"result":{"sum":2}}`
	events, _, err := Decode(json.RawMessage(chunk), i18n.New("en"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	tc := events[0].ToolCall
	if tc == nil {
		t.Fatal("expected non-nil ToolCall")
		return
	}
	if tc.Name != "calc" || tc.CallID != "c1" {
		t.Fatalf("unexpected name/callID: %q / %q", tc.Name, tc.CallID)
	}
	if tc.Input == nil || tc.Result == nil {
		t.Fatal("expected non-nil Input and Result")
	}
}

func TestDecode_UserInputRequest(t *testing.T) {
	t.Parallel()

	chunk := `{"type":"user_input_request","toolName":"ask_user","toolCallId":"ask-1","userInputId":"input-1","shortId":7,"status":"pending","input":{"questions":[{"text":"Original model input","kind":"single_select","options":[{"label":"Alpha"},{"label":"Beta"}]}]},"metadata":{"ui_payload":{"version":2,"questions":[{"id":"q1","text":"Pick one","kind":"single_select","options":[{"id":"q1.o1","label":"Alpha"},{"id":"q1.o2","label":"Beta"}]}]}}}`
	events, _, err := Decode(json.RawMessage(chunk), i18n.New("en"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	tc := events[0].ToolCall
	if events[0].Type != channel.StreamEventToolCallStart || tc == nil {
		t.Fatalf("event = %#v, want tool call start", events[0])
	}
	if tc.Name != "ask_user" || tc.CallID != "ask-1" || tc.ShortID != 7 {
		t.Fatalf("tool call = %#v", tc)
	}
	input, ok := tc.Input.(map[string]any)
	if !ok {
		t.Fatalf("input = %#v, want map", tc.Input)
	}
	if input["user_input_id"] != "input-1" || input["status"] != "pending" {
		t.Fatalf("input = %#v", input)
	}
	payload, ok := input["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload = %#v", input["payload"])
	}
	questions, ok := payload["questions"].([]any)
	if !ok || len(questions) != 1 || questions[0].(map[string]any)["text"] != "Pick one" {
		t.Fatalf("payload questions = %#v", payload["questions"])
	}
	// The shared layer emits a single filter-keepalive marker; adapters with
	// native controls (Telegram) rebuild the real keyboard from the payload.
	if len(tc.Actions) != 1 || tc.Actions[0].Type != "user_input" || tc.Actions[0].Label != "Reply" || tc.Actions[0].Value != "respond:input-1" {
		t.Fatalf("actions = %#v", tc.Actions)
	}
}

func TestDecode_UserInputTextFallback(t *testing.T) {
	t.Parallel()

	chunk := `{"type":"user_input_request","toolName":"ask_user","toolCallId":"ask-1","userInputId":"input-1","shortId":7,"status":"pending","input":{"questions":[{"id":"q1","text":"Explain","kind":"text"}]}}`
	events, _, err := Decode(json.RawMessage(chunk), i18n.New("en"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 || events[0].ToolCall == nil {
		t.Fatalf("events = %#v", events)
	}
	actions := events[0].ToolCall.Actions
	if len(actions) != 1 || actions[0].Label != "Reply" || actions[0].Value != "respond:input-1" {
		t.Fatalf("actions = %#v", actions)
	}
}

func TestDecode_UserInputCustomOptionFallback(t *testing.T) {
	t.Parallel()

	chunk := `{"type":"user_input_request","toolName":"ask_user","toolCallId":"ask-1","userInputId":"input-1","shortId":7,"status":"pending","metadata":{"ui_payload":{"version":2,"questions":[{"id":"q1","text":"Pick one","kind":"single_select","allow_custom":true,"options":[{"id":"q1.o1","label":"Alpha"},{"id":"q1.o2","label":"Beta"}]}]}}}`
	events, _, err := Decode(json.RawMessage(chunk), i18n.New("en"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	actions := events[0].ToolCall.Actions
	if len(actions) != 1 || actions[0].Label != "Reply" || actions[0].Value != "respond:input-1" {
		t.Fatalf("actions = %#v", actions)
	}
}

func TestDecode_UserInputMultiQuestionKeepsActions(t *testing.T) {
	t.Parallel()

	// Multi-question prompts must still emit a user_input action so the
	// show_tool_calls_in_im filter does not drop the pending card. Telegram
	// rebuilds the paged keyboard from the payload.
	chunk := `{"type":"user_input_request","toolName":"ask_user","toolCallId":"ask-1","userInputId":"input-1","shortId":7,"status":"pending","metadata":{"ui_payload":{"version":2,"questions":[{"id":"q1","text":"What?","kind":"text"},{"id":"q2","text":"How fast?","kind":"single_select","options":[{"id":"q2.o1","label":"Fast"},{"id":"q2.o2","label":"Slow"}]}]}}}`
	events, _, err := Decode(json.RawMessage(chunk), i18n.New("en"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 || events[0].ToolCall == nil {
		t.Fatalf("events = %#v", events)
	}
	actions := events[0].ToolCall.Actions
	if len(actions) == 0 || actions[0].Type != "user_input" {
		t.Fatalf("multi-question must keep user_input actions, got %#v", actions)
	}
	// A prompt the user has to answer is shown even when tool calls are hidden in IM.
	planned, _ := Plan(events[0], previewCaps, hiding(state(t0)))
	if !sameKinds(planned, KindSend) {
		t.Fatalf("multi-question ask_user was not shown: %v", kinds(planned))
	}
}

func TestDecode_FinalMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		chunk     string
		eventType channel.StreamEventType
		wantEvent bool
	}{
		{
			name:      "agent_end",
			chunk:     `{"type":"agent_end","messages":[{"role":"assistant","content":"done"}]}`,
			eventType: channel.StreamEventAgentEnd,
			wantEvent: true,
		},
		{
			name:  "agent_abort",
			chunk: `{"type":"agent_abort","messages":[{"role":"assistant","content":"partial failure"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			events, messages, err := Decode(json.RawMessage(tt.chunk), i18n.New("en"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantEvent {
				if len(events) != 1 {
					t.Fatalf("expected 1 event, got %d", len(events))
				}
				if events[0].Type != tt.eventType {
					t.Fatalf("expected event type %q, got %q", tt.eventType, events[0].Type)
				}
			} else if len(events) != 0 {
				t.Fatalf("expected no channel event, got %d", len(events))
			}
			if len(messages) != 1 {
				t.Fatalf("expected 1 final message, got %d", len(messages))
			}
			if messages[0].Role != "assistant" {
				t.Fatalf("expected role assistant, got %q", messages[0].Role)
			}
		})
	}
}
