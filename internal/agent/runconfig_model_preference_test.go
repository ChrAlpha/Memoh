package agent

import (
	"context"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
)

func TestModelOnlyPatchUsesNewModelDefaultEffort(t *testing.T) {
	provider := agenttest.ProviderRow(t, "00000000-0000-0000-0000-000000000610", "openai-completions", true)
	old := agenttest.ModelRow(t, "00000000-0000-0000-0000-000000000611", "old", provider.ID, models.ModelTypeChat, true)
	next := agenttest.ModelRow(t, "00000000-0000-0000-0000-000000000612", "next", provider.ID, models.ModelTypeChat, true)
	next.Config = []byte(`{"thinking_mode":"toggle","reasoning_efforts":["low","medium","high"]}`)
	fake := &agenttest.ModelQueries{Models: map[string]sqlc.Model{"old": old, "next": next}, Provider: provider, Session: sqlc.BotSession{ID: old.ID, PreferredChatModelID: old.ID, PreferredReasoningEffort: pgtype.Text{String: "high", Valid: true}}}
	svc := newTestService(t, Deps{Models: models.NewService(slog.New(slog.DiscardHandler), fake), Queries: fake})
	ref := next.ID.String()
	_, defaultEffort, err := svc.runconfig.ReconcilePreference(context.Background(), "bot", ref, "")
	if err != nil {
		t.Fatal(err)
	}
	err = svc.PatchSessionModelPreference(context.Background(), "bot", "00000000-0000-0000-0000-000000000613", &ref, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.PatchedPrefs) != 1 {
		t.Fatal(fake.PatchedPrefs)
	}
	got := fake.PatchedPrefs[0].PreferredReasoningEffort.String
	if got != defaultEffort {
		t.Fatalf("model-only PATCH carried old effort %q; new model default is %q", got, defaultEffort)
	}
}
