package sessionruntime

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Releases before the session input queue kept follow-ups in Redis. A server
// of the old release keeps writing them for as long as it runs, so moving them
// over is a step of the sweep, not a migration that runs once: every pass
// takes what the old servers left since the last. The step lives in this file
// and the sweep's one call to Manager.ImportLegacyFollowUps; a release that no
// old server can outlive removes both.

// LegacyFollowUp is a follow-up an earlier release wrote to Redis.
type LegacyFollowUp struct {
	Key                 Key
	ID                  string
	InvocationID        string
	Payload             []byte
	EnqueuedDuringRunID string
	CreatedAt           time.Time
}

// legacyFollowUpBackend is implemented by the backend that may hold such
// follow-ups. The memory backend never did across restarts, so for it the
// import has nothing to do.
type legacyFollowUpBackend interface {
	LegacyFollowUpKeys(ctx context.Context) ([]Key, error)
	// ClaimLegacyFollowUp takes the next follow-up of the queue away from the
	// servers of the old release, which skip a claimed item. The import's own
	// claim is found again by the next call, so a claim that was never
	// finished is not lost.
	ClaimLegacyFollowUp(ctx context.Context, key Key) (LegacyFollowUp, bool, error)
	FinishLegacyFollowUp(ctx context.Context, key Key, id string) error
	ReleaseLegacyFollowUp(ctx context.Context, key Key, id string) error
}

// legacyImportTrigger is the trigger run id the import claims under. An old
// server claims under the id of the run that ended; this one is no run's.
const legacyImportTrigger = "queue-import"

// LegacyFollowUpConverter turns a legacy follow-up into a queue entry. ok is
// false for one that this pass must leave where it is, such as an input of a
// team the pass is not bound to; an error is an input that cannot be read and
// is dropped, as the old release rejected it.
type LegacyFollowUpConverter func(LegacyFollowUp) (entry QueueEnqueue, ok bool, err error)

// LegacyFollowUpSink is where imported follow-ups are written: the queue.
type LegacyFollowUpSink interface {
	Enqueue(context.Context, QueueEnqueue) (QueuedInput, error)
}

// ImportLegacyFollowUps moves the follow-ups the Redis backend still holds into
// the queue and reports how many it moved. A follow-up is written to the queue
// under the invocation id it had, so one moved twice, as after a crash between
// the write and the removal, is one input. With any other backend it does
// nothing.
func (m *Manager) ImportLegacyFollowUps(ctx context.Context, queue LegacyFollowUpSink, convert LegacyFollowUpConverter) (int, error) {
	backend, ok := m.backendOrNil().(legacyFollowUpBackend)
	if !ok {
		return 0, nil
	}
	keys, err := backend.LegacyFollowUpKeys(ctx)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, key := range keys {
		n, err := m.importLegacyQueue(ctx, backend, queue, key, convert)
		moved += n
		if err != nil {
			return moved, err
		}
	}
	return moved, nil
}

func (m *Manager) backendOrNil() any {
	if m == nil {
		return nil
	}
	return m.backend
}

