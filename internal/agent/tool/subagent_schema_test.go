package tools

import "context"

type goldenSubagentController struct{}

func (goldenSubagentController) Spawn(context.Context, SessionContext, SpawnAgentArgs) (any, error) {
	return nil, nil
}

func (goldenSubagentController) Send(context.Context, SessionContext, SendAgentMessageArgs) (any, error) {
	return nil, nil
}
func (goldenSubagentController) List(context.Context, SessionContext) (any, error)   { return nil, nil }
func (goldenSubagentController) Models(context.Context, SessionContext) (any, error) { return nil, nil }

func (goldenSubagentController) Backends(context.Context, SessionContext) (any, error) {
	return nil, nil
}

func (goldenSubagentController) Control(context.Context, SessionContext, SubagentControlArgs) (any, error) {
	return nil, nil
}
