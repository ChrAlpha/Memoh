package persist

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/step"
	"github.com/felinics/memoh/internal/agent/toolexec"
	chatview "github.com/felinics/memoh/internal/agent/view"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/runtimefence"
)

// StepCommitter bridges Twilight's complete-step barrier to history
// persistence. It is intentionally enabled only for admitted, fenced turns;
// legacy calls without a runtime owner keep their terminal-snapshot behavior.
type StepCommitter struct {
	ownerContext    context.Context
	writer          *Writer
	req             runreq.ChatRequest
	rc              runreq.ResolvedContext
	persister       messagepkg.AgentStepPersister
	reasoningTiming *ReasoningTimingTracker
	queueStep       *queueStepCoordinator

	mu                   sync.Mutex
	turnRequestMessageID string
	persisted            []messagepkg.Message
	memoryPersisted      []messagepkg.Message
	messages             []runreq.ModelMessage
	nextStep             int // In-process ordering guard, not a durable replay cursor.
	finalized            bool
	replacementFinalized bool
	// checkpointed means an interrupted step was written: the run already has
	// a history row for the model call it stopped in.
	checkpointed bool
}

// NewStepCommitter is the committer of an admitted, fenced run, or nil when the
// request does not call for one: no run identity, no fence, a message store that
// cannot write steps, or an input whose turn is not named.
func (w *Writer) NewStepCommitter(ctx context.Context, req runreq.ChatRequest, rc runreq.ResolvedContext) *StepCommitter {
	if w.messages == nil || strings.TrimSpace(req.RunID) == "" ||
		strings.TrimSpace(req.BotID) == "" || strings.TrimSpace(req.ThreadID) == "" ||
		(req.SkipHistoryTurn && req.TurnReplacement == nil) {
		return nil
	}
	if _, ok := runtimefence.FromContext(ctx); !ok {
		return nil
	}
	persister, ok := w.messages.(messagepkg.AgentStepPersister)
	if !ok {
		return nil
	}
	queueStep := newQueueStepCoordinator(w, req)
	if queueStep != nil && queueStep.steerEnabled {
		if err := w.runtime.EnableSteer(ctx, req.RunHandle); err != nil {
			queueStep.steerEnabled = false
			if w.logger != nil {
				w.logger.WarnContext(ctx, "steer consumer could not be published", slog.String("run_id", req.RunID), slog.Any("error", err))
			}
		}
	}
	if req.TurnReplacement != nil && queueStep == nil {
		return nil
	}
	requestMessageID := ""
	switch {
	case req.ReusePersistedUserMessage:
		requestMessageID = strings.TrimSpace(req.PersistedUserMessageID)
		if requestMessageID == "" {
			return nil
		}
	case req.UserMessagePersisted:
		requestMessageID = strings.TrimSpace(req.PersistedUserMessageID)
	case strings.TrimSpace(req.TurnID) == "" || req.TurnPosition == nil:
		return nil
	}
	return &StepCommitter{
		writer: w, req: req, rc: rc, persister: persister, ownerContext: ctx,
		queueStep:            queueStep,
		turnRequestMessageID: requestMessageID,
		// A continuation resumes a run whose earlier steps are already
		// persisted; the loop numbers its commits from the same offset.
		nextStep: req.StepIndexOffset,
	}
}

// BindContinuation wires the steer hooks of a continuation's run configuration
// to this committer: the wake channel, the pending check and the steered
// checkpoint.
func (c *StepCommitter) BindContinuation(cfg *native.RunConfig) {
	if c == nil || cfg == nil {
		return
	}
	if c.queueStep != nil && c.queueStep.steerEnabled {
		cfg.SteerWake = c.writer.runtime.SteerWake(c.req.RunHandle)
		cfg.PendingSteer = func(ctx context.Context) (bool, error) {
			items, err := c.writer.runtime.PendingSteers(ctx, sessionruntime.Key{
				BotID: c.req.BotID, SessionID: c.req.ThreadID,
			}, sessionruntime.MaxPendingQueueItems)
			for _, item := range items {
				if item.TargetRunID == c.req.RunID && item.Status == sessionruntime.QueueAccepted {
					return true, err
				}
			}
			return false, err
		}
		// The steered checkpoint is also a claim boundary: its directive
		// carries whatever input the claim admitted, so the loop continues
		// with the new input instead of restarting the run.
		cfg.OnSteer = func(ctx context.Context, index int, record *step.Record) (native.StepDirective, error) {
			return c.persist(ctx, index, record, stepSteered, outcome.RunFailure{})
		}
	}
}

