package handlers

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	chatview "github.com/felinics/memoh/internal/agent/view"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/server"
)

// The files under testdata/ws-frames are the browser-facing WebSocket frame
// contract. The web client (including the desktop build, which may be older than
// the server) reads the same files in apps/web/src/store/chat/ws-frames.golden.test.ts.
var updateWSFrames = flag.Bool("update", false, "rewrite testdata/ws-frames golden files")

const wsFramesDir = "testdata/ws-frames"

const (
	goldenSession = "22222222-2222-2222-2222-222222222222"
	goldenBot     = "11111111-1111-1111-1111-111111111111"
	goldenRun     = "33333333-3333-3333-3333-333333333333"
	goldenTurn    = "44444444-4444-4444-4444-444444444444"
	goldenEpoch   = "epoch-1"
)

var goldenTime = time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func goldenAttachment() chatview.UIAttachment {
	return chatview.UIAttachment{
		ID: "att-1", Type: "image", Path: "/data/a.png", URL: "https://example.test/a.png",
		Name: "a.png", ContentHash: "sha256:abc", BotID: goldenBot, Mime: "image/png",
		Size: 2048, StorageKey: "bots/a.png", Metadata: map[string]any{"width": 640, "height": 480},
	}
}

func goldenMessages() []chatview.UIMessage {
	return []chatview.UIMessage{
		{ID: 1, Type: chatview.UIMessageReasoning, Content: "thinking", ReasoningTiming: &chatview.UIReasoningTiming{DurationMS: 1200}},
		{ID: 2, Type: chatview.UIMessageText, Content: "hello"},
		{
			ID: 3, Type: chatview.UIMessageTool, Name: "exec", ToolCallID: "call-1",
			Input: map[string]any{"command": "ls"}, Output: map[string]any{"stdout": "a\n"},
			Running: ptr(false), Progress: []any{"line 1", map[string]any{"pct": 50}},
			ElapsedTimeSeconds: ptr(1.5), Diff: "--- a\n+++ b\n",
			ExecutionLocation: &chatview.UIExecutionLocation{Kind: "workspace", Name: "main"},
			Background: &chatview.UIBackgroundTask{
				TaskID: "task-1", Status: "running", Command: "sleep 9", AgentID: "agent-1", AgentSessionID: "as-1",
				OutputFile: "/tmp/out", ExitCode: 0, Duration: "3s", OutputTail: "tail", Stream: "stdout", Chunk: "c", Stalled: true,
			},
		},
		{
			ID: 4, Type: chatview.UIMessageTool, Name: "write", ToolCallID: "call-2", Input: map[string]any{"path": "x"},
			Running: ptr(true),
			Approval: &chatview.UIToolApproval{
				ApprovalID: "ap-1", ShortID: 7, Status: "pending", DecisionReason: "needs review", CanApprove: true,
				Options: []chatview.UIToolApprovalOption{
					{ID: "o1", Name: "Allow once", Kind: "allow_once"},
					{ID: "o2", Name: "Reject", Kind: "reject_once"},
				},
				SelectedOptionID: "o1",
			},
		},
		{
			ID: 5, Type: chatview.UIMessageTool, Name: "ask_user", ToolCallID: "call-3", Running: ptr(true),
			UserInput: &chatview.UIUserInput{
				UserInputID: "ui-1", ShortID: 3, Status: "pending", CanRespond: true,
				Questions: []userinput.UIQuestion{{
					ID: "q1", Text: "Pick one", Kind: "single_select", AllowCustom: true, CustomExclusive: true,
					Required: ptr(true), Placeholder: "other",
					Options: []userinput.UIOption{{LabelKey: "opt.a", ID: "a", Label: "A", Description: "first"}, {ID: "b", Label: "B"}},
				}},
				Answers: []userinput.UIAnswer{{
					QuestionID: "q1", Question: "Pick one", Selected: []userinput.UIOption{{ID: "a", Label: "A"}},
					CustomText: "c", Text: "t", Skipped: false,
				}},
			},
		},
		{ID: 6, Type: chatview.UIMessageAttachments, Attachments: []chatview.UIAttachment{goldenAttachment()}},
		{ID: 7, Type: chatview.UIMessageError, Code: "session_runtime.session_busy", Content: "busy", Args: map[string]string{"k": "v"}},
		{ID: 8, Type: chatview.UIMessageNotice, Name: "tools_unavailable", Content: "tools are unavailable", Args: map[string]string{"dep_id": "d1"}},
		{ID: 9, Type: chatview.UIMessageCommand, Name: "help", Content: "/help"},
		{ID: 10, Type: chatview.UIMessageStatus, Name: "compacting", Args: map[string]string{"phase": "start"}},
	}
}

