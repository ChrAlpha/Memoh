package contextfrag

import (
	"encoding/json"

	sdk "github.com/memohai/twilight-ai/sdk"
)

// EstimateBytesPerToken is the byte-per-token heuristic shared by every
// context ledger (selection budgets, compaction triggers, manifest
// accounting), and the single swap point for a real tokenizer.
const EstimateBytesPerToken = 4

// TokensFromBytes converts a byte count to the shared token estimate.
func TokensFromBytes(n int) int {
	if n <= 0 {
		return 0
	}
	return n / EstimateBytesPerToken
}

// EstimateSDKMessageTokens estimates tokens for one SDK message additively
// across all parts: text and reasoning count their raw bytes, tool calls and
// tool results count their serialized payload, images are excluded because
// their token cost is resolution-dependent and tracked as a separate count.
func EstimateSDKMessageTokens(msg sdk.Message) int {
	return TokensFromBytes(sdkMessageEstimateBytes(msg))
}

func sdkMessageEstimateBytes(msg sdk.Message) int {
	total := 0
	for _, part := range msg.Content {
		switch p := part.(type) {
		case sdk.TextPart:
			total += len(p.Text)
		case sdk.ReasoningPart:
			total += len(p.Text)
		case sdk.ImagePart:
		default:
			if data, err := json.Marshal(part); err == nil {
				total += len(data)
			}
		}
	}
	return total
}

// EstimateFragTokens computes the token estimate from the fragment's parts,
// ignoring any preset TokenEstimate.
func EstimateFragTokens(frag ContextFrag) int {
	return TokensFromBytes(fragEstimateBytes(frag))
}

func fragEstimateBytes(frag ContextFrag) int {
	total := 0
	for _, part := range frag.Parts {
		switch part.Type {
		case PartText:
			total += len(part.Text)
		case PartSDKMessage:
			if msg := partMessage(part); msg != nil {
				total += sdkMessageEstimateBytes(*msg)
			}
		case PartImage:
		}
	}
	return total
}

// ResolveFragTokens returns the fragment's authoritative token estimate:
// the collector-provided TokenEstimate when set (which may carry real
// provider usage), otherwise the computed part estimate.
func ResolveFragTokens(frag ContextFrag) int {
	if frag.TokenEstimate > 0 {
		return frag.TokenEstimate
	}
	return EstimateFragTokens(frag)
}