// withOutputTruncatedNotice records the truncation notice on the assistant row
// of a final step the model cut off at its output limit, so history shows it
// the way the live stream did.
func withOutputTruncatedNotice(opts RoundOptions, record *step.Record, messages []runreq.ModelMessage) RoundOptions {
	if record.Result.FinishReason != sdk.FinishReasonLength || len(record.Result.ToolCalls) > 0 {
		return opts
	}
	idx := lastAssistantMessageIndex(messages)
	if idx < 0 {
		return opts
	}
	definition, ok := apperror.Lookup(apperror.CodeRuntimeOutputTruncated)
	if !ok {
		return opts
	}
	if opts.MessageMetadataByIndex == nil {
		opts.MessageMetadataByIndex = make(map[int]map[string]any, 1)
	}
	if opts.MessageMetadataByIndex[idx] == nil {
		opts.MessageMetadataByIndex[idx] = map[string]any{}
	}
	opts.MessageMetadataByIndex[idx][event.RuntimeNoticesMetadataKey] = []event.Notice{{
		Code:    string(apperror.CodeRuntimeOutputTruncated),
		Content: definition.Detail,
	}}
	return opts
}

type stepCommitMode uint8

const (
	stepCompleted stepCommitMode = iota
	stepInterrupted
	stepSteered
)

func (c *StepCommitter) commit(ctx context.Context, stepIndex int, record *step.Record) (native.StepDirective, error) {
	return c.persist(ctx, stepIndex, record, stepCompleted, outcome.RunFailure{})
}

func (c *StepCommitter) interrupt(ctx context.Context, stepIndex int, record *step.Record) error {
	_, err := c.persist(ctx, stepIndex, record, stepInterrupted, outcome.RunFailure{})
	return err
}

