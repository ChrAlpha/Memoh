package application

import (
	"context"
	"log/slog"
	"testing"

	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/apperror"
)

func requestedSkillGuardRequest(sessionID string) runreq.ChatRequest {
	return runreq.ChatRequest{
		BotID:    "bot-1",
		ThreadID: sessionID,
		RequestedSkills: []runreq.RequestedSkillContext{{
			Name:    "alpha",
			Content: "skill content",
		}},
	}
}

func TestWebSendRejectsACPRequestedSkillsBeforePool(t *testing.T) {
	pool := &recordingACPPrompter{}
	resolver := wireComponents(&Service{
		logger:         slog.New(slog.DiscardHandler),
		sessionService: acpRuntimeSessionServiceForTest("user-1"),
	})
	registerACPPool(resolver, pool)

	// The refusal is the run's setup failure, recorded under the catalog code
	// the composer already renders for it.
	if _, err := streamWebSend(context.Background(), t, resolver, requestedSkillGuardRequest("session-1")); apperror.CodeOf(err) != apperror.CodeSlashSkillActivationUnsupported {
		t.Fatalf("Web send failure = %v, want %s", err, apperror.CodeSlashSkillActivationUnsupported)
	}
	if pool.calls != 0 {
		t.Fatalf("ACP pool calls = %d, want 0", pool.calls)
	}
}
