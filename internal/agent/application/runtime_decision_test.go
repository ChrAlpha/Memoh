package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/memohai/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/memohai/memoh/internal/agent/runtime/session"
	"github.com/memohai/memoh/internal/agent/turn"
	"github.com/memohai/memoh/internal/apperror"
)

func TestRuntimeDecisionTerminalDoesNotExposePrivateErrors(t *testing.T) {
	tests := []struct {
		name         string
		contextCause error
		cause        error
		status       string
		message      string
	}{
		{name: "success"},
		{
			name:         "explicit cancellation",
			contextCause: context.Canceled,
			cause:        context.Canceled,
		},
		{
			name:   "provider cancellation with active context",
			cause:  context.Canceled,
			status: sessionruntime.RunStatusErrored,
		},
		{
			name:         "ownership loss",
			contextCause: sessionruntime.ErrRunOwnershipLost,
			cause:        context.Canceled,
			status:       sessionruntime.RunStatusErrored,
		},
		{
			name:   "private provider error",
			cause:  errors.New("private provider detail"),
			status: sessionruntime.RunStatusErrored,
		},
		{
			name:    "stable application error",
			cause:   apperror.New(apperror.CodeSessionHistoryInconsistent, nil),
			status:  sessionruntime.RunStatusErrored,
			message: string(apperror.CodeSessionHistoryInconsistent),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.contextCause != nil {
				var cancel context.CancelCauseFunc
				ctx, cancel = context.WithCancelCause(ctx)
				cancel(tt.contextCause)
			}
			status, message := runtimeDecisionTerminal(ctx, tt.cause)
			if status != tt.status || message != tt.message {
				t.Fatalf("runtimeDecisionTerminal() = (%q, %q), want (%q, %q)", status, message, tt.status, tt.message)
			}
		})
	}
}

func TestContinueRuntimeDecisionDoesNotParkProviderCancellation(t *testing.T) {
	const (
		botID     = "bot-provider-cancel"
		sessionID = "session-provider-cancel"
		runID     = "run-provider-cancel"
	)
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-provider-cancel",
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Second,
		CommandAckTTL: time.Second,
	})
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("start runtime manager: %v", err)
	}
	handle, err := manager.StartRunHandle(
		context.Background(),
		botID,
		sessionID,
		runID,
		make(chan struct{}, 1),
		func() {},
		make(chan turn.InjectMessage, 1),
	)
	if err != nil {
		t.Fatalf("start runtime run: %v", err)
	}
	if _, err := manager.HandleAgentEvent(context.Background(), handle, native.StreamEvent{
		Type:        native.EventUserInputRequest,
		UserInputID: "input-provider-cancel",
		Status:      "pending",
	}); err != nil {
		t.Fatalf("park runtime decision: %v", err)
	}
	if err := manager.FinishRun(context.Background(), handle, "", ""); err != nil {
		t.Fatalf("mark deferred producer ready: %v", err)
	}

	service := &Service{decisionRuntime: manager}
	service.continueRuntimeDecision(context.Background(), sessionruntime.Command{
		BotID:      botID,
		SessionID:  sessionID,
		RunID:      runID,
		Generation: handle.Generation,
	}, func(chan<- WSStreamEvent) error {
		return context.Canceled
	})

	snapshot, err := manager.Snapshot(context.Background(), botID, sessionID)
	if err != nil {
		t.Fatalf("load terminal snapshot: %v", err)
	}
	if snapshot.CurrentRunView == nil || snapshot.CurrentRunView.Status != sessionruntime.RunStatusErrored {
		t.Fatalf("terminal run = %#v, want errored instead of parked", snapshot.CurrentRunView)
	}
}
