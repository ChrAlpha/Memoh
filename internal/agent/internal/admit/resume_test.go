package admit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/auth"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/runtimefence"
)

type resumeSaveQueries struct {
	dbstore.Queries
	saved sqlc.SaveSessionRunResumeContextParams
	calls int
}

func (q *resumeSaveQueries) SaveSessionRunResumeContext(_ context.Context, arg sqlc.SaveSessionRunResumeContextParams) (int64, error) {
	q.saved = arg
	q.calls++
	return 1, nil
}

func TestResumeContextPreservesDeadlineAndScopesWithoutCredentials(t *testing.T) {
	secret := "resume-test-secret"
	bearer, _, err := auth.GenerateToken("account", secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	scoped, _, err := auth.GenerateChatToken(auth.ChatToken{BotID: "bot", ChatID: "chat", UserID: "sender", RouteID: "route"}, secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	q := &resumeSaveQueries{}
	svc := New(Deps{Queries: q, Signer: NewSigner(secret)})
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(runtimefence.WithContext(t.Context(), runtimefence.Fence{BotID: "bot", SessionID: "session", Token: 7}), deadline)
	defer cancel()
	req := runreq.ChatRequest{RunID: uuid.NewString(), UserID: "sender", ChatID: "chat", Token: "Bearer " + bearer, ChatToken: "Bearer " + scoped, Query: "do the work"}
	if err := svc.RecordResume(ctx, req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(q.saved.ResumeContext), bearer) || strings.Contains(string(q.saved.ResumeContext), scoped) || strings.Contains(string(q.saved.ResumeContext), secret) {
		t.Fatal("credential persisted")
	}
	var saved ResumeContext
	if err := json.Unmarshal(q.saved.ResumeContext, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.DeadlineAt == nil || !saved.DeadlineAt.Equal(deadline) || saved.ChatClaims["route_id"] != "route" || saved.TokenClaims["user_id"] != "account" {
		t.Fatalf("saved context=%+v", saved)
	}
	req.ShutdownResume = true
	if err := svc.RecordResume(ctx, req); err != nil {
		t.Fatal(err)
	}
	if q.calls != 1 {
		t.Fatal("resume rewrote the original recovery intent")
	}
}
