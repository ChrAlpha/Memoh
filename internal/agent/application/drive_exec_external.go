package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/felinics/memoh/internal/agent/activeprompt"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/schedule"
)

// externalPlan is everything an External Agent run resolved before its driver
// was called: which session it runs in, what context it gets, and the user
// message a subscriber already saw. One plan serves both the interactive entry
// and the schedule fire, which is what keeps a runtime's turn the same work
// whoever started it.
type externalPlan struct {
	driver                external.Driver
	runtimeType           string
	sess                  session.Thread
	runtimeMeta           map[string]any
	projectPath           string
	runtimeOwnerAccountID string
	prepared              runtimePreparedAttachments
	contextMarkdown       string
	contextURI            string
	contextManifest       *contextfrag.Manifest
	memoryTrace           *contextfrag.MemoryRecallTrace
	// leadingUser is the user message this turn wrote before prompting. It is
	// handed back only when the runtime never accepted the turn, where no round
	// exists to carry it.
	leadingUser *messagepkg.Message
}

// prepareExternal resolves the shared setup of an External Agent run: the
// session and its owner, the model preference, the attachments and the context
// sections. A failure here is a setup failure: nothing has run, so an input the
// submitter still holds is handed back rather than written.
func (s *Service) prepareExternal(ctx context.Context, req runreq.ChatRequest, driver external.Driver) (externalPlan, runreq.ChatRequest, error) {
	plan := externalPlan{
		driver:      driver,
		runtimeType: strings.TrimSpace(driver.RuntimeType()),
	}
	sess, err := s.sessionService.Get(ctx, req.ThreadID)
	if err != nil {
		return externalPlan{}, req, err
	}
	if err := runreq.ValidateSessionBot(req.BotID, req.ThreadID, sess.BotID); err != nil {
		return externalPlan{}, req, err
	}
	plan.sess = sess
	plan.runtimeMeta = runreq.RuntimeSessionMeta(sess)
	plan.projectPath = runreq.MetadataString(plan.runtimeMeta, "project_path")
	plan.runtimeOwnerAccountID = runreq.MetadataString(plan.runtimeMeta, "runtime_owner_account_id")
	if err := s.requireRuntimeOwnerWorkspaceExec(ctx, req.BotID, plan.runtimeOwnerAccountID); err != nil {
		return externalPlan{}, req, err
	}
	req, err = s.applyDirectModelPreference(ctx, req, sess)
	if err != nil {
		return externalPlan{}, req, err
	}
	if req.OnModelPreferenceSettled != nil {
		req.OnModelPreferenceSettled()
	}
	// A concurrent turn never reaches here: admission holds the session's
	// single active slot upstream.
	prepared, err := s.prepareRuntimeAttachments(ctx, req)
	if err != nil {
		return externalPlan{}, req, err
	}
	plan.prepared = prepared
	if req.RawQuery == "" {
		req.RawQuery = strings.TrimSpace(req.Query)
	}
	req.Query = strings.TrimSpace(req.Query)
	contextReq := req
	contextReq.Attachments = prepared.Context
	contextReq.ReplyAttachments = nil
	sections, memoryTrace := s.buildRuntimeContextSections(ctx, contextReq, runtimeContextAgentID(plan.runtimeType, plan.runtimeMeta), plan.projectPath)
	plan.memoryTrace = memoryTrace
	plan.contextMarkdown, plan.contextURI, plan.contextManifest = runtimeContextViaContextView(ctx, s.logger, sections, req.Query)

	// The user's message is written before the runtime is prompted, and the
	// round it belongs to is written at the end. A subscriber sees what was
	// sent while the turn runs, the runtime a decision projects against is
	// anchored to a real request row, and a turn the runtime never accepted
	// hands the message back instead of leaving it behind.
	req, plan.leadingUser, err = s.persistRuntimeLeadingUserMessage(context.WithoutCancel(ctx), req)
	if err != nil {
		return externalPlan{}, req, apperror.Wrap(apperror.CodeSessionHistoryInconsistent, err, nil)
	}
	return plan, req, nil
}

