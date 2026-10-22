package plan

import (
	"context"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/models"
)

// Mechanism tests for the session model preference (issue #879, spec v2
// §4.3): resolution chain order, the session level inside reasoning
// resolution, and the per-turn write-back gate.

// Write-back gate (spec §3.3): only a request that CARRIES the pair writes;
// every carried pair rotates the revision; the stored value is the resolved
// pair in its normalized vocabulary.
func TestWriteBackModelPreference(t *testing.T) {
	ctx := context.Background()
	const (
		sessionID = "00000000-0000-0000-0000-000000000615"
		modelUUID = "00000000-0000-0000-0000-000000000616"
	)
	chatModel := models.GetResponse{ID: modelUUID, Model: models.Model{ModelID: "gpt-pref"}}
	active := &models.ReasoningConfig{Active: true, Effort: "high"}

	t.Run("request without the pair never writes", func(t *testing.T) {
		fake := &agenttest.ModelQueries{}
		planner := New(Deps{Store: fake})
		planner.writeBackModelPreference(ctx, sessionID, false, chatModel, active)
		if len(fake.UpdatedPrefs) != 0 {
			t.Fatalf("writes = %d, want 0", len(fake.UpdatedPrefs))
		}
	})

	t.Run("carried pair writes the resolved values", func(t *testing.T) {
		fake := &agenttest.ModelQueries{}
		planner := New(Deps{Store: fake})
		planner.writeBackModelPreference(ctx, sessionID, true, chatModel, active)
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
		planner := New(Deps{Store: fake})
		planner.writeBackModelPreference(ctx, sessionID, true, chatModel, &models.ReasoningConfig{Disabled: true})
		if len(fake.UpdatedPrefs) != 1 || fake.UpdatedPrefs[0].PreferredReasoningEffort.String != "disable" {
			t.Fatalf("writes = %+v, want one write with effort=disable", fake.UpdatedPrefs)
		}
	})

	t.Run("reasoning-less model stores NULL effort", func(t *testing.T) {
		fake := &agenttest.ModelQueries{}
		planner := New(Deps{Store: fake})
		planner.writeBackModelPreference(ctx, sessionID, true, chatModel, nil)
		if len(fake.UpdatedPrefs) != 1 || fake.UpdatedPrefs[0].PreferredReasoningEffort.Valid {
			t.Fatalf("writes = %+v, want one write with NULL effort", fake.UpdatedPrefs)
		}
	})
}
