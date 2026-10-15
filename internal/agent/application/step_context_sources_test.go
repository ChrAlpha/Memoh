package application

import (
	"context"
	"fmt"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/step"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type contextSourcePersister struct {
	*recordingStepPersister
	rows []messagepkg.Message
}

func (s *contextSourcePersister) PersistAgentStep(ctx context.Context, value messagepkg.AgentStep) ([]messagepkg.Message, error) {
	rows, err := s.recordingStepPersister.PersistAgentStep(ctx, value)
	for i := range rows {
		rows[i].ID = fmt.Sprintf("source-%d", i)
	}
	s.rows = rows
	return rows, err
}

func TestStepCommitCarriesExactSourcesThroughPersistence(t *testing.T) {
	store := &contextSourcePersister{recordingStepPersister: &recordingStepPersister{recordingMessageService: &recordingMessageService{}}}
	committer := &agentStepCommitter{
		service: &Service{}, persister: store, ownerContext: t.Context(),
		req: ChatRequest{BotID: "bot", ThreadID: "session", RunID: "run", Query: "original request"},
		rc:  resolvedContext{runConfig: native.RunConfig{ContextLifecycle: contextfrag.NewLifecycleHolder()}},
	}
	record := &step.Record{Messages: []sdk.Message{
		{},
		sdk.AssistantMessage("identical output"),
		sdk.AssistantMessage("identical output"),
	}}
	directive, err := committer.commit(t.Context(), 0, record)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 3 {
		t.Fatalf("stored rows = %d, want original input and two outputs", len(store.rows))
	}
	if len(directive.CommittedSources) != 3 {
		t.Fatalf("source receipts = %d, want one per original SDK message", len(directive.CommittedSources))
	}
	if directive.CommittedSources[0].ID != "" {
		t.Fatal("rejected message acquired another message's source")
	}
	for i, wantID := range []string{"source-1", "source-2"} {
		want, err := historyfrag.FromDBMessage(store.rows[i+1], historyfrag.ScopeFallback{})
		if err != nil {
			t.Fatal(err)
		}
		got := directive.CommittedSources[i+1]
		if got.ID != wantID || got != want.Ref {
			t.Fatalf("message %d source = %+v, want persisted %+v", i+1, got, want.Ref)
		}
	}
}
