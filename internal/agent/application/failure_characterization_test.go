package application

// Characterization tests for the outward results of a failed native Agent run
// as the application layer produces them: the public stream event, the history
// failure marker, and the terminal state a real session runtime manager records
// for the run. They pin CURRENT behavior so that the failure-classification
// refactor can show exactly which outward values it changes. A value that looks
// wrong is still asserted as it is today and marked "current behavior".
//
// Codes and texts are string literals on purpose: a renamed constant or edited
// catalog entry changes what clients receive, and must fail here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/models/modelretry"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

const (
	charOverloadedDetail  = "The model provider is unavailable or overloaded right now. Please try again in a moment."
	charInterruptedDetail = "The model response was interrupted. Please try again."
	charRunFailedDetail   = "The response could not be completed. Please try again."
	charRejectedDetail    = "The model provider rejected the request. Check the model settings, or try another model."
	charUnreachableDetail = "Memoh could not reach the model provider. Check the provider address and that the service is running."
)

// charProviderErr is a provider's answer as the SDK reports it; its message
// and request id stay in the process.
func charProviderErr(status int, kind sdk.ErrorKind) error {
	return &sdk.APIError{Provider: "openai-completions", StatusCode: status, Kind: kind, Message: "SECRET provider message", RequestID: "req_SECRET"}
}

// charExhausted is the cause the native runtime ends a run with when every
// retry of a model call failed.
func charExhausted(last error) error {
	return errs.Wrap(errs.WrapDependency(last, "model stream"), "model call retries exhausted")
}

// charFailingProvider fails every model call with err.
type charFailingProvider struct {
	abortAlignmentProvider
	err error
}

func (p charFailingProvider) DoStream(context.Context, sdk.Request) (<-chan sdk.StreamPart, error) {
	return nil, p.err
}

// charModelCallFailure is the cause a native run ends with when its model call
// fails with err, retried once when err is worth another call.
func charModelCallFailure(t *testing.T, err error) error {
	t.Helper()
	var cause error
	for event := range native.New(native.Deps{}).Stream(context.Background(), native.RunConfig{
		Model:            &sdk.Model{ID: "char-model", Provider: charFailingProvider{err: err}},
		Messages:         []sdk.Message{sdk.UserMessage("hello")},
		Identity:         native.SessionContext{BotID: "char-bot"},
		ContextMutations: contextfrag.NewMutationLedger(),
		Retry:            modelretry.Config{MaxAttempts: 1, FastAttempts: 1},
	}) {
		if event.Type == native.EventError {
			cause = event.Cause
		}
	}
	if cause == nil {
		t.Fatalf("native run with a model call failing on %v reported no error", err)
	}
	return cause
}

