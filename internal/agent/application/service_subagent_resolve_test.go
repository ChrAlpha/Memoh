package application

import (
	"context"
	"log/slog"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/transcript"
	session "github.com/felinics/memoh/internal/chat/thread"
)

func TestResolveReturnsTheEffectiveSubagentRequest(t *testing.T) {
	svc := &agenttest.SubagentSessions{
		Sessions: agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{ID: sessionID, Type: session.TypeSubagent}, nil
			},
		},
		Config: session.SubagentConfig{ModelUUID: "model-uuid-1", ModelID: "gpt-x", ProviderName: "prov"},
	}
	s := wireComponents(&Service{
		sessionService: svc,
		transcript:     transcript.New(transcript.Deps{Sessions: svc, Logger: slog.New(slog.DiscardHandler)}),
		logger:         slog.New(slog.DiscardHandler),
	})

	// Resolution fails at model selection (no settings service), which is the
	// point: even the failure path must not hand back a request that has lost
	// the subagent defaults.
	_, effective, err := s.resolve(context.Background(), runreq.ChatRequest{
		BotID: "bot-1", ChatID: "chat-1", ThreadID: "sub-1", Query: "hello",
	})
	if err == nil {
		t.Fatal("resolve unexpectedly succeeded without a settings service")
	}
	if effective.SessionType != session.TypeSubagent {
		t.Fatalf("session type = %q, want subagent", effective.SessionType)
	}
	if !effective.SkipMemoryExtraction || !effective.SkipTitleGeneration {
		t.Fatalf("skip flags lost on the way out of resolve: %+v", effective)
	}
	if effective.Model != "model-uuid-1" {
		t.Fatalf("model = %q, want the pinned model uuid", effective.Model)
	}
}
