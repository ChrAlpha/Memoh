// Package answer takes the answer to a parked decision (a tool approval, a
// question to the user) and resumes the run that parked on it. It routes the
// answer to the session runtime that owns the run, commits it through decide,
// and drives the continuation the plan builds, either as the run the answer
// was routed to or, when nothing routed it, as one driven here for the caller.
package answer

import (
	"context"
	"log/slog"

	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/transcript"
)

// Continuer plans the run that continues from an answered decision.
type Continuer interface {
	Continue(ctx context.Context, call decide.DecidedCall, req runreq.ChatRequest, live bool) (drive.RunPlan, error)
}

// Deps is everything New needs. Every field is required.
type Deps struct {
	// Logger receives the component's own records.
	Logger *slog.Logger
	// Decider commits an answer and describes the call it decided.
	Decider *decide.Decider
	// Runtime routes an answer to the run's owner and publishes its output.
	Runtime *sessionruntime.Manager
	// Driver executes the continuation.
	Driver *drive.Driver
	// Transcript finds the request row the continuation answers.
	Transcript *transcript.Reader
	// Continuer plans the continuation.
	Continuer Continuer
}

// Responder resumes the runs parked on decisions.
type Responder struct {
	logger     *slog.Logger
	decider    *decide.Decider
	runtime    *sessionruntime.Manager
	driver     *drive.Driver
	transcript *transcript.Reader
	continuer  Continuer
}

// New returns a Responder.
func New(d Deps) *Responder {
	return &Responder{
		logger:     d.Logger,
		decider:    d.Decider,
		runtime:    d.Runtime,
		driver:     d.Driver,
		transcript: d.Transcript,
		continuer:  d.Continuer,
	}
}
