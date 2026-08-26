package workspace

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/memohai/memoh/internal/workspace/bridge"
)

const (
	fsWatchRetryDelay     = 5 * time.Second
	fsWatchUnsupportedTTL = 5 * time.Minute
	// A watch that dies before this uptime never actually observed the
	// directory (dial failure, stopped workspace) — it missed nothing, so no
	// stale-dir signal is sent and only the retry remains.
	fsWatchEstablishedAfter = 2 * time.Second
)

type fsWatchKey struct {
	botID string
	dir   string
}

type fsDirWatch struct {
	cancel context.CancelFunc
}

// FSWatchService maintains refcounted per-(bot, dir) bridge watch streams
// driven by viewer subscriptions (the set of directories expanded in a files
// pane) and forwards change batches to the fsevent hub. Watch lifetime equals
// viewer attention: when the last subscription for a directory goes away the
// stream is canceled, so idle workspaces carry no standing watches.
type FSWatchService struct {
	logger  *slog.Logger
	publish func(botID string, paths []string)
	// watchDir blocks while streaming batches; swapped in tests.
	watchDir         func(ctx context.Context, botID, dir string, onBatch func([]string)) error
	retryDelay       time.Duration
	establishedAfter time.Duration

	mu          sync.Mutex
	subs        map[string]map[fsWatchKey]struct{}
	watches     map[fsWatchKey]*fsDirWatch
	unsupported map[string]time.Time
}

func NewFSWatchService(logger *slog.Logger, clients bridge.Provider, publish func(botID string, paths []string)) *FSWatchService {
	if logger == nil {
		logger = slog.Default()
	}
	s := &FSWatchService{
		logger:           logger,
		publish:          publish,
		retryDelay:       fsWatchRetryDelay,
		establishedAfter: fsWatchEstablishedAfter,
		subs:             make(map[string]map[fsWatchKey]struct{}),
		watches:          make(map[fsWatchKey]*fsDirWatch),
		unsupported:      make(map[string]time.Time),
	}
	s.watchDir = func(ctx context.Context, botID, dir string, onBatch func([]string)) error {
		client, err := clients.MCPClient(ctx, botID)
		if err != nil {
			return err
		}
		return client.WatchDir(ctx, dir, onBatch)
	}
	return s
}

// SetSubscription replaces subID's watched directory set for botID. An empty
// dirs slice clears it.
func (s *FSWatchService) SetSubscription(subID, botID string, dirs []string) {
	if subID == "" || botID == "" {
		return
	}
	want := make(map[fsWatchKey]struct{}, len(dirs))
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		want[fsWatchKey{botID: botID, dir: dir}] = struct{}{}
	}

	s.mu.Lock()
	current := s.subs[subID]
	for key := range current {
		if _, keep := want[key]; !keep {
			s.releaseLocked(subID, key)
		}
	}
	if len(want) > 0 {
		if s.subs[subID] == nil {
			s.subs[subID] = make(map[fsWatchKey]struct{})
		}
		for key := range want {
			if _, has := s.subs[subID][key]; !has {
				s.subs[subID][key] = struct{}{}
				s.acquireLocked(key)
			}
		}
	}
	if len(s.subs[subID]) == 0 {
		delete(s.subs, subID)
	}
	s.mu.Unlock()
}

// DropSubscription releases every directory subID was watching.
func (s *FSWatchService) DropSubscription(subID string) {
	s.mu.Lock()
	for key := range s.subs[subID] {
		s.releaseLocked(subID, key)
	}
	delete(s.subs, subID)
	s.mu.Unlock()
}

func (s *FSWatchService) acquireLocked(key fsWatchKey) {
	if _, ok := s.watches[key]; ok {
		return
	}
	s.startWatchLocked(key)
}

func (s *FSWatchService) startWatchLocked(key fsWatchKey) {
	if until, ok := s.unsupported[key.botID]; ok {
		if time.Now().Before(until) {
			return
		}
		delete(s.unsupported, key.botID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.watches[key] = &fsDirWatch{cancel: cancel}
	go s.runWatch(ctx, key)
}

func (s *FSWatchService) releaseLocked(subID string, key fsWatchKey) {
	delete(s.subs[subID], key)
	if s.hasSubscriberLocked(key) {
		return
	}
	if watch, ok := s.watches[key]; ok {
		watch.cancel()
		delete(s.watches, key)
	}
}

func (s *FSWatchService) hasSubscriberLocked(key fsWatchKey) bool {
	for _, keys := range s.subs {
		if _, ok := keys[key]; ok {
			return true
		}
	}
	return false
}

func (s *FSWatchService) runWatch(ctx context.Context, key fsWatchKey) {
	startedAt := time.Now()
	err := s.watchDir(ctx, key.botID, key.dir, func(paths []string) {
		if s.publish != nil {
			s.publish(key.botID, paths)
		}
	})
	if ctx.Err() != nil {
		return
	}
	established := time.Since(startedAt) >= s.establishedAfter

	s.mu.Lock()
	delete(s.watches, key)
	stillWanted := s.hasSubscriberLocked(key)
	if errors.Is(err, bridge.ErrWatchUnsupported) {
		s.unsupported[key.botID] = time.Now().Add(fsWatchUnsupportedTTL)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	if !stillWanted {
		return
	}
	// A stream that died after being established may have missed events, so
	// tell viewers to re-list it; then re-attempt after a delay (bounded —
	// one timer per dead watch, re-armed only while wanted).
	if established && s.publish != nil {
		s.publish(key.botID, []string{key.dir})
	}
	if err != nil {
		s.logger.Debug("workspace fs watch ended; scheduling retry",
			slog.String("bot_id", key.botID),
			slog.String("dir", key.dir),
			slog.Any("error", err))
	}
	time.AfterFunc(s.retryDelay, func() { //nolint:contextcheck // the retry outlives the dead stream's context by design; the new watch owns a fresh one.
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, running := s.watches[key]; running {
			return
		}
		if !s.hasSubscriberLocked(key) {
			return
		}
		s.startWatchLocked(key)
	})
}
