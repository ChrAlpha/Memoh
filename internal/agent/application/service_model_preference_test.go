package application

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
)

// Mechanism tests for the session model preference (issue #879, spec v2
// §4.3): resolution chain order, the session level inside reasoning
// resolution, and the per-turn write-back gate.

// Write-back gate (spec §3.3): only a request that CARRIES the pair writes;
// every carried pair rotates the revision; the stored value is the resolved
// pair in its normalized vocabulary.
func TestWriteBackSessionModelPreference(t *testing.T) {
	ctx := context.Background()
	const (
		sessionID = "00000000-0000-0000-0000-000000000615"
		modelUUID = "00000000-0000-0000-0000-000000000616"
	)
	chatModel := models.GetResponse{ID: modelUUID, Model: models.Model{ModelID: "gpt-pref"}}
	active := &models.ReasoningConfig{Active: true, Effort: "high"}

	t.Run("request without the pair never writes", func(t *testing.T) {
		fake := &agenttest.ModelQueries{}
		svc := newModelSelectionService(t, fake)
		svc.writeBackSessionModelPreference(ctx, sessionID, false, chatModel, active)
		if len(fake.UpdatedPrefs) != 0 {
			t.Fatalf("writes = %d, want 0", len(fake.UpdatedPrefs))
		}
	})

	t.Run("carried pair writes the resolved values", func(t *testing.T) {
		fake := &agenttest.ModelQueries{}
		svc := newModelSelectionService(t, fake)
		svc.writeBackSessionModelPreference(ctx, sessionID, true, chatModel, active)
		if len(fake.UpdatedPrefs) != 1 {
			t.Fatalf("writes = %d, want 1", len(fake.UpdatedPrefs))
		}
		got := fake.UpdatedPrefs[0]
		if !got.PreferredChatModelID.Valid || got.PreferredChatModelID.String() != modelUUID {
			t.Fatalf("model = %v, want %s", got.PreferredChatModelID, modelUUID)
		}
		if !got.PreferredReasoningEffort.Valid || got.PreferredReasoningEffort.String != "high" {
			t.Fatalf("effort = %v, want high", got.PreferredReasoningEffort)
		}
	})

	t.Run("disabled thinking stores the disable vocabulary", func(t *testing.T) {
		fake := &agenttest.ModelQueries{}
		svc := newModelSelectionService(t, fake)
		svc.writeBackSessionModelPreference(ctx, sessionID, true, chatModel, &models.ReasoningConfig{Disabled: true})
		if len(fake.UpdatedPrefs) != 1 || fake.UpdatedPrefs[0].PreferredReasoningEffort.String != "disable" {
			t.Fatalf("writes = %+v, want one write with effort=disable", fake.UpdatedPrefs)
		}
	})

	t.Run("reasoning-less model stores NULL effort", func(t *testing.T) {
		fake := &agenttest.ModelQueries{}
		svc := newModelSelectionService(t, fake)
		svc.writeBackSessionModelPreference(ctx, sessionID, true, chatModel, nil)
		if len(fake.UpdatedPrefs) != 1 || fake.UpdatedPrefs[0].PreferredReasoningEffort.Valid {
			t.Fatalf("writes = %+v, want one write with NULL effort", fake.UpdatedPrefs)
		}
	})
}

func TestModelOnlyPatchUsesNewModelDefaultEffort(t *testing.T) {
	provider := agenttest.ProviderRow(t, "00000000-0000-0000-0000-000000000610", "openai-completions", true)
	old := agenttest.ModelRow(t, "00000000-0000-0000-0000-000000000611", "old", provider.ID, models.ModelTypeChat, true)
	next := agenttest.ModelRow(t, "00000000-0000-0000-0000-000000000612", "next", provider.ID, models.ModelTypeChat, true)
	next.Config = []byte(`{"thinking_mode":"toggle","reasoning_efforts":["low","medium","high"]}`)
	fake := &agenttest.ModelQueries{Models: map[string]sqlc.Model{"old": old, "next": next}, Provider: provider, Session: sqlc.BotSession{ID: old.ID, PreferredChatModelID: old.ID, PreferredReasoningEffort: pgtype.Text{String: "high", Valid: true}}}
	svc := newModelSelectionService(t, fake)
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
