package runconfig

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
)

// Chain: request > session memory > bot default (spec §3.2). The history
// fallback lives below the bot default and is already covered by
// TestSelectChatModelFallsBackToSessionLastModel.
func TestSelectChatModelSessionMemoryLevel(t *testing.T) {
	ctx := context.Background()
	provider := agenttest.ProviderRow(t, "00000000-0000-0000-0000-000000000610", "openai-completions", true)
	botModel := agenttest.ModelRow(t, "00000000-0000-0000-0000-000000000611", "gpt-bot", provider.ID, models.ModelTypeChat, true)
	prefModel := agenttest.ModelRow(t, "00000000-0000-0000-0000-000000000612", "gpt-pref", provider.ID, models.ModelTypeChat, true)
	fake := &agenttest.ModelQueries{
		Models: map[string]sqlc.Model{
			botModel.ModelID:  botModel,
			prefModel.ModelID: prefModel,
		},
		Provider: provider,
	}
	resolver := newModelSelectionBuilder(fake)
	botSettings := settings.Settings{ChatModelID: botModel.ID.String()}

	// Session memory beats the bot default.
	got, _, err := resolver.selectChatModel(ctx, runreq.ChatRequest{
		BotID:    "00000000-0000-0000-0000-000000000613",
		ThreadID: "00000000-0000-0000-0000-000000000614",
	}, botSettings, prefModel.ID.String())
	if err != nil {
		t.Fatalf("selectChatModel with session memory error = %v, want nil", err)
	}
	if got.ModelID != "gpt-pref" {
		t.Fatalf("model_id = %q, want session memory %q", got.ModelID, "gpt-pref")
	}

	// The request's own model beats session memory.
	got, _, err = resolver.selectChatModel(ctx, runreq.ChatRequest{
		BotID:    "00000000-0000-0000-0000-000000000613",
		ThreadID: "00000000-0000-0000-0000-000000000614",
		Model:    "gpt-bot",
	}, botSettings, prefModel.ID.String())
	if err != nil {
		t.Fatalf("selectChatModel with request model error = %v, want nil", err)
	}
	if got.ModelID != "gpt-bot" {
		t.Fatalf("model_id = %q, want request model %q", got.ModelID, "gpt-bot")
	}
}

// The session's remembered effort sits between the per-message request and
// the bot's stored value (spec §3.2): request > session memory > bot stored
// > model default. It must never be passed as the request level — that one
// travels to spawned subagents as "the user's explicit pick this turn".
func TestResolveReasoningConfigSessionLevel(t *testing.T) {
	toggleModel := models.GetResponse{
		Model: models.Model{
			Config: models.ModelConfig{
				ThinkingMode:     models.ThinkingModeToggle,
				ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"},
			},
		},
	}
	disablableModel := models.GetResponse{
		Model: models.Model{
			Config: models.ModelConfig{
				ThinkingMode:     models.ThinkingModeToggle,
				ReasoningEfforts: []string{models.ReasoningEffortDisable, "low", "medium", "high"},
			},
		},
	}
	const clientType = "openai-completions"

	tests := []struct {
		name         string
		model        models.GetResponse
		stored       string
		session      string
		requested    string
		wantEffort   string
		wantDisabled bool
	}{
		{name: "session beats stored", model: toggleModel, stored: "low", session: "high", requested: "", wantEffort: "high"},
		// "max" would be the wrong probe here: the generic OpenAI wire drops
		// it, so an illegal request tier would test wire policy, not ordering.
		{name: "request beats session", model: toggleModel, stored: "low", session: "high", requested: "xhigh", wantEffort: "xhigh"},
		{name: "empty session keeps stored", model: toggleModel, stored: "low", session: "", requested: "", wantEffort: "low"},
		{name: "illegal session tier falls to model default", model: toggleModel, stored: "", session: "bogus", requested: "", wantEffort: "medium"},
		{name: "session disable on disablable model", model: disablableModel, stored: "", session: "disable", requested: "", wantDisabled: true},
		// A remembered "disable" on a model that cannot turn off reconciles to
		// the model's default-on tier — the DB never replays an illegal pair.
		{name: "session disable reconciled on always-on model", model: toggleModel, stored: "", session: "disable", requested: "", wantEffort: "medium"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveReasoningConfig(tt.model, settings.Settings{ReasoningEffort: tt.stored}, tt.requested, tt.session, clientType)
			if got == nil {
				t.Fatal("resolveReasoningConfig returned nil, want a decision")
			}
			if got.Disabled != tt.wantDisabled {
				t.Fatalf("Disabled = %v, want %v", got.Disabled, tt.wantDisabled)
			}
			if !tt.wantDisabled && got.Effort != tt.wantEffort {
				t.Fatalf("Effort = %q, want %q", got.Effort, tt.wantEffort)
			}
		})
	}
}

