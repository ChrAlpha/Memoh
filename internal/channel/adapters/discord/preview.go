package discord

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/felinics/memoh/internal/channel"
)

const (
	discordThinkingText = "Thinking..."
	// discordEditInterval spaces the edits that show streamed text, which
	// Discord rate limits strictly.
	discordEditInterval = 2 * time.Second
)

// discordPreview shows one reply as a message that is edited while the reply
// is produced. A message opens as a placeholder and takes the streamed text,
// then the final text.
type discordPreview struct {
	target  string
	reply   *channel.ReplyRef
	session *discordgo.Session

	mu          sync.Mutex
	ended       bool
	msgID       string
	placeholder bool
	buffer      strings.Builder
	lastUpdate  time.Time
	// toolMessages maps a running tool call to the message that shows it.
	toolMessages map[string]string
}

var _ channel.PreparedPreview = (*discordPreview)(nil)

func (p *discordPreview) Update(ctx context.Context, event channel.PreparedStreamEvent) error {
	if p.finished() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	switch event.Type {
	case channel.StreamEventStatus:
		if event.Status == channel.StreamStatusStarted {
			return p.ensureMessage(discordThinkingText)
		}
		return nil

	case channel.StreamEventDelta:
		if event.Delta == "" || event.Phase == channel.StreamPhaseReasoning {
			return nil
		}
		p.mu.Lock()
		p.buffer.WriteString(event.Delta)
		due := time.Since(p.lastUpdate) > discordEditInterval
		p.mu.Unlock()
		if due {
			return p.updateMessage()
		}
		return nil

	case channel.StreamEventReset:
		// Keep the message; the regenerated reply edits over the failed text.
		p.mu.Lock()
		p.buffer.Reset()
		p.mu.Unlock()
		return nil

	case channel.StreamEventError:
		errText := strings.TrimSpace(event.Error)
		if errText == "" {
			return nil
		}
		return p.finalizeMessage(channel.ErrorReplyText(event.ErrorCode, errText), nil, nil)

	case channel.StreamEventToolCallStart:
		if bufText := p.bufferedText(); bufText != "" {
			if err := p.finalizeMessage(bufText, nil, nil); err != nil {
				return err
			}
		}
		p.resetStreamState()
		return p.sendToolCallMessage(event.ToolCall, channel.BuildToolCallStart(event.ToolCall))
	case channel.StreamEventToolCallEnd:
		return p.sendToolCallMessage(event.ToolCall, channel.BuildToolCallEnd(event.ToolCall))

	case channel.StreamEventAgentStart, channel.StreamEventAgentEnd, channel.StreamEventPhaseStart, channel.StreamEventPhaseEnd, channel.StreamEventProcessingStarted, channel.StreamEventProcessingCompleted, channel.StreamEventProcessingFailed:
		// Status events - no action needed for Discord
		return nil

	default:
		return fmt.Errorf("unsupported stream event type: %s", event.Type)
	}
}

// Finish ends the reply. A nil final means the run said nothing: a message
// that still shows only the placeholder is removed. A final without a message
// leaves the text streamed so far as the reply.
func (p *discordPreview) Finish(ctx context.Context, final *channel.PreparedStreamEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.end() {
		return nil
	}
	if final == nil || final.Final == nil {
		return p.clearPlaceholder()
	}

	msg := final.Final.Message
	bufText := p.bufferedText()
	finalText := bufText
	var source *channel.Message
	if !msg.Message.IsEmpty() {
		finalText = renderDiscordStreamFinalText(msg.Message, bufText)
		source = &msg.Message
	}
	if finalText != "" {
		if err := p.finalizeMessage(finalText, msg.Message.Actions, source); err != nil {
			return err
		}
	} else if err := p.clearPlaceholder(); err != nil {
		return err
	}
	for _, att := range msg.Attachments {
		if err := p.sendAttachment(ctx, att); err != nil {
			return err
		}
	}
	return nil
}

// Abort ends the reply as it stands.
func (p *discordPreview) Abort(context.Context) error {
	p.end()
	return nil
}

func (p *discordPreview) finished() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ended
}

// end marks the preview ended and reports whether this call did it.
func (p *discordPreview) end() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return false
	}
	p.ended = true
	return true
}

func (p *discordPreview) bufferedText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.TrimSpace(p.buffer.String())
}

func renderDiscordStreamFinalText(msg channel.Message, buffered string) string {
	if rich := renderDiscordMessagePartsContent(msg); rich != "" {
		return rich
	}
	if authoritative := strings.TrimSpace(msg.PlainText()); authoritative != "" {
		return authoritative
	}
	return strings.TrimSpace(buffered)
}

func (p *discordPreview) ensureMessage(text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.msgID != "" {
		return nil
	}

	content := truncateDiscordText(text)
	messageSend := &discordgo.MessageSend{
		Content:         content,
		AllowedMentions: discordAllowedMentionsNone(),
	}
	if p.reply != nil && p.reply.MessageID != "" {
		messageSend.Reference = &discordgo.MessageReference{
			ChannelID: p.target,
			MessageID: p.reply.MessageID,
		}
	}

	msg, err := p.session.ChannelMessageSendComplex(p.target, messageSend)
	if err != nil {
		return err
	}

	p.msgID = msg.ID
	p.placeholder = true
	p.lastUpdate = time.Now()
	return nil
}

