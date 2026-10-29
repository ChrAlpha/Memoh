package tools

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/background"
)

// A spawned task reads its own thread's window, sized for the model it runs
// on, and names its persisted request so that row is not replayed twice.
func TestSpawnedTaskReadsItsThreadWindowForItsModel(t *testing.T) {
	agent := &fakeSpawnAgent{}
	p, _, _, messages := newAgentControlProvider(t, agent)
	loader := &fakeTranscriptLoader{messages: messages}
	p.SetTranscriptLoader(loader)
	p.modelResolver = func(context.Context, SessionContext, string, string, string) (resolvedSubagentModel, error) {
		return resolvedSubagentModel{Model: &sdk.Model{}, ModelID: "model-2", ContextBudgetMaxTokens: 64000}, nil
	}
	session := SessionContext{
		BotID:                  "bot1",
		ChatID:                 "chat1",
		SessionID:              "parent1",
		ConversationType:       "group",
		ReplyTarget:            "target1",
		ContextBudgetMaxTokens: 128000,
	}

	result := asMap(t, mustExecuteAgentTool(t, p, session, "spawn_agent", map[string]any{"id": "worker", "task": "alpha"}))

	req, ok := loader.requestAt(0)
	if !ok {
		t.Fatal("spawned task did not read its transcript")
	}
	stored, _ := messages.ListBySession(context.Background(), result["session_id"].(string))
	if len(stored) == 0 {
		t.Fatal("task request was not persisted")
	}
	want := SubagentTranscriptRequest{
		BotID:              "bot1",
		ChatID:             "chat1",
		SessionID:          result["session_id"].(string),
		ConversationType:   "group",
		ReplyTarget:        "target1",
		RequestMessageID:   stored[0].ID,
		ContextTokenBudget: 64000,
	}
	if req != want {
		t.Fatalf("transcript request = %+v, want %+v", req, want)
	}
}

type failingTranscriptLoader struct {
	err    error
	cancel context.CancelCauseFunc
}

func (l failingTranscriptLoader) LoadSubagentTranscript(context.Context, SubagentTranscriptRequest) ([]sdk.Message, error) {
	if l.cancel != nil {
		l.cancel(context.Canceled)
	}
	return nil, l.err
}

// A task whose history cannot be read fails instead of running without it.
func TestSpawnedTaskFailsWhenItsTranscriptCannotBeRead(t *testing.T) {
	agent := &fakeSpawnAgent{}
	p := &SpawnProvider{
		agent:       agent,
		transcripts: failingTranscriptLoader{err: errors.New("history unavailable")},
		logger:      slog.New(slog.DiscardHandler),
		modelResolver: func(context.Context, SessionContext, string, string, string) (resolvedSubagentModel, error) {
			return resolvedSubagentModel{}, nil
		},
	}

	result := p.runSubagentTask(context.Background(), &agentRequest{
		taskID:         "task-history",
		agentID:        "worker",
		agentSessionID: "session-history",
		message:        "work",
		parentSession:  SessionContext{BotID: "bot-1"},
	})

	if result.Status != string(background.TaskFailed) || result.Cause == nil || result.Cause.Error() != "history unavailable" {
		t.Fatalf("result = %+v, want a failure carrying the read error", result)
	}
	if len(agent.queries()) != 0 {
		t.Fatal("task ran without its history")
	}
}

// A task stopped while its history loads ends as stopped, not as a failed
// read.
func TestSpawnedTaskStoppedWhileReadingItsTranscriptIsAborted(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	agent := &fakeSpawnAgent{}
	p := &SpawnProvider{
		agent:       agent,
		transcripts: failingTranscriptLoader{err: context.Canceled, cancel: cancel},
		logger:      slog.New(slog.DiscardHandler),
		modelResolver: func(context.Context, SessionContext, string, string, string) (resolvedSubagentModel, error) {
			return resolvedSubagentModel{}, nil
		},
	}

	result := p.runSubagentTask(ctx, &agentRequest{
		taskID:         "task-stopped",
		agentID:        "worker",
		agentSessionID: "session-stopped",
		message:        "work",
		parentSession:  SessionContext{BotID: "bot-1"},
	})

	if !result.AttemptResolved || result.AttemptOutcome != SpawnAttemptAbort || !errors.Is(result.Cause, context.Canceled) {
		t.Fatalf("result = %+v, want an aborted attempt", result)
	}
	if len(agent.queries()) != 0 {
		t.Fatal("stopped task still ran")
	}
}
