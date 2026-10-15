package compaction

import (
	"context"
	"strings"
	"testing"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

type scopedReadQueries struct{ *fakeQueries }

func (q scopedReadQueries) ListUncompactedMessagesBySessionWithinBytes(ctx context.Context, params sqlc.ListUncompactedMessagesBySessionWithinBytesParams) ([]sqlc.ListUncompactedMessagesBySessionWithinBytesRow, error) {
	rows, err := q.fakeQueries.ListUncompactedMessagesBySessionWithinBytes(ctx, params)
	if err != nil {
		return nil, err
	}
	end := 0
	for end < len(rows) && rows[end].CumulativeBytes <= params.MaxBytes {
		end++
	}
	return rows[:end], nil
}

func TestScopedReadReachesEligibleTailWithoutCrossingRetainedInput(t *testing.T) {
	head := mkRow(t, "assistant", jsonStr("excluded old record"), 100)
	head.Metadata = []byte(`{"diagnostic":"` + strings.Repeat("d", 600<<10) + `"}`)
	first := mkRow(t, "assistant", jsonStr(strings.Repeat("completed A ", 100)), 1000)
	barrier := mkRow(t, "user", jsonStr("unconsumed instruction"), 100)
	second := mkRow(t, "assistant", jsonStr(strings.Repeat("completed B ", 100)), 1000)
	queries := scopedReadQueries{&fakeQueries{uncompacted: []sqlc.ListUncompactedMessagesBySessionRow{head, first, barrier, second}}}
	model := &stubModel{summary: "completed A summarized"}
	cfg := machineryConfig(model, 200)
	cfg.EligibleSources = []contextfrag.ContextRef{scopedRowRef(t, first), scopedRowRef(t, second)}
	result, err := newMachineryService(queries).RunCompactionSync(t.Context(), cfg)
	if err != nil || result.Status != StatusOK || len(queries.markedIDs) != 1 || queries.markedIDs[0] != first.ID {
		t.Fatalf("scoped progress crossed barrier or stayed blocked: result=%+v error=%v claims=%v calls=%d", result, err, queries.markedIDs, model.calls)
	}
}

func TestScopedReadDoesNotJoinToolExchangeAcrossOmittedInput(t *testing.T) {
	call, result := toolCallRow(t, 100), toolResultRow(t, 100)
	barrier := mkRow(t, "user", jsonStr("unconsumed instruction"), 100)
	queries := scopedReadQueries{&fakeQueries{uncompacted: []sqlc.ListUncompactedMessagesBySessionRow{call, barrier, result}}}
	model := &stubModel{summary: "invalid joined exchange"}
	cfg := machineryConfig(model, 100)
	cfg.EligibleSources = []contextfrag.ContextRef{scopedRowRef(t, call), scopedRowRef(t, result)}
	got, err := newMachineryService(queries).RunCompactionSync(t.Context(), cfg)
	if err != nil || got.Status != StatusNoop || model.calls != 0 || len(queries.markedIDs) != 0 {
		t.Fatalf("joined across omitted input: result=%+v error=%v claims=%v", got, err, queries.markedIDs)
	}
}
