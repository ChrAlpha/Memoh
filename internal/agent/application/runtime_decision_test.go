package application

import (
	"context"
	"errors"
	"testing"

	sessionruntime "github.com/memohai/memoh/internal/agent/runtime/session"
	"github.com/memohai/memoh/internal/apperror"
)

func TestRuntimeDecisionTerminalDoesNotExposePrivateErrors(t *testing.T) {
	tests := []struct {
		name    string
		cause   error
		status  string
		message string
	}{
		{name: "success"},
		{name: "canceled", cause: context.Canceled},
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
			status, message := runtimeDecisionTerminal(tt.cause)
			if status != tt.status || message != tt.message {
				t.Fatalf("runtimeDecisionTerminal() = (%q, %q), want (%q, %q)", status, message, tt.status, tt.message)
			}
		})
	}
}
