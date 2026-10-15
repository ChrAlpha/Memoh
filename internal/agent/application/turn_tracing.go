package application

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/telemetry"
)

// startTurnSpan opens the span that covers one agent turn and returns the
// function that closes it.
//
// The run driver opens it, once per run, so every entry has one: the turn
// port's StartTurn, the web UI's WebSocket, a schedule fire, a decision
// continuation, a discuss batch and a resumed session all run through the
// driver. A span opened by an entry would be missing for the entries that
// forgot it, and nothing would say so — the traces would simply have no turn in
// them.
//
// The turn is the only span that says how long an answer took. Everything
// below it — the model call, each tool call, the queries they make — is a
// fragment of that number.
//
// StartDetached, not Start, because whether a turn belongs inside its caller's
// trace depends on how it arrived. Over the internal RPC it does: Channel is
// waiting on the call, and the trace has to stay continuous across the hop.
// Over a WebSocket or off the inbound queue it does not: the thing that asked
// for it is a connection that outlives the turn or a request that was already
// answered, so the turn is its own trace and links back. The ingress says
// which by putting a trigger in the context.
func startTurnSpan(ctx context.Context, req ChatRequest) (context.Context, func(error)) {
	ctx, span := telemetry.StartDetached(ctx, "agent.turn",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("agent.bot_id", req.BotID),
			attribute.String("agent.thread_id", req.ThreadID),
		),
	)
	return ctx, func(err error) {
		switch {
		// err is what the run's ending names (runSpanCause), not what the run's
		// context looks like by now: the session runtime cancels a run's context
		// once its terminal state is written, so at this point a completed run
		// and a stopped one both have a cancelled context.
		//
		// A run someone stopped is not a failure. Marking cancellations as
		// errors would make the error rate track how often users press stop.
		// The ending names a stop as the cancellation itself, never as an error
		// that wraps one: a run this process ended for its own reason, such as
		// the idle timeout or a lost run, ended the turn in failure.
		case err == context.Canceled: //nolint:errorlint // RN 2.8: identity, not a wrapped cancellation.
			span.SetAttributes(attribute.String("agent.turn.outcome", "aborted"))
		case err != nil:
			span.SetAttributes(attribute.String("agent.turn.outcome", "errored"))
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		default:
			span.SetAttributes(attribute.String("agent.turn.outcome", "completed"))
		}
		span.End()
	}
}

// runSpanCause is what a driven run's turn span records: the failure's public
// error, a cancellation for a stop (a run someone stopped is not a failure),
// and nothing for a run that completed or parked on a decision.
func runSpanCause(result runResult) error {
	switch result.End {
	case endFailed:
		if result.Failure != nil {
			return result.Failure.Public()
		}
		return errs.New("agent run failed")
	case endStopped:
		return context.Canceled
	case endOwnershipLost:
		return sessionruntime.ErrRunOwnershipLost
	default:
		return nil
	}
}
