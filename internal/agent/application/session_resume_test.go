package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/auth"
	"github.com/felinics/memoh/internal/db"
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
	svc := &Service{queries: q, resumeSecret: secret}
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(runtimefence.WithContext(t.Context(), runtimefence.Fence{BotID: "bot", SessionID: "session", Token: 7}), deadline)
	defer cancel()
	req := ChatRequest{RunID: uuid.NewString(), UserID: "sender", ChatID: "chat", Token: "Bearer " + bearer, ChatToken: "Bearer " + scoped, Query: "do the work"}
	if err := svc.recordRunResumeContext(ctx, req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(q.saved.ResumeContext), bearer) || strings.Contains(string(q.saved.ResumeContext), scoped) || strings.Contains(string(q.saved.ResumeContext), secret) {
		t.Fatal("credential persisted")
	}
	var saved resumeContext
	if err := json.Unmarshal(q.saved.ResumeContext, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.DeadlineAt == nil || !saved.DeadlineAt.Equal(deadline) || saved.ChatClaims["route_id"] != "route" || saved.TokenClaims["user_id"] != "account" {
		t.Fatalf("saved context=%+v", saved)
	}
	req.ShutdownResume = true
	if err := svc.recordRunResumeContext(ctx, req); err != nil {
		t.Fatal(err)
	}
	if q.calls != 1 {
		t.Fatal("resume rewrote the original recovery intent")
	}
}

