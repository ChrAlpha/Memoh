package native

import (
	"context"
	"errors"
	"fmt"
	"strings"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	tools "github.com/felinics/memoh/internal/agent/tool"
)

// SpawnRunBinder arms one spawned run the way the application arms every
// admitted native run: it fills rc with the agent thread's history under its
// checkpoint, the task's request row as the input the run answers, persists
// each step as it commits, and compacts the request in place when a step does
// not fit. It pauses watchdog for the work it does that streams nothing. The
// call it returns closes the run's persistence once the run has ended; a
// failed close of a completed run fails the run.
type SpawnRunBinder func(
	ctx context.Context,
	task tools.SpawnRunConfig,
	rc *RunConfig,
	watchdog *tools.SubagentWatchdog,
) (func(context.Context) error, error)

// SpawnRunObservation carries the terminal outcome selected by the session
// runtime that serialized terminal publication against routed controls.
type SpawnRunObservation struct {
	TerminalResolved bool
	TerminalOutcome  tools.SpawnAttemptDisposition
}

// SpawnRunObserver publishes one spawned-run event and reports a terminal
// outcome when the session runtime resolved one. Nonterminal events and
// observers without a session runtime return the zero observation.
type SpawnRunObserver func(StreamEvent) SpawnRunObservation

// SpawnRunObserverFactory builds the per-event publisher for one spawned run.
// A nil return means nothing observes this run and events are not forwarded.
type SpawnRunObserverFactory func(ctx context.Context) SpawnRunObserver

var errSpawnAgentAborted = errors.New("agent run aborted")

var errSpawnRunUnbound = errors.New("spawned run has no binder")

// SpawnAdapter wraps *Agent to satisfy tools.SpawnAgent without creating
// an import cycle (tools -> agent).
type SpawnAdapter struct {
	agent       *Agent
	bind        SpawnRunBinder
	runObserver SpawnRunObserverFactory
	failure     func(StreamEvent) error
}

// NewSpawnAdapter creates a SpawnAdapter from the given Agent.
func NewSpawnAdapter(a *Agent) *SpawnAdapter {
	return &SpawnAdapter{agent: a}
}

// SetRunBinder installs what arms each spawned run. Without one a spawned run
// fails before it starts: nothing would persist its steps.
func (s *SpawnAdapter) SetRunBinder(b SpawnRunBinder) {
	s.bind = b
}

// SetRunObserverFactory installs live event publishing for spawned runs.
func (s *SpawnAdapter) SetRunObserverFactory(f SpawnRunObserverFactory) {
	s.runObserver = f
}

// SetFailureTranslator installs the error a spawned attempt reports for the
// error event it ended with. The application names that failure as it names
// any native run's, so the spawn provider reads the code the run failed with.
// Without one the attempt reports the event's cause.
func (s *SpawnAdapter) SetFailureTranslator(f func(StreamEvent) error) {
	s.failure = f
}

// attemptFailure is the error a spawned attempt reports for the error event
// it ended with.
func (s *SpawnAdapter) attemptFailure(event StreamEvent) error {
	if s.failure != nil {
		if err := s.failure(event); err != nil {
			return err
		}
	}
	return event.Cause
}

func runConfigFromSpawnRunConfig(cfg tools.SpawnRunConfig) RunConfig {
	identity := SessionContext{
		BotID:               cfg.Identity.BotID,
		ChatID:              cfg.Identity.ChatID,
		SessionID:           cfg.Identity.SessionID,
		UserID:              cfg.Identity.UserID,
		ChannelIdentityID:   cfg.Identity.ChannelIdentityID,
		CurrentPlatform:     cfg.Identity.CurrentPlatform,
		ReplyTarget:         cfg.Identity.ReplyTarget,
		ConversationType:    cfg.Identity.ConversationType,
		SessionToken:        cfg.Identity.SessionToken,
		WorkspaceTargetID:   cfg.Identity.WorkspaceTargetID,
		WorkspaceTargetKind: cfg.Identity.WorkspaceTargetKind,
		WorkspaceTargetName: cfg.Identity.WorkspaceTargetName,
		WorkdirPath:         cfg.Identity.WorkdirPath,
		TimezoneLocation:    cfg.Identity.TimezoneLocation,
		IsSubagent:          cfg.Identity.IsSubagent,
	}
	skills := make([]SkillEntry, 0, len(cfg.Skills))
	for name, skill := range cfg.Skills {
		skills = append(skills, SkillEntry{
			Name:        name,
			Description: skill.Description,
			Content:     skill.Content,
			Path:        skill.Path,
		})
	}
	return RunConfig{
		RunID:                    strings.TrimSpace(cfg.RunID),
		Model:                    cfg.Model,
		CurrentModelUUID:         cfg.ModelUUID,
		CurrentModelID:           cfg.ModelID,
		CurrentModelProvider:     cfg.ModelProvider,
		CurrentModelProviderID:   cfg.ModelProviderID,
		SessionType:              cfg.SessionType,
		ReasoningConfig:          cfg.ReasoningConfig,
		ReasoningStoredEffort:    cfg.ReasoningStoredEffort,
		ReasoningRequestedEffort: cfg.ReasoningRequestedEffort,
		PromptCacheTTL:           cfg.PromptCacheTTL,
		ChatCompletionsCompat:    cfg.ChatCompletionsCompat,
		SupportsImageInput:       cfg.SupportsImageInput,
		SupportsFileInput:        cfg.SupportsFileInput,
		SupportsToolCall:         cfg.SupportsToolCall,
		Identity:                 identity,
		Skills:                   skills,
		BackgroundManager:        cfg.BackgroundManager,
		ContextBudgetMaxTokens:   cfg.ContextBudgetMaxTokens,
		ModelMaxOutputTokens:     cfg.ModelMaxOutputTokens,
		ContextScope: contextfrag.Scope{
			BotID:             identity.BotID,
			ChatID:            identity.ChatID,
			SessionID:         identity.SessionID,
			ChannelIdentityID: identity.ChannelIdentityID,
			Platform:          identity.CurrentPlatform,
		},
		LoopDetection: LoopDetectionConfig{
			Enabled: cfg.LoopDetection.Enabled,
		},
		ContextLifecycle: contextfrag.NewLifecycleHolder(),
	}
}

