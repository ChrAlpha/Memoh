package external_test

import (
	"encoding/json"
	"testing"

	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	acpclient "github.com/felinics/memoh/internal/agent/runtime/acp/client"
)

func TestACPResultOutputMessagesPersistsUserInputMetadata(t *testing.T) {
	t.Parallel()

	output := transcriptModelMessages(acpclient.PromptResult{
		Events: []event.StreamEvent{
			{
				Type:       event.ToolCallStart,
				ToolCallID: "mcp-http-call-1",
				ToolName:   "ask_user",
				Input:      map[string]any{"questions": []any{map[string]any{"text": "Which plan?", "kind": "single_select"}}},
			},
			{
				Type:        event.UserInputRequest,
				ToolCallID:  "mcp-http-call-1",
				ToolName:    "ask_user",
				UserInputID: "input-1",
				ShortID:     3,
				Status:      "pending",
				Metadata: map[string]any{
					"ui_payload": map[string]any{
						"version": 2,
						"questions": []any{
							map[string]any{"id": "q1", "text": "Which plan?", "kind": "single_select"},
						},
					},
				},
			},
		},
	})
	if len(output) != 1 || output[0].Role != "assistant" {
		t.Fatalf("output = %#v, want one assistant message", output)
	}
	var parts []struct {
		Type             string         `json:"type"`
		ToolCallID       string         `json:"toolCallId"`
		ProviderMetadata map[string]any `json:"providerMetadata"`
	}
	if err := json.Unmarshal(output[0].Content, &parts); err != nil {
		t.Fatalf("unmarshal assistant content: %v", err)
	}
	if len(parts) != 1 || parts[0].Type != "tool-call" || parts[0].ToolCallID != "mcp-http-call-1" {
		t.Fatalf("assistant parts = %#v", parts)
	}
	userInput, ok := parts[0].ProviderMetadata["user_input"].(map[string]any)
	if !ok {
		t.Fatalf("provider metadata = %#v, want user_input", parts[0].ProviderMetadata)
	}
	if userInput["user_input_id"] != "input-1" || userInput["status"] != "pending" {
		t.Fatalf("user_input metadata = %#v", userInput)
	}
	if _, ok := userInput["ui_payload"].(map[string]any); !ok {
		t.Fatalf("user_input ui_payload = %#v", userInput["ui_payload"])
	}
}

func TestACPResultOutputMessagesPersistsToolApprovalMetadata(t *testing.T) {
	t.Parallel()

	output := transcriptModelMessages(acpclient.PromptResult{
		Events: []event.StreamEvent{
			{
				Type:       event.ToolCallStart,
				ToolCallID: "write-1",
				ToolName:   "write",
				Input:      map[string]any{"path": "/data/review.txt"},
			},
			{
				Type:       event.ToolApprovalRequest,
				ToolCallID: "write-1",
				ToolName:   "write",
				ApprovalID: "approval-1",
				ShortID:    4,
				Status:     toolapproval.StatusPending,
			},
		},
	})
	if len(output) != 1 || output[0].Role != "assistant" {
		t.Fatalf("output = %#v, want one assistant message", output)
	}
	var parts []struct {
		Type             string         `json:"type"`
		ToolCallID       string         `json:"toolCallId"`
		ProviderMetadata map[string]any `json:"providerMetadata"`
	}
	if err := json.Unmarshal(output[0].Content, &parts); err != nil {
		t.Fatalf("unmarshal assistant content: %v", err)
	}
	if len(parts) != 1 || parts[0].Type != "tool-call" || parts[0].ToolCallID != "write-1" {
		t.Fatalf("assistant parts = %#v", parts)
	}
	approval, ok := parts[0].ProviderMetadata["approval"].(map[string]any)
	if !ok {
		t.Fatalf("provider metadata = %#v, want approval", parts[0].ProviderMetadata)
	}
	if approval["approval_id"] != "approval-1" || approval["status"] != toolapproval.StatusPending {
		t.Fatalf("approval metadata = %#v", approval)
	}
	if approval["short_id"] != float64(4) {
		t.Fatalf("approval short_id = %#v, want 4", approval["short_id"])
	}
}

