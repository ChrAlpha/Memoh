package application

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/grpctransport"
	"github.com/felinics/memoh/internal/agent/turn/turnpb"
	"github.com/felinics/memoh/internal/apperror"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
	intrpc "github.com/felinics/memoh/internal/rpc"
	"github.com/felinics/memoh/internal/settings"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

// These tests cross the seam a split deployment runs on: the channel process
// holds a turn RPC client, and the server process serves the real application
// service behind the turn RPC server. Only the model and the stores are fake.

// serveTurnOverRPC exposes service through the turn RPC server and returns the
// client the channel process would use.
func serveTurnOverRPC(t *testing.T, service turn.Service) *grpctransport.Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	server := intrpc.NewServer(slog.New(slog.DiscardHandler), "secret")
	turnpb.RegisterTurnServiceServer(server, grpctransport.NewServer(slog.New(slog.DiscardHandler), service))
	go func() { _ = server.Serve(lis) }()
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(intrpc.UnaryClientAuth("secret")),
		grpc.WithStreamInterceptor(intrpc.StreamClientAuth("secret")),
	)
	if err != nil {
		t.Fatalf("dial turn rpc: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		server.Stop()
		_ = lis.Close()
	})
	return grpctransport.NewClient(conn, grpctransport.WithClientLogger(slog.New(slog.DiscardHandler)))
}

// newRPCRuntime is the session runtime the server process runs turns on. Its
// ledger records each run's terminal state, which run_terminal reports.
func newRPCRuntime(t *testing.T, owner string) *sessionruntime.Manager {
	t.Helper()
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID: owner, OwnerLeaseTTL: time.Minute, Ledger: sessionledger.New(), Fence: abortAlignmentFence{},
	})
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

// recordingModelServer is an OpenAI-compatible endpoint that answers every
// streaming call with one text reply and keeps the request bodies it got.
// When failWith is set it rejects the request instead, which is how a test
// makes a run fail with a provider code and args.
type recordingModelServer struct {
	mu       sync.Mutex
	bodies   []string
	failWith string
}

func (m *recordingModelServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.bodies = append(m.bodies, string(body))
	failWith := m.failWith
	m.mu.Unlock()
	if failWith != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":{"message":%q}}`, failWith)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-rpc\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":null}]}\n\n")
	_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-rpc\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
}

func (m *recordingModelServer) requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.bodies...)
}

// rpcChatQueries adds the reads a run's terminal write makes: the run asked
// for no decisions.
type rpcChatQueries struct {
	directLifecycleQueries
}

func (*rpcChatQueries) ListToolApprovalsByRun(context.Context, pgtype.UUID) ([]sqlc.ToolApprovalRequest, error) {
	return nil, nil
}

func (*rpcChatQueries) ListUserInputsByRun(context.Context, pgtype.UUID) ([]sqlc.UserInputRequest, error) {
	return nil, nil
}

// newRPCChatService is the server process's application service for chat
// turns: the native runtime calling a recorded model endpoint.
func newRPCChatService(t *testing.T) (*Service, *recordingModelServer) {
	t.Helper()
	model := &recordingModelServer{}
	server := httptest.NewServer(model)
	t.Cleanup(server.Close)

	provider := modelSelectionProviderRow(t, directLifecycleProviderID, string(models.ClientTypeOpenAICompletions), true)
	provider.Config, _ = json.Marshal(map[string]any{"api_key": "test-key", "base_url": server.URL})
	chatModel := modelSelectionModelRow(t, directLifecycleModelID, "rpc-roundtrip-model", provider.ID, models.ModelTypeChat, true)
	chatModel.Config = []byte(`{"context_window":128000}`)
	queries := &rpcChatQueries{directLifecycleQueries{
		modelSelectionFakeQueries: modelSelectionFakeQueries{
			models:   map[string]sqlc.Model{chatModel.ModelID: chatModel},
			provider: provider,
		},
		modelID: chatModel.ID,
	}}
	logger := slog.New(slog.DiscardHandler)
	service := &Service{
		agent:             native.New(native.Deps{Logger: logger}),
		modelsService:     models.NewService(logger, queries),
		queries:           queries,
		contextLifecycles: &synchronizedLifecycleStore{},
		messageService:    &recordingMessageService{},
		settingsService:   settings.NewService(logger, queries, nil, nil),
		sessionService: &fakeBackgroundSessionService{
			getFn: func(_ context.Context, sessionID string) (sessionpkg.Thread, error) {
				return sessionpkg.Thread{
					ID: sessionID, BotID: lifecycleTestBotID, Type: sessionpkg.TypeChat,
					SessionMode: sessionpkg.TypeChat, RuntimeType: sessionpkg.RuntimeModel,
				}, nil
			},
		},
		streamHTTPClient: server.Client(),
		logger:           logger,
	}
	service.SetSessionRuntime(newRPCRuntime(t, "owner-rpc-chat"))
	return service, model
}

// drainOverRPC reads a remote handle to its end.
func drainOverRPC(t *testing.T, handle turn.RunHandle) ([]turn.Event, []error) {
	t.Helper()
	var (
		events []turn.Event
		errs   []error
	)
	timeout := time.After(10 * time.Second)
	for eventCh, errCh := handle.Events(), handle.Errs(); eventCh != nil || errCh != nil; {
		select {
		case event, ok := <-eventCh:
			if !ok {
				eventCh = nil
				continue
			}
			events = append(events, event)
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			errs = append(errs, err)
		case <-timeout:
			t.Fatal("turn over rpc did not end")
		}
	}
	return events, errs
}

