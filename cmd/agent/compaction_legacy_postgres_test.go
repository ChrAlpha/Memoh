package main

import (
	"encoding/json"
	"log/slog"
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
		APIKey: "test-key", BaseURL: f.providerURL, TargetTokens: 1,
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
