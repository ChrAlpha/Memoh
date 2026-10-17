// Package drive executes one admitted run from the moment it is admitted to the
// moment its terminal state is written: it consumes the runtime's events,
// publishes them once, hands them to the caller's delivery end, and writes the
// history and the terminal state in a fixed order. It knows nothing about how
// a run's context was assembled, compacted or decided; an executor and a plan
// arrive from the entry that started the run.
package drive

import (
	"context"
	"log/slog"
	"time"

	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	chatview "github.com/felinics/memoh/internal/agent/view"
)

// Runtime is the durable run ledger the terminal write goes through.
type Runtime interface {
	FinishRunWithFailure(ctx context.Context, handle sessionruntime.RunHandle, status, errorCode string, errorArgs map[string]string) (sessionruntime.TerminalRun, error)
}

// EventSink is the session runtime's intake for the events of a run it owns:
// the live projection every subscriber reads.
type EventSink interface {
	HandleAgentEvent(ctx context.Context, handle sessionruntime.RunHandle, ev native.StreamEvent) ([]chatview.UIMessage, error)
}

// Deps is everything New needs.
type Deps struct {
	// Logger receives the driver's own records.
	Logger *slog.Logger
	// Runtime writes the terminal state of a run.
	Runtime Runtime
	// Events receives the events of a run; without it a run publishes nothing.
	Events EventSink
	// Lifecycle records how the context of a run ended.
	Lifecycle *lifecycle.Recorder
	// Persist writes the history a run's ending owns.
	Persist *persist.Writer
	// OutputMedia stores attachments that runs emit; without it an attachment
	// keeps the path it arrived with.
	OutputMedia OutputMediaStore
	// OutputSpeech synthesizes spoken replies; without it a speech event is
	// dropped.
	OutputSpeech OutputSpeech
	// StreamIdleTimeout is the model's silence window; zero keeps the default.
	StreamIdleTimeout time.Duration
	// StreamIdleTimeoutMax caps the window the reasoning effort scales it to;
	// zero keeps the default.
	StreamIdleTimeoutMax time.Duration
}

// Driver runs admitted runs.
type Driver struct {
	logger               *slog.Logger
	runtime              Runtime
	events               EventSink
	lifecycle            *lifecycle.Recorder
	persist              *persist.Writer
	outputMedia          OutputMediaStore
	outputSpeech         OutputSpeech
	streamIdleTimeout    time.Duration
	streamIdleTimeoutMax time.Duration
}

// New returns a Driver. A missing logger discards the driver's records, and a
// missing event sink drops the events a run publishes.
func New(d Deps) *Driver {
	driver := &Driver{
		logger:               d.Logger,
		runtime:              d.Runtime,
		events:               d.Events,
		lifecycle:            d.Lifecycle,
		persist:              d.Persist,
		outputMedia:          d.OutputMedia,
		outputSpeech:         d.OutputSpeech,
		streamIdleTimeout:    d.StreamIdleTimeout,
		streamIdleTimeoutMax: d.StreamIdleTimeoutMax,
	}
	if driver.logger == nil {
		driver.logger = slog.New(slog.DiscardHandler)
	}
	if driver.events == nil {
		driver.events = noEvents{}
	}
	return driver
}

type noEvents struct{}

func (noEvents) HandleAgentEvent(context.Context, sessionruntime.RunHandle, native.StreamEvent) ([]chatview.UIMessage, error) {
	return nil, nil
}