func goldenUserTurn(id string, position int64) chatview.UITurn {
	return chatview.UITurn{
		TurnID: id, TurnPosition: ptr(position), Role: "user", Text: "do it", UserMessageKind: "normal",
		SkillActivation: &turn.SkillActivation{
			Skills: []turn.SkillActivationSkill{{Name: "s1", DisplayName: "S1", Description: "d", SourceKind: "bot", State: "active"}},
			Prompt: "use s1",
		},
		Attachments:       []chatview.UIAttachment{goldenAttachment()},
		Reply:             &chatview.UIReplyRef{MessageID: "m0", Sender: "bob", Preview: "earlier", Attachments: []chatview.UIAttachment{goldenAttachment()}},
		Forward:           &chatview.UIForwardRef{MessageID: "m9", FromUserID: "u9", FromConversationID: "c9", Sender: "eve", Date: 1760000000},
		Timestamp:         goldenTime,
		Platform:          "web",
		SenderDisplayName: "Alice",
		SenderAvatarURL:   "https://example.test/alice.png",
		SenderUserID:      "u1",
		ExternalMessageID: "ext-1",
		ID:                "row-1",
	}
}

func goldenRunView(status string) sessionruntime.CurrentRunView {
	return sessionruntime.CurrentRunView{
		RunID: goldenRun, TurnID: goldenTurn, TurnPosition: 5, InvocationID: "inv-1", Generation: "gen-1",
		Status: status, OwnerID: "owner-1", OwnerLeaseExpiresAt: ptr(goldenTime.Add(30 * time.Second)),
		StartedAt: goldenTime, UpdatedAt: goldenTime.Add(time.Second), Messages: []chatview.UIMessage{},
	}
}

func goldenRichRunView() sessionruntime.CurrentRunView {
	run := goldenRunView("running")
	run.Messages = goldenMessages()
	run.UserTurns = []chatview.UITurn{goldenUserTurn(goldenTurn, 5)}
	run.SteerSupported = true
	run.SteerTurns = []sessionruntime.SteerTurnView{
		{ItemID: "55555555-5555-5555-5555-555555555555", Status: "claimed", Text: "also this", TurnID: "66666666-6666-6666-6666-666666666666", TurnPosition: 6, AfterMessageID: 2, Timestamp: goldenTime},
		{ItemID: "77777777-7777-7777-7777-777777777777", Status: "applied", Text: "and this", AfterMessageID: 3, Timestamp: goldenTime},
	}
	run.Retry = &sessionruntime.RunRetryView{Attempt: 2, MaxAttempt: 5, DelayMs: 4000, Reason: "rate_limited", RetryAt: goldenTime.Add(4 * time.Second)}
	run.Operation = &sessionruntime.RunOperationView{Kind: "edit", ReplaceFromMessageID: "m1", ReplacementUserTurn: ptr(goldenUserTurn(goldenTurn, 5))}
	return run
}

func runtimeFrame(t *testing.T, event sessionruntime.Event) any {
	t.Helper()
	frame, ok := runtimeEventFrame(goldenSession, event)
	if !ok {
		t.Fatalf("runtimeEventFrame dropped %q", event.Type)
	}
	return frame
}

