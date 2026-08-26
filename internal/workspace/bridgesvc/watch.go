package bridgesvc

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/memohai/memoh/internal/workspace/bridgepb"
)

const watchCoalesceWindow = 100 * time.Millisecond

// WatchDir watches one directory (non-recursive) and streams coalesced change
// batches with container-visible paths. Chmod-only events are dropped: they
// carry no content or listing change the UI could render.
func (s *Server) WatchDir(req *pb.WatchDirRequest, stream grpc.ServerStreamingServer[pb.WatchDirEvent]) error {
	requested := strings.TrimSpace(req.GetPath())
	if requested == "" {
		return status.Error(codes.InvalidArgument, "path is required")
	}
	dir := s.resolvePath(requested)
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return status.Error(codes.NotFound, "directory not found")
		}
		return status.Error(codes.Internal, err.Error())
	}
	if !info.IsDir() {
		return status.Error(codes.InvalidArgument, "path is not a directory")
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	defer func() { _ = watcher.Close() }()
	if err := watcher.Add(dir); err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	ctx := stream.Context()
	pending := make(map[string]struct{})
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if ev.Op == fsnotify.Chmod {
				continue
			}
			pending[watchEventPath(requested, dir, ev.Name)] = struct{}{}
			if timer == nil {
				timer = time.NewTimer(watchCoalesceWindow)
				timerC = timer.C
			}
			// The watched directory itself disappearing ends the watch: flush
			// what we have (including the directory path) and let the stream
			// end tell the host to re-list the parent and re-subscribe.
			if ev.Name == dir && ev.Op.Has(fsnotify.Remove|fsnotify.Rename) {
				return stream.Send(&pb.WatchDirEvent{Paths: drainWatchPending(pending)})
			}
		case <-timerC:
			timer = nil
			timerC = nil
			if err := stream.Send(&pb.WatchDirEvent{Paths: drainWatchPending(pending)}); err != nil {
				return err
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			return status.Error(codes.Internal, "filesystem watch failed")
		}
	}
}

func watchEventPath(requested, watchedDir, eventPath string) string {
	if eventPath == watchedDir {
		return requested
	}
	return path.Join(requested, filepath.Base(eventPath))
}

func drainWatchPending(pending map[string]struct{}) []string {
	paths := make([]string, 0, len(pending))
	for p := range pending {
		paths = append(paths, p)
	}
	for p := range pending {
		delete(pending, p)
	}
	return paths
}
