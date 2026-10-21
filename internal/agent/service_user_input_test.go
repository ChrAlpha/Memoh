package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/answer"
	"github.com/felinics/memoh/internal/agent/decide"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/drivetest"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/bots"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
	sessiontest "github.com/felinics/memoh/internal/testutil/sessionruntime"
)

const testACPUserInputOwnerID = "owner-user"

func chatResolvedRequest() userinput.Request {
	return userinput.Request{
		ID:         "input-1",
		SessionID:  "session-1",
		ToolCallID: "call-1",
		ToolName:   userinput.ToolNameAskUser,
		Status:     userinput.StatusSubmitted,
		Result: map[string]any{
			"status": userinput.StatusSubmitted,
			"answers": []any{
				map[string]any{"question_id": "q1", "selected": []any{map[string]any{"id": "q1.o1", "label": "Plan A"}}},
			},
		},
	}
}

// The continuation path (prepareContinuationRunConfig) builds its outgoing
// messages via runreq.RepairToolCallClosures(runreq.NonNilModelMessages(runreq.SanitizeMessages(...))).
// This asserts that composition closes an orphaned older ask_user call while
// preserving the current call's real result — the reason a process restart
// mid-ask_user no longer breaks strict assistant-tool adjacency.
func TestUserInputContinuationHistoryClosesOlderPendingCall(t *testing.T) {
	t.Parallel()

	oldCall := runreq.SDKMessagesToModelMessages([]sdk.Message{{
		Role: sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{sdk.ToolCallPart{
			ToolCallID: "old-call",
			ToolName:   userinput.ToolNameAskUser,
		}},
	}})[0]
	currentCall := runreq.SDKMessagesToModelMessages([]sdk.Message{{
		Role: sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{sdk.ToolCallPart{
			ToolCallID: "current-call",
			ToolName:   userinput.ToolNameAskUser,
		}},
	}})[0]
	currentResult := runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.ToolMessage(sdk.ToolResultPart{
		ToolCallID: "current-call",
		ToolName:   userinput.ToolNameAskUser,
		Result:     toolexec.OutputFromValue(map[string]any{"status": userinput.StatusSubmitted}),
	})})[0]

	input := runreq.NonNilModelMessages(runreq.SanitizeMessages([]runreq.ModelMessage{
		oldCall,
		{Role: "user", Content: runreq.NewTextContent("start another ask_user")},
		currentCall,
		currentResult,
	}))
	got := runreq.RepairToolCallClosures(input, runreq.SyntheticToolClosureError)
	if len(got) != 5 {
		t.Fatalf("history length = %d, want 5: %#v", len(got), got)
	}
	oldResults := runreq.ExtractToolResultParts(got[1])
	if len(oldResults) != 1 || oldResults[0].ToolCallID != "old-call" || !oldResults[0].IsError {
		t.Fatalf("old call closure = %#v", oldResults)
	}
	currentResults := runreq.ExtractToolResultParts(got[4])
	if len(currentResults) != 1 || currentResults[0].ToolCallID != "current-call" || currentResults[0].IsError {
		t.Fatalf("current result = %#v", currentResults)
	}
}

func collectAgentStreamEvents(t *testing.T, ch <-chan runreq.WSStreamEvent, count int) []native.StreamEvent {
	t.Helper()
	events := make([]native.StreamEvent, 0, count)
	timeout := time.After(2 * time.Second)
	for len(events) < count {
		select {
		case raw := <-ch:
			var ev native.StreamEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				t.Fatalf("unmarshal stream event: %v", err)
			}
			events = append(events, ev)
		case <-timeout:
			t.Fatalf("timed out waiting for stream event %d/%d", len(events)+1, count)
		}
	}
	return events
}

// acpUserInputAuthDeps is the Deps of a service whose session is an ACP session
// owned by testACPUserInputOwnerID, who may execute in the workspace.
func acpUserInputAuthDeps() Deps {
	return Deps{
		BotPermissions: &agenttest.PermissionChecker{
			Values: map[string]bool{
				"bot-1:" + testACPUserInputOwnerID + ":" + bots.PermissionWorkspaceExec: true,
			},
		},
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{
					ID:          sessionID,
					BotID:       "bot-1",
					Type:        session.TypeACPAgent,
					RuntimeType: session.RuntimeACPAgent,
					RuntimeMetadata: map[string]any{
						"runtime_owner_account_id": testACPUserInputOwnerID,
					},
				}, nil
			},
		},
	}
}

