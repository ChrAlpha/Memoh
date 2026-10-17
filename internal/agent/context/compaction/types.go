package compaction

import (
	"net/http"
	"time"

	"github.com/felinics/memoh/internal/agent/turn"
)

// Compaction result statuses reported by RunCompactionSync.
const (
	StatusProgress = "progress" // compacted history still exceeds the requested replay budget
	StatusOK       = "ok"       // messages were compacted into a summary
	StatusNoop     = "noop"     // nothing to compact (already compact or in flight)
)

// Result is the scoped outcome of a synchronous compaction. Callers use it to
// respond with this session's own result instead of reading unscoped bot-wide
// logs. A failed attempt returns an error, not a Result.
type Result struct {
	Status       string
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
	// TotalInputTokens is the history pressure that called for this
	// compaction, reported to hooks.
	TotalInputTokens int
	// SummaryWindowTokens and MaxOutputTokens are the summarizer model's
	// declared window and output cap; they size the summarizer call.
	SummaryWindowTokens int
	MaxOutputTokens     int
	// CandidatesFrom is the creation time of the oldest row the caller's
	// request loaded and CandidatesBefore the cutoff an edit or retry
	// replays history before. Rows outside are invisible to that request,
	// so summarizing them cannot shrink it. Zero leaves that side open.
	CandidatesFrom   time.Time
	CandidatesBefore time.Time
	// ProtectedSources stay raw. Current ones are the caller's current input,
	// budgeted outside HistoryBudgetTokens; the rest are history it keeps.
	ProtectedSources []turn.ContextMessageSource
	// HistoryBudgetTokens is the room the caller's request has for the
	// summary and the history it keeps raw.
	HistoryBudgetTokens int
	PromptCacheTTL      string
}
