package checkpoint

import (
	"testing"
	"time"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbpkg "github.com/felinics/memoh/internal/db"
)

// turns writes a user and an assistant row in each turn of the session from
// first through last, and returns them by turn.
func (f *historyFixture) turnsOf(session string, first, last int64) map[int64][2]string {
	f.t.Helper()
	rows := make(map[int64][2]string)
	for turn := first; turn <= last; turn++ {
		rows[turn] = [2]string{f.row(session, turn, 1), f.row(session, turn, 2)}
	}
	return rows
}

// keepHistoryUnderCheckpointsFromNow makes every row written so far predate
// the bot's checkpoints, as the migration that introduced them does for the
// history it finds.
func (f *historyFixture) keepHistoryUnderCheckpointsFromNow() {
	f.t.Helper()
	f.exec(`UPDATE bots SET history_checkpoints_since = clock_timestamp() WHERE id = $1`, f.botID)
}

// contentBytes is what the history load counts of the rows: their content.
func (f *historyFixture) contentBytes(ids ...string) int64 {
	f.t.Helper()
	var total int64
	if err := f.pool.QueryRow(f.ctx, `SELECT COALESCE(SUM(octet_length(content::text)), 0) FROM bot_history_messages WHERE id = ANY($1::uuid[])`, ids).Scan(&total); err != nil {
		f.t.Fatalf("measure content: %v", err)
	}
	return total
}

