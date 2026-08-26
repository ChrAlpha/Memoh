package bridgesvc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/memohai/memoh/internal/workspace/bridgepb"
)

type watchStream struct {
	ctx     context.Context
	batches chan []string
}

func newWatchStream(ctx context.Context) *watchStream {
	return &watchStream{ctx: ctx, batches: make(chan []string, 16)}
}

func (s *watchStream) Send(msg *pb.WatchDirEvent) error {
	s.batches <- append([]string(nil), msg.GetPaths()...)
	return nil
}

func (s *watchStream) Context() context.Context   { return s.ctx }
func (*watchStream) SetHeader(metadata.MD) error  { return nil }
func (*watchStream) SendHeader(metadata.MD) error { return nil }
func (*watchStream) SetTrailer(metadata.MD)       {}
func (*watchStream) SendMsg(any) error            { return nil }
func (*watchStream) RecvMsg(any) error            { return nil }

func waitBatch(t *testing.T, s *watchStream) []string {
	t.Helper()
	select {
	case batch := <-s.batches:
		return batch
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for watch batch")
		return nil
	}
}

func TestWatchDirStreamsChildChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	srv := New(Options{DefaultWorkDir: "/data", WorkspaceRoot: root, DataMount: "/data"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newWatchStream(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.WatchDir(&pb.WatchDirRequest{Path: "/data/sub"}, stream) }()

	// Give the watcher a beat to register before mutating.
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(root, "sub", "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	batch := waitBatch(t, stream)
	found := false
	for _, p := range batch {
		if p == "/data/sub/a.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("batch = %v, want to contain /data/sub/a.txt", batch)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WatchDir returned %v after cancel, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WatchDir did not return after cancel")
	}
}

func TestWatchDirCoalescesBursts(t *testing.T) {
	root := t.TempDir()
	srv := New(Options{DefaultWorkDir: "/data", WorkspaceRoot: root, DataMount: "/data"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newWatchStream(ctx)
	go func() { _ = srv.WatchDir(&pb.WatchDirRequest{Path: "/data"}, stream) }()

	time.Sleep(100 * time.Millisecond)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	batch := waitBatch(t, stream)
	if len(batch) < 2 {
		t.Fatalf("batch = %v, want the burst coalesced into one batch", batch)
	}
}

func TestWatchDirRejectsMissingDir(t *testing.T) {
	root := t.TempDir()
	srv := New(Options{DefaultWorkDir: "/data", WorkspaceRoot: root, DataMount: "/data"})

	stream := newWatchStream(context.Background())
	err := srv.WatchDir(&pb.WatchDirRequest{Path: "/data/absent"}, stream)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("error = %v, want NotFound", err)
	}
}

func TestWatchDirRejectsFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := New(Options{DefaultWorkDir: "/data", WorkspaceRoot: root, DataMount: "/data"})

	stream := newWatchStream(context.Background())
	err := srv.WatchDir(&pb.WatchDirRequest{Path: "/data/f.txt"}, stream)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("error = %v, want InvalidArgument", err)
	}
}

func TestWatchDirEmptyPathRejected(t *testing.T) {
	srv := New(Options{DefaultWorkDir: "/data", WorkspaceRoot: t.TempDir(), DataMount: "/data"})
	stream := newWatchStream(context.Background())
	err := srv.WatchDir(&pb.WatchDirRequest{Path: "  "}, stream)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("error = %v, want InvalidArgument", err)
	}
}
