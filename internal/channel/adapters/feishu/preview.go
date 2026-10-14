package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/felinics/memoh/internal/channel"
)

const (
	feishuStreamThinkingText  = "Thinking..."
	feishuStreamToolHintText  = "Calling tools..."
	feishuStreamPatchInterval = 700 * time.Millisecond
	feishuStreamMaxRunes      = 8000
)

// feishuPreview shows one reply as an interactive card that is patched while
// the reply is produced. The card opens as a placeholder and takes the streamed
// text, then the final text.
type feishuPreview struct {
	adapter       *FeishuAdapter
	cfg           channel.ChannelConfig
	target        string
	reply         *channel.ReplyRef
	client        *lark.Client
	receiveID     string
	receiveType   string
	cardMessageID string
	placeholder   bool
	textBuffer    strings.Builder
	lastPatchedAt time.Time
	lastPatched   string
	patchInterval time.Duration
	// sendMessage delivers the attachments of the final as messages of their own.
	sendMessage  func(ctx context.Context, cfg channel.ChannelConfig, msg channel.PreparedOutboundMessage) error
	ended        bool
	toolMessages map[string]string
}

var _ channel.PreparedPreview = (*feishuPreview)(nil)

func (s *feishuPreview) Update(ctx context.Context, event channel.PreparedStreamEvent) error {
	if s.ended {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	switch event.Type {
	case channel.StreamEventStatus:
		if event.Status == channel.StreamStatusStarted {
			return s.ensureCard(ctx, feishuStreamThinkingText)
		}
		return nil
	case channel.StreamEventDelta:
		if event.Delta == "" || event.Phase == channel.StreamPhaseReasoning {
			return nil
		}
		s.textBuffer.WriteString(event.Delta)
		if err := s.ensureCard(ctx, feishuStreamThinkingText); err != nil {
			return err
		}
		if time.Since(s.lastPatchedAt) < s.patchInterval && !strings.Contains(event.Delta, "\n") {
			return nil
		}
		return s.patchCard(ctx, s.textBuffer.String())
	case channel.StreamEventReset:
		// Keep the card; the regenerated reply patches over the failed text.
		s.textBuffer.Reset()
		return nil
	case channel.StreamEventToolCallStart:
		bufText := strings.TrimSpace(s.textBuffer.String())
		if s.cardMessageID != "" && bufText != "" {
			_ = s.patchCard(ctx, bufText)
		}
		s.resetCard()
		return s.renderToolCallCard(ctx, event.ToolCall, channel.BuildToolCallStart(event.ToolCall))
	case channel.StreamEventToolCallEnd:
		s.resetCard()
		return s.renderToolCallCard(ctx, event.ToolCall, channel.BuildToolCallEnd(event.ToolCall))
	case channel.StreamEventPhaseStart, channel.StreamEventPhaseEnd:
		return nil
	case channel.StreamEventAgentStart, channel.StreamEventAgentEnd, channel.StreamEventProcessingStarted, channel.StreamEventProcessingCompleted, channel.StreamEventProcessingFailed:
		return nil
	case channel.StreamEventError:
		errText := strings.TrimSpace(event.Error)
		if errText == "" {
			return nil
		}
		if err := s.ensureCard(ctx, feishuStreamThinkingText); err != nil {
			return err
		}
		return s.patchCard(ctx, channel.ErrorReplyText(event.ErrorCode, errText))
	default:
		return nil
	}
}

// Finish ends the reply. A nil final means the run said nothing: a card that
// still shows only the placeholder is recalled. A final without a message
// leaves the text streamed so far as the reply.
func (s *feishuPreview) Finish(ctx context.Context, final *channel.PreparedStreamEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ended {
		return nil
	}
	s.ended = true
	if final == nil || final.Final == nil {
		return s.clearPlaceholder(ctx)
	}

	msg := final.Final.Message
	bufText := strings.TrimSpace(s.textBuffer.String())
	finalText := bufText
	if !msg.Message.IsEmpty() {
		finalText = renderFeishuStreamFinalText(msg.Message, bufText)
	}
	if finalText != "" {
		if err := s.ensureCard(ctx, feishuStreamThinkingText); err != nil {
			return err
		}
		if err := s.patchCard(ctx, finalText); err != nil {
			return err
		}
	} else if err := s.clearPlaceholder(ctx); err != nil {
		return err
	}
	if len(msg.Attachments) > 0 {
		media := msg
		media.Message.Format = ""
		media.Message.Text = ""
		media.Message.Parts = nil
		media.Message.Actions = nil
		media.Message.Reply = nil
		media.Message.Attachments = feishuLogicalAttachments(media.Attachments)
		return s.sendMessage(ctx, s.cfg, channel.PreparedOutboundMessage{
			Target:  s.target,
			Message: media,
		})
	}
	return nil
}

// Abort ends the reply as it stands.
func (s *feishuPreview) Abort(context.Context) error {
	s.ended = true
	return nil
}

// resetCard forgets the open card, so the next text opens a new one.
func (s *feishuPreview) resetCard() {
	s.cardMessageID = ""
	s.placeholder = false
	s.lastPatched = ""
	s.lastPatchedAt = time.Time{}
	s.textBuffer.Reset()
}

func renderFeishuStreamFinalText(msg channel.Message, buffered string) string {
	if len(msg.Parts) > 0 {
		if body := renderFeishuMessagePartsLarkMD(msg); body != "" {
			if len([]rune(body)) > feishuStreamMaxRunes {
				return channel.RenderPartsAsPlain(msg.Parts)
			}
			return body
		}
	}
	if authoritative := strings.TrimSpace(msg.PlainText()); authoritative != "" {
		return authoritative
	}
	return strings.TrimSpace(buffered)
}

func feishuLogicalAttachments(attachments []channel.PreparedAttachment) []channel.Attachment {
	if len(attachments) == 0 {
		return nil
	}
	logical := make([]channel.Attachment, 0, len(attachments))
	for _, att := range attachments {
		logical = append(logical, att.Logical)
	}
	return logical
}

func (s *feishuPreview) ensureCard(ctx context.Context, text string) error {
	if strings.TrimSpace(s.cardMessageID) != "" {
		return nil
	}
	if s.client == nil {
		return errors.New("feishu client not configured")
	}
	content, err := buildFeishuStreamCardContent(text)
	if err != nil {
		return err
	}
	if s.reply != nil && strings.TrimSpace(s.reply.MessageID) != "" {
		replyReq := larkim.NewReplyMessageReqBuilder().
			MessageId(strings.TrimSpace(s.reply.MessageID)).
			Body(larkim.NewReplyMessageReqBodyBuilder().
				Content(content).
				MsgType(larkim.MsgTypeInteractive).
				Uuid(uuid.NewString()).
				Build()).
			Build()
		replyResp, err := s.client.Im.Message.Reply(ctx, replyReq)
		if err != nil {
			return err
		}
		if replyResp == nil || !replyResp.Success() {
			code, msg := 0, ""
			if replyResp != nil {
				code, msg = replyResp.Code, replyResp.Msg
			}
			return fmt.Errorf("feishu stream reply failed: %s (code: %d)", msg, code)
		}
		if replyResp.Data == nil || replyResp.Data.MessageId == nil || strings.TrimSpace(*replyResp.Data.MessageId) == "" {
			return errors.New("feishu stream reply failed: empty message id")
		}
		s.cardMessageID = strings.TrimSpace(*replyResp.Data.MessageId)
		s.placeholder = true
		s.lastPatched = normalizeFeishuStreamText(text)
		s.lastPatchedAt = time.Now()
		return nil
	}
	createReq := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(s.receiveType).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(s.receiveID).
			MsgType(larkim.MsgTypeInteractive).
			Content(content).
			Uuid(uuid.NewString()).
			Build()).
		Build()
	createResp, err := s.client.Im.Message.Create(ctx, createReq)
	if err != nil {
		return err
	}
	if createResp == nil || !createResp.Success() {
		code, msg := 0, ""
		if createResp != nil {
			code, msg = createResp.Code, createResp.Msg
		}
		return fmt.Errorf("feishu stream create failed: %s (code: %d)", msg, code)
	}
	if createResp.Data == nil || createResp.Data.MessageId == nil || strings.TrimSpace(*createResp.Data.MessageId) == "" {
		return errors.New("feishu stream create failed: empty message id")
	}
	s.cardMessageID = strings.TrimSpace(*createResp.Data.MessageId)
	s.placeholder = true
	s.lastPatched = normalizeFeishuStreamText(text)
	s.lastPatchedAt = time.Now()
	return nil
}