// A half pair (NULL model, surviving effort) arises when ON DELETE SET NULL
// clears only the model column. The effort must be dropped with the model —
// honoring it alone would pin it onto whatever model the chain falls back
// to, the cross-model effort memory the spec rules out (§3.7). A full pair
// still round-trips.
func TestSessionModelPreferenceHalfPair(t *testing.T) {
	ctx := context.Background()
	const sessionID = "00000000-0000-0000-0000-000000000617"

	var id pgtype.UUID
	if err := id.Scan(sessionID); err != nil {
		t.Fatalf("scan session id: %v", err)
	}

	newSvc := func(session sqlc.BotSession) *Builder {
		return newModelSelectionBuilder(&agenttest.ModelQueries{Session: session})
	}

	t.Run("NULL model drops the surviving effort", func(t *testing.T) {
		svc := newSvc(sqlc.BotSession{
			ID:                       id,
			PreferredReasoningEffort: pgtype.Text{String: "high", Valid: true},
		})
		modelID, effort := svc.sessionModelPreference(ctx, sessionID)
		if modelID != "" || effort != "" {
			t.Fatalf("half pair = (%q, %q), want no memory", modelID, effort)
		}
	})

	t.Run("full pair round-trips", func(t *testing.T) {
		var modelUUID pgtype.UUID
		if err := modelUUID.Scan("00000000-0000-0000-0000-000000000618"); err != nil {
			t.Fatalf("scan model id: %v", err)
		}
		svc := newSvc(sqlc.BotSession{
			ID:                       id,
			PreferredChatModelID:     modelUUID,
			PreferredReasoningEffort: pgtype.Text{String: "high", Valid: true},
		})
		modelID, effort := svc.sessionModelPreference(ctx, sessionID)
		if modelID != modelUUID.String() || effort != "high" {
			t.Fatalf("full pair = (%q, %q), want (%s, high)", modelID, effort, modelUUID.String())
		}
	})
}

func TestRunReasoningKeepsEffortWithResolvedModel(t *testing.T) {
	const modelA = "00000000-0000-0000-0000-000000000611"
	const modelB = "00000000-0000-0000-0000-000000000612"
	for _, tc := range []struct{ name, resolved, requestedModel, requestedEffort, want string }{
		{"switch uses model default", modelB, "model-b", "", "medium"},
		{"same model slug keeps memory", modelA, "model-a", "", "high"},
		{"explicit effort wins on switch", modelB, "model-b", "low", "low"},
		{"omitted model keeps memory", modelA, "", "", "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := models.GetResponse{ID: tc.resolved, Model: models.Model{Config: models.ModelConfig{ThinkingMode: models.ThinkingModeToggle, ReasoningEfforts: []string{"low", "medium", "high"}}}}
			got := resolveRunReasoningConfig(model, settings.Settings{ChatModelID: modelA, ReasoningEffort: "high"}, baseParams{Model: tc.requestedModel, ReasoningEffort: tc.requestedEffort, SessionPrefModelID: modelA, SessionPrefEffort: "high"}, "openai-completions")
			if got == nil || got.Effort != tc.want {
				t.Fatalf("reasoning = %+v, want %s", got, tc.want)
			}
		})
	}
}
