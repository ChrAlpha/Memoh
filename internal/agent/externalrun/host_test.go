package externalrun

import (
	"log/slog"
	"testing"

	"github.com/felinics/memoh/internal/agent/activeprompt"
	"github.com/felinics/memoh/internal/agent/botenv"
	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/memory"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/transcript"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/providers"
	"github.com/felinics/memoh/internal/settings"
)

// testHost builds a Host whose collaborators are the real components wired to
// whatever d names. A component d leaves nil is built from the rest.
func testHost(t *testing.T, d Deps) *Host {
	t.Helper()
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	bot := botenv.New(botenv.Deps{Logger: d.Logger})
	if d.Memory == nil {
		d.Memory = memory.New(memory.Deps{
			Logger: d.Logger, Messages: d.Messages, Bot: bot,
			Transcript: transcript.New(transcript.Deps{Logger: d.Logger, Messages: d.Messages, Sessions: d.Sessions}),
		})
	}
	if d.Persist == nil {
		d.Persist = persist.New(persist.Deps{Logger: d.Logger, Messages: d.Messages, Sessions: d.Sessions, Bot: bot, Rounds: d.Memory})
	}
	if d.ActivePrompts == nil {
		d.ActivePrompts = activeprompt.New()
	}
	if d.Decider == nil {
		d.Decider = decide.New(decide.Deps{Logger: d.Logger, Sessions: d.Sessions, BotPermissions: d.BotPermissions, ActivePrompts: d.ActivePrompts})
	}
	if d.Runconfig == nil {
		d.Runconfig = runconfig.New(runconfig.Deps{
			Logger: d.Logger, Credentials: providers.NewService(nil, nil, ""), Sessions: d.Sessions, Bot: bot,
			Memory: d.Memory, Decider: d.Decider, BotPermissions: d.BotPermissions,
		})
	}
	if d.Lifecycle == nil {
		d.Lifecycle = lifecycle.New(lifecycle.Deps{Decisions: d.Decider, Logger: d.Logger})
	}
	if d.Driver == nil {
		dd := drive.Deps{Logger: d.Logger, Lifecycle: d.Lifecycle, Persist: d.Persist}
		if runtime, ok := d.Admitter.(drive.Runtime); ok {
			dd.Runtime = runtime
		}
		d.Driver = drive.New(dd)
	}
	return New(d)
}

// memoryKeeper builds the keeper a registry of providers backs.
func memoryKeeper(d Deps, store *settings.Service, registry *memprovider.Registry) *memory.Keeper {
	bot := botenv.New(botenv.Deps{Logger: d.Logger})
	return memory.New(memory.Deps{
		Logger: d.Logger, Settings: store, Messages: d.Messages, Bot: bot, Registry: registry,
		Transcript: transcript.New(transcript.Deps{Logger: d.Logger, Messages: d.Messages}),
	})
}

// withRunconfig builds a Host whose run configuration builder has the
// dependencies adjust names.
func withRunconfig(t *testing.T, adjust func(*runconfig.Deps)) *Host {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	bot := botenv.New(botenv.Deps{Logger: logger})
	d := runconfig.Deps{Logger: logger, Credentials: providers.NewService(nil, nil, ""), Bot: bot}
	adjust(&d)
	return testHost(t, Deps{Logger: logger, Runconfig: runconfig.New(d)})
}
