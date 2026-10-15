package application

import "testing"

func TestQueuePayloadTextNeverExposesCommandEnvelope(t *testing.T) {
	for _, tc := range []struct{ payload, want string }{
		{`{"text":"hello"}`, "hello"},
		{`{"text":"", "command":{"Token":"PRIVATE","ChatToken":"PRIVATE","Query":""}}`, ""},
		{`{"command":{"Token":"PRIVATE","UserVisibleText":"display","Query":"wrapped"}}`, "display"},
		{`{"command":{"Token":"PRIVATE","Query":"query"}}`, "query"},
		{`{"command":{"Token":"PRIVATE"`, ""},
		{`unversioned raw text`, ""},
	} {
		if got := QueuePayloadText([]byte(tc.payload)); got != tc.want {
			t.Errorf("visible text=%q, want %q", got, tc.want)
		}
	}
}

// testQueueInput is what an ingress hands the application for one queued text:
// the team and sender it authenticated, next to the session and the text.
func testQueueInput(botID, sessionID, invocationID, text string) QueueInput {
	return QueueInput{
		TeamID: "team1", BotID: botID, SessionID: sessionID, InvocationID: invocationID,
		UserID: "user-1", SourceChannelIdentityID: "user-1", Text: text,
	}
}
