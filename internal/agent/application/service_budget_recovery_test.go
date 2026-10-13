package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/contextview"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func chatBudgetFixture(t *testing.T) (*Service, *recoveryHistoryService, native.RunConfig) {
	t.Helper()
	service, _ := newControllerPolicyService(t, nil)
	service.queries = &recoveryHistoryQueries{controllerQueries: service.queries.(*controllerQueries)}
	history := &recoveryHistoryService{}
	service.messageService = history
	cfg := native.RunConfig{RunID: "original-run", Model: &sdk.Model{ID: "original-model"}, Identity: native.SessionContext{BotID: syncCompactBotID, SessionID: syncCompactThreadID}, Messages: []sdk.Message{sdk.UserMessage("current request")}, ContextCurrentUserMessageIndex: intPointer(0), ContextBudgetMaxTokens: 16000, ContextToolDefsResolved: true, ContextToolDefs: []contextfrag.ToolDefAccounting{{Name: "large_roster", TokenEstimate: 10000}}, ContextQueryMaterialized: true, ContextLifecycle: contextfrag.NewLifecycleHolder()}
	return service, history, cfg
}

type recoveryHistoryQueries struct{ *controllerQueries }

func (*recoveryHistoryQueries) ListCompactionArtifactLineageBySession(context.Context, pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	return nil, nil
}

type recoveryHistoryService struct {
	messagepkg.Service
	messages []messagepkg.Message
	loads    int
	maxBytes int64
	err      error
}

func (s *recoveryHistoryService) ListActiveSinceBySessionWithinBytes(_ context.Context, _ string, _ time.Time, maxBytes int64) ([]messagepkg.Message, error) {
	s.loads++
	s.maxBytes = maxBytes
	return append([]messagepkg.Message(nil), s.messages...), s.err
}

func countRecoveryText(messages []sdk.Message, text string) int {
	count := 0
	for _, message := range messages {
		for _, part := range message.Content {
			if value, ok := part.(sdk.TextPart); ok && strings.Contains(value.Text, text) {
				count++
			}
		}
	}
	return count
}

type triggerRecoveryMessages struct {
	*recordingMessageService
	history *recoveryHistoryService
}

func (m triggerRecoveryMessages) ListActiveSinceBySessionWithinBytes(ctx context.Context, sessionID string, since time.Time, maxBytes int64) ([]messagepkg.Message, error) {
	return m.history.ListActiveSinceBySessionWithinBytes(ctx, sessionID, since, maxBytes)
}

func slowRecoveryFixture(t *testing.T) directLifecycleFixture {
	t.Helper()
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess)
	queries := fixture.service.queries.(*directLifecycleQueries)
	queries.compactionEnabled = true
	model := queries.models["direct-lifecycle-model"]
	model.Config = []byte(`{"context_window":8000}`)
	queries.models["direct-lifecycle-model"] = model
	history := &recoveryHistoryService{messages: []messagepkg.Message{{ID: "history-old", BotID: lifecycleTestBotID, SessionID: lifecycleTestSessionID, Role: "assistant", Content: newTextContent(strings.Repeat("h", 14000))}}}
	fixture.service.messageService = triggerRecoveryMessages{recordingMessageService: fixture.messages, history: history}
	logger := slog.New(slog.DiscardHandler)
	fixture.service.agent = native.New(native.Deps{Logger: logger, ContextViewApplier: contextview.ProviderRunConfigApplier(logger)})
	fixture.service.streamIdleTimeout = 100 * time.Millisecond
	return fixture
}

func slowRecoveryRequest() ChatRequest {
	return ChatRequest{
		BotID: lifecycleTestBotID, ChatID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID,
		Query: directLifecyclePrompt + " " + strings.Repeat("q", 6000), UserMessagePersisted: true,
	}
}

func TestOverBudgetChatDoesNotWaitForCompaction(t *testing.T) {
	for _, websocket := range []bool{false, true} {
		t.Run(fmt.Sprintf("websocket=%t", websocket), func(t *testing.T) {
			fixture := slowRecoveryFixture(t)
			runner := newGatedCompactionRunner()
			defer close(runner.release)
			fixture.service.compactionService = runner
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			ctx, endRun := WithRunEnd(ctx)
			if websocket {
				events := make(chan WSStreamEvent, 256)
				outcome, err := fixture.service.StreamChatWS(ctx, slowRecoveryRequest(), events, nil)
				if err != nil || outcome.Cause != nil {
					t.Fatalf("budgeted turn waited for compaction: error=%v outcome=%+v", err, outcome)
				}
			} else {
				chunks, errs := fixture.service.StreamChat(ctx, slowRecoveryRequest())
				for chunks != nil || errs != nil {
					select {
					case _, ok := <-chunks:
						if !ok {
							chunks = nil
						}
					case err, ok := <-errs:
						if !ok {
							errs = nil
						} else if err != nil {
							t.Fatalf("budgeted turn waited for compaction: %v", err)
						}
					}
				}
			}
			if ctx.Err() != nil {
				t.Fatal("turn did not finish before its deadline")
			}
			runner.assertNoPass(t)
			endRun()
			runner.awaitPass(t)
		})
	}
}