func TestRespondUserInputContinuesChatSession(t *testing.T) {
	t.Parallel()

	fake := &agenttest.UserInput{
		Target:   userinput.Request{ID: "input-1", Status: userinput.StatusPending},
		Resolved: chatResolvedRequest(),
	}
	var continued *sdk.ToolResultPart
	resolver := newTestService(t, Deps{}, withDecide(func(d *decide.Deps) { d.UserInput = fake }), withAnswer(func(d *answer.Deps) {
		d.Continuer = continueFunc(func(_ context.Context, call decide.DecidedCall, _ runreq.ChatRequest, _ bool) (drive.RunPlan, error) {
			if call.DecisionID != "input-1" {
				t.Errorf("continued call = %#v", call)
			}
			result := call.Result
			continued = &result
			return completedContinuation(), nil
		})
	}))

	eventCh := make(chan runreq.WSStreamEvent, 4)
	answers := []userinput.QuestionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1.o1"}}}
	err := resolver.answer.RespondUserInput(context.Background(), decide.UserInputResponseInput{
		BotID:    "bot-1",
		ThreadID: "session-1",
		Answers:  answers,
	}, eventCh)
	if err != nil {
		t.Fatalf("respond user input: %v", err)
	}

	if fake.SubmitCalls != 1 || fake.CancelCalls != 0 {
		t.Fatalf("submit/cancel calls = %d/%d", fake.SubmitCalls, fake.CancelCalls)
	}
	if fake.Submitted.RequestID != "input-1" || len(fake.Submitted.Answers) != 1 || fake.Submitted.Answers[0].QuestionID != "q1" {
		t.Fatalf("submitted input = %#v", fake.Submitted)
	}
	if continued == nil {
		t.Fatal("chat request must continue the session")
	}
	if continued.ToolCallID != "call-1" || continued.ToolName != userinput.ToolNameAskUser {
		t.Fatalf("continued tool result = %#v", continued)
	}
	// The caller follows the resumed run, which reports its own ending; an
	// acknowledgement would open with agent_start instead.
	close(eventCh)
	var delivered []native.StreamEvent
	for raw := range eventCh {
		var event native.StreamEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("decode delivered event %s: %v", raw, err)
		}
		delivered = append(delivered, event)
	}
	if len(delivered) != 1 || delivered[0].Type != native.EventAgentEnd {
		t.Fatalf("chat continuation delivered %#v, want the resumed run's terminal and no ack", delivered)
	}
}

