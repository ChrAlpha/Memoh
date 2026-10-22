package externalrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// A failed External Agent round records the code the executor names its cause
// with, and nothing beside it.
func TestPersistExternalRoundFailureCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		cause    error
		wantCode string
		wantArgs map[string]any
	}{
		{name: "catalogued code", cause: apperror.Wrap(apperror.CodeAgentResponseTimeout, errors.New("SECRET"), nil), wantCode: "agent.response_timeout"},
		{name: "external agent code", cause: fmt.Errorf("prompt: %w", apperror.New(apperror.CodeACPRuntimeBusy, nil)), wantCode: "acp_runtime_busy"},
		{name: "plain error", cause: errors.New("SECRET driver exit"), wantCode: "runtime_prompt_failed"},
		{
			name:     "missing dependency",
			cause:    &external.DependencyMissingError{DependencyID: "codex", TaskID: "task-1"},
			wantCode: "agent_dependency_missing",
			wantArgs: map[string]any{"dep_id": "codex", "install_task_id": "task-1", "operation_in_progress": "false"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			messages := &agenttest.RecordingMessages{}
			service := testHost(t, Deps{Messages: messages})
			if err := service.persistRound(
				context.Background(),
				runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "run"},
				"codex",
				"/data/app",
				external.PromptResult{},
				externalRoundFailure(tc.cause),
				false,
				nil,
				nil,
			); err != nil {
				t.Fatalf("persistRound() error = %v", err)
			}
			if len(messages.Persisted) != 2 {
				t.Fatalf("persisted %d messages, want user + assistant", len(messages.Persisted))
			}
			meta := messages.Persisted[1].Metadata
			if got, _ := meta["error_code"].(string); got != tc.wantCode {
				t.Fatalf("error_code = %q, want %q", got, tc.wantCode)
			}
			stored, _ := json.Marshal(meta[messagepkg.HistoryErrorArgsMetadataKey])
			var gotArgs map[string]any
			_ = json.Unmarshal(stored, &gotArgs)
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Fatalf("error_args = %s, want %v", stored, tc.wantArgs)
			}
			for _, key := range []string{"error_reason", "i18n_key"} {
				if got, found := meta[key]; found {
					t.Fatalf("%s = %#v, want it absent", key, got)
				}
			}
		})
	}
}