func snapshotFrame(t *testing.T, run *sessionruntime.CurrentRunView) any {
	t.Helper()
	return runtimeFrame(t, sessionruntime.Event{
		Type: sessionruntime.EventRuntimeSnapshot, Epoch: goldenEpoch, Seq: 7,
		Snapshot: &sessionruntime.Snapshot{BotID: goldenBot, SessionID: goldenSession, Epoch: goldenEpoch, Seq: 7, CurrentRunView: run, UpdatedAt: goldenTime},
	})
}

func deltaFrame(t *testing.T, delta sessionruntime.RuntimeDelta) any {
	t.Helper()
	return runtimeFrame(t, sessionruntime.Event{Type: sessionruntime.EventRuntimeDelta, Epoch: goldenEpoch, Seq: 8, Delta: &delta})
}

// sentFrame returns what a production sender wrote to the socket.
func sentFrame(send func(w *wsWriter)) []byte {
	w := &wsWriter{ch: make(chan []byte, 4), stop: make(chan struct{}), done: make(chan struct{})}
	send(w)
	return <-w.ch
}

func goldenFrames(t *testing.T) map[string][]byte {
	t.Helper()
	frames := map[string][]byte{}
	put := func(name string, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		frames[name] = data
	}
	sent := func(name string, send func(w *wsWriter)) { frames[name] = sentFrame(send) }

	ref := wsTurnRef{RunID: goldenRun, InvocationID: "inv-1", SessionID: goldenSession}
	preRun := wsTurnRef{InvocationID: "inv-1", SessionID: goldenSession}
	accepted := wsRunAcceptance{TurnID: goldenTurn, TurnPosition: 5, Cursor: sessionruntime.Cursor{Epoch: goldenEpoch, Seq: 6}}

	sent("run_accepted", func(w *wsWriter) { sendWSRunAccepted(w, ref, accepted) })
	accepted.Duplicate = true
	sent("run_accepted_duplicate", func(w *wsWriter) { sendWSRunAccepted(w, ref, accepted) })
	sent("run_rejected", func(w *wsWriter) { sendWSRunRejected(w, preRun, apperror.CodeSessionBusy) })
	sent("session_created", func(w *wsWriter) { w.SendJSON(preRun.event("session_created")) })
	sent("model_preference_settled", func(w *wsWriter) {
		w.SendJSON(wsOutboundEvent{Type: "model_preference_settled", RunID: ref.RunID, InvocationID: ref.InvocationID, SessionID: ref.SessionID})
	})
	sent("error", func(w *wsWriter) {
		w.SendJSON(wsErrorEvent(preRun, server.StreamError{Code: string(apperror.CodeSessionBusy), Args: map[string]string{"k": "v"}, Message: "busy", Fault: apperror.FaultClient}))
	})
	sent("error_run", func(w *wsWriter) {
		w.SendJSON(wsErrorEvent(ref, server.StreamError{Code: "agent.provider_failed", Message: "provider failed", Fault: apperror.FaultDependency}))
	})
	sent("control_ack_applied", func(w *wsWriter) { sendWSControlAck(w, ref, "abort", "ctl-1", true, "") })
	sent("control_ack_not_applied", func(w *wsWriter) { sendWSControlAck(w, ref, "tool_approval_response", "ctl-2", false, "") })
	sent("control_ack_failed", func(w *wsWriter) {
		sendWSControlAck(w, ref, "user_input_response", "ctl-3", false, "session_runtime.command_owner_unavailable")
	})
	msg := wsClientMessage{InvocationID: "inv-2", ComposerScope: "scope-1", SessionID: goldenSession}
	sent("command_result", func(w *wsWriter) {
		sendWSCommandResult(w, msg, "skill.list", &CommandActionResult{
			Kind: "list", Title: "Skills", Text: "text", Notice: "n", TextKey: "k", Data: map[string]any{"n": 1},
			Items: []CommandActionListItem{{I18nKey: "i", ID: "s1", Title: "S1", Description: "d", Kind: "skill"}},
		})
	})
	sent("command_error", func(w *wsWriter) { sendWSCommandError(w, msg, "slash.unknown_command") })

	put("runtime_snapshot_empty", snapshotFrame(t, nil))
	rich := goldenRichRunView()
	put("runtime_snapshot_running_rich", snapshotFrame(t, &rich))
	for _, status := range []string{"admitting", "running", "waiting_decision", "aborting", "finishing", "completed", "aborted", "errored", "lost"} {
		run := goldenRunView(status)
		put("runtime_snapshot_status_"+status, snapshotFrame(t, &run))
	}
	waiting := goldenRunView("waiting_decision")
	waiting.Messages = goldenMessages()[3:5]
	waiting.UserTurns = []chatview.UITurn{goldenUserTurn(goldenTurn, 5)}
	put("runtime_snapshot_waiting_decision", snapshotFrame(t, &waiting))
	errored := goldenRunView("errored")
	errored.ErrorCode = "agent.provider_failed"
	errored.Messages = goldenMessages()[6:7]
	put("runtime_snapshot_errored", snapshotFrame(t, &errored))
	finishing := goldenRunView("finishing")
	finishing.ProposedTerminalStatus = "completed"
	finishing.FinishProposedAt = ptr(goldenTime.Add(2 * time.Second))
	put("runtime_snapshot_finishing", snapshotFrame(t, &finishing))
	retrying := goldenRunView("running")
	retrying.Retry = &sessionruntime.RunRetryView{Attempt: 1, MaxAttempt: 3, DelayMs: 1000, RetryAt: goldenTime.Add(time.Second)}
	put("runtime_snapshot_retry_without_reason", snapshotFrame(t, &retrying))
	for _, reason := range []string{"rate_limited", "server_error", "stream_incomplete", "network"} {
		run := goldenRunView("running")
		run.Retry = &sessionruntime.RunRetryView{Attempt: 1, MaxAttempt: 3, DelayMs: 1000, Reason: reason, RetryAt: goldenTime.Add(time.Second)}
		put("runtime_snapshot_retry_"+reason, snapshotFrame(t, &run))
	}
	configOnly := goldenRunView("running")
	configOnly.ConfigurationOnly = true
	put("runtime_snapshot_configuration_only", snapshotFrame(t, &configOnly))
	retryOp := goldenRunView("running")
	retryOp.Operation = &sessionruntime.RunOperationView{Kind: "retry", ReplaceFromMessageID: "m1"}
	put("runtime_snapshot_operation_retry", snapshotFrame(t, &retryOp))

	put("runtime_delta_current_run_view", deltaFrame(t, sessionruntime.RuntimeDelta{CurrentRunView: &rich}))
	put("runtime_delta_run_patch", deltaFrame(t, sessionruntime.RuntimeDelta{Run: &sessionruntime.CurrentRunPatch{
		RunID: goldenRun, Status: ptr("errored"), ErrorCode: ptr("agent.provider_failed"),
		UpdatedAt: ptr(goldenTime.Add(3 * time.Second)), OwnerLeaseExpiresAt: ptr(goldenTime.Add(33 * time.Second)),
	}}))
	put("runtime_delta_run_patch_retry", deltaFrame(t, sessionruntime.RuntimeDelta{Run: &sessionruntime.CurrentRunPatch{
		RunID: goldenRun, Retry: &sessionruntime.RunRetryView{Attempt: 2, MaxAttempt: 5, DelayMs: 4000, Reason: "network", RetryAt: goldenTime.Add(4 * time.Second)},
	}}))
	put("runtime_delta_run_patch_clear_retry", deltaFrame(t, sessionruntime.RuntimeDelta{Run: &sessionruntime.CurrentRunPatch{RunID: goldenRun, ClearRetry: true}}))
	put("runtime_delta_message_appends", deltaFrame(t, sessionruntime.RuntimeDelta{MessageAppends: []sessionruntime.RuntimeMessageAppend{
		{ID: 1, Type: chatview.UIMessageReasoning, Content: " more"}, {ID: 2, Type: chatview.UIMessageText, Content: " world"},
	}}))
	put("runtime_delta_progress_appends", deltaFrame(t, sessionruntime.RuntimeDelta{ProgressAppends: []sessionruntime.RuntimeProgressAppend{
		{ID: 3, Progress: "line 2", Input: map[string]any{"command": "ls -l"}}, {ID: 3, Progress: map[string]any{"pct": 80}},
	}}))
	put("runtime_delta_message_upserts", deltaFrame(t, sessionruntime.RuntimeDelta{MessageUpserts: goldenMessages()}))
	put("runtime_delta_user_turn_upserts", deltaFrame(t, sessionruntime.RuntimeDelta{UserTurnUpserts: []chatview.UITurn{
		goldenUserTurn("66666666-6666-6666-6666-666666666666", 6),
		{TurnID: "88888888-8888-8888-8888-888888888888", TurnPosition: ptr(int64(7)), Role: "system", Kind: "background_task", BackgroundTask: &chatview.UIBackgroundTask{TaskID: "task-1", Status: "done", ExitCode: 1}, Timestamp: goldenTime},
		{TurnID: "99999999-9999-9999-9999-999999999999", TurnPosition: ptr(int64(8)), Role: "assistant", RuntimeForkable: true, Messages: goldenMessages()[:2], Timestamp: goldenTime},
	}}))
	put("runtime_delta_steer_upserts_removals", deltaFrame(t, sessionruntime.RuntimeDelta{
		SteerTurnUpserts:  []sessionruntime.SteerTurnView{{ItemID: "55555555-5555-5555-5555-555555555555", Status: "applied", Text: "also this", TurnID: "66666666-6666-6666-6666-666666666666", TurnPosition: 6, AfterMessageID: 2, Timestamp: goldenTime}},
		SteerTurnRemovals: []string{"77777777-7777-7777-7777-777777777777"},
	}))
	put("runtime_delta_reset_messages", deltaFrame(t, sessionruntime.RuntimeDelta{ResetMessages: true}))
	put("runtime_delta_empty", deltaFrame(t, sessionruntime.RuntimeDelta{}))
	put("runtime_dropped", runtimeFrame(t, sessionruntime.Event{Type: sessionruntime.EventRuntimeDropped, Epoch: goldenEpoch, Seq: 9, Message: "runtime subscription gap"}))
	return frames
}