// The classification every native exit shares: an EventError from the runtime
// becomes a lifecycle cause (its code feeds the run's terminal write and the
// history marker) and a public event (what WS, SSE and discuss subscribers see).
func TestCharacterizeNativeStreamErrorClassification(t *testing.T) {
	t.Parallel()
	refused := &url.Error{Op: "Post", URL: "http://127.0.0.1:1/v1/chat/completions", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}
	for _, tc := range []struct {
		name        string
		event       native.StreamEvent
		code        string
		historyCode string
		public      string
	}{
		{
			name:        "retries exhausted on 503",
			event:       native.StreamEvent{Type: native.EventError, Cause: charExhausted(charProviderErr(503, sdk.KindServerError))},
			code:        "agent.provider_overloaded",
			historyCode: "agent.provider_overloaded",
			public:      `{"type":"error","code":"agent.provider_overloaded","error":"` + charOverloadedDetail + `"}`,
		},
		{
			name:        "retries exhausted on a rate limit",
			event:       native.StreamEvent{Type: native.EventError, Cause: charExhausted(charProviderErr(429, sdk.KindRateLimited))},
			code:        "agent.provider_rate_limited",
			historyCode: "agent.provider_rate_limited",
		},
		{
			name:        "request the provider rejected",
			event:       native.StreamEvent{Type: native.EventError, Cause: errs.WrapDependency(charProviderErr(400, sdk.KindUnknown), "model stream")},
			code:        "agent.provider_request_rejected",
			historyCode: "agent.provider_request_rejected",
			public:      `{"type":"error","code":"agent.provider_request_rejected","error":"` + charRejectedDetail + `"}`,
		},
		{
			name:        "key without access to the model",
			event:       native.StreamEvent{Type: native.EventError, Cause: errs.WrapDependency(charProviderErr(403, sdk.KindPermissionDenied), "model stream")},
			code:        "agent.provider_permission_denied",
			historyCode: "agent.provider_permission_denied",
		},
		{
			name:        "provider not listening",
			event:       native.StreamEvent{Type: native.EventError, Cause: charModelCallFailure(t, refused)},
			code:        "agent.provider_unreachable",
			historyCode: "agent.provider_unreachable",
			public:      `{"type":"error","code":"agent.provider_unreachable","error":"` + charUnreachableDetail + `"}`,
		},
		{
			// The request may have gone anywhere, so it names no provider.
			name:        "refused connection in the runtime's own work",
			event:       native.StreamEvent{Type: native.EventError, Cause: errs.Wrap(refused, "approval handler")},
			code:        "agent.response_interrupted",
			historyCode: "agent.response_interrupted",
			public:      `{"type":"error","code":"agent.response_interrupted","error":"` + charInterruptedDetail + `"}`,
		},
		{
			name:        "stream start failure",
			event:       native.StreamEvent{Type: native.EventError, Cause: errors.New("stream start: SECRET dispatch failure")},
			code:        "agent.response_interrupted",
			historyCode: "agent.response_interrupted",
			public:      `{"type":"error","code":"agent.response_interrupted","error":"` + charInterruptedDetail + `"}`,
		},
		{
			name:        "status text without an APIError",
			event:       native.StreamEvent{Type: native.EventError, Cause: errors.New("api error 429: Too Many Requests")},
			code:        "agent.response_interrupted",
			historyCode: "agent.response_interrupted",
		},
		{
			name:        "no cause",
			event:       native.StreamEvent{Type: native.EventError},
			code:        "agent.response_interrupted",
			historyCode: "agent.response_interrupted",
		},
		{
			name:        "failed steer checkpoint",
			event:       native.StreamEvent{Type: native.EventError, Cause: errs.Wrap(errors.New("SECRET ledger"), "checkpoint steered model call")},
			code:        "agent.response_interrupted",
			historyCode: "agent.response_interrupted",
			public:      `{"type":"error","code":"agent.response_interrupted","error":"` + charInterruptedDetail + `"}`,
		},
		{
			// A catalogued code on the event wins over its cause.
			name:        "coded event",
			event:       native.StreamEvent{Type: native.EventError, Code: "context.budget_unsatisfied", Cause: charProviderErr(503, sdk.KindServerError)},
			code:        "context.budget_unsatisfied",
			historyCode: "",
		},
		{
			// An unknown code is ignored and the cause decides.
			name:        "uncatalogued code",
			event:       native.StreamEvent{Type: native.EventError, Code: "not.a.code", Cause: charProviderErr(529, sdk.KindServerError)},
			code:        "agent.provider_overloaded",
			historyCode: "agent.provider_overloaded",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cause := outcome.AgentStreamFailure(tc.event)
			if got := string(apperror.CodeOf(cause)); got != tc.code {
				t.Fatalf("lifecycle code = %q, want %q", got, tc.code)
			}
			if got := string(outcome.HistoryFailureCode(outcome.PublicFailureCode(cause))); got != tc.historyCode {
				t.Fatalf("history failure code = %q, want %q", got, tc.historyCode)
			}
			public := outcome.PublicAgentStreamEvent(tc.event)
			if public.Code != tc.code {
				t.Fatalf("public event code = %q, want %q", public.Code, tc.code)
			}
			data, err := json.Marshal(public)
			if err != nil {
				t.Fatalf("marshal public event: %v", err)
			}
			if tc.public != "" && string(data) != tc.public {
				t.Fatalf("public event = %s, want %s", data, tc.public)
			}
			if strings.Contains(string(data), "SECRET") {
				t.Fatalf("public event leaked the cause: %s", data)
			}
		})
	}
}

// Scenario 10: a failure cause without an apperror code gets a public event,
// named by the run failure default because nothing else named it.
func TestCharacterizeFailureEventForNonAppErrorCause(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{errors.New("SECRET provider response"), context.Canceled} {
		data, err := json.Marshal(outcome.AgentFailureStreamEvent(cause))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if want := `{"type":"error","code":"runtime_run_failed","error":"` + charRunFailedDetail + `"}`; string(data) != want {
			t.Fatalf("failure event for %v = %s, want %s", cause, data, want)
		}
	}
	// The history marker is not written for such a cause: outcome.HistoryFailureCode
	// only accepts the codes it lists.
	if got := outcome.HistoryFailureCode(outcome.PublicFailureCode(errors.New("SECRET provider response"))); got != "" {
		t.Fatalf("history failure code = %q, want none", got)
	}
}

