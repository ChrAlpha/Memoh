package application

import (
	"context"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// A row refused at admission stays out of the model context, while an admitted
// row in the same session is still loaded.
func TestAssembleContextOmitsRowsRefusedAtAdmission(t *testing.T) {
	t.Parallel()

	refused := map[string]any{}
	messagepkg.SetAdmission(refused, messagepkg.AdmissionRejectedACL)
	rows := []messagepkg.Message{
		assembleText(t, "question", "user", "admitted question", assembleBase.Add(-time.Minute)),
		assembleRow(t, "refused", sdk.UserMessage("refused input"), assembleBase, refused),
	}
	source := &assembleMessageService{after: rows}
	assembled, err := newAssembleService(source).assembleContext(context.Background(), assembleRequest(contextPurposeRun, ChatRequest{}))
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	assertTexts(t, assembled.Messages, "admitted question")
}

// The window re-read that keeps a request's own row in context (a retry whose
// request was compacted away) reads the same stored rows as the initial load,
// so it leaves out refused rows too.
func TestAssembleContextOmitsRefusedRowsFromRequiredWindow(t *testing.T) {
	t.Parallel()

	refused := map[string]any{}
	messagepkg.SetAdmission(refused, messagepkg.AdmissionRejectedACL)
	ask := assembleText(t, "ask", "user", "ask", assembleBase)
	refusedRow := assembleRow(t, "refused", sdk.UserMessage("refused input"), assembleBase.Add(time.Minute), refused)
	answer := assembleText(t, "answer", "assistant", "answer", assembleBase.Add(2*time.Minute))
	later := assembleText(t, "later", "assistant", "later", assembleBase.Add(3*time.Minute))
	source := &assembleMessageService{
		after:   []messagepkg.Message{later},
		visible: []messagepkg.Message{ask, refusedRow, answer, later},
	}
	assembled, err := newAssembleService(source).assembleContext(context.Background(),
		assembleRequest(contextPurposeRun, ChatRequest{RequiredHistoryMessageID: "ask"}))
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	assertTexts(t, assembled.Messages, "ask", "answer", "later")
}

// A required row that was refused at admission is outside the context like any
// other refused row, so the run fails as it does for a row that is not visible.
func TestAssembleContextFailsWhenRequiredRowWasRefused(t *testing.T) {
	t.Parallel()

	refused := map[string]any{}
	messagepkg.SetAdmission(refused, messagepkg.AdmissionRejectedRuntimeDriver)
	source := &assembleMessageService{
		visible: []messagepkg.Message{
			assembleRow(t, "refused", sdk.UserMessage("refused input"), assembleBase, refused),
			assembleText(t, "answer", "assistant", "answer", assembleBase.Add(time.Minute)),
		},
	}
	_, err := newAssembleService(source).assembleContext(context.Background(),
		assembleRequest(contextPurposeRun, ChatRequest{RequiredHistoryMessageID: "refused"}))
	if err == nil || !strings.Contains(err.Error(), "not visible") {
		t.Fatalf("assembleContext error = %v, want the required row reported as not visible", err)
	}
}
