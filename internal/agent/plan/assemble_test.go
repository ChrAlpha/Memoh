package plan

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/transcript"
)

// assemblyPlanner is a planner whose history reader runs over stand-ins.
func assemblyPlanner(queries *agenttest.RecordingCompactionLog) *Planner {
	return New(Deps{Transcript: transcript.New(transcript.Deps{
		Logger:   slog.New(slog.DiscardHandler),
		Messages: &agenttest.RecordingMessages{},
		Queries:  queries,
		Sessions: agenttest.NewChatSessions(),
	})})
}

func TestAssembleContinuationReplacesStaleContext(t *testing.T) {
	t.Parallel()

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

	got, err := assemblyPlanner(&agenttest.RecordingCompactionLog{}).assembleContinuation(
		context.Background(),
		base,
		historyfrag.ScopeFallback{},
		contextfrag.Scope{},
	)
	if err != nil {
		t.Fatalf("assembleContinuation() error = %v", err)
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
}

func TestAssembleContinuationPropagatesArtifactProjectionFailure(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("artifact projection unavailable")
	got, err := assemblyPlanner(&agenttest.RecordingCompactionLog{ListErr: sentinel}).assembleContinuation(
		context.Background(),
		native.RunConfig{Query: "must not survive"},
		historyfrag.ScopeFallback{},
		contextfrag.Scope{SessionID: "00000000-0000-0000-0000-00000000f401"},
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("assembleContinuation() error = %v, want %v", err, sentinel)
	}
	if got.Query != "" || len(got.Messages) != 0 || len(got.ContextFrags) != 0 {
		t.Fatalf("failed continuation returned partial config: %#v", got)
	}
}
