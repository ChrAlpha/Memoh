package outcome

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/felinics/memoh/internal/apperror"
)

func TestClassifyRunFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		cause error
		want  apperror.Code
	}{
		{name: "catalogued code", cause: apperror.New(apperror.CodeAgentProviderOverloaded, nil), want: apperror.CodeAgentProviderOverloaded},
		{name: "catalogued code under a wrap", cause: fmt.Errorf("stream: %w", apperror.New(apperror.CodeAgentResponseTimeout, nil)), want: apperror.CodeAgentResponseTimeout},
		{name: "code outside the catalog", cause: apperror.New("not.catalogued", nil), want: apperror.CodeRuntimeRunFailed},
		{name: "plain error", cause: errors.New("connection reset"), want: apperror.CodeRuntimeRunFailed},
		{name: "cancellation", cause: context.Canceled, want: apperror.CodeRuntimeRunFailed},
		{name: "nil", cause: nil, want: apperror.CodeRuntimeRunFailed},
		{name: "external agent code", cause: fmt.Errorf("prompt: %w", apperror.New(apperror.CodeACPRuntimeBusy, nil)), want: apperror.CodeACPRuntimeBusy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyRunFailure(tc.cause); got != tc.want {
				t.Fatalf("classifyRunFailure() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHistoryFailureCodeMarksOnlyNamedFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		cause error
		want  apperror.Code
	}{
		{name: "listed code", cause: apperror.New(apperror.CodeAgentProviderOverloaded, nil), want: apperror.CodeAgentProviderOverloaded},
		{name: "unlisted code", cause: apperror.New(apperror.CodeContextProtectedOverflow, nil), want: ""},
		{name: "user stop", cause: context.Canceled, want: ""},
		{name: "unnamed failure", cause: errors.New("agent stream ended without a terminal event"), want: ""},
		{name: "tool timeout", cause: apperror.New(apperror.CodeAgentToolTimeout, nil), want: apperror.CodeAgentToolTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := HistoryFailureCode(PublicFailureCode(tc.cause)); got != tc.want {
				t.Fatalf("historyFailureCode() = %q, want %q", got, tc.want)
			}
		})
	}
}
