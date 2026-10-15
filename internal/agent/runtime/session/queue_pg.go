package sessionruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	dbpkg "github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// QueueOrigin is how an input reached the session queue, which decides the
// process that drains it: the server starts follow-ups, the channel process
// starts the platform messages that met a busy bot, because it is the one that
// delivers their replies.
type QueueOrigin string

const (
	QueueOriginFollowUp     QueueOrigin = "follow_up"
	QueueOriginPlatformBusy QueueOrigin = "platform_busy"
)

func (o QueueOrigin) valid() bool {
	return o == QueueOriginFollowUp || o == QueueOriginPlatformBusy
}

// QueuedState is where an input stands in the queue.
type QueuedState string

const (
	QueuedStateQueued    QueuedState = "queued"
	QueuedStateClaimed   QueuedState = "claimed"
	QueuedStatePromoting QueuedState = "promoting"
	QueuedStatePromoted  QueuedState = "promoted"
	QueuedStateConsumed  QueuedState = "consumed"
	QueuedStateCancelled QueuedState = "cancelled"
)

// QueuedInvocationPrefix starts the invocation id of the run that answers a
// batch. The batch id is the rest, so the ledger's unique invocation index
// refuses to admit one batch twice.
const QueuedInvocationPrefix = "queued:"

// DefaultQueueClaimLease is how long a drainer holds a batch before another may
// take it again. Admission is a few statements, so the lease only has to
// outlast a stall, not a run.
const DefaultQueueClaimLease = time.Minute

