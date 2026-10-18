package decide

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agenttest"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func TestRunDecisionCleanupRejectsSuccessorFence(t *testing.T) {
	q := &agenttest.DecisionQueries{Row: sqlc.UserInputRequest{
		ID:    db.ParseUUIDOrEmpty("44444444-4444-4444-8444-444444444444"),
		BotID: db.ParseUUIDOrEmpty(agenttest.LifecycleBotID), SessionID: db.ParseUUIDOrEmpty(agenttest.LifecycleSessionID),
		RunID: db.ParseUUIDOrEmpty(agenttest.LifecycleRunID), Status: userinput.StatusPending,
		RuntimeFencingToken: pgtype.Int8{Int64: 8, Valid: true},
	}}
	input := &agenttest.UserInput{}
	s := newTestDecider(Deps{Queries: q, UserInput: input})
	err := s.FinalizeRuntimeDecisions(context.Background(), sessionruntime.RunHandle{
		BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID, FencingToken: 7,
	})
	if !errors.Is(err, sessionruntime.ErrRunOwnershipLost) || input.CancelCalls != 0 {
		t.Fatalf("successor decision changed: %v, calls=%d", err, input.CancelCalls)
	}
}

func TestRunDecisionFinishFenceRules(t *testing.T) {
	for _, tt := range []struct {
		name, status string
		fence        int64
		wantLost     bool
	}{
		{"old_terminal", userinput.StatusSubmitted, 6, false},
		{"successor_terminal", userinput.StatusSubmitted, 8, true},
		{"old_pending", userinput.StatusPending, 6, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := &agenttest.DecisionQueries{Row: sqlc.UserInputRequest{ID: db.ParseUUIDOrEmpty("44444444-4444-4444-8444-444444444444"), BotID: db.ParseUUIDOrEmpty(agenttest.LifecycleBotID), SessionID: db.ParseUUIDOrEmpty(agenttest.LifecycleSessionID), RunID: db.ParseUUIDOrEmpty(agenttest.LifecycleRunID), Status: tt.status, RuntimeFencingToken: pgtype.Int8{Int64: tt.fence, Valid: true}}}
			input := &agenttest.UserInput{Target: userinput.Request{ID: q.Row.ID.String(), Status: tt.status}}
			service := newTestDecider(Deps{Queries: q, UserInput: input})
			err := service.FinalizeRuntimeDecisions(t.Context(), sessionruntime.RunHandle{BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID, FencingToken: 7})
			if errors.Is(err, sessionruntime.ErrRunOwnershipLost) != tt.wantLost || (err != nil && !tt.wantLost) || input.CancelCalls != 0 {
				t.Fatalf("fence result: %v, mutations=%d", err, input.CancelCalls)
			}
		})
	}
}

func TestFinishRereadsConcurrentUserDecision(t *testing.T) {
	q := &agenttest.DecisionQueries{Row: sqlc.UserInputRequest{ID: db.ParseUUIDOrEmpty("44444444-4444-4444-8444-444444444444"), BotID: db.ParseUUIDOrEmpty(agenttest.LifecycleBotID), SessionID: db.ParseUUIDOrEmpty(agenttest.LifecycleSessionID), RunID: db.ParseUUIDOrEmpty(agenttest.LifecycleRunID), Status: userinput.StatusPending, RuntimeFencingToken: pgtype.Int8{Int64: 7, Valid: true}}}
	input := &agenttest.UserInput{CancelErr: userinput.ErrAlreadyDecided, Target: userinput.Request{ID: q.Row.ID.String(), Status: userinput.StatusSubmitted}}
	service := newTestDecider(Deps{Queries: q, UserInput: input})
	if err := service.FinalizeRuntimeDecisions(t.Context(), sessionruntime.RunHandle{BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID, FencingToken: 7}); err != nil {
		t.Fatal(err)
	}
	if input.CancelCalls != 1 {
		t.Fatal("pending row was not resolved")
	}
	input.Target.Status = userinput.StatusPending
	if err := service.FinalizeRuntimeDecisions(t.Context(), sessionruntime.RunHandle{BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID, FencingToken: 7}); err == nil {
		t.Fatal("unresolved decision allowed finish")
	}
}
