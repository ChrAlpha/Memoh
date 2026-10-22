package compaction

import (
	"context"
	"testing"
)

func TestRunCompactionSyncReportsNoopWhileSessionIsBusy(t *testing.T) {
	t.Parallel()

	q := &fakeQueries{uncompacted: machineryCorpus(t)}
	stub := &stubModel{summary: "healthy summary"}
	svc := newMachineryService(q)
	cfg := machineryConfig(stub, 450)

	if _, ok := svc.beginSessionCompaction(cfg.SessionID); !ok {
		t.Fatal("first acquisition must succeed")
	}
	res, err := svc.compactOnce(context.Background(), cfg)
	if err != nil || res.Status != passNoop || stub.calls != 0 || len(q.markedIDs) != 0 {
		t.Fatalf("busy session must noop without running: res=%#v err=%v calls=%d marked=%d", res, err, stub.calls, len(q.markedIDs))
	}

	svc.endSessionCompaction(cfg.SessionID)
	res, err = svc.compactOnce(context.Background(), cfg)
	if err != nil || res.Status != passOK {
		t.Fatalf("session must be usable after the owner completes: res=%#v err=%v", res, err)
	}
}
