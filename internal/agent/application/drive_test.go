package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/drivetest"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/errs"
)

// ---------------------------------------------------------------------------
// driver table: one run, one ending, every entry
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------- table cases

func newDriveService(pub *agenttest.EventRecorder, admitter runreq.TurnAdmitter) *Service {
	svc := wireComponents(&Service{logger: slog.New(slog.DiscardHandler)})
	if admitter != nil {
		svc.sessionRuntime = admitter
	}
	rebuildDriver(svc, "events", func(d *drive.Deps) {
		if pub != nil {
			d.Events = pub
		}
	})
	return svc
}

// L3: 续跑
//
// A decision continuation resumes from an answer that is already committed, so
// its input is accepted: every failure, from its setup on, leaves a failure
// row in the turn the decision parked, and no new user row.

// continuationRequest is the request a continuation resumes as: the turn's
// request message is already in history.
func continuationRequest() runreq.ChatRequest {
	return runreq.ChatRequest{
		BotID: agenttest.LifecycleBotID, ChatID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
		UserMessagePersisted: true, PersistedUserMessageID: "request-message",
	}
}

// A continuation whose model call the provider refuses before its first step
// ends under the provider's code everywhere the failure is reported: the
// caller's error event with the catalog detail, the run's failure, and the
// failure row. The provider's own words reach none of them.
func TestDriveDecisionContinuationNamesTheProviderFailure(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	service := wireComponents(&Service{
		agent:          native.New(native.Deps{Logger: slog.New(slog.DiscardHandler)}),
		logger:         slog.New(slog.DiscardHandler),
		messageService: messages,
	})
	req := continuationRequest()
	cfg := native.RunConfig{
		Model:    &sdk.Model{ID: "continuation-model", Provider: rejectingSpawnProvider{}, Type: sdk.ModelTypeChat},
		Messages: []sdk.Message{sdk.UserMessage("continue after the decision")},
		Identity: native.SessionContext{BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID},
	}
	eventCh := make(chan runreq.WSStreamEvent, 64)

	err := service.driveUnroutedContinuation(context.Background(), decisionContinuation{
		Req: req,
		Setup: func(ctx context.Context, _ runreq.ChatRequest) (drive.RunPlan, error) {
			return service.nativeContinuationPlan(ctx, req, cfg, "model-1")
		},
	}, eventCh)
	close(eventCh)
	if err != nil {
		t.Fatalf("continuation error = %v, want the failure delivered in the stream", err)
	}

	var errorEvents []native.StreamEvent
	for raw := range eventCh {
		if strings.Contains(string(raw), "SECRET") {
			t.Fatalf("delivered event leaked the cause: %s", raw)
		}
		var ev native.StreamEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode delivered event %s: %v", raw, err)
		}
		if ev.Type == native.EventError {
			errorEvents = append(errorEvents, ev)
		}
	}
	definition, _ := apperror.Lookup(apperror.CodeAgentProviderAuthFailed)
	if len(errorEvents) != 1 || errorEvents[0].Code != string(apperror.CodeAgentProviderAuthFailed) || errorEvents[0].Error != definition.Detail {
		t.Fatalf("delivered error events = %#v, want one with the provider's code and detail", errorEvents)
	}
	row := failureRows(t, messages)
	if got := row.Metadata[messagepkg.HistoryErrorCodeMetadataKey]; got != string(apperror.CodeAgentProviderAuthFailed) {
		t.Fatalf("failure row code = %#v, want %s", got, apperror.CodeAgentProviderAuthFailed)
	}
}

// failureRows is the one assistant failure row a continuation left. A
// continuation writes no user row: the turn's request message is in history.
func failureRows(t *testing.T, messages *agenttest.RecordingMessages) messagepkg.PersistInput {
	t.Helper()
	if len(messages.Persisted) != 1 || messages.Persisted[0].Role != "assistant" {
		t.Fatalf("persisted %#v, want one assistant failure row", messages.Persisted)
	}
	row := messages.Persisted[0]
	if row.TurnRequestMessageID != "request-message" {
		t.Fatalf("failure row turn request = %q, want the parked turn's request message", row.TurnRequestMessageID)
	}
	return row
}

