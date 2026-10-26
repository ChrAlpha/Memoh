package compaction

import (
	"encoding/json"
	"slices"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	contextlimit "github.com/felinics/memoh/internal/agent/context/limit"
)

// forkRead is what a pass sends its summarizer of the request before the
// tail, ahead of the instruction: the request's messages as the run sends
// them, except that a message larger than a fork can read whole keeps its
// head and tail (forkMessageCap). A fold therefore reads, and stands for, any
// message the request holds, however large.
func forkRead(ev Event) []sdk.Message {
	messages := slices.Clip(ev.Request.Messages[:ev.Cut])
	capTokens := forkMessageCap(ev)
	var read []sdk.Message
	for i := range messages {
		if forkTokens(sdk.Request{Messages: messages[i : i+1]}) <= capTokens {
			continue
		}
		if read == nil {
			read = slices.Clone(messages)
		}
		read[i] = cutForFork(messages[i], capTokens)
	}
	if read == nil {
		return messages
	}
	return read
}

// forkMessageCap is the most one message takes in a fork request: what the
// window has beside the system prompt and tools, the longest summary a pass
// reads before it, the instruction and the least output a fork asks for. A
// pass can therefore read any message after a summary, and a step's results,
// which take at most half of the run's room, reach the fork whole.
func forkMessageCap(ev Event) int {
	envelope := forkTokens(sdk.Request{System: ev.Request.System, Tools: ev.Request.Tools})
	return max(ev.Window-envelope-ev.Allowance/summaryShareOfAllowance-forkInstructionReserve-minForkOutputTokens, minForkOutputTokens)
}

// cutForFork renders message within capTokens for a fork: its text, the
// arguments of the calls it makes and the results it carries keep their head
// and tail, and the media it carries are named instead of sent. Its role and
// its tool calls and results stay, so the fork pairs them the way the request
// does.
func cutForFork(message sdk.Message, capTokens int) sdk.Message {
	texts := 0
	for _, part := range message.Content {
		switch part.(type) {
		case sdk.TextPart, sdk.ToolResultPart, sdk.ToolCallPart:
			texts++
		}
	}
	// Each text takes an equal share, with a quarter of it left for the
	// part's envelope and the marker the cut adds.
	limit := contextlimit.ToolOutputLimit{MaxBytes: int(contextfrag.BudgetBytesForTokens(capTokens)) * 3 / 4 / max(texts, 1)}
	cut := message
	cut.Content = make([]sdk.MessagePart, 0, len(message.Content))
	for _, part := range message.Content {
		switch p := part.(type) {
		case sdk.TextPart:
			p.Text = contextlimit.LimitString(p.Text, "message", limit)
			cut.Content = append(cut.Content, p)
		case sdk.ToolResultPart:
			p.Result = sdk.TextOutput(contextlimit.LimitString(p.Result.String(), "tool result", limit))
			cut.Content = append(cut.Content, p)
		case sdk.ToolCallPart:
			if arguments := p.Input.String(); len(arguments) > limit.MaxBytes {
				// The fork sends a call's arguments as a JSON object, so the
				// cut ones travel as one string field of one.
				encoded, _ := json.Marshal(map[string]string{"truncated_arguments": contextlimit.LimitString(arguments, "tool call arguments", limit)})
				p.Input = sdk.ParseToolArguments(string(encoded))
			}
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
