package application

import (
	"context"
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
