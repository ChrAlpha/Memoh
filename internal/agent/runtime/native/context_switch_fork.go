package native

import (
	"context"
	"errors"
	"fmt"
	"time"

	sdk "github.com/felinics/twilight/sdk"
)

// defaultForkSilence is how long a fork request may stay silent when the run
// names no model window.
const defaultForkSilence = 5 * time.Minute

// errForkIdle marks a summary stream that fell silent.
var errForkIdle = errors.New("summary stream sent nothing within the model window")

// forkSummarizer answers a compaction event's fork requests through the
// run's own model, streaming, so a long summary is bounded by silence rather
// than by a deadline on its first byte: a stream that sends nothing for the
// run's model window is cut off. The call is no step of the run: it bypasses
// the run's call observer.
func forkSummarizer(model *sdk.Model, silence time.Duration) func(context.Context, sdk.Request) (sdk.ModelResult, error) {
	model = unobservedModel(model)
	if silence <= 0 {
		silence = defaultForkSilence
	}
	return func(ctx context.Context, req sdk.Request) (sdk.ModelResult, error) {
		if model == nil {
			return sdk.ModelResult{}, errors.New("compaction fork: the run has no model")
		}
		ctx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		stream, err := model.Stream(ctx, req)
		if err != nil {
			return sdk.ModelResult{}, err
		}
		quiet := time.AfterFunc(silence, func() { cancel(errForkIdle) })
		defer quiet.Stop()
		for range stream.Parts {
			quiet.Reset(silence)
		}
		result, err := stream.Result()
		if errors.Is(context.Cause(ctx), errForkIdle) {
			// A silent stream is cut off, as one the provider dropped is.
			return sdk.ModelResult{}, fmt.Errorf("%w: %w", sdk.ErrStreamIncomplete, errForkIdle)
		}
		if err != nil {
			return sdk.ModelResult{}, err
		}
		if result == nil {
			return sdk.ModelResult{}, sdk.ErrStreamIncomplete
		}
		return *result, nil
	}
}

// unobservedModel is model without the run's call observers, which time and
// publish the run's own model calls.
func unobservedModel(model *sdk.Model) *sdk.Model {
	if model == nil {
		return nil
	}
	plain := *model
	for {
		observer, ok := plain.Provider.(providerCallObserver)
		if !ok {
			return &plain
		}
		plain.Provider = observer.Provider
	}
}
