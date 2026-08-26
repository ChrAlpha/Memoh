package workspace

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/memohai/memoh/internal/workspace/bridge"
)

type fakeWatchRun struct {
	botID string
	dir   string
	ctx   context.Context
	batch func([]string)
	errCh chan error
}

type fakeWatcher struct {
	mu   sync.Mutex
	runs []*fakeWatchRun
	ch   chan *fakeWatchRun
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{ch: make(chan *fakeWatchRun, 16)}
}

func (f *fakeWatcher) watchDir(ctx context.Context, botID, dir string, onBatch func([]string)) error {
	run := &fakeWatchRun{botID: botID, dir: dir, ctx: ctx, batch: onBatch, errCh: make(chan error, 1)}
	f.mu.Lock()
	f.runs = append(f.runs, run)
	f.mu.Unlock()
	f.ch <- run
	select {
	case <-ctx.Done():
		return nil
	case err := <-run.errCh:
		return err
	}
}

func (f *fakeWatcher) next(t *testing.T) *fakeWatchRun {
	t.Helper()
	select {
	case run := <-f.ch:
		return run
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watch run")
		return nil
	}
}

func (f *fakeWatcher) expectNone(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case run := <-f.ch:
		t.Fatalf("unexpected watch run for %s %s", run.botID, run.dir)
	case <-time.After(wait):
	}
}

type publishRecord struct {
	botID string
	paths []string
}

func newTestFSWatchService(watcher *fakeWatcher) (*FSWatchService, chan publishRecord) {
	published := make(chan publishRecord, 16)
	svc := NewFSWatchService(nil, nil, func(botID string, paths []string) {
		published <- publishRecord{botID: botID, paths: paths}
	})
	svc.watchDir = watcher.watchDir
	svc.retryDelay = 20 * time.Millisecond
	return svc, published
}

func TestFSWatchServiceStartsAndStopsWithSubscriptions(t *testing.T) {
	watcher := newFakeWatcher()
	svc, published := newTestFSWatchService(watcher)

	svc.SetSubscription("conn-1", "bot-1", []string{"/data"})
	run := watcher.next(t)
	if run.botID != "bot-1" || run.dir != "/data" {
		t.Fatalf("run = %s %s", run.botID, run.dir)
	}

	run.batch([]string{"/data/a.txt"})
	select {
	case rec := <-published:
		if rec.botID != "bot-1" || len(rec.paths) != 1 || rec.paths[0] != "/data/a.txt" {
			t.Fatalf("published = %+v", rec)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for publish")
	}

	// A second viewer of the same dir shares the watch.
	svc.SetSubscription("conn-2", "bot-1", []string{"/data"})
	watcher.expectNone(t, 100*time.Millisecond)

	svc.DropSubscription("conn-1")
	select {
	case <-run.ctx.Done():
		t.Fatal("watch canceled while another viewer holds it")
	case <-time.After(100 * time.Millisecond):
	}

	svc.DropSubscription("conn-2")
	select {
	case <-run.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("watch not canceled after last viewer left")
	}
}

func TestFSWatchServiceDiffsSubscriptionDirs(t *testing.T) {
	watcher := newFakeWatcher()
	svc, _ := newTestFSWatchService(watcher)

	svc.SetSubscription("conn-1", "bot-1", []string{"/data", "/data/sub"})
	first := watcher.next(t)
	second := watcher.next(t)

	svc.SetSubscription("conn-1", "bot-1", []string{"/data/sub"})
	var rootRun *fakeWatchRun
	if first.dir == "/data" {
		rootRun = first
	} else {
		rootRun = second
	}
	select {
	case <-rootRun.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("dropped dir watch not canceled")
	}
}

func TestFSWatchServiceUnsupportedBridgeBacksOff(t *testing.T) {
	watcher := newFakeWatcher()
	svc, _ := newTestFSWatchService(watcher)

	svc.SetSubscription("conn-1", "bot-1", []string{"/data"})
	run := watcher.next(t)
	run.errCh <- bridge.ErrWatchUnsupported

	// New subscriptions for the same bot do not retry while marked
	// unsupported.
	time.Sleep(50 * time.Millisecond)
	svc.SetSubscription("conn-1", "bot-1", []string{"/data", "/data/sub"})
	watcher.expectNone(t, 150*time.Millisecond)
}

func TestFSWatchServiceRecoversAfterUnsupportedTTL(t *testing.T) {
	watcher := newFakeWatcher()
	svc, _ := newTestFSWatchService(watcher)
	svc.unsupportedFor = 50 * time.Millisecond

	svc.SetSubscription("conn-1", "bot-1", []string{"/data"})
	run := watcher.next(t)
	run.errCh <- bridge.ErrWatchUnsupported

	// Within the TTL an identical re-send stays backed off.
	time.Sleep(20 * time.Millisecond)
	svc.SetSubscription("conn-1", "bot-1", []string{"/data"})
	watcher.expectNone(t, 20*time.Millisecond)

	// After the TTL (e.g. the workspace was upgraded to a watch-capable
	// bridge), re-sending the SAME set restarts the watch — a subscription
	// must not stay watchless forever just because its dirs never changed.
	time.Sleep(60 * time.Millisecond)
	svc.SetSubscription("conn-1", "bot-1", []string{"/data"})
	retry := watcher.next(t)
	if retry.dir != "/data" {
		t.Fatalf("retry dir = %s", retry.dir)
	}
}

func TestFSWatchServiceImmediateFailureRetriesWithoutStaleSignal(t *testing.T) {
	watcher := newFakeWatcher()
	svc, published := newTestFSWatchService(watcher)

	svc.SetSubscription("conn-1", "bot-1", []string{"/data"})
	run := watcher.next(t)
	// A watch that dies before it was ever established (e.g. the workspace is
	// stopped) missed nothing — no stale signal, just a retry.
	run.errCh <- errors.New("dial failed")

	select {
	case rec := <-published:
		t.Fatalf("unexpected publish %+v for never-established watch", rec)
	case <-time.After(60 * time.Millisecond):
	}
	retry := watcher.next(t)
	if retry.dir != "/data" {
		t.Fatalf("retry dir = %s", retry.dir)
	}
}

func TestFSWatchServiceRetriesFailedWatchAndSignalsStaleDir(t *testing.T) {
	watcher := newFakeWatcher()
	svc, published := newTestFSWatchService(watcher)
	svc.establishedAfter = 30 * time.Millisecond

	svc.SetSubscription("conn-1", "bot-1", []string{"/data"})
	run := watcher.next(t)
	time.Sleep(60 * time.Millisecond)
	run.errCh <- errors.New("stream broke")

	// The dying watch may have missed events anywhere under it: viewers get a
	// wildcard so the stale directory itself (not just its parent) reloads.
	select {
	case rec := <-published:
		if rec.botID != "bot-1" || rec.paths != nil {
			t.Fatalf("published = %+v, want wildcard", rec)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stale wildcard publish")
	}

	// And the watch is re-attempted while a subscription still wants it.
	retry := watcher.next(t)
	if retry.dir != "/data" {
		t.Fatalf("retry dir = %s", retry.dir)
	}
}