func (f *historyFixture) createdAt(id string) time.Time {
	f.t.Helper()
	var at time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT created_at FROM bot_history_messages WHERE id = $1`, id).Scan(&at); err != nil {
		f.t.Fatalf("read created_at: %v", err)
	}
	return at
}

func (f *historyFixture) stepOver(before *messagepkg.HistoryPosition, limits StepOverLimits) int64 {
	f.t.Helper()
	omitted, err := f.store.StepOver(f.ctx, f.session, before, limits)
	if err != nil {
		f.t.Fatalf("step over: %v", err)
	}
	return omitted
}

// A request renders the newest content of the history from before
// checkpoints within its load's byte budget. The step-over omits the turns
// past it, through the oldest after the checkpoint, keeps the turns inside
// it whole, records what it omits, and is done: a second one finds nothing
// to omit.
func TestPostgresStepOverOmitsWhatARequestCannotRender(t *testing.T) {
	f := newHistoryFixture(t)
	rows := f.turnsOf(f.session, 1, 10)
	f.keepHistoryUnderCheckpointsFromNow()

	window := f.contentBytes(rows[8][0], rows[8][1], rows[9][0], rows[9][1], rows[10][0], rows[10][1])
	if omitted := f.stepOver(nil, StepOverLimits{ContentBytes: window}); omitted != 14 {
		t.Fatalf("step-over omitted %d rows, want the 14 of turns 1-7", omitted)
	}
	got := f.active()
	if got.ParentID != "" || got.Position != (messagepkg.HistoryPosition{Turn: 7, Seq: 2}) || len(got.Items) != 0 {
		t.Fatalf("checkpoint = %+v, want one at 7.2 that holds nothing", got)
	}
	if got.Omitted.Count != 14 || !got.Omitted.First.Equal(f.createdAt(rows[1][0])) || !got.Omitted.Last.Equal(f.createdAt(rows[7][1])) {
		t.Fatalf("omitted = %+v, want turns 1-7", got.Omitted)
	}
	if omitted := f.stepOver(nil, StepOverLimits{ContentBytes: window}); omitted != 0 || f.active().ID != got.ID {
		t.Fatalf("second step-over omitted %d rows", omitted)
	}

	// A wider budget renders all of it: nothing to omit.
	g := newHistoryFixture(t)
	all := g.turnsOf(g.session, 1, 3)
	g.keepHistoryUnderCheckpointsFromNow()
	if omitted := g.stepOver(nil, StepOverLimits{ContentBytes: g.contentBytes(all[1][0], all[1][1], all[2][0], all[2][1], all[3][0], all[3][1])}); omitted != 0 || g.pointer(g.session) != "" {
		t.Fatalf("history inside the budget: omitted %d rows, checkpoint %q", omitted, g.pointer(g.session))
	}
}

// Only history from before checkpoints is ever omitted. Rows written since
// pass the walk at no cost, so a session created since never steps over,
// whatever its size, and in an older session the newer turns stay.
func TestPostgresStepOverNeverOmitsHistoryWrittenSinceCheckpoints(t *testing.T) {
	f := newHistoryFixture(t)
	f.turnsOf(f.session, 1, 6)
	if omitted := f.stepOver(nil, StepOverLimits{ContentBytes: 1}); omitted != 0 || f.pointer(f.session) != "" {
		t.Fatalf("a session written since checkpoints stepped over %d rows", omitted)
	}

	g := newHistoryFixture(t)
	legacy := g.turnsOf(g.session, 1, 4)
	g.keepHistoryUnderCheckpointsFromNow()
	g.turnsOf(g.session, 5, 8)
	if omitted := g.stepOver(nil, StepOverLimits{ContentBytes: g.contentBytes(legacy[4][0], legacy[4][1])}); omitted != 6 {
		t.Fatalf("step-over omitted %d rows, want the 6 of turns 1-3", omitted)
	}
	if got := g.active(); got.Position != (messagepkg.HistoryPosition{Turn: 3, Seq: 2}) {
		t.Fatalf("checkpoint at %+v, want 3.2: turn 4 and the turns written since stay", got.Position)
	}

	// A turn that began since checkpoints ends the step-over even among older
	// turns; a row written since into a turn that began before is that
	// turn's, and goes with it.
	h := newHistoryFixture(t)
	h.turnsOf(h.session, 1, 1)
	h.turnsOf(h.session, 3, 6)
	h.keepHistoryUnderCheckpointsFromNow()
	h.turnsOf(h.session, 2, 2)
	h.row(h.session, 1, 3)
	if omitted := h.stepOver(nil, StepOverLimits{ContentBytes: 1}); omitted != 3 {
		t.Fatalf("step-over omitted %d rows, want the 3 of turn 1 before the turn that began since", omitted)
	}
	if got := h.active(); got.Position != (messagepkg.HistoryPosition{Turn: 1, Seq: 3}) {
		t.Fatalf("checkpoint at %+v, want 1.3", got.Position)
	}
}

// The rows a checkpoint keeps by reference are older than every row after
// it. Once the history after it passes the budget they are all past it too,
// and the step-over drops the earlier ones it keeps, with their whole turns,
// while its summaries stay.
func TestPostgresStepOverDropsEarlierRowsTheCheckpointKeeps(t *testing.T) {
	f := newHistoryFixture(t)
	rows := f.turnsOf(f.session, 1, 8)
	f.keepHistoryUnderCheckpointsFromNow()
	summary := f.summary(f.session, "turns 1 and 2")
	base, err := create(f.ctx, f.queries, dbpkg.ParseUUIDOrEmpty(f.session), Checkpoint{}, messagepkg.HistoryPosition{Turn: 2, Seq: 2}, Omitted{},
		items(Item{RowID: rows[1][0]}, Item{RowID: rows[1][1]}, Item{SummaryID: summary}))
	if err != nil {
		t.Fatal(err)
	}
	if omitted := f.stepOver(nil, StepOverLimits{ContentBytes: f.contentBytes(rows[8][0], rows[8][1])}); omitted != 12 {
		t.Fatalf("step-over omitted %d rows, want the 10 of turns 3-7 and the 2 kept", omitted)
	}
	got := f.active()
	if got.ParentID != base.ID || got.Position != (messagepkg.HistoryPosition{Turn: 7, Seq: 2}) {
		t.Fatalf("checkpoint = %+v, want one at 7.2 on %s", got, base.ID)
	}
	if len(got.Items) != 1 || got.Items[0].SummaryID != summary || got.Items[0].Summary != "turns 1 and 2" {
		t.Fatalf("items = %+v, want the summary alone", got.Items)
	}
	if !got.Omitted.First.Equal(f.createdAt(rows[1][0])) {
		t.Fatalf("omitted from %v, want the first kept row", got.Omitted.First)
	}

	// Inside the budget the kept rows stay.
	g := newHistoryFixture(t)
	kept := g.turnsOf(g.session, 1, 3)
	g.keepHistoryUnderCheckpointsFromNow()
	if _, err := create(g.ctx, g.queries, dbpkg.ParseUUIDOrEmpty(g.session), Checkpoint{}, messagepkg.HistoryPosition{Turn: 2, Seq: 2}, Omitted{},
		items(Item{RowID: kept[1][0]}, Item{SummaryID: g.summary(g.session, "turn 1")})); err != nil {
		t.Fatal(err)
	}
	if omitted := g.stepOver(nil, StepOverLimits{ContentBytes: 1 << 20}); omitted != 0 {
		t.Fatalf("history inside the budget dropped %d kept rows", omitted)
	}
}

// A row larger than one compaction pass reads could never be folded: the
// event steps over through its turn, however much budget is left.
func TestPostgresStepOverStepsPastARowNoPassCanRead(t *testing.T) {
	f := newHistoryFixture(t)
	rows := f.turnsOf(f.session, 1, 6)
	f.exec(`UPDATE bot_history_messages SET metadata = jsonb_build_object('lifecycle', repeat('x', 20000)) WHERE id = $1`, rows[4][1])
	f.keepHistoryUnderCheckpointsFromNow()
	if omitted := f.stepOver(nil, StepOverLimits{ContentBytes: 1 << 20, RowPayloadBytes: 10000}); omitted != 8 {
		t.Fatalf("step-over omitted %d rows, want the 8 of turns 1-4", omitted)
	}
	if got := f.active(); got.Position != (messagepkg.HistoryPosition{Turn: 4, Seq: 2}) {
		t.Fatalf("checkpoint at %+v, want 4.2", got.Position)
	}
}

// An edit or retry's cutoff bounds the history the step-over reads.
func TestPostgresStepOverStaysBeforeTheCutoff(t *testing.T) {
	f := newHistoryFixture(t)
	rows := f.turnsOf(f.session, 1, 6)
	f.keepHistoryUnderCheckpointsFromNow()
	cutoff := messagepkg.HistoryPosition{Turn: 5, Seq: 1}
	if omitted := f.stepOver(&cutoff, StepOverLimits{ContentBytes: f.contentBytes(rows[4][0], rows[4][1])}); omitted != 6 {
		t.Fatalf("step-over omitted %d rows, want the 6 of turns 1-3", omitted)
	}
	if got := f.active(); got.Position != (messagepkg.HistoryPosition{Turn: 3, Seq: 2}) {
		t.Fatalf("checkpoint at %+v, want 3.2", got.Position)
	}
}

// A fold onto the step-over leaves out what it omits, and a row written
// into an omitted turn moves the session back before it; the next
// step-over omits that history again.
func TestPostgresAStepOverCarriesOverAndComesBackAfterARewind(t *testing.T) {
	f := newHistoryFixture(t)
	rows := f.turnsOf(f.session, 1, 6)
	f.keepHistoryUnderCheckpointsFromNow()
	limits := StepOverLimits{ContentBytes: f.contentBytes(rows[5][0], rows[5][1], rows[6][0], rows[6][1])}
	if omitted := f.stepOver(nil, limits); omitted != 8 {
		t.Fatalf("step-over omitted %d rows, want 8", omitted)
	}
	stepped := f.active()
	folded := f.mustFold(stepped, "turns 5 and 6", rows[5][0], rows[5][1], rows[6][0], rows[6][1])
	if folded.Omitted != stepped.Omitted || f.active().Omitted != stepped.Omitted {
		t.Fatalf("fold omits %+v, want what the step-over omits %+v", folded.Omitted, stepped.Omitted)
	}

	f.row(f.session, 2, 3)
	if got := f.active(); got.ID != "" {
		t.Fatalf("Active after a row was written into omitted turn 2 = %q, want none", got.ID)
	}
	if omitted := f.stepOver(nil, limits); omitted != 9 {
		t.Fatalf("step-over after the rewind omitted %d rows, want the 9 of turns 1-4", omitted)
	}
}
