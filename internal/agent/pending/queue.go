package pending

import (
	"context"
	"strings"

	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// queueStore is the queue, or the unavailability of one when the deployment
// has no database queue.
func (in *Inputs) queueStore() (Store, error) {
	if !in.queueing {
		return nil, sessionruntime.ErrLiveQueueUnavailable
	}
	return in.queue, nil
}

// EnqueueFollowUp puts a text the user queued behind the session's run. The
// queue panel and the /queue command are for input that waits for a run in
// flight, so a session with nothing running and nothing waiting refuses it,
// and the sender says it again as an ordinary message.
//
// The input stands alone: each queued text is answered by a run of its own, in
// the order the panel shows.
func (in *Inputs) EnqueueFollowUp(ctx context.Context, input QueueInput) (sessionruntime.FollowUpItem, error) {
	queue, err := in.queueStore()
	if err != nil {
		return sessionruntime.FollowUpItem{}, err
	}
	cmd, err := queueTurnCommand(input)
	if err != nil {
		return sessionruntime.FollowUpItem{}, err
	}
	payload, err := runreq.EncodeQueueCommand(cmd, nil)
	if err != nil {
		return sessionruntime.FollowUpItem{}, err
	}
	key := sessionruntime.Key{BotID: cmd.BotID, SessionID: cmd.ThreadID}
	invocationID := strings.TrimSpace(input.InvocationID)
	runID, err := in.followUpTargetRun(ctx, queue, key, invocationID)
	if err != nil {
		return sessionruntime.FollowUpItem{}, err
	}
	item, err := queue.Enqueue(ctx, sessionruntime.QueueEnqueue{
		Key: key, Origin: sessionruntime.QueueOriginFollowUp, InvocationID: invocationID,
		Payload: payload, EnqueuedDuringRunID: runID,
	})
	if err != nil {
		return sessionruntime.FollowUpItem{}, err
	}
	// The run the input waits for may have ended before the row was written, in
	// which case nobody else would start it until the session's next run ends.
	in.Kick(ctx, key.BotID, key.SessionID)
	return item.FollowUp(), nil
}

// followUpTargetRun is the run a follow-up waits behind. A session with no run
// in flight takes one only behind inputs already waiting, or as the repeat of
// one it took before.
func (in *Inputs) followUpTargetRun(ctx context.Context, queue Store, key sessionruntime.Key, invocationID string) (string, error) {
	snapshot, err := in.runtime.Snapshot(ctx, key.BotID, key.SessionID)
	if err != nil {
		return "", err
	}
	if run := snapshot.CurrentRunView; run != nil && sessionruntime.IsActiveRunStatus(run.Status) {
		return run.RunID, nil
	}
	gate, err := queue.Gate(ctx, key, invocationID)
	if err != nil {
		return "", err
	}
	if !gate.Waiting && !gate.Seen {
		return "", sessionruntime.ErrQueueNoActiveRun
	}
	return "", nil
}

func (in *Inputs) pendingFollowUps(ctx context.Context, key sessionruntime.Key) ([]sessionruntime.FollowUpItem, error) {
	queue, err := in.queueStore()
	if err != nil {
		return nil, err
	}
	inputs, err := queue.Pending(ctx, key, sessionruntime.QueueOriginFollowUp)
	if err != nil {
		return nil, err
	}
	return followUpItems(inputs), nil
}

func followUpItems(inputs []sessionruntime.QueuedInput) []sessionruntime.FollowUpItem {
	items := make([]sessionruntime.FollowUpItem, len(inputs))
	for i, input := range inputs {
		items[i] = input.FollowUp()
	}
	return items
}

// ReorderFollowUp moves a queued follow-up before another.
func (in *Inputs) ReorderFollowUp(ctx context.Context, botID, sessionID string, item, before sessionruntime.FollowUpPendingRef) ([]sessionruntime.FollowUpItem, error) {
	queue, err := in.queueStore()
	if err != nil {
		return nil, err
	}
	inputs, err := queue.Reorder(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, sessionruntime.QueueOriginFollowUp, string(item.ItemID), string(before.ItemID))
	if err != nil {
		return nil, err
	}
	return followUpItems(inputs), nil
}

// UpdateFollowUp replaces the text of a queued follow-up on behalf of
// editorUserID, who must be the sender the item recorded. Sender, routing, and
// attachment metadata are immutable for a queued command; only the text is
// rewritten, and the queue rechecks that the input is still queued as it
// writes.
func (in *Inputs) UpdateFollowUp(ctx context.Context, botID, sessionID, itemID, editorUserID, text string) (sessionruntime.FollowUpItem, error) {
	queue, err := in.queueStore()
	if err != nil {
		return sessionruntime.FollowUpItem{}, err
	}
	key := sessionruntime.Key{BotID: botID, SessionID: sessionID}
	inputs, err := queue.Pending(ctx, key, sessionruntime.QueueOriginFollowUp)
	if err != nil {
		return sessionruntime.FollowUpItem{}, err
	}
	for _, input := range inputs {
		if input.ID != itemID {
			continue
		}
		payload, err := runreq.RewriteQueuePayloadText(input.Payload, editorUserID, text)
		if err != nil {
			return sessionruntime.FollowUpItem{}, err
		}
		edited, err := queue.Edit(ctx, key, sessionruntime.QueueOriginFollowUp, input.ID, payload)
		if err != nil {
			return sessionruntime.FollowUpItem{}, err
		}
		return edited.FollowUp(), nil
	}
	return sessionruntime.FollowUpItem{}, sessionruntime.ErrQueueNotPending
}

// CancelFollowUp removes a queued follow-up.
func (in *Inputs) CancelFollowUp(ctx context.Context, botID, sessionID, itemID string) error {
	queue, err := in.queueStore()
	if err != nil {
		return err
	}
	return queue.Cancel(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, sessionruntime.QueueOriginFollowUp, itemID)
}
