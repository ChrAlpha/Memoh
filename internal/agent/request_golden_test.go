package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/decide"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	"github.com/felinics/memoh/internal/agent/web"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// driveWebSend runs a Web chat send the way the WebSocket handler hands it to
// the application.
func driveWebSend(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t, driveRequestRuntime(t))
	if _, err := streamWebSend(context.Background(), t, fixture.service, runreq.ChatRequest{
		BotID:           agenttest.LifecycleBotID,
		ChatID:          agenttest.LifecycleBotID,
		ThreadID:        agenttest.LifecycleSessionID,
		Query:           directLifecyclePrompt,
		RawQuery:        directLifecyclePrompt,
		UserVisibleText: directLifecyclePrompt,
		CurrentChannel:  "local",
	}); err != nil {
		t.Fatalf("Web send failed: %v", err)
	}
	return requireDriveRequests(t, recorder)
}

// driveIMStartTurn runs an inbound IM message through the turn port.
func driveIMStartTurn(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t, driveRequestRuntime(t))
	handle, err := fixture.service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:           1,
		TeamID:                  "drive-request-team",
		Mode:                    turn.ModeChat,
		BotID:                   agenttest.LifecycleBotID,
		ChatID:                  agenttest.LifecycleBotID,
		ThreadID:                agenttest.LifecycleSessionID,
		Query:                   directLifecyclePrompt,
		UserVisibleText:         directLifecyclePrompt,
		ExternalMessageID:       "im-message-1",
		SourceChannelIdentityID: "im-sender-1",
		DisplayName:             "IM Sender",
		CurrentChannel:          "telegram",
		Channels:                []string{"telegram"},
		ReplyTarget:             "im-chat-1",
		ConversationType:        "private",
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	drainTurn(t, handle)
	return requireDriveRequests(t, recorder)
}

