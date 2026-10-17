package runreq

import (
	"errors"
)

// ErrResumeUnrecoverable marks an interrupted intent that no later scan can
// continue. The worker retires it instead of retrying it every pass; failures
// that may clear (workspace readiness, database errors) stay unwrapped.
var ErrResumeUnrecoverable = errors.New("interrupted session cannot resume")
