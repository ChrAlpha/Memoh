package tools

import (
	"context"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/toolexec"
)

// SubagentController owns managed child tasks. Tool definitions contain no
// runtime selection, scheduling, persistence, or execution policy.
type SubagentController interface {
	Backends(context.Context, SessionContext) (any, error)
	Control(context.Context, SessionContext, SubagentControlArgs) (any, error)
	Spawn(context.Context, SessionContext, SpawnAgentArgs) (any, error)
	Send(context.Context, SessionContext, SendAgentMessageArgs) (any, error)
	List(context.Context, SessionContext) (any, error)
	Models(context.Context, SessionContext) (any, error)
}

type SpawnProvider struct{ controller SubagentController }

func NewSpawnProvider() *SpawnProvider                      { return &SpawnProvider{} }
func (p *SpawnProvider) SetController(c SubagentController) { p.controller = c }

// Usage frames how the available agent-control tools are meant to be used.
func (*SpawnProvider) Usage(_ context.Context, _ SessionContext, available AvailableTools) string {
	var parts []string
	canStartBackground := false
	if spawnRef, ok := available.Ref(ToolSpawnAgent()); ok {
		canStartBackground = true
		parts = append(parts,
			"Use "+spawnRef+" to create a managed subagent for an independent task.",
			"Subagents can use the bot's configured tools, including workspace, web, memory, skills, browser, media, and MCP tools. They cannot ask the user, send direct chat messages or reactions, or create more subagents.",
			"Native subagents use a model from the current session provider. `bot_agent_id` selects a configured external instance with its own model and context; it cannot be combined with `model_id`, `provider`, or `fork`.",
			"Set `fork: true` when the worker needs the parent model's current message context; otherwise it starts with only the assigned task.",
			"Use subagents when work benefits from isolated context or can proceed while you continue. Don't use one for simple single-step work — just do it directly.",
		)
	}
	if ref, ok := available.Ref(ToolListModels()); ok {
		parts = append(parts, ref+" lists enabled chat models, provider names, and descriptions. Only models from the current session's provider are available to subagents.")
	}
	if ref, ok := available.Ref(ToolSendMessage()); ok {
		canStartBackground = true
		parts = append(parts, "Use "+ref+" to continue an existing agent with a follow-up.")
	}
	if backgroundTools := available.Refs(ToolWaitUntil(), ToolGetBackgroundStatus(), ToolListBackground(), ToolKillBackground()); len(backgroundTools) > 0 {
		if canStartBackground {
			parts = append(parts, "For long work, set `run_in_background: true`. The call returns a task ID immediately. Use `wait_until(task_id)`, then `get_background_status(task_id)` to read `result`.")
		}
		parts = append(parts, "Manage running agent tasks with "+joinRefs(backgroundTools, "and")+".")
	}
	if ref, ok := available.Ref(ToolListAgentBackends()); ok {
		parts = append(parts, ref+" lists external instances and capabilities. External children accept one task; provide the task and material references explicitly.")
	}
	if ref, ok := available.Ref(ToolControlAgent()); ok {
		parts = append(parts, ref+" reads durable status and a bounded result report, or stops the exact child run. Use it after a restart when the background task ID is unavailable. An unknown execution retains its slot until stop reconciliation confirms it ended.")
	}
	if ref, ok := available.Ref(ToolListAgents()); ok {
		parts = append(parts, "Use "+ref+" to see agents created in the current session.")
	}
	if ref, ok := available.Ref(ToolSearchMessages()); ok {
		parts = append(parts, "Read a finished task's full transcript with "+ref+" using the session ID returned by get_background_status when the brief result is not enough.")
	}
	if len(parts) == 0 {
		return ""
	}
	return usageSection("Subagents", parts)
}

