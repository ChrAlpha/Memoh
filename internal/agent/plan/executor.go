package plan

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

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
	persist   *persist.Writer

	// Written by execute, read by commit after execute returned.
	terminal  *native.StreamEvent
	snapshot  *persist.TerminalSnapshot
	visible   bool
	toolCalls int
	// text and usage are the run's result as the terminal event reports it,
	// kept apart from the history write so a scheduled fire reads them
	// whichever way the history was written.
	text        string
	usage       json.RawMessage
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
	e.text = terminalText(snap)
	e.usage = snap.Usage
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
	summary := drive.CommitSummary{ModelID: e.modelID(), Text: e.text, Usage: e.usage}
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
		return summary, nil
	}
	if e.snapshot == nil {
		// Nothing was checkpointed and the stream never produced a terminal
		// snapshot. A run with visible output has nowhere to write it, which is
		// a history failure rather than a success.
		if e.visible {
			return summary, outcome.RuntimeHistoryError(errors.New("agent terminal event has no persistable snapshot"))
		}
		return summary, nil
	}
	snap := *e.snapshot
	if row && f != nil && snap.VisibleOutput {
		// With visible output the failure rides on the snapshot's answer.
		// Without it, recordFailure already wrote the failure row, and the
		// snapshot carrying the failure too would write it a second time.
		snap.Failure = *f
	}
	persisted, err := e.persist.TerminalSnapshotResult(ctx, e.req, e.rc, snap)
	if err != nil {
		return summary, outcome.RuntimeHistoryError(err)
	}
	summary.Persisted = persisted
	summary.UsageTokens = persist.ExtractInputTokensFromUsage(snap.Usage)
	return summary, nil
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
		// The snapshot's answer carries the failure; the snapshot path writes it.
		return nil, nil
	}
	return e.persist.FailureRow(ctx, e.req, e.rc, f, "")
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

// terminalText is the text of the last assistant message the run's terminal
// snapshot carries. A schedule fire reports it as the run's result; every other
// entry reads its text from the projection.
func terminalText(snap persist.TerminalSnapshot) string {
	messages := snap.OutputMessages()
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "assistant") {
			return strings.TrimSpace(messages[i].TextContent())
		}
	}
	return ""
}