// QueuedInput is one input as the queue table holds it.
type QueuedInput struct {
	ID                  string
	BotID, SessionID    string
	Origin              QueueOrigin
	InvocationID        string
	MergeKey            string
	Payload             []byte
	Position            int64
	EnqueuedDuringRunID string
	State               QueuedState
	// ClaimToken is set while the input is being promoted.
	ClaimToken string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// FollowUp is the item the follow-up queue API shows for the input.
func (q QueuedInput) FollowUp() FollowUpItem {
	return FollowUpItem{
		ID: FollowUpItemID(q.ID), BotID: q.BotID, SessionID: q.SessionID,
		EnqueuedDuringRunID: q.EnqueuedDuringRunID, InvocationID: q.InvocationID,
		Payload: q.Payload, Status: QueueAccepted, Position: q.Position, CreatedAt: q.CreatedAt,
	}
}

// QueueEnqueue is one input to add to a session's queue.
type QueueEnqueue struct {
	Key    Key
	Origin QueueOrigin
	// InvocationID is the input's retry identity. Adding the same identity with
	// the same payload again returns the first input; with another payload it
	// is a conflict.
	InvocationID        string
	MergeKey            string
	Payload             []byte
	EnqueuedDuringRunID string
	// CreatedAt is the arrival time when it is older than this call, as for an
	// input moved over from Redis; zero means now.
	CreatedAt time.Time
}

// QueueBatch is the inputs one run answers together, taken from the queue in
// one claim.
type QueueBatch struct {
	ID         string
	ClaimToken string
	Key        Key
	Origin     QueueOrigin
	Items      []QueuedInput
	// Recovered marks a batch whose lease ran out and that was claimed again.
	// An earlier attempt may have admitted its run, so it must not be handed
	// back to the queue: a later claim would give its inputs a new batch id
	// and a second run.
	Recovered bool
}

// InvocationID is the identity the batch's run is admitted under.
func (b QueueBatch) InvocationID() string { return QueuedInvocationPrefix + b.ID }

// QueueGate is what a new turn needs to know about its session's queue.
type QueueGate struct {
	// Seen means the turn's own invocation is already queued or consumed: the
	// message was delivered before.
	Seen bool
	// Waiting means inputs are queued or being admitted ahead of the turn.
	Waiting bool
}

// QueuedPromotion is an input whose hand-over to the run in flight has begun.
type QueuedPromotion struct {
	Item  QueuedInput
	Token string
	// Replay is true when the promotion had already begun.
	Replay bool
}

// QueuedInputs is the session input queue in PostgreSQL. It is independent of
// the live backend: what waits here must survive a restart.
type QueuedInputs struct {
	q     *dbsqlc.Queries
	pool  *pgxpool.Pool
	lease time.Duration
}

// QueuedInputsOption configures QueuedInputs.
type QueuedInputsOption func(*QueuedInputs)

// WithQueueClaimLease sets how long a claim holds a batch.
func WithQueueClaimLease(lease time.Duration) QueuedInputsOption {
	return func(s *QueuedInputs) {
		if lease > 0 {
			s.lease = lease
		}
	}
}

// NewQueuedInputs returns the queue over the given connection.
func NewQueuedInputs(q *dbsqlc.Queries, pool *pgxpool.Pool, opts ...QueuedInputsOption) *QueuedInputs {
	s := &QueuedInputs{q: q, pool: pool, lease: DefaultQueueClaimLease}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *QueuedInputs) ready() error {
	if s == nil || s.q == nil || s.pool == nil {
		return errors.New("session queue: database is not configured")
	}
	return nil
}

func queueError(operation string, err error) error {
	return fmt.Errorf("session queue: %s: %w", operation, err)
}

func queueUUIDs(key Key) (pgtype.UUID, pgtype.UUID, error) {
	botID, err := dbpkg.ParseUUID(key.BotID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, ErrQueueInvalidReference
	}
	sessionID, err := dbpkg.ParseUUID(key.SessionID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, ErrQueueInvalidReference
	}
	return botID, sessionID, nil
}

func queuedInputFromRow(row dbsqlc.SessionQueuedInput) QueuedInput {
	input := QueuedInput{
		ID:                  row.ID.String(),
		BotID:               row.BotID.String(),
		SessionID:           row.SessionID.String(),
		Origin:              QueueOrigin(row.Origin),
		InvocationID:        row.InvocationID,
		MergeKey:            row.MergeKey,
		Payload:             slices.Clone(row.Payload),
		Position:            row.Position,
		EnqueuedDuringRunID: row.EnqueuedDuringRunID,
		State:               QueuedState(row.State),
		CreatedAt:           dbpkg.TimeFromPg(row.CreatedAt),
		UpdatedAt:           dbpkg.TimeFromPg(row.UpdatedAt),
	}
	if row.ClaimToken.Valid {
		input.ClaimToken = row.ClaimToken.String()
	}
	return input
}

func queuedInputsFromRows(rows []dbsqlc.SessionQueuedInput) []QueuedInput {
	out := make([]QueuedInput, len(rows))
	for i, row := range rows {
		out[i] = queuedInputFromRow(row)
	}
	return out
}

// samePayload compares two stored payloads as JSON documents, so key order and
// spacing do not make a retry look like a different input.
func samePayload(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return bytes.Equal(a, b)
	}
	canonicalLeft, errLeft := json.Marshal(left)
	canonicalRight, errRight := json.Marshal(right)
	return errLeft == nil && errRight == nil && bytes.Equal(canonicalLeft, canonicalRight)
}

