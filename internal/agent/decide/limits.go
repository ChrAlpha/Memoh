package decide

import (
	contextlimit "github.com/felinics/memoh/internal/agent/context/limit"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

func (d *Decider) ToolOutputLimit() contextlimit.ToolOutputLimit {
	return d.limits.Limits().ToolOutputLimit()
}

func (d *Decider) LimitToolResultText(text, toolName string) string {
	limit := d.ToolOutputLimit()
	return contextlimit.LimitString(text, "tool result ("+toolName+")", limit)
}

func (d *Decider) LimitToolResultValue(value any, toolName string) any {
	return contextlimit.LimitToolOutput(value, "tool result ("+toolName+")", d.ToolOutputLimit())
}

func (d *Decider) limitToolApprovalResult(result toolexec.ToolApprovalResult, toolName string) toolexec.ToolApprovalResult {
	if result.Decision == toolexec.ToolApprovalDecisionRejected {
		result.Reason = d.LimitToolResultText(result.Reason, toolName)
	}
	return result
}