// History exit: a failure without assistant output stores the user message and
// an empty assistant message carrying the failure marker.
func TestCharacterizeHistoryFailureMarker(t *testing.T) {
	t.Parallel()
	for _, code := range []apperror.Code{apperror.CodeAgentProviderOverloaded, apperror.CodeAgentResponseInterrupted} {
		messages := &agenttest.RecordingMessages{}
		service := wireComponents(&Service{messageService: messages, logger: slog.New(slog.DiscardHandler)})
		if _, err := service.persist.TerminalSnapshotResult(context.Background(),
			runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "hello"},
			runreq.ResolvedContext{}, persist.TerminalSnapshot{FailureCode: code}); err != nil {
			t.Fatalf("persistTerminalSnapshotResult: %v", err)
		}
		if len(messages.Persisted) != 2 || messages.Persisted[0].Role != "user" || messages.Persisted[1].Role != "assistant" {
			t.Fatalf("persisted = %#v, want user + assistant", messages.Persisted)
		}
		metadata := messages.Persisted[1].Metadata
		if metadata["agent_step_interrupted"] != true || metadata["error_code"] != string(code) {
			t.Fatalf("assistant metadata = %#v, want interrupted with error_code %q", metadata, code)
		}
		if _, hasError := metadata["error"]; hasError {
			t.Fatalf("assistant metadata = %#v, native failure has no error text key", metadata)
		}
	}
}

// History exit with visible partial output: the partial answer is stored and
// the failure code is not.
//
// Current behavior: a turn that failed after printing text looks like a normal
// assistant message in history.
func TestCharacterizeHistoryPartialOutputDropsFailureCode_CurrentBehavior(t *testing.T) {
	t.Parallel()
	messages := &agenttest.RecordingMessages{}
	service := wireComponents(&Service{messageService: messages, logger: slog.New(slog.DiscardHandler)})
	partial := persist.TerminalSnapshot{
		SDKMessages:   []sdk.Message{sdk.AssistantMessage("partial answer")},
		VisibleOutput: true,
		FailureCode:   apperror.CodeAgentProviderOverloaded,
	}
	if _, err := service.persist.TerminalSnapshotResult(context.Background(),
		runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "hello"}, runreq.ResolvedContext{}, partial); err != nil {
		t.Fatalf("persistTerminalSnapshotResult: %v", err)
	}

	if len(messages.Persisted) != 2 {
		t.Fatalf("persisted = %#v, want user + assistant", messages.Persisted)
	}
	metadata := messages.Persisted[1].Metadata
	if _, has := metadata[messagepkg.HistoryErrorCodeMetadataKey]; has {
		t.Fatalf("assistant metadata = %#v, current behavior stores no error_code", metadata)
	}

	// Without visible output the same partial snapshot becomes the failure marker.
	hidden := &agenttest.RecordingMessages{}
	service = wireComponents(&Service{messageService: hidden, logger: slog.New(slog.DiscardHandler)})
	partial.VisibleOutput = false
	if _, err := service.persist.TerminalSnapshotResult(context.Background(),
		runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "hello"}, runreq.ResolvedContext{}, partial); err != nil {
		t.Fatalf("persistTerminalSnapshotResult: %v", err)
	}
	if len(hidden.Persisted) != 2 || hidden.Persisted[1].Metadata["error_code"] != "agent.provider_overloaded" {
		t.Fatalf("persisted without visible output = %#v, want failure marker", hidden.Persisted)
	}
}

func assertStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s = %s\nwant %s", what, gotJSON, wantJSON)
	}
}

