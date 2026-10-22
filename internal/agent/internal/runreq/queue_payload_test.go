package runreq

import (
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
)

func TestQueuePayloadTextNeverExposesCommandEnvelope(t *testing.T) {
	for _, tc := range []struct{ payload, want string }{
		{`{"text":"hello"}`, "hello"},
		{`{"text":"", "command":{"Token":"PRIVATE","ChatToken":"PRIVATE","Query":""}}`, ""},
		{`{"command":{"Token":"PRIVATE","UserVisibleText":"display","Query":"wrapped"}}`, "display"},
		{`{"command":{"Token":"PRIVATE","Query":"query"}}`, "query"},
		{`{"command":{"Token":"PRIVATE"`, ""},
		{`unversioned raw text`, ""},
	} {
		if got := turnwire.ParseQueuedInput([]byte(tc.payload)).VisibleText(); got != tc.want {
			t.Errorf("visible text=%q, want %q", got, tc.want)
		}
	}
}

// Steers and follow-ups share the rewrite. An item without a recorded sender
// user (a platform sender with no linked account, or a payload without a
// command) has nobody who may edit it.
func TestRewriteQueuePayloadTextRequiresRecordedSender(t *testing.T) {
	withSender, err := EncodeQueueCommand(turn.StartTurnCommand{TeamID: "team1", BotID: "bot", ThreadID: "session", UserID: "user-1", Query: "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	withoutSender, err := EncodeQueueCommand(turn.StartTurnCommand{TeamID: "team1", BotID: "bot", ThreadID: "session", SourceChannelIdentityID: "ci-1", Query: "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		payload []byte
		editor  string
		ok      bool
	}{
		"sender":            {withSender, "user-1", true},
		"other user":        {withSender, "user-2", false},
		"no editor":         {withSender, "", false},
		"unlinked sender":   {withoutSender, "user-1", false},
		"text-only payload": {[]byte(`{"text":"hi"}`), "user-1", false},
	} {
		_, err := RewriteQueuePayloadText(tc.payload, tc.editor, "new")
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, agentapi.ErrQueueItemNotEditable)) {
			t.Errorf("%s: error = %v, want ok=%v", name, err, tc.ok)
		}
	}
}
