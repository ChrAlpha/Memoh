package sessionmode

import "strings"

const (
	Chat     = "chat"
	Schedule = "schedule"
	Subagent = "subagent"
	Discuss  = "discuss"
	ACPAgent = "acp_agent"
)

// IsInteractive reports whether someone waits on the outcome of a run admitted
// in this mode, which decides how the run's result is recorded. Whether a run
// may park on a decision is a separate fact, sessionfacts.DecisionMode.
func IsInteractive(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", Chat, ACPAgent:
		return true
	default:
		return false
	}
}