// GenerateWithWatchdog runs one spawned task in streaming mode: the binder
// arms the run, every stream event (token, tool progress, etc.) touches
// watchdog, and the result is collected once the stream ends.
func (s *SpawnAdapter) GenerateWithWatchdog(ctx context.Context, cfg tools.SpawnRunConfig, watchdog *tools.SubagentWatchdog) (*tools.SpawnResult, error) {
	rc := runConfigFromSpawnRunConfig(cfg)
	var observe SpawnRunObserver
	if s.runObserver != nil {
		observe = s.runObserver(ctx)
	}
	finish, err := s.bindRun(ctx, cfg, &rc, watchdog)
	if err != nil {
		outcome := observeSpawnAttemptFailure(ctx, observe, cfg, nil, []StreamEvent{{Type: EventError, Cause: err}}, err)
		if cfg.ReconcileTerminal != nil {
			cfg.ReconcileTerminal(outcome)
		}
		return spawnFailureResult(rc), err
	}
	closed := false
	closeRun := func() error {
		if closed {
			return nil
		}
		closed = true
		return finish(context.WithoutCancel(ctx))
	}
	defer func() { _ = closeRun() }()

	// Use Stream instead of Generate to get per-token/per-tool activity signals.
	eventCh := s.agent.Stream(ctx, rc)

	var allText strings.Builder
	var lastFailure *StreamEvent
	completed := false
	var abortEvent *StreamEvent
	var endEvent *StreamEvent
	var pendingErrors []StreamEvent

	for evt := range eventCh {
		// Touch the watchdog on every event — this is the activity signal.
		watchdog.Touch()
		switch evt.Type {
		case EventError:
			// The stream reports only the failure it ends with; a failure it
			// retries appears as EventRetry alone. The error is still held
			// until the caller resolves the following abort: owning
			// cancellation must not be mislabeled as a provider failure merely
			// because the two raced.
			pendingErrors = append(pendingErrors, evt)
		case EventAgentAbort:
			// The spawn provider owns the outer retry loop, so it decides below
			// whether this is a terminal abort or only the end of one attempt.
		case EventAgentEnd:
			// A clean end is authoritative. Any un-retried transient error did not
			// become this attempt's outcome and must not poison the terminal view.
			pendingErrors = nil
		default:
			// Published before this loop reads anything out of the event, so a
			// subscriber to the spawned session never lags the parent's view.
			observeSpawnEvent(observe, evt)
		}

		switch evt.Type {
		case EventTextDelta:
			allText.WriteString(evt.Delta)
		case EventError:
			failure := evt
			lastFailure = &failure
		case EventAgentEnd, EventAgentAbort:
			completed = evt.Type == EventAgentEnd
			if completed {
				terminal := evt
				endEvent = &terminal
			} else {
				terminal := evt
				abortEvent = &terminal
			}
		}
	}

	var runErr error
	// Check if context was cancelled (watchdog fired or parent cancelled).
	if !completed && ctx.Err() != nil {
		if cause := context.Cause(ctx); cause != nil {
			runErr = cause
		} else {
			runErr = ctx.Err()
		}
	}
	// A stream that errored without reaching a clean end is a failed attempt,
	// not a short answer: the error it ended with is the attempt's error, named
	// as the run names it and keeping the runtime's whole chain, so the caller
	// sees the provider's own failure. A failure the run recovered from never
	// reaches here; the stream retried it without reporting it.
	if runErr == nil && !completed {
		if lastFailure != nil {
			runErr = s.attemptFailure(*lastFailure)
		}
		if runErr == nil {
			runErr = errSpawnAgentAborted
		}
	}
	if runErr != nil {
		outcome := observeSpawnAttemptFailure(ctx, observe, cfg, abortEvent, pendingErrors, runErr)
		if cfg.ReconcileTerminal != nil {
			cfg.ReconcileTerminal(outcome)
		}
		return spawnFailureResult(rc), runErr
	}
	disposition := tools.SpawnAttemptCompleted
	if cfg.ResolveCompletion != nil {
		disposition = cfg.ResolveCompletion()
	}
	spawnResult := &tools.SpawnResult{Text: allText.String()}
	if snapshot, ok := rc.ContextLifecycle.Snapshot(); ok {
		spawnResult.ContextLifecycle = &snapshot
	}
	// The steps are durable as they commit; what closes the run's persistence
	// lands before the run is published as completed.
	if disposition == tools.SpawnAttemptCompleted {
		if closeErr := closeRun(); closeErr != nil {
			if cfg.ReconcileTerminal != nil {
				cfg.ReconcileTerminal(tools.SpawnAttemptFailure)
			}
			return spawnFailureResult(rc), fmt.Errorf("close subagent run before publication: %w", closeErr)
		}
	}
	terminal := StreamEvent{Type: EventAgentEnd}
	if endEvent != nil {
		terminal = *endEvent
	}
	if disposition != tools.SpawnAttemptCompleted {
		terminal.Type = EventAgentAbort
		if disposition != tools.SpawnAttemptAbort {
			observeSpawnEvent(observe, StreamEvent{Type: EventError, Cause: errSpawnAgentAborted})
		}
	}
	observation := observeSpawnEvent(observe, terminal)
	if observation.TerminalResolved {
		disposition = observation.TerminalOutcome
		if cfg.ReconcileTerminal != nil {
			cfg.ReconcileTerminal(disposition)
		}
	}
	if disposition != tools.SpawnAttemptCompleted {
		cause := context.Cause(ctx)
		if cause == nil {
			if disposition == tools.SpawnAttemptAbort {
				cause = context.Canceled
			} else {
				cause = errSpawnAgentAborted
			}
		}
		return spawnFailureResult(rc), cause
	}

	return spawnResult, nil
}