func TestRuntimeUserInputCommandCommitsAndResumesSameRun(t *testing.T) {
	t.Parallel()

	const (
		botID     = "bot-1"
		sessionID = "session-1"
		runID     = "55555555-5555-4555-8555-555555555555"
		inputID   = "input-1"
	)
	fake := &agenttest.UserInput{
		Target: userinput.Request{
			ID:         inputID,
			BotID:      botID,
			SessionID:  sessionID,
			ToolCallID: "call-1",
			ToolName:   userinput.ToolNameAskUser,
			Status:     userinput.StatusPending,
		},
		Resolved: chatResolvedRequest(),
	}
	releaseContinuation := make(chan struct{})
	continuationStarted := make(chan struct{})
	var continuationStartedOnce sync.Once
	manager := sessiontest.New(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-1",
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Second,
		CommandAckTTL: time.Second,
	})
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("start runtime manager: %v", err)
	}
	resolver := newTestService(t, Deps{SessionRuntime: manager, Messages: &agenttest.RecordingMessages{RunRequest: &messagepkg.RunTurnRequest{MessageID: "request-message-id", TurnID: "request-turn-id"}}}, withManagerEvents(manager), withDecide(func(d *decide.Deps) { d.UserInput = fake }), withAnswer(func(d *answer.Deps) {
		d.Continuer = continueFunc(func(context.Context, decide.DecidedCall, runreq.ChatRequest, bool) (drive.RunPlan, error) {
			continuationStartedOnce.Do(func() { close(continuationStarted) })
			return drive.RunPlan{Exec: runFuncExecutor(func(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
				select {
				case <-releaseContinuation:
				case <-ctx.Done():
					return drive.ExecEnding{Cause: ctx.Err(), Phase: outcome.PhaseExecution}
				}
				emit(native.StreamEvent{Type: native.EventAgentStart})
				return drive.ExecEnding{Terminal: &native.StreamEvent{Type: native.EventAgentEnd}}
			})}, nil
		})
	}))
	if _, err := sessiontest.Start(context.Background(), manager,
		botID,
		sessionID,
		runID,
		make(chan struct{}, 1),
		func() {},
		make(chan turn.InjectMessage, 1),
	); err != nil {
		t.Fatalf("start runtime run: %v", err)
	}
	handle := sessionruntime.RunHandle{
		BotID:     botID,
		SessionID: sessionID,
		RunID:     runID,
	}
	snapshot, err := manager.Snapshot(context.Background(), botID, sessionID)
	if err != nil || snapshot.CurrentRunView == nil {
		t.Fatalf("load runtime run: %#v, %v", snapshot.CurrentRunView, err)
	}
	handle.Generation = snapshot.CurrentRunView.Generation
	if _, err := manager.HandleAgentEvent(context.Background(), handle, native.StreamEvent{
		Type:        native.EventUserInputRequest,
		ToolName:    userinput.ToolNameAskUser,
		ToolCallID:  "call-1",
		UserInputID: inputID,
		Status:      "pending",
	}); err != nil {
		t.Fatalf("publish user input request: %v", err)
	}

	payload, err := json.Marshal(decide.UserInputResponseInput{
		BotID:       botID,
		ThreadID:    sessionID,
		UserInputID: inputID,
		ExplicitID:  inputID,
		Answers:     []userinput.QuestionAnswer{{QuestionID: "q1", Text: "continue"}},
	})
	if err != nil {
		t.Fatalf("encode response: %v", err)
	}
	err = resolver.answer.HandleCommand(context.Background(), sessionruntime.Command{
		ID: "control-early-ack", Type: sessionruntime.CommandUserInputResponse,
		BotID: botID, SessionID: sessionID, RunID: runID,
		Generation: handle.Generation, TargetID: inputID, Payload: payload,
	})
	if err != nil {
		t.Fatalf("handle decision command: %v", err)
	}
	if fake.SubmitCalls != 1 {
		t.Fatalf("submit calls = %d, want 1 before acknowledgement", fake.SubmitCalls)
	}

	snapshot, err = manager.Snapshot(context.Background(), botID, sessionID)
	if err != nil || snapshot.CurrentRunView == nil {
		t.Fatalf("load acknowledged decision snapshot: %#v, %v", snapshot.CurrentRunView, err)
	}
	if snapshot.CurrentRunView.Status != sessionruntime.RunStatusWaitingDecision {
		t.Fatalf("run status before continuation = %q, want waiting_decision", snapshot.CurrentRunView.Status)
	}
	var projectedStatus string
	var projectedCanRespond bool
	for _, message := range snapshot.CurrentRunView.Messages {
		if message.UserInput != nil && message.UserInput.UserInputID == inputID {
			projectedStatus = message.UserInput.Status
			projectedCanRespond = message.UserInput.CanRespond
			break
		}
	}
	if projectedStatus != userinput.StatusSubmitted || projectedCanRespond {
		t.Fatalf("acknowledged decision projection = status:%q can_respond:%v, want submitted/false", projectedStatus, projectedCanRespond)
	}

	select {
	case <-continuationStarted:
		t.Fatal("continuation started before the deferred producer finished persistence")
	case <-time.After(25 * time.Millisecond):
	}
	if _, err := manager.FinishRun(context.Background(), handle, ""); err != nil {
		t.Fatalf("park deferred producer: %v", err)
	}
	select {
	case <-continuationStarted:
	case <-time.After(time.Second):
		t.Fatal("continuation did not start after the deferred producer finished")
	}
	close(releaseContinuation)
	deadline := time.Now().Add(time.Second)
	for {
		snapshot, err = manager.Snapshot(context.Background(), botID, sessionID)
		if err != nil {
			t.Fatalf("load completed run: %v", err)
		}
		if snapshot.CurrentRunView != nil && snapshot.CurrentRunView.Status == sessionruntime.RunStatusCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resumed run did not complete: %#v", snapshot.CurrentRunView)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRespondUserInputLimitsChatToolResult(t *testing.T) {
	t.Parallel()

	large := "HEAD\n" + strings.Repeat("answer detail ", 300) + "\nTAIL"
	resolved := chatResolvedRequest()
	resolved.Result = map[string]any{
		"status": userinput.StatusSubmitted,
		"answers": []any{
			map[string]any{"question_id": "q1", "text": large},
		},
		"instruction": "Continue with the answer.",
	}
	fake := &agenttest.UserInput{
		Target:   userinput.Request{ID: "input-1", Status: userinput.StatusPending},
		Resolved: resolved,
	}
	var continued *sdk.ToolResultPart
	resolver := newTestService(t, Deps{Agent: native.New(native.Deps{Limits: native.Limits{ToolOutputMaxBytes: 512, ToolOutputMaxLines: 80}})}, withDecide(func(d *decide.Deps) { d.UserInput = fake }), withAnswer(func(d *answer.Deps) {
		d.Continuer = continueFunc(func(_ context.Context, call decide.DecidedCall, _ runreq.ChatRequest, _ bool) (drive.RunPlan, error) {
			result := call.Result
			continued = &result
			return completedContinuation(), nil
		})
	}))

	err := resolver.answer.RespondUserInput(context.Background(), decide.UserInputResponseInput{
		BotID:    "bot-1",
		ThreadID: "session-1",
		Answers:  []userinput.QuestionAnswer{{QuestionID: "q1", Text: large}},
	}, nil)
	if err != nil {
		t.Fatalf("respond user input: %v", err)
	}
	if continued == nil {
		t.Fatal("chat request must continue the session")
	}
	result, ok := toolexec.OutputValue(continued.Result).(map[string]any)
	if !ok {
		t.Fatalf("continued result = %#v, want map", continued.Result)
	}
	answers, ok := result["answers"].([]any)
	if !ok || len(answers) != 1 {
		t.Fatalf("continued answers = %#v, want one answer", result["answers"])
	}
	answer, ok := answers[0].(map[string]any)
	if !ok {
		t.Fatalf("continued answer = %#v, want map", answers[0])
	}
	text, ok := answer["text"].(string)
	if !ok {
		t.Fatalf("continued answer text = %#v, want string", answer["text"])
	}
	if len(text) >= len(large) {
		t.Fatalf("answer text was not pruned: got %d bytes, original %d", len(text), len(large))
	}
	if !strings.Contains(text, "[memoh pruned]") {
		t.Fatalf("answer text missing prune marker:\n%s", text)
	}
}

