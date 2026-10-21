package compact

import (
	"context"
	"log/slog"
	"reflect"
	"strings"

	"github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/transcript"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
)

// BindStep lets a native run compact its own context between model
// calls. The compaction reads the run's committed steps back from its thread,
// so it applies to runs that commit them as they complete.
//
// A retry or edit run is the exception in what it may read. The turn it
// replaces is cut off from its history, and its own first step starts the
// replacement. Such a run compacts only the visible history before its cutoff
// and carries its own messages over from the loop's memory.
func (c *Compactor) BindStep(cfg *native.RunConfig, req runreq.ChatRequest, committer *persist.StepCommitter) {
	if committer == nil || !c.runner.enabled() {
		return
	}
	replacement := req.TurnReplacement != nil
	if !replacement && strings.TrimSpace(req.HistoryCutoffBeforeMessageID) != "" {
		return
	}
	run := &stepRun{
		compactor:        c,
		req:              req,
		requestMessageID: committer.RequestMessageID,
		budget:           cfg.ContextBudgetMaxTokens,
		modelID:          cfg.CurrentModelUUID,
		replacement:      replacement,
		tailStart:        cfg.ContextTrimmableMessages,
		extras:           ephemeralRunMessages(*cfg),
		request:          unstoredRequestMessage(*cfg, req),
		inlineParts:      inlineRunParts(*cfg),
		pressure:         committer.Pressure(),
	}
	cfg.StepContextCompactor = run.compact
}

// SubagentStep is the between-calls compactor of the spawned run runID, whose
// steps are committed to its thread as they complete (SubagentStepCommit
// declines otherwise, and the adapter installs this only when it did not).
func (c *Compactor) SubagentStep(run native.SpawnCompaction, runID string) native.StepContextCompactor {
	if !c.runner.enabled() || strings.TrimSpace(runID) == "" {
		return nil
	}
	requestMessageID := strings.TrimSpace(run.TurnRequestMessageID)
	return (&stepRun{
		compactor: c,
		req: runreq.ChatRequest{
			BotID:       run.BotID,
			ChatID:      run.ChatID,
			ThreadID:    run.SessionID,
			UserID:      run.UserID,
			RunID:       runID,
			SessionType: sessionpkg.TypeSubagent,
		},
		requestMessageID: func() string { return requestMessageID },
		budget:           run.ContextBudgetMaxTokens,
		modelID:          run.ModelUUID,
	}).compact
}

// stepRun is one run's between-calls compaction.
type stepRun struct {
	compactor *Compactor
	// req names the run and the thread its steps are committed to.
	req runreq.ChatRequest
	// requestMessageID is the row of the user message the run answers; empty
	// while it is not stored.
	requestMessageID func() string
	budget           int
	modelID          string
	// replacement marks a retry or edit run, which carries its own messages
	// (everything from tailStart on) over from the loop instead of reading
	// them back. tailStart moves to the end of the history once a rebuild
	// has replaced the conversation.
	replacement bool
	tailStart   int
	// extras are the messages the run was started with that no history row
	// holds (workspace notice, recalled memory, requested skills). They stay
	// in front of the user message they were written for.
	extras []sdk.Message
	// request is the user's message as the run sends it, for a run whose
	// message becomes a row only with its first step: before the first call
	// no row holds it yet.
	request []sdk.Message
	// inlineParts are the images and attachments the run's user message
	// carried inline; the stored row holds only the text.
	inlineParts []sdk.MessagePart
	// pressure follows the conversation each call sends, so the run's end is
	// judged by what it grew to; nil when nothing reads it.
	pressure *runreq.RunPressure
}

// ephemeralRunMessages returns the messages a run adds between the stored
// history and the user's message.
func ephemeralRunMessages(cfg native.RunConfig) []sdk.Message {
	start := cfg.ContextTrimmableMessages
	end := len(cfg.Messages)
	if index := cfg.ContextCurrentUserMessageIndex; index != nil && *index >= start && *index < end {
		end = *index
	}
	if start < 0 || start >= end {
		return nil
	}
	return append([]sdk.Message(nil), cfg.Messages[start:end]...)
}

// unstoredRequestMessage is the user's message a run sends from its query,
// which is stored with the run's first step rather than ahead of it.
func unstoredRequestMessage(cfg native.RunConfig, req runreq.ChatRequest) []sdk.Message {
	if req.ReusePersistedUserMessage || strings.TrimSpace(cfg.Query) == "" {
		return nil
	}
	return []sdk.Message{sdk.UserMessage(cfg.Query)}
}

