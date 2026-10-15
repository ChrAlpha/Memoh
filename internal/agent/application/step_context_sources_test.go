package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/step"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

type contextSourcePersister struct {
	*recordingStepPersister
	rows    []messagepkg.Message
	readErr error
}

func TestPostgresCommittedSourceMatchesReloadedJSONB(t *testing.T) {
	ctx := t.Context()
	pool := openTurnAdmissionPostgres(t, ctx)
	botID, sessionID := createTurnAdmissionFixture(t, ctx, pool)
	queries := postgresstore.NewQueriesWithPool(pool, sqlc.New(pool))
	messages := messagepkg.NewService(slog.New(slog.DiscardHandler), queries)
	persisted, err := messages.Persist(ctx, messagepkg.PersistInput{
		BotID: botID, SessionID: sessionID, Role: "user",
		Content:  []byte(`{"role":"user","content":[{"type":"text","text":"committed input"}]}`),
		Usage:    []byte(`{"inputTokens":7,"outputTokens":3}`),
		Metadata: map[string]any{"nested": map[string]any{"z": 7, "a": "data"}},
		Assets:   []messagepkg.AssetRef{{ContentHash: "receipt-asset", Role: "attachment", Name: "diagram.png", Metadata: map[string]any{"z": 2, "a": "asset"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := messages.GetByIDBySession(ctx, sessionID, persisted.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, err := historyfrag.FromDBMessage(loaded, historyfrag.ScopeFallback{})
	if err != nil {
		t.Fatal(err)
	}
	committer := &agentStepCommitter{service: &Service{messageService: messages}}
	got, err := committer.committedContextSources(ctx, 1, []int{0}, []messagepkg.Message{persisted})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != want.Ref {
		t.Fatalf("receipt differs from durable reload: got=%+v want=%+v persisted_content=%s loaded_content=%s", got, want.Ref, persisted.Content, loaded.Content)
	}
}

func (s *contextSourcePersister) PersistAgentStep(ctx context.Context, value messagepkg.AgentStep) ([]messagepkg.Message, error) {
	rows, err := s.recordingStepPersister.PersistAgentStep(ctx, value)
	for i := range rows {
		rows[i].ID = fmt.Sprintf("source-%d", i)
	}
	s.rows = rows
	return rows, err
}

func (s *contextSourcePersister) GetByIDBySession(_ context.Context, sessionID, messageID string) (messagepkg.Message, error) {
	if s.readErr != nil {
		return messagepkg.Message{}, s.readErr
	}
	for _, row := range s.rows {
		if row.ID == messageID && row.SessionID == sessionID {
			return row, nil
		}
	}
	return messagepkg.Message{}, fmt.Errorf("source %s not found", messageID)
}

func TestCommittedSourceDeclinesChangedPayload(t *testing.T) {
	for _, tc := range []struct {
		name, original, loaded string
		accepted               bool
	}{
		{"formatting", `{"a":1,"b":["text"]}`, `{"b": ["text"], "a": 1}`, true},
		{"numeric_notation", `{"n":1e3}`, `{"n": 1000}`, true},
		{"large_exact_integer", `{"n":9007199254740993}`, `{"n": 9007199254740993}`, true},
		{"changed_large_integer", `{"n":9007199254740993}`, `{"n": 9007199254740992}`, false},
		{"changed_text", `{"text":"old"}`, `{"text": "new"}`, false},
		{"changed_type", `{"n":1000}`, `{"n": "1000"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := messagepkg.Message{ID: "source", BotID: "bot", SessionID: "session", Role: "assistant", Content: []byte(tc.original)}
			loaded := original
			loaded.Content = []byte(tc.loaded)
			store := &contextSourcePersister{rows: []messagepkg.Message{loaded}}
			committer := &agentStepCommitter{service: &Service{messageService: store}}
			got, err := committer.committedContextSources(t.Context(), 1, []int{0}, []messagepkg.Message{original})
			if err != nil {
				t.Fatal(err)
			}
			if (got[0].ID != "") != tc.accepted {
				t.Fatalf("receipt=%+v accepted=%v", got[0], tc.accepted)
			}
		})
	}
}

func TestSourceReadFailureKeepsCommittedStepAndDoesNotReplay(t *testing.T) {
	readErr := errors.New("receipt read failed")
	store := &contextSourcePersister{recordingStepPersister: &recordingStepPersister{recordingMessageService: &recordingMessageService{}}, readErr: readErr}
	committer := &agentStepCommitter{
		service: &Service{messageService: store}, persister: store, ownerContext: t.Context(),
		req: ChatRequest{BotID: "bot", ThreadID: "session", RunID: "run", Query: "original request"},
		rc:  resolvedContext{runConfig: native.RunConfig{ContextLifecycle: contextfrag.NewLifecycleHolder()}},
	}
	record := &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("completed effect")}}
	if _, err := committer.commit(t.Context(), 0, record); !errors.Is(err, readErr) {
		t.Fatalf("source read failure lost: %v", err)
	}
	if len(committer.persisted) != 2 || len(committer.messages) != 2 || committer.nextStep != 1 {
		t.Fatalf("durable step was forgotten: rows=%d messages=%d next=%d", len(committer.persisted), len(committer.messages), committer.nextStep)
	}
	if _, err := committer.commit(t.Context(), 0, record); err == nil || len(store.steps) != 1 {
		t.Fatalf("failed receipt replayed committed step: err=%v writes=%d", err, len(store.steps))
	}
}

func TestHiddenReplacementStepKeepsUnknownSources(t *testing.T) {
	store := &contextSourcePersister{readErr: errors.New("hidden rows are not visible")}
	committer := &agentStepCommitter{
		service: &Service{messageService: store},
		req:     ChatRequest{TurnReplacement: &messagepkg.TurnReplacement{}},
	}
	got, err := committer.committedContextSources(t.Context(), 1, []int{0}, []messagepkg.Message{{ID: "hidden", SessionID: "session"}})
	if err != nil || len(got) != 1 || got[0].ID != "" {
		t.Fatalf("unpublished replacement acquired a source or failed: sources=%+v err=%v", got, err)
	}
}

func TestStepCommitCarriesExactSourcesThroughPersistence(t *testing.T) {
	store := &contextSourcePersister{recordingStepPersister: &recordingStepPersister{recordingMessageService: &recordingMessageService{}}}
	committer := &agentStepCommitter{
		service: &Service{messageService: store}, persister: store, ownerContext: t.Context(),
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
