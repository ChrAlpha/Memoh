// Package persist writes a run's history: the rows of a turn and of each
// committed step, the failure row of a run that ended without output, the
// snapshot a terminal event leaves, and the rewrite of a replaced turn.
package persist

import (
	"context"
	"log/slog"

	"github.com/felinics/memoh/internal/agent/internal/botenv"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
)

// Sessions is the part of the session store the writer uses: the session
// metadata the history rows are stamped from and the fork bookkeeping of a
// replaced turn.
type Sessions interface {
	Get(ctx context.Context, sessionID string) (sessionpkg.Thread, error)
	UpdateMetadata(ctx context.Context, sessionID string, metadata map[string]any) (sessionpkg.Thread, error)
}

// SteerRuntime is the live queue of the session runtime. A step that commits
// history also claims the steer input the run takes in next, so the two are
// ordered here.
type SteerRuntime interface {
	EnableSteer(ctx context.Context, handle sessionruntime.RunHandle) error
	SteerWake(handle sessionruntime.RunHandle) <-chan struct{}
	PendingSteers(ctx context.Context, key sessionruntime.Key, limit int) ([]sessionruntime.SteerItem, error)
	ClaimNextSteer(ctx context.Context, handle sessionruntime.RunHandle, sealIfEmpty bool) (sessionruntime.SteerItem, sessionruntime.SteerClaimRef, bool, error)
	ApplySteer(ctx context.Context, key sessionruntime.Key, ref sessionruntime.SteerClaimRef) error
	ReleaseSteer(ctx context.Context, key sessionruntime.Key, ref sessionruntime.SteerClaimRef) error
	PublishQueueUserTurns(ctx context.Context, handle sessionruntime.RunHandle, update sessionruntime.QueueUserTurnUpdate) error
}

// RoundObserver is told which rows a round made durable. Memory extraction is
// a policy of a higher layer; the writer only says when its input exists.
type RoundObserver interface {
	RoundPersisted(ctx context.Context, req runreq.ChatRequest, persisted []messagepkg.Message)
}

// CommitObserver is told that a run's history is written, with the input token
// count the run was accounted with. Context compaction is a policy of a higher
// layer; the writer only says when its input exists.
type CommitObserver interface {
	RunCommitted(ctx context.Context, req runreq.ChatRequest, rc runreq.ResolvedContext, inputTokens int)
}

// PromotedInputs records that a run applied a steer which was promoted from a
// queued input.
type PromotedInputs interface {
	ConsumePromoted(ctx context.Context, steerID string) error
}

// Deps is everything New needs.
type Deps struct {
	// Logger receives the writer's own records.
	Logger *slog.Logger
	// Messages stores the rows.
	Messages messagepkg.Service
	// Sessions reads and updates session metadata.
	Sessions Sessions
	// Bot resolves the bot settings and identities a row is stamped with.
	Bot *botenv.Env
	// Runtime is the live steer queue.
	Runtime SteerRuntime
	// Rounds is told which rows a round wrote; without it nothing follows.
	Rounds RoundObserver
	// Commits is told when a run's history is written; without it nothing
	// follows.
	Commits CommitObserver
	// Promoted records applied promoted inputs; without it none are recorded.
	Promoted PromotedInputs
}

// Writer writes history rows.
type Writer struct {
	logger   *slog.Logger
	messages messagepkg.Service
	sessions Sessions
	botenv   *botenv.Env
	runtime  SteerRuntime
	rounds   RoundObserver
	commits  CommitObserver
	promoted PromotedInputs
}

// New returns a Writer. Optional dependencies left nil become stand-ins that
// do nothing.
func New(d Deps) *Writer {
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	w := &Writer{
		logger:   logger,
		messages: d.Messages,
		sessions: d.Sessions,
		botenv:   d.Bot,
		runtime:  d.Runtime,
		rounds:   d.Rounds,
		commits:  d.Commits,
		promoted: d.Promoted,
	}
	if w.rounds == nil {
		w.rounds = noRounds{}
	}
	if w.commits == nil {
		w.commits = noCommits{}
	}
	if w.promoted == nil {
		w.promoted = noPromoted{}
	}
	return w
}

type noRounds struct{}

func (noRounds) RoundPersisted(context.Context, runreq.ChatRequest, []messagepkg.Message) {}

type noCommits struct{}

func (noCommits) RunCommitted(context.Context, runreq.ChatRequest, runreq.ResolvedContext, int) {}

type noPromoted struct{}

func (noPromoted) ConsumePromoted(context.Context, string) error { return nil }
