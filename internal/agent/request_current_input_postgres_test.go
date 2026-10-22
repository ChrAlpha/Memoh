package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// sdkUserText returns the text of every user message the provider request
// carried.
func sdkUserText(request sdk.Request) []string {
	var texts []string
	for _, message := range request.Messages {
		if message.Role != sdk.MessageRoleUser {
			continue
		}
		for _, part := range message.Content {
			if text, ok := part.(sdk.TextPart); ok {
				texts = append(texts, text.Text)
			}
		}
	}
	return texts
}

// sdkUserImages counts the image parts on the request's user messages.
func sdkUserImages(request sdk.Request) int {
	total := 0
	for _, message := range request.Messages {
		if message.Role != sdk.MessageRoleUser {
			continue
		}
		for _, part := range message.Content {
			if _, ok := part.(sdk.ImagePart); ok {
				total++
			}
		}
	}
	return total
}

// A run that carries its input as its own query still sends it when the stored
// history was truncated to fit the byte budget, and the header names the
// message and its sender. The image the user attached rides the same message.
func TestDriveQueryInputReachesProviderThroughTruncatedHistory(t *testing.T) {
	s, fixture, recorder := compactionDriveSession(t)
	ctx := s.Ctx
	alice := uuid.NewString()
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO channel_identities (id, channel_type, channel_subject_id, display_name)
		VALUES ($1, 'telegram', $2, 'Alice')
	`, alice, "subject-"+alice); err != nil {
		t.Fatalf("create channel identity: %v", err)
	}
	for i := range 3 {
		text := fmt.Sprintf("old-%d ", i) + strings.Repeat("padding ", 400)
		content, err := json.Marshal(runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.UserMessage(text)})[0])
		if err != nil {
			t.Fatal(err)
		}
		row, err := s.Messages.Persist(ctx, messagepkg.PersistInput{
			BotID: s.BotID, SessionID: s.SessionID, Role: "user", Content: content,
			SenderChannelIdentityID: alice, ExternalMessageID: fmt.Sprintf("m-old-%d", i), DisplayText: text,
		})
		if err != nil {
			t.Fatalf("persist old row %d: %v", i, err)
		}
		s.Clock = s.Clock.Add(time.Minute)
		if _, err := s.Pool.Exec(ctx, `UPDATE bot_history_messages SET created_at = $2 WHERE id = $1`, row.ID, s.Clock); err != nil {
			t.Fatal(err)
		}
	}

	// A 400-token window loads 1600 bytes of content, far below the rows above,
	// and vision lets the attached image route natively.
	for key, model := range fixture.queries.Models {
		model.Config = []byte(`{"context_window":400,"compatibilities":["tool-call","vision"]}`)
		fixture.queries.Models[key] = model
	}

	const prompt = "CURRENT_INPUT"
	handle, err := fixture.service.StartTurn(ctx, turn.StartTurnCommand{
		SchemaVersion:           1,
		TeamID:                  "current-input-team",
		Mode:                    turn.ModeChat,
		BotID:                   agenttest.LifecycleBotID,
		ChatID:                  agenttest.LifecycleBotID,
		ThreadID:                agenttest.LifecycleSessionID,
		Query:                   prompt,
		UserVisibleText:         prompt,
		ExternalMessageID:       "current-message",
		SourceChannelIdentityID: "sender-carol",
		DisplayName:             "Carol",
		CurrentChannel:          "telegram",
		Channels:                []string{"telegram"},
		ReplyTarget:             "current-chat",
		ConversationType:        "private",
		Attachments:             []turn.Attachment{{Type: "image", Base64: agenttest.DataURL("image/png", agenttest.RasterPNG(t)), Mime: "image/png", Name: "photo.png"}},
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	drainTurn(t, handle)

	requests := requireDriveRequests(t, recorder)
	text := strings.Join(sdkUserText(requests[0]), "\n")
	if !strings.Contains(text, prompt) {
		t.Fatalf("the run's input never reached the provider request:\n%s", text)
	}
	if !strings.Contains(text, `id="current-message"`) || !strings.Contains(text, `sender="Carol"`) {
		t.Fatalf("the current input reached the provider without its own source:\n%s", text)
	}
	if strings.Contains(text, "old-0") {
		t.Fatal("the byte budget dropped nothing, so the truncation this test rests on never happened")
	}
	if got := sdkUserImages(requests[0]); got != 1 {
		t.Fatalf("image parts on the request = %d, want the attached image to ride the current input", got)
	}
}
