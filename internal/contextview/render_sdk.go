package contextview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

type SDKRenderedPayload struct {
	System                string
	Messages              []sdk.Message
	Query                 string
	InlineImages          []sdk.ImagePart
	lastMessageMergeClass string
}

type SDKMessagesRenderer struct{}

func (*SDKMessagesRenderer) Target() contextfrag.RenderTarget {
	return contextfrag.RenderSDKMessages
}

func (*SDKMessagesRenderer) Render(_ context.Context, input RenderInput) (RenderedPayload, error) {
	ordered, err := orderedSelectedFrags(input.Selected, input.Placement)
	if err != nil {
		return RenderedPayload{}, err
	}

	payload := &SDKRenderedPayload{}
	var previousSystemRender contextfrag.RenderPolicy
	hasSystemFrag := false
	for _, frag := range sortSystemFragsByPriority(ordered) {
		switch frag.Slot {
		case contextfrag.SlotSystem:
			renderSystemFrag(payload, frag, previousSystemRender, hasSystemFrag)
			previousSystemRender = frag.Render
			hasSystemFrag = true
		case contextfrag.SlotCurrentUser:
			renderCurrentUserFrag(payload, frag)
		default:
			renderMessageFrag(payload, frag)
		}
	}

	contentHash, err := sdkRenderedPayloadHash(payload)
	if err != nil {
		return RenderedPayload{}, err
	}
	return RenderedPayload{
		Target:      contextfrag.RenderSDKMessages,
		ContentHash: contentHash,
		Data:        payload,
	}, nil
}

func orderedSelectedFrags(selected []contextfrag.ContextFrag, placement PlacementPlan) ([]contextfrag.ContextFrag, error) {
	if len(placement.Items) == 0 {
		if len(selected) > 0 {
			return nil, fmt.Errorf("placement is empty for %d selected fragments", len(selected))
		}
		return nil, nil
	}
	if len(placement.Items) != len(selected) {
		return nil, fmt.Errorf("placement item count %d does not match selected fragment count %d", len(placement.Items), len(selected))
	}
	byID := make(map[string]contextfrag.ContextFrag, len(selected))
	for _, frag := range selected {
		if _, ok := byID[frag.ID]; ok {
			return nil, fmt.Errorf("selected fragments contain duplicate id %q", frag.ID)
		}
		byID[frag.ID] = frag
	}
	ordered := make([]contextfrag.ContextFrag, 0, len(placement.Items))
	seenPlacement := make(map[string]bool, len(placement.Items))
	for _, item := range placement.Items {
		if seenPlacement[item.FragID] {
			return nil, fmt.Errorf("placement contains duplicate fragment %q", item.FragID)
		}
		seenPlacement[item.FragID] = true
		frag, ok := byID[item.FragID]
		if !ok {
			return nil, fmt.Errorf("placement references unknown fragment %q", item.FragID)
		}
		ordered = append(ordered, frag)
	}
	return ordered, nil
}

// renderSystemFrag appends a system-slot fragment's text to the payload.
// Every selected system fragment counts as one section boundary for the
// "\n\n" join, including a fragment whose text happens to be empty (such as
// an always-present-but-currently-blank bot-identity section), so rendering
// several system fragments reproduces the same spacing as joining their
// source sections into one string would (see renderSystemSections).
func renderSystemFrag(
	payload *SDKRenderedPayload,
	frag contextfrag.ContextFrag,
	previous contextfrag.RenderPolicy,
	hasPrevious bool,
) {
	if hasPrevious {
		payload.System += contextfrag.RenderSeparator(previous, frag.Render)
	}
	for _, part := range frag.Parts {
		if part.Type != contextfrag.PartText {
			continue
		}
		payload.System += contextfrag.RenderText(part.Text, frag.Render)
	}
}

func renderCurrentUserFrag(payload *SDKRenderedPayload, frag contextfrag.ContextFrag) {
	for _, part := range frag.Parts {
		switch part.Type {
		case contextfrag.PartText:
			if text := strings.TrimSpace(part.Text); text != "" {
				payload.Query = text
			}
		case contextfrag.PartImage:
			if image := sdkImagePart(part); image != nil && strings.TrimSpace(image.Image) != "" {
				payload.InlineImages = append(payload.InlineImages, *image)
			}
		case contextfrag.PartSDKMessage:
			if msg := sdkMessagePart(part); msg != nil {
				payload.Messages = append(payload.Messages, cloneSDKMessage(*msg))
			}
		}
	}
}

