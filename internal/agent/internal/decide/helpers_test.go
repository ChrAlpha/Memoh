package decide

import (
	"context"
	"log/slog"

	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/view"
)

// newTestDecider is a Decider over d. What d leaves out is an explicit double
// that reads nothing and decides nothing, so a test names only the
// collaborators its behavior goes through.
func newTestDecider(d Deps) *Decider {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Queries == nil {
		d.Queries = &agenttest.DecisionQueries{}
	}
	if d.Sessions == nil {
		d.Sessions = agenttest.NewChatSessions()
	}
	if d.ToolApproval == nil {
		d.ToolApproval = &toolapproval.Service{}
	}
	if d.UserInput == nil {
		d.UserInput = &agenttest.UserInput{}
	}
	if d.BotPermissions == nil {
		d.BotPermissions = &agenttest.PermissionChecker{}
	}
	if d.Events == nil {
		d.Events = discardEvents{}
	}
	if d.Limits == nil {
		d.Limits = &native.Agent{}
	}
	return New(d)
}

type discardEvents struct{}

func (discardEvents) HandleAgentEvent(context.Context, sessionruntime.RunHandle, native.StreamEvent) ([]view.UIMessage, error) {
	return nil, nil
}
