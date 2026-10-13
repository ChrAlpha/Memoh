package application

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func (f progressFixture) pending(id string) pgtype.UUID {
	f.t.Helper()
	state, err := f.queries.GetCompactionScan(f.ctx, f.uuid(f.sessionID))
	if err != nil {
		f.t.Fatal(err)
	}
	claim, err := f.queries.CreateCompactionLog(f.ctx, dbsqlc.CreateCompactionLogParams{
		BotID: f.uuid(f.botID), SessionID: f.uuid(f.sessionID), ExpectedEpoch: state.CompactionEpoch,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if n, err := f.queries.MarkMessagesCompacted(f.ctx, dbsqlc.MarkMessagesCompactedParams{
		CompactID: claim.ID, MessageIds: []pgtype.UUID{f.uuid(id)}, ExpectedCompactIds: []pgtype.UUID{{}},
	}); err != nil || n != 1 {
		f.t.Fatalf("pending claim: %d, %v", n, err)
	}
	return claim.ID
}

func TestPostgresCompactionRevisitsAnExpiredPendingClaim(t *testing.T) {
	f, store := newProgressFixture(t)
	short := f.text("user", "short question")
	held := f.text("assistant", strings.Repeat("pending history. ", 300))
	later := f.text("assistant", strings.Repeat("later history. ", 300))
	f.text("user", "current question")
	claim := f.pending(held.ID)
	model := &countingSummarizer{summary: strings.Repeat("s", 4*254-21)}
	cfg := f.config(model)
	svc := compaction.NewService(slog.New(slog.DiscardHandler), store)
	for pass := 0; pass < 2; pass++ {
		_, _ = svc.RunCompactionSync(f.ctx, cfg)
	}
	if _, status := f.claim(later.ID); status != "ok" {
		t.Fatal("fresh pending claim blocked later history")
	}
	if _, err := f.pool.Exec(f.ctx, "UPDATE bot_history_message_compacts SET started_at=now()-INTERVAL '16 minutes' WHERE id=$1", claim); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 3; pass++ {
		_, _ = svc.RunCompactionSync(f.ctx, cfg)
		if id, status := f.claim(short.ID); status == "ok" {
			if other, status := f.claim(held.ID); other != id || status != "ok" {
				t.Fatal("expired claim was not reconsidered with the preceding short row")
			}
			return
		}
	}
	t.Fatal("expired claim remained behind the scan cursor")
}