// Enqueue adds an input at the end of the session's queue. The same identity
// delivered again finds the first input. A queue that already holds the bound
// of waiting inputs refuses with ErrQueueCapacityExceeded: the sender is told,
// the input is not dropped quietly.
func (s *QueuedInputs) Enqueue(ctx context.Context, in QueueEnqueue) (QueuedInput, error) {
	if err := s.ready(); err != nil {
		return QueuedInput{}, err
	}
	in.InvocationID = strings.TrimSpace(in.InvocationID)
	if !in.Origin.valid() || in.InvocationID == "" || len(in.Payload) == 0 || validateQueueKey(in.Key) != nil {
		return QueuedInput{}, ErrQueueInvalidReference
	}
	botID, sessionID, err := queueUUIDs(in.Key)
	if err != nil {
		return QueuedInput{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return QueuedInput{}, queueError("begin enqueue", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := s.q.WithTx(tx)

	if err := q.LockSessionQueue(ctx, in.Key.SessionID+":"+string(in.Origin)); err != nil {
		return QueuedInput{}, queueError("lock queue", err)
	}
	existing, err := q.GetSessionQueuedInputByInvocation(ctx, dbsqlc.GetSessionQueuedInputByInvocationParams{
		SessionID: sessionID, InvocationID: in.InvocationID,
	})
	switch {
	case err == nil:
		return replayQueued(existing, in.Payload)
	case !errors.Is(err, pgx.ErrNoRows):
		return QueuedInput{}, queueError("read invocation", err)
	}
	waiting, err := q.CountQueuedSessionInputs(ctx, dbsqlc.CountQueuedSessionInputsParams{SessionID: sessionID, Origin: string(in.Origin)})
	if err != nil {
		return QueuedInput{}, queueError("count queue", err)
	}
	if waiting >= MaxPendingQueueItems {
		return QueuedInput{}, ErrQueueCapacityExceeded
	}
	params := dbsqlc.InsertSessionQueuedInputParams{
		ID:                  dbpkg.ParseUUIDOrEmpty(uuid.NewString()),
		BotID:               botID,
		SessionID:           sessionID,
		Origin:              string(in.Origin),
		InvocationID:        in.InvocationID,
		MergeKey:            in.MergeKey,
		Payload:             in.Payload,
		EnqueuedDuringRunID: in.EnqueuedDuringRunID,
	}
	if !in.CreatedAt.IsZero() {
		params.CreatedAt = pgtype.Timestamptz{Time: in.CreatedAt, Valid: true}
	}
	row, err := q.InsertSessionQueuedInput(ctx, params)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Another origin took the invocation between the read and the insert.
		existing, readErr := q.GetSessionQueuedInputByInvocation(ctx, dbsqlc.GetSessionQueuedInputByInvocationParams{
			SessionID: sessionID, InvocationID: in.InvocationID,
		})
		if readErr != nil {
			return QueuedInput{}, queueError("read invocation", readErr)
		}
		return replayQueued(existing, in.Payload)
	case isForeignKeyViolation(err):
		return QueuedInput{}, fmt.Errorf("%w: session not found", ErrQueueInvalidReference)
	case err != nil:
		return QueuedInput{}, queueError("insert input", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return QueuedInput{}, queueError("commit enqueue", err)
	}
	return queuedInputFromRow(row), nil
}

func replayQueued(existing dbsqlc.SessionQueuedInput, payload []byte) (QueuedInput, error) {
	if !samePayload(existing.Payload, payload) {
		return QueuedInput{}, ErrQueueInvocationConflict
	}
	return queuedInputFromRow(existing), nil
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// Gate reads what admitting a new turn on the session has to know about its
// queue.
func (s *QueuedInputs) Gate(ctx context.Context, key Key, invocationID string) (QueueGate, error) {
	if err := s.ready(); err != nil {
		return QueueGate{}, err
	}
	_, sessionID, err := queueUUIDs(key)
	if err != nil {
		return QueueGate{}, err
	}
	row, err := s.q.QueuedInputGate(ctx, dbsqlc.QueuedInputGateParams{SessionID: sessionID, InvocationID: strings.TrimSpace(invocationID)})
	if err != nil {
		return QueueGate{}, queueError("read gate", err)
	}
	return QueueGate{Seen: row.Seen, Waiting: row.Waiting}, nil
}

// Pending lists the inputs of one origin that are queued, in order.
func (s *QueuedInputs) Pending(ctx context.Context, key Key, origin QueueOrigin) ([]QueuedInput, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	_, sessionID, err := queueUUIDs(key)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListSessionQueuedInputs(ctx, dbsqlc.ListSessionQueuedInputsParams{SessionID: sessionID, Origin: string(origin)})
	if err != nil {
		return nil, queueError("list inputs", err)
	}
	return queuedInputsFromRows(rows), nil
}

// Reorder moves item in front of before, or to the end when before is empty. A
// reference to an input that is no longer queued is ErrQueueNotPending. The
// inputs keep the position values they had and trade them, so a position is
// never one an input added later also holds.
func (s *QueuedInputs) Reorder(ctx context.Context, key Key, origin QueueOrigin, item, before string) ([]QueuedInput, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	_, sessionID, err := queueUUIDs(key)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, queueError("begin reorder", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := s.q.WithTx(tx)
	rows, err := q.ListSessionQueuedInputsForUpdate(ctx, dbsqlc.ListSessionQueuedInputsForUpdateParams{SessionID: sessionID, Origin: string(origin)})
	if err != nil {
		return nil, queueError("lock inputs", err)
	}
	type slot struct {
		id       string
		position int64
	}
	slots := make([]slot, len(rows))
	positions := make([]int64, len(rows))
	for i, row := range rows {
		slots[i] = slot{id: row.ID.String(), position: row.Position}
		positions[i] = row.Position
	}
	err = reorderQueuePositions(slots, item, before, func(s *slot) (string, QueueStatus, *int64) {
		return s.id, QueueAccepted, &s.position
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(slots, func(a, b slot) int { return int(a.position - b.position) })
	slices.Sort(positions)
	for i, moved := range slots {
		if err := q.SetSessionQueuedInputPosition(ctx, dbsqlc.SetSessionQueuedInputPositionParams{
			ID: dbpkg.ParseUUIDOrEmpty(moved.id), Position: positions[i],
		}); err != nil {
			return nil, queueError("set position", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, queueError("commit reorder", err)
	}
	return s.Pending(ctx, key, origin)
}

// Edit replaces the payload of a queued input.
func (s *QueuedInputs) Edit(ctx context.Context, key Key, origin QueueOrigin, id string, payload []byte) (QueuedInput, error) {
	if err := s.ready(); err != nil {
		return QueuedInput{}, err
	}
	_, sessionID, err := queueUUIDs(key)
	itemID, idErr := dbpkg.ParseUUID(id)
	if err != nil || idErr != nil || len(payload) == 0 {
		return QueuedInput{}, ErrQueueInvalidReference
	}
	row, err := s.q.UpdateSessionQueuedInputPayload(ctx, dbsqlc.UpdateSessionQueuedInputPayloadParams{
		SessionID: sessionID, ID: itemID, Origin: string(origin), Payload: payload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return QueuedInput{}, ErrQueueNotPending
	}
	if err != nil {
		return QueuedInput{}, queueError("edit input", err)
	}
	return queuedInputFromRow(row), nil
}

// Cancel withdraws a queued input.
func (s *QueuedInputs) Cancel(ctx context.Context, key Key, origin QueueOrigin, id string) error {
	if err := s.ready(); err != nil {
		return err
	}
	_, sessionID, err := queueUUIDs(key)
	itemID, idErr := dbpkg.ParseUUID(id)
	if err != nil || idErr != nil {
		return ErrQueueInvalidReference
	}
	rows, err := s.q.CancelSessionQueuedInput(ctx, dbsqlc.CancelSessionQueuedInputParams{SessionID: sessionID, ID: itemID, Origin: string(origin)})
	if err != nil {
		return queueError("cancel input", err)
	}
	if rows == 0 {
		return ErrQueueNotPending
	}
	return nil
}

// ClaimBatch takes the inputs the session's next run answers. It takes nothing
// while the session has a run in flight, while another drainer holds a batch
// of the origin, or when nothing is queued.
//
// A batch whose lease ran out is taken again whole, under the batch id it
// had, before anything newer: its run may already exist, and admitting it
// again under the same invocation id is how that is found out. Otherwise the
// batch is the inputs at the head of the queue that share a merge key; an
// input without one is a batch of its own.
func (s *QueuedInputs) ClaimBatch(ctx context.Context, key Key, origin QueueOrigin) (QueueBatch, bool, error) {
	if err := s.ready(); err != nil {
		return QueueBatch{}, false, err
	}
	_, sessionID, err := queueUUIDs(key)
	if err != nil || !origin.valid() {
		return QueueBatch{}, false, ErrQueueInvalidReference
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return QueueBatch{}, false, queueError("begin claim", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := s.q.WithTx(tx)

	if _, err := q.GetActiveSessionRun(ctx, sessionID); err == nil {
		return QueueBatch{}, false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return QueueBatch{}, false, queueError("read active run", err)
	}
	live, err := q.HasLiveQueuedClaim(ctx, dbsqlc.HasLiveQueuedClaimParams{SessionID: sessionID, Origin: string(origin)})
	if err != nil {
		return QueueBatch{}, false, queueError("read live claim", err)
	}
	if live {
		return QueueBatch{}, false, nil
	}
	claimToken := dbpkg.ParseUUIDOrEmpty(uuid.NewString())
	lapsed, err := q.ListLapsedQueuedInputs(ctx, dbsqlc.ListLapsedQueuedInputsParams{SessionID: sessionID, Origin: string(origin)})
	if err != nil {
		return QueueBatch{}, false, queueError("list lapsed claims", err)
	}
	var (
		rows      []dbsqlc.SessionQueuedInput
		recovered bool
	)
	if len(lapsed) > 0 {
		recovered = true
		rows, err = q.RenewQueuedBatchClaim(ctx, dbsqlc.RenewQueuedBatchClaimParams{
			BatchID: lapsed[0].BatchID, ClaimToken: claimToken, LeaseSeconds: s.lease.Seconds(),
		})
		if err != nil {
			return QueueBatch{}, false, queueError("renew batch", err)
		}
	} else {
		queued, err := q.ListQueuedInputsToClaim(ctx, dbsqlc.ListQueuedInputsToClaimParams{SessionID: sessionID, Origin: string(origin)})
		if err != nil {
			return QueueBatch{}, false, queueError("list queued inputs", err)
		}
		head := batchHead(queued)
		if len(head) == 0 {
			return QueueBatch{}, false, nil
		}
		ids := make([]pgtype.UUID, len(head))
		for i, row := range head {
			ids[i] = row.ID
		}
		rows, err = q.ClaimQueuedInputs(ctx, dbsqlc.ClaimQueuedInputsParams{
			Ids: ids, BatchID: dbpkg.ParseUUIDOrEmpty(uuid.NewString()), ClaimToken: claimToken, LeaseSeconds: s.lease.Seconds(),
		})
		if err != nil {
			return QueueBatch{}, false, queueError("claim inputs", err)
		}
	}
	if len(rows) == 0 {
		return QueueBatch{}, false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return QueueBatch{}, false, queueError("commit claim", err)
	}
	slices.SortStableFunc(rows, func(a, b dbsqlc.SessionQueuedInput) int { return int(a.Position - b.Position) })
	return QueueBatch{
		ID:         rows[0].BatchID.String(),
		ClaimToken: claimToken.String(),
		Key:        key,
		Origin:     origin,
		Items:      queuedInputsFromRows(rows),
		Recovered:  recovered,
	}, true, nil
}

// batchHead is the inputs at the head of the queue that one run answers: the
// first one and, while it has a merge key, those after it with the same key.
func batchHead(queued []dbsqlc.SessionQueuedInput) []dbsqlc.SessionQueuedInput {
	if len(queued) == 0 {
		return nil
	}
	end := 1
	if queued[0].MergeKey != "" {
		for end < len(queued) && queued[end].MergeKey == queued[0].MergeKey {
			end++
		}
	}
	return queued[:end]
}

// SettleBatch records that the batch's run exists, whether the caller admitted
// it or found it admitted. It does not depend on the claim token: the run is a
// fact, and settling twice changes nothing.
func (s *QueuedInputs) SettleBatch(ctx context.Context, batch QueueBatch) error {
	if err := s.ready(); err != nil {
		return err
	}
	batchID, err := dbpkg.ParseUUID(batch.ID)
	if err != nil {
		return ErrQueueInvalidReference
	}
	if _, err := s.q.SettleQueuedBatch(ctx, batchID); err != nil {
		return queueError("settle batch", err)
	}
	return nil
}

// ReleaseBatch hands a batch back to the queue, unadmitted. A recovered batch
// is left claimed: see QueueBatch.Recovered.
func (s *QueuedInputs) ReleaseBatch(ctx context.Context, batch QueueBatch) error {
	if err := s.ready(); err != nil {
		return err
	}
	if batch.Recovered {
		return nil
	}
	batchID, err := dbpkg.ParseUUID(batch.ID)
	token, tokenErr := dbpkg.ParseUUID(batch.ClaimToken)
	if err != nil || tokenErr != nil {
		return ErrQueueInvalidReference
	}
	if _, err := s.q.ReleaseQueuedBatch(ctx, dbsqlc.ReleaseQueuedBatchParams{BatchID: batchID, ClaimToken: token}); err != nil {
		return queueError("release batch", err)
	}
	return nil
}

// Reject ends an input no drainer can start. It keeps the error code and is
// not tried again.
func (s *QueuedInputs) Reject(ctx context.Context, id, errorCode string) error {
	if err := s.ready(); err != nil {
		return err
	}
	itemID, err := dbpkg.ParseUUID(id)
	if err != nil || strings.TrimSpace(errorCode) == "" {
		return ErrQueueInvalidReference
	}
	if _, err := s.q.RejectQueuedInput(ctx, dbsqlc.RejectQueuedInputParams{ID: itemID, ErrorCode: pgtype.Text{String: errorCode, Valid: true}}); err != nil {
		return queueError("reject input", err)
	}
	return nil
}

// maxDrainableSessions bounds how many sessions one sweep pass drains, oldest
// waiting first; the rest are taken on the next pass.
const maxDrainableSessions = 256

// Drainable lists the sessions that have something for the origin's drainer:
// queued inputs, or a batch whose lease ran out.
func (s *QueuedInputs) Drainable(ctx context.Context, origin QueueOrigin) ([]Key, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.q.ListDrainableQueuedSessions(ctx, dbsqlc.ListDrainableQueuedSessionsParams{Origin: string(origin), MaxSessions: maxDrainableSessions})
	if err != nil {
		return nil, queueError("list drainable sessions", err)
	}
	keys := make([]Key, len(rows))
	for i, row := range rows {
		keys[i] = Key{BotID: row.BotID.String(), SessionID: row.SessionID.String()}
	}
	return keys, nil
}

// BeginPromotion starts handing a queued follow-up to the run in flight: the
// first of its three steps. The input leaves the queue's claimable set, so
// nothing starts it while its steer is being written. An input already being
// promoted comes back with Replay set.
func (s *QueuedInputs) BeginPromotion(ctx context.Context, key Key, id string) (QueuedPromotion, error) {
	if err := s.ready(); err != nil {
		return QueuedPromotion{}, err
	}
	_, sessionID, err := queueUUIDs(key)
	itemID, idErr := dbpkg.ParseUUID(id)
	if err != nil || idErr != nil {
		return QueuedPromotion{}, ErrQueueInvalidReference
	}
	row, err := s.q.BeginQueuedInputPromotion(ctx, dbsqlc.BeginQueuedInputPromotionParams{SessionID: sessionID, ID: itemID})
	if err == nil {
		input := queuedInputFromRow(row)
		return QueuedPromotion{Item: input, Token: input.ClaimToken}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return QueuedPromotion{}, queueError("begin promotion", err)
	}
	existing, err := s.q.GetSessionQueuedInput(ctx, dbsqlc.GetSessionQueuedInputParams{SessionID: sessionID, ID: itemID})
	if errors.Is(err, pgx.ErrNoRows) {
		return QueuedPromotion{}, ErrQueueNotPending
	}
	if err != nil {
		return QueuedPromotion{}, queueError("read input", err)
	}
	input := queuedInputFromRow(existing)
	if input.Origin != QueueOriginFollowUp || (input.State != QueuedStatePromoting && input.State != QueuedStatePromoted) {
		return QueuedPromotion{}, ErrQueueNotPending
	}
	return QueuedPromotion{Item: input, Token: input.ClaimToken, Replay: true}, nil
}

// CompletePromotion is the third step: the steer is written, the input is
// promoted. A promotion that something else already settled is left alone.
func (s *QueuedInputs) CompletePromotion(ctx context.Context, id, token string) error {
	return s.settlePromotion(ctx, id, token, "complete promotion", func(itemID, claim pgtype.UUID) (int64, error) {
		return s.q.CompleteQueuedInputPromotion(ctx, dbsqlc.CompleteQueuedInputPromotionParams{ID: itemID, ClaimToken: claim})
	})
}

// AbortPromotion puts an input back in the queue when its steer could not be
// written.
func (s *QueuedInputs) AbortPromotion(ctx context.Context, id, token string) error {
	return s.settlePromotion(ctx, id, token, "abort promotion", func(itemID, claim pgtype.UUID) (int64, error) {
		return s.q.AbortQueuedInputPromotion(ctx, dbsqlc.AbortQueuedInputPromotionParams{ID: itemID, ClaimToken: claim})
	})
}

func (s *QueuedInputs) settlePromotion(_ context.Context, id, token, operation string, run func(itemID, claim pgtype.UUID) (int64, error)) error {
	if err := s.ready(); err != nil {
		return err
	}
	itemID, err := dbpkg.ParseUUID(id)
	claim, tokenErr := dbpkg.ParseUUID(token)
	if err != nil || tokenErr != nil {
		return ErrQueueInvalidReference
	}
	if _, err := run(itemID, claim); err != nil {
		return queueError(operation, err)
	}
	return nil
}

// ConsumePromoted records that the run took the input as a steer.
func (s *QueuedInputs) ConsumePromoted(ctx context.Context, id string) error {
	return s.settleOne(id, "consume promoted input", func(itemID pgtype.UUID) (int64, error) {
		return s.q.ConsumePromotedQueuedInput(ctx, itemID)
	})
}

// RequeuePromoted puts a promoted input back in the queue because its steer is
// gone without having been applied.
func (s *QueuedInputs) RequeuePromoted(ctx context.Context, id string) error {
	return s.settleOne(id, "requeue promoted input", func(itemID pgtype.UUID) (int64, error) {
		return s.q.RequeuePromotedQueuedInput(ctx, itemID)
	})
}

func (s *QueuedInputs) settleOne(id, operation string, run func(itemID pgtype.UUID) (int64, error)) error {
	if err := s.ready(); err != nil {
		return err
	}
	itemID, err := dbpkg.ParseUUID(id)
	if err != nil {
		return ErrQueueInvalidReference
	}
	if _, err := run(itemID); err != nil {
		return queueError(operation, err)
	}
	return nil
}

// Promotions lists the session's inputs that are being or have been promoted
// and have no outcome yet.
func (s *QueuedInputs) Promotions(ctx context.Context, key Key) ([]QueuedInput, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	_, sessionID, err := queueUUIDs(key)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListSessionQueuedPromotions(ctx, sessionID)
	if err != nil {
		return nil, queueError("list promotions", err)
	}
	return queuedInputsFromRows(rows), nil
}

// maxStalePromotions bounds one sweep pass over promotions without an outcome.
const maxStalePromotions = 256

// StalePromotions lists the inputs whose promotion has had no outcome since
// before the given time.
func (s *QueuedInputs) StalePromotions(ctx context.Context, before time.Time) ([]QueuedInput, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.q.ListStaleQueuedPromotions(ctx, dbsqlc.ListStaleQueuedPromotionsParams{
		Before: pgtype.Timestamptz{Time: before, Valid: true}, MaxItems: maxStalePromotions,
	})
	if err != nil {
		return nil, queueError("list stale promotions", err)
	}
	return queuedInputsFromRows(rows), nil
}

// PurgeSettled deletes inputs that were consumed or cancelled before the given
// time. They are kept for a while so a redelivered message finds its first
// input instead of starting a second turn.
func (s *QueuedInputs) PurgeSettled(ctx context.Context, before time.Time) (int64, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	n, err := s.q.PurgeSettledQueuedInputs(ctx, pgtype.Timestamptz{Time: before, Valid: true})
	if err != nil {
		return 0, queueError("purge settled inputs", err)
	}
	return n, nil
}
