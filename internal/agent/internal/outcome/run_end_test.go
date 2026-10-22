package outcome

import (
	"context"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
)

// The failure policy: the outcome each ending names, and whether that ending
// leaves a failure row. One table, read by every entry.
func TestNameRunEndTable(t *testing.T) {
	t.Parallel()

	terminalCompleted := native.StreamEvent{Type: native.EventAgentEnd}
	terminalAbort := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndCanceled}
	terminalLoop := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndLoopDetected}
	terminalFailed := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndFailed}
	providerErr := apperror.Wrap(apperror.CodeAgentProviderAuthFailed, errors.New("401"), nil)

	cases := []struct {
		name       string
		facts      EndingFacts
		wantEnd    RunEnd
		wantCode   apperror.Code
		input      InputOwnership
		hook       bool
		replacing  bool
		wantRow    bool
		stoppedCtx bool
	}{
		{
			name:    "clean terminal completes",
			facts:   EndingFacts{Phase: PhaseExecution, Terminal: &terminalCompleted},
			wantEnd: EndCompleted,
			input:   InputAccepted,
			wantRow: false,
		},
		{
			name:    "deferred parks",
			facts:   EndingFacts{Phase: PhaseExecution, Terminal: &terminalCompleted, Deferred: true},
			wantEnd: EndParked,
			input:   InputAccepted,
			wantRow: false,
		},
		{
			name:       "stopped before the first failure",
			facts:      EndingFacts{Phase: PhaseExecution, Terminal: &terminalAbort, Declared: providerErr, StoppedAtDeclare: true},
			wantEnd:    EndStopped,
			input:      InputAccepted,
			wantRow:    false,
			stoppedCtx: true,
		},
		{
			name:       "failed before the stop keeps its name",
			facts:      EndingFacts{Phase: PhaseExecution, Terminal: &terminalAbort, Declared: providerErr, StoppedAtDeclare: false},
			wantEnd:    EndFailed,
			wantCode:   apperror.CodeAgentProviderAuthFailed,
			input:      InputAccepted,
			wantRow:    true,
			stoppedCtx: true,
		},
		{
			name:     "loop guard has its own code",
			facts:    EndingFacts{Phase: PhaseExecution, Terminal: &terminalLoop},
			wantEnd:  EndFailed,
			wantCode: apperror.CodeAgentLoopDetected,
			input:    InputAccepted,
			wantRow:  true,
		},
		{
			name:     "abort without a reason fails unnamed",
			facts:    EndingFacts{Phase: PhaseExecution, Terminal: &terminalFailed},
			wantEnd:  EndFailed,
			wantCode: apperror.CodeRuntimeRunFailed,
			input:    InputAccepted,
			wantRow:  true,
		},
		{
			name:     "no terminal and nothing declared",
			facts:    EndingFacts{Phase: PhaseExecution},
			wantEnd:  EndFailed,
			wantCode: apperror.CodeRuntimeRunFailed,
			input:    InputAccepted,
			wantRow:  true,
		},
		{
			name:     "hook refusal leaves a row for a held input",
			facts:    EndingFacts{Phase: PhaseSetup, Declared: errors.New("hook denied")},
			wantEnd:  EndFailed,
			wantCode: apperror.CodeHookUserMessageFailed,
			input:    InputHeld,
			hook:     true,
			wantRow:  true,
		},
		{
			name:     "setup failure hands a held input back",
			facts:    EndingFacts{Phase: PhaseSetup, Declared: errors.New("resolve failed")},
			wantEnd:  EndFailed,
			wantCode: apperror.CodeRuntimeRunFailed,
			input:    InputHeld,
			wantRow:  false,
		},
		{
			name:     "setup failure keeps an accepted input",
			facts:    EndingFacts{Phase: PhaseSetup, Declared: errors.New("resolve failed")},
			wantEnd:  EndFailed,
			wantCode: apperror.CodeRuntimeRunFailed,
			input:    InputAccepted,
			wantRow:  true,
		},
		{
			name:     "synthetic input never writes history",
			facts:    EndingFacts{Phase: PhaseExecution, Declared: providerErr},
			wantEnd:  EndFailed,
			wantCode: apperror.CodeAgentProviderAuthFailed,
			input:    InputSynthetic,
			wantRow:  false,
		},
		{
			name:      "a replacement turn records a named failure",
			facts:     EndingFacts{Phase: PhaseExecution, Declared: providerErr},
			wantEnd:   EndFailed,
			wantCode:  apperror.CodeAgentProviderAuthFailed,
			input:     InputAccepted,
			replacing: true,
			wantRow:   true,
		},
		{
			name:      "a replacement turn records an unnamed failure",
			facts:     EndingFacts{Phase: PhaseExecution, Declared: errors.New("boom")},
			wantEnd:   EndFailed,
			wantCode:  apperror.CodeRuntimeRunFailed,
			input:     InputAccepted,
			replacing: true,
			wantRow:   true,
		},
		{
			name:      "a replacement turn that failed in setup leaves no row",
			facts:     EndingFacts{Phase: PhaseSetup, Declared: errors.New("target is no longer the latest turn")},
			wantEnd:   EndFailed,
			wantCode:  apperror.CodeRuntimeRunFailed,
			input:     InputAccepted,
			replacing: true,
			wantRow:   false,
		},
		{
			name:     "a delivered failure keeps its code when the history write also fails",
			facts:    EndingFacts{Phase: PhaseExecution, Terminal: &terminalFailed, Declared: providerErr, Commit: errors.New("write failed")},
			wantEnd:  EndFailed,
			wantCode: apperror.CodeAgentProviderAuthFailed,
			input:    InputAccepted,
			wantRow:  true,
		},
		{
			name:     "a failed history write ends a parked run",
			facts:    EndingFacts{Phase: PhaseCommit, Terminal: &terminalCompleted, Deferred: true, Commit: errors.New("write failed")},
			wantEnd:  EndFailed,
			wantCode: apperror.CodeSessionHistoryInconsistent,
			input:    InputAccepted,
			wantRow:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tc.stoppedCtx {
				var cancel context.CancelCauseFunc
				ctx, cancel = context.WithCancelCause(context.Background())
				cancel(ErrRunStopped)
				defer cancel(nil)
			}
			facts := tc.facts
			facts.RunCtx = ctx
			facts.Hook = tc.hook
			end, failure := NameRunEnd(facts)
			if end != tc.wantEnd {
				t.Fatalf("end = %s, want %s", end, tc.wantEnd)
			}
			if tc.wantCode != "" {
				if failure == nil {
					t.Fatalf("failure = nil, want %s", tc.wantCode)
				}
				if failure.Code != tc.wantCode {
					t.Fatalf("code = %q, want %q", failure.Code, tc.wantCode)
				}
			}
			row := WantsFailureRow(tc.input, end, failure, tc.hook, tc.replacing)
			if row != tc.wantRow {
				t.Fatalf("wantsFailureRow = %v, want %v", row, tc.wantRow)
			}
		})
	}
}

