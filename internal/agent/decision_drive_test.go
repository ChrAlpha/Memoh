package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/answer"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/drivetest"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/web"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

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
	service := newTestService(t, Deps{Messages: messages})

	err := service.answer.DriveUnrouted(context.Background(), answer.Continuation{
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
	service := newTestService(t, Deps{Messages: messages})

	err := service.answer.DriveUnrouted(context.Background(), answer.Continuation{
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
	service := newTestService(t, Deps{})

	err := service.answer.DriveUnrouted(ctx, answer.Continuation{
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
func webEntrySpecs(svc *Service, admission *web.Admission, delivery drive.RunDelivery) map[string]drive.RunSpec {
	return map[string]drive.RunSpec{
		"send": web.RunSpec(admission, runreq.ChatRequest{
			BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1",
			Query: directLifecyclePrompt, UserVisibleText: directLifecyclePrompt,
		}, delivery, outcome.InputHeld),
		"retry": svc.web.RetrySpec(admission, web.RetryLatestMessageInput{
			BotID: "bot-1", SessionID: "session-1", TargetTurnID: "old-turn",
		}, delivery),
		"edit": svc.web.EditSpec(admission, web.EditLatestMessageInput{
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
		{entry: "retry", phase: outcome.PhaseExecution, code: apperror.CodeInternal, wantRow: true},
		{entry: "edit", phase: outcome.PhaseExecution, code: apperror.CodeInternal, wantRow: true},
		{entry: "retry", phase: outcome.PhaseCommit, code: apperror.CodeAgentProviderAuthFailed},
		{entry: "edit", phase: outcome.PhaseCommit, code: apperror.CodeAgentProviderAuthFailed},
	} {
		name := tc.entry + " " + phaseLabel(tc.phase) + " " + string(tc.code)
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			messages := &agenttest.RecordingMessages{}
			svc := newDriveService(t, &agenttest.EventRecorder{}, &agenttest.ScriptedAdmitter{Started: true}, withDeps(func(d *Deps) { d.Messages = messages }))
			admission, err := svc.AdmitWebRun(context.Background(), web.AdmitInput{
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

			result := svc.driver.Drive(admission.Context(), spec)
			admission.Stop(outcome.ErrRunStopped)
			admission.Release()

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
