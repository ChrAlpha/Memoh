package agenttest

import (
	"context"
	"errors"

	session "github.com/felinics/memoh/internal/chat/thread"
)

// Sessions is a session-service double driven by per-method functions.
type Sessions struct {
	GetFn                  func(ctx context.Context, sessionID string) (session.Thread, error)
	UpdateMetadataFn       func(ctx context.Context, sessionID string, metadata map[string]any) (session.Thread, error)
	mergeRuntimeMetadataFn func(ctx context.Context, sessionID, runtimeType string, delta map[string]any) (session.Thread, error)
}

func (f *Sessions) Get(ctx context.Context, sessionID string) (session.Thread, error) {
	if f == nil || f.GetFn == nil {
		return session.Thread{}, errors.New("unexpected Get call")
	}
	return f.GetFn(ctx, sessionID)
}

func (*Sessions) UpdateTitle(context.Context, string, string) (session.Thread, error) {
	return session.Thread{}, errors.New("unexpected UpdateTitle call")
}

func (f *Sessions) UpdateMetadata(ctx context.Context, sessionID string, metadata map[string]any) (session.Thread, error) {
	if f == nil || f.UpdateMetadataFn == nil {
		return session.Thread{}, errors.New("unexpected UpdateMetadata call")
	}
	return f.UpdateMetadataFn(ctx, sessionID, metadata)
}

func (f *Sessions) MergeRuntimeMetadata(ctx context.Context, sessionID, runtimeType string, delta map[string]any) (session.Thread, error) {
	if f == nil || f.mergeRuntimeMetadataFn == nil {
		return session.Thread{}, errors.New("unexpected MergeRuntimeMetadata call")
	}
	return f.mergeRuntimeMetadataFn(ctx, sessionID, runtimeType, delta)
}
