package runconfig

import (
	"context"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/sessionfacts"
)

func TestRunFactsKeepSessionFactsExceptScheduleDecisions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		session  sessionfacts.Facts
		runMode  string
		decision sessionfacts.DecisionMode
		delivery sessionfacts.ReplyDelivery
	}{
		{name: "chat session", session: sessionfacts.Preset(sessionmode.Chat), runMode: sessionmode.Chat, decision: sessionfacts.DecisionPark, delivery: sessionfacts.ReplyStream},
		{name: "discuss session", session: sessionfacts.Preset(sessionmode.Discuss), runMode: sessionmode.Discuss, decision: sessionfacts.DecisionAutoReject, delivery: sessionfacts.ReplyExplicit},
		{name: "subagent session", session: sessionfacts.Preset(sessionmode.Subagent), runMode: sessionmode.Subagent, decision: sessionfacts.DecisionAutoReject, delivery: sessionfacts.ReplyStream},
		{name: "schedule session", session: sessionfacts.Preset(sessionmode.Schedule), runMode: sessionmode.Schedule, decision: sessionfacts.DecisionAutoReject, delivery: sessionfacts.ReplyStream},
		{name: "schedule fire in a chat session", session: sessionfacts.Preset(sessionmode.Chat), runMode: sessionmode.Schedule, decision: sessionfacts.DecisionAutoReject, delivery: sessionfacts.ReplyStream},
	}
	for _, tt := range tests {
		got := runFacts(tt.session, tt.runMode)
		if got.DecisionMode != tt.decision || got.ReplyDelivery != tt.delivery {
			t.Fatalf("%s: facts = %+v, want decision %q delivery %q", tt.name, got, tt.decision, tt.delivery)
		}
	}
}

// The run follows what the session records, not what its mode label says.
func TestResolveRunConfigReadsRecordedSessionFacts(t *testing.T) {
	t.Parallel()

	recorded := sessionfacts.Preset(sessionmode.Chat)
	resolver := newBuilder(func(d *Deps) {
		d.Sessions = &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{
					ID:          sessionID,
					Type:        session.TypeDiscuss,
					SessionMode: session.TypeDiscuss,
					RuntimeType: session.RuntimeACPAgent,
					Facts:       recorded,
				}, nil
			},
		}
	})

	got, err := resolver.ForSession(context.Background(), "bot-1", "session-1", "user-1", "telegram", "", "group", "")
	if err != nil {
		t.Fatalf("ResolveRunConfig() error = %v", err)
	}
	if got.RunConfig.ReplyDelivery != recorded.ReplyDelivery || got.RunConfig.DecisionMode != recorded.DecisionMode {
		t.Fatalf("run facts = %q/%q, want the recorded %q/%q",
			got.RunConfig.ReplyDelivery, got.RunConfig.DecisionMode, recorded.ReplyDelivery, recorded.DecisionMode)
	}
}

func TestAgentSessionModesMatchPersistedSessionTypes(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		sessionmode.Chat:     session.TypeChat,
		sessionmode.Schedule: session.TypeSchedule,
		sessionmode.Subagent: session.TypeSubagent,
		sessionmode.Discuss:  session.TypeDiscuss,
		sessionmode.ACPAgent: session.TypeACPAgent,
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("agent session mode %q must match persisted type %q", got, want)
		}
	}
}

func TestResolveRunConfigSessionTypeUsesStoredSessionType(t *testing.T) {
	t.Parallel()

	resolver := newBuilder(func(d *Deps) {
		d.Sessions = &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				if sessionID != "session-1" {
					t.Fatalf("unexpected session id: %s", sessionID)
				}
				return session.Thread{ID: sessionID, Type: session.TypeChat}, nil
			},
		}
	})

	if got, _, _ := resolver.sessionDescriptor(context.Background(), "session-1"); got != session.TypeChat {
		t.Fatalf("session type = %q, want %q", got, session.TypeChat)
	}
}

func TestSessionDescriptorFallsBackToChat(t *testing.T) {
	t.Parallel()

	resolver := newBuilder(func(d *Deps) {
		d.Sessions = &agenttest.Sessions{
			GetFn: func(context.Context, string) (session.Thread, error) {
				return session.Thread{}, errors.New("db unavailable")
			},
		}
	})

	if got, _, _ := resolver.sessionDescriptor(context.Background(), "session-1"); got != session.TypeChat {
		t.Fatalf("session type = %q, want %q", got, session.TypeChat)
	}
}

func TestResolveRunConfigSkipsModelResolutionForACPRuntime(t *testing.T) {
	t.Parallel()

	resolver := newBuilder(func(d *Deps) {
		d.Sessions = &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				if sessionID != "session-1" {
					t.Fatalf("unexpected session id: %s", sessionID)
				}
				return session.Thread{
					ID:          sessionID,
					Type:        session.TypeDiscuss,
					SessionMode: session.TypeDiscuss,
					RuntimeType: session.RuntimeACPAgent,
				}, nil
			},
		}
	})

	got, err := resolver.ForSession(context.Background(), "bot-1", "session-1", "user-1", "telegram", "", "group", "")
	if err != nil {
		t.Fatalf("ResolveRunConfig() error = %v", err)
	}
	if got.RuntimeType != session.RuntimeACPAgent {
		t.Fatalf("runtime type = %q, want %q", got.RuntimeType, session.RuntimeACPAgent)
	}
	if got.RunConfig.SessionType != session.TypeDiscuss {
		t.Fatalf("run config session type = %q, want %q", got.RunConfig.SessionType, session.TypeDiscuss)
	}
	if got.ModelID != "" || got.RunConfig.Model != nil {
		t.Fatalf("ACP runtime should not resolve a model, model_id=%q model=%#v", got.ModelID, got.RunConfig.Model)
	}
}