// WantsFailureRow over every ownership and phase combination, so a table that
// grew a case cannot silently change another.
func TestWantsFailureRowTable(t *testing.T) {
	t.Parallel()

	phases := []RunPhase{PhaseSetup, PhaseExecution, PhaseCommit, PhasePublish}
	owned := []InputOwnership{InputHeld, InputAccepted, InputSynthetic}

	for _, o := range owned {
		for _, phase := range phases {
			for _, replacing := range []bool{false, true} {
				f := &RunFailure{Code: apperror.CodeAgentProviderAuthFailed, Phase: phase}
				got := WantsFailureRow(o, EndFailed, f, false, replacing)
				want := expectedRow(o, phase, replacing)
				if got != want {
					t.Fatalf("ownership %d phase %d replacing %v: wantsFailureRow = %v, want %v",
						o, phase, replacing, got, want)
				}
			}
		}
	}
	// Only a failure leaves a row.
	if WantsFailureRow(InputAccepted, EndCompleted, nil, false, false) {
		t.Fatal("a completed run left a failure row")
	}
	if WantsFailureRow(InputAccepted, EndParked, nil, false, false) {
		t.Fatal("a parked run left a failure row")
	}
}

// expectedRow states the rule independently of the implementation.
func expectedRow(o InputOwnership, phase RunPhase, replacing bool) bool {
	switch o {
	case InputSynthetic:
		return false
	case InputHeld:
		if replacing {
			return false
		}
		return phase == PhaseExecution || phase == PhasePublish
	default:
		if replacing {
			return phase == PhaseExecution
		}
		return phase != PhaseCommit
	}
}
