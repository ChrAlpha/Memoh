package matrix

import (
	"context"
	"strings"
	"sync"

	"github.com/felinics/memoh/internal/channel"
)

// matrixPreview shows one reply as room messages. The text of a phase is sent
// when the phase ends, and the final edits that message with the text in its
// final format. A tool call is its own message, edited when the call ends.
type matrixPreview struct {
	adapter *MatrixAdapter
	cfg     Config
	target  string
	reply   *channel.ReplyRef

	mu    sync.Mutex
	ended bool

	roomID          string
	originalEventID string
	rawBuffer       strings.Builder
	lastText        string
	lastFormat      channel.MessageFormat
	toolMessages    map[string]string
}

var _ channel.PreparedPreview = (*matrixPreview)(nil)

func (s *matrixPreview) Update(ctx context.Context, event channel.PreparedStreamEvent) error {
	if s.finished() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	switch event.Type {
	case channel.StreamEventStatus,
		channel.StreamEventPhaseStart,
		channel.StreamEventAgentStart,
		channel.StreamEventAgentEnd,
		channel.StreamEventProcessingStarted,
		channel.StreamEventProcessingCompleted,
		channel.StreamEventProcessingFailed:
		return nil
	case channel.StreamEventPhaseEnd:
		if event.Phase != channel.StreamPhaseText {
			return nil
		}
		return s.upsertText(ctx, s.bufferedText(), channel.MessageFormatPlain)
	case channel.StreamEventToolCallStart:
		if bufText := s.bufferedText(); bufText != "" {
			if err := s.upsertText(ctx, bufText, channel.MessageFormatPlain); err != nil {
				return err
			}
		}
		s.resetMessageState()
		return s.sendToolCallMessage(ctx, event.ToolCall, channel.BuildToolCallStart(event.ToolCall))
	case channel.StreamEventToolCallEnd:
		return s.sendToolCallMessage(ctx, event.ToolCall, channel.BuildToolCallEnd(event.ToolCall))
	case channel.StreamEventDelta:
		if event.Phase == channel.StreamPhaseReasoning || event.Delta == "" {
			return nil
		}
		s.mu.Lock()
		s.rawBuffer.WriteString(event.Delta)
		s.mu.Unlock()
		return nil
	case channel.StreamEventReset:
		s.mu.Lock()
		s.rawBuffer.Reset()
		s.mu.Unlock()
		return nil
	case channel.StreamEventError:
		errText := strings.TrimSpace(event.Error)
		if errText == "" {
			return nil
		}
		return s.upsertText(ctx, channel.ErrorReplyText(event.ErrorCode, errText), channel.MessageFormatPlain)
	default:
		return nil
	}
}

// Finish ends the reply. The room shows no placeholder, so a nil final adds
// nothing. A final without a message sends the text streamed so far.
func (s *matrixPreview) Finish(ctx context.Context, final *channel.PreparedStreamEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.end() {
		return nil
	}
	if final == nil || final.Final == nil {
		return nil
	}

	finalMsg := final.Final.Message.Message
	if matrixMessageBody(finalMsg) == "" {
		finalMsg.Text = s.bufferedText()
		if finalMsg.Format == "" {
			finalMsg.Format = channel.MessageFormatPlain
		}
	}
	if err := s.upsertMessage(ctx, finalMsg); err != nil {
		return err
	}
	return s.pushAttachments(ctx, final.Final.Message.Attachments)
}

// Abort ends the reply as it stands.
func (s *matrixPreview) Abort(context.Context) error {
	s.end()
	return nil
}

func (s *matrixPreview) finished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

// end marks the preview ended and reports whether this call did it.
func (s *matrixPreview) end() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	s.ended = true
	return true
}

func (s *matrixPreview) bufferedText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.rawBuffer.String())
}

func (s *matrixPreview) upsertText(ctx context.Context, text string, format channel.MessageFormat) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if format == "" {
		format = channel.MessageFormatPlain
	}
	return s.upsertMessage(ctx, channel.Message{Text: text, Format: format})
}

