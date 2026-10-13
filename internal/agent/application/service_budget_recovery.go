package application

import (
	"context"

	"github.com/felinics/memoh/internal/agent/runtime/native"
)

func pauseIdleDuringBudgetRecovery(cfg native.RunConfig, idle *idleCancel) native.RunConfig {
	if recoverBudget := cfg.RecoverContextBudget; recoverBudget != nil {
		cfg.RecoverContextBudget = func(ctx context.Context, cfg native.RunConfig) (native.RunConfig, bool, error) {
			idle.Pause()
			defer idle.Resume()
			return recoverBudget(ctx, cfg)
		}
	}
	return cfg
}
