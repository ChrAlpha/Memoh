package models

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	sdk "github.com/felinics/twilight/sdk"
)

// ErrStreamSilent marks a stream whose provider sent nothing for as long as
// its caller allows.
var ErrStreamSilent = errors.New("model stream sent nothing within its silence window")

// StreamResult answers req through model as a stream and returns what the
// stream completed with. A long answer is bounded by silence rather than by a
// deadline on its first byte or on the whole call: whatever the provider's
// response yields renews the window, including the keepalive frames the SDK
// drops, and a stream that yields nothing for silence is cut off as an
// incomplete one, which a retry may ask again for. An answer the provider gave
// before the cut keeps its own error. model must come from NewSDKChatModel,
// whose client reports what the provider's response yields.
//
//nolint:gocritic // hugeParam: the request is the model call's value DTO.
func StreamResult(ctx context.Context, model *sdk.Model, req sdk.Request, silence time.Duration) (sdk.ModelResult, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// Activity is kept as an offset from start, so the window runs on the
	// monotonic clock and a wall clock step does not move it.
	start := time.Now()
	var last atomic.Int64
	heard := func() { last.Store(int64(time.Since(start))) }
	go func() {
		quiet := time.NewTimer(silence)
		defer quiet.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-quiet.C:
			}
			left := silence - (time.Since(start) - time.Duration(last.Load()))
			if left <= 0 {
				cancel(ErrStreamSilent)
				return
			}
			quiet.Reset(left)
		}
	}()

	stream, err := model.Stream(withActivity(ctx, heard), req)
	if err != nil {
		return sdk.ModelResult{}, err
	}
	for range stream.Parts {
		heard()
	}
	result, err := stream.Result()
	var answer *sdk.APIError
	if errors.As(err, &answer) {
		return sdk.ModelResult{}, err
	}
	if errors.Is(context.Cause(ctx), ErrStreamSilent) {
		// A silent stream is cut off, as one the provider dropped is.
		return sdk.ModelResult{}, fmt.Errorf("%w: %w", sdk.ErrStreamIncomplete, ErrStreamSilent)
	}
	if err != nil {
		return sdk.ModelResult{}, err
	}
	if result == nil {
		return sdk.ModelResult{}, sdk.ErrStreamIncomplete
	}
	return *result, nil
}
