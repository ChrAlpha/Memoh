package compaction

import (
	"net/http"
	"time"
)

// Compaction result statuses reported by RunCompactionSync.
const (
	StatusOK   = "ok"   // messages were compacted into a summary
	StatusNoop = "noop" // nothing to compact (already compact, cooled down, or in flight)
)

// Reasons a compaction ended as a noop. Every noop carries one.
const (
	ReasonNoModel         = "no_model"          // no summarizer is configured or the configured one is disabled
	ReasonCooldown        = "cooldown"          // a recent failure is still cooling down
	ReasonInFlight        = "in_flight"         // another compaction owns the session and finished without a summary
	ReasonCanceled        = "canceled"          // the caller stopped waiting for the owner
	ReasonNoCandidates    = "no_candidates"     // every row is protected, or none fits the read budget
	ReasonBelowTarget     = "below_target"      // unprotected rows exist, but the rows above target fit inside it
	ReasonNoCompleteGroup = "no_complete_group" // every selected group rendered empty, so none can be summarized
)

// Result is the scoped outcome of a synchronous compaction. Callers use it to
// respond with this session's own result instead of reading unscoped bot-wide
// logs. A failed attempt returns an error, not a Result.
type Result struct {
	Status       string
	Reason       string // set when Status is StatusNoop
	Summary      string
	MessageCount int
}

// Log represents a compaction log entry.
type Log struct {
	ID           string     `json:"id"`
	BotID        string     `json:"bot_id"`
	SessionID    string     `json:"session_id,omitempty"`
	Status       string     `json:"status"`
	Summary      string     `json:"summary"`
	MessageCount int        `json:"message_count"`
	ErrorMessage string     `json:"error_message"`
	Usage        any        `json:"usage,omitempty"`
	ModelID      string     `json:"model_id,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
} // @name compaction.Log

// ListLogsResponse is the API response for listing compaction logs.
type ListLogsResponse struct {
	Items      []Log `json:"items"`
	TotalCount int64 `json:"total_count"`
} // @name compaction.ListLogsResponse

// TriggerConfig holds the parameters needed to trigger a compaction.
type TriggerConfig struct {
	BotID                 string
	SessionID             string
	ModelID               string // runtime model slug sent to the provider
	ModelRecordID         string // models.id row UUID recorded as artifact/log provenance
	ClientType            string
	APIKey                string //nolint:gosec // runtime credential, not a hardcoded secret
	CodexAccountID        string
	BaseURL               string
	ChatCompletionsCompat string
	HTTPClient            *http.Client
	Ratio                 int
	TotalInputTokens      int
	MaxCompactTokens      int // if > 0, cap compaction input to this many tokens (e.g. 85% of the summarizer window)
	TargetTokens          int // if > 0, compaction goal: reduce the whole request to this many tokens (used by sync compaction)
	// FixedTokens is the part of the judged request that no row can shrink:
	// system prompt, tool definitions, and messages no history row holds. The
	// rows are aimed at TargetTokens - FixedTokens, so the target and the
	// pressure that triggered it measure the same request.
	FixedTokens int
	// HistoryCutoff limits the compaction to the history before a message the
	// caller is about to replace. Nil means the whole visible history.
	HistoryCutoff *HistoryCutoff
	// SummaryWindowTokens is the summarizer model's full context window; it
	// bounds the summary output reserve. Zero means the model declares none,
	// and the service's fallback window applies.
	SummaryWindowTokens int
	// ContextWindowTokens is the chat model's context window. It is separate
	// from SummaryWindowTokens, which belongs to the summarizer model.
	ContextWindowTokens int
	PromptCacheTTL      string
	AllowFrontierFusion bool

	// Manual marks a user-initiated compaction (slash command, HTTP endpoint).
	// Such a request bypasses the per-session failure cooldown so a user who
	// just fixed their credentials/model isn't told "done" while nothing runs.
	// Automatic per-request paths leave this false to keep the cooldown backstop.
	Manual bool

	// HardPressure marks an automatic trigger fired at or above the blocking
	// share of the context window. Such retries re-attempt on an exponential
	// backoff instead of waiting out the full failure cooldown, because every
	// turn until a summary lands degrades or fails.
	HardPressure bool
}
