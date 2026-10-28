package modelretry

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	sdk "github.com/felinics/twilight/sdk"
)

// Overflow is feedback about one rejected input. Zero counts mean the
// provider identified context overflow without reporting token counts.
type Overflow struct {
	InputTokens int
	LimitTokens int
}

var overflowCounts = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^prompt is too long:\s*([0-9]+) tokens > ([0-9]+) maximum`),
	regexp.MustCompile(`(?i)^the input token count \(([0-9]+)\) exceeds the maximum number of tokens allowed \(([0-9]+)\)`),
}

var contextLengthCounts = regexp.MustCompile(`(?i)maximum context length is ([0-9]+) tokens.*?(?:requested|resulted in) ([0-9]+) tokens`)

// ContextOverflow recognizes a provider's explicit token-window rejection,
// including an APIError delivered inside a stream. HTTP 400/413 alone, byte
// limits, output limits and transient errors do not authorize compaction.
func ContextOverflow(err error) (Overflow, bool) {
	var api *sdk.APIError
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &api) {
		return Overflow{}, false
	}
	if api.StatusCode != 0 && api.StatusCode != http.StatusBadRequest && api.StatusCode != http.StatusRequestEntityTooLarge && api.StatusCode != http.StatusUnprocessableEntity {
		return Overflow{}, false
	}
	message := strings.TrimSpace(api.Message)
	for _, pattern := range overflowCounts {
		if match := pattern.FindStringSubmatch(message); match != nil {
			input, _ := strconv.Atoi(match[1])
			limit, _ := strconv.Atoi(match[2])
			return Overflow{InputTokens: input, LimitTokens: limit}, true
		}
	}
	if match := contextLengthCounts.FindStringSubmatch(message); match != nil {
		limit, _ := strconv.Atoi(match[1])
		input, _ := strconv.Atoi(match[2])
		return Overflow{InputTokens: input, LimitTokens: limit}, true
	}
	for _, code := range []string{api.Code, api.Type} {
		switch strings.ToLower(code) {
		case "context_length_exceeded", "context_window_exceeded", "prompt_too_long", "input_tokens_exceeded":
			return Overflow{}, true
		}
	}
	// Anthropic also reports this without counts. Keep the compatibility
	// form specific to its invalid-request envelope, not arbitrary error text.
	return Overflow{}, api.Type == "invalid_request_error" && strings.EqualFold(message, "prompt is too long")
}

// InputBudget tightens the estimate of a rejected input, leaving room for
// tokenizer uncertainty. The bound always falls even without token counts;
// callers bound the number of attempts independently.
func (o Overflow) InputBudget(rejected int) int {
	budget := rejected / 4 * 3
	if o.InputTokens > o.LimitTokens && o.LimitTokens > 0 {
		budget = min(budget, int(float64(rejected)*float64(o.LimitTokens)/float64(o.InputTokens)*0.9))
	}
	return max(1, budget)
}