func TestResumeRejectsInvalidCredentialAndForeignBotScope(t *testing.T) {
	s := &Service{resumeSecret: "resume-secret"}
	token, _, err := auth.GenerateToken("account", "other-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.resumeClaims(token); err == nil {
		t.Fatal("accepted invalid signature")
	}
	if _, err := s.renewResumeCredential(t.Context(), "bot", map[string]string{"typ": "chat_route", "bot_id": "other", "chat_id": "chat"}); !errors.Is(err, errResumeUnrecoverable) {
		t.Fatalf("cross-bot credential = %v, want an unrecoverable intent", err)
	}
	if _, err := s.renewResumeCredential(t.Context(), "bot", map[string]string{"sub": "account"}); err == nil {
		t.Fatal("renewed account without reauthorization")
	}
	token, err = s.renewResumeCredential(t.Context(), "bot", map[string]string{"typ": "chat_route", "bot_id": "bot", "chat_id": "chat", "route_id": "route", "user_id": "sender"})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.Parse(strings.TrimPrefix(token, "Bearer "), func(_ *jwt.Token) (any, error) { return []byte(s.resumeSecret), nil }, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !parsed.Valid {
		t.Fatal("renewed token invalid")
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["route_id"] != "route" || claims["typ"] != "chat_route" {
		t.Fatal("scope widened")
	}
}

// A resume re-enters the session it saved, on the request the intent recorded,
// and keeps the deadline the interrupted run was given. The turn resolves
// through the whole application path, so the assertion is on what the model was
// asked for rather than on an intermediate request value.
func TestResumeReentersSavedSessionOnceAndKeepsDeadline(t *testing.T) {
	fixture := newChatTurnFixture(t, directLifecycleModelSuccess)
	service := fixture.fixture.service
	service.allowedTeam = "00000000-0000-0000-0000-000000000001"
	deadline := time.Now().Add(time.Minute)
	data := resumeContext{Version: 1, ChatID: lifecycleTestBotID, UserID: "sender", Query: "original task", DeadlineAt: &deadline}
	raw, err := json.Marshal(map[string]any{"resume": data})
	if err != nil {
		t.Fatal(err)
	}
	row := sqlc.SessionRun{
		TeamID:    db.ParseUUIDOrEmpty("00000000-0000-0000-0000-000000000001"),
		RunID:     db.ParseUUIDOrEmpty(uuid.NewString()),
		BotID:     db.ParseUUIDOrEmpty(lifecycleTestBotID),
		SessionID: db.ParseUUIDOrEmpty(lifecycleTestSessionID),
		InputJson: raw,
	}
	done, err := service.resumeInterruptedSession(t.Context(), row)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not finish")
	}
	// The resumed run asks the model the original task, with the instruction
	// that tells it why it is seeing it again.
	requests := fixture.fixture.requests.recorded()
	if len(requests) == 0 {
		t.Fatal("the resumed run never reached the model provider")
	}
	if !strings.Contains(requests[0], "original task") || !strings.Contains(requests[0], resumeInstruction) {
		t.Fatalf("model request lacks the resumed task: %s", requests[0])
	}
	// A resume writes no new user message: the input is synthetic, and the
	// turn it continues is already in history.
	for _, input := range fixture.fixture.messages.persisted {
		if input.Role == "user" {
			t.Fatalf("resume persisted a user message: %+v", input)
		}
	}

	inputs := fixture.admitter.admitted()
	if len(inputs) != 1 || inputs[0].InvocationID != "resume:"+row.RunID.String() {
		t.Fatalf("admission=%+v", inputs)
	}
	fixture.admitter.mu.Lock()
	fixture.admitter.started = false
	fixture.admitter.mu.Unlock()
	if _, err := service.resumeInterruptedSession(t.Context(), row); !errors.Is(err, errResumeUnrecoverable) {
		t.Fatalf("duplicate admission = %v, want an unrecoverable intent", err)
	}
	expired := time.Now().Add(-time.Second)
	data.DeadlineAt = &expired
	row.InputJson, _ = json.Marshal(map[string]any{"resume": data})
	if done, err := service.resumeInterruptedSession(t.Context(), row); !errors.Is(err, errResumeUnrecoverable) || done != nil {
		t.Fatalf("expired budget: done=%v err=%v, want an unrecoverable intent", done, err)
	}
	if len(fixture.admitter.admitted()) != 2 {
		t.Fatal("expired task reached admission")
	}
}

func TestResumeWaitsForWorkspaceBeforeAdmission(t *testing.T) {
	s, admitter := newAdmittedTurnTestService()
	s.allowedTeam = "00000000-0000-0000-0000-000000000001"
	s.resumeReady = func(context.Context, string, string) error { return errors.New("bridge not ready") }
	raw, _ := json.Marshal(map[string]any{"resume": resumeContext{Version: 1, ChatID: "chat", Query: "work"}})
	done, err := s.resumeInterruptedSession(t.Context(), sqlc.SessionRun{TeamID: db.ParseUUIDOrEmpty("00000000-0000-0000-0000-000000000001"), InputJson: raw})
	if err == nil || done != nil {
		t.Fatal("unready workspace admitted")
	}
	if len(admitter.admitted()) != 0 {
		t.Fatal("readiness failure consumed resume identity")
	}
}

type retireResumeQueries struct {
	dbstore.Queries
	rows    []sqlc.SessionRun
	swept   int
	retired []string
}

func (q *retireResumeQueries) ListInterruptedSessionRuns(context.Context, pgtype.UUID) ([]sqlc.SessionRun, error) {
	rows := q.rows
	q.rows = nil
	return rows, nil
}

func (q *retireResumeQueries) RetireSupersededInterruptedSessionRuns(context.Context) (int64, error) {
	q.swept++
	return 0, nil
}

func (q *retireResumeQueries) RetireInterruptedSessionRun(_ context.Context, runID pgtype.UUID) (int64, error) {
	q.retired = append(q.retired, runID.String())
	return 1, nil
}

// An intent that can never continue is retired once instead of being retried
// on every pass; one that may still clear (the workspace is not up yet) keeps
// its intent, and a resumable one is admitted.
func TestResumeScanRetiresIntentsThatCannotContinue(t *testing.T) {
	const teamID = "00000000-0000-0000-0000-000000000001"
	fixture := newChatTurnFixture(t, directLifecycleModelSuccess)
	s, admitter := fixture.fixture.service, fixture.admitter
	s.allowedTeam = teamID
	unreadyBot := uuid.NewString()
	s.resumeReady = func(_ context.Context, botID, _ string) error {
		if botID == unreadyBot {
			return errors.New("bridge not ready")
		}
		return nil
	}
	future, past := time.Now().Add(time.Minute), time.Now().Add(-time.Second)
	row := func(botID string, data resumeContext) sqlc.SessionRun {
		raw, err := json.Marshal(map[string]any{"resume": data})
		if err != nil {
			t.Fatal(err)
		}
		return sqlc.SessionRun{TeamID: db.ParseUUIDOrEmpty(teamID), RunID: db.ParseUUIDOrEmpty(uuid.NewString()), BotID: db.ParseUUIDOrEmpty(botID), SessionID: db.ParseUUIDOrEmpty(uuid.NewString()), InputJson: raw}
	}
	expired := row(uuid.NewString(), resumeContext{Version: 1, ChatID: lifecycleTestBotID, Query: "late", DeadlineAt: &past})
	unsupported := row(uuid.NewString(), resumeContext{Version: 2, ChatID: lifecycleTestBotID, Query: "future format"})
	unready := row(unreadyBot, resumeContext{Version: 1, ChatID: lifecycleTestBotID, Query: "wait", DeadlineAt: &future})
	resumable := row(uuid.NewString(), resumeContext{Version: 1, ChatID: lifecycleTestBotID, Query: "go on", DeadlineAt: &future})
	// The resumable row runs the whole turn, so the store the fixture serves
	// stays underneath the resume-specific reads this test added.
	q := &retireResumeQueries{Queries: s.queries, rows: []sqlc.SessionRun{expired, unsupported, unready, resumable}}
	s.queries = q

	scan := sessionResumeScan{}
	if err := s.scanInterruptedSessions(t.Context(), s.sessionResumeScopes(), &scan, make(chan struct{}, 4)); err != nil {
		t.Fatal(err)
	}
	if q.swept != 1 {
		t.Fatalf("superseded sweep ran %d times, want once per scope pass", q.swept)
	}
	want := []string{expired.RunID.String(), unsupported.RunID.String()}
	if fmt.Sprint(q.retired) != fmt.Sprint(want) {
		t.Fatalf("retired %v, want %v", q.retired, want)
	}
	inputs := admitter.admitted()
	if len(inputs) != 1 || inputs[0].InvocationID != "resume:"+resumable.RunID.String() {
		t.Fatalf("admission=%+v, want only the resumable run", inputs)
	}
}