func (s *feishuPreview) patchCard(ctx context.Context, text string) error {
	if strings.TrimSpace(s.cardMessageID) == "" {
		return errors.New("feishu stream card message not initialized")
	}
	contentText := normalizeFeishuStreamText(text)
	if contentText == s.lastPatched {
		return nil
	}
	content, err := buildFeishuStreamCardContent(contentText)
	if err != nil {
		return err
	}
	patchReq := larkim.NewPatchMessageReqBuilder().
		MessageId(strings.TrimSpace(s.cardMessageID)).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(content).
			Build()).
		Build()
	patchResp, err := s.client.Im.Message.Patch(ctx, patchReq)
	if err != nil {
		return err
	}
	if patchResp == nil || !patchResp.Success() {
		code, msg := 0, ""
		if patchResp != nil {
			code, msg = patchResp.Code, patchResp.Msg
		}
		return fmt.Errorf("feishu stream patch failed: %s (code: %d)", msg, code)
	}
	s.placeholder = false
	s.lastPatched = contentText
	s.lastPatchedAt = time.Now()
	return nil
}

// clearPlaceholder recalls the card while it still shows only the placeholder.
func (s *feishuPreview) clearPlaceholder(ctx context.Context) error {
	if !s.placeholder || strings.TrimSpace(s.cardMessageID) == "" {
		return nil
	}
	resp, err := s.client.Im.Message.Delete(ctx, larkim.NewDeleteMessageReqBuilder().
		MessageId(strings.TrimSpace(s.cardMessageID)).
		Build())
	if err != nil {
		return err
	}
	if resp == nil || !resp.Success() {
		code, msg := 0, ""
		if resp != nil {
			code, msg = resp.Code, resp.Msg
		}
		return fmt.Errorf("feishu stream placeholder recall failed: %s (code: %d)", msg, code)
	}
	s.resetCard()
	return nil
}

