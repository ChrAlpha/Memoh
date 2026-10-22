package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
)

func TestStartTurnRequiresTeamID(t *testing.T) {
	a := newTurnTestService(t)
	_, err := a.StartTurn(context.Background(), turn.StartTurnCommand{Mode: turn.ModeChat})
	if err == nil {
		t.Fatal("want error for empty TeamID")
	}
}

// A chat turn's events reach its caller with the run's identity and a counter
// that starts at one and never repeats, and the turn ends with the terminal
// record its run wrote.
func TestStartTurnStreamsEvents(t *testing.T) {
	fixture := newChatTurnFixture(t, directLifecycleModelSuccess)
	handle := fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "streams-events-1"})
	events, _ := drainTurn(t, handle)

	if len(events) == 0 {
		t.Fatal("the run delivered no events")
	}
	for i, event := range events {
		if event.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d; the port indexes events from one", i, event.Seq, i+1)
		}
		if event.RunID != handle.RunID() || event.TeamID != "team-1" || event.ThreadID != agenttest.LifecycleSessionID {
			t.Fatalf("event %d envelope = %+v, want the run's identity", i, event)
		}
	}
	// The run's own output reached the caller, not a synthesized stand-in.
	if !strings.Contains(string(events[len(events)-1].Payload), turn.RunStateCompleted) {
		t.Fatalf("last event = %s, want the completed run_terminal", events[len(events)-1].Payload)
	}
	if got := fixture.admitter.AwaitFinish(t).Status; got != sessionruntime.RunStatusCompleted {
		t.Fatalf("finish status = %q, want %q", got, sessionruntime.RunStatusCompleted)
	}
}

// A chat run's events reach the session projection as the runtime produced
// them, in order, and the run's own terminal is among them. The entry no
// longer synthesizes a terminal of its own: the runtime's terminal is the
// run's, and the state written for the run is the driver's reading of it.
func TestStartTurnPublishesNativeEventsBeforeCleanFinish(t *testing.T) {
	fixture := newChatTurnFixture(t, directLifecycleModelSuccess)
	handle := fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "native-events-1"})
	drainTurn(t, handle)

	published := fixture.admitter.PublishedEvents()
	if len(published) == 0 {
		t.Fatal("the run published nothing to the session projection")
	}
	var sawDelta, sawEnd bool
	for _, event := range published {
		if event.Type == native.EventTextDelta {
			sawDelta = true
		}
		if event.Type == native.EventAgentEnd {
			sawEnd = true
		}
	}
	if !sawDelta || !sawEnd {
		t.Fatalf("published events = %#v, want the runtime's output and its terminal", published)
	}
	if got := fixture.admitter.AwaitFinish(t).Status; got != sessionruntime.RunStatusCompleted {
		t.Fatalf("finish status = %q, want %q", got, sessionruntime.RunStatusCompleted)
	}
}

// A publication the runtime refuses fails the run: the failure is what the
// caller is told, and the run is cancelled rather than left publishing into a
// A publication the session projection refuses fails the run: the failure is
// what the caller is told, and the run's execution is cancelled rather than
// A publication the session projection refuses fails the run, and the run
// stops executing: continuing would produce events no subscriber can receive.
// The cause stays private; the caller is told the catalogued code.
func TestStartTurnFailsWhenRuntimeEventPublicationFails(t *testing.T) {
	fixture := newChatTurnFixture(t, directLifecycleModelHoldOnCancel)
	publishErr := errors.New("private runtime publication failure")
	fixture.admitter.PublishErr = publishErr

	handle := fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "publish-fail-1"})
	var publicErr error
	deadline := time.After(10 * time.Second)
	for eventCh, errCh := handle.Events(), handle.Errs(); eventCh != nil || errCh != nil; {
		select {
		case _, ok := <-eventCh:
			if !ok {
				eventCh = nil
			}
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			publicErr = err
		case <-deadline:
			t.Fatal("run did not end")
		}
	}
	fixture.releaseModel(t)
	if got := fixture.admitter.AwaitFinish(t).Status; got != sessionruntime.RunStatusErrored {
		t.Fatalf("publication failure status = %q, want %q", got, sessionruntime.RunStatusErrored)
	}
	if got := apperror.CodeOf(publicErr); got != apperror.CodeSessionPublishFailed {
		t.Fatalf("publication failure code = %q, want %q", got, apperror.CodeSessionPublishFailed)
	}
	// The refused publication keeps its own cause in the message, so an
	// operator reading the error can tell a projection outage from a failure
	// the run itself declared.
	if cause := apperror.CauseOf(publicErr); cause == nil || !strings.Contains(cause.Error(), publishErr.Error()) {
		t.Fatalf("publication cause = %v, want it to name %v", cause, publishErr)
	}
}

