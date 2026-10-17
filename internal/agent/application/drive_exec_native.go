package application

import (
	"context"
	"errors"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// nativeStreamer is the part of *native.Agent a native run needs: it turns a
// RunConfig into a stream of events. Narrowing it here is what lets the driver
// be tested without a provider.
type nativeStreamer interface {
	Stream(ctx context.Context, cfg native.RunConfig) <-chan native.StreamEvent
}

// nativeExecutor runs one turn on the built-in agent.
type nativeExecutor struct {
	agent     nativeStreamer
	cfg       native.RunConfig
	rc        runreq.ResolvedContext
	req       runreq.ChatRequest
	committer *persist.StepCommitter // nil when steps are not committed as they complete
	timing    *persist.ReasoningTimingTracker
	replace   *drive.TurnReplacement
	service   *Service

	// Written by execute, read by commit after execute returned.
	terminal    *native.StreamEvent
	snapshot    *persist.TerminalSnapshot
	visible     bool
	toolCalls   int
	uncommitted persist.UncommittedStepText
	// interrupted is the failure the step committer reported while the stream
	// was still running.
	stepErr error
}

// execute streams the turn and hands every event to emit except the terminal
// event, which it keeps: the driver publishes that after the ending's history
// write has succeeded.
func (e *nativeExecutor) Execute(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
	for ev := range e.agent.Stream(ctx, e.cfg) {
		if ev.Type == native.EventToolCallStart {
			e.toolCalls++
		}
		if outcome.HasVisibleAgentStreamOutput(ev) {
			e.visible = true
		}
		e.uncommitted.Observe(ev)
		if ev.IsTerminal() {
			terminal := ev
			e.terminal = &terminal
			e.observeTerminal(terminal)
			continue
		}
		emit(ev)
	}
	ending := drive.ExecEnding{
		Terminal: e.terminal,
		Commit:   e.commit,
	}
	if e.terminal != nil {
		ending.Deferred = e.deferred()
	}
	if e.stepErr != nil {
		// A step that failed to persist is the run's failure even when the
		// stream went on to end cleanly.
		ending.Cause = e.stepErr
		ending.Phase = outcome.PhaseCommit
	}
	return ending
}

// observeTerminal decodes what the terminal event carries into the snapshot
// the ending's history write persists.
func (e *nativeExecutor) observeTerminal(ev native.StreamEvent) {
	if len(ev.Messages) == 0 {
		return
	}
	data := drive.MarshalRunEvent(ev)
	if len(data) == 0 {
		return
	}
	snap, ok := persist.ExtractTerminalSnapshot(data)
	if !ok {
		return
	}
	if e.committer == nil {
		snap.ReasoningTiming = persist.TakeTerminalReasoningTiming(e.timing, ev.Type)
	}
	snap.VisibleOutput = e.visible
	e.snapshot = &snap
}

// deferred reports whether the terminal event parked the run on a decision.
func (e *nativeExecutor) deferred() bool {
	if e.snapshot != nil && e.snapshot.DeferredToolID != "" {
		return true
	}
	return e.terminal != nil && e.terminal.ApprovalID != ""
}

// commit writes the ending's history: the failure row or the interrupted step
// when the run failed, and the run's final messages in every case.
func (e *nativeExecutor) commit(ctx context.Context, f *outcome.RunFailure, row bool) (drive.CommitSummary, error) {
	summary := drive.CommitSummary{ModelID: e.modelID()}
	if row && f != nil {
		persisted, err := e.recordFailure(ctx, *f, summary)
		if err != nil {
			return summary, err
		}
		summary.Persisted = append(summary.Persisted, persisted...)
	}
	if e.committer != nil {
		tokens := e.inputTokens()
		if err := e.committer.Finish(ctx, tokens); err != nil {
			return summary, outcome.RuntimeHistoryError(err)
		}
		summary.Persisted = append(summary.Persisted, e.committer.PersistedMessages()...)
		summary.UsageTokens = tokens
		// A replacement turn that commits its steps is finalized by its queue
		// coordinator in the final step's transaction; rewriting it here
		// again would find no visible old turn left to replace.
		return summary, nil
	}
	if e.snapshot == nil {
		// Nothing was checkpointed and the stream never produced a terminal
		// snapshot. A run with visible output has nowhere to write it, which is
		// a history failure rather than a success.
		if e.visible {
			return summary, outcome.RuntimeHistoryError(errors.New("agent terminal event has no persistable snapshot"))
		}
		return summary, e.postPersist(ctx, summary)
	}
	snap := *e.snapshot
	snap.FailureCode = persist.FailureRowCode(f)
	if !row || !snap.VisibleOutput {
		// Without visible output the failure row is already written by
		// recordFailure; the snapshot carrying the code too would write it a
		// second time.
		snap.FailureCode = ""
	}
	persisted, err := e.service.persist.TerminalSnapshotResult(ctx, e.req, e.rc, snap)
	if err != nil {
		return summary, outcome.RuntimeHistoryError(err)
	}
	summary.Persisted = persisted
	summary.Text = terminalText(snap)
	summary.Usage = snap.Usage
	summary.UsageTokens = persist.ExtractInputTokensFromUsage(snap.Usage)
	return summary, e.postPersist(ctx, summary)
}

// recordFailure writes the failure into the run's history: the text the failed
// step showed, or the turn-level failure row when nothing was committed.
func (e *nativeExecutor) recordFailure(ctx context.Context, f outcome.RunFailure, _ drive.CommitSummary) ([]messagepkg.Message, error) {
	if e.committer != nil {
		persisted, err := e.committer.RecordFailure(ctx, e.uncommitted.String(), f)
		if err != nil {
			return nil, outcome.RuntimeHistoryError(err)
		}
		return persisted, nil
	}
	if e.snapshot != nil && e.snapshot.VisibleOutput {
		// A committed-ish snapshot carries the failure as its own row; the
		// snapshot path writes it.
		return nil, nil
	}
	return e.service.persist.FailureRow(ctx, e.req, e.rc, f, "")
}

// postPersist rewrites the replaced turn once its own history is written, for
// a run without step commits. A replacement turn that cannot be rewritten
// failed, and the failure belongs to the ending rather than to the turn it
// replaced.
func (e *nativeExecutor) postPersist(ctx context.Context, summary drive.CommitSummary) error {
	if e.replace == nil || e.replace.PostPersist == nil {
		return nil
	}
	if err := e.replace.PostPersist(ctx, summary.Persisted); err != nil {
		return outcome.RuntimeHistoryError(err)
	}
	return nil
}

// inputTokens is the token count the ending's history write accounts the run
// with: what the terminal event reported, or the resolution's estimate when the
// run ended without one.
func (e *nativeExecutor) inputTokens() int {
	if e.snapshot != nil {
		if tokens := persist.ExtractInputTokensFromUsage(e.snapshot.Usage); tokens > 0 {
			return tokens
		}
	}
	return e.rc.EstimatedTokens
}

func (e *nativeExecutor) modelID() string {
	return e.rc.Model.ID
}

// terminalText is the assistant text the run's final snapshot carries. A
// schedule fire reports it as the run's result; every other entry reads its
// text from the projection.
func terminalText(snap persist.TerminalSnapshot) string {
	var builder strings.Builder
	for _, msg := range snap.SDKMessages {
		if msg.Role != sdk.MessageRoleAssistant {
			continue
		}
		for _, part := range msg.Content {
			if text, ok := part.(sdk.TextPart); ok {
				builder.WriteString(text.Text)
			}
		}
	}
	return builder.String()
}
