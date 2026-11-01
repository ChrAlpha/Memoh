package tools

import (
	"context"
	"errors"
	"strings"

	sdk "github.com/felinics/twilight/sdk"
)

// SubagentTranscriptLoader reads the transcript a spawned task continues.
type SubagentTranscriptLoader interface {
	// LoadSubagentTranscript returns the agent thread's own history as a
	// direct turn on that thread would replay it: under its history
	// checkpoint, inside the context budget, tool calls closed. History the
	// budget cannot hold is compacted first; history still over it fails the
	// load. The task's request row is left out; the run sends it as the query.
	LoadSubagentTranscript(ctx context.Context, req SubagentTranscriptRequest) ([]sdk.Message, error)
}

// SubagentTranscriptRequest names the thread to read and the window to fit.
type SubagentTranscriptRequest struct {
	BotID            string
	ChatID           string
	SessionID        string
	UserID           string
	ConversationType string
	ReplyTarget      string
	// Query is the task the run sends; a compaction the load needs writes its
	// summary toward it.
	Query string
	// RequestMessageID is the persisted task message of this run, if any.
	RequestMessageID string
	// TurnPosition is the turn admission allocated the task, if any. A
	// compaction folds only the rows before it.
	TurnPosition *int64
	// ModelID is the subagent model's record id, which a compaction the load
	// needs summarizes with unless the bot names another.
	ModelID string
	// ContextTokenBudget is the subagent model's context window; zero leaves
	// the window to the loader's own cap.
	ContextTokenBudget int
}

// SetTranscriptLoader injects the history reader. Setter injection for the
// same reason as the admitter: the application service that reads history
// also runs the tools this provider registers.
func (p *SpawnProvider) SetTranscriptLoader(loader SubagentTranscriptLoader) {
	p.transcripts = loader
}

// loadAgentContext assembles what a task replays ahead of its query: the
// parent snapshot a forked agent inherited, then the agent's own transcript.
func (p *SpawnProvider) loadAgentContext(ctx context.Context, req *agentRequest, contextBudget int) ([]sdk.Message, error) {
	if p.transcripts == nil {
		return nil, errors.New("subagent transcript loader not available")
	}
	transcriptReq := SubagentTranscriptRequest{
		BotID:              req.parentSession.BotID,
		ChatID:             req.parentSession.ChatID,
		SessionID:          req.agentSessionID,
		UserID:             req.parentSession.UserID,
		ConversationType:   req.parentSession.ConversationType,
		ReplyTarget:        req.parentSession.ReplyTarget,
		Query:              req.message,
		RequestMessageID:   req.requestMessageID,
		ModelID:            req.runtime.UUID,
		ContextTokenBudget: contextBudget,
	}
	if strings.TrimSpace(req.admission.TurnID) != "" {
		position := req.admission.TurnPosition
		transcriptReq.TurnPosition = &position
	}
	transcript, err := p.transcripts.LoadSubagentTranscript(ctx, transcriptReq)
	if err != nil {
		return nil, err
	}
	if !req.config.Forked {
		return transcript, nil
	}
	parent, err := p.loadAgentForkContext(ctx, req.agentSessionID)
	if err != nil {
		return nil, err
	}
	return append(parent, transcript...), nil
}