func observeSpawnAttemptFailure(
	ctx context.Context,
	observe SpawnRunObserver,
	cfg tools.SpawnRunConfig,
	abortEvent *StreamEvent,
	pendingErrors []StreamEvent,
	runErr error,
) tools.SpawnAttemptDisposition {
	disposition := tools.SpawnAttemptFailure
	if cfg.ResolveAttempt != nil {
		disposition = cfg.ResolveAttempt(runErr)
	} else if errors.Is(context.Cause(ctx), context.Canceled) {
		disposition = tools.SpawnAttemptAbort
	}
	if observe == nil {
		return disposition
	}
	reconcile := func(observation SpawnRunObservation) tools.SpawnAttemptDisposition {
		if observation.TerminalResolved {
			return observation.TerminalOutcome
		}
		return disposition
	}
	switch disposition {
	case tools.SpawnAttemptRetry:
		// The attempt is run again, so its failure is not the run's: the
		// session sees the retry and nothing else, exactly as for a model call
		// the stream retried.
		attempt := cfg.Attempt
		if attempt <= 0 {
			attempt = 1
		}
		maxAttempts := cfg.MaxAttempts
		if maxAttempts < attempt {
			maxAttempts = attempt
		}
		observeSpawnEvent(observe, StreamEvent{
			Type:       EventRetry,
			Attempt:    attempt,
			MaxAttempt: maxAttempts,
		})
		return disposition
	case tools.SpawnAttemptAbort:
		if abortEvent != nil {
			return reconcile(observeSpawnEvent(observe, *abortEvent))
		}
		return disposition
	default:
		observeSpawnEvents(observe, pendingErrors)
		if len(pendingErrors) == 0 {
			observeSpawnEvent(observe, StreamEvent{Type: EventError, Cause: errSpawnAgentAborted})
		}
		if abortEvent != nil {
			return reconcile(observeSpawnEvent(observe, *abortEvent))
		}
		return disposition
	}
}

func observeSpawnEvents(observe SpawnRunObserver, events []StreamEvent) {
	if observe == nil {
		return
	}
	for _, event := range events {
		observeSpawnEvent(observe, event)
	}
}

func observeSpawnEvent(observe SpawnRunObserver, event StreamEvent) SpawnRunObservation {
	if observe == nil {
		return SpawnRunObservation{}
	}
	return observe(event)
}

func spawnFailureResult(rc RunConfig) *tools.SpawnResult {
	if rc.ContextLifecycle == nil {
		return nil
	}
	snapshot, ok := rc.ContextLifecycle.Snapshot()
	if !ok {
		return nil
	}
	return &tools.SpawnResult{ContextLifecycle: &snapshot}
}

// bindRun arms the run through the installed binder.
func (s *SpawnAdapter) bindRun(ctx context.Context, cfg tools.SpawnRunConfig, rc *RunConfig, watchdog *tools.SubagentWatchdog) (func(context.Context) error, error) {
	if s.bind == nil {
		return nil, errSpawnRunUnbound
	}
	return s.bind(ctx, cfg, rc, watchdog)
}