// runWebReplacement admits a Web retry or edit and runs it to its end.
func runWebReplacement(t *testing.T, fixture directLifecycleFixture, run func(*web.Admission, drive.RunDelivery)) {
	t.Helper()
	admission, err := fixture.service.AdmitWebRun(context.Background(), web.AdmitInput{
		BotID:        agenttest.LifecycleBotID,
		SessionID:    agenttest.LifecycleSessionID,
		InvocationID: "web-replacement",
		Payload:      []byte(`{"kind":"replacement"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun() error = %v", err)
	}
	delivery := newWebTestDelivery()
	run(admission, delivery)
	if _, ended := delivery.wait(t); ended.Failure != nil {
		t.Fatalf("replacement run failed: %v", ended.Failure)
	}
}

// driveWebRetry regenerates the latest answer from the stored request.
func driveWebRetry(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t, driveRequestRuntime(t), withFixtureMessages(driveReplaceableHistory(t)))
	runWebReplacement(t, fixture, func(admission *web.Admission, delivery drive.RunDelivery) {
		fixture.service.RunWebRetry(admission, web.RetryLatestMessageInput{
			BotID:        agenttest.LifecycleBotID,
			SessionID:    agenttest.LifecycleSessionID,
			TargetTurnID: driveReplacedTurnID,
		}, delivery)
	})
	return requireDriveRequests(t, recorder)
}

// driveWebEdit replaces the latest request with new text.
func driveWebEdit(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t, driveRequestRuntime(t), withFixtureMessages(driveReplaceableHistory(t)))
	runWebReplacement(t, fixture, func(admission *web.Admission, delivery drive.RunDelivery) {
		fixture.service.RunWebEdit(admission, web.EditLatestMessageInput{
			BotID:        agenttest.LifecycleBotID,
			SessionID:    agenttest.LifecycleSessionID,
			TargetTurnID: driveReplacedTurnID,
			Text:         "the edited question",
		}, delivery)
	})
	return requireDriveRequests(t, recorder)
}

// awaitDriveLifecycle waits for the run's context lifecycle terminal, which
// is written once the run has ended, for entries that drain their own run.
func awaitDriveLifecycle(t *testing.T, fixture directLifecycleFixture) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(fixture.lifecycles.creates()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// driveFollowUp queues an IM message while the session is busy and starts it
// from the queue once the running turn ends.
func driveFollowUp(t *testing.T) []sdk.Request {
	t.Helper()
	queue := &memoryQueuedInputs{}
	fixture, recorder := newDriveRequestFixture(t, withFixtureOptions(withAllowedTeam(driveTeamID), withQueue(queue)))
	key := sessionruntime.Key{BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID}

	ctx := context.Background()
	payload, err := turnwire.NewQueuedInput(turn.StartTurnCommand{
		SchemaVersion:           1,
		TeamID:                  driveTeamID,
		Mode:                    turn.ModeChat,
		BotID:                   agenttest.LifecycleBotID,
		ChatID:                  agenttest.LifecycleBotID,
		ThreadID:                agenttest.LifecycleSessionID,
		Query:                   directLifecyclePrompt,
		UserVisibleText:         directLifecyclePrompt,
		IdempotencyKey:          "im-message-2",
		ExternalMessageID:       "im-message-2",
		SourceChannelIdentityID: "im-sender-1",
		UserID:                  "im-user-1",
		DisplayName:             "IM Sender",
		CurrentChannel:          "telegram",
		Channels:                []string{"telegram"},
		ReplyTarget:             "im-chat-1",
		ConversationType:        "private",
	}, nil, time.Time{}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, sessionruntime.QueueEnqueue{
		Key: key, Origin: sessionruntime.QueueOriginFollowUp, InvocationID: "im-message-2", Payload: payload,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// A started batch is a run that holds the session, which admission reports as busy.
	if admission, err := fixture.service.pending.CheckAndAdmitDiscuss(ctx, key.BotID, key.SessionID); err != nil || admission != pending.DiscussBusy {
		t.Fatalf("CheckAndAdmitDiscuss() = %v, %v, want a started batch", admission, err)
	}
	awaitDriveLifecycle(t, fixture)
	return requireDriveRequests(t, recorder)
}

// driveResume continues a run the previous process was stopped in the middle
// of, from the intent it saved.
func driveResume(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t, withFixtureOptions(withAllowedTeam(driveTeamID)), withFixtureMessages(&resumeTurnMessages{requestMessageID: driveRequestMessageID}))
	raw, err := json.Marshal(map[string]any{"resume": admit.ResumeContext{
		Version:           1,
		Query:             directLifecyclePrompt,
		ChatID:            agenttest.LifecycleBotID,
		UserID:            "im-user-1",
		ChannelIdentityID: "im-sender-1",
		CurrentChannel:    "telegram",
		ReplyTarget:       "im-chat-1",
		ConversationType:  "private",
	}})
	if err != nil {
		t.Fatal(err)
	}
	done, err := startContinuation(context.Background(), fixture.service, sqlc.SessionRun{
		TeamID:    db.ParseUUIDOrEmpty(driveTeamID),
		RunID:     db.ParseUUIDOrEmpty(driveInterruptedRunID),
		BotID:     db.ParseUUIDOrEmpty(agenttest.LifecycleBotID),
		SessionID: db.ParseUUIDOrEmpty(agenttest.LifecycleSessionID),
		TurnID:    db.ParseUUIDOrEmpty(driveReplacedTurnID),
		InputJson: raw,
	})
	if err != nil {
		t.Fatalf("resumeInterruptedSession() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("resumed run did not finish")
	}
	return requireDriveRequests(t, recorder)
}

// driveSchedule fires a scheduled task on a session the built-in agent runs.
func driveSchedule(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t)
	result, err := fixture.service.TriggerSchedule(context.Background(), agenttest.LifecycleBotID, driveSchedulePayload("schedule-owner"), "")
	if err != nil {
		t.Fatalf("TriggerSchedule() error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("TriggerSchedule() status = %q, want ok", result.Status)
	}
	return requireDriveRequests(t, recorder)
}

// runDecisionContinuation runs a decision continuation with the caller's
// event channel as its end, so the resumed run can still deliver a question.
func runDecisionContinuation(t *testing.T, run func(chan<- runreq.WSStreamEvent) error) {
	t.Helper()
	eventCh := make(chan runreq.WSStreamEvent, 64)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range eventCh {
		}
	}()
	err := run(eventCh)
	close(eventCh)
	<-drained
	if err != nil {
		t.Fatalf("continuation error = %v", err)
	}
}

// driveApprovalContinuation continues a run after its tool call was approved
// and executed. The approved call runs before the answer arrives, so the
// continuation's job is only to store the result it produced as the call's
// answer and resume the model from it. The run supplies that result rather
// than executing the call again: the fixture's model is offered no
// container_exec tool, and replaying the call is what the approval path exists
// to avoid.
func driveApprovalContinuation(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t, withFixtureMessages(driveDecisionHistory(t, "container_exec", sdk.TextOutput("command finished"))))
	approval := toolapproval.Request{
		ID: "tool-approval", BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID,
		ToolCallID: driveToolCallID, ToolName: "container_exec",
		SourcePlatform: "telegram", ReplyTarget: "im-chat-1", ConversationType: "private",
	}
	input := decide.ToolApprovalResponseInput{BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID}
	result := sdk.ToolResultPart{
		ToolCallID: approval.ToolCallID, ToolName: approval.ToolName,
		Result: sdk.TextOutput("command finished"),
	}
	call := fixture.service.decider.ToolApprovalCall(decide.NewCommittedToolApproval(approval, input, false))
	call.Verdict, call.Result = decide.VerdictAnswered, result
	cont := fixture.service.answer.DecidedContinuation(context.Background(), call, true)
	runDecisionContinuation(t, func(eventCh chan<- runreq.WSStreamEvent) error {
		return fixture.service.answer.DriveUnrouted(context.Background(), cont, eventCh)
	})
	return requireDriveRequests(t, recorder)
}

// driveAskUserContinuation continues a run after the user answered its
// question. The answer comes back on the committed request the user input
// service resolved, so the continuation stores it as the ask_user call's
// result and runs the model on from there; storing it as a new user message
// instead would schedule a memory extraction the answer never asked for.
func driveAskUserContinuation(t *testing.T) []sdk.Request {
	t.Helper()
	answered := userinput.Request{
		ID: "user-input", BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID,
		ToolCallID: driveToolCallID, ToolName: "ask_user",
		SourcePlatform: "telegram", ReplyTarget: "im-chat-1", ConversationType: "private",
		Result: map[string]any{"answers": []any{map[string]any{"text": "blue"}}},
	}
	fixture, recorder := newDriveRequestFixture(t,
		withFixtureMessages(driveDecisionHistory(t, "ask_user", sdk.TextOutput(`{"answers":[{"text":"blue"}]}`))),
		withFixtureOptions(withDecide(func(d *decide.Deps) { d.UserInput = &agenttest.UserInput{Target: answered, Resolved: answered} })))
	committed, err := fixture.service.decider.CommitUserInputResponse(context.Background(), decide.UserInputResponseInput{
		BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID, UserInputID: answered.ID,
	})
	if err != nil {
		t.Fatalf("CommitUserInputResponse() error = %v", err)
	}
	runDecisionContinuation(t, func(eventCh chan<- runreq.WSStreamEvent) error {
		return fixture.service.ContinueCommittedUserInputResponse(context.Background(), committed, eventCh)
	})
	return requireDriveRequests(t, recorder)
}

type driveRequestEntry struct {
	name  string
	drive func(t *testing.T) []sdk.Request
}

var driveRequestEntries = []driveRequestEntry{
	{name: "web_send", drive: driveWebSend},
	{name: "web_retry", drive: driveWebRetry},
	{name: "web_edit", drive: driveWebEdit},
	{name: "im_start_turn", drive: driveIMStartTurn},
	{name: "follow_up", drive: driveFollowUp},
	{name: "resume", drive: driveResume},
	{name: "schedule", drive: driveSchedule},
	{name: "approval_continuation", drive: driveApprovalContinuation},
	{name: "ask_user_continuation", drive: driveAskUserContinuation},
}

// TestDriveRequestGolden pins the model requests each entry produces.
func TestDriveRequestGolden(t *testing.T) {
	for _, entry := range driveRequestEntries {
		t.Run(entry.name, func(t *testing.T) {
			assertDriveGolden(t, "drive-requests", entry.name, marshalDriveRequests(t, entry.drive(t)))
		})
	}
}

// marshalDrivePrompts renders the prompts one run gave its runtime, without
// the event sink and the steering port: both are callbacks into this process,
// not something the runtime is told.
func marshalDrivePrompts(t *testing.T, inputs []external.PromptInput) []byte {
	t.Helper()
	rendered := make([]map[string]any, 0, len(inputs))
	for _, input := range inputs {
		input.Sink = nil
		input.Steering = nil
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatalf("marshal prompt input: %v", err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("decode prompt input: %v", err)
		}
		delete(fields, "Sink")
		delete(fields, "Steering")
		rendered = append(rendered, fields)
	}
	got, err := json.MarshalIndent(rendered, "", "  ")
	if err != nil {
		t.Fatalf("marshal prompt inputs: %v", err)
	}
	got = driveRequestHeaderTime.ReplaceAll(got, []byte(`t=\"<time>\"`))
	return append(got, '\n')
}

func drivePromptWebSend(t *testing.T) []external.PromptInput {
	t.Helper()
	service, recorder := newDrivePromptFixture(t)
	if _, err := streamWebSend(context.Background(), t, service, runreq.ChatRequest{
		BotID:           agenttest.LifecycleBotID,
		ChatID:          agenttest.LifecycleBotID,
		ThreadID:        agenttest.LifecycleSessionID,
		Query:           directLifecyclePrompt,
		RawQuery:        directLifecyclePrompt,
		UserVisibleText: directLifecyclePrompt,
		CurrentChannel:  "local",
	}); err != nil {
		t.Fatalf("Web send failed: %v", err)
	}
	return recorder.recorded(t)
}

func drivePromptIMStartTurn(t *testing.T) []external.PromptInput {
	t.Helper()
	service, recorder := newDrivePromptFixture(t)
	handle, err := service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:           1,
		TeamID:                  driveTeamID,
		Mode:                    turn.ModeChat,
		BotID:                   agenttest.LifecycleBotID,
		ChatID:                  agenttest.LifecycleBotID,
		ThreadID:                agenttest.LifecycleSessionID,
		Query:                   directLifecyclePrompt,
		UserVisibleText:         directLifecyclePrompt,
		ExternalMessageID:       "im-message-1",
		SourceChannelIdentityID: "im-sender-1",
		UserID:                  "user-1",
		DisplayName:             "IM Sender",
		CurrentChannel:          "telegram",
		Channels:                []string{"telegram"},
		ReplyTarget:             "im-chat-1",
		ConversationType:        "private",
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	drainTurn(t, handle)
	return recorder.recorded(t)
}

func drivePromptSchedule(t *testing.T) []external.PromptInput {
	t.Helper()
	service, recorder := newDrivePromptFixture(t)
	result, err := service.TriggerSchedule(context.Background(), agenttest.LifecycleBotID, driveSchedulePayload("user-1"), "")
	if err != nil {
		t.Fatalf("TriggerSchedule() error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("TriggerSchedule() status = %q, want ok", result.Status)
	}
	return recorder.recorded(t)
}

type drivePromptEntry struct {
	name  string
	drive func(t *testing.T) []external.PromptInput
}

var drivePromptEntries = []drivePromptEntry{
	{name: "web_send", drive: drivePromptWebSend},
	{name: "im_start_turn", drive: drivePromptIMStartTurn},
	{name: "schedule", drive: drivePromptSchedule},
}

// TestDriveRequestGoldenExternalPrompts pins what an External Agent runtime is
// told for each entry that can run on one.
func TestDriveRequestGoldenExternalPrompts(t *testing.T) {
	for _, entry := range drivePromptEntries {
		t.Run(entry.name, func(t *testing.T) {
			assertDriveGolden(t, "drive-prompt-inputs", entry.name, marshalDrivePrompts(t, entry.drive(t)))
		})
	}
}
