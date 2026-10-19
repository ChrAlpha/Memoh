package runconfig

import (
	"context"
	"slices"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	attachmentpkg "github.com/felinics/memoh/internal/attachment"
	"github.com/felinics/memoh/internal/media/lottie"
	"github.com/felinics/memoh/internal/models"
)

// discussImages is the vision input of a discuss run's batch. A batch row
// stores only its text and a link to the images it arrived with, so the images
// are read back from the assets here, once the run's context has settled and
// only for rows still in it: a row a summary covers or admission left out
// never has its images loaded.
type discussImages struct {
	// current belongs to the row the run answers, which the request names.
	current []sdk.ImagePart
	// byMessage belongs to the batch's earlier rows, keyed by history row id.
	byMessage map[string][]sdk.ImagePart
	// omitted are the rows whose images the turn's vision budget had no room
	// for, oldest first.
	omitted []string
}

// discussBatchImages loads the images of the batch rows in records. The
// turn-wide vision budget goes to the newest row first: older images are
// history a budget may trim, the row the run answers is not. An image sent
// again belongs to its newest sender. A model without vision gets nothing and
// nothing is read.
func (b *Builder) discussBatchImages(ctx context.Context, model models.GetResponse, req runreq.ChatRequest, records []historyfrag.HistoryRecord) discussImages {
	if !strings.EqualFold(strings.TrimSpace(req.SessionType), sessionmode.Discuss) ||
		len(req.DiscussBatchMessageIDs) == 0 || !supportsImageInputForModel(model) {
		return discussImages{}
	}
	ctx, cancel := context.WithTimeout(ctx, attachmentPreparationTimeout)
	defer cancel()
	inBatch := make(map[string]bool, len(req.DiscussBatchMessageIDs))
	for _, id := range req.DiscussBatchMessageIDs {
		inBatch[id] = true
	}
	current := strings.TrimSpace(req.RequiredHistoryMessageID)
	var (
		result discussImages
		budget visionBudget
		seen   = map[string]bool{}
	)
	for _, record := range slices.Backward(records) {
		if !inBatch[record.DBMessageID] {
			continue
		}
		var parts []sdk.ImagePart
		omitted := false
		for _, asset := range record.Assets {
			if !isImageAsset(asset.Mime) || seen[asset.ContentHash] {
				continue
			}
			seen[asset.ContentHash] = true
			frames, err := b.inlineStoredImageParts(ctx, req.BotID, asset.ContentHash, asset.Mime)
			if err != nil {
				b.logImageInputRejected(err, req.BotID, asset.ContentHash)
				continue
			}
			taken := budget.take(frames)
			omitted = omitted || len(taken) == 0
			parts = append(parts, taken...)
		}
		if omitted {
			result.omitted = append(result.omitted, record.DBMessageID)
		}
		switch {
		case len(parts) == 0:
		case record.DBMessageID == current:
			result.current = parts
		default:
			if result.byMessage == nil {
				result.byMessage = map[string][]sdk.ImagePart{}
			}
			result.byMessage[record.DBMessageID] = parts
		}
	}
	slices.Reverse(result.omitted)
	return result
}

// isImageAsset reports whether a stored asset is vision input by its label. A
// row keeps no attachment type; an animated sticker is the one image the
// label does not give away, and its bytes are what the renderer checks.
func isImageAsset(mime string) bool {
	mime = attachmentpkg.NormalizeMime(mime)
	return attachmentpkg.InferTypeFromMime(mime) == "image" || mime == lottie.MimeTGS
}

// attach puts each earlier row's images on that row's message. sourceIDs names
// the history row behind every message, as ForkContextSourceMessageIDs does.
func (d discussImages) attach(messages []sdk.Message, sourceIDs []string) {
	for i := range messages {
		if i >= len(sourceIDs) || messages[i].Role != sdk.MessageRoleUser {
			continue
		}
		for _, image := range d.byMessage[sourceIDs[i]] {
			messages[i].Content = append(messages[i].Content, image)
		}
	}
}

// record notes the rows whose images the budget left out.
func (d discussImages) record(ledger *contextfrag.MutationLedger) {
	if len(d.omitted) == 0 {
		return
	}
	ledger.Record(contextfrag.MutationCurrentInputImagesOmitted, "sources="+strings.Join(d.omitted, ","))
}
