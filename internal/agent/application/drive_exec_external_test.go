package application

import (
	"context"
	"testing"

	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// A scheduled run has nobody to answer a decision, but an interactive External
// Agent turn does: the flag says whether the run can deliver a question, and a
// run that keeps it false never offers the runtime its ask_user tools.
func TestExternalPromptInputCarriesWhetherTheRunCanAskTheUser(t *testing.T) {
	t.Parallel()

	newExecutor := func(unattended bool, hasInputPort bool) *externalExecutor {
		svc := &Service{}
		if hasInputPort {
			svc.userInput = &fakeUserInputService{}
		}
		return &externalExecutor{service: svc, unattended: unattended}
	}

	cases := []struct {
		name         string
		unattended   bool
		hasInputPort bool
		want         bool
	}{
		{name: "interactive with a port", unattended: false, hasInputPort: true, want: true},
		{name: "interactive without a port", unattended: false, hasInputPort: false, want: false},
		{name: "unattended with a port", unattended: true, hasInputPort: true, want: false},
		{name: "unattended without a port", unattended: true, hasInputPort: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exec := newExecutor(tc.unattended, tc.hasInputPort)
			got := exec.promptInput(context.Background(), func(native.StreamEvent) {}).CanRequestUserInput
			if got != tc.want {
				t.Fatalf("CanRequestUserInput = %v, want %v", got, tc.want)
			}
		})
	}
}

var _ = external.PromptInput{}