// A continuation that fails before it reaches the model, such as storing the
// answer's result, still leaves its failure row: the answer was accepted, and
// there is nowhere to hand it back to.
func TestDriveDecisionContinuationSetupFailureLeavesAFailureRow(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	service := wireComponents(&Service{logger: slog.New(slog.DiscardHandler), messageService: messages})

	err := service.driveUnroutedContinuation(context.Background(), decisionContinuation{
		Req: continuationRequest(),
		Setup: func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
			return drive.RunPlan{}, errors.New("SECRET tool result store unavailable")
		},
	}, nil)

	if apperror.CodeOf(err) != apperror.CodeRuntimeRunFailed {
		t.Fatalf("continuation error = %v, want %s for a caller without a stream", err, apperror.CodeRuntimeRunFailed)
	}
	row := failureRows(t, messages)
	if got := row.Metadata[messagepkg.HistoryErrorCodeMetadataKey]; got != string(apperror.CodeRuntimeRunFailed) {
		t.Fatalf("failure row code = %#v, want %s", got, apperror.CodeRuntimeRunFailed)
	}
}

// A continuation whose answer no longer targets the live run lost it: another
// owner names the run's outcome, so it writes no history.
func TestDriveDecisionContinuationTargetGoneWritesNothing(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	service := wireComponents(&Service{logger: slog.New(slog.DiscardHandler), messageService: messages})

	err := service.driveUnroutedContinuation(context.Background(), decisionContinuation{
		Req: continuationRequest(),
		Setup: func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
			return drive.RunPlan{}, sessionruntime.ErrCommandTargetNotActive
		},
	}, nil)
	if err != nil {
		t.Fatalf("continuation error = %v, want none for a run another owner holds", err)
	}
	if len(messages.Persisted) != 0 {
		t.Fatalf("persisted %#v, want nothing", messages.Persisted)
	}
}

// fixedStream is a native runtime that produces a fixed event sequence.
type fixedStream []native.StreamEvent

func (f fixedStream) Stream(context.Context, native.RunConfig) <-chan native.StreamEvent {
	ch := make(chan native.StreamEvent, len(f))
	for _, ev := range f {
		ch <- ev
	}
	close(ch)
	return ch
}

// A failed run whose terminal still carries the conversation it was given, but
// nothing the user saw, leaves one failure row: the row is the failure's, and
// the terminal's snapshot has no output of its own to add.
func TestDriveDecisionContinuationSnapshotWithoutOutputWritesOneRow(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	service := wireComponents(&Service{logger: slog.New(slog.DiscardHandler), messageService: messages})
	snapshot, err := json.Marshal([]sdk.Message{sdk.UserMessage("continue after the decision")})
	if err != nil {
		t.Fatal(err)
	}
	failure := apperror.Wrap(apperror.CodeAgentProviderAuthFailed, errors.New("SECRET key"), nil)
	exec := &nativeExecutor{
		agent: fixedStream{
			{Type: native.EventAgentStart},
			{Type: native.EventError, Code: string(apperror.CodeAgentProviderAuthFailed), Cause: failure},
			{Type: native.EventAgentAbort, Messages: snapshot, EndReason: event.EndFailed},
		},
		req:     continuationRequest(),
		timing:  persist.NewReasoningTimingTracker(nil),
		service: service,
	}

	_ = service.driveUnroutedContinuation(context.Background(), decisionContinuation{
		Req: continuationRequest(),
		Setup: func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
			return drive.RunPlan{Exec: exec}, nil
		},
	}, nil)

	row := failureRows(t, messages)
	if got := row.Metadata[messagepkg.HistoryErrorCodeMetadataKey]; got != string(apperror.CodeAgentProviderAuthFailed) {
		t.Fatalf("failure row code = %#v, want %s", got, apperror.CodeAgentProviderAuthFailed)
	}
}