func TestRespondUserInputOnlyAcksACPRequests(t *testing.T) {
	t.Parallel()

	resolved := chatResolvedRequest()
	resolved.ProviderMetadata = map[string]any{"source": userinput.ProviderSourceACPMCP}
	fake := &agenttest.UserInput{
		Target:            userinput.Request{ID: "input-1", Status: userinput.StatusPending, ProviderMetadata: map[string]any{"source": userinput.ProviderSourceACPMCP}},
		Resolved:          resolved,
		RespondAllowed:    true,
		RespondAllowedSet: true,
	}
	resolver := newTestService(t, acpUserInputAuthDeps(), withDecide(func(d *decide.Deps) { d.UserInput = fake }), withAnswer(func(d *answer.Deps) {
		d.Continuer = continueFunc(func(context.Context, decide.DecidedCall, runreq.ChatRequest, bool) (drive.RunPlan, error) {
			t.Error("ACP request must not continue the chat session; the blocked waiter resumes it")
			return completedContinuation(), nil
		})
	}))

	eventCh := make(chan runreq.WSStreamEvent, 4)
	err := resolver.answer.RespondUserInput(context.Background(), decide.UserInputResponseInput{
		BotID:       "bot-1",
		ThreadID:    "session-1",
		ActorUserID: testACPUserInputOwnerID,
		Answers:     []userinput.QuestionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1.o1"}}},
	}, eventCh)
	if err != nil {
		t.Fatalf("respond user input: %v", err)
	}
	if fake.SubmitCalls != 1 {
		t.Fatalf("submit calls = %d", fake.SubmitCalls)
	}
	// runreq.EmitApprovalAck sends agent start + end so the client stream settles.
	if len(eventCh) != 2 {
		t.Fatalf("ack events = %d, want 2", len(eventCh))
	}
}