// externalExecutor runs one turn on an External Agent driver.
type externalExecutor struct {
	service *Service
	plan    externalPlan
	req     runreq.ChatRequest
	// prompt is what the driver is prompted with; a schedule fire prompts with
	// the generated schedule prompt rather than the raw command.
	prompt string
	// unattended says nobody can answer a decision for this run.
	unattended bool
	// input says who holds the run's input, which decides whether the leading
	// user message may be taken back when the turn leaves nothing to write.
	input outcome.InputOwnership

	lifecycle *contextfrag.LifecycleHolder
	timing    *persist.ReasoningTimingTracker
	notices   runtimeNotices
	// hub carries this turn's live output to whatever the session exposes as
	// its active prompt.
	hub *activeprompt.Hub
	// projections tracks the decision rows this turn projected, so they are
	// cleaned up whichever way the turn ends.
	projections projectionTracker

	rc runreq.ResolvedContext

	// Written by execute, read by commit after execute returned.
	result    external.PromptResult
	failure   *outcome.RunFailure
	completed bool
	// roundUnwritable records that this ending has no round to write: the
	// runtime never accepted the turn, the run was stopped before the runtime
	// did anything, or the anchor the round must not commit past could not be
	// stored.
	roundUnwritable bool
	// projectionsCleanup makes the removal of this turn's decision rows happen
	// once. The run's own cleanup and the background reconciliation of an
	// unknown round both ask for it, from different goroutines, and the rows
	// are gone after the first.
	projectionsCleanup sync.Once
}

// execute prompts the driver once and reports how its turn ended. Every event
// the driver sinks goes to emit; the terminal event is synthesized here,
// because an External Agent runtime reports a finished turn rather than a
// stream that ends.
func (e *externalExecutor) Execute(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
	e.hub = e.service.activePrompts.Register(e.req.BotID, e.req.ThreadID)
	defer e.service.activePrompts.Unregister(e.req.BotID, e.req.ThreadID, e.hub)
	if e.service.sessionRuntime != nil {
		// External drivers block their turn inline on decisions; declare it
		// before the first prompt so the manager resumes on terminal decision
		// statuses instead of waiting for a native-style re-entry.
		e.service.sessionRuntime.MarkInlineDecisionRun(e.req.BotID, e.req.ThreadID, e.req.RunID)
	}
	emit(native.StreamEvent{Type: native.EventStart})

	result, promptErr := e.plan.driver.Prompt(ctx, e.promptInput(ctx, emit))
	promptErr = outcome.RuntimeTurnError(e.plan.runtimeType, promptErr)
	configurationFailure := outcome.IsRuntimeConfigurationError(promptErr)
	promptErr = outcome.ExternalAgentError(promptErr)
	e.notices.apply(&result)
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, outcome.ErrResponseTimeout), errors.Is(cause, outcome.ErrToolTimeout):
		// Drivers normalize a cancellation into a partial nil-error result so
		// the application can persist interrupted output. Restore the
		// watchdog's stable cause here: unlike a user stop, an idle timeout is
		// a failed turn and must carry its code through history and the
		// terminal record.
		promptErr = outcome.RuntimeTurnError(e.plan.runtimeType, cause)
		configurationFailure = false
	case errors.Is(cause, schedule.ErrExecutionTimeout):
		promptErr = outcome.RuntimeTurnError(e.plan.runtimeType, outcome.AgentAbortCause(ctx))
		configurationFailure = false
	}
	e.result = result
	// A stop that arrives before the runtime did anything leaves no transcript
	// for a round to carry. Whatever the runtime's driver called its
	// cancellation, the run driver names the ending a stop, because the stop
	// was raised first.
	stopped := outcome.RunStopped(ctx)
	nothingRan := !runtimeTurnRan(result) && len(result.Notices) == 0

	// Persist the driver-owned session keys before anything else. History must
	// never advance past this anchor: committing the round while its new
	// thread id was lost makes every later turn resume the pre-round context,
	// forking the conversation permanently. Failing the round keeps state
	// consistent instead.
	if len(result.RuntimeMetadata) > 0 {
		if _, mergeErr := e.service.sessionService.MergeRuntimeMetadata(context.WithoutCancel(ctx), e.req.ThreadID, e.plan.runtimeType, result.RuntimeMetadata); mergeErr != nil {
			e.service.logger.ErrorContext(ctx, "external runtime metadata merge failed",
				slog.String("session_id", e.req.ThreadID),
				slog.String("runtime", e.plan.runtimeType),
				slog.Any("error", mergeErr))
			anchorErr := fmt.Errorf("persist runtime session anchor: %w", mergeErr)
			if promptErr != nil {
				anchorErr = errors.Join(promptErr, anchorErr)
			}
			e.failure = &outcome.RunFailure{Code: apperror.CodeSessionHistoryInconsistent, Cause: anchorErr, Phase: outcome.PhaseCommit}
			e.roundUnwritable = true
			return e.ending(emit, native.EventAgentAbort, event.EndCanceled)
		}
	}

	switch {
	case promptErr != nil:
		phase := outcome.PhaseExecution
		if configurationFailure && nothingRan {
			// Nothing ran: the driver refused the turn before it started.
			phase = outcome.PhaseSetup
		}
		code, args := outcome.RunFailureCode(outcome.NewExternalPromptFailure(promptErr), phase, false)
		e.failure = &outcome.RunFailure{Code: code, Args: args, Cause: promptErr, Phase: phase}
		// Nothing ran means there is no transcript for a round to carry. A
		// failure the runtime reported after it started produced one.
		e.roundUnwritable = phase == outcome.PhaseSetup || (stopped && nothingRan)
		return e.ending(emit, native.EventAgentAbort, event.EndFailed)
	case ctx.Err() != nil || !result.TurnCompleted:
		// Either the interrupt landed, or the runtime finished in the same
		// instant the run was stopped. Both end the turn as a stop, keeping the
		// partial transcript.
		e.roundUnwritable = nothingRan && stopped
		return e.ending(emit, native.EventAgentAbort, event.EndCanceled)
	}
	e.completed = true
	return e.ending(emit, native.EventEnd, event.EndCompleted)
}

