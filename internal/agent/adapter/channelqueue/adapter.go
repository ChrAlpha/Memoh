// Package channelqueue adapts channel queue controls to the application
// service without exposing queue storage types to channels or RPC.
package channelqueue

import (
	"context"
	"strings"

	"github.com/felinics/memoh/internal/agent"
	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/channel/inbound"
)

type Adapter struct{ service *agent.Service }

func New(service *agent.Service) *Adapter { return &Adapter{service: service} }

func (a *Adapter) EnqueueSteer(ctx context.Context, input inbound.QueueCommandInput) error {
	return a.enqueue(input, func(queued pending.QueueInput) error {
		_, err := a.service.EnqueueSteer(ctx, queued)
		return err
	})
}

func (a *Adapter) EnqueueFollowUp(ctx context.Context, input inbound.QueueCommandInput) error {
	return a.enqueue(input, func(queued pending.QueueInput) error {
		_, err := a.service.EnqueueFollowUp(ctx, queued)
		return err
	})
}

func (a *Adapter) enqueue(input inbound.QueueCommandInput, admit func(pending.QueueInput) error) error {
	if a == nil || a.service == nil {
		return inbound.NewQueueCommandError(inbound.QueueCommandCodeUnavailable)
	}
	if strings.TrimSpace(input.BotID) == "" || strings.TrimSpace(input.SessionID) == "" ||
		strings.TrimSpace(input.InvocationID) == "" || strings.TrimSpace(input.Text) == "" {
		return inbound.NewQueueCommandError(inbound.QueueCommandCodeInvalid)
	}
	return mapAdmissionError(admit(pending.QueueInput{
		TeamID:                  input.TeamID,
		BotID:                   input.BotID,
		SessionID:               input.SessionID,
		InvocationID:            input.InvocationID,
		UserID:                  input.UserID,
		SourceChannelIdentityID: input.ChannelIdentityID,
		Text:                    input.Text,
	}))
}

// admissionCodes renders queue admission refusals in the channel command
// vocabulary, which crosses the split-runtime RPC and is not the HTTP one.
var admissionCodes = map[pending.QueueAdmissionFailure]string{
	pending.QueueAdmissionSteerUnsupported:   inbound.QueueCommandCodeUnsupported,
	pending.QueueAdmissionNoActiveRun:        inbound.QueueCommandCodeNoActiveRun,
	pending.QueueAdmissionInvocationConflict: inbound.QueueCommandCodeConflict,
	pending.QueueAdmissionOverloaded:         inbound.QueueCommandCodeOverloaded,
	pending.QueueAdmissionCapacityExceeded:   inbound.QueueCommandCodeCapacity,
	pending.QueueAdmissionInvalidReference:   inbound.QueueCommandCodeInvalid,
	pending.QueueAdmissionUnavailable:        inbound.QueueCommandCodeUnavailable,
}

func mapAdmissionError(err error) error {
	code, ok := admissionCodes[pending.QueueAdmissionFailureOf(err)]
	if !ok {
		return err
	}
	return inbound.NewQueueCommandError(code)
}

var _ inbound.QueueCommandHandler = (*Adapter)(nil)
