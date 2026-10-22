package admit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/internal/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	chatview "github.com/felinics/memoh/internal/agent/view"
)

// Turn puts a StartTurnCommand through durable admission and answers in
// the vocabulary of the turn port, which is the only agent surface its callers
// can see.
//
// Three answers matter to a channel adapter, and they are deliberately distinct.
// Busy means nothing was persisted and a redelivery will be admitted once the
// thread frees up. Duplicate means this invocation already has a run, so the
// redelivery has been answered and must be dropped. Anything else is a genuine
// failure to report.
func (a *Admitter) Turn(
	ctx context.Context,
	cmd turn.StartTurnCommand,
	cancel context.CancelFunc,
	ownershipCancel context.CancelCauseFunc,
	injectCh chan turn.InjectMessage,
) (sessionruntime.Admission, error) {
	payload, err := turnSubmissionPayload(cmd)
	if err != nil {
		return sessionruntime.Admission{}, fmt.Errorf("encode turn submission: %w", err)
	}
	invocationID := turnInvocationID(cmd)

	admission, err := a.runtime.Admit(ctx, sessionruntime.AdmitInput{
		BotID:        cmd.BotID,
		SessionID:    cmd.ThreadID,
		InvocationID: invocationID,
		InitiatorID:  runreq.FirstNonEmpty(cmd.UserID, cmd.SourceChannelIdentityID),
		Payload:      payload,
		Execution: sessionruntime.Execution{
			// Project the inbound user message so subscribers (an open web
			// session on the thread) see what fired the run while it still
			// executes — the same contract the ws and schedule admissions
			// already honor. NewRequestUserTurn returns nil for commands that
			// persist no user message (attachment-only), and
			// those runs stay contentless in the projection.
			Admission: func(_ context.Context, handle sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
				return sessionruntime.RunAdmissionView{
					RequestUserTurn: chatview.NewRequestUserTurn(cmd, handle.TurnID),
				}, nil
			},
			Cancel:          cancel,
			InjectCh:        injectCh,
			OwnershipCancel: ownershipCancel,
		},
	})
	switch {
	case errors.Is(err, sessionruntime.ErrSessionBusy):
		return sessionruntime.Admission{}, fmt.Errorf("%w: thread %s", turn.ErrSessionBusy, cmd.ThreadID)
	case errors.Is(err, sessionruntime.ErrInvocationConflict):
		// The same retry identity naming different content. Whichever side is
		// wrong, running it would double-answer a message that already has a
		// run, so it is dropped exactly like a redelivery.
		return sessionruntime.Admission{}, fmt.Errorf("%w: %s: %w", turn.ErrDuplicateTurn, invocationID, sessionruntime.ErrInvocationConflict)
	case err != nil:
		return sessionruntime.Admission{}, fmt.Errorf("admit turn: %w", err)
	}
	if !admission.Started {
		// Someone owns this run already, or it has finished. Either way this
		// call has no execution to perform and the caller has its answer.
		return sessionruntime.Admission{}, fmt.Errorf("%w: %s", turn.ErrDuplicateTurn, invocationID)
	}
	return admission, nil
}

// turnInvocationID resolves the command's retry identity.
//
// Channel adapters derive IdempotencyKey from the platform's external message
// id, which is what makes a webhook redelivery the same invocation rather than a
// second turn. When the platform offers nothing stable there is nothing to
// deduplicate against, so each attempt is honestly its own submission — minting
// here keeps that explicit instead of letting an empty id reach admission.
func turnInvocationID(cmd turn.StartTurnCommand) string {
	if key := strings.TrimSpace(cmd.IdempotencyKey); key != "" {
		return key
	}
	return "turn:" + uuid.NewString()
}

// turnSubmissionPayload encodes what the caller actually submitted, and only
// that. The fingerprint of this payload decides whether a repeated invocation id
// is the same submission or a conflict, so it must not carry anything that
// varies between two deliveries of one message: no timestamps, no tokens, no
// per-attempt ids, and attachments by content hash rather than by bytes.
func turnSubmissionPayload(cmd turn.StartTurnCommand) ([]byte, error) {
	return json.Marshal(struct {
		Mode              string   `json:"mode"`
		BotID             string   `json:"bot_id"`
		ThreadID          string   `json:"thread_id"`
		ExternalMessageID string   `json:"external_message_id,omitempty"`
		Query             string   `json:"query,omitempty"`
		UserVisibleText   string   `json:"user_visible_text,omitempty"`
		UserMessageKind   string   `json:"user_message_kind,omitempty"`
		Attachments       []string `json:"attachments,omitempty"`
	}{
		Mode:              string(cmd.Mode),
		BotID:             strings.TrimSpace(cmd.BotID),
		ThreadID:          strings.TrimSpace(cmd.ThreadID),
		ExternalMessageID: strings.TrimSpace(cmd.ExternalMessageID),
		Query:             cmd.Query,
		UserVisibleText:   cmd.UserVisibleText,
		UserMessageKind:   strings.TrimSpace(cmd.UserMessageKind),
		Attachments:       attachmentIdentities(cmd.Attachments),
	})
}

// attachmentIdentities names each attachment by the most stable identity it
// carries. Base64 bytes are deliberately not used: the same upload can arrive
// re-encoded, and the payload is a fingerprint input rather than a copy.
func attachmentIdentities(attachments []turn.Attachment) []string {
	if len(attachments) == 0 {
		return nil
	}
	out := make([]string, 0, len(attachments))
	for _, a := range attachments {
		switch {
		case strings.TrimSpace(a.ContentHash) != "":
			out = append(out, "hash:"+strings.TrimSpace(a.ContentHash))
		case strings.TrimSpace(a.PlatformKey) != "":
			out = append(out, "platform:"+strings.TrimSpace(a.PlatformKey))
		case strings.TrimSpace(a.URL) != "":
			out = append(out, "url:"+strings.TrimSpace(a.URL))
		case strings.TrimSpace(a.Path) != "":
			out = append(out, "path:"+strings.TrimSpace(a.Path))
		default:
			out = append(out, "type:"+strings.TrimSpace(a.Type))
		}
	}
	return out
}