// persist writes one step. failure, when it names a code, is the failure that
// ended the run on an interrupted step and is recorded on its assistant rows.
func (c *StepCommitter) persist(ctx context.Context, stepIndex int, record *step.Record, mode stepCommitMode, failure outcome.RunFailure) (native.StepDirective, error) {
	interrupted := mode != stepCompleted
	if c == nil || record == nil {
		return native.StepDirective{}, errors.New("agent step is missing")
	}
	persistCtx, ownershipErr := StepContext(ctx, c.ownerContext)
	if ownershipErr != nil {
		return native.StepDirective{}, ownershipErr
	}
	ctx = persistCtx
	messages := runreq.SDKMessagesWithOrigins(record.Messages, native.InternalFeedbackIndexes(ctx))
	timingState := "completed"
	if interrupted {
		timingState = "interrupted"
	}
	reasoningTiming := c.reasoningTiming.take(timingState)

	c.mu.Lock()
	defer c.mu.Unlock()
	var dir native.StepDirective
	fail := func(err error) (native.StepDirective, error) {
		return native.StepDirective{}, err
	}
	if stepIndex != c.nextStep {
		return fail(fmt.Errorf("unexpected agent step %d, want %d", stepIndex, c.nextStep))
	}
	hasAssistantOutput := hasPersistableAssistantOutput(messages)
	// A durable run must still cross the coordinator boundary for an empty
	// provider result: final handoff and queue reconciliation are keyed to the
	// step, not to whether the provider emitted a message. Legacy/non-durable
	// paths retain the old cheap no-op behavior.
	if !hasAssistantOutput && mode != stepSteered && (c.queueStep == nil || interrupted) {
		c.nextStep++
		return native.StepDirective{}, nil
	}
	if (hasAssistantOutput || mode == stepSteered) && stepIndex == 0 && !c.req.UserMessagePersisted && !c.req.ReusePersistedUserMessage {
		messages = runreq.PrependTurnUserMessage(c.req, messages)
	}
	storeReq := c.req
	// Outbound assets are linked once after the stream closes; including the
	// collector in every step would attach the same accumulated assets again.
	storeReq.OutboundAssetCollector = nil
	opts := RoundOptions{
		AllowPendingToolCalls: record.Deferred != nil,
		ContextLifecycle:      c.rc.RunConfig.ContextLifecycle,
		ReasoningTiming:       reasoningTiming,
	}
	if interrupted {
		opts.MessageMetadataByIndex = make(map[int]map[string]any)
		for i, message := range messages {
			if strings.EqualFold(strings.TrimSpace(message.Role), "assistant") {
				opts.MessageMetadataByIndex[i] = map[string]any{messagepkg.AgentStepInterruptedMetadataKey: true}
				if failure.Code != "" {
					opts.MessageMetadataByIndex[i] = FailureRowMetadata(failure, "", opts.MessageMetadataByIndex[i])
				}
			}
		}
	}
	opts = withOutputTruncatedNotice(opts, record, messages)
	opts = opts.withContextLifecycleMetadata(c.writer.logger, storeReq, messages)
	var (
		inputs []messagepkg.PersistInput
		err    error
	)
	if len(messages) > 0 {
		inputs, err = c.writer.buildPersistInputs(context.WithoutCancel(ctx), storeReq, messages, c.rc.Model.ID, opts)
		if err != nil {
			return fail(err)
		}
	}
	for i := range inputs {
		inputs[i].TurnRequestMessageID = c.turnRequestMessageID
	}
	c.stampSteerTurn(inputs)
	agentStep := messagepkg.AgentStep{RunID: c.req.RunID, Messages: inputs, Interrupted: interrupted}
	var persisted []messagepkg.Message
	var queueErr error
	if c.queueStep != nil && mode != stepInterrupted {
		stepCtx := context.WithoutCancel(ctx)
		kind := classifyQueueStep(record)
		if mode == stepSteered {
			kind = queueStepSteered
		}
		result, commitErr := c.queueStep.commit(
			stepCtx, kind, agentStep, c.persisted,
		)
		if !result.historyCommitted {
			return fail(commitErr)
		}
		// History is already durable even if later queue coordination failed.
		// Record that prefix below before returning the error; a cleanup must
		// not lose it or write the same step again.
		queueErr = commitErr
		persisted = result.persisted
		c.replacementFinalized = result.replacementFinalized
		if queueErr == nil {
			c.publishQueueUserTurns(context.WithoutCancel(ctx), stepIndex, result)
			if result.claimedSteer != nil {
				if text := runreq.QueuePayloadText(result.claimedSteer.Payload); text != "" {
					dir.NextInputs = []native.DirectiveInput{{
						ID:   string(result.claimedSteer.ID),
						Text: text,
					}}
				}
			}
		}
	} else {
		persisted, err = c.persister.PersistAgentStep(context.WithoutCancel(ctx), agentStep)
	}
	if err != nil {
		return fail(err)
	}
	if len(persisted) > 0 {
		c.rc.RunConfig.ContextLifecycle.SetAssistantMessageID(runreq.LastPersistedAssistantMessageID(persisted))
	}
	c.nextStep++
	for _, message := range persisted {
		if strings.EqualFold(strings.TrimSpace(message.Role), "user") && !messagepkg.IsInternalFeedback(message.Metadata) {
			c.turnRequestMessageID = message.ID
		}
	}
	c.persisted = append(c.persisted, persisted...)
	if len(persisted) > 0 {
		c.rc.Pressure.Add(record.Messages)
	}
	if mode == stepInterrupted && len(persisted) > 0 {
		c.checkpointed = true
	}
	if c.replacementFinalized {
		c.writer.publishReplacementMessageCreated(c.req.BotID, c.persisted)
	}
	if !interrupted {
		// Unfinished reasoning/text is history context, not a fact source for
		// asynchronous long-term memory extraction.
		c.memoryPersisted = append(c.memoryPersisted, persisted...)
		c.messages = append(c.messages, messages...)
	}
	if queueErr != nil {
		return fail(queueErr)
	}
	return dir, nil
}

