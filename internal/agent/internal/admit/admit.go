// Package admit turns what a caller wants run into an admitted run: the
// durable slot on a session, the context the run executes in, and the record of
// what is needed to run it again after a restart. It also signs the credentials
// of runs the server starts itself.
package admit

import (
	"context"

	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// Queries is the part of the database admission writes: the intent a run is
// resumed from.
type Queries interface {
	SaveSessionRunResumeContext(ctx context.Context, arg sqlc.SaveSessionRunResumeContextParams) (int64, error)
}

// Deps is everything New needs.
type Deps struct {
	// Runtime is the durable admission gate. Every run goes through it.
	Runtime runreq.TurnAdmitter
	// Queries stores the resume context of a run.
	Queries Queries
	// Signer signs and reads the credentials a resume context is built from.
	Signer *Signer
}

// Admitter admits runs.
type Admitter struct {
	runtime runreq.TurnAdmitter
	queries Queries
	signer  *Signer
}

// New returns an Admitter.
func New(d Deps) *Admitter {
	return &Admitter{runtime: d.Runtime, queries: d.Queries, signer: d.Signer}
}
