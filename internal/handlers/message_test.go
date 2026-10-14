package handlers

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	chatview "github.com/felinics/memoh/internal/agent/view"
	session "github.com/felinics/memoh/internal/chat/thread"
)

type testFlusher struct{}

func (*testFlusher) Flush() {}

func TestParseBeforeParam(t *testing.T) {
	t.Parallel()

	if _, ok := parseBeforeParam(""); ok {
		t.Fatalf("expected empty before value to be ignored")
	}
	parsed, ok := parseBeforeParam("1735689600000")
	if !ok {
		t.Fatalf("expected epoch millis before value to parse")
	}
	if parsed.UnixMilli() != 1735689600000 {
		t.Fatalf("expected parsed epoch millis 1735689600000, got %d", parsed.UnixMilli())
	}
}

func TestIsUserFacingSessionType(t *testing.T) {
	t.Parallel()

	for _, typ := range []string{session.TypeChat, session.TypeDiscuss, session.TypeACPAgent} {
		if !session.IsUserFacingType(typ) {
			t.Fatalf("expected %q to be user-facing", typ)
		}
	}
	for _, typ := range []string{session.TypeSchedule, session.TypeSubagent, ""} {
		if session.IsUserFacingType(typ) {
			t.Fatalf("expected %q to be filtered out of user-facing surfaces", typ)
		}
	}
}

func TestMergeToolApprovalsSplitsLivenessFromViewerAuthority(t *testing.T) {
	t.Parallel()

	newTurns := func() []chatview.UITurn {
		return []chatview.UITurn{{
			Role: "assistant",
			Messages: []chatview.UIMessage{
				{Type: chatview.UIMessageTool, ToolCallID: "call-1"},
				{Type: chatview.UIMessageTool, ToolCallID: "call-2"},
			},
		}}
	}
	approvals := []toolapproval.Request{
		{ID: "approval-1", ToolCallID: "call-1", ShortID: 7, Status: toolapproval.StatusPending},
		{ID: "approval-2", ToolCallID: "call-2", ShortID: 8, Status: toolapproval.StatusApproved},
	}

	t.Run("no viewer authority renders read-only", func(t *testing.T) {
		turns := newTurns()
		mergeToolApprovals(turns, approvals, nil, nil)
		pending, decided := turns[0].Messages[0].Approval, turns[0].Messages[1].Approval
		if pending == nil || !pending.PendingAlive || pending.ViewerCanDecide {
			t.Fatalf("pending approval = %+v, want alive and not decidable", pending)
		}
		if decided == nil || decided.PendingAlive || decided.ViewerCanDecide {
			t.Fatalf("decided approval = %+v, want neither alive nor decidable", decided)
		}
	})

	t.Run("viewer authority is asked only about live approvals", func(t *testing.T) {
		turns := newTurns()
		var asked []string
		mergeToolApprovals(turns, approvals, nil, func(req toolapproval.Request) bool {
			asked = append(asked, req.ID)
			return true
		})
		if len(asked) != 1 || asked[0] != "approval-1" {
			t.Fatalf("viewer authority asked about %v, want only approval-1", asked)
		}
		if !turns[0].Messages[0].Approval.ViewerCanDecide || turns[0].Messages[1].Approval.ViewerCanDecide {
			t.Fatalf("viewer_can_decide = %t/%t, want true/false",
				turns[0].Messages[0].Approval.ViewerCanDecide, turns[0].Messages[1].Approval.ViewerCanDecide)
		}
	})

	t.Run("injected liveness wins over status", func(t *testing.T) {
		turns := newTurns()
		mergeToolApprovals(turns, approvals, func(toolapproval.Request) bool { return false }, func(toolapproval.Request) bool { return true })
		if approval := turns[0].Messages[0].Approval; approval.PendingAlive || approval.ViewerCanDecide {
			t.Fatalf("zombie approval = %+v, want neither alive nor decidable", approval)
		}
	})
}

func TestMergeUserInputsRestoresStoredAnswers(t *testing.T) {
	t.Parallel()

	turns := []chatview.UITurn{{
		Role: "assistant",
		Messages: []chatview.UIMessage{{
			Type:       chatview.UIMessageTool,
			ToolCallID: "call-ask",
		}},
	}}
	requests := []userinput.Request{{
		ID:         "input-1",
		ToolCallID: "call-ask",
		Status:     userinput.StatusSubmitted,
		Result: map[string]any{
			"answers": []any{map[string]any{
				"question_id": "q1",
				"question":    "Which plan?",
				"selected": []any{map[string]any{
					"id": "q1.o2", "label": "Plan B",
				}},
			}},
		},
	}}

	mergeUserInputs(turns, requests, nil)

	merged := turns[0].Messages[0].UserInput
	if merged == nil || len(merged.Answers) != 1 || len(merged.Answers[0].Selected) != 1 {
		t.Fatalf("stored answers were not merged: %#v", merged)
	}
	if merged.Answers[0].Selected[0].Label != "Plan B" || merged.CanRespond {
		t.Fatalf("merged user input = %#v", merged)
	}
}

func TestWriteSSEJSON(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	flusher := &testFlusher{}

	if err := writeSSEJSON(&output, flusher, map[string]any{"type": "ping"}); err != nil {
		t.Fatalf("writeSSEJSON failed: %v", err)
	}
	raw := output.String()
	if !strings.HasPrefix(raw, "data: ") {
		t.Fatalf("expected SSE data prefix, got %q", raw)
	}
	if !strings.HasSuffix(raw, "\n\n") {
		t.Fatalf("expected SSE payload suffix, got %q", raw)
	}
	payloadText := strings.TrimSuffix(strings.TrimPrefix(raw, "data: "), "\n\n")
	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadText), &payload); err != nil {
		t.Fatalf("decode SSE payload failed: %v", err)
	}
	if payload["type"] != "ping" {
		t.Fatalf("expected payload type ping, got %#v", payload["type"])
	}
}