// stampSteerTurn files this step's injected steer input under the turn drawn
// when the input was claimed, instead of letting persistence mint a second name
// for it. The row is identified by position rather than by text, and the
// position to take is the last one: the steer is the final user row a step can
// carry. The loop appends its other user rows first at a step boundary — the
// read-media carrier, then injected platform messages — and the steer
// directive last (see streamEngine.run), and step.Record.Messages holds only
// the assistant and tool rows the provider produced. So scan from the end and
// take the last user row nobody named.
// native.TestQueuedSteerIsAppendedAfterEveryOtherPreparedMessage pins the
// ordering this depends on.
func (c *StepCommitter) stampSteerTurn(inputs []messagepkg.PersistInput) {
	if c == nil || c.queueStep == nil {
		return
	}
	slot := c.queueStep.steerTurnForStep()
	if slot == nil || strings.TrimSpace(slot.TurnID) == "" {
		return
	}
	for i := len(inputs) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(inputs[i].Role), "user") || messagepkg.IsInternalFeedback(inputs[i].Metadata) {
			continue
		}
		if strings.TrimSpace(inputs[i].TurnID) != "" {
			continue
		}
		position := slot.Position
		inputs[i].TurnID = slot.TurnID
		inputs[i].TurnPosition = &position
		return
	}
}

func (c *StepCommitter) publishQueueUserTurns(ctx context.Context, stepIndex int, result queueStepOutcome) {
	if c == nil || c.writer == nil || c.writer.runtime == nil {
		return
	}
	projected := chatview.ConvertMessagesToUITurns(result.persisted)
	userTurns := make([]chatview.UITurn, 0, len(projected))
	for _, turn := range projected {
		if strings.EqualFold(strings.TrimSpace(turn.Role), "user") {
			userTurns = append(userTurns, turn)
		}
	}
	update := sessionruntime.QueueUserTurnUpdate{
		PersistedTurns:     userTurns,
		AppliedSteerItemID: strings.TrimSpace(result.appliedSteerItemID),
	}
	if update.AppliedSteerItemID != "" && len(userTurns) > 0 {
		// A queue claim is the only mid-run user input admitted by this adapter.
		// The step capture places it after any earlier durable users, so the last
		// persisted user is the history identity for the applied item.
		applied := userTurns[len(userTurns)-1]
		update.AppliedSteerTurn = &applied
	}
	if result.claimedSteer != nil {
		update.ClaimedSteerItemID = string(result.claimedSteer.ID)
		update.ClaimedSteerText = runreq.QueuePayloadText(result.claimedSteer.Payload)
		update.ClaimedSteerTimestamp = result.claimedSteer.CreatedAt
		if result.claimedSteerTurn != nil {
			update.ClaimedSteerTurnID = result.claimedSteerTurn.TurnID
			update.ClaimedSteerTurnPosition = result.claimedSteerTurn.Position
		}
		// Anchor after the step that just committed. Its step_end marker was
		// emitted by the native loop before the commit barrier ran, so the wait
		// only covers event consumption and is bounded.
		after := stepIndex
		update.AfterStepIndex = &after
	}
	if len(update.PersistedTurns) == 0 && update.AppliedSteerItemID == "" && update.ClaimedSteerItemID == "" {
		return
	}
	if err := c.writer.runtime.PublishQueueUserTurns(ctx, c.req.RunHandle, update); err != nil && c.writer.logger != nil {
		c.writer.logger.WarnContext(ctx, "publish runtime queue user turns failed",
			slog.String("run_id", c.req.RunID), slog.Any("error", err))
	}
}

