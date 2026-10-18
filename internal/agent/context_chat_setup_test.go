package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/hooks"
	"github.com/felinics/memoh/internal/workspace/bridge"
	pb "github.com/felinics/memoh/internal/workspace/bridgepb"
)

// These tests cover chatSetup's own decisions: which inputs run the
// user-message hook, and which turns the runtime refuses because it owns its
// conversation context. Every case reaches them through StartTurn, so what is
// asserted is what a sender experiences rather than what an internal call
// returned.

// hookWorkspace is the bridge a hooks.Service reads its bot's hook
// configuration from. It serves one config file and answers the hook's own
// command, so a test can make the hook allow, deny, or fail.
type hookWorkspace struct {
	pb.UnimplementedContainerServiceServer

	mu       sync.Mutex
	config   []byte
	response string
	env      []string
	calls    int
}

func (w *hookWorkspace) setConfig(t *testing.T, config string) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.config = []byte(config)
}

// setResponse is what the hook command prints, which is the hook's decision.
func (w *hookWorkspace) setResponse(t *testing.T, response string) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.response = response
}

func (w *hookWorkspace) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

func (w *hookWorkspace) ReadRaw(req *pb.ReadRawRequest, stream grpc.ServerStreamingServer[pb.DataChunk]) error {
	w.mu.Lock()
	config := append([]byte(nil), w.config...)
	w.mu.Unlock()
	if req.GetPath() != hooks.DefaultConfigPath || len(config) == 0 {
		return errors.New("no such file")
	}
	return stream.Send(&pb.DataChunk{Data: config})
}

func (*hookWorkspace) WriteFile(context.Context, *pb.WriteFileRequest) (*pb.WriteFileResponse, error) {
	return &pb.WriteFileResponse{}, nil
}

// Exec answers the hook's command with the response the test set, which is the
// hook's decision.
func (w *hookWorkspace) Exec(stream grpc.BidiStreamingServer[pb.ExecInput, pb.ExecOutput]) error {
	input, err := stream.Recv()
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.calls++
	response := w.response
	w.env = append(w.env, input.GetEnv()...)
	w.mu.Unlock()
	if response != "" {
		if err := stream.Send(&pb.ExecOutput{Stream: pb.ExecOutput_STDOUT, Data: []byte(response)}); err != nil {
			return err
		}
	}
	return stream.Send(&pb.ExecOutput{Stream: pb.ExecOutput_EXIT, ExitCode: 0})
}

// hookBridgeProvider hands the application a session runtime's workspace
// client, which is what the hooks service reads through.
type hookBridgeProvider struct{ client *bridge.Client }

func (p hookBridgeProvider) MCPClient(context.Context, string) (*bridge.Client, error) {
	return p.client, nil
}

// newHookWorkspaceBridge serves the hook workspace over an in-process
// connection and returns the provider the service reaches it with.
func newHookWorkspaceBridge(t *testing.T, workspace *hookWorkspace) bridge.Provider {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterContainerServiceServer(server, workspace)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(lis)
	}()
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial hook workspace: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		server.Stop()
		<-done
		_ = lis.Close()
	})
	return hookBridgeProvider{client: bridge.NewClientFromConn(conn)}
}

