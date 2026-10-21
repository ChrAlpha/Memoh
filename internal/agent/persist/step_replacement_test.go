package persist

import (
	"context"
	"sync"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/step"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/runtimefence"
)

func replacementTestContext(fx steerFixture) context.Context {
	return runtimefence.WithContext(context.Background(), runtimefence.Fence{
		BotID: fx.handle.BotID, SessionID: fx.handle.SessionID, Token: fx.handle.FencingToken,
	})
}

func replacementResolvedContext() runreq.ResolvedContext {
	return runreq.ResolvedContext{RunConfig: native.RunConfig{ContextLifecycle: contextfrag.NewLifecycleHolder()}}
}

func TestRetryStepStartsReplacementBeforeDecisionAndContinuesOnClonedRequest(t *testing.T) {
	fx := newSteerFixture(t)
	handle := fx.handle
	ctx := replacementTestContext(fx)
	replacement := &messagepkg.TurnReplacement{OldTurnID: "old-turn", ReplacementTurnID: "new-turn", ReplacementTurnPosition: int64Pointer(2), RequestMessageID: "old-request", Reason: "retry"}
	committer := fx.writer.NewStepCommitter(ctx, runreq.ChatRequest{
		BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID, RunHandle: handle,
		TurnID: "new-turn", TurnPosition: int64Pointer(2), TurnReplacement: replacement,
		ReusePersistedUserMessage: true, PersistedUserMessageID: "old-request",
	}, replacementResolvedContext())
	if committer == nil {
		t.Fatal("missing replacement step committer")
	}
	if _, err := committer.commit(ctx, 0, &step.Record{
		Messages: []sdk.Message{sdk.AssistantMessage("Which option?")}, Deferred: &toolexec.ToolApprovalResult{},
	}); err != nil {
		t.Fatalf("commit deferred step: %v", err)
	}
	store := fx.messages
	if len(store.Steps) != 1 || store.Steps[0].Replacement != replacement || !committer.replacementStarted {
		t.Fatalf("waiting step did not start its replacement: %#v", store.Steps)
	}
	if len(store.Steps[0].Messages) != 1 || store.Steps[0].Messages[0].SkipHistoryTurn {
		t.Fatalf("retry should clone its user in the bound transaction: %#v", store.Steps[0])
	}
	if committer.turnRequestMessageID != "replacement-request" {
		t.Fatalf("continuation anchor = %q, want cloned request", committer.turnRequestMessageID)
	}
	if _, err := committer.commit(ctx, 1, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("done")}}); err != nil {
		t.Fatalf("commit follow-up: %v", err)
	}
	if store.Steps[1].Replacement != nil || store.Steps[1].Messages[0].TurnRequestMessageID != "replacement-request" {
		t.Fatalf("follow-up tried to replace again or used the old turn: %#v", store.Steps[1])
	}
}

func TestReplacementFailureWithoutTextUsesBoundInterruptedStep(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "edit", true: "retry"}[reuse], func(t *testing.T) {
			fx := newSteerFixture(t)
			handle := fx.handle
			ctx := replacementTestContext(fx)
			replacement := &messagepkg.TurnReplacement{OldTurnID: "old-turn", ReplacementTurnID: "new-turn", ReplacementTurnPosition: int64Pointer(2)}
			if reuse {
				replacement.RequestMessageID = "old-request"
			}
			committer := fx.writer.NewStepCommitter(ctx, runreq.ChatRequest{
				BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID, RunHandle: handle,
				TurnID: "new-turn", TurnPosition: int64Pointer(2), TurnReplacement: replacement, Query: "edited request",
				ReusePersistedUserMessage: reuse, PersistedUserMessageID: "old-request",
			}, replacementResolvedContext())
			if committer == nil {
				t.Fatal("missing replacement step committer")
			}
			if _, err := committer.RecordFailure(ctx, "", outcome.RunFailure{Code: apperror.CodeAgentProviderRequestRejected}); err != nil {
				t.Fatalf("record failure: %v", err)
			}
			store := fx.messages
			if len(store.Steps) != 1 || store.Steps[0].Replacement != replacement || !store.Steps[0].Interrupted {
				t.Fatalf("failure did not cross bound replacement transaction: %#v", store.Steps)
			}
			messages := store.Steps[0].Messages
			if len(messages) != map[bool]int{false: 2, true: 1}[reuse] {
				t.Fatalf("failure inputs = %#v", messages)
			}
			assistant := messages[len(messages)-1]
			if assistant.SkipHistoryTurn || assistant.Metadata[messagepkg.HistoryErrorCodeMetadataKey] != string(apperror.CodeAgentProviderRequestRejected) {
				t.Fatalf("failure row lost turn binding or stable code: %#v", assistant)
			}
		})
	}
}

// roundRecorder keeps the rows a finished run hands to memory extraction.
type roundRecorder struct {
	mu     sync.Mutex
	rounds [][]messagepkg.Message
}

func (r *roundRecorder) RoundPersisted(_ context.Context, _ runreq.ChatRequest, persisted []messagepkg.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rounds = append(r.rounds, persisted)
}

func TestRetryStepMemoryUsesClonedRequest(t *testing.T) {
	testRetryStepMemoryUsesClonedRequest(t, false)
}

func TestRetrySteeredStepMemoryUsesClonedRequest(t *testing.T) {
	testRetryStepMemoryUsesClonedRequest(t, true)
}

func testRetryStepMemoryUsesClonedRequest(t *testing.T, steered bool) {
	t.Helper()
	fx := newSteerFixture(t)
	handle := fx.handle
	request := messagepkg.Message{ID: "replacement-request", Role: "user", TurnID: "new-turn"}
	fx.messages.ReplacementRequest = &request
	rounds := &roundRecorder{}
	fx.writer.rounds = rounds
	ctx := replacementTestContext(fx)
	committer := fx.writer.NewStepCommitter(ctx, runreq.ChatRequest{
		BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID, RunHandle: handle,
		TurnID: "new-turn", TurnPosition: int64Pointer(2),
		ReusePersistedUserMessage: true, PersistedUserMessageID: "old-request",
		TurnReplacement: &messagepkg.TurnReplacement{
			OldTurnID: "old-turn", ReplacementTurnID: "new-turn", ReplacementTurnPosition: int64Pointer(2), RequestMessageID: "old-request",
		},
	}, replacementResolvedContext())
	if committer == nil {
		t.Fatal("missing replacement step committer")
	}
	nextStep := 0
	if steered {
		if _, err := committer.persist(ctx, 0, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("partial answer")}}, stepSteered, outcome.RunFailure{}); err != nil {
			t.Fatalf("checkpoint replacement: %v", err)
		}
		nextStep++
	}
	if _, err := committer.commit(ctx, nextStep, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("new answer")}}); err != nil {
		t.Fatalf("commit replacement: %v", err)
	}
	if err := committer.Finish(ctx, 0); err != nil {
		t.Fatalf("finish replacement: %v", err)
	}
	rounds.mu.Lock()
	defer rounds.mu.Unlock()
	if len(rounds.rounds) != 1 {
		t.Fatalf("memory extraction ran %d times, want once", len(rounds.rounds))
	}
	got := rounds.rounds[0]
	if len(got) != 2 || got[0].ID != "replacement-request" || got[0].Role != "user" || got[1].Role != "assistant" {
		t.Fatalf("memory rows = %#v, want the cloned request exactly once, then the answer", got)
	}
}