// The caller that streams an unrouted continuation leaving is a stop: the run
// ends without a failure and writes no row.
func TestDriveDecisionContinuationCallerLeavingIsAStop(t *testing.T) {
	t.Parallel()

	ctx, leave := context.WithCancel(context.Background())
	eventCh := make(chan runreq.WSStreamEvent)
	var commits []bool
	exec := runFuncExecutor(func(ctx context.Context, _ func(native.StreamEvent)) drive.ExecEnding {
		leave()
		<-ctx.Done()
		return drive.ExecEnding{
			Terminal: &native.StreamEvent{Type: native.EventAgentAbort},
			Commit: func(_ context.Context, _ *outcome.RunFailure, row bool) (drive.CommitSummary, error) {
				commits = append(commits, row)
				return drive.CommitSummary{}, nil
			},
		}
	})
	service := wireComponents(&Service{logger: slog.New(slog.DiscardHandler)})

	err := service.driveUnroutedContinuation(ctx, decisionContinuation{
		Req: continuationRequest(),
		Setup: func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
			return drive.RunPlan{Exec: exec}, nil
		},
	}, eventCh)
	if err != nil {
		t.Fatalf("continuation error = %v, want none", err)
	}
	// A stop writes no history at all, so the ending's history write is not
	// even reached.
	if len(commits) != 0 {
		t.Fatalf("history writes = %v, want none for a stop", commits)
	}
}

var (
	_ = fmt.Sprintf
	_ = messagepkg.HistoryErrorCodeMetadataKey
	_ = errs.Recorded
	_ = sdk.Usage{}
)

// L4: Web
//
// The Web entries decide two things the driver reads: who holds the run's
// input, and whether the run replaces a turn history already has. A send's
// draft is still the submitter's, so a failure before execution writes nothing
// and the composer can hand it back; a retry or an edit replaces a turn, so it
// writes nothing either — the turn it replaces is already there, and only a
// failure after execution started leaves it its own row, under a code the
// history whitelist names.

// webEntrySpecs builds one spec per Web entry through the constructor the
// entry itself uses, so the input ownership and the replacement under test are
// the entry's own.
func webEntrySpecs(svc *Service, admission *WebAdmission, delivery drive.RunDelivery) map[string]drive.RunSpec {
	return map[string]drive.RunSpec{
		"send": svc.webRunSpec(admission, runreq.ChatRequest{
			BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1",
			Query: directLifecyclePrompt, UserVisibleText: directLifecyclePrompt,
		}, delivery, outcome.InputHeld),
		"retry": svc.retryRunSpec(admission, RetryLatestMessageInput{
			BotID: "bot-1", SessionID: "session-1", TargetTurnID: "old-turn",
		}, delivery),
		"edit": svc.editRunSpec(admission, EditLatestMessageInput{
			BotID: "bot-1", SessionID: "session-1", TargetTurnID: "old-turn", Text: "edited",
		}, delivery),
	}
}

