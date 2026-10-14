package application

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/telemetry"
)

// startTurnSpan opens the span that covers one agent turn and returns the
// function that closes it.
//
// It is opened where a turn actually runs rather than on the turn port's
// StartTurn. StartTurn is only one of the ways in: the web UI's WebSocket
// starts its runs through startWebRun and never touches it. A span on
// StartTurn alone would be missing for the entry point most turns arrive
// through, and nothing would say so — the traces would simply have no turn in
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
		// A run someone stopped is not a failure. Marking cancellations as
		// errors would make the error rate track how often users press stop.
		// Only the caller's cancellation counts: a context this process ended
		// for its own reason, such as the idle timeout or a lost run, ended
		// the turn in failure.
		// The turn's own error counts only when it is that cancellation itself,
		// not an error that wraps it.
		case errs.CallerEnded(ctx) || err == context.Canceled: //nolint:errorlint // RN 2.8: identity, not a wrapped cancellation.
			span.SetAttributes(attribute.String("agent.turn.outcome", "aborted"))
		case err != nil || ctx.Err() != nil:
			if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if cause := context.Cause(ctx); cause != nil {
					err = cause
				}
			}
			span.SetAttributes(attribute.String("agent.turn.outcome", "errored"))
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		default:
			span.SetAttributes(attribute.String("agent.turn.outcome", "completed"))
		}
		span.End()
	}
}
