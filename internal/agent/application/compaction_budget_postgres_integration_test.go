package application

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func (f progressFixture) scanThrough(id string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE bot_sessions
		SET compaction_scan_after=$2, compaction_scan_epoch=compaction_epoch, compaction_scan_at=clock_timestamp()
		WHERE id=$1`, f.sessionID, id); err != nil {
		f.t.Fatal(err)
	}
}

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

func TestPostgresCompactionBudgetsProtectedInputOnce(t *testing.T) {
	for _, placement := range []string{"before_scan", "all_before_scan", "pending", "oversized"} {
		t.Run(placement, func(t *testing.T) {
			capWith := func(size int) int {
				f, store := newProgressFixture(t)
				model := &countingSummarizer{summary: strings.Repeat("s", 8000)}
				f.text("assistant", strings.Repeat("archived detail. ", 3000))
				f.reasoning()
				svc := compaction.NewService(slog.New(slog.DiscardHandler), store)
				if res, err := svc.RunCompactionSync(f.ctx, f.config(model)); err != nil || res.Status != compaction.StatusOK {
					t.Fatalf("seed summary: %+v, %v", res, err)
				}
				barriers := f.exchange("ask_user", "guard", "continue?")
				raw, _ := json.Marshal(map[string]any{"role": "user", "content": strings.Repeat("I", size)})
				input, err := f.messages.Persist(f.ctx, messagepkg.PersistInput{
					BotID: f.botID, SessionID: f.sessionID, Role: "user", Content: raw, ExternalMessageID: "current-input",
				})
				if err != nil {
					t.Fatal(err)
				}
				f.reasoning()
				later := f.text("assistant", strings.Repeat("later ", 40))
				end := f.text("user", "later question")
				switch placement {
				case "before_scan":
					f.scanThrough(input.ID)
				case "all_before_scan":
					f.scanThrough(end.ID)
				case "pending":
					f.pending(input.ID)
					f.scanThrough(barriers[len(barriers)-1].ID)
				case "oversized":
					f.scanThrough(barriers[len(barriers)-1].ID)
				}
				model.summary, model.calls = "short replacement", 0
				cfg := f.config(model)
				cfg.TargetTokens, cfg.HistoryBudgetTokens, cfg.SummaryWindowTokens = 600, 500, 128000
				cfg.AllowFrontierFusion = true
				cfg.ProtectedSources = []turn.ContextMessageSource{{Kind: "external", ID: "current-input", Current: true}}
				res, err := compaction.NewService(slog.New(slog.DiscardHandler), store).RunCompactionSync(f.ctx, cfg)
				if err != nil || res.Status != compaction.StatusProgress || model.calls != 1 || res.MessageCount != 0 {
					t.Fatalf("recovery: %+v, %v calls=%d; want summary-only progress", res, err, model.calls)
				}
				for _, row := range []messagepkg.Message{input, later, end} {
					if _, status := f.claim(row.ID); status == "ok" {
						t.Fatal("protected input or its suffix was summarized")
					}
				}
				if model.maxTokens <= 0 || model.maxTokens >= cfg.HistoryBudgetTokens {
					t.Fatalf("output cap=%d did not reserve retained history", model.maxTokens)
				}
				return model.maxTokens
			}
			large := 6000
			if placement == "oversized" {
				large = 9 << 20
			}
			if small, large := capWith(40), capWith(large); small != large {
				t.Fatalf("output caps %d / %d: the caller already budgets current input", small, large)
			}
		})
	}
}

func TestPostgresCompactionBudgetsUnreadImagesOnce(t *testing.T) {
	var baseline int
	for _, size := range []int{0, 40000, 9 << 20} {
		f, store := newProgressFixture(t)
		barriers := f.exchange("ask_user", "guard", "continue?")
		uri := "https://example.invalid/a.png"
		if size > 0 {
			uri = "data:image/png;base64," + strings.Repeat("A", size)
		}
		image := f.persist("user", []map[string]any{{"type": "image", "image": uri, "mediaType": "image/png"}})
		end := f.reasoning()
		later := f.text("assistant", strings.Repeat("later history. ", 300))
		f.text("user", "current question")
		if size < 9<<20 {
			f.scanThrough(end.ID)
		} else {
			f.scanThrough(barriers[len(barriers)-1].ID)
		}
		model := &countingSummarizer{summary: "short summary"}
		cfg := f.config(model)
		cfg.TargetTokens, cfg.HistoryBudgetTokens = 600, 2100
		res, err := compaction.NewService(slog.New(slog.DiscardHandler), store).RunCompactionSync(f.ctx, cfg)
		_, status := f.claim(later.ID)
		claim, _ := f.claim(image.ID)
		if err != nil || model.calls != 1 || status != "ok" || claim != "" {
			t.Fatalf("image bytes=%d: %+v, %v calls=%d; want later history summarized and image retained", size, res, err, model.calls)
		}
		if model.maxTokens < 200 || model.maxTokens > 600 {
			t.Fatalf("output cap=%d; the retained image needs 1500 tokens", model.maxTokens)
		}
		if size == 0 {
			baseline = model.maxTokens
		} else if delta := model.maxTokens - baseline; delta < -30 || delta > 30 {
			t.Fatalf("image bytes=%d: output cap %d versus URL cap %d; encoded bytes must not be charged again", size, model.maxTokens, baseline)
		}
	}
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