func TestPipelineMediaFollowsExplicitCurrentInput(t *testing.T) {
	s, _, cfg := chatBudgetFixture(t)
	cfg.Messages = []sdk.Message{sdk.UserMessage("current"), sdk.UserMessage("self echo"), sdk.UserMessage("memory")}
	cfg.ContextCurrentUserMessageIndex = intPointer(0)
	cfg.ContextMemoryMessageIndex = intPointer(2)
	cfg.InlineImages = []sdk.ImagePart{{Image: "data:image/png;base64,YQ=="}}
	cfg.InlineAttachments = []sdk.MessagePart{sdk.FilePart{Data: "Yg==", MediaType: "application/pdf"}}
	cfg = s.prepareRunConfig(t.Context(), cfg)
	if len(cfg.Messages[0].Content) != 3 || len(cfg.Messages[1].Content) != 1 || len(cfg.Messages[2].Content) != 1 {
		t.Fatalf("media detached from current input: %v", cfg.Messages)
	}
}

func discussHistoryFrag(index int, sourceID string, kind contextfrag.Kind, msg sdk.Message) contextfrag.ContextFrag {
	return contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID: fmt.Sprintf("discuss.message.%03d", index), Message: msg, Kind: kind, Slot: contextfrag.SlotHistory,
		SourceID: sourceID, Collector: discussContextCollector, Index: index,
	})
}

func TestShedOlderBatchImagesChargesTheTrimNotice(t *testing.T) {
	older := sdk.UserMessage("older input", sdk.ImagePart{Image: "data:image/png;base64,YQ=="})
	frags := []contextfrag.ContextFrag{
		discussHistoryFrag(0, "answered", contextfrag.KindConversationEvent, sdk.UserMessage(strings.Repeat("answered ", 500))),
		discussHistoryFrag(1, "older", contextfrag.KindConversationEvent, older),
		discussHistoryFrag(2, "newest", contextfrag.KindCurrentUserMessage, sdk.UserMessage("newest input")),
	}
	batch := map[string]turn.ContextMessageSource{"older": {Kind: "external", ID: "older", Current: true}, "newest": {Kind: "external", ID: "newest", Current: true}}
	notice := contextfrag.ResolveProviderBudgetFragTokens(contextview.TrimNoticeFrag(contextfrag.Scope{}))
	budget := contextfrag.ResolveProviderBudgetFragTokens(frags[1]) + notice - 1
	cfg := native.RunConfig{ContextSourceFrags: frags, Messages: []sdk.Message{sdk.UserMessage(strings.Repeat("answered ", 500)), older, sdk.UserMessage("newest input")}, ContextMutations: contextfrag.NewMutationLedger()}
	if _, shed := shedOlderBatchImages(cfg, batch, budget); !shed {
		t.Fatal("the older input kept an image that leaves no room for the trim notice")
	}
	if _, shed := shedOlderBatchImages(cfg, batch, budget+1); shed {
		t.Fatal("an image that fits beside the trim notice was shed")
	}
}

func TestShedOlderBatchImagesShedsOnlyTheImagesNeeded(t *testing.T) {
	image := func(data string) sdk.ImagePart { return sdk.ImagePart{Image: "data:image/png;base64," + data} }
	album := sdk.UserMessage("album", image("MQ=="), image("Mg=="), image("Mw=="))
	frags := []contextfrag.ContextFrag{
		discussHistoryFrag(0, "album", contextfrag.KindConversationEvent, album),
		discussHistoryFrag(1, "newest", contextfrag.KindCurrentUserMessage, sdk.UserMessage("newest input")),
	}
	batch := map[string]turn.ContextMessageSource{"album": {Kind: "external", ID: "album", Current: true}, "newest": {Kind: "external", ID: "newest", Current: true}}
	cfg := native.RunConfig{ContextSourceFrags: frags, Messages: []sdk.Message{album, sdk.UserMessage("newest input")}, ContextMutations: contextfrag.NewMutationLedger()}
	budget := contextfrag.ResolveProviderBudgetFragTokens(frags[0]) - contextfrag.EstimateImageTokens
	shed, ok := shedOlderBatchImages(cfg, batch, budget)
	if !ok {
		t.Fatal("an album over budget shed nothing")
	}
	var kept []string
	for _, part := range shed.Messages[0].Content {
		if image, isImage := part.(sdk.ImagePart); isImage {
			kept = append(kept, image.Image)
		}
	}
	if len(kept) != 2 || kept[0] != image("Mg==").Image {
		t.Fatalf("album kept %v, want only its oldest image shed", kept)
	}
	if records := cfg.ContextMutations.Records(); len(records) != 1 || records[0].Detail != "sources=album" {
		t.Fatalf("mutations = %+v", records)
	}
}