func TestWSFramesGolden(t *testing.T) {
	frames := goldenFrames(t)
	if *updateWSFrames {
		if err := os.MkdirAll(wsFramesDir, 0o750); err != nil {
			t.Fatal(err)
		}
		existing, _ := filepath.Glob(filepath.Join(wsFramesDir, "*.json"))
		for _, path := range existing {
			if _, ok := frames[strings.TrimSuffix(filepath.Base(path), ".json")]; !ok {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	names := make([]string, 0, len(frames))
	for name := range frames {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var indented bytes.Buffer
		if err := json.Indent(&indented, frames[name], "", "  "); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		indented.WriteByte('\n')
		path := filepath.Join(wsFramesDir, name+".json")
		if *updateWSFrames {
			if err := os.WriteFile(path, indented.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path) //nolint:gosec // fixed testdata directory
		if err != nil {
			t.Fatalf("%s: %v (run go test ./internal/handlers -run TestWSFramesGolden -update)", name, err)
		}
		if !bytes.Equal(want, indented.Bytes()) {
			t.Errorf("%s differs from golden; removing or renaming a field breaks clients that predate the change.\nwant:\n%s\ngot:\n%s", name, want, indented.Bytes())
		}
	}
	if !*updateWSFrames {
		onDisk, _ := filepath.Glob(filepath.Join(wsFramesDir, "*.json"))
		if len(onDisk) != len(frames) {
			t.Errorf("testdata has %d frames, test produces %d", len(onDisk), len(frames))
		}
	}
}