// Scenario 10 through FinishRun: whatever the cause, only its catalogued
// code reaches the runtime.
//
// Current behavior: a cause without a code finishes as runtime_run_failed; an
// External Agent cause finishes with its code.
func TestCharacterizeFinishRunCodes_CurrentBehavior(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status string
		cause  error
		want   [3]string
	}{
		{"plain cause", sessionruntime.RunStatusErrored, errors.New("SECRET adapter crashed"), [3]string{"failed", "runtime_run_failed", ""}},
		{"coded cause", sessionruntime.RunStatusErrored, apperror.Wrap(apperror.CodeWorkspaceUnreachable, errors.New("dial"), nil), [3]string{"failed", "workspace.unreachable", ""}},
		{"external agent cause", sessionruntime.RunStatusErrored, apperror.New(apperror.CodeACPAgentNotConfigured, nil), [3]string{"failed", "acp_agent_not_configured", ""}},
		{"errored without cause", sessionruntime.RunStatusErrored, nil, [3]string{"failed", "runtime_run_failed", ""}},
		{"aborted", sessionruntime.RunStatusAborted, context.Canceled, [3]string{"aborted", "", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runs := sessionledger.New()
			manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
				OwnerID: "owner-finisher", OwnerLeaseTTL: time.Minute, Ledger: runs, Fence: abortAlignmentFence{},
			})
			t.Cleanup(func() { _ = manager.Close() })
			service := wireComponents(&Service{logger: slog.New(slog.DiscardHandler)})
			bindRuntime(service, manager)
			admission, err := manager.Admit(context.Background(), sessionruntime.AdmitInput{
				BotID: "bot-1", SessionID: "session-1", InvocationID: "invocation-finisher", Payload: []byte(`{}`),
				Execution: sessionruntime.Execution{
					Admission: func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
						return sessionruntime.RunAdmissionView{}, nil
					},
				},
			})
			if err != nil {
				t.Fatalf("admit: %v", err)
			}
			service.driver.FinishRun(context.Background(), drive.AdmittedRun{Handle: admission.Handle, Mode: sessionmode.Chat}, drive.FromOutcome(outcome.RunOutcome{Status: tc.status, Cause: tc.cause}))
			run, err := runs.Get(context.Background(), admission.RunID)
			if err != nil {
				t.Fatalf("load run: %v", err)
			}
			if got := [3]string{string(run.State), run.ErrorCode, run.ErrorMessage}; got != tc.want {
				t.Fatalf("session_runs = %q, want %q", got, tc.want)
			}
		})
	}
}

// Scenario 7 (X7): a decision continuation that fails after the run resumed.
//
// Current behavior: session_runs records the cause's catalogued code in
// error_code and no message; a cause without one finishes as
// runtime_run_failed. A continuation stopped while it ran is aborted with no
// code, even when the call it interrupted wraps the cancellation in a failure
// of its own.
func TestCharacterizeDecisionContinuationFailureColumns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cause   error
		stopped bool
		want    [3]string
	}{
		{"coded cause", apperror.Wrap(apperror.CodeSessionHistoryInconsistent, errors.New("SECRET"), nil), false, [3]string{"failed", "session_runtime.history_inconsistent", ""}},
		{"plain cause", errors.New("SECRET continuation failed"), false, [3]string{"failed", "runtime_run_failed", ""}},
		{"wrapped cancellation", fmt.Errorf("SECRET provider stream: %w", context.Canceled), true, [3]string{"aborted", "", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runs := sessionledger.New()
			manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
				OwnerID: "owner-continuation", OwnerLeaseTTL: time.Minute, Ledger: runs, Fence: abortAlignmentFence{},
			})
			t.Cleanup(func() { _ = manager.Close() })
			admission, err := manager.Admit(context.Background(), sessionruntime.AdmitInput{
				BotID: "bot-1", SessionID: "session-1", InvocationID: "invocation-continuation", Payload: []byte(`{}`),
				Execution: sessionruntime.Execution{
					Admission: func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
						return sessionruntime.RunAdmissionView{}, nil
					},
				},
			})
			if err != nil {
				t.Fatalf("admit: %v", err)
			}
			service := continuationService(manager)
			command := sessionruntime.Command{
				BotID: admission.Handle.BotID, SessionID: admission.Handle.SessionID,
				RunID: admission.Handle.RunID, Generation: admission.Handle.Generation,
			}
			runCtx, release, handle, err := manager.DecisionContinuationContext(command)
			if err != nil {
				t.Fatalf("decision continuation context: %v", err)
			}
			defer release()
			if tc.stopped {
				var stop context.CancelFunc
				runCtx, stop = context.WithCancel(runCtx)
				stop()
			}
			service.driveRoutedContinuation(runCtx, command, handle, decisionContinuation{
				Req: runreq.ChatRequest{BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID, RunHandle: handle, UserMessagePersisted: true},
				Setup: func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
					return drive.RunPlan{}, tc.cause
				},
			})
			run, err := runs.Get(context.Background(), admission.RunID)
			if err != nil {
				t.Fatalf("load run: %v", err)
			}
			if got := [3]string{string(run.State), run.ErrorCode, run.ErrorMessage}; got != tc.want {
				t.Fatalf("session_runs = %q, want %q", got, tc.want)
			}
		})
	}
}