// A steer reaches the run while it executes, and stops being accepted once the
// run's execution has ended: a caller can tell a rejected steer from an
// accepted one, and a message nobody will read is not silently swallowed.
func TestInjectAndAssets(t *testing.T) {
	fixture := newChatTurnFixture(t, directLifecycleModelHoldOnCancel)
	handle := fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "inject-assets-1"})
	fixture.awaitModelRequest(t)
	if err := handle.Inject(context.Background(), turn.InjectMessage{Text: "more"}); err != nil {
		t.Fatalf("Inject() during the run = %v, want the steer accepted", err)
	}
	fixture.stopRun(t, handle)
	fixture.releaseModel(t)
	drainTurn(t, handle)
	if err := handle.Inject(context.Background(), turn.InjectMessage{Text: "late"}); !errors.Is(err, errRunAlreadyFinished) {
		t.Fatalf("Inject() after the run = %v, want %v", err, errRunAlreadyFinished)
	}
}

func TestCancelClosesEvents(t *testing.T) {
	a := newTurnTestService(t)
	h, err := a.StartTurn(context.Background(), turn.StartTurnCommand{TeamID: "t", Mode: turn.ModeChat})
	if err != nil {
		t.Fatal(err)
	}
	h.Cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return // closed as expected
			}
		case <-deadline:
			t.Fatal("events channel not closed after cancel")
		}
	}
}

// Cancelling a run stops it and the run's durable record names the stop: a run
// the caller ended is aborted, not failed. A run recorded as failed after its
// caller stopped it would misreport the turn, which is what this pins.
//
// The provider handler is deliberately left holding its response open: the
// stop must come from the caller's own signal rather than from the transport
// closing under the runtime.
func TestCancelWaitsForStreamCleanupBeforeFinalizingRun(t *testing.T) {
	fixture := newChatTurnFixture(t, directLifecycleModelHoldOnCancel)
	handle := fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "cancel-1"})
	fixture.awaitModelRequest(t)
	fixture.stopRun(t, handle)
	fixture.releaseModel(t)
	drainTurn(t, handle)
	finish := fixture.admitter.AwaitFinish(t)
	if finish.Status != sessionruntime.RunStatusAborted {
		t.Fatalf("finish status = %q, want %q", finish.Status, sessionruntime.RunStatusAborted)
	}
	if finish.Message != "" {
		t.Fatalf("aborted run recorded code %q, want none: a stop is not a failure", finish.Message)
	}
}

