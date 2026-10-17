package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/step"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/runtimefence"
)

// StepCommit returns the per-step persistence callback for one spawned
// agent run, or nil when incremental persistence is unavailable — no admitted
// handle on the context, no fence, no persisted request row to bind steps to,
// or a message service that cannot write fenced steps. A nil return means the
// spawn path keeps its terminal-snapshot persistence, so this is a capability
// probe as much as a constructor.
//
// The callbacks persist exactly what the legacy terminal path persists — every
// non-user message of the step, marshalled whole — only earlier: each complete
// step or safe interrupted checkpoint lands in one fenced transaction instead
// of the whole run landing at the end. onPersisted fires after either kind of
// durable write; the spawn provider uses it to stop retrying an attempt whose
// output is already part of history.
func (h *Host) StepCommit(
	ctx context.Context,
	botID, sessionID, modelID, turnRequestMessageID string,
	contextLifecycle *contextfrag.LifecycleHolder,
	onPersisted func(),
) (
	func(context.Context, int, *step.Record) error,
	func(context.Context, int, *step.Record) error,
) {
	if h.steps == nil {
		return nil, nil
	}
	handle, ok := HandleFromContext(ctx)
	if !ok || strings.TrimSpace(handle.RunID) == "" || handle.FencingToken <= 0 {
		return nil, nil
	}
	if _, ok := runtimefence.FromContext(ctx); !ok {
		return nil, nil
	}
	if strings.TrimSpace(botID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	// No request row means the pre-run user message write failed. Committing
	// steps anyway would file them under no turn and — because the spawn path
	// then reports the run as persisted — permanently drop the task prompt.
	// Decline instead: terminal persistence still writes the user message and
	// the whole run behind it.
	if strings.TrimSpace(turnRequestMessageID) == "" {
		return nil, nil
	}
	committer := &stepCommitter{
		persister:            h.steps,
		ownerContext:         ctx,
		runID:                handle.RunID,
		botID:                botID,
		sessionID:            sessionID,
		modelID:              modelID,
		turnRequestMessageID: strings.TrimSpace(turnRequestMessageID),
		contextLifecycle:     contextLifecycle,
		onPersisted:          onPersisted,
	}
	return committer.commit, committer.interrupt
}

// stepCommitter appends a spawned agent's complete steps and safe
// interrupted checkpoint to its thread's history. Unlike persist.StepCommitter it
// carries no ChatRequest: the subagent path has no resolved chat context, and
// its user message is persisted by the spawn provider before execution starts.
type stepCommitter struct {
	ownerContext context.Context
	persister    messagepkg.AgentStepPersister
	runID        string
	botID        string
	sessionID    string
	modelID      string
	// turnRequestMessageID binds every step row to the task's persisted user
	// message, so the whole run files under the turn admission allocated
	// instead of splitting into history-minted turns.
	turnRequestMessageID string
	contextLifecycle     *contextfrag.LifecycleHolder
	onPersisted          func()

	mu       sync.Mutex
	nextStep int // In-process ordering guard, not a durable replay cursor.
}

func (c *stepCommitter) commit(ctx context.Context, stepIndex int, record *step.Record) error {
	return c.persist(ctx, stepIndex, record, false)
}

func (c *stepCommitter) interrupt(ctx context.Context, stepIndex int, record *step.Record) error {
	return c.persist(ctx, stepIndex, record, true)
}

func (c *stepCommitter) persist(ctx context.Context, stepIndex int, record *step.Record, interrupted bool) error {
	if c == nil || record == nil {
		return errors.New("agent step is missing")
	}
	persistCtx, ownershipErr := persist.StepContext(ctx, c.ownerContext)
	if ownershipErr != nil {
		return ownershipErr
	}
	ctx = persistCtx
	c.mu.Lock()
	defer c.mu.Unlock()
	if stepIndex != c.nextStep {
		return fmt.Errorf("unexpected agent step %d, want %d", stepIndex, c.nextStep)
	}
	inputs := make([]messagepkg.PersistInput, 0, len(record.Messages))
	for _, msg := range record.Messages {
		if msg.Role == sdk.MessageRoleUser {
			continue
		}
		msg, toolCallDiffs := historyfrag.ExtractToolCallDiffsFromSDK(msg)
		content, err := historyfrag.MarshalStoredSDKMessage(msg)
		if err != nil {
			continue
		}
		var usage json.RawMessage
		if msg.Usage != nil {
			usage, _ = json.Marshal(msg.Usage)
		}
		var metadata map[string]any
		if interrupted && msg.Role == sdk.MessageRoleAssistant {
			metadata = map[string]any{messagepkg.AgentStepInterruptedMetadataKey: true}
		}
		if len(toolCallDiffs) > 0 {
			if metadata == nil {
				metadata = make(map[string]any, 1)
			}
			metadata[messagepkg.ToolCallDiffsMetadataKey] = toolCallDiffs
		}
		inputs = append(inputs, messagepkg.PersistInput{
			BotID:                c.botID,
			SessionID:            c.sessionID,
			Role:                 string(msg.Role),
			Content:              content,
			Metadata:             metadata,
			Usage:                usage,
			ModelID:              c.modelID,
			RunID:                c.runID,
			TurnRequestMessageID: c.turnRequestMessageID,
		})
	}
	if len(inputs) == 0 {
		c.nextStep++
		return nil
	}
	if snapshot, ok := c.contextLifecycle.Snapshot(); ok {
		for i := len(inputs) - 1; i >= 0; i-- {
			if !strings.EqualFold(strings.TrimSpace(inputs[i].Role), string(sdk.MessageRoleAssistant)) {
				continue
			}
			if inputs[i].Metadata == nil {
				inputs[i].Metadata = make(map[string]any, 1)
			}
			inputs[i].Metadata[contextfrag.MetadataContextLifecycleKey] = snapshot.Summary()
			break
		}
	}
	persisted, err := c.persister.PersistAgentStep(context.WithoutCancel(ctx), messagepkg.AgentStep{
		RunID: c.runID, Messages: inputs, Interrupted: interrupted,
	})
	if err != nil {
		return err
	}
	c.contextLifecycle.SetAssistantMessageID(runreq.LastPersistedAssistantMessageID(persisted))
	c.nextStep++
	if c.onPersisted != nil {
		c.onPersisted()
	}
	return nil
}