func (p *discordPreview) updateMessage() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.msgID == "" {
		return nil
	}

	content := p.buffer.String()
	if content == "" {
		return nil
	}

	content = truncateDiscordText(content)

	edit := discordgo.NewMessageEdit(p.target, p.msgID)
	edit.SetContent(content)
	edit.AllowedMentions = discordAllowedMentionsNone()
	_, err := p.session.ChannelMessageEditComplex(edit)
	if err != nil {
		return err
	}

	p.placeholder = false
	p.lastUpdate = time.Now()
	return nil
}

func (p *discordPreview) finalizeMessage(text string, actions []channel.Action, source *channel.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	text = truncateDiscordText(text)
	components, err := discordURLActionComponents(actions)
	if err != nil {
		return err
	}
	allowedMentions := discordAllowedMentionsNone()
	if source != nil {
		allowedMentions = discordAllowedMentionsForMessage(*source)
	}

	if p.msgID == "" {
		messageSend := &discordgo.MessageSend{
			Content:         text,
			Components:      components,
			AllowedMentions: allowedMentions,
		}
		if p.reply != nil && p.reply.MessageID != "" {
			messageSend.Reference = &discordgo.MessageReference{
				ChannelID: p.target,
				MessageID: p.reply.MessageID,
			}
		}
		msg, err := p.session.ChannelMessageSendComplex(p.target, messageSend)
		if err != nil {
			return err
		}
		p.msgID = msg.ID
		p.placeholder = false
		p.lastUpdate = time.Now()
		return nil
	}

	edit := discordgo.NewMessageEdit(p.target, p.msgID)
	edit.SetContent(text)
	if len(components) > 0 {
		edit.Components = &components
	}
	edit.AllowedMentions = allowedMentions
	if _, err = p.session.ChannelMessageEditComplex(edit); err != nil {
		return err
	}
	p.placeholder = false
	return nil
}

// clearPlaceholder removes the message while it still shows only the
// placeholder.
func (p *discordPreview) clearPlaceholder() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.placeholder || p.msgID == "" {
		return nil
	}
	if err := p.session.ChannelMessageDelete(p.target, p.msgID); err != nil {
		return err
	}
	p.msgID = ""
	p.placeholder = false
	return nil
}

// sendToolCallMessage posts a Discord message on tool_call_start and edits it
// on tool_call_end so the running → completed/failed transition is contained
// in one visible post. Falls back to a new message if the edit fails.
func (p *discordPreview) sendToolCallMessage(tc *channel.StreamToolCall, pres channel.ToolCallPresentation) error {
	text := truncateDiscordText(strings.TrimSpace(channel.RenderToolCallMessageMarkdown(pres)))
	if text == "" {
		return nil
	}
	callID := ""
	if tc != nil {
		callID = strings.TrimSpace(tc.CallID)
	}
	if pres.Status != channel.ToolCallStatusRunning && callID != "" {
		if msgID, ok := p.lookupToolCallMessage(callID); ok {
			edit := discordgo.NewMessageEdit(p.target, msgID)
			edit.SetContent(text)
			edit.AllowedMentions = discordAllowedMentionsNone()
			if _, err := p.session.ChannelMessageEditComplex(edit); err == nil {
				p.forgetToolCallMessage(callID)
				return nil
			}
			p.forgetToolCallMessage(callID)
		}
	}
	messageSend := &discordgo.MessageSend{
		Content:         text,
		AllowedMentions: discordAllowedMentionsNone(),
	}
	if p.reply != nil && p.reply.MessageID != "" {
		messageSend.Reference = &discordgo.MessageReference{
			ChannelID: p.target,
			MessageID: p.reply.MessageID,
		}
	}
	msg, err := p.session.ChannelMessageSendComplex(p.target, messageSend)
	if err != nil {
		return err
	}
	if pres.Status == channel.ToolCallStatusRunning && callID != "" && msg != nil && msg.ID != "" {
		p.storeToolCallMessage(callID, msg.ID)
	}
	return nil
}

func (p *discordPreview) lookupToolCallMessage(callID string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.toolMessages == nil {
		return "", false
	}
	v, ok := p.toolMessages[callID]
	return v, ok
}

func (p *discordPreview) storeToolCallMessage(callID, msgID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.toolMessages == nil {
		p.toolMessages = make(map[string]string)
	}
	p.toolMessages[callID] = msgID
}

func (p *discordPreview) forgetToolCallMessage(callID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.toolMessages == nil {
		return
	}
	delete(p.toolMessages, callID)
}

func (p *discordPreview) resetStreamState() {
	p.mu.Lock()
	p.msgID = ""
	p.placeholder = false
	p.buffer.Reset()
	p.lastUpdate = time.Time{}
	p.mu.Unlock()
}

func (p *discordPreview) sendAttachment(ctx context.Context, att channel.PreparedAttachment) error {
	file, err := discordPreparedAttachmentToFile(ctx, att)
	if err != nil {
		return err
	}

	messageSend := &discordgo.MessageSend{
		Files:           []*discordgo.File{file},
		AllowedMentions: discordAllowedMentionsNone(),
	}

	if p.reply != nil && p.reply.MessageID != "" {
		messageSend.Reference = &discordgo.MessageReference{
			ChannelID: p.target,
			MessageID: p.reply.MessageID,
		}
	}

	_, err = p.session.ChannelMessageSendComplex(p.target, messageSend)
	return err
}
