package agenttest

import (
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// Identifiers of the run the lifecycle tests act on.
const (
	LifecycleRunID     = "11111111-1111-4111-8111-111111111111"
	LifecycleBotID     = "22222222-2222-4222-8222-222222222222"
	LifecycleSessionID = "33333333-3333-4333-8333-333333333333"
)

// LifecycleRunConfig is the run configuration of the run above, carrying a
// content-light lifecycle snapshot with the given extra mutations.
func LifecycleRunConfig(mutations ...contextfrag.MutationRecord) native.RunConfig {
	ledger := contextfrag.NewMutationLedger()
	for _, mutation := range mutations {
		ledger.Record(mutation.Kind, mutation.Detail)
	}
	holder := contextfrag.NewLifecycleHolder()
	holder.SetManifest(contextfrag.Manifest{
		View: contextfrag.ViewRunConfigPreProvider,
		Counts: contextfrag.ManifestCounts{
			Fragments: 2,
			Messages:  1,
			TextBytes: 64,
		},
		Items:     []contextfrag.ManifestItem{{ID: "private-content-marker"}},
		Mutations: ledger,
	})
	return native.RunConfig{
		RunID: LifecycleRunID,
		Identity: native.SessionContext{
			BotID:     LifecycleBotID,
			SessionID: LifecycleSessionID,
		},
		ContextLifecycle: holder,
	}
}

// UUID parses value as a UUID and panics when it is not one.
func UUID(value string) pgtype.UUID {
	var out pgtype.UUID
	if err := out.Scan(value); err != nil {
		panic(err)
	}
	return out
}
