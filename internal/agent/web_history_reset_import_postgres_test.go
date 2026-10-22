package agent

import (
	"context"
	"errors"
	"testing"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/botbackup"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	"github.com/felinics/memoh/internal/errs"
)

// An overwrite import that replaces the history takes the same reset as a
// clear, entered through the import service: a projection it cannot clear
// fails the import before any history is replaced; otherwise the runs of the
// replaced sessions leave the live projection and the ledger fallback, a
// client watching one of them is cleared, and a turn in the restored session
// runs as usual.
func TestPostgresHistoryResetReplaceImport(t *testing.T) {
	backend := &failingUpdateBackend{MemoryBackend: sessionruntime.NewMemoryBackend()}
	h := newWSStepHistoryHarnessOn(t, wsStepHistorySuccess, backend)
	ctx := context.Background()
	view := h.watch(t)
	h.run(t, nil)
	before := mustSnapshot(t, h.manager, h.botID, h.sessionID).CurrentRunView
	if before == nil {
		t.Fatal("finished run is missing from the live snapshot before the import")
	}
	kept, err := h.messages.ListBySession(ctx, h.sessionID)
	if err != nil || len(kept) == 0 {
		t.Fatalf("history before the import = (%d rows, %v)", len(kept), err)
	}
	view.drain()

	backup := h.historyBackup(t)
	importer := botbackup.New(botbackup.Params{
		DB: h.pool, Queries: postgresstore.NewQueriesWithPool(h.pool, dbsqlc.New(h.pool)), ACPRuntimes: h.manager,
	})
	opts := botbackup.ImportOptions{
		Mode: botbackup.ImportModeOverwrite, TargetBotID: h.botID,
		Sections: map[botbackup.Section]botbackup.ImportStrategy{botbackup.SectionHistory: botbackup.StrategyReplace},
	}

	backend.fail.Store(true)
	_, err = importer.Import(ctx, "", backup, opts, "")
	backend.fail.Store(false)
	if !errors.Is(err, botbackup.ErrHistoryResetUnavailable) || errs.FaultOf(err) != apperror.FaultDependency {
		t.Fatalf("import with an uncleared projection = %v (fault %q), want a dependency failure", err, errs.FaultOf(err))
	}
	if rows, err := h.messages.ListBySession(ctx, h.sessionID); err != nil || len(rows) != len(kept) {
		t.Fatalf("history after the failed import = (%d rows, %v), want the %d rows kept", len(rows), err, len(kept))
	}
	if got := h.liveSessions(t); len(got) != 1 || got[0] != h.sessionID {
		t.Fatalf("sessions after the failed import = %v, want only %s", got, h.sessionID)
	}
	if got := mustSnapshot(t, h.manager, h.botID, h.sessionID).CurrentRunView; got == nil || got.RunID != before.RunID {
		t.Fatalf("live run after the failed import = %s, want %s", describeRun(got), describeRun(before))
	}

	result, err := importer.Import(ctx, "", backup, opts, "")
	if err != nil || len(result.Warnings) != 0 {
		t.Fatalf("import = (%+v, %v), want it clean", result, err)
	}
	if got := mustSnapshot(t, h.manager, h.botID, h.sessionID).CurrentRunView; got != nil {
		t.Errorf("live snapshot of the replaced session holds %s", describeRun(got))
	}
	view.awaitCleared(before.RunID)
	if got := mustSnapshot(t, h.restarted(t), h.botID, h.sessionID).CurrentRunView; got != nil {
		t.Errorf("ledger fallback of the replaced session reports %s", describeRun(got))
	}

	sessions := h.liveSessions(t)
	if len(sessions) != 1 || sessions[0] == h.sessionID {
		t.Fatalf("sessions after the import = %v, want one restored session", sessions)
	}
	restored := h
	restored.sessionID = sessions[0]
	if rows, err := restored.messages.ListBySession(ctx, restored.sessionID); err != nil || len(rows) != len(kept) {
		t.Fatalf("restored history = (%d rows, %v), want %d", len(rows), err, len(kept))
	}
	if got := mustSnapshot(t, restored.manager, restored.botID, restored.sessionID).CurrentRunView; got != nil {
		t.Fatalf("restored session reports %s before any turn", describeRun(got))
	}
	next := restored.watch(t)
	restored.run(t, nil)
	after := mustSnapshot(t, restored.manager, restored.botID, restored.sessionID).CurrentRunView
	if after == nil || after.RunID == before.RunID {
		t.Fatalf("live run after a turn in the restored session = %s, want a new run", describeRun(after))
	}
	next.awaitRun(after.RunID)
}