// extractReadableFromJSON tries to extract human-readable text from JSON-like content.
// Returns the original text if not JSON or extraction fails.
func extractReadableFromJSON(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text
	}
	first := strings.TrimLeft(trimmed, " \t\n\r")
	if (len(first) > 0 && first[0] != '{' && first[0] != '[') || len(first) < 2 {
		return text
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		var arr []any
		if err := json.Unmarshal([]byte(trimmed), &arr); err != nil {
			return text
		}
		if len(arr) == 0 {
			return text
		}
		if s, ok := arr[0].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
		return text
	}
	for _, key := range []string{"text", "message", "content", "result", "output", "response", "answer"} {
		if v, ok := raw[key]; ok && v != nil {
			switch val := v.(type) {
			case string:
				if strings.TrimSpace(val) != "" {
					return val
				}
			case map[string]any:
				if b, err := json.Marshal(val); err == nil {
					return string(b)
				}
			}
		}
	}
	return text
}

func buildFeishuCardContent(text string) (string, error) {
	return buildFeishuCardContentWithTag(text, "lark_md")
}

func buildFeishuPlainCardContent(text string) (string, error) {
	return buildFeishuCardContentWithTag(text, "plain_text")
}

func buildFeishuCardContentWithTag(text string, tag string) (string, error) {
	if tag == "" {
		tag = "lark_md"
	}
	body := processFeishuCardMarkdown(strings.TrimSpace(text))
	if tag == "plain_text" {
		body = strings.TrimSpace(text)
	}
	card := map[string]any{
		"config": map[string]any{
			"wide_screen_mode": true,
			"enable_forward":   true,
			"update_multi":     true,
		},
		"elements": []map[string]any{
			{
				"tag": "div",
				"fields": []map[string]any{
					{
						"is_short": false,
						"text": map[string]any{
							"tag":     tag,
							"content": body,
						},
					},
				},
			},
		},
	}
	data, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func buildFeishuStreamCardContent(text string) (string, error) {
	content := normalizeFeishuStreamText(extractReadableFromJSON(text))
	return buildFeishuCardContent(content)
}

var feishuCardHeadingPrefix = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)

// processFeishuCardMarkdown normalizes markdown for Feishu card lark_md (e.g. ATX headings to bold).
func processFeishuCardMarkdown(s string) string {
	s = strings.ReplaceAll(s, "\\n", "\n")
	s = feishuCardHeadingPrefix.ReplaceAllStringFunc(s, func(m string) string {
		parts := feishuCardHeadingPrefix.FindStringSubmatch(m)
		if len(parts) == 2 {
			return "**" + parts[1] + "**"
		}
		return m
	})
	return s
}