func (m *Manager) importLegacyQueue(ctx context.Context, backend legacyFollowUpBackend, queue LegacyFollowUpSink, key Key, convert LegacyFollowUpConverter) (int, error) {
	moved := 0
	for ctx.Err() == nil {
		item, ok, err := backend.ClaimLegacyFollowUp(ctx, key)
		if err != nil || !ok {
			return moved, err
		}
		entry, mine, err := convert(item)
		switch {
		case err != nil:
			m.logger.WarnContext(ctx, "dropping a legacy follow-up that cannot be read",
				slog.String("session_id", key.SessionID), slog.String("item_id", item.ID), slog.Any("error", err))
		case !mine:
			return moved, backend.ReleaseLegacyFollowUp(ctx, key, item.ID)
		default:
			if _, err := queue.Enqueue(ctx, entry); err != nil && !errors.Is(err, ErrQueueInvocationConflict) {
				// A full queue keeps the rest in Redis for a later pass.
				if releaseErr := backend.ReleaseLegacyFollowUp(ctx, key, item.ID); releaseErr != nil {
					err = errors.Join(err, releaseErr)
				}
				if errors.Is(err, ErrQueueCapacityExceeded) {
					return moved, nil
				}
				return moved, err
			}
		}
		if err := backend.FinishLegacyFollowUp(ctx, key, item.ID); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, ctx.Err()
}

// legacyFollowUpState is the Redis document of one follow-up queue as the old
// release wrote it. The names are the old ones, which is why the item types
// carry no tags where the old ones carried none.
type legacyFollowUpState struct {
	Items          []legacyFollowUpItem `json:"items"`
	TerminalClaims map[string]string    `json:"terminal_claims,omitempty"`
	UpdatedAt      time.Time            `json:"updated_at"`
}

type legacyFollowUpItem struct {
	ID                  string
	BotID, SessionID    string
	EnqueuedDuringRunID string
	InvocationID        string
	Payload             []byte
	Status              QueueStatus
	Position            int64
	Claim               *legacyFollowUpClaim
	ErrorCode           string
	CreatedAt           time.Time
}

type legacyFollowUpClaim struct {
	ItemID       string
	TriggerRunID string
	ClaimToken   string
}

// claimForImport hands out the item an earlier import claimed and did not
// finish, or else the first accepted one.
func (state *legacyFollowUpState) claimForImport(now time.Time) (legacyFollowUpItem, bool) {
	best := -1
	for i := range state.Items {
		item := &state.Items[i]
		if item.Status == QueueClaimed && item.Claim != nil && item.Claim.TriggerRunID == legacyImportTrigger {
			return *item, true
		}
		if item.Status == QueueAccepted && (best < 0 || item.Position < state.Items[best].Position) {
			best = i
		}
	}
	if best < 0 {
		return legacyFollowUpItem{}, false
	}
	item := &state.Items[best]
	item.Status = QueueClaimed
	item.Claim = &legacyFollowUpClaim{ItemID: item.ID, TriggerRunID: legacyImportTrigger, ClaimToken: uuid.NewString()}
	state.UpdatedAt = now
	return *item, true
}

// release gives back an item the import claimed, and reports whether there
// was one.
func (state *legacyFollowUpState) release(id string, now time.Time) bool {
	for i := range state.Items {
		item := &state.Items[i]
		if item.ID == id && item.Status == QueueClaimed && item.Claim != nil && item.Claim.TriggerRunID == legacyImportTrigger {
			item.Status = QueueAccepted
			item.Claim = nil
			state.UpdatedAt = now
			return true
		}
	}
	return false
}

// remove drops an item, and the record of the run that claimed it.
func (state *legacyFollowUpState) remove(id string) {
	kept := state.Items[:0]
	for _, item := range state.Items {
		if item.ID != id {
			kept = append(kept, item)
		}
	}
	state.Items = kept
	for runID, itemID := range state.TerminalClaims {
		if itemID == id {
			delete(state.TerminalClaims, runID)
		}
	}
}

func (state *legacyFollowUpState) pending() bool {
	for _, item := range state.Items {
		if item.Status == QueueAccepted || item.Status == QueueClaimed {
			return true
		}
	}
	return false
}

func (b *RedisBackend) legacyFollowUpKey(key Key) string {
	return b.keyPrefix + "follow_up_queue:" + key.String()
}

// LegacyFollowUpKeys lists the sessions that still have a follow-up queue in
// Redis.
func (b *RedisBackend) LegacyFollowUpKeys(ctx context.Context) ([]Key, error) {
	if err := b.ensureQueueOpen(); err != nil {
		return nil, err
	}
	prefix := b.keyPrefix + "follow_up_queue:"
	var keys []Key
	iter := b.client.Scan(ctx, 0, prefix+"*", 128).Iterator()
	for iter.Next(ctx) {
		botID, sessionID, ok := strings.Cut(strings.TrimPrefix(iter.Val(), prefix), ":")
		if ok && botID != "" && sessionID != "" {
			keys = append(keys, Key{BotID: botID, SessionID: sessionID})
		}
	}
	return keys, iter.Err()
}

// legacyMutate changes a legacy document and writes it back only when the
// change did something, so looking at a queue that is empty, or gone, does not
// bring its key back.
func (b *RedisBackend) legacyMutate(ctx context.Context, key Key, change func(state *legacyFollowUpState, now time.Time) bool) error {
	if err := validateQueueKey(key); err != nil {
		return err
	}
	redisKey := b.legacyFollowUpKey(key)
	return redisWatch(ctx, b, []string{redisKey}, func(tx *redis.Tx) error {
		state, err := loadRedisJSON[legacyFollowUpState](ctx, tx, redisKey)
		if err != nil {
			return err
		}
		now, err := tx.Time(ctx).Result()
		if err != nil {
			return err
		}
		if !change(&state, now.UTC()) {
			return nil
		}
		return storeRedisJSON(ctx, tx, redisKey, state, b.stateTTL)
	})
}

func (b *RedisBackend) ClaimLegacyFollowUp(ctx context.Context, key Key) (LegacyFollowUp, bool, error) {
	var claimed LegacyFollowUp
	var ok bool
	err := b.legacyMutate(ctx, key, func(state *legacyFollowUpState, now time.Time) bool {
		item, found := state.claimForImport(now)
		claimed, ok = LegacyFollowUp{
			Key: key, ID: item.ID, InvocationID: item.InvocationID, Payload: item.Payload,
			EnqueuedDuringRunID: item.EnqueuedDuringRunID, CreatedAt: item.CreatedAt,
		}, found
		return found
	})
	if err != nil {
		return LegacyFollowUp{}, false, err
	}
	return claimed, ok, nil
}

func (b *RedisBackend) ReleaseLegacyFollowUp(ctx context.Context, key Key, id string) error {
	return b.legacyMutate(ctx, key, func(state *legacyFollowUpState, now time.Time) bool {
		return state.release(id, now)
	})
}

// FinishLegacyFollowUp removes a follow-up that now lives in the queue, and the
// whole document once nothing in it is left to start.
func (b *RedisBackend) FinishLegacyFollowUp(ctx context.Context, key Key, id string) error {
	if err := validateQueueKey(key); err != nil {
		return err
	}
	redisKey := b.legacyFollowUpKey(key)
	return redisWatch(ctx, b, []string{redisKey}, func(tx *redis.Tx) error {
		state, err := loadRedisJSON[legacyFollowUpState](ctx, tx, redisKey)
		if err != nil {
			return err
		}
		state.remove(id)
		if !state.pending() {
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Del(ctx, redisKey)
				return nil
			})
			return err
		}
		return storeRedisJSON(ctx, tx, redisKey, state, b.stateTTL)
	})
}

var _ legacyFollowUpBackend = (*RedisBackend)(nil)
