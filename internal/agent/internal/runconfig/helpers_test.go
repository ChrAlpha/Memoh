package runconfig

import (
	"log/slog"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/compact"
	"github.com/felinics/memoh/internal/agent/internal/decide"
	"github.com/felinics/memoh/internal/agent/internal/memory"
	"github.com/felinics/memoh/internal/agent/internal/transcript"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// discardLogger is the logger of tests that call package functions directly.
var discardLogger = slog.New(slog.DiscardHandler)

// newBuilder returns a Builder over collaborators built on nothing, which
// answer as an unconfigured deployment does. A test names only what it reads:
// adjust changes the dependencies before the components that copy them are
// built.
func newBuilder(adjust ...func(*Deps)) *Builder {
	logger := slog.New(slog.DiscardHandler)
	d := Deps{Logger: logger}
	for _, a := range adjust {
		a(&d)
	}
	if d.Agent == nil {
		d.Agent = &native.Agent{}
	}
	if d.Bot == nil {
		d.Bot = agenttest.BotEnv(logger)
	}
	if d.Transcript == nil {
		d.Transcript = transcript.New(transcript.Deps{Logger: logger})
	}
	if d.Memory == nil {
		d.Memory = memory.New(memory.Deps{Logger: logger, Bot: d.Bot, Transcript: d.Transcript})
	}
	if d.MemoryOverview == nil {
		d.MemoryOverview = memory.NewOverview(memory.OverviewDeps{Logger: logger, Keeper: d.Memory})
	}
	if d.Compact == nil {
		d.Compact = compact.New(compact.Deps{Logger: logger, Transcript: d.Transcript})
	}
	if d.Decider == nil {
		d.Decider = decide.New(decide.Deps{Logger: logger})
	}
	return New(d)
}