// userMessageHookConfig enables one hook on the event that decides a send.
func userMessageHookConfig(t *testing.T, command string) string {
	t.Helper()
	config, err := json.Marshal(map[string]any{
		"version": 1,
		"enabled": true,
		"hooks": []map[string]any{{
			"name":  "user message policy",
			"event": "UserMessageReceived",
			"actions": []map[string]any{{
				"type":    "command",
				"command": command,
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(config)
}

// newHookChatFixture is a chat fixture whose user-message hook is served by
// workspace, so a test can make the hook decide.
func newHookChatFixture(t *testing.T, workspace *hookWorkspace) chatTurnFixture {
	t.Helper()
	return newChatTurnFixture(t, directLifecycleModelSuccess, withFixtureDeps(func(d *Deps) {
		d.Hooks = hooks.NewService(d.Logger, newHookWorkspaceBridge(t, workspace))
	}))
}

// withUserMessageHook installs a hook that prints response, which is how a test
// chooses the hook's decision.
func withUserMessageHook(t *testing.T, response string) (*hookWorkspace, chatTurnFixture) {
	t.Helper()
	workspace := &hookWorkspace{}
	workspace.setConfig(t, userMessageHookConfig(t, "user-message-policy"))
	workspace.setResponse(t, response)
	return workspace, newHookChatFixture(t, workspace)
}

// A hook that refuses an IM message leaves the sender's own message and a
// failure row marked with the hook as their origin, so the sender sees why
// their message was not answered and the model's context does not carry it.
func TestChatSetupHookRefusalLeavesMarkedRowsForIM(t *testing.T) {
	workspace, fixture := withUserMessageHook(t, `{"decision":"deny","reason":"policy"}`)
	fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "hook-deny-1"}).Cancel()
	collectEvents(t, fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "hook-deny-2"}))

	if workspace.callCount() == 0 {
		t.Fatal("the user-message hook never ran for an IM send")
	}
	persisted := fixture.fixture.messages.Persisted
	if len(persisted) == 0 {
		t.Fatal("a hook refused the message and nothing was written")
	}
	finish := fixture.admitter.AwaitFinish(t)
	if finish.Message != string(apperror.CodeHookUserMessageFailed) {
		t.Fatalf("finish code = %q, want %q", finish.Message, apperror.CodeHookUserMessageFailed)
	}
}

// A retry of a turn the hook itself rejected passes through the hook again: the
// turn being replaced was never answered, and the hook is what decides whether
// it is answered now. The retry entry reaches this by leaving
// ReusePersistedUserMessage false, which is the fact chatSetup reads.
func TestChatSetupRetryRerunsTheHook(t *testing.T) {
	workspace, fixture := withUserMessageHook(t, `{"decision":"allow"}`)
	service := fixture.fixture.service

	if _, err := service.plan.ChatSetup(context.Background(), runreq.ChatRequest{
		BotID:           agenttest.LifecycleBotID,
		ChatID:          agenttest.LifecycleBotID,
		ThreadID:        agenttest.LifecycleSessionID,
		RunID:           agenttest.LifecycleRunID,
		Query:           directLifecyclePrompt,
		RawQuery:        directLifecyclePrompt,
		UserVisibleText: directLifecyclePrompt,
	}, drive.RunSpec{}); err != nil {
		t.Fatalf("ChatSetup() error = %v", err)
	}
	if workspace.callCount() != 1 {
		t.Fatalf("hook calls = %d, want the retry to pass through the hook once", workspace.callCount())
	}
}

// A retry of a turn that was answered has its user message reused, and the hook
// does not run again: the input was already accepted, and re-deciding it would
// let a later policy change rewrite what the user already sent.
func TestChatSetupRetryOfAnAnsweredTurnSkipsTheHook(t *testing.T) {
	workspace, fixture := withUserMessageHook(t, `{"decision":"deny","reason":"policy"}`)

	if _, err := fixture.fixture.service.plan.ChatSetup(context.Background(), runreq.ChatRequest{
		BotID:                     agenttest.LifecycleBotID,
		ChatID:                    agenttest.LifecycleBotID,
		ThreadID:                  agenttest.LifecycleSessionID,
		RunID:                     agenttest.LifecycleRunID,
		Query:                     directLifecyclePrompt,
		RawQuery:                  directLifecyclePrompt,
		UserVisibleText:           directLifecyclePrompt,
		ReusePersistedUserMessage: true,
	}, drive.RunSpec{}); err != nil {
		t.Fatalf("ChatSetup() error = %v", err)
	}
	if workspace.callCount() != 0 {
		t.Fatalf("hook calls = %d, want none for a reused user message", workspace.callCount())
	}
}

// A hook that allows the send does not run twice for it, and the run reaches
// the model: the hook is per input, not per attempt of the same input.
func TestChatSetupAllowedHookRunsTheTurnToCompletion(t *testing.T) {
	workspace, fixture := withUserMessageHook(t, `{"decision":"allow"}`)
	collectEvents(t, fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "hook-allow-1"}))

	if workspace.callCount() != 1 {
		t.Fatalf("hook calls = %d, want exactly one for one input", workspace.callCount())
	}
	if got := fixture.admitter.AwaitFinish(t).Status; got != "completed" {
		t.Fatalf("finish status = %q, want completed", got)
	}
	if len(fixture.fixture.requests.recorded()) == 0 {
		t.Fatal("an allowed turn never reached the model")
	}
}

