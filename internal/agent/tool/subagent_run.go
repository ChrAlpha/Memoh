package tools

import (
	"context"
	"errors"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/background"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/models"
)

type SpawnAgent interface {
	Generate(ctx context.Context, cfg SpawnRunConfig) (*SpawnResult, error)
	GenerateWithWatchdog(ctx context.Context, cfg SpawnRunConfig, touchFn func()) (*SpawnResult, error)
}

// SpawnAttemptDisposition names the authoritative outcome of one native
// attempt. Failed attempts can end the admitted run, retry inside it, or
// reflect owning cancellation; a clean end is recorded as completed.
type SpawnAttemptDisposition uint8

const (
	SpawnAttemptFailure SpawnAttemptDisposition = iota
	SpawnAttemptRetry
	SpawnAttemptAbort
	SpawnAttemptCompleted
)

// SpawnRunConfig mirrors agent.RunConfig fields needed by subagent controls.
type SpawnRunConfig struct {
	RunID           string
	Model           *sdk.Model
	ModelUUID       string
	ModelID         string
	ModelProvider   string
	ModelProviderID string
	System          string
	Query           string
	SessionType     string
	Identity        SpawnIdentity
	LoopDetection   SpawnLoopConfig
	Messages        []sdk.Message
	// ReasoningConfig is the thinking decision resolved for the subagent's own
	// model. It replaces a lone effort string that was never assigned, which is
	// how subagents came to run with no reasoning configuration at all (#983).
	ReasoningConfig *models.ReasoningConfig
	// Keep the unresolved parent-turn inputs as well, so a nested subagent can
	// resolve the same override against its own selected model.
	ReasoningStoredEffort    string
	ReasoningRequestedEffort string
	PromptCacheTTL           string
	ChatCompletionsCompat    string
	SupportsImageInput       bool
	SupportsFileInput        bool
	SupportsToolCall         bool
	Skills                   map[string]SkillDetail
	BackgroundManager        *background.Manager
	ContextBudgetMaxTokens   int
	ModelMaxOutputTokens     int
	// TurnRequestMessageID is the persisted task user message this run's
	// assistant and tool rows bind to, so incremental step persistence files
	// them into the same history turn the runtime view names.
	TurnRequestMessageID string
	// OnStepPersisted, if set, is called after a complete or interrupted step of
	// this run has been durably persisted. The spawn provider uses it to stop
	// retrying an attempt that has already produced durable output (and possibly
	// real side effects): replaying the turn from the top would do that work twice.
	OnStepPersisted func()
	// BeforeTerminal is the fallback durability barrier when incremental step
	// persistence is unavailable. The native adapter invokes it after a clean
	// result is assembled but before publishing EventAgentEnd to the session
	// runtime. Returning an error withholds that terminal proposal.
	BeforeTerminal func(context.Context, *SpawnResult) error
	// Attempt is one-based and MaxAttempts is the total outer-attempt budget.
	// ResolveAttempt is called at most once after a failed native attempt. It is
	// the spawn provider's authoritative disposition: retry keeps the admitted
	// run live, abort records owning cancellation, and failure ends the run.
	// ResolveCompletion arbitrates a clean native end against concurrent owning
	// cancellation before that end is published; it returns Completed, Abort for
	// an explicit stop, or Failure for another owning-context failure.
	// ReconcileTerminal applies the outcome selected by the session runtime when
	// terminal publication races a routed control such as AbortControl.
	Attempt           int
	MaxAttempts       int
	ResolveAttempt    func(error) SpawnAttemptDisposition
	ResolveCompletion func() SpawnAttemptDisposition
	ReconcileTerminal func(SpawnAttemptDisposition)
}

// SpawnIdentity mirrors agent.SessionContext fields needed by subagent controls.
type SpawnIdentity struct {
	BotID               string
	ChatID              string
	SessionID           string
	UserID              string
	ChannelIdentityID   string
	CurrentPlatform     string
	ReplyTarget         string
	ConversationType    string
	SessionToken        string //nolint:gosec // #nosec G117 -- session identifier, not a secret
	WorkspaceTargetID   string
	WorkspaceTargetKind string
	WorkspaceTargetName string
	WorkdirPath         string
	TimezoneLocation    *time.Location
	IsSubagent          bool
}

// SpawnLoopConfig mirrors agent.LoopDetectionConfig.
type SpawnLoopConfig struct {
	Enabled bool
}

// SpawnResult mirrors agent.GenerateResult.
type SpawnResult struct {
	Messages         []sdk.Message
	Text             string
	Usage            *sdk.Usage
	ContextLifecycle *contextfrag.LifecycleSnapshot
	// Persisted reports that incremental step persistence owned this run's
	// history, so the caller must not persist the result again.
	Persisted bool
}

var ErrWatchdogTimedOut = errors.New("subagent watchdog: no activity within timeout")