// ending builds what the driver receives: the terminal event, which the driver
// publishes after this ending's history write, and the write itself.
func (e *externalExecutor) ending(emit func(native.StreamEvent), eventType native.StreamEventType, reason event.EndReason) drive.ExecEnding {
	emit(native.StreamEvent{Type: native.EventTextEnd})
	t := e.terminalFor(eventType, reason)
	return drive.ExecEnding{
		Terminal:      &t,
		Failure:       e.failure,
		Commit:        e.commit,
		CommitStopped: e.commitStopped,
		Cleanup:       e.cleanup,
	}
}

// terminalFor synthesizes the terminal event an External Agent run reports. An
// external runtime has no stream to end, so the driver states the outcome and
// marks it owner-named: the session runtime takes the driver's word for it
// instead of re-deriving one from whichever error the live view saw last.
func (e *externalExecutor) terminalFor(eventType native.StreamEventType, reason event.EndReason) native.StreamEvent {
	ev := runtimeTerminalStreamEvent(eventType, e.result)
	ev.EndReason = reason
	ev.OwnerNamed = true
	if e.failure != nil {
		ev.Code = string(e.failure.Code)
		ev.Args = e.failure.Args
	}
	return ev
}

// deliver is the observer every event the driver produces passes through
// before it is published: notices are folded in, the reasoning timing is
// recorded, a decision the turn projects gets its own row, and the session's
// active prompt sees the event.
func (e *externalExecutor) deliver(deliveryCtx context.Context, emit func(native.StreamEvent), ev native.StreamEvent) {
	ev = publicRuntimeNotice(ev)
	if !e.notices.observe(ev) {
		return
	}
	e.timing.Observe(ev)
	if isRuntimeDecisionProjectionEvent(ev) && e.projections.record(ev.ToolCallID) {
		e.projections.complete(ev.ToolCallID, e.service.persistRuntimeDecisionProjection(context.WithoutCancel(deliveryCtx), e.req, ev))
	}
	if e.hub != nil {
		e.hub.Emit(ev)
	}
	emit(ev)
}

// commit writes the run's one round. An External Agent runtime reports a whole
// turn at once, so this is the run's only history write: the completed round,
// or the failure row of a turn that wrote none.
func (e *externalExecutor) commit(ctx context.Context, f *outcome.RunFailure, row bool) (drive.CommitSummary, error) {
	return e.writeRound(ctx, e.req, f, row)
}

