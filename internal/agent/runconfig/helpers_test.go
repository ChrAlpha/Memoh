package runconfig

import (
	"log/slog"

	"github.com/felinics/memoh/internal/agent/botenv"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/memory"
	"github.com/felinics/memoh/internal/agent/transcript"
)

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
	if d.Bot == nil {
		d.Bot = botenv.New(botenv.Deps{Logger: logger})
	}
	if d.Transcript == nil {
		d.Transcript = transcript.New(transcript.Deps{Logger: logger})
	}
	if d.Memory == nil {
		d.Memory = memory.New(memory.Deps{Logger: logger, Bot: d.Bot, Transcript: d.Transcript})
	}
	if d.Compact == nil {
		d.Compact = compact.New(compact.Deps{Logger: logger, Transcript: d.Transcript})
	}
	if d.Decider == nil {
		d.Decider = decide.New(decide.Deps{Logger: logger})
	}
	return New(d)
}