func TestRespondUserInputAcksAlreadyDecidedACPRequest(t *testing.T) {
	t.Parallel()

	fake := &agenttest.UserInput{
		Target: userinput.Request{
			ID:               "input-1",
			Status:           userinput.StatusPending,
			ProviderMetadata: map[string]any{"source": userinput.ProviderSourceACPMCP},
		},
		SubmitErr:         userinput.ErrAlreadyDecided,
		RespondAllowed:    true,
		RespondAllowedSet: true,
	}
	resolver := newTestService(t, acpUserInputAuthDeps(), withDecide(func(d *decide.Deps) { d.UserInput = fake }), withAnswer(func(d *answer.Deps) {
		d.Continuer = continueFunc(func(context.Context, decide.DecidedCall, runreq.ChatRequest, bool) (drive.RunPlan, error) {
			t.Error("already-decided ACP request must not continue the chat session")
			return completedContinuation(), nil
		})
	}))

	eventCh := make(chan runreq.WSStreamEvent, 4)
	err := resolver.answer.RespondUserInput(context.Background(), decide.UserInputResponseInput{
		BotID:       "bot-1",
		ThreadID:    "session-1",
		ActorUserID: testACPUserInputOwnerID,
		Answers:     []userinput.QuestionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1.o1"}}},
	}, eventCh)
	if err != nil {
		t.Fatalf("respond user input: %v", err)
	}
	if fake.SubmitCalls != 1 || fake.CancelCalls != 0 {
		t.Fatalf("submit/cancel calls = %d/%d", fake.SubmitCalls, fake.CancelCalls)
	}
	if len(eventCh) != 2 {
		t.Fatalf("ack events = %d, want 2", len(eventCh))
	}
}

func TestRespondUserInputACPRequestSubmitsWithLiveWaiter(t *testing.T) {
	t.Parallel()

	resolved := chatResolvedRequest()
	resolved.ProviderMetadata = map[string]any{"source": userinput.ProviderSourceACPMCP}
	fake := &agenttest.UserInput{
		Target: userinput.Request{
			ID:               "input-1",
			Status:           userinput.StatusPending,
			ProviderMetadata: map[string]any{"source": userinput.ProviderSourceACPMCP},
		},
		Resolved:          resolved,
		RespondAllowed:    true,
		RespondAllowedSet: true,
	}
	resolver := newTestService(t, acpUserInputAuthDeps(), withDecide(func(d *decide.Deps) { d.UserInput = fake }), withAnswer(func(d *answer.Deps) {
		d.Continuer = continueFunc(func(context.Context, decide.DecidedCall, runreq.ChatRequest, bool) (drive.RunPlan, error) {
			t.Error("ACP request must not continue the session in this response handler")
			return completedContinuation(), nil
		})
	}))

	eventCh := make(chan runreq.WSStreamEvent, 4)
	err := resolver.answer.RespondUserInput(context.Background(), decide.UserInputResponseInput{
		BotID:       "bot-1",
		ThreadID:    "session-1",
		ActorUserID: testACPUserInputOwnerID,
		Answers:     []userinput.QuestionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1.o1"}}},
	}, eventCh)
	if err != nil {
		t.Fatalf("respond user input: %v", err)
	}
	if fake.SubmitCalls != 1 {
		t.Fatalf("submit calls = %d, want 1", fake.SubmitCalls)
	}
	if fake.CancelCalls != 0 {
		t.Fatalf("cancel calls = %d, want 0", fake.CancelCalls)
	}
	if len(eventCh) != 2 {
		t.Fatalf("ack events = %d, want 2", len(eventCh))
	}
}

