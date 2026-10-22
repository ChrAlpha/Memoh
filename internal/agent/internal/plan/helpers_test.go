package plan

import (
	"log/slog"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/compact"
	"github.com/felinics/memoh/internal/agent/internal/decide"
	"github.com/felinics/memoh/internal/agent/internal/memory"
	"github.com/felinics/memoh/internal/agent/internal/runconfig"
	"github.com/felinics/memoh/internal/agent/internal/transcript"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// newTestBuilder is a run configuration builder over collaborators built on
// nothing, which answer as an unconfigured deployment does.
func newTestBuilder() *runconfig.Builder {
	logger := slog.New(slog.DiscardHandler)
	bot := agenttest.BotEnv(logger)
	reader := transcript.New(transcript.Deps{Logger: logger})
	keeper := memory.New(memory.Deps{Logger: logger, Bot: bot, Transcript: reader})
	return runconfig.New(runconfig.Deps{
		Logger:         logger,
		Agent:          &native.Agent{},
		Bot:            bot,
		Transcript:     reader,
		Memory:         keeper,
		MemoryOverview: memory.NewOverview(memory.OverviewDeps{Logger: logger, Keeper: keeper}),
		Compact:        compact.New(compact.Deps{Logger: logger, Transcript: reader}),
		Decider:        decide.New(decide.Deps{Logger: logger}),
	})
}
