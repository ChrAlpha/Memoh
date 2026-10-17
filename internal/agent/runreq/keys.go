package runreq

import (
	"strings"

	"github.com/google/uuid"
)

// RunID is the run identity of a request: the one admission minted, or a new UUID when none exists.
func RunID(admittedRunID string) string {
	if runID := strings.TrimSpace(admittedRunID); runID != "" {
		return runID
	}
	return uuid.NewString()
}

// SessionTurnKey names the per-session scope of process-local bookkeeping that
// only makes sense inside one process: a compaction barrier and the ACP prompt
// hubs. It is deliberately not an admission gate — single-session execution is
// enforced durably by the session runtime, which is the only thing that can
// answer for owners in other processes.
func SessionTurnKey(botID, sessionID string) string {
	return strings.TrimSpace(botID) + ":" + strings.TrimSpace(sessionID)
}