// commitStopped writes the partial transcript of a turn the user stopped. The
// round carries no failure, and no memory is extracted from it: a stop is not a
// completed turn, even when the runtime finished in the same instant.
func (e *externalExecutor) commitStopped(ctx context.Context) (drive.CommitSummary, error) {
	req := e.req
	req.SkipMemoryExtraction = true
	return e.writeRound(ctx, req, nil, false)
}

// writeRound is the one history write of an External Agent run, for req as the
// ending wants it written.
func (e *externalExecutor) writeRound(ctx context.Context, req runreq.ChatRequest, f *outcome.RunFailure, row bool) (drive.CommitSummary, error) {
	summary := drive.CommitSummary{ModelID: strings.TrimSpace(e.req.Model)}
	ctx = context.WithoutCancel(ctx)
	if e.roundUnwritable {
		// Nothing ran, so there is no transcript to write. An input the
		// submitter still holds is handed back untouched; an input that has
		// left their hands keeps a failure row, because nowhere else records
		// that the turn did not happen.
		if row && f != nil {
			persisted, err := e.service.persist.FailureRow(ctx, e.req, e.rc, *f, "")
			if err != nil {
				return summary, outcome.RuntimeHistoryError(err)
			}
			summary.Persisted = persisted
		}
		return summary, nil
	}
	err := e.service.persistExternalRound(
		ctx,
		req,
		e.plan.runtimeType,
		e.plan.projectPath,
		e.result,
		f,
		e.result.TurnCompleted,
		e.lifecycle,
		persist.TakeTerminalReasoningTiming(e.timing, terminalEventTypeOf(e.completed)),
	)
	if err != nil {
		switch e.service.resolveRuntimeRoundPersistFailure(ctx, e.req, err, e.cleanupProjectionsIn) {
		case runtimeRoundCommitted:
			// The round landed after all; the failure was in the bookkeeping
			// that followed it, and the run's own outcome stands.
			return summary, nil
		case runtimeRoundRolledBack:
			e.rollback(ctx)
		}
		return summary, outcome.RuntimeHistoryError(err)
	}
	if e.result.Usage != nil {
		summary.Usage, _ = json.Marshal(e.result.Usage)
	}
	summary.Text = strings.TrimSpace(e.result.Text)
	if e.result.Usage != nil {
		summary.UsageTokens = e.result.Usage.InputTokens
	}
	return summary, nil
}

// rollback repairs a driver whose runtime remembers a round the visible history
// lost. A driver with a repair policy discards its warm process; a runtime whose
// durable state is authoritative keeps its session, and the divergence is
// logged. Only a turn the runtime completed moved it past the history's head: an
// interrupted one did not, so there is nothing to repair.
func (e *externalExecutor) rollback(ctx context.Context) {
	e.service.handleRuntimeRoundRollback(ctx, e.plan.driver, e.req, e.plan.runtimeType, e.result.TurnCompleted)
}

// cleanup removes what this turn left behind that no history write owns: the
// decision rows it projected, the approvals nobody answered, and the leading
// user message of a turn that came to nothing. That message goes back to its
// submitter only when the input was still theirs to take back: an input that
// has left their hands (an IM message, a follow-up) stays, because nowhere else
// records it, and a round or a failure row is anchored to it besides. A run
// another owner took is not cleaned up at all: nothing here speaks for it.
func (e *externalExecutor) cleanup(ctx context.Context) {
	e.service.cancelPendingRuntimeApprovals(ctx, e.req, "tool approval cancelled: the turn ended before a decision arrived")
	e.cleanupProjections(ctx)
	if e.roundUnwritable && e.input != outcome.InputAccepted && e.plan.leadingUser != nil {
		e.service.persist.CleanupReplacementMessages(ctx, []messagepkg.Message{*e.plan.leadingUser})
	}
}

// cleanupProjections deletes the decision rows this turn projected and the
// decision projection rows it wrote for its run. Neither carries a fence: a
// turn that lost ownership still cleans up after itself, because a stranded
// decision row would stay visible and answerable.
func (e *externalExecutor) cleanupProjections(ctx context.Context) {
	// The driver cleans once from its endings, and a round whose outcome was
	// unknown is cleaned again by the reconciliation that resolves it. The rows
	// are temporary either way, so the second pass is a no-op rather than a
	// second delete of the same row.
	e.projectionsCleanup.Do(func() {
		e.service.persist.CleanupReplacementMessages(ctx, e.projections.snapshot())
		e.service.cleanupRuntimeDecisionProjectionRows(ctx, e.req)
	})
}

