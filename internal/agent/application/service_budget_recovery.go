package application

import (
	"context"
	"slices"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/contextview"
)

func pauseIdleDuringBudgetRecovery(cfg native.RunConfig, idle *idleCancel) native.RunConfig {
	if recoverBudget := cfg.RecoverContextBudget; recoverBudget != nil {
		cfg.RecoverContextBudget = func(ctx context.Context, cfg native.RunConfig) (native.RunConfig, bool, error) {
			idle.Pause()
			defer idle.Resume()
			return recoverBudget(ctx, cfg)
		}
	}
	return cfg
}

func (s *Service) recoverDiscussContextBudget(ctx context.Context, cmd turn.StartTurnCommand, admitted []turn.DiscussMessage, modelID string, cfg native.RunConfig, compacted *bool) (native.RunConfig, bool, error) {
	plan := cfg.ContextManifest.BudgetPlan
	if plan == nil || ctx.Err() != nil {
		return cfg, false, nil
	}
	batch := make(map[string]turn.ContextMessageSource)
	newestTokens := 0
	for _, message := range admitted {
		if message.Source != nil && message.Source.Current {
			batch[message.Source.ID] = *message.Source
			newestTokens = discussMessageTokens(message)
		}
	}
	cfg, shed := shedOlderBatchImages(cfg, batch, plan.HistoryBudget)
	if *compacted {
		return cfg, shed, nil
	}
	type batchInput struct {
		source turn.ContextMessageSource
		cost   int
	}
	var protected []turn.ContextMessageSource
	var older []batchInput
	available, pressure := plan.HistoryBudget, 0
	for _, frag := range cfg.ContextSourceFrags {
		source, inBatch := batch[frag.Provenance.SourceID]
		switch {
		case frag.Slot == contextfrag.SlotSystem:
		case frag.Slot == contextfrag.SlotCurrentUser || frag.Kind == contextfrag.KindCurrentUserMessage:
			if inBatch {
				protected = append(protected, source)
			}
		case frag.Kind == contextfrag.KindConversationSummary || (frag.Slot == contextfrag.SlotHistory && frag.Kind == contextfrag.KindConversationEvent):
			pressure += contextfrag.ResolveProviderBudgetFragTokens(frag)
			if inBatch {
				// Priced by its text: its images go before it does.
				if msg := contextfrag.FragMessage(frag); msg != nil {
					frag = contextfrag.RebuildFragMessage(frag, withoutImages(*msg))
				}
				older = append(older, batchInput{source: source, cost: contextfrag.ResolveProviderBudgetFragTokens(frag)})
			}
		default:
			available -= contextfrag.ResolveProviderBudgetFragTokens(frag)
		}
	}
	originalHistory := max(0, cmd.DiscussContextTokens-newestTokens)
	pressure = max(pressure, contextfrag.ProviderBudgetTokensFromBytes(int(contextfrag.BudgetBytesForTokens(originalHistory))))
	if pressure <= available || available <= 0 {
		return cfg, shed, nil
	}
	// Older input of the batch stays raw while it fits, leaving a tenth of the
	// allowance for the summary of what does not; only the rest is compacted.
	historyBudget, limit := available, available-available/10
	for i := len(older) - 1; i >= 0 && older[i].cost <= limit; i-- {
		protected = append(protected, older[i].source)
		limit -= older[i].cost
		historyBudget -= older[i].cost
	}
	if historyBudget <= 0 {
		return cfg, shed, nil
	}
	*compacted = true
	result := s.runBudgetCompactionSync(ctx, ChatRequest{BotID: cmd.BotID, ChatID: cmd.BotID, ThreadID: cmd.ThreadID, RunID: cfg.RunID, discussCurrentSources: protected}, pressure, historyBudget, modelID)
	if result.Status != compaction.StatusOK && result.Status != compaction.StatusProgress {
		return cfg, shed, nil
	}
	return cfg, false, native.ErrContextRecompose
}

// shedOlderBatchImages strips images of older input of the batch, oldest
// first, until what must stay fits the budget. Answered history before the
// batch is what selection trims first, so it does not count; an image costs
// far more than its message's text and no summary can carry it, so the text
// survives its images. The newest input keeps its own. It reports whether
// any were shed.
func shedOlderBatchImages(cfg native.RunConfig, batch map[string]turn.ContextMessageSource, historyBudget int) (native.RunConfig, bool) {
	excess, total, trimmable, inBatch := -historyBudget, -historyBudget, false, false
	for _, frag := range cfg.ContextSourceFrags {
		if frag.Slot == contextfrag.SlotSystem || frag.Slot == contextfrag.SlotCurrentUser || frag.Kind == contextfrag.KindCurrentUserMessage {
			continue
		}
		cost := contextfrag.ResolveProviderBudgetFragTokens(frag)
		total += cost
		_, member := batch[frag.Provenance.SourceID]
		inBatch = inBatch || member && frag.Kind == contextfrag.KindConversationEvent
		if inBatch || frag.Slot != contextfrag.SlotHistory || frag.Kind != contextfrag.KindConversationEvent {
			excess += cost
		} else {
			trimmable = true
		}
	}
	if total <= 0 {
		return cfg, false
	}
	if trimmable {
		excess += contextfrag.ResolveProviderBudgetFragTokens(contextview.TrimNoticeFrag(contextfrag.Scope{}))
	}
	var shed []string
	frags, messages := slices.Clone(cfg.ContextSourceFrags), slices.Clone(cfg.Messages)
	for i, frag := range frags {
		if excess <= 0 {
			break
		}
		msg := contextfrag.FragMessage(frag)
		if _, inBatch := batch[frag.Provenance.SourceID]; !inBatch || msg == nil || msg.Role != sdk.MessageRoleUser ||
			frag.Kind != contextfrag.KindConversationEvent || frag.Provenance.Collector != discussContextCollector {
			continue
		}
		// Oldest image first, only as many as the budget needs.
		kept := *msg
		for excess > 0 {
			image := slices.IndexFunc(kept.Content, func(part sdk.MessagePart) bool {
				_, isImage := part.(sdk.ImagePart)
				return isImage
			})
			if image < 0 {
				break
			}
			before := contextfrag.ResolveProviderBudgetFragTokens(frags[i])
			kept.Content = slices.Delete(slices.Clone(kept.Content), image, image+1)
			frags[i] = contextfrag.RebuildFragMessage(frag, kept)
			excess -= before - contextfrag.ResolveProviderBudgetFragTokens(frags[i])
		}
		if len(kept.Content) == len(msg.Content) {
			continue
		}
		if index := frag.Provenance.Index; index >= 0 && index < len(messages) {
			messages[index] = kept
		}
		shed = append(shed, frag.Provenance.SourceID)
	}
	if len(shed) == 0 {
		return cfg, false
	}
	cfg.ContextSourceFrags, cfg.Messages = frags, messages
	if cfg.ContextMutations != nil {
		cfg.ContextMutations.Record(contextfrag.MutationCurrentInputImagesOmitted, "sources="+strings.Join(shed, ","))
	}
	return cfg.RefreshContextFrag(), true
}

func withoutImages(msg sdk.Message) sdk.Message {
	msg.Content = slices.DeleteFunc(slices.Clone(msg.Content), func(part sdk.MessagePart) bool {
		_, image := part.(sdk.ImagePart)
		return image
	})
	return msg
}
