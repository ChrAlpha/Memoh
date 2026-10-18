package plan

import (
	"log/slog"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/memory"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/transcript"
)

// newTestBuilder is a run configuration builder over collaborators built on
// nothing, which answer as an unconfigured deployment does.
func newTestBuilder() *runconfig.Builder {
	logger := slog.New(slog.DiscardHandler)
	bot := agenttest.BotEnv(logger)
	reader := transcript.New(transcript.Deps{Logger: logger})
	return runconfig.New(runconfig.Deps{
		Logger:     logger,
		Agent:      &native.Agent{},
		Bot:        bot,
		Transcript: reader,
		Memory:     memory.New(memory.Deps{Logger: logger, Bot: bot, Transcript: reader}),
		Compact:    compact.New(compact.Deps{Logger: logger, Transcript: reader}),
		Decider:    decide.New(decide.Deps{Logger: logger}),
	})
}