// inlineRunParts returns the media the run attached to the user's message.
func inlineRunParts(cfg native.RunConfig) []sdk.MessagePart {
	var parts []sdk.MessagePart
	for _, image := range cfg.InlineImages {
		if strings.TrimSpace(image.Image) != "" {
			parts = append(parts, image)
		}
	}
	return append(parts, cfg.InlineAttachments...)
}

// compact is the loop's hook. Past the hard share of the window it compacts
// the steps the run has committed and rebuilds the conversation from the
// stored rows, the newest tool exchange and the user's message included, since
// the compaction never takes them. A compaction already running is waited for
// first and its summary used. Failure, cooldown, and a rebuilt context no
// smaller than the current one keep the conversation as it is.
func (c *stepRun) compact(ctx context.Context, input native.StepContextInput) []sdk.Message {
	c.pressure.Observe(input.Messages)
	current := runreq.RequestInputTokens(input.FixedTokens, input.Messages)
	if urgencyOf(current, c.budget, 0) != urgencySync {
		return nil
	}
	k, req := c.compactor, c.req
	if k.await(req.BotID, req.ThreadID, strings.TrimSpace(req.RunID)) {
		if rebuilt, ok := c.rebuild(ctx, input); ok && rebuilt.tokens < current {
			if urgencyOf(rebuilt.tokens, c.budget, 0) != urgencySync {
				return c.adopt(rebuilt)
			}
			current = rebuilt.tokens
		}
	}
	res := k.runSync(ctx, req, current, c.fixedTokens(input), c.budget, c.modelID)
	k.logger.InfoContext(ctx, "step_compaction",
		slog.String("status", res.Status),
		slog.String("reason", res.Reason),
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.String("run_id", req.RunID),
		slog.Int("input_tokens", current),
		slog.Int("threshold_tokens", hardThreshold(c.budget)))
	if res.Status != compaction.StatusOK {
		return nil
	}
	rebuilt, ok := c.rebuild(ctx, input)
	if !ok || rebuilt.tokens >= current {
		return nil
	}
	if urgencyOf(rebuilt.tokens, c.budget, 0) == urgencySync {
		k.logger.WarnContext(ctx, "step_compaction_insufficient",
			slog.String("bot_id", req.BotID),
			slog.String("session_id", req.ThreadID),
			slog.Int("input_tokens", rebuilt.tokens),
			slog.Int("threshold_tokens", hardThreshold(c.budget)))
	}
	return c.adopt(rebuilt)
}

// rebuiltConversation is a conversation assembled from the stored rows.
type rebuiltConversation struct {
	messages []sdk.Message
	tokens   int
	// tailStart is where the run's own messages begin in messages.
	tailStart int
}

// fixedTokens is the part of the run's current request that no compaction can
// shrink: the system prompt and tools, plus the messages no stored row holds.
// Those are a replacement's own messages from tailStart on, or otherwise the
// run's notices and skills, and the user's message while no step of it is
// stored. Everything else in the request is a row the compaction may claim.
func (c *stepRun) fixedTokens(input native.StepContextInput) int {
	own := c.extras
	switch {
	case c.replacement:
		if c.tailStart < 0 || c.tailStart > len(input.Messages) {
			return input.FixedTokens
		}
		own = input.Messages[c.tailStart:]
	case c.requestMessageID() == "":
		own = append(append([]sdk.Message(nil), c.extras...), c.request...)
	}
	return input.FixedTokens + runreq.RequestInputTokens(0, own)
}

// adopt records that the loop continues on the rebuilt conversation.
func (c *stepRun) adopt(rebuilt rebuiltConversation) []sdk.Message {
	c.tailStart = rebuilt.tailStart
	c.pressure.Observe(rebuilt.messages)
	return rebuilt.messages
}

