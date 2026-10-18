package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
)

type decisionOutputBackend struct {
	*sessionruntime.MemoryBackend
	output []struct {
		Type   string
		Output json.RawMessage
	}
}

func (b *decisionOutputBackend) AppendDecisionOutput(ctx context.Context, ref sessionruntime.DecisionOutputRef, seq int64, payload json.RawMessage, limits sessionruntime.DecisionOutputLimits) (sessionruntime.DecisionOutputState, error) {
	state, err := b.MemoryBackend.AppendDecisionOutput(ctx, ref, seq, payload, limits)
	if err == nil && state.Applied {
		entry := struct {
			Type   string
			Output json.RawMessage
		}{Output: payload}
		if payload == nil {
			entry.Type = "decision_output_end"
		}
		b.output = append(b.output, entry)
	}
	return state, err
}

func TestContinuationPublishesTextAndNextQuestionToChannel(t *testing.T) {
	for _, nextQuestion := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary reply", true: "second question"}[nextQuestion], func(t *testing.T) {
			backend := &decisionOutputBackend{MemoryBackend: sessionruntime.NewMemoryBackend()}
			manager, handle := newWaitingDecisionRuntime(t, backend)
			service := continuationService(t, manager)
			events := []native.StreamEvent{{Type: native.EventAgentStart}, {Type: native.EventTextDelta, Delta: "收到你的答案"}}
			terminal := native.StreamEvent{Type: native.EventAgentEnd}
			if nextQuestion {
				events = append(events, native.StreamEvent{Type: native.EventUserInputRequest, UserInputID: "next-question", Status: "pending"})
				terminal.ApprovalID = "next-question"
				terminal.UserInputID = "next-question"
				terminal.Status = "pending"
			}
			events = append(events, terminal)
			continueDecision(t, service, manager, decisionCommand(handle, "answer-command"), scriptedRun(events, nil))
			if len(backend.output) != len(events)+1 {
				t.Fatalf("channel output count=%d, want %d", len(backend.output), len(events)+1)
			}
			for i, want := range events {
				var got native.StreamEvent
				if err := json.Unmarshal(backend.output[i].Output, &got); err != nil {
					t.Fatal(err)
				}
				if got.Type != want.Type || got.Delta != want.Delta || got.UserInputID != want.UserInputID {
					t.Fatalf("event %d=%+v want %+v", i, got, want)
				}
			}
			if backend.output[len(events)].Type != "decision_output_end" {
				t.Fatal("channel stream did not close")
			}
		})
	}
}

type failedEndCheckpointBackend struct{ *sessionruntime.MemoryBackend }

func (b failedEndCheckpointBackend) AppendDecisionOutput(ctx context.Context, ref sessionruntime.DecisionOutputRef, seq int64, payload json.RawMessage, limits sessionruntime.DecisionOutputLimits) (sessionruntime.DecisionOutputState, error) {
	if payload == nil {
		return sessionruntime.DecisionOutputState{}, errors.New("checkpoint write unavailable")
	}
	return b.MemoryBackend.AppendDecisionOutput(ctx, ref, seq, payload, limits)
}

func TestContinuationClosesRunWhenEndCheckpointCannotPersist(t *testing.T) {
	backend := failedEndCheckpointBackend{sessionruntime.NewMemoryBackend()}
	manager, handle := newWaitingDecisionRuntime(t, backend)
	service := continuationService(t, manager)
	continueDecision(t, service, manager, decisionCommand(handle, "answer"), scriptedRun([]native.StreamEvent{
		{Type: native.EventUserInputRequest, UserInputID: "next", Status: "pending"},
		{Type: native.EventAgentEnd, ApprovalID: "next", UserInputID: "next", Status: "pending"},
	}, nil))
	snapshot, err := manager.Snapshot(context.Background(), handle.BotID, handle.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CurrentRunView == nil || snapshot.CurrentRunView.Status != sessionruntime.RunStatusErrored ||
		snapshot.CurrentRunView.ErrorCode != string(apperror.CodeSessionPublishFailed) {
		t.Fatalf("failed end write left run parked: %+v", snapshot.CurrentRunView)
	}
}
