package drive_test

import (
	"log/slog"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/botenv"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type driverConfig struct {
	events   drive.EventSink
	messages messagepkg.Service
	media    drive.OutputMediaStore
	speech   drive.OutputSpeech
}

type driverOption func(*driverConfig)

func withMessages(messages messagepkg.Service) driverOption {
	return func(c *driverConfig) { c.messages = messages }
}

func withOutput(media drive.OutputMediaStore, speech drive.OutputSpeech) driverOption {
	return func(c *driverConfig) { c.media, c.speech = media, speech }
}

func withEvents(events drive.EventSink) driverOption {
	return func(c *driverConfig) { c.events = events }
}

// newDriveService is a Driver over stand-ins: pub records the events it
// publishes, admitter records and answers the terminal write. Either may be
// nil; without a publisher the driver publishes nowhere.
func newDriveService(pub *agenttest.EventRecorder, admitter *agenttest.ScriptedAdmitter, opts ...driverOption) *drive.Driver {
	var cfg driverConfig
	if pub != nil {
		cfg.events = pub
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	logger := slog.New(slog.DiscardHandler)
	deps := drive.Deps{
		Logger:       logger,
		Events:       cfg.events,
		OutputMedia:  cfg.media,
		OutputSpeech: cfg.speech,
		Lifecycle:    lifecycle.New(lifecycle.Deps{Logger: logger}),
		Persist: persist.New(persist.Deps{
			Logger:   logger,
			Messages: cfg.messages,
			Bot:      botenv.New(botenv.Deps{Logger: logger}),
		}),
	}
	if admitter != nil {
		deps.Runtime = admitter
	}
	return drive.New(deps)
}

var _ runreq.TurnAdmitter = (*agenttest.ScriptedAdmitter)(nil)

const drivePrompt = "answer the question"

var _ = time.Second