// rebuild assembles the run's conversation from the stored rows and prices
// the request it would make.
func (c *stepRun) rebuild(ctx context.Context, input native.StepContextInput) (rebuiltConversation, bool) {
	req := c.req
	// A retry or edit reads its history the way it did at the start, so the
	// rebuilt rows line up with the messages it carries over from the loop.
	if !c.replacement {
		// The user's message is a row by now; the continuation reads it with
		// the rest instead of having it appended from the request.
		req.UserMessagePersisted = false
		req.PersistedUserMessageID = ""
		req.RequiredHistoryMessageID = ""
	}
	assembled, err := c.compactor.transcript.Assemble(ctx, transcript.ForChat(transcript.PurposeContinuation, req, c.budget))
	if err != nil {
		c.compactor.logger.WarnContext(ctx, "step_compaction: assemble failed",
			slog.String("bot_id", req.BotID), slog.String("session_id", req.ThreadID), slog.Any("error", err))
		return rebuiltConversation{}, false
	}
	var rebuilt rebuiltConversation
	if c.replacement {
		if c.tailStart < 0 || c.tailStart > len(input.Messages) {
			return rebuiltConversation{}, false
		}
		// The cutoff keeps the replaced turn's rows out. The messages from
		// tailStart on are this run's own (notice, memory, an edit's new
		// message with its media, every step executed so far). Their rows
		// stay hidden until the run finalizes and are dropped if it fails,
		// so they are taken from what the loop holds and never compacted.
		messages := c.forkContext(ctx)
		if split, found := recordIndex(assembled.Records, req.RequiredHistoryMessageID); found {
			// A retry answers a request that is a visible row. The run put
			// its inline media on that message, which the row does not hold.
			messages = append(messages, stepMessages(assembled.Records[:split])...)
			request := stepMessages(assembled.Records[split:])
			attachInlineParts(request, c.inlineParts, true)
			messages = append(messages, request...)
		} else {
			messages = append(messages, stepMessages(assembled.Records)...)
		}
		rebuilt.tailStart = len(messages)
		messages = append(messages, input.Messages[c.tailStart:]...)
		rebuilt.messages = messages
	} else {
		// The user's message and every step after it are rows once the
		// first step is stored. Before the run's first call none are, and
		// the message is the one the run sends from its query.
		history, request := assembled.Records, append([]sdk.Message(nil), c.request...)
		split, found := recordIndex(assembled.Records, c.requestMessageID())
		if found {
			history, request = assembled.Records[:split], stepMessages(assembled.Records[split:])
		}
		messages := c.forkContext(ctx)
		messages = append(messages, stepMessages(history)...)
		messages = append(messages, c.extras...)
		attachInlineParts(request, c.inlineParts, found)
		messages = append(messages, request...)
		rebuilt.messages = messages
	}
	rebuilt.tokens = runreq.RequestInputTokens(input.FixedTokens, rebuilt.messages)
	return rebuilt, true
}

// forkContext is the parent's context a forked subagent thread starts from.
// It is stored apart from the thread's rows, so a rebuild has to put it back.
func (c *stepRun) forkContext(ctx context.Context) []sdk.Message {
	fork := c.compactor.transcript.SubagentForkMessages(ctx, c.req)
	if len(fork) == 0 {
		return nil
	}
	return runreq.ModelMessagesToSDKMessages(transcript.NormalizeContextMessages(fork))
}

// recordIndex is where the row with id sits in the rows, and whether it is
// among them.
func recordIndex(records []historyfrag.HistoryRecord, id string) (int, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return 0, false
	}
	for i := range records {
		if strings.TrimSpace(records[i].DBMessageID) == id {
			return i, true
		}
	}
	return 0, false
}

// attachInlineParts puts the run's inline media back on its user message: the
// first of the messages when the message's row was found, the last user
// message otherwise. Parts the message already carries are not added twice.
func attachInlineParts(messages []sdk.Message, parts []sdk.MessagePart, found bool) {
	if len(parts) == 0 {
		return
	}
	var target *sdk.Message
	switch {
	case found && len(messages) > 0 && messages[0].Role == sdk.MessageRoleUser:
		target = &messages[0]
	default:
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == sdk.MessageRoleUser {
				target = &messages[i]
				break
			}
		}
	}
	if target == nil {
		return
	}
	content := append([]sdk.MessagePart(nil), target.Content...)
	for _, part := range parts {
		if !containsPart(content, part) {
			content = append(content, part)
		}
	}
	target.Content = content
}

func containsPart(parts []sdk.MessagePart, want sdk.MessagePart) bool {
	for _, part := range parts {
		if reflect.DeepEqual(part, want) {
			return true
		}
	}
	return false
}

func stepMessages(records []historyfrag.HistoryRecord) []sdk.Message {
	messages := make([]runreq.ModelMessage, 0, len(records))
	for i := range records {
		messages = append(messages, records[i].ModelMessage)
	}
	return runreq.ModelMessagesToSDKMessages(transcript.NormalizeContextMessages(messages))
}