func TestRespondUserInputACPRequestReattachesActivePrompt(t *testing.T) {
	t.Parallel()

	resolved := chatResolvedRequest()
	resolved.ProviderMetadata = map[string]any{"source": userinput.ProviderSourceACPMCP}
	submitted := make(chan struct{})
	fake := &agenttest.UserInput{
		Target: userinput.Request{
			ID:               "input-1",
			SessionID:        "session-1",
			ToolCallID:       "call-1",
			Status:           userinput.StatusPending,
			ProviderMetadata: map[string]any{"source": userinput.ProviderSourceACPMCP},
		},
		Resolved:          resolved,
		SubmitHook:        func() { close(submitted) },
		RespondAllowed:    true,
		RespondAllowedSet: true,
	}
	resolver := newTestService(t, acpUserInputAuthDeps(), withDecide(func(d *decide.Deps) { d.UserInput = fake }), withAnswer(func(d *answer.Deps) {
		d.Continuer = continueFunc(func(context.Context, decide.DecidedCall, runreq.ChatRequest, bool) (drive.RunPlan, error) {
			t.Error("ACP request must resume through the active ACP prompt")
			return completedContinuation(), nil
		})
	}))
	hub := resolver.activePrompts.Register("bot-1", "session-1")
	if hub == nil {
		t.Fatal("expected active ACP prompt hub")
	}
	defer resolver.activePrompts.Unregister("bot-1", "session-1", hub)

	eventCh := make(chan runreq.WSStreamEvent, 8)
	done := make(chan error, 1)
	go func() {
		done <- resolver.answer.RespondUserInput(context.Background(), decide.UserInputResponseInput{
			BotID:       "bot-1",
			ThreadID:    "session-1",
			ActorUserID: testACPUserInputOwnerID,
			Answers:     []userinput.QuestionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1.o1"}}},
		}, eventCh)
	}()

	select {
	case <-submitted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for submit")
	}
	hub.Emit(native.StreamEvent{
		Type:        native.EventUserInputRequest,
		ToolCallID:  "call-1",
		ToolName:    userinput.ToolNameAskUser,
		UserInputID: "input-1",
		Status:      userinput.StatusSubmitted,
	})
	hub.Emit(native.StreamEvent{
		Type:       native.EventToolCallEnd,
		ToolCallID: "call-1",
		ToolName:   userinput.ToolNameAskUser,
		Result:     map[string]any{"status": userinput.StatusSubmitted},
	})
	hub.Emit(native.StreamEvent{Type: native.EventTextDelta, Delta: "continuing"})
	hub.Emit(native.StreamEvent{Type: native.EventEnd})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("respond user input: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ACP prompt reattach")
	}

	events := collectAgentStreamEvents(t, eventCh, 3)
	if events[0].Type != native.EventStart {
		t.Fatalf("first event = %q, want %q", events[0].Type, native.EventStart)
	}
	if events[1].Type != native.EventTextDelta || events[1].Delta != "continuing" {
		t.Fatalf("second event = %#v, want text delta", events[1])
	}
	if events[2].Type != native.EventEnd {
		t.Fatalf("third event = %q, want %q", events[2].Type, native.EventEnd)
	}
	if fake.SubmitCalls != 1 || fake.CancelCalls != 0 {
		t.Fatalf("submit/cancel calls = %d/%d", fake.SubmitCalls, fake.CancelCalls)
	}
}

func TestRespondUserInputACPRequestCanSuppressActivePromptReattach(t *testing.T) {
	t.Parallel()

	resolved := chatResolvedRequest()
	resolved.ProviderMetadata = map[string]any{"source": userinput.ProviderSourceACPMCP}
	fake := &agenttest.UserInput{
		Target: userinput.Request{
			ID:               "input-1",
			SessionID:        "session-1",
			Status:           userinput.StatusPending,
			ProviderMetadata: map[string]any{"source": userinput.ProviderSourceACPMCP},
		},
		Resolved:          resolved,
		RespondAllowed:    true,
		RespondAllowedSet: true,
	}
	resolver := newTestService(t, acpUserInputAuthDeps(), withDecide(func(d *decide.Deps) { d.UserInput = fake }), withAnswer(func(d *answer.Deps) {
		d.Continuer = continueFunc(func(context.Context, decide.DecidedCall, runreq.ChatRequest, bool) (drive.RunPlan, error) {
			t.Error("ACP request must not continue the chat session")
			return completedContinuation(), nil
		})
	}))
	hub := resolver.activePrompts.Register("bot-1", "session-1")
	if hub == nil {
		t.Fatal("expected active ACP prompt hub")
	}
	defer resolver.activePrompts.Unregister("bot-1", "session-1", hub)

	eventCh := make(chan runreq.WSStreamEvent, 4)
	err := resolver.answer.RespondUserInput(context.Background(), decide.UserInputResponseInput{
		BotID:                      "bot-1",
		ThreadID:                   "session-1",
		ActorUserID:                testACPUserInputOwnerID,
		Answers:                    []userinput.QuestionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1.o1"}}},
		SuppressActivePromptAttach: true,
	}, eventCh)
	if err != nil {
		t.Fatalf("respond user input: %v", err)
	}
	if fake.SubmitCalls != 1 || fake.CancelCalls != 0 {
		t.Fatalf("submit/cancel calls = %d/%d", fake.SubmitCalls, fake.CancelCalls)
	}
	if len(eventCh) != 2 {
		t.Fatalf("ack events = %d, want 2", len(eventCh))
	}
}