func TestDriveWebEntryFailureRowsFollowInputOwnership(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		entry   string
		phase   outcome.RunPhase
		code    apperror.Code
		hook    bool
		wantRow bool
	}{
		{entry: "send", phase: outcome.PhaseSetup, code: apperror.CodeAgentProviderAuthFailed},
		{entry: "send", phase: outcome.PhaseSetup, code: apperror.CodeHookUserMessageFailed, hook: true, wantRow: true},
		{entry: "send", phase: outcome.PhaseExecution, code: apperror.CodeAgentProviderAuthFailed, wantRow: true},
		{entry: "send", phase: outcome.PhasePublish, code: apperror.CodeAgentProviderAuthFailed, wantRow: true},
		{entry: "retry", phase: outcome.PhaseSetup, code: apperror.CodeAgentProviderAuthFailed},
		{entry: "edit", phase: outcome.PhaseSetup, code: apperror.CodeAgentProviderAuthFailed},
		{entry: "retry", phase: outcome.PhaseExecution, code: apperror.CodeAgentProviderAuthFailed, wantRow: true},
		{entry: "edit", phase: outcome.PhaseExecution, code: apperror.CodeAgentProviderAuthFailed, wantRow: true},
		{entry: "retry", phase: outcome.PhaseExecution, code: apperror.CodeInternal},
		{entry: "edit", phase: outcome.PhaseExecution, code: apperror.CodeInternal},
		{entry: "retry", phase: outcome.PhaseCommit, code: apperror.CodeAgentProviderAuthFailed},
		{entry: "edit", phase: outcome.PhaseCommit, code: apperror.CodeAgentProviderAuthFailed},
	} {
		name := tc.entry + " " + phaseLabel(tc.phase) + " " + string(tc.code)
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			messages := &agenttest.RecordingMessages{}
			svc := newDriveService(&agenttest.EventRecorder{}, &agenttest.ScriptedAdmitter{Started: true})
			svc.messageService = messages
			rewireComponents(svc)
			admission, err := svc.AdmitWebRun(context.Background(), WebAdmitInput{
				BotID: "bot-1", SessionID: "session-1", InvocationID: name,
				Payload: []byte(`{"kind":"` + tc.entry + `"}`),
			})
			if err != nil || admission.Duplicate {
				t.Fatalf("AdmitWebRun() = %+v, %v; want a started run", admission, err)
			}
			// The setup is replaced by the phase's failure, so what the ending
			// writes is decided by the entry's own input ownership and
			// replacement alone.
			var commits []bool
			spec := webEntrySpecs(svc, admission, &drivetest.RecordingDelivery{})[tc.entry]
			req := spec.Req
			spec.Setup = func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
				if tc.hook {
					return drive.RunPlan{}, apperror.New(apperror.CodeHookUserMessageFailed, nil)
				}
				exec := &drivetest.ScriptedExecutor{
					Terminal:   ptrTo(agenttest.TerminalEvent(t, "an answer")),
					Cause:      apperror.Wrap(tc.code, errors.New("SECRET cause"), nil),
					Phase:      tc.phase,
					CommitRows: &commits,
				}
				return drive.RunPlan{Exec: exec, Req: req}, nil
			}

			result := svc.driver.Drive(admission.ctx, spec)
			admission.stop(outcome.ErrRunStopped)
			admission.endActive()

			if result.End != outcome.EndFailed || result.Failure == nil || result.Failure.Code != tc.code {
				t.Fatalf("result = %#v, want failed %s", result, tc.code)
			}
			if tc.hook {
				// The hook's refusal is a setup failure the driver writes
				// itself: the user has to be shown why the send was refused.
				if len(messages.Persisted) != 2 || messages.Persisted[1].Metadata[messagepkg.HistoryFailureOriginMetadataKey] != messagepkg.HistoryFailureOriginUserMessageHook {
					t.Fatalf("persisted %#v, want the hook-refused pair", messages.Persisted)
				}
				return
			}
			if len(commits) != 1 {
				t.Fatalf("history writes = %v, want one", commits)
			}
			if commits[0] != tc.wantRow {
				t.Fatalf("failure row written = %v, want %v (input %d, replacing %v, phase %s)",
					commits[0], tc.wantRow, spec.Input, spec.Replacement != nil, phaseLabel(tc.phase))
			}
		})
	}
}

// phaseLabel names a run phase in a test case's name.
func phaseLabel(phase outcome.RunPhase) string {
	switch phase {
	case outcome.PhaseSetup:
		return "setup"
	case outcome.PhaseExecution:
		return "execution"
	case outcome.PhaseCommit:
		return "commit"
	case outcome.PhasePublish:
		return "publish"
	default:
		return "unknown"
	}
}
