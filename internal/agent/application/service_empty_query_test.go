package application

import (
	"context"
	"net/http"
	"testing"
)

// A request with no query and no attachments is refused unless its input is
// already persisted: the current message is then the history row it names.
func TestResolveRejectsEmptyQueryUnlessInputIsPersisted(t *testing.T) {
	svc := &Service{}

	_, _, err := svc.resolveWithHTTPClient(context.Background(), ChatRequest{BotID: "bot-1", ThreadID: "session-1"}, http.DefaultClient)
	if err == nil || err.Error() != "query or attachments is required" {
		t.Fatalf("empty query without persisted input: err = %v, want the query requirement", err)
	}

	// The persisted-input request passes the query check and stops at the
	// next one, which needs a bot id this request leaves empty.
	_, _, err = svc.resolveWithHTTPClient(context.Background(), ChatRequest{ThreadID: "session-1", ReusePersistedUserMessage: true}, http.DefaultClient)
	if err == nil || err.Error() == "query or attachments is required" {
		t.Fatalf("empty query with persisted input: err = %v, want a failure past the query check", err)
	}
}
