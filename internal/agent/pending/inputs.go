// Package pending owns the input that waits for a session: the follow-ups and
// steers a user queued behind a run, the messages a busy platform session
// held, and the batches of external input a discuss session has not answered.
// It starts them when the session is free, in one order, and settles what a
// crash left half done.
package pending

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/agentapi/ports"
	"github.com/felinics/memoh/internal/agent/botenv"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/plan"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/transcript"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// Queries is the database port of discuss admission.
type Queries interface {
	GetDiscussSession(context.Context, dbsqlc.GetDiscussSessionParams) (dbsqlc.GetDiscussSessionRow, error)
	GetDiscussCoverPosition(context.Context, dbsqlc.GetDiscussCoverPositionParams) (int64, error)
	ListDiscussPendingInputs(context.Context, dbsqlc.ListDiscussPendingInputsParams) ([]dbsqlc.ListDiscussPendingInputsRow, error)
	ListDiscussSweepCandidates(context.Context, dbsqlc.ListDiscussSweepCandidatesParams) ([]dbsqlc.ListDiscussSweepCandidatesRow, error)
	GetBotByID(ctx context.Context, id pgtype.UUID) (dbsqlc.GetBotByIDRow, error)
	ListUserIDsByChannelIdentity(ctx context.Context, channelIdentityID pgtype.UUID) ([]pgtype.UUID, error)
}

// TurnStarter starts a turn from a stored command. A queued batch starts as the
// turn its command describes; the turn service is the one that admits it.
type TurnStarter interface {
	StartTurn(context.Context, turn.StartTurnCommand) (turn.RunHandle, error)
}

// Deps is everything New needs. Queue and WorkspaceReadiness are optional; the
// rest is required.
type Deps struct {
	// Logger receives the component's own records.
	Logger *slog.Logger
	// Queries reads discuss sessions and their uncovered input.
	Queries Queries
	// Queue is the durable queue of input that met a busy session. Without it
	// (a deployment with no database for the queue) nothing queues and such
	// input is refused as busy.
	Queue Store
	// Runtime is the session runtime: live steers, snapshots and the legacy
	// follow-up import.
	Runtime *sessionruntime.Manager
	// Turns starts the turn a queued batch stands for.
	Turns TurnStarter
	// Admit admits the run that answers a discuss batch.
	Admit *admit.Admitter
	// Signer issues the credentials of the runs this component starts.
	Signer *admit.Signer
	// ActiveTurns counts the runs the component starts, for shutdown.
	ActiveTurns *runreq.ActiveTurnTracker
	// Driver executes the discuss run and writes its terminal state.
	Driver *drive.Driver
	// Plan plans the discuss run.
	Plan *plan.Planner
	// Transcript renders the context an external runtime is prompted with.
	Transcript *transcript.Reader
	// Bot resolves the display name an earlier message of a batch is headed with.
	Bot *botenv.Env
	// Persist writes the earlier messages of a batch.
	Persist *persist.Writer
	// Messages is the store those rows are written to.
	Messages messagepkg.Service
	// TeamScopes names the scopes the sweeps cover.
	TeamScopes ports.TeamScopes
	// Continuations are the interrupted runs sessions are owed a continuation of.
	Continuations Continuations
	// WorkspaceReadiness is asked before work the system owes a session starts:
	// a continuation, a queued follow-up batch, a discuss batch. A turn the user
	// starts directly is not checked. Without it the workspace is always ready.
	WorkspaceReadiness ports.WorkspaceReadiness
}

// Inputs admits what waits on a session.
type Inputs struct {
	logger      *slog.Logger
	queries     Queries
	queue       Store
	queueing    bool
	runtime     *sessionruntime.Manager
	turns       TurnStarter
	admit       *admit.Admitter
	signer      *admit.Signer
	activeTurns *runreq.ActiveTurnTracker
	driver      *drive.Driver
	plan        *plan.Planner
	transcript  *transcript.Reader
	bot         *botenv.Env
	persist     *persist.Writer
	messages    messagepkg.Service
	teamScopes  ports.TeamScopes
	// continuations and readiness decide what the session is owed and whether
	// it can start now.
	continuations Continuations
	readiness     ports.WorkspaceReadiness
	// sessions serializes the admission of what waits on one session.
	sessions sessionLocks
}

// New returns the component. Without a queue it holds the no-queue store, which
// answers the way an empty queue does.
func New(d Deps) *Inputs {
	in := &Inputs{
		logger:      d.Logger,
		queries:     d.Queries,
		queue:       d.Queue,
		queueing:    d.Queue != nil,
		runtime:     d.Runtime,
		turns:       d.Turns,
		admit:       d.Admit,
		signer:      d.Signer,
		activeTurns: d.ActiveTurns,
		driver:      d.Driver,
		plan:        d.Plan,
		transcript:  d.Transcript,
		bot:         d.Bot,
		persist:     d.Persist,
		messages:    d.Messages,
		teamScopes:  d.TeamScopes,

		continuations: d.Continuations,
		readiness:     d.WorkspaceReadiness,
	}
	if in.queue == nil {
		in.queue = noQueue{}
	}
	if in.readiness == nil {
		in.readiness = func(context.Context, string, string) error { return nil }
	}
	return in
}

// Queueing reports whether the deployment has a durable queue. Without one a
// send that finds the session busy is refused, not parked.
func (in *Inputs) Queueing() bool { return in.queueing }

// Gate is the session queue's verdict on an invocation: whether the queue
// already holds or has taken it (Seen), and whether input waits ahead of it.
func (in *Inputs) Gate(ctx context.Context, key sessionruntime.Key, invocationID string) (sessionruntime.QueueGate, error) {
	return in.queue.Gate(ctx, key, invocationID)
}

// Enqueue puts an input in the session's queue.
func (in *Inputs) Enqueue(ctx context.Context, item sessionruntime.QueueEnqueue) (sessionruntime.QueuedInput, error) {
	return in.queue.Enqueue(ctx, item)
}
