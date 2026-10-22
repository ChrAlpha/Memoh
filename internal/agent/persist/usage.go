package persist

import "encoding/json"

// ExtractInputTokensFromUsage is the input token count a run's usage payload
// reports, or zero when it reports none.
func ExtractInputTokensFromUsage(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var u struct {
		InputTokens int `json:"inputTokens"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return 0
	}
	return u.InputTokens
}