func (s *matrixPreview) upsertMessage(ctx context.Context, msg channel.Message) error {
	text := matrixMessageBody(msg)
	if text == "" {
		return nil
	}
	format := msg.Format
	if format == "" {
		if len(msg.Parts) > 0 {
			format = channel.MessageFormatRich
		} else {
			format = channel.MessageFormatPlain
		}
		msg.Format = format
	}

	s.mu.Lock()
	roomID := s.roomID
	originalEventID := s.originalEventID
	lastText := s.lastText
	lastFormat := s.lastFormat
	reply := s.reply
	s.mu.Unlock()

	if roomID == "" {
		resolvedRoomID, err := s.adapter.resolveRoomTarget(ctx, s.cfg, s.target)
		if err != nil {
			return err
		}
		roomID = resolvedRoomID
		s.mu.Lock()
		s.roomID = resolvedRoomID
		s.mu.Unlock()
	}

	if originalEventID == "" {
		if msg.Reply == nil {
			msg.Reply = reply
		}
		eventID, err := s.adapter.sendTextEvent(ctx, s.cfg, roomID, buildMatrixMessageContent(msg, false, ""))
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.originalEventID = eventID
		s.lastText = text
		s.lastFormat = format
		s.mu.Unlock()
		return nil
	}

	if text == lastText && format == lastFormat {
		return nil
	}
	msg.Reply = nil
	_, err := s.adapter.sendTextEvent(ctx, s.cfg, roomID, buildMatrixMessageContent(msg, true, originalEventID))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.lastText = text
	s.lastFormat = format
	s.mu.Unlock()
	return nil
}

// sendToolCallMessage posts a room event on tool_call_start and sends an
// m.replace edit event on tool_call_end so both lifecycle states share a
// single visible message. If no prior event is tracked (or the edit fails),
// it falls back to creating a new event.
func (s *matrixPreview) sendToolCallMessage(
	ctx context.Context,
	tc *channel.StreamToolCall,
	p channel.ToolCallPresentation,
) error {
	text := strings.TrimSpace(channel.RenderToolCallMessageMarkdown(p))
	format := channel.MessageFormatMarkdown
	if text == "" {
		text = strings.TrimSpace(channel.RenderToolCallMessage(p))
		format = channel.MessageFormatPlain
	}
	if text == "" {
		return nil
	}

	s.mu.Lock()
	roomID := s.roomID
	reply := s.reply
	s.mu.Unlock()

	if roomID == "" {
		resolved, err := s.adapter.resolveRoomTarget(ctx, s.cfg, s.target)
		if err != nil {
			return err
		}
		roomID = resolved
		s.mu.Lock()
		s.roomID = roomID
		s.mu.Unlock()
	}

	callID := ""
	if tc != nil {
		callID = strings.TrimSpace(tc.CallID)
	}
	if p.Status != channel.ToolCallStatusRunning && callID != "" {
		if eventID, ok := s.lookupToolCallMessage(callID); ok {
			editMsg := channel.Message{Text: text, Format: format}
			if _, err := s.adapter.sendTextEvent(ctx, s.cfg, roomID, buildMatrixMessageContent(editMsg, true, eventID)); err == nil {
				s.forgetToolCallMessage(callID)
				return nil
			}
			s.forgetToolCallMessage(callID)
		}
	}
	msg := channel.Message{
		Text:   text,
		Format: format,
		Reply:  reply,
	}
	eventID, err := s.adapter.sendTextEvent(ctx, s.cfg, roomID, buildMatrixMessageContent(msg, false, ""))
	if err != nil {
		return err
	}
	if p.Status == channel.ToolCallStatusRunning && callID != "" && eventID != "" {
		s.storeToolCallMessage(callID, eventID)
	}
	return nil
}

func (s *matrixPreview) lookupToolCallMessage(callID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.toolMessages == nil {
		return "", false
	}
	v, ok := s.toolMessages[callID]
	return v, ok
}

func (s *matrixPreview) storeToolCallMessage(callID, eventID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.toolMessages == nil {
		s.toolMessages = make(map[string]string)
	}
	s.toolMessages[callID] = eventID
}

func (s *matrixPreview) forgetToolCallMessage(callID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.toolMessages == nil {
		return
	}
	delete(s.toolMessages, callID)
}

func (s *matrixPreview) resetMessageState() {
	s.mu.Lock()
	s.originalEventID = ""
	s.rawBuffer.Reset()
	s.lastText = ""
	s.lastFormat = ""
	s.mu.Unlock()
}

func (s *matrixPreview) pushAttachments(ctx context.Context, attachments []channel.PreparedAttachment) error {
	if len(attachments) == 0 {
		return nil
	}

	s.mu.Lock()
	roomID := s.roomID
	originalEventID := s.originalEventID
	reply := s.reply
	s.mu.Unlock()

	if roomID == "" {
		resolvedRoomID, err := s.adapter.resolveRoomTarget(ctx, s.cfg, s.target)
		if err != nil {
			return err
		}
		roomID = resolvedRoomID
		s.mu.Lock()
		s.roomID = resolvedRoomID
		s.mu.Unlock()
	}

	for idx, att := range attachments {
		mediaMsg := channel.Message{}
		if idx == 0 && originalEventID == "" {
			mediaMsg.Reply = reply
		}
		if err := s.adapter.sendMediaAttachment(ctx, s.cfg, roomID, mediaMsg, att); err != nil {
			return err
		}
	}
	return nil
}
