package application

import (
	"context"
	"errors"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
)

type continuationTurnRequestResolverService struct {
	messagepkg.Service
	requestMessageID string
}

func (s *continuationTurnRequestResolverService) GetVisibleHistoryTurnRequestMessageIDByTurn(context.Context, string, string) (string, error) {
	return s.requestMessageID, nil
}

func TestContinuationTurnRequestMessageIDUsesAdmittedTurn(t *testing.T) {
	t.Parallel()

	service := wireComponents(&Service{messageService: &continuationTurnRequestResolverService{
		requestMessageID: "request-message-id",
	}})
	got, err := service.transcript.ContinuationRequestMessageID(
		context.Background(),
		"session-id",
		sessionruntime.RunHandle{TurnID: "turn-id"},
	)
	if err != nil {
		t.Fatalf("continuationTurnRequestMessageID() error = %v", err)
	}
	if got != "request-message-id" {
		t.Fatalf("continuation request message ID = %q, want %q", got, "request-message-id")
	}
}

func TestPrepareContinuationRunConfigReplacesStaleContextAndSetsCapabilities(t *testing.T) {
	t.Parallel()

	resolver := wireComponents(&Service{userInput: &userinput.Service{}})
	staleIndex := 0
	staleMemoryIndex := 0
	base := native.RunConfig{
		Query:                          "stale query",
		Messages:                       []sdk.Message{sdk.UserMessage("stale context")},
		ContextCurrentUserMessageIndex: &staleIndex,
		ContextMemoryMessageIndex:      &staleMemoryIndex,
		ContextSourceFrags: []contextfrag.ContextFrag{{
			ID:   "stale-source-fragment",
			Kind: contextfrag.KindConversationEvent,
		}},
		ContextFrags: []contextfrag.ContextFrag{{
			ID:   "stale-fragment",
			Kind: contextfrag.KindConversationEvent,
		}},
	}

	got, err := resolver.prepareContinuationRunConfig(
		context.Background(),
		base,
		historyfrag.ScopeFallback{},
		contextfrag.Scope{},
		true,
	)
	if err != nil {
		t.Fatalf("prepareContinuationRunConfig() error = %v", err)
	}
	if got.Query != "" || len(got.Messages) != 0 {
		t.Fatalf("continuation retained stale context: %#v", got)
	}
	for _, frag := range got.ContextFrags {
		if frag.ID == "stale-fragment" {
			t.Fatalf("continuation retained stale fragment: %#v", frag)
		}
	}
	if got.ContextCurrentUserMessageIndex != nil {
		t.Fatalf("continuation retained stale current-user index: %#v", got.ContextCurrentUserMessageIndex)
	}
	if got.ContextMemoryMessageIndex != nil {
		t.Fatalf("continuation retained stale memory index: %#v", got.ContextMemoryMessageIndex)
	}
	if got.ContextTrimmableMessages != len(got.Messages) || len(got.ContextHistoryTokenEstimates) != len(got.Messages) {
		t.Fatalf("budget signals = trimmable:%d estimates:%d messages:%d",
			got.ContextTrimmableMessages, len(got.ContextHistoryTokenEstimates), len(got.Messages))
	}
	for _, frag := range got.ContextSourceFrags {
		if frag.ID == "stale-source-fragment" {
			t.Fatalf("continuation retained stale source fragment: %#v", frag)
		}
	}
	if !got.LiveToolStream || !got.CanRequestUserInput {
		t.Fatalf("continuation capabilities = live:%v input:%v, want true/true", got.LiveToolStream, got.CanRequestUserInput)
	}
}

func TestPrepareContinuationRunConfigPropagatesArtifactProjectionFailure(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("artifact projection unavailable")
	resolver := wireComponents(&Service{queries: &agenttest.RecordingCompactionLog{ListErr: sentinel}})
	got, err := resolver.prepareContinuationRunConfig(
		context.Background(),
		native.RunConfig{Query: "must not survive"},
		historyfrag.ScopeFallback{},
		contextfrag.Scope{SessionID: "00000000-0000-0000-0000-00000000f401"},
		false,
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("prepareContinuationRunConfig() error = %v, want %v", err, sentinel)
	}
	if got.Query != "" || len(got.Messages) != 0 || len(got.ContextFrags) != 0 {
		t.Fatalf("failed continuation returned partial config: %#v", got)
	}
}

func TestPendingRuntimeDecisionsFailsClosedOnRuntimeLookupFailure(t *testing.T) {
	sentinel := errors.New("session unavailable")
	service := wireComponents(&Service{queries: &agenttest.AbortedLifecycleQueries{Pending: true}, sessionService: &agenttest.Sessions{GetFn: func(context.Context, string) (sessionpkg.Thread, error) { return sessionpkg.Thread{}, sentinel }}})
	got, err := service.PendingRuntimeDecisions(t.Context(), "00000000-0000-0000-0000-000000000001")
	if !errors.Is(err, sentinel) || got != nil {
		t.Fatalf("runtime lookup error treated as native: %v %v", got, err)
	}
}

// A continuation offers ask_user, and streams tool progress, only to a run
// someone follows live: a question nobody receives could never be answered.
func TestPrepareContinuationRunConfigOffersQuestionsOnlyToALiveRun(t *testing.T) {
	t.Parallel()

	resolver := wireComponents(&Service{userInput: &agenttest.UserInput{}})
	for _, live := range []bool{true, false} {
		got, err := resolver.prepareContinuationRunConfig(context.Background(), native.RunConfig{}, historyfrag.ScopeFallback{}, contextfrag.Scope{}, live)
		if err != nil {
			t.Fatalf("prepareContinuationRunConfig(live=%t) error = %v", live, err)
		}
		if got.CanRequestUserInput != live || got.LiveToolStream != live {
			t.Fatalf("live=%t: CanRequestUserInput=%t LiveToolStream=%t, want both %t", live, got.CanRequestUserInput, got.LiveToolStream, live)
		}
	}
}