func renderMessageFrag(payload *SDKRenderedPayload, frag contextfrag.ContextFrag) {
	for _, part := range frag.Parts {
		if part.Type != contextfrag.PartSDKMessage {
			continue
		}
		if msg := sdkMessagePart(part); msg != nil {
			appendRenderedMessage(payload, frag, *msg)
		}
	}
}

func appendRenderedMessage(payload *SDKRenderedPayload, frag contextfrag.ContextFrag, msg sdk.Message) {
	if mergeDiscussRCMessage(payload, frag, msg) {
		return
	}
	payload.Messages = append(payload.Messages, cloneSDKMessage(msg))
	payload.lastMessageMergeClass = messageMergeClass(frag)
}

func mergeDiscussRCMessage(payload *SDKRenderedPayload, frag contextfrag.ContextFrag, msg sdk.Message) bool {
	if messageMergeClass(frag) != "discuss_rc" ||
		payload.lastMessageMergeClass != "discuss_rc" ||
		msg.Role != sdk.MessageRoleUser ||
		len(payload.Messages) == 0 {
		return false
	}
	last := &payload.Messages[len(payload.Messages)-1]
	if last.Role != sdk.MessageRoleUser {
		return false
	}
	text, ok := singleTextContent(msg)
	if !ok {
		return false
	}
	lastText, ok := singleTextContent(*last)
	if !ok {
		return false
	}
	last.Content = []sdk.MessagePart{sdk.TextPart{Text: lastText + "\n" + text}}
	return true
}

func messageMergeClass(frag contextfrag.ContextFrag) string {
	if frag.Provenance.Source == discussContextSource &&
		frag.Provenance.Collector == discussContextCollectorName &&
		strings.HasPrefix(frag.Provenance.SourceID, "rc.") {
		return "discuss_rc"
	}
	return ""
}

func singleTextContent(msg sdk.Message) (string, bool) {
	if len(msg.Content) != 1 {
		return "", false
	}
	textPart, ok := msg.Content[0].(sdk.TextPart)
	if !ok {
		return "", false
	}
	return textPart.Text, true
}

func sdkMessagePart(part contextfrag.Part) *sdk.Message {
	if part.SDKMessage != nil {
		return part.SDKMessage
	}
	return part.Message
}

func sdkImagePart(part contextfrag.Part) *sdk.ImagePart {
	if part.SDKImage != nil {
		return part.SDKImage
	}
	return part.ImagePart
}

func cloneSDKMessage(msg sdk.Message) sdk.Message {
	out := msg
	if len(msg.Content) > 0 {
		out.Content = append([]sdk.MessagePart(nil), msg.Content...)
	}
	return out
}

func sdkRenderedPayloadHash(payload *SDKRenderedPayload) (string, error) {
	data, err := json.Marshal(struct {
		System       string          `json:"system"`
		Messages     []sdk.Message   `json:"messages"`
		Query        string          `json:"query"`
		InlineImages []sdk.ImagePart `json:"inline_images"`
	}{
		System:       payload.System,
		Messages:     payload.Messages,
		Query:        payload.Query,
		InlineImages: payload.InlineImages,
	})
	if err != nil {
		return "", fmt.Errorf("marshal sdk rendered payload: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// sortSystemFragsByPriority stably reorders only the system-slot fragments by
// priority so late-collected system sources (agent tool usage) render at
// their declared position; message ordering is untouched.
func sortSystemFragsByPriority(ordered []contextfrag.ContextFrag) []contextfrag.ContextFrag {
	systemIdx := make([]int, 0, 4)
	for i, frag := range ordered {
		if frag.Slot == contextfrag.SlotSystem {
			systemIdx = append(systemIdx, i)
		}
	}
	if len(systemIdx) < 2 {
		return ordered
	}
	systemFrags := make([]contextfrag.ContextFrag, 0, len(systemIdx))
	for _, i := range systemIdx {
		systemFrags = append(systemFrags, ordered[i])
	}
	sort.SliceStable(systemFrags, func(a, b int) bool {
		return systemFrags[a].Priority < systemFrags[b].Priority
	})
	out := make([]contextfrag.ContextFrag, len(ordered))
	copy(out, ordered)
	for pos, i := range systemIdx {
		out[i] = systemFrags[pos]
	}
	return out
}