func TestACPResultOutputMessagesPersistsResolvedToolApprovalMetadata(t *testing.T) {
	t.Parallel()

	output := transcriptModelMessages(acpclient.PromptResult{
		Events: []event.StreamEvent{
			{
				Type:       event.ToolCallStart,
				ToolCallID: "write-1",
				ToolName:   "write",
				Input:      map[string]any{"path": "/data/review.txt"},
			},
			{
				Type:       event.ToolApprovalRequest,
				ToolCallID: "write-1",
				ToolName:   "write",
				ApprovalID: "approval-1",
				ShortID:    4,
				Status:     toolapproval.StatusPending,
			},
			{
				Type:       event.ToolApprovalRequest,
				ToolCallID: "write-1",
				ToolName:   "write",
				ApprovalID: "approval-1",
				ShortID:    4,
				Status:     toolapproval.StatusApproved,
			},
		},
	})
	if len(output) != 1 || output[0].Role != "assistant" {
		t.Fatalf("output = %#v, want one assistant message", output)
	}
	var parts []struct {
		Type             string         `json:"type"`
		ToolCallID       string         `json:"toolCallId"`
		ProviderMetadata map[string]any `json:"providerMetadata"`
	}
	if err := json.Unmarshal(output[0].Content, &parts); err != nil {
		t.Fatalf("unmarshal assistant content: %v", err)
	}
	if len(parts) != 1 || parts[0].Type != "tool-call" || parts[0].ToolCallID != "write-1" {
		t.Fatalf("assistant parts = %#v", parts)
	}
	approval, ok := parts[0].ProviderMetadata["approval"].(map[string]any)
	if !ok {
		t.Fatalf("provider metadata = %#v, want approval", parts[0].ProviderMetadata)
	}
	if approval["approval_id"] != "approval-1" || approval["status"] != toolapproval.StatusApproved || approval["pending_alive"] != false {
		t.Fatalf("approval metadata = %#v", approval)
	}
}

func TestACPResultOutputMessagesMergesApprovalBeforeToolStart(t *testing.T) {
	t.Parallel()

	output := transcriptModelMessages(acpclient.PromptResult{
		Events: []event.StreamEvent{
			{
				Type:       event.ToolApprovalRequest,
				ToolCallID: "exec-1",
				ToolName:   "exec",
				Input:      map[string]any{"command": "pwd"},
				ApprovalID: "approval-1",
				ShortID:    1,
				Status:     toolapproval.StatusPending,
			},
			{
				Type:       event.ToolCallStart,
				ToolCallID: "exec-1",
				ToolName:   "exec",
				Input:      map[string]any{"command": "pwd"},
			},
		},
	})
	if len(output) != 1 || output[0].Role != "assistant" {
		t.Fatalf("output = %#v, want one assistant message", output)
	}
	var parts []struct {
		Type             string         `json:"type"`
		ToolCallID       string         `json:"toolCallId"`
		Input            map[string]any `json:"input"`
		ProviderMetadata map[string]any `json:"providerMetadata"`
	}
	if err := json.Unmarshal(output[0].Content, &parts); err != nil {
		t.Fatalf("unmarshal assistant content: %v", err)
	}
	if len(parts) != 1 || parts[0].Type != "tool-call" || parts[0].ToolCallID != "exec-1" {
		t.Fatalf("assistant parts = %#v, want one merged tool-call", parts)
	}
	if parts[0].Input["command"] != "pwd" {
		t.Fatalf("merged input = %#v, want pwd", parts[0].Input)
	}
	if _, ok := parts[0].ProviderMetadata["approval"].(map[string]any); !ok {
		t.Fatalf("provider metadata = %#v, want approval", parts[0].ProviderMetadata)
	}
}

// transcriptModelMessages builds model messages from streamed ACP events.
func transcriptModelMessages(result acpclient.PromptResult) []runreq.ModelMessage {
	output := runreq.SDKMessagesToModelMessages(acpclient.TranscriptFromEvents(result.Events, result.Text))
	if len(output) == 0 {
		return []runreq.ModelMessage{{Role: "assistant", Content: runreq.NewTextContent("")}}
	}
	return output
}
