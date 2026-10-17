package application

import (
	"context"
	"strings"

	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/transcript"
)

// A discuss run reads the batch it answers from history, which is what a
// runtime that assembles its own context needs. An External Agent runtime
// does not: it is reset for the run, so whatever it is prompted with is the
// whole of what it knows. Its batch therefore travels as the prompt, framed by
// the contract the conversation is answered under.
const (
	discussPromptPrefix = "You are replying in a discuss-mode conversation. The runtime is reset each turn, so use the complete context below as the source of truth.\n\n" +
		"IMPORTANT: You MUST use the `send` tool to speak in the observed conversation. Ordinary text output is internal and invisible to everyone.\n\n"
	discussPromptSuffix = "Reply to the latest user-visible message when a response is appropriate."
)

// discussExternalPrompt renders what a discuss run hands an External Agent
// runtime: the context the run assembles, message by message. An entry whose
// runtime owns its context still reads history the same way a native run does,
// so the batch, its sender attribution and any summary covering it arrive as
// one context rather than as a message of its own.
func (s *Service) discussExternalPrompt(ctx context.Context, req runreq.ChatRequest) (string, error) {
	assembled, err := s.transcript.Assemble(ctx, transcript.ForChat(transcript.PurposeRun, req, 0))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(discussPromptPrefix)
	for _, message := range assembled.Messages {
		content := strings.TrimSpace(message.TextContent())
		if content == "" {
			continue
		}
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		b.WriteString("[")
		b.WriteString(role)
		b.WriteString("]\n")
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	b.WriteString(discussPromptSuffix)
	return b.String(), nil
}