func requireCompletedRunTerminal(t *testing.T, handle turn.RunHandle, events []turn.Event, errs []error) {
	t.Helper()
	if len(errs) != 0 {
		t.Fatalf("turn errors over rpc = %v", errs)
	}
	if !turn.ReportsRunTerminal(handle) {
		t.Fatal("remote handle does not report run_terminal")
	}
	if len(events) == 0 {
		t.Fatal("no events over rpc")
	}
	last := events[len(events)-1]
	terminal, ok := turn.RunTerminalFrom(last)
	if !ok || terminal.State != turn.RunStateCompleted || terminal.ErrorCode != "" {
		t.Fatalf("last event over rpc = %s %s, want completed run_terminal", last.Kind, last.Payload)
	}
	if last.RunID != handle.RunID() || last.ThreadID == "" || last.Seq != int64(len(events)) {
		t.Fatalf("run_terminal envelope = run %q thread %q seq %d, want run %q seq %d",
			last.RunID, last.ThreadID, last.Seq, handle.RunID(), len(events))
	}
}

// A skill named in an IM message reaches the model in a split deployment:
// its body crosses the turn RPC and is rendered into the model request, next
// to the attachment the message carried.
func TestChatTurnOverRPCDeliversRequestedSkillsAndAttachments(t *testing.T) {
	service, model := newRPCChatService(t)
	client := serveTurnOverRPC(t, service)

	const skillBody = "PRIVATE_REQUESTED_SKILL_BODY"
	const attachmentPath = "/data/media/report.pdf"
	handle, err := client.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:   1,
		TeamID:          "team-1",
		Mode:            turn.ModeChat,
		BotID:           lifecycleTestBotID,
		ChatID:          lifecycleTestBotID,
		ThreadID:        lifecycleTestSessionID,
		IdempotencyKey:  "msg-rpc-1",
		Query:           "summarize the report",
		UserVisibleText: "summarize the report",
		Attachments: []turn.Attachment{{
			Type: "file", Path: attachmentPath, Name: "report.pdf", Mime: "application/pdf",
		}},
		RequestedSkills: []turn.RequestedSkillContext{{
			Name: "alpha", Description: "Alpha skill", Content: skillBody, SourceKind: "registry",
			OpaqueSourceID: "opaque-alpha", ContentHash: "hash-alpha", Identity: "alpha@registry",
		}},
		SkipMemoryExtraction: true,
		SkipTitleGeneration:  true,
	})
	if err != nil {
		t.Fatalf("start turn over rpc: %v", err)
	}
	events, errs := drainOverRPC(t, handle)
	requireCompletedRunTerminal(t, handle, events, errs)

	requests := model.requests()
	if len(requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(requests))
	}
	request := requests[0]
	if !strings.Contains(request, skillBody) || !strings.Contains(request, `requested_skill name=\"alpha\" source_kind=\"registry\"`) {
		t.Fatalf("model request lacks the requested skill: %s", request)
	}
	if !strings.Contains(request, attachmentPath) {
		t.Fatalf("model request lacks the attachment: %s", request)
	}
}

// A failed run crosses the RPC with its outcome intact: the channel process
// reads the state, error code, and args the server process recorded, so the
// failure reply it renders names the failure the run had. The provider's own
// text is not part of any of it.
func TestChatTurnFailureOverRPCCarriesStateCodeAndArgs(t *testing.T) {
	const privateDetail = "PRIVATE_RPC_PROVIDER_FAILURE"
	service, model := newRPCChatService(t)
	model.failWith = privateDetail
	client := serveTurnOverRPC(t, service)

	handle, err := client.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:   1,
		TeamID:          "team-1",
		Mode:            turn.ModeChat,
		BotID:           lifecycleTestBotID,
		ChatID:          lifecycleTestBotID,
		ThreadID:        lifecycleTestSessionID,
		IdempotencyKey:  "msg-rpc-failure",
		Query:           "inspect the report",
		UserVisibleText: "inspect the report",
	})
	if err != nil {
		t.Fatalf("start turn over rpc: %v", err)
	}
	events, _ := drainOverRPC(t, handle)
	if !turn.ReportsRunTerminal(handle) {
		t.Fatal("remote handle does not report run_terminal")
	}
	last := events[len(events)-1]
	if last.Kind != turn.EventRunTerminal {
		t.Fatalf("last event over rpc = %s %s, want run_terminal", last.Kind, last.Payload)
	}
	terminal, ok := turn.RunTerminalFrom(last)
	if !ok {
		t.Fatalf("run_terminal payload = %s", last.Payload)
	}
	if terminal.State != turn.RunStateFailed {
		t.Fatalf("run_terminal state = %q, want %q", terminal.State, turn.RunStateFailed)
	}
	if terminal.ErrorCode == "" {
		t.Fatalf("run_terminal = %+v, want the failure's own code", terminal)
	}
	if strings.Contains(string(last.Payload), privateDetail) {
		t.Fatalf("run_terminal leaks the provider's text: %s", last.Payload)
	}
	// The args a failure carries cross the same way its code does: the
	// terminal's payload is the record's, so a channel process that renders a
	// failure reply from it reads what the server process wrote.
	carried, err := json.Marshal(turn.RunTerminal{
		Type:      turn.EventRunTerminal,
		State:     turn.RunStateFailed,
		ErrorCode: string(apperror.CodeAgentDependencyMissing),
		ErrorArgs: map[string]string{"dep_id": "codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	roundTripped, ok := turn.RunTerminalFrom(turn.Event{Kind: turn.EventRunTerminal, Payload: carried})
	if !ok || roundTripped.ErrorArgs["dep_id"] != "codex" {
		t.Fatalf("run_terminal args did not survive encoding: %+v (%v)", roundTripped, ok)
	}
}