// renderToolCallCard posts a card for tool_call_start and patches the same
// card on tool_call_end, producing a single message whose status flips from
// "running" to "completed"/"failed". If no prior card is tracked (or patching
// fails), it falls back to creating a new card.
func (s *feishuPreview) renderToolCallCard(
	ctx context.Context,
	tc *channel.StreamToolCall,
	p channel.ToolCallPresentation,
) error {
	text := strings.TrimSpace(channel.RenderToolCallMessageMarkdown(p))
	if text == "" {
		return nil
	}
	if s.client == nil {
		return errors.New("feishu client not configured")
	}
	callID := ""
	if tc != nil {
		callID = strings.TrimSpace(tc.CallID)
	}
	if p.Status != channel.ToolCallStatusRunning && callID != "" {
		if existing, ok := s.lookupToolCallMessage(callID); ok {
			patchErr := s.patchToolCallCard(ctx, existing, text)
			if patchErr == nil {
				s.forgetToolCallMessage(callID)
				return nil
			}
			if s.adapter != nil && s.adapter.logger != nil {
				s.adapter.logger.WarnContext(ctx, "feishu: tool-call end patch failed, falling back to new card",
					slog.String("call_id", callID),
					slog.Any("error", patchErr),
				)
			}
			s.forgetToolCallMessage(callID)
		}
	}
	msgID, err := s.sendToolCallCard(ctx, text)
	if err != nil {
		return err
	}
	if p.Status == channel.ToolCallStatusRunning && callID != "" && msgID != "" {
		s.storeToolCallMessage(callID, msgID)
	}
	return nil
}

func (s *feishuPreview) patchToolCallCard(ctx context.Context, messageID, text string) error {
	content, err := buildFeishuCardContent(text)
	if err != nil {
		return err
	}
	patchReq := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(content).
			Build()).
		Build()
	patchResp, err := s.client.Im.Message.Patch(ctx, patchReq)
	if err != nil {
		return err
	}
	if patchResp == nil || !patchResp.Success() {
		code, msg := 0, ""
		if patchResp != nil {
			code, msg = patchResp.Code, patchResp.Msg
		}
		return fmt.Errorf("feishu tool card patch failed: %s (code: %d)", msg, code)
	}
	return nil
}

func (s *feishuPreview) lookupToolCallMessage(callID string) (string, bool) {
	if s.toolMessages == nil {
		return "", false
	}
	m, ok := s.toolMessages[callID]
	return m, ok
}

func (s *feishuPreview) storeToolCallMessage(callID, messageID string) {
	if s.toolMessages == nil {
		s.toolMessages = make(map[string]string)
	}
	s.toolMessages[callID] = messageID
}

func (s *feishuPreview) forgetToolCallMessage(callID string) {
	if s.toolMessages == nil {
		return
	}
	delete(s.toolMessages, callID)
}

func (s *feishuPreview) sendToolCallCard(ctx context.Context, text string) (string, error) {
	content, err := buildFeishuCardContent(text)
	if err != nil {
		return "", err
	}
	if s.reply != nil && strings.TrimSpace(s.reply.MessageID) != "" {
		replyReq := larkim.NewReplyMessageReqBuilder().
			MessageId(strings.TrimSpace(s.reply.MessageID)).
			Body(larkim.NewReplyMessageReqBodyBuilder().
				Content(content).
				MsgType(larkim.MsgTypeInteractive).
				Uuid(uuid.NewString()).
				Build()).
			Build()
		replyResp, err := s.client.Im.Message.Reply(ctx, replyReq)
		if err != nil {
			return "", err
		}
		if replyResp == nil || !replyResp.Success() {
			code, msg := 0, ""
			if replyResp != nil {
				code, msg = replyResp.Code, replyResp.Msg
			}
			return "", fmt.Errorf("feishu tool card reply failed: %s (code: %d)", msg, code)
		}
		if replyResp.Data == nil || replyResp.Data.MessageId == nil {
			return "", nil
		}
		return strings.TrimSpace(*replyResp.Data.MessageId), nil
	}
	createReq := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(s.receiveType).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(s.receiveID).
			MsgType(larkim.MsgTypeInteractive).
			Content(content).
			Uuid(uuid.NewString()).
			Build()).
		Build()
	createResp, err := s.client.Im.Message.Create(ctx, createReq)
	if err != nil {
		return "", err
	}
	if createResp == nil || !createResp.Success() {
		code, msg := 0, ""
		if createResp != nil {
			code, msg = createResp.Code, createResp.Msg
		}
		return "", fmt.Errorf("feishu tool card create failed: %s (code: %d)", msg, code)
	}
	if createResp.Data == nil || createResp.Data.MessageId == nil {
		return "", nil
	}
	return strings.TrimSpace(*createResp.Data.MessageId), nil
}

func normalizeFeishuStreamText(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return feishuStreamThinkingText
	}
	runes := []rune(trimmed)
	if len(runes) <= feishuStreamMaxRunes {
		return trimmed
	}
	return "...\n" + string(runes[len(runes)-feishuStreamMaxRunes:])
}