func (e *externalExecutor) cleanupProjectionsIn(ctx context.Context) { e.cleanupProjections(ctx) }

// promptInput is the driver call for this run.
func (e *externalExecutor) promptInput(ctx context.Context, emit func(native.StreamEvent)) external.PromptInput {
	return external.PromptInput{
		Steering:                 e.service.runtimeSteering(e.req),
		BotID:                    e.req.BotID,
		BotAgentID:               e.plan.sess.BotAgentID,
		ChatID:                   e.req.ChatID,
		ThreadID:                 e.req.ThreadID,
		RunID:                    e.req.RunID,
		RouteID:                  e.req.RouteID,
		Prompt:                   e.prompt,
		ContextMarkdown:          e.plan.contextMarkdown,
		ContextURI:               e.plan.contextURI,
		ContextBudgetMaxTokens:   e.service.runtimeContextBudgetDefault(ctx, e.req.BotID),
		Images:                   e.plan.prepared.Images,
		AttachmentReferences:     e.plan.prepared.References,
		CanFallbackImagesToFiles: e.plan.prepared.CanFallbackImagesToFiles,
		ModelID:                  runreq.FirstNonEmpty(strings.TrimSpace(e.req.RuntimeModelID), strings.TrimSpace(e.req.Model)),
		ReasoningEffort:          strings.TrimSpace(e.req.ReasoningEffort),
		SessionMode:              runreq.FirstNonEmpty(e.req.SessionType, sessionmode.Chat),
		CurrentPlatform:          e.req.CurrentChannel,
		ReplyTarget:              e.req.ReplyTarget,
		ConversationType:         e.req.ConversationType,
		Command:                  e.req.AgentCommand,
		CommandArgs:              strings.TrimSpace(strings.TrimPrefix(runreq.FirstNonEmpty(e.req.RawQuery, e.req.Query), "/"+e.req.AgentCommand)),
		ForceFreshRuntime:        e.req.ForceFreshRuntime,
		RuntimeMetadata:          e.plan.runtimeMeta,
		RuntimeOwnerAccountID:    e.plan.runtimeOwnerAccountID,
		ChannelIdentityID:        e.req.SourceChannelIdentityID,
		SessionToken:             e.req.Token,
		ToolHTTPURL:              e.req.ToolHTTPURL,
		ToolOutputLimit:          e.service.toolOutputLimit(),
		CanRequestUserInput:      !e.unattended && e.service.canDeliverUserInputStream(),
		Sink:                     external.EventSinkFunc(func(ev native.StreamEvent) { e.deliver(ctx, emit, ev) }),
	}
}

// projectionTracker remembers the decision rows one turn projected. A row
// completes when its message is written; a row that never completes is deleted
// when the turn ends, so a decision nobody answered leaves no visible trace.
type projectionTracker struct {
	mu      sync.Mutex
	pending map[string]*messagepkg.Message
}

func (t *projectionTracker) record(toolCallID string) bool {
	toolCallID = strings.TrimSpace(toolCallID)
	if toolCallID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == nil {
		t.pending = map[string]*messagepkg.Message{}
	}
	if _, exists := t.pending[toolCallID]; exists {
		return false
	}
	t.pending[toolCallID] = nil
	return true
}

func (t *projectionTracker) complete(toolCallID string, message *messagepkg.Message) {
	toolCallID = strings.TrimSpace(toolCallID)
	if toolCallID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == nil {
		return
	}
	if message == nil {
		delete(t.pending, toolCallID)
		return
	}
	t.pending[toolCallID] = message
}

func (t *projectionTracker) snapshot() []messagepkg.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) == 0 {
		return nil
	}
	out := make([]messagepkg.Message, 0, len(t.pending))
	for _, message := range t.pending {
		if message != nil {
			out = append(out, *message)
		}
	}
	return out
}

// cleanup deletes the decision rows this turn projected, and the decision
// projections it wrote for the run.

// terminalEventTypeOf names the terminal event an ending reported, which
// decides whether its round records its reasoning timing as completed or
// interrupted.
func terminalEventTypeOf(completed bool) native.StreamEventType {
	if completed {
		return native.EventAgentEnd
	}
	return native.EventAgentAbort
}
