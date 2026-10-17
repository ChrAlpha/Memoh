package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

// A summary written before artifacts carried coverage is folded into the next
// summary, and the read path accepts the lineage: one active summary covering
// both the legacy rows and the newly compacted ones.
func TestPostgresCompactionFoldsALegacySummaryIntoOne(t *testing.T) {
	f := newBacklogFixture(t)
	var legacyID string
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO bot_history_message_compacts(bot_id,session_id,status,summary,completed_at) VALUES($1,$2,'ok','Legacy summary.',now()) RETURNING id`, f.botID, f.sessionID).Scan(&legacyID); err != nil {
		t.Fatal(err)
	}
	legacyRows := map[string]bool{}
	for _, text := range []string{"legacy question", "legacy answer"} {
		content, _ := json.Marshal(text)
		msg, err := f.messages.Persist(f.ctx, messagepkg.PersistInput{BotID: f.botID, SessionID: f.sessionID, Role: "user", Content: content})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(f.ctx, `UPDATE bot_history_messages SET compact_id=$2 WHERE id=$1`, msg.ID, legacyID); err != nil {
			t.Fatal(err)
		}
		legacyRows[msg.ID] = true
	}
	f.appendBacklog(4, 400)

	queries := postgresstore.NewQueriesWithPool(f.pool, sqlc.New(f.pool))
	res, err := compaction.NewService(slog.Default(), queries).RunCompactionSync(f.ctx, compaction.TriggerConfig{
		BotID: f.botID, SessionID: f.sessionID, ModelID: "compact-model", ClientType: "openai-completions",
		APIKey: "test-key", BaseURL: f.providerURL, SummaryWindowTokens: 128000, HistoryBudgetTokens: 100000,
	})
	if err != nil || res.Status != compaction.StatusOK {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	frontier, err := compaction.NewArtifactProjection(queries).LoadActiveSession(f.ctx, compaction.ArtifactOwner{BotID: f.botID, SessionID: f.sessionID, SessionIDKnown: true})
	if err != nil || len(frontier.Issues) != 0 || len(frontier.Artifacts) != 1 {
		t.Fatalf("frontier=%+v err=%v, want one active summary and no lineage issue", frontier, err)
	}
	active := frontier.Artifacts[0]
	if len(active.ParentIDs) != 1 || active.ParentIDs[0] != legacyID {
		t.Fatalf("active summary parents=%v, want the legacy summary %s", active.ParentIDs, legacyID)
	}
	covered := 0
	for _, source := range active.Coverage {
		if legacyRows[source.Ref.ID] {
			covered++
		}
	}
	if covered != len(legacyRows) || len(active.Coverage) != len(legacyRows)+res.MessageCount {
		t.Fatalf("coverage=%d legacy=%d/%d new=%d, want legacy and new rows covered once", len(active.Coverage), covered, len(legacyRows), res.MessageCount)
	}
}

// Budget recovery only summarizes rows the request actually loaded: an older
// backlog outside the loaded view stays raw instead of being folded into a
// summary the request then has to carry on top of the view.
func TestPostgresCompactionLeavesBacklogOutsideTheViewRaw(t *testing.T) {
	f := newBacklogFixture(t)
	f.appendBacklog(3, 400)
	if _, err := f.pool.Exec(f.ctx, `UPDATE bot_history_messages SET created_at=created_at - interval '2 days' WHERE session_id=$1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		f.appendMessage(fmt.Sprintf("view-%d", i), fmt.Sprintf("view-%d ", i)+strings.Repeat("y", 400))
	}

	queries := postgresstore.NewQueriesWithPool(f.pool, sqlc.New(f.pool))
	res, err := compaction.NewService(slog.Default(), queries).RunCompactionSync(f.ctx, compaction.TriggerConfig{
		BotID: f.botID, SessionID: f.sessionID, ModelID: "compact-model", ClientType: "openai-completions",
		APIKey: "test-key", BaseURL: f.providerURL, SummaryWindowTokens: 128000, HistoryBudgetTokens: 100000,
		CandidatesFrom: f.base,
	})
	if err != nil || res.Status != compaction.StatusOK {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if raw, compacted := f.backlogRows(); raw != 3 || compacted != 0 {
		t.Fatalf("backlog outside the view: raw=%d compacted=%d, want all 3 left raw", raw, compacted)
	}
	if _, compacted := f.backlogRowsPrefixed("view-"); compacted == 0 {
		t.Fatal("rows inside the view were not compacted")
	}
}
