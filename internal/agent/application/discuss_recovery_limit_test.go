package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/turn"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
)

func TestDiscussIrreducibleContextDoesNotWaitForCompaction(t *testing.T) {
	irreducible := []turn.DiscussMessage{
		{Role: "user", Content: strings.Repeat("s", 16000), CompactionArtifactID: "summary"},
		{Role: "user", Content: strings.Repeat("n", 52000), Source: &turn.ContextMessageSource{Kind: "external", ID: "newest", Current: true}},
	}
	_, agentAdmission := admitDiscussAgentMessages(irreducible, 16000)
	for _, tc := range []struct {
		name   string
		setup  func(*Service, *fakeDiscussService, *turn.StartTurnCommand)
		window int
	}{
		{name: "provider"},
		{name: "channel", window: 16000 - 1200, setup: func(_ *Service, _ *fakeDiscussService, cmd *turn.StartTurnCommand) {
			cmd.DiscussContextOverflow, cmd.DiscussContextTokens, cmd.DiscussCurrentTokens = true, 20000, 1200
		}},
		{name: "admission", window: 16000 - 13000, setup: func(_ *Service, _ *fakeDiscussService, cmd *turn.StartTurnCommand) {
			cmd.DiscussMessages = irreducible
		}},
		{name: "agent", window: agentAdmission.HistoryBudgetTokens, setup: func(service *Service, resolver *fakeDiscussService, cmd *turn.StartTurnCommand) {
			service.contextAbsoluteCapTokens = 16000
			resolver.resolveResult.RuntimeType = sessionpkg.RuntimeACPAgent
			cmd.DiscussAddressed, cmd.DiscussMessages = true, irreducible
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, resolver, provider, runner, cmd := discussBudgetRecoveryFixture(t)
			configureDiscussLifecycle(service)
			if tc.setup != nil {
				tc.setup(service, resolver, &cmd)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			handle, err := service.StartTurn(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			for range handle.Events() {
			}
			failed := false
			for err := range handle.Errs() {
				failed = failed || err != nil
			}
			if !failed || ctx.Err() != nil || provider.callCount() != 0 {
				t.Fatalf("failed=%v timeout=%v provider=%d", failed, ctx.Err(), provider.callCount())
			}
			window := runner.awaitPass(t).ContextWindowTokens
			if tc.window == 0 && (window <= 0 || window >= 16000) || tc.window != 0 && window != tc.window {
				t.Fatalf("pass window=%d, want the history allowance beside the protected input (%d)", window, tc.window)
			}
		})
	}
}