func TestRespondUserInputACPRequestWithoutWaiterCancelsInsteadOfSubmitting(t *testing.T) {
	t.Parallel()

	resolved := chatResolvedRequest()
	resolved.ProviderMetadata = map[string]any{"source": userinput.ProviderSourceACPMCP}
	fake := &agenttest.UserInput{
		Target: userinput.Request{
			ID:               "input-1",
			Status:           userinput.StatusPending,
			ProviderMetadata: map[string]any{"source": userinput.ProviderSourceACPMCP},
		},
		Resolved: resolved,
		// The blocked waiter is gone (process restart, prompt settled).
		RespondAllowedSet: true,
		RespondAllowed:    false,
	}
	resolver := newTestService(t, acpUserInputAuthDeps(), withDecide(func(d *decide.Deps) { d.UserInput = fake }), withAnswer(func(d *answer.Deps) {
		d.Continuer = continueFunc(func(context.Context, decide.DecidedCall, runreq.ChatRequest, bool) (drive.RunPlan, error) {
			t.Error("orphaned ACP request must not continue the chat session")
			return completedContinuation(), nil
		})
	}))

	eventCh := make(chan runreq.WSStreamEvent, 4)
	err := resolver.answer.RespondUserInput(context.Background(), decide.UserInputResponseInput{
		BotID:       "bot-1",
		ThreadID:    "session-1",
		ActorUserID: testACPUserInputOwnerID,
		Answers:     []userinput.QuestionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1.o1"}}},
	}, eventCh)
	if err != nil {
		t.Fatalf("respond user input: %v", err)
	}
	if fake.SubmitCalls != 0 {
		t.Fatalf("submit calls = %d, want 0", fake.SubmitCalls)
	}
	if fake.CancelCalls != 1 {
		t.Fatalf("cancel calls = %d, want 1", fake.CancelCalls)
	}
	if fake.Canceled.RequestID != "input-1" || fake.Canceled.Reason == "" {
		t.Fatalf("canceled input = %#v", fake.Canceled)
	}
	if len(eventCh) != 2 {
		t.Fatalf("ack events = %d, want 2", len(eventCh))
	}
}

func TestRespondUserInputCancelRoutesToCancel(t *testing.T) {
	t.Parallel()

	fake := &agenttest.UserInput{
		Target:   userinput.Request{ID: "input-1", Status: userinput.StatusPending},
		Resolved: chatResolvedRequest(),
	}
	continueCalls := 0
	resolver := newTestService(t, Deps{}, withDecide(func(d *decide.Deps) { d.UserInput = fake }), withAnswer(func(d *answer.Deps) {
		d.Continuer = continueFunc(func(context.Context, decide.DecidedCall, runreq.ChatRequest, bool) (drive.RunPlan, error) {
			continueCalls++
			return completedContinuation(), nil
		})
	}))

	err := resolver.answer.RespondUserInput(context.Background(), decide.UserInputResponseInput{
		BotID:    "bot-1",
		ThreadID: "session-1",
		Canceled: true,
		Reason:   "user_canceled",
	}, nil)
	if err != nil {
		t.Fatalf("respond user input: %v", err)
	}
	if fake.CancelCalls != 1 || fake.SubmitCalls != 0 {
		t.Fatalf("cancel/submit calls = %d/%d", fake.CancelCalls, fake.SubmitCalls)
	}
	if fake.Canceled.RequestID != "input-1" || fake.Canceled.Reason != "user_canceled" {
		t.Fatalf("canceled input = %#v", fake.Canceled)
	}
	if continueCalls != 1 {
		t.Fatalf("canceled chat request must still continue the session, calls = %d", continueCalls)
	}
}

// completedContinuation is a resumed run that ends cleanly at once.
func completedContinuation() drive.RunPlan {
	return drive.RunPlan{Exec: &drivetest.ScriptedExecutor{Terminal: &native.StreamEvent{Type: native.EventAgentEnd}}}
}

// continueFunc is a continuer made of a function.
type continueFunc func(ctx context.Context, call decide.DecidedCall, req runreq.ChatRequest, live bool) (drive.RunPlan, error)

func (f continueFunc) Continue(ctx context.Context, call decide.DecidedCall, req runreq.ChatRequest, live bool) (drive.RunPlan, error) {
	return f(ctx, call, req, live)
}