// The command's own fields reach the application's request unchanged. The
// translation is one function, so it is read directly; the run-related fields
// it deliberately does not carry are checked where a run wires them.
func TestCommandFieldTranslation(t *testing.T) {
	cmd := turn.StartTurnCommand{
		TeamID: "t", Mode: turn.ModeChat,
		BotID: "bot", ChatID: "chat", ThreadID: "sess", RouteID: "route",
		Token: "tok", ChatToken: "ctok", UserID: "u", SourceChannelIdentityID: "ci",
		DisplayName: "dn", ExternalMessageID: "ext", EventID: "ev",
		Query: "q", ModelQuery: "mq", UserMessageKind: "kind", UserVisibleText: "uvt",
		Attachments: []turn.Attachment{{Type: "image", ContentHash: "ch1", Name: "a.png"}},
		ReplyTarget: "rt", ConversationType: "group", ConversationName: "cn",
		SourceReplyToMessageID: "srm", ReplySender: "rs", ReplyPreview: "rp",
		ReplyAttachments: []turn.Attachment{{Type: "file"}},
		MentionsBot:      true, RepliesToBot: true,
		ForwardMessageID: "fm", ForwardFromUserID: "fu", ForwardFromConversationID: "fc",
		ForwardSender: "fs", ForwardDate: 42,
		CurrentChannel: "telegram", Channels: []string{"telegram"},
		Model: "m1", ReasoningEffort: "high", WorkspaceTargetID: "wt",
		SkillActivation:      &turn.SkillActivation{Prompt: "p", Skills: []turn.SkillActivationSkill{{Name: "sk"}}},
		RequestedSkills:      []turn.RequestedSkillContext{{Name: "rs1", ContentHash: "rh"}},
		SkipMemoryExtraction: true, SkipTitleGeneration: true, UserMessagePersisted: true,
	}
	got := chatRequestFromCommand(cmd)
	checks := map[string][2]string{
		"BotID":                     {got.BotID, "bot"},
		"ChatID":                    {got.ChatID, "chat"},
		"ThreadID":                  {got.ThreadID, "sess"},
		"RouteID":                   {got.RouteID, "route"},
		"Token":                     {got.Token, "tok"},
		"ChatToken":                 {got.ChatToken, "ctok"},
		"UserID":                    {got.UserID, "u"},
		"SourceChannelIdentityID":   {got.SourceChannelIdentityID, "ci"},
		"DisplayName":               {got.DisplayName, "dn"},
		"ExternalMessageID":         {got.ExternalMessageID, "ext"},
		"EventID":                   {got.EventID, "ev"},
		"Query":                     {got.Query, "q"},
		"ModelQuery":                {got.ModelQuery, "mq"},
		"UserMessageKind":           {got.UserMessageKind, "kind"},
		"UserVisibleText":           {got.UserVisibleText, "uvt"},
		"ReplyTarget":               {got.ReplyTarget, "rt"},
		"ConversationType":          {got.ConversationType, "group"},
		"ConversationName":          {got.ConversationName, "cn"},
		"SourceReplyToMessageID":    {got.SourceReplyToMessageID, "srm"},
		"ReplySender":               {got.ReplySender, "rs"},
		"ReplyPreview":              {got.ReplyPreview, "rp"},
		"ForwardMessageID":          {got.ForwardMessageID, "fm"},
		"ForwardFromUserID":         {got.ForwardFromUserID, "fu"},
		"ForwardFromConversationID": {got.ForwardFromConversationID, "fc"},
		"ForwardSender":             {got.ForwardSender, "fs"},
		"CurrentChannel":            {got.CurrentChannel, "telegram"},
		"Model":                     {got.Model, "m1"},
		"ReasoningEffort":           {got.ReasoningEffort, "high"},
		"WorkspaceTargetID":         {got.WorkspaceTargetID, "wt"},
	}
	for name, pair := range checks {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
	if !got.MentionsBot || !got.RepliesToBot || !got.SkipMemoryExtraction || !got.SkipTitleGeneration || !got.UserMessagePersisted {
		t.Error("bool fields not translated")
	}
	if got.ForwardDate != 42 {
		t.Errorf("ForwardDate = %d", got.ForwardDate)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].ContentHash != "ch1" || got.Attachments[0].Name != "a.png" {
		t.Errorf("Attachments = %+v", got.Attachments)
	}
	if len(got.ReplyAttachments) != 1 || got.ReplyAttachments[0].Type != "file" {
		t.Errorf("ReplyAttachments = %+v", got.ReplyAttachments)
	}
	if got.SkillActivation == nil || got.SkillActivation.Prompt != "p" || len(got.SkillActivation.Skills) != 1 {
		t.Errorf("SkillActivation = %+v", got.SkillActivation)
	}
	if len(got.RequestedSkills) != 1 || got.RequestedSkills[0].Name != "rs1" || got.RequestedSkills[0].ContentHash != "rh" {
		t.Errorf("RequestedSkills = %+v", got.RequestedSkills)
	}
	if len(got.Channels) != 1 || got.Channels[0] != "telegram" {
		t.Errorf("Channels = %+v", got.Channels)
	}
}

func TestBoundaryValuesPassThrough(t *testing.T) {
	metadata := map[string]any{"key": "value"}
	attachment := turn.Attachment{
		Type: "image", Base64: "base64", Path: "/tmp/a", URL: "https://example.test/a",
		PlatformKey: "platform", ContentHash: "hash", Name: "a.png", Mime: "image/png",
		Size: 42, Metadata: metadata,
	}
	activation := &turn.SkillActivation{
		Skills: []turn.SkillActivationSkill{{
			Name: "skill", DisplayName: "Skill", Description: "desc",
			SourceKind: "registry", State: "effective",
		}},
		Prompt: "prompt",
	}
	requested := []turn.RequestedSkillContext{{
		Name: "skill", Description: "desc", Content: "body", SourceKind: "registry",
		OpaqueSourceID: "opaque", ContentHash: "hash", Identity: "identity",
	}}
	request := chatRequestFromCommand(turn.StartTurnCommand{
		ReplyAttachments: []turn.Attachment{attachment},
		Attachments:      []turn.Attachment{attachment},
		SkillActivation:  activation,
		RequestedSkills:  requested,
	})
	if !reflect.DeepEqual(request.ReplyAttachments, []turn.Attachment{attachment}) {
		t.Errorf("reply attachments = %#v", request.ReplyAttachments)
	}
	if !reflect.DeepEqual(request.Attachments, []turn.Attachment{attachment}) {
		t.Errorf("attachments = %#v", request.Attachments)
	}
	if request.SkillActivation != activation {
		t.Error("skill activation pointer was not passed through")
	}
	if !reflect.DeepEqual(request.RequestedSkills, requested) {
		t.Errorf("requested skills = %#v, want %#v", request.RequestedSkills, requested)
	}

	answers := []turn.QuestionAnswer{{
		QuestionID: "question", OptionIDs: []string{"a"}, CustomText: "custom",
		Text: "text", Skipped: true,
	}}
	wantAnswers := []userinput.QuestionAnswer{{
		QuestionID: "question", OptionIDs: []string{"a"}, CustomText: "custom",
		Text: "text", Skipped: true,
	}}
	if got := questionAnswersToUserInput(answers); !reflect.DeepEqual(got, wantAnswers) {
		t.Errorf("question answer conversion = %#v, want %#v", got, wantAnswers)
	}
}

func TestStartTurnRejectsForeignTeam(t *testing.T) {
	a := newTurnTestService(t, withDeps(func(d *Deps) { d.AllowedTeam = "team-home" }))
	_, err := a.StartTurn(context.Background(), turn.StartTurnCommand{TeamID: "team-other", Mode: turn.ModeChat})
	if !errors.Is(err, turn.ErrTeamNotServed) {
		t.Fatalf("err = %v, want ErrTeamNotServed", err)
	}
	if _, err := a.StartTurn(context.Background(), turn.StartTurnCommand{TeamID: "team-home", Mode: turn.ModeChat}); err != nil {
		t.Fatalf("home team rejected: %v", err)
	}
}

// The platform's message identity must reach admission unchanged: it is what
// makes a webhook redelivery the same invocation instead of a second turn.
func TestStartTurnAdmitsUnderTheIdempotencyKey(t *testing.T) {
	a, admitter := newAdmittedTurnTestService(t)
	h, err := a.StartTurn(context.Background(), turn.StartTurnCommand{
		TeamID: "t", Mode: turn.ModeChat, BotID: "b", ThreadID: "s", IdempotencyKey: "telegram:route-1:42",
	})
	if err != nil {
		t.Fatal(err)
	}
	drainTurn(t, h)

	admitted := admitter.Admitted()
	if len(admitted) != 1 {
		t.Fatalf("admissions = %d, want 1", len(admitted))
	}
	if admitted[0].InvocationID != "telegram:route-1:42" {
		t.Fatalf("invocation id = %q, want the command's idempotency key", admitted[0].InvocationID)
	}
	if admitted[0].BotID != "b" || admitted[0].SessionID != "s" {
		t.Fatalf("admitted %q/%q, want b/s", admitted[0].BotID, admitted[0].SessionID)
	}
	if admitted[0].Mode != sessionmode.Chat {
		t.Fatalf("admitted mode = %q, want %q", admitted[0].Mode, sessionmode.Chat)
	}
	if h.RunID() != "run-1" {
		t.Fatalf("run id = %q, want the admitted run id", h.RunID())
	}
}

// A source with no stable message identity has nothing to deduplicate against,
// so each attempt must be admitted as its own submission rather than colliding
// with the previous one under a shared empty id.
func TestStartTurnMintsInvocationIDWhenKeyIsAbsent(t *testing.T) {
	a, admitter := newAdmittedTurnTestService(t)
	for range 2 {
		h, err := a.StartTurn(context.Background(), turn.StartTurnCommand{
			TeamID: "t", Mode: turn.ModeChat, BotID: "b", ThreadID: "s",
		})
		if err != nil {
			t.Fatal(err)
		}
		drainTurn(t, h)
	}
	admitted := admitter.Admitted()
	if len(admitted) != 2 {
		t.Fatalf("admissions = %d, want 2", len(admitted))
	}
	for _, in := range admitted {
		if in.InvocationID == "" {
			t.Fatal("admission reached the runtime without an invocation id")
		}
	}
	if admitted[0].InvocationID == admitted[1].InvocationID {
		t.Fatalf("both attempts minted %q: separate submissions must not share a retry identity", admitted[0].InvocationID)
	}
}

// The two rejections a channel adapter must tell apart: busy is retryable and
// nothing was persisted, while a run that already exists has been answered and
// its redelivery must be dropped.
func TestStartTurnTranslatesAdmissionRejections(t *testing.T) {
	cmd := turn.StartTurnCommand{TeamID: "t", Mode: turn.ModeChat, BotID: "b", ThreadID: "s", IdempotencyKey: "msg-1"}

	for _, tc := range []struct {
		name     string
		admitErr error
		started  bool
		want     error
	}{
		{name: "busy", admitErr: sessionruntime.ErrSessionBusy, want: turn.ErrSessionBusy},
		{name: "conflict", admitErr: sessionruntime.ErrInvocationConflict, want: turn.ErrDuplicateTurn},
		{name: "replay of a run this call does not own", started: false, want: turn.ErrDuplicateTurn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, admitter := newAdmittedTurnTestService(t)
			admitter.AdmitErr = tc.admitErr
			admitter.Started = tc.started
			if _, err := a.StartTurn(context.Background(), cmd); !errors.Is(err, tc.want) {
				t.Fatalf("StartTurn() error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestRunEndClosesInjectChannel pins the fix for the per-turn goroutine
// leak: the application's inject-forwarding goroutine exits by ranging over
// InjectCh, so the service must close it when the run ends.
func TestRunEndClosesInjectChannel(t *testing.T) {
	fixture := newChatTurnFixture(t, directLifecycleModelSuccess)
	handle := fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "close-inject-1"})
	drainTurn(t, handle)
	// The run ended, so a steer for it is refused rather than left to rot in a
	// channel nobody will read.
	if err := handle.Inject(context.Background(), turn.InjectMessage{Text: "late"}); !errors.Is(err, errRunAlreadyFinished) {
		t.Fatalf("Inject() after the run ended = %v, want %v", err, errRunAlreadyFinished)
	}
}

// Every run must close its durable record, and the outcomes must stay
// distinguishable: the record is what releases the thread's single slot, and a
// run recorded as failed when it was merely stopped misreports the turn.
func TestRunEndRecordsTerminalState(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		fixture := newChatTurnFixture(t, directLifecycleModelFailure)
		handle := fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "records-error-1"})
		drainTurn(t, handle)
		if got := fixture.admitter.AwaitFinish(t); got.Status != sessionruntime.RunStatusErrored {
			t.Fatalf("status = %q, want %q", got.Status, sessionruntime.RunStatusErrored)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		fixture := newChatTurnFixture(t, directLifecycleModelHoldOnCancel)
		handle := fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "records-cancel-1"})
		fixture.awaitModelRequest(t)
		fixture.stopRun(t, handle)
		fixture.releaseModel(t)
		drainTurn(t, handle)
		// A cancelled run and a clean one both end with their channels closed;
		// only the run's own context tells them apart, and the state written
		// for the run is the driver's reading of it.
		if got := fixture.admitter.AwaitFinish(t); got.Status != sessionruntime.RunStatusAborted {
			t.Fatalf("status = %q, want %q", got.Status, sessionruntime.RunStatusAborted)
		}
	})

	t.Run("completion", func(t *testing.T) {
		fixture := newChatTurnFixture(t, directLifecycleModelSuccess)
		handle := fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "records-complete-1"})
		drainTurn(t, handle)
		got := fixture.admitter.AwaitFinish(t)
		if got.Status != sessionruntime.RunStatusCompleted {
			t.Fatalf("status = %q, want %q", got.Status, sessionruntime.RunStatusCompleted)
		}
		if got.Handle.FencingToken == 0 {
			t.Fatal("terminal write carried no fencing token: a superseded owner could close this run")
		}
	})
}
