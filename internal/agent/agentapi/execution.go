package agentapi

// SessionExecutionInfo says whether a session executes in the workspace and who
// owns its runtime.
type SessionExecutionInfo struct {
	RequiresWorkspaceExec bool
	BotID                 string
	RuntimeOwnerAccountID string
}