// An External Agent runtime owns its conversation context, so it has no rewind
// primitive: running a replacement turn against it would leave that context
// inconsistent with the visible history. chatSetup refuses it with the code
// that says so, rather than rewriting history the runtime keeps.
func TestChatSetupExternalRuntimeRefusesTurnReplacement(t *testing.T) {
	service, _ := newDrivePromptFixture(t)

	spec := drive.RunSpec{
		Run: drive.AdmittedRun{Handle: sessionruntime.RunHandle{
			BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID, FencingToken: 1,
		}},
		Replacement: &drive.TurnReplacement{},
	}
	_, err := service.plan.ChatSetup(context.Background(), runreq.ChatRequest{
		BotID:           agenttest.LifecycleBotID,
		ChatID:          agenttest.LifecycleBotID,
		ThreadID:        agenttest.LifecycleSessionID,
		RunID:           agenttest.LifecycleRunID,
		Query:           directLifecyclePrompt,
		RawQuery:        directLifecyclePrompt,
		UserVisibleText: directLifecyclePrompt,
	}, spec)
	if err == nil {
		t.Fatal("an External Agent runtime accepted a replacement turn")
	}
	if got := apperror.CodeOf(err); got != apperror.CodeExternalAgentTurnReplacementUnsupported {
		t.Fatalf("ChatSetup() error code = %q, want %q", got, apperror.CodeExternalAgentTurnReplacementUnsupported)
	}
}

// A replacement turn does not run the user-message hook: the turn it rewrites
// is already in history, and the hook decided that input once. Running it again
// would let the current policy rewrite a message the user already sent.
func TestChatSetupReplacementTurnSkipsTheHook(t *testing.T) {
	workspace, fixture := withUserMessageHook(t, `{"decision":"deny","reason":"policy"}`)

	_, err := fixture.fixture.service.plan.ChatSetup(context.Background(), runreq.ChatRequest{
		BotID:           agenttest.LifecycleBotID,
		ChatID:          agenttest.LifecycleBotID,
		ThreadID:        agenttest.LifecycleSessionID,
		RunID:           agenttest.LifecycleRunID,
		Query:           directLifecyclePrompt,
		RawQuery:        directLifecyclePrompt,
		UserVisibleText: directLifecyclePrompt,
	}, drive.RunSpec{Replacement: &drive.TurnReplacement{}})
	if err != nil {
		t.Fatalf("ChatSetup() error = %v", err)
	}
	if workspace.callCount() != 0 {
		t.Fatalf("hook calls = %d, want none for a replacement turn", workspace.callCount())
	}
}

// A quiet workspace is the common case: no hook is configured, the turn runs
// without one, and nothing about it changes.
func TestChatSetupWithoutAHookRunsTheTurn(t *testing.T) {
	workspace := &hookWorkspace{}
	workspace.setConfig(t, `{"version":1,"enabled":false}`)
	fixture := newHookChatFixture(t, workspace)
	collectEvents(t, fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "no-hook-1"}))

	if workspace.callCount() != 0 {
		t.Fatalf("hook calls = %d, want none for a bot with hooks disabled", workspace.callCount())
	}
	if got := fixture.admitter.AwaitFinish(t).Status; got != "completed" {
		t.Fatalf("finish status = %q, want completed", got)
	}
}
