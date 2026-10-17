package outcome

import (
	"errors"
)

// ErrResponseTimeout and ErrToolTimeout are the idle watchdog's own stop
// markers. They name which window expired; the code the run reports is taken
// from them rather than from the deadline they wrap, so a timeout is reported
// as a timeout and not as a cancellation.
var (
	ErrResponseTimeout = errors.New("agent response idle timeout")
	ErrToolTimeout     = errors.New("agent tool idle timeout")
)