// RecordFailure writes the history of a run that failed with f, before the run
// is finished. Text the stream showed for the model call that failed is written
// as an interrupted step carrying f's code and args. Without such text, a run
// that wrote nothing gets the user message and failure row for a turn without
// step commits, and a run with committed steps keeps them as they are. A model
// call the runtime already checkpointed is not written again. It returns the
// rows the failure row wrote; a checkpoint joins the committed steps.
func (c *StepCommitter) RecordFailure(ctx context.Context, partialText string, f outcome.RunFailure) ([]messagepkg.Message, error) {
	if c == nil || f.Code == "" {
		return nil, nil
	}
	c.mu.Lock()
	skip := c.finalized || c.checkpointed
	stepIndex := c.nextStep
	persistedSteps := len(c.persisted)
	c.mu.Unlock()
	if skip {
		return nil, nil
	}
	if strings.TrimSpace(partialText) != "" {
		record := &step.Record{Messages: toolexec.BuildStepMessages(partialText, nil, nil, nil, nil, nil)}
		_, err := c.persist(ctx, stepIndex, record, stepInterrupted, f)
		return nil, err
	}
	// A replacement turn finalizes through its coordinator, which only a
	// committed step reaches.
	if persistedSteps > 0 || c.req.TurnReplacement != nil {
		return nil, nil
	}
	if _, err := StepContext(ctx, c.ownerContext); err != nil {
		return nil, err
	}
	return c.writer.FailureRow(context.WithoutCancel(ctx), c.req, c.rc, f, "")
}

// Finish closes the run's history once its steps are committed: it records the
// workspace target, links the assets the run produced, and tells the observer
// what the run wrote. A run that committed nothing has nothing to close.
func (c *StepCommitter) Finish(ctx context.Context, inputTokens int) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.finalized {
		c.mu.Unlock()
		return nil
	}
	persisted := append([]messagepkg.Message(nil), c.persisted...)
	memoryPersisted := append([]messagepkg.Message(nil), c.memoryPersisted...)
	messages := append([]runreq.ModelMessage(nil), c.messages...)
	if len(persisted) == 0 {
		c.finalized = true
	}
	c.mu.Unlock()
	if len(persisted) == 0 {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	if err := c.writer.persistSessionWorkspaceTarget(ctx, c.req); err != nil {
		return err
	}
	c.mu.Lock()
	c.finalized = true
	c.mu.Unlock()
	if c.req.OutboundAssetCollector != nil {
		c.writer.LinkOutboundAssets(ctx, c.req.BotID, c.req.ThreadID, runreq.OutboundAssetRefsToMessageRefs(c.req.OutboundAssetCollector()))
	}
	if !c.req.SkipMemoryExtraction && len(memoryPersisted) == len(messages) && len(memoryPersisted) > 0 {
		c.writer.observer.RoundPersisted(ctx, c.req, memoryPersisted)
	}
	if inputTokens > 0 {
		c.writer.observer.RunCommitted(ctx, c.req, c.rc, inputTokens)
	}
	return nil
}

// PersistedMessages are the rows the committed steps wrote so far.
func (c *StepCommitter) PersistedMessages() []messagepkg.Message {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]messagepkg.Message(nil), c.persisted...)
}

// UncommittedStepText is the answer text the stream showed for the model call
// that has not committed yet. A step end closes the call and a retry discards
// it, so the text starts over at either; the runtime regenerates a retried
// call from its last committed step.
type UncommittedStepText struct {
	text strings.Builder
}

func (t *UncommittedStepText) Observe(event native.StreamEvent) {
	switch event.Type {
	case native.EventTextDelta:
		t.text.WriteString(event.Delta)
	case native.EventStepEnd, native.EventRetry:
		t.text.Reset()
	}
}

func (t *UncommittedStepText) String() string {
	return t.text.String()
}

// The run's owner context remains authoritative even when the SDK supplies a
// detached cleanup context. User abort checkpoints may outlive cancellation;
// a revoked owner must never write in the reaper's grace window.
func StepContext(ctx, owner context.Context) (context.Context, error) {
	if outcome.RunOwnershipLost(ctx) || outcome.RunOwnershipLost(owner) {
		return nil, sessionruntime.ErrRunOwnershipLost
	}
	return context.WithoutCancel(ctx), nil
}

// RequestMessageID is the row of the user message this run answers; empty
// until the first step has stored it.
func (c *StepCommitter) RequestMessageID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnRequestMessageID
}

// Pressure is the run's running estimate of what its context holds.
func (c *StepCommitter) Pressure() *runreq.RunPressure {
	return c.rc.Pressure
}
