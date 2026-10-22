// Package channelqueue adapts channel queue controls to the application
// service without exposing queue storage types to channels or RPC.
package channelqueue

import (
	"context"
	"strings"

	"github.com/felinics/memoh/internal/agent"
	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/agentapi/queueerr"
	"github.com/felinics/memoh/internal/channel/inbound"
)

type Adapter struct{ service *agent.Service }

func New(service *agent.Service) *Adapter { return &Adapter{service: service} }

func (a *Adapter) EnqueueSteer(ctx context.Context, input inbound.QueueCommandInput) error {
	return a.enqueue(input, func(queued agentapi.QueueInput) error {
		_, err := a.service.EnqueueSteer(ctx, queued)
		return err
	})
}

func (a *Adapter) EnqueueFollowUp(ctx context.Context, input inbound.QueueCommandInput) error {
	return a.enqueue(input, func(queued agentapi.QueueInput) error {
		_, err := a.service.EnqueueFollowUp(ctx, queued)
		return err
	})
}

func (a *Adapter) enqueue(input inbound.QueueCommandInput, admit func(agentapi.QueueInput) error) error {
	if a == nil || a.service == nil {
		return inbound.NewQueueCommandError(inbound.QueueCommandCodeUnavailable)
	}
	if strings.TrimSpace(input.BotID) == "" || strings.TrimSpace(input.SessionID) == "" ||
		strings.TrimSpace(input.InvocationID) == "" || strings.TrimSpace(input.Text) == "" {
		return inbound.NewQueueCommandError(inbound.QueueCommandCodeInvalid)
	}
	return mapAdmissionError(admit(agentapi.QueueInput{
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
var admissionCodes = map[queueerr.QueueAdmissionFailure]string{
	queueerr.QueueAdmissionSteerUnsupported:   inbound.QueueCommandCodeUnsupported,
	queueerr.QueueAdmissionNoActiveRun:        inbound.QueueCommandCodeNoActiveRun,
	queueerr.QueueAdmissionInvocationConflict: inbound.QueueCommandCodeConflict,
	queueerr.QueueAdmissionOverloaded:         inbound.QueueCommandCodeOverloaded,
	queueerr.QueueAdmissionCapacityExceeded:   inbound.QueueCommandCodeCapacity,
	queueerr.QueueAdmissionInvalidReference:   inbound.QueueCommandCodeInvalid,
	queueerr.QueueAdmissionUnavailable:        inbound.QueueCommandCodeUnavailable,
}

func mapAdmissionError(err error) error {
	code, ok := admissionCodes[queueerr.QueueAdmissionFailureOf(err)]
	if !ok {
		return err
	}
	return inbound.NewQueueCommandError(code)
}

var _ inbound.QueueCommandHandler = (*Adapter)(nil)
