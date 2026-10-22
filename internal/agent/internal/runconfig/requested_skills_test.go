package runconfig

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/slash"
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

func TestRejectRequestedSkillsAllowsModelChatSession(t *testing.T) {
	resolver := newBuilder(func(d *Deps) {
		d.Sessions = &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{
					ID:          sessionID,
					BotID:       "bot-1",
					Type:        session.TypeChat,
					SessionMode: session.TypeChat,
					RuntimeType: session.RuntimeModel,
				}, nil
			},
		}
	})

	req := requestedSkillGuardRequest("session-1")
	req.InjectCh = make(chan runreq.InjectMessage)
	if err := resolver.RejectRequestedSkillsIfUnsupportedContext(context.Background(), req); err != nil {
		t.Fatalf("rejectRequestedSkillsIfUnsupportedContext() error = %v, want nil", err)
	}
}

func TestRejectRequestedSkillsAllowsPersistedSkillActivation(t *testing.T) {
	resolver := newBuilder(func(d *Deps) {
		d.Sessions = &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{
					ID:          sessionID,
					BotID:       "bot-1",
					Type:        session.TypeChat,
					SessionMode: session.TypeChat,
					RuntimeType: session.RuntimeModel,
				}, nil
			},
		}
	})

	req := requestedSkillGuardRequest("session-1")
	req.UserMessagePersisted = true
	req.UserMessageKind = runreq.UserMessageKindSkillActivation
	if err := resolver.RejectRequestedSkillsIfUnsupportedContext(context.Background(), req); err != nil {
		t.Fatalf("rejectRequestedSkillsIfUnsupportedContext() error = %v, want nil", err)
	}
}

func TestRejectReservedSkillMetadataIfPresentRejectsAttachments(t *testing.T) {
	req := runreq.ChatRequest{
		Attachments: []runreq.ChatAttachment{{
			Type:     "image",
			Metadata: map[string]any{"model_requested_skills": []string{"alpha"}},
		}},
	}
	err := runreq.RejectReservedSkillMetadataIfPresent(req)
	var slashErr slash.Error
	if !errors.As(err, &slashErr) || slashErr.Code != slash.CodeReservedSkillMetadata {
		t.Fatalf("err = %#v, want %s", err, slash.CodeReservedSkillMetadata)
	}
}

func TestRejectReservedSkillMetadataIfPresentRejectsMessageContentPartMetadata(t *testing.T) {
	content, err := json.Marshal([]runreq.ContentPart{{
		Type:     "text",
		Text:     "hello",
		Metadata: map[string]any{"requested_skills": []string{"alpha"}},
	}})
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	req := runreq.ChatRequest{
		Messages: []runreq.ModelMessage{{
			Role:    "user",
			Content: content,
		}},
	}

	err = runreq.RejectReservedSkillMetadataIfPresent(req)
	var slashErr slash.Error
	if !errors.As(err, &slashErr) || slashErr.Code != slash.CodeReservedSkillMetadata {
		t.Fatalf("err = %#v, want %s", err, slash.CodeReservedSkillMetadata)
	}
}

func TestRejectRequestedSkillsRejectsUnsupportedContexts(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		mutate    func(*runreq.ChatRequest)
		session   session.Thread
	}{
		{
			name:      "missing session",
			sessionID: "",
		},
		{
			name:      "discuss session",
			sessionID: "session-1",
			session: session.Thread{
				ID:          "session-1",
				BotID:       "bot-1",
				Type:        session.TypeDiscuss,
				SessionMode: session.TypeDiscuss,
				RuntimeType: session.RuntimeModel,
			},
		},
		{
			name:      "schedule run mode",
			sessionID: "session-1",
			mutate: func(req *runreq.ChatRequest) {
				req.SessionType = session.TypeSchedule
			},
			session: session.Thread{
				ID:          "session-1",
				BotID:       "bot-1",
				Type:        session.TypeChat,
				SessionMode: session.TypeChat,
				RuntimeType: session.RuntimeModel,
			},
		},
		{
			name:      "already persisted user message",
			sessionID: "session-1",
			mutate: func(req *runreq.ChatRequest) {
				req.UserMessagePersisted = true
			},
			session: session.Thread{
				ID:          "session-1",
				BotID:       "bot-1",
				Type:        session.TypeChat,
				SessionMode: session.TypeChat,
				RuntimeType: session.RuntimeModel,
			},
		},
		{
			name:      "ACP runtime",
			sessionID: "session-1",
			session: session.Thread{
				ID:          "session-1",
				BotID:       "bot-1",
				Type:        session.TypeChat,
				SessionMode: session.TypeChat,
				RuntimeType: session.RuntimeACPAgent,
			},
		},
		{
			name:      "legacy ACP session",
			sessionID: "session-1",
			session: session.Thread{
				ID:    "session-1",
				BotID: "bot-1",
				Type:  session.TypeACPAgent,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolver := newBuilder(func(d *Deps) {
				d.Sessions = &agenttest.Sessions{
					GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
						if sessionID != tc.sessionID {
							t.Fatalf("session id = %q, want %q", sessionID, tc.sessionID)
						}
						return tc.session, nil
					},
				}
			})
			if tc.sessionID == "" {
				resolver.sessions = nil
			}

			req := requestedSkillGuardRequest(tc.sessionID)
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			err := resolver.RejectRequestedSkillsIfUnsupportedContext(context.Background(), req)
			var slashErr slash.Error
			if !errors.As(err, &slashErr) || slashErr.Code != slash.CodeUnsupportedSkillSlashContext {
				t.Fatalf("error = %v, want %s", err, slash.CodeUnsupportedSkillSlashContext)
			}
		})
	}
}

func TestResolveRejectsRequestedSkillsBeforeModelResolution(t *testing.T) {
	resolver := newBuilder()
	_, _, err := resolver.Resolve(context.Background(), runreq.ChatRequest{
		BotID:       "bot-1",
		ChatID:      "chat-1",
		ThreadID:    "session-1",
		SessionType: session.TypeSchedule,
		Query:       "scheduled prompt",
		RequestedSkills: []runreq.RequestedSkillContext{{
			Name:    "alpha",
			Content: "skill content",
		}},
	})
	var slashErr slash.Error
	if !errors.As(err, &slashErr) || slashErr.Code != slash.CodeUnsupportedSkillSlashContext {
		t.Fatalf("resolve() error = %v, want %s", err, slash.CodeUnsupportedSkillSlashContext)
	}
}
