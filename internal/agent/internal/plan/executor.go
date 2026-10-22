package plan

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/internal/drive"
	"github.com/felinics/memoh/internal/agent/internal/outcome"
	"github.com/felinics/memoh/internal/agent/internal/persist"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
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
	committer *persist.StepCommitter // writes the run's history, step by step

	// Written by execute, read by commit after execute returned.
	terminal    *native.StreamEvent
	uncommitted persist.UncommittedStepText
	// text and usage are the run's result as the terminal event reports it,
	// kept apart from the history write so a scheduled fire reads them
	// whichever way the history was written.
	text  string
	usage json.RawMessage
	// stepErr is the failure the step committer reported while the stream was
	// still running.
	stepErr error
}

// execute streams the turn and hands every event to emit except the terminal
// event, which it keeps: the driver publishes that after the ending's history
// write has succeeded.
func (e *nativeExecutor) Execute(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
	for ev := range e.agent.Stream(ctx, e.cfg) {
		e.uncommitted.Observe(ev)
		if ev.IsTerminal() {
			terminal := ev
			e.terminal = &terminal
			e.observeTerminal(terminal)
			continue
		}
		if e.takeStepFailure(ev) {
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

// takeStepFailure keeps the failure event the runtime raised for a step the
// committer could not write, instead of publishing it. The runtime knows only
// that its commit callback failed; the run's failure policy names a history
// write failure itself, under the commit phase, and the driver publishes that
// name. An unrelated error event, or a step lost with the run's ownership, is
// not taken.
func (e *nativeExecutor) takeStepFailure(ev native.StreamEvent) bool {
	if ev.Type != native.EventError || e.stepErr != nil {
		return false
	}
	failure := e.committer.StepFailure()
	if failure == nil || !errors.Is(ev.Cause, failure) {
		return false
	}
	e.stepErr = failure
	return true
}

// observeTerminal records the result the terminal event carries: the run's
// final text and its usage.
func (e *nativeExecutor) observeTerminal(ev native.StreamEvent) {
	// A failure is reported as an error event that carries no messages.
	public := outcome.PublicAgentStreamEvent(ev)
	if len(public.Messages) == 0 {
		return
	}
	var messages []sdk.Message
	if err := json.Unmarshal(public.Messages, &messages); err != nil || len(messages) == 0 {
		return
	}
	e.text = finalText(runreq.SDKMessagesWithOrigins(messages, public.InternalFeedbackIndexes))
	e.usage = public.Usage
}

// deferred reports whether the terminal event parked the run on a decision.
func (e *nativeExecutor) deferred() bool {
	return e.terminal != nil && strings.TrimSpace(e.terminal.ApprovalID) != ""
}

// commit writes the ending's history: the failure row or the interrupted step
// when the run failed, and the run's final messages in every case.
func (e *nativeExecutor) commit(ctx context.Context, f *outcome.RunFailure, row bool) (drive.CommitSummary, error) {
	summary := drive.CommitSummary{ModelID: e.modelID(), Text: e.text, Usage: e.usage}
	if e.stepErr != nil {
		// The history has a gap the run cannot close: the ending's write fails
		// with it, which keeps the terminal from being published.
		return summary, outcome.RuntimeHistoryError(e.stepErr)
	}
	if row && f != nil {
		persisted, err := e.committer.RecordFailure(ctx, e.uncommitted.String(), *f)
		if err != nil {
			return summary, outcome.RuntimeHistoryError(err)
		}
		summary.Persisted = append(summary.Persisted, persisted...)
	}
	tokens := e.inputTokens()
	if err := e.committer.Finish(ctx, tokens); err != nil {
		return summary, outcome.RuntimeHistoryError(err)
	}
	summary.Persisted = append(summary.Persisted, e.committer.PersistedMessages()...)
	summary.UsageTokens = tokens
	return summary, nil
}

// inputTokens is the token count the ending's history write accounts the run
// with: what the terminal event reported, or the resolution's estimate when the
// run ended without one.
func (e *nativeExecutor) inputTokens() int {
	if tokens := persist.ExtractInputTokensFromUsage(e.usage); tokens > 0 {
		return tokens
	}
	return e.rc.EstimatedTokens
}

func (e *nativeExecutor) modelID() string {
	return e.rc.Model.ID
}

// finalText is the text of the last assistant message of the run's terminal
// messages. A schedule fire reports it as the run's result; every other entry
// reads its text from the projection.
func finalText(messages []runreq.ModelMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "assistant") {
			return strings.TrimSpace(messages[i].TextContent())
		}
	}
	return ""
}
