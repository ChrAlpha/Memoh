package outcome_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/models/modelretry"
)

// The outward results of a failed native Agent run as the application layer
// produces them: the public stream event and the history failure marker.
//
// Codes and texts are string literals on purpose: a renamed constant or edited
// catalog entry changes what clients receive, and must fail here.

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
	sdk.Provider
	err error
}

func (charFailingProvider) Name() string { return "char-failing" }

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
func TestNativeStreamErrorClassification(t *testing.T) {
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
func TestFailureEventForNonAppErrorCause(t *testing.T) {
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

func TestAgentStreamFailureConversion(t *testing.T) {
	t.Parallel()

	overloaded := &sdk.APIError{Provider: "anthropic-messages", StatusCode: 529, Kind: sdk.KindServerError, Message: "SECRET overloaded"}
	for _, tc := range []struct {
		name  string
		event native.StreamEvent
		code  apperror.Code
	}{
		{
			name:  "catalogued code",
			event: native.StreamEvent{Type: native.EventError, Code: string(apperror.CodeContextBudgetUnsatisfied), Cause: overloaded},
			code:  apperror.CodeContextBudgetUnsatisfied,
		},
		{
			name:  "uncatalogued code",
			event: native.StreamEvent{Type: native.EventError, Code: "not.a.code", Cause: overloaded},
			code:  apperror.CodeAgentProviderOverloaded,
		},
		{
			name:  "provider answer",
			event: native.StreamEvent{Type: native.EventError, Cause: overloaded},
			code:  apperror.CodeAgentProviderOverloaded,
		},
		{
			name:  "runtime failure",
			event: native.StreamEvent{Type: native.EventError, Cause: errors.New("checkpoint steered model call: SECRET")},
			code:  apperror.CodeAgentResponseInterrupted,
		},
		{
			name:  "no cause",
			event: native.StreamEvent{Type: native.EventError},
			code:  apperror.CodeAgentResponseInterrupted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			failure := outcome.AgentStreamFailure(tc.event)
			if got := apperror.CodeOf(failure); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
			if cause := apperror.CauseOf(failure); !errors.Is(cause, tc.event.Cause) {
				t.Fatalf("cause = %v, want the event's cause %v", cause, tc.event.Cause)
			}
		})
	}
	if err := outcome.AgentStreamFailure(native.StreamEvent{Type: native.EventTextDelta}); err != nil {
		t.Fatalf("agentStreamFailure(text delta) = %v, want nil", err)
	}
}

// The cause of a failure never leaves the application: the published event
// carries the code and its catalog detail, and nothing of the cause.
func TestPublicAgentStreamEventRedactsPrivateFailure(t *testing.T) {
	for _, cause := range []error{
		errors.New("SECRET provider payload"),
		&sdk.APIError{Provider: "openai-completions", StatusCode: 401, Kind: sdk.KindAuthentication, Message: "Incorrect API key provided: SECRET", RequestID: "req_SECRET"},
		errors.New("SECRET window"),
	} {
		event := outcome.PublicAgentStreamEvent(native.StreamEvent{Type: native.EventError, Cause: cause})
		definition, ok := apperror.Lookup(apperror.Code(event.Code))
		if !ok || event.Error != definition.Detail || event.Cause != nil {
			t.Fatalf("public event = %#v, want a catalogued code with its detail", event)
		}
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal public event: %v", err)
		}
		if strings.Contains(string(data), "SECRET") {
			t.Fatalf("private detail leaked: %s", data)
		}
	}
}

func TestAgentFailureStreamEventExposesOnlyStablePublicContract(t *testing.T) {
	t.Parallel()

	t.Run("timeout", func(t *testing.T) {
		cause := apperror.Wrap(apperror.CodeAgentResponseTimeout, errors.New("SECRET provider timeout"), nil)
		event := outcome.AgentFailureStreamEvent(cause)
		if event.Type != native.EventError || event.Code != string(apperror.CodeAgentResponseTimeout) {
			t.Fatalf("timeout event = %#v", event)
		}
		definition, _ := apperror.Lookup(apperror.CodeAgentResponseTimeout)
		if event.Error != definition.Detail {
			t.Fatalf("timeout detail = %q, want %q", event.Error, definition.Detail)
		}
		if strings.Contains(event.Error, "SECRET") {
			t.Fatalf("private timeout cause leaked: %q", event.Error)
		}
	})

	t.Run("uncatalogued failure", func(t *testing.T) {
		event := outcome.AgentFailureStreamEvent(errors.New("SECRET provider response"))
		if event.Code != string(apperror.CodeRuntimeRunFailed) {
			t.Fatalf("fallback code = %q", event.Code)
		}
		if strings.Contains(event.Error, "SECRET") {
			t.Fatalf("private failure cause leaked: %q", event.Error)
		}
	})
}

func TestHasVisibleAgentStreamOutputIgnoresLifecycleOnlyEvents(t *testing.T) {
	t.Parallel()

	cases := []native.StreamEvent{
		{Type: native.EventTextStart},
		{Type: native.EventTextDelta, Delta: "  \n\t"},
		{Type: native.EventTextEnd},
		{Type: native.EventReasoningStart},
		{Type: native.EventReasoningDelta, Delta: ""},
		{Type: native.EventReasoningEnd},
		{Type: native.EventAttachment},
		{Type: native.EventAgentAbort},
	}
	for _, event := range cases {
		if outcome.HasVisibleAgentStreamOutput(event) {
			t.Fatalf("event %q unexpectedly counted as visible", event.Type)
		}
	}

	visible := []native.StreamEvent{
		{Type: native.EventTextDelta, Delta: "hello"},
		{Type: native.EventReasoningDelta, Delta: "thinking"},
		{Type: native.EventAttachment, Attachments: []native.FileAttachment{{Path: "/tmp/a.png"}}},
		{Type: native.EventToolCallStart},
		{Type: native.EventUserInputRequest},
	}
	for _, event := range visible {
		if !outcome.HasVisibleAgentStreamOutput(event) {
			t.Fatalf("event %q unexpectedly counted as invisible", event.Type)
		}
	}
}
