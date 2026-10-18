package drive_test

import (
	"log/slog"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
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
	idle     time.Duration
}

type driverOption func(*driverConfig)

func withMessages(messages messagepkg.Service) driverOption {
	return func(c *driverConfig) { c.messages = messages }
}

func withOutput(media drive.OutputMediaStore, speech drive.OutputSpeech) driverOption {
	return func(c *driverConfig) { c.media, c.speech = media, speech }
}

// withIdleWindow sets the model's silence window for every effort level.
func withIdleWindow(window time.Duration) driverOption {
	return func(c *driverConfig) { c.idle = window }
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
	if cfg.events == nil {
		cfg.events = &agenttest.EventRecorder{}
	}
	if cfg.messages == nil {
		cfg.messages = &agenttest.RecordingMessages{}
	}
	if admitter == nil {
		admitter = &agenttest.ScriptedAdmitter{}
	}
	return drive.New(drive.Deps{
		Logger:               logger,
		Runtime:              admitter,
		Events:               cfg.events,
		OutputMedia:          cfg.media,
		OutputSpeech:         cfg.speech,
		StreamIdleTimeout:    cfg.idle,
		StreamIdleTimeoutMax: cfg.idle * 100,
		Lifecycle: lifecycle.New(lifecycle.Deps{
			Store: &agenttest.RecordingLifecycleStore{}, Runs: agenttest.NoRuns{}, Decisions: agenttest.NoDecisions{}, Logger: logger,
		}),
		Persist: persist.New(persist.Deps{
			Logger:   logger,
			Messages: cfg.messages,
			Sessions: &agenttest.Sessions{},
			Bot:      agenttest.BotEnv(logger),
			Runtime:  steerRuntimeUnused{},
		}),
	})
}

var _ runreq.TurnAdmitter = (*agenttest.ScriptedAdmitter)(nil)

const drivePrompt = "answer the question"

var _ = time.Second

// steerRuntimeUnused is the steer queue of a driver whose runs never steer;
// any call into it panics.
type steerRuntimeUnused struct{ persist.SteerRuntime }