func (p *SpawnProvider) Tools(_ context.Context, session SessionContext) ([]toolexec.Tool, error) {
	if session.IsSubagent || p.controller == nil {
		return nil, nil
	}
	sess := session
	return []toolexec.Tool{
		{Name: ToolListAgentBackends().String(), Description: "List configured external agent instances and their managed child capabilities. Only available instances accept delegation.", Parameters: toolexec.SchemaFor[listAgentsArgs](), Execute: toolexec.Typed(func(ctx *toolexec.ToolExecContext, _ listAgentsArgs) (sdk.ToolOutput, error) {
			return toolexec.OutputPair(p.controller.Backends(ctx.Context, sess))
		})},
		{Name: ToolControlAgent().String(), Description: "Read a child task's durable status and result, or stop its exact run. Status works after restart. Stop requires the run_id returned by status or list_agents; an accepted stop is not confirmed termination.", Parameters: toolexec.SchemaFor[SubagentControlArgs](toolexec.Enum("action", "status", "stop")), Execute: toolexec.Typed(func(ctx *toolexec.ToolExecContext, args SubagentControlArgs) (sdk.ToolOutput, error) {
			return toolexec.OutputPair(p.controller.Control(ctx.Context, sess, args))
		})},
		{
			Name: ToolSpawnAgent().String(),
			// The description stays fixed: tool definitions precede the
			// conversation in every request, so the model catalog lives behind
			// list_models instead of changing this prefix whenever a model is
			// enabled, renamed or picked for the session.
			Description: "Create one managed subagent for an independent task. Choose a native model with list_models or a configured external instance with list_agent_backends. Returns an agent_id and child session reference.",
			Parameters:  toolexec.SchemaFor[SpawnAgentArgs](),
			Execute: toolexec.Typed(func(ctx *toolexec.ToolExecContext, args SpawnAgentArgs) (sdk.ToolOutput, error) {
				return toolexec.OutputPair(p.controller.Spawn(ctx.Context, sess, args))
			}),
		},
		{
			Name:        ToolSendMessage().String(),
			Description: "Send a follow-up message to an existing managed subagent. Messages to a busy agent are queued and run serially.",
			Parameters:  toolexec.SchemaFor[SendAgentMessageArgs](),
			Execute: toolexec.Typed(func(ctx *toolexec.ToolExecContext, args SendAgentMessageArgs) (sdk.ToolOutput, error) {
				return toolexec.OutputPair(p.controller.Send(ctx.Context, sess, args))
			}),
		},
		{
			Name:        ToolListAgents().String(),
			Description: "List managed subagents created in the current session only.",
			Parameters:  toolexec.SchemaFor[listAgentsArgs](),
			Execute: toolexec.Typed(func(ctx *toolexec.ToolExecContext, _ listAgentsArgs) (sdk.ToolOutput, error) {
				return toolexec.OutputPair(p.controller.List(ctx.Context, sess))
			}),
		},
		{
			Name:        ToolListModels().String(),
			Description: "List enabled chat models, including model_id, provider, description, and the current session model marker. Subagents can only use models from the current session's provider.",
			Parameters:  toolexec.SchemaFor[listModelsArgs](),
			Execute: toolexec.Typed(func(ctx *toolexec.ToolExecContext, _ listModelsArgs) (sdk.ToolOutput, error) {
				return toolexec.OutputPair(p.controller.Models(ctx.Context, sess))
			}),
		},
	}, nil
}

type SpawnAgentArgs struct {
	BotAgentID      string `json:"bot_agent_id,omitempty" jsonschema:"Configured external agent instance from list_agent_backends. Mutually exclusive with model_id, provider and fork. Runs one task with the instance configuration."`
	Fork            bool   `json:"fork,omitempty" jsonschema:"If true, inherit the parent model's current message context while keeping the subagent system prompt and tools."`
	ID              string `json:"id,omitempty" jsonschema:"Optional memorable agent id. If omitted, an id like agent_1 is assigned. Must be lowercase letters, digits, underscore, or hyphen."`
	ModelID         string `json:"model_id,omitempty" jsonschema:"Optional external model name from the current session's provider. Omit to use the current session model."`
	Provider        string `json:"provider,omitempty" jsonschema:"Optional provider name. Must match the current session's provider."`
	RunInBackground bool   `json:"run_in_background,omitempty" jsonschema:"If true, return immediately with a task_id. Use wait_until(task_id), then get_background_status(task_id) to inspect result."`
	Task            string `json:"task" jsonschema:"Task instruction for the new agent."`
}

type SendAgentMessageArgs struct {
	ID              string `json:"id" jsonschema:"Existing agent id returned when the agent was created or listed."`
	Message         string `json:"message" jsonschema:"Follow-up instruction for the agent."`
	RunInBackground bool   `json:"run_in_background,omitempty" jsonschema:"If true, return immediately with a task_id. If the agent is busy, the message is queued regardless of this value. Use wait_until(task_id), then get_background_status(task_id) to inspect result."`
}

type listAgentsArgs struct{}

type listModelsArgs struct{}

// SubagentControlArgs addresses a durable child run, independently of transient background IDs.
type SubagentControlArgs struct {
	ID     string `json:"id" jsonschema:"Child agent id from spawn_agent or list_agents."`
	Action string `json:"action" jsonschema:"Read status or stop the exact run."`
	RunID  string `json:"run_id,omitempty" jsonschema:"Required for stop. The exact child run_id returned by status."`
}
