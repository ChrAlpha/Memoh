package application

import (
	"context"
	"testing"
	"time"
)

func TestDiscussIrreducibleContextDoesNotWaitForCompaction(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(map[bool]string{false: "provider", true: "channel"}[overflow], func(t *testing.T) {
			service, _, provider, _, cmd := discussBudgetRecoveryFixture(t)
			configureDiscussLifecycle(service)
			runner := newGatedCompactionRunner()
			defer close(runner.release)
			service.compactionService = runner
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			cmd.DiscussContextOverflow = overflow
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
		})
	}
}
