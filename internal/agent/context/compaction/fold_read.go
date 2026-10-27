package compaction

import (
	"encoding/json"
	"fmt"
	"slices"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	contextlimit "github.com/felinics/memoh/internal/agent/context/limit"
)

// forkRead is what a pass sends its summarizer of the request before the
// newest messages the event keeps, ahead of the instruction: the request's messages as the run sends
// them, except that a message larger than a fork can read whole keeps its
// head and tail (forkMessageCap). A fold therefore reads, and stands for, any
// message the request holds, however large. Messages that render no source,
// runtime context and a system prompt the prompt cache moved into the
// messages, reach the fork whole: every pass reads them. A message no cut
// brings within the cap fails the pass as an input overflow.
func forkRead(ev Event) ([]sdk.Message, error) {
	messages := slices.Clip(ev.Request.Messages[:ev.cut])
	capTokens := forkMessageCap(ev)
	var read []sdk.Message
	for i := range messages {
		if len(ev.Sources[i].Sources) == 0 || forkTokens(sdk.Request{Messages: messages[i : i+1]}) <= capTokens {
			continue
		}
		if read == nil {
			read = slices.Clone(messages)
		}
		cut, ok := cutForFork(messages[i], capTokens)
		if !ok {
			return nil, failed(ReasonInputOverflow, fmt.Errorf("%w: message %d does not fit a fork in %d tokens however it is cut", errCompactionInputOverflow, i, capTokens))
		}
		read[i] = cut
	}
	if read == nil {
		return messages, nil
	}
	return read, nil
}

// forkMessageCap is the most one message takes in a fork request: what the
// window has beside the system prompt, the tools and the messages that render
// no source, the longest summary a pass reads before it, the instruction and
// the least output a fork asks for. A pass can therefore read any message
// after a summary.
func forkMessageCap(ev Event) int {
	var fixed []sdk.Message
	for i, message := range ev.Request.Messages[:ev.cut] {
		if len(ev.Sources[i].Sources) == 0 {
			fixed = append(fixed, message)
		}
	}
	envelope := forkTokens(sdk.Request{System: ev.Request.System, Messages: fixed, Tools: ev.Request.Tools})
	return max(ev.Window-envelope-summaryBudget(ev)-forkInstructionReserve-minForkOutputTokens, minForkOutputTokens)
}

// cutForFork renders message within capTokens for a fork: its text, the
// arguments of the calls it makes and the results it carries keep their head
// and tail, and the media it carries are named instead of sent. Its role and
// its tool calls and results stay, so the fork pairs them the way the request
// does. The cut is measured the way the fork is priced, so content that grows
// when it is encoded keeps less of itself until the message fits; it reports
// false when none does.
func cutForFork(message sdk.Message, capTokens int) (sdk.Message, bool) {
	budget := int(contextfrag.ProviderBudgetBytesForTokens(capTokens))
	cut := cutParts(message, budget)
	for range 8 {
		tokens := forkTokens(sdk.Request{Messages: []sdk.Message{cut}})
		if tokens <= capTokens {
			return cut, true
		}
		budget = budget * capTokens / tokens * 9 / 10
		cut = cutParts(message, budget)
	}
	return cut, forkTokens(sdk.Request{Messages: []sdk.Message{cut}}) <= capTokens
}

// cutParts keeps the parts of message within budget bytes together, an equal
// share each, a quarter of it left for the part's envelope and the marker the
// cut adds.
func cutParts(message sdk.Message, budget int) sdk.Message {
	texts := 0
	for _, part := range message.Content {
		switch part.(type) {
		case sdk.TextPart, sdk.ToolResultPart, sdk.ToolCallPart:
			texts++
		}
	}
	limit := contextlimit.ToolOutputLimit{MaxBytes: budget * 3 / 4 / max(texts, 1)}
	cut := message
	cut.Content = make([]sdk.MessagePart, 0, len(message.Content))
	for _, part := range message.Content {
		switch p := part.(type) {
		case sdk.TextPart:
			p.Text = contextlimit.LimitString(p.Text, "message", limit)
			cut.Content = append(cut.Content, p)
		case sdk.ToolResultPart:
			p.Result = cutOutput(p.Result, limit)
			cut.Content = append(cut.Content, p)
		case sdk.ToolCallPart:
			p.Input = cutArguments(p.Input, limit)
			cut.Content = append(cut.Content, p)
		case sdk.ReasoningPart:
			// A summary stands for what was said and done, not how it was
			// reasoned.
		case sdk.ImagePart, sdk.FilePart:
			if message.Role != sdk.MessageRoleTool {
				cut.Content = append(cut.Content, sdk.TextPart{Text: "[media]"})
			}
		default:
			cut.Content = append(cut.Content, part)
		}
	}
	return cut
}

// cutOutput keeps a result within limit as the fork encodes it: a text keeps
// its head and tail, a document the head and tail of its strings.
func cutOutput(output sdk.ToolOutput, limit contextlimit.ToolOutputLimit) sdk.ToolOutput {
	var value any = output.Text
	if output.IsJSON() && json.Unmarshal(output.JSON, &value) != nil {
		value = string(output.JSON)
	}
	limited := contextlimit.LimitToolOutput(value, "tool result", limit)
	if text, ok := limited.(string); ok {
		return sdk.TextOutput(text)
	}
	if document, err := sdk.JSONOutput(limited); err == nil {
		return document
	}
	return sdk.TextOutput(contextlimit.MarshalString(limited))
}

// cutArguments keeps a call's arguments within limit as the fork encodes
// them: the head and tail of their strings, still one JSON object, so the
// fork sends the call the way providers take one.
func cutArguments(arguments sdk.ToolArguments, limit contextlimit.ToolOutputLimit) sdk.ToolArguments {
	if !contextlimit.EncodedExceeds(arguments.Object(), limit) {
		return arguments
	}
	var value any
	if arguments.Unmarshal(&value) != nil {
		value = arguments.String()
	}
	limited := contextlimit.LimitToolOutput(value, "tool call arguments", limit)
	if _, ok := limited.(map[string]any); !ok {
		limited = map[string]any{"truncated_arguments": limited}
	}
	encoded, err := json.Marshal(limited)
	if err != nil {
		return arguments
	}
	return sdk.ParseToolArguments(string(encoded))
}
