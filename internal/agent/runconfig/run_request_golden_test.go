package runconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sync"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	tools "github.com/felinics/memoh/internal/agent/tool"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/sessionfacts"
)

var updateRunRequests = flag.Bool("update-run-requests", false, "rewrite testdata/run-requests golden files")

type runRequestRecorder struct {
	mu       sync.Mutex
	requests []sdk.Request
}

func (*runRequestRecorder) Name() string { return "run-request-recorder" }

func (*runRequestRecorder) ListModels(context.Context) ([]sdk.Model, error) { return nil, nil }

func (*runRequestRecorder) Test(context.Context) error { return nil }

func (*runRequestRecorder) TestModel(context.Context, string) (*sdk.ModelTestResult, error) {
	return &sdk.ModelTestResult{Supported: true}, nil
}

func (p *runRequestRecorder) DoGenerate(_ context.Context, req sdk.Request) (sdk.ModelResult, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return sdk.ModelResult{Text: "ok", FinishReason: sdk.FinishReasonStop}, nil
}

func (*runRequestRecorder) DoStream(context.Context, sdk.Request) (<-chan sdk.StreamPart, error) {
	return nil, nil
}

// runRequestCase is one session kind as the application hands it to the
// native runtime: the run label it carries and the conversation it answers.
type runRequestCase struct {
	name             string
	sessionType      string
	conversationType string
	isSubagent       bool
}

var runRequestCases = []runRequestCase{
	{name: "chat", sessionType: "", conversationType: "private"},
	{name: "discuss", sessionType: "discuss", conversationType: "group"},
	{name: "schedule", sessionType: "schedule", conversationType: ""},
	{name: "subagent", sessionType: "subagent", conversationType: "", isSubagent: true},
}

// goldenRunConfig derives the run's facts the way the run config assembly
// does: discuss turns through ResolveRunConfig, every other kind through a
// chat request. The session row carries the preset its mode is created with.
func goldenRunConfig(tc runRequestCase, provider sdk.Provider) native.RunConfig {
	mode := tc.sessionType
	if mode == "" {
		mode = sessionmode.Chat
	}
	svc := newBuilder(func(d *Deps) {
		d.Sessions = &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (sessionpkg.Thread, error) {
				return sessionpkg.Thread{ID: sessionID, Type: mode, SessionMode: mode, RuntimeType: sessionpkg.RuntimeModel, Facts: sessionfacts.Preset(mode)}, nil
			},
		}
	})
	var facts sessionfacts.Facts
	if tc.sessionType == sessionmode.Discuss {
		typ, _, stored := svc.sessionDescriptor(context.Background(), "session-golden")
		facts = runFacts(stored, typ)
	} else {
		facts = svc.requestRunFacts(context.Background(), runreq.ChatRequest{ThreadID: "session-golden", SessionType: tc.sessionType})
	}
	cfg := native.RunConfig{
		Model:               &sdk.Model{ID: "golden-model", Provider: provider, Type: sdk.ModelTypeChat},
		SessionType:         tc.sessionType,
		ReplyDelivery:       facts.ReplyDelivery,
		DecisionMode:        facts.DecisionMode,
		SupportsToolCall:    true,
		CanRequestUserInput: true,
		Messages:            []sdk.Message{sdk.UserMessage("hello")},
		Identity: native.SessionContext{
			BotID:            "bot-golden",
			ChatID:           "bot-golden",
			SessionID:        "session-golden",
			CurrentPlatform:  "telegram",
			ReplyTarget:      "chat-golden",
			ConversationType: tc.conversationType,
			IsSubagent:       tc.isSubagent,
		},
	}
	return newBuilder().Prepare(context.Background(), cfg)
}

// TestRunRequestGolden pins the system prompt and tool definitions the model
// receives for each session kind.
func TestRunRequestGolden(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "run-requests")
	for _, tc := range runRequestCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recorder := &runRequestRecorder{}
			agent := native.New(native.Deps{})
			agent.SetToolProviders([]tools.ToolProvider{
				tools.NewMessageProvider(nil, agenttest.MessageSender{}, agenttest.MessageReactor{}, agenttest.ChannelParser{}, nil),
				tools.NewAskUserProvider(nil),
			})
			if _, err := agent.Generate(context.Background(), goldenRunConfig(tc, recorder)); err != nil {
				t.Fatalf("Generate: %v", err)
			}
			recorder.mu.Lock()
			if len(recorder.requests) != 1 {
				recorder.mu.Unlock()
				t.Fatalf("model requests = %d, want 1", len(recorder.requests))
			}
			req := recorder.requests[0]
			recorder.mu.Unlock()

			got, err := json.MarshalIndent(struct {
				System string               `json:"system"`
				Tools  []sdk.ToolDefinition `json:"tools"`
			}{System: req.System, Tools: req.Tools}, "", "  ")
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			got = append(got, '\n')
			path := filepath.Join(dir, tc.name+".json")
			if *updateRunRequests {
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path) //nolint:gosec // fixed test fixture path
			if err != nil {
				t.Fatalf("read golden: %v (run with -update-run-requests to create)", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("model request for %s session differs from %s", tc.name, path)
			}
		})
	}
}
