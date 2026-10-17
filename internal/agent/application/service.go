// Package application orchestrates agent turns, including context assembly,
// persistence, memory, compaction, approvals, and runtime invocation.
package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"golang.org/x/sync/singleflight"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/background"
	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/agent/toolexec"
	turnpkg "github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	messageevent "github.com/felinics/memoh/internal/chat/event"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/hooks"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/oauthctx"
	"github.com/felinics/memoh/internal/providers"
	"github.com/felinics/memoh/internal/reasoning"
	"github.com/felinics/memoh/internal/sessionfacts"
	"github.com/felinics/memoh/internal/settings"
	"github.com/felinics/memoh/internal/workspace"
)

// SkillEntry represents a skill loaded from the container.
type SkillEntry struct {
	Name        string
	Description string
	Content     string
	Path        string
	Metadata    map[string]any
}

// SkillLoader loads skills for a given bot from its container.
type SkillLoader interface {
	LoadSkills(ctx context.Context, botID string) ([]SkillEntry, error)
}

// gatewayAssetLoader resolves content_hash references to binary payloads for gateway dispatch.
type gatewayAssetLoader interface {
	OpenForGateway(ctx context.Context, botID, contentHash string) (reader io.ReadCloser, mime string, err error)
	AccessPathForGateway(ctx context.Context, botID, contentHash string) (string, error)
}

// PlatformIdentity is the Agent-owned projection of a connected platform
// account used while assembling the system prompt.
type PlatformIdentity struct {
	ID               string
	Platform         string
	ExternalIdentity string
	SelfIdentity     map[string]any
}

// PlatformIdentitySource supplies connected platform identities without
// exposing Channel configuration types to the application layer.
type PlatformIdentitySource interface {
	ListPlatformIdentities(ctx context.Context, botID string) ([]PlatformIdentity, error)
}

type workspaceTargetResolver interface {
	ResolveWorkspaceTarget(ctx context.Context, botID, targetID string) (workspace.ResolvedWorkspaceTarget, error)
}

type compactionRunner interface {
	RunCompactionSync(context.Context, compaction.TriggerConfig) (compaction.Result, error)
}

// Service orchestrates chat with the internal agent.
type Service struct {
	activeTurns             runreq.ActiveTurnTracker
	resumeSecret            string
	teamScopes              runreq.TeamScopes
	resumeReady             func(context.Context, string, string) error
	resumeStop              context.CancelFunc
	resumeDone              chan struct{}
	agent                   *native.Agent
	modelsService           *models.Service
	queries                 dbstore.Queries
	memoryRegistry          *memprovider.Registry
	messageService          messagepkg.Service
	settingsService         *settings.Service
	accountService          *accounts.Service
	sessionService          runreq.SessionService
	externalDrivers         map[string]external.Driver
	compactionService       compactionRunner
	eventPublisher          messageevent.Publisher
	skillLoader             SkillLoader
	assetLoader             gatewayAssetLoader
	animationFrames         animationCache
	platformIdentities      PlatformIdentitySource
	botPermissions          runreq.BotPermissionChecker
	workspaceTargets        workspaceTargetResolver
	workdirs                sessionWorkdirResolver
	streamHTTPClient        *http.Client
	compactionHTTPClient    *http.Client
	streamIdleTimeout       time.Duration
	streamIdleTimeoutMax    time.Duration
	bgManager               *background.Manager
	toolApproval            *toolapproval.Service
	userInput               userInputService
	hookService             *hooks.Service
	memoryContextMu         sync.Mutex
	memoryContextCache      *memprovider.MemoryContextCache
	starterPromptFlights    singleflight.Group
	externalAgentPromptHubs sync.Map
	// continueUserInputFn overrides the application resume after a user input
	// response; nil means storeUserInputResultAndContinue. Test seam.
	continueUserInputFn               func(ctx context.Context, req userinput.Request, input UserInputResponseInput, result sdk.ToolResultPart) (runPlan, error)
	sessionCompactionMu               sync.Mutex
	sessionCompactions                map[string]*sessionCompactionGate
	memorySearchTimeout               time.Duration
	contextAbsoluteCapTokens          int
	clockLocation                     *time.Location
	logger                            *slog.Logger
	allowedTeam                       string
	sessionRuntime                    runreq.TurnAdmitter
	decisionRuntime                   *sessionruntime.Manager
	abortRuntime                      runtimeAbortController
	contextLifecycles                 contextLifecycleStore
	contextLifecyclePersistenceErrors atomic.Uint64
	contextLifecycleCandidatesMu      sync.Mutex
	contextLifecycleCandidates        map[contextLifecycleCandidateKey]contextLifecycleCandidate
	publishTurnEvent                  func(context.Context, sessionruntime.RunHandle, native.StreamEvent) error
	sessionManager                    *sessionruntime.Manager
	// outputMedia and outputSpeech prepare a run's events on their way to the
	// subscribers: attachments are stored, speech is synthesized into voice
	// attachments. Both are optional.
	outputMedia  outputMediaStore
	outputSpeech outputSpeech
	// queue is the durable queue of input that met a busy session. It is nil
	// where the deployment has no database, and then nothing queues.
	queue queuedInputStore
	// pendingSessions serializes the admission of what waits on one session.
	pendingSessions sessionLocks
}

// Deps is everything New wires into a Service. The required block is checked
// by New; the optional block may stay zero, which turns the feature it names
// off; the configuration block carries plain values.
type Deps struct {
	// Required.

	// Logger receives the service's own records.
	Logger *slog.Logger
	// Queries is the database port used by every persistence path.
	Queries dbstore.Queries
	// Messages persists and reads chat messages.
	Messages messagepkg.Service
	// Sessions reads and updates session metadata and titles.
	Sessions runreq.SessionService
	// Settings resolves per-bot settings.
	Settings *settings.Service
	// Accounts resolves the account behind a request.
	Accounts *accounts.Service
	// Models resolves model and provider configuration.
	Models *models.Service
	// Agent is the in-process model runtime.
	Agent *native.Agent
	// SessionRuntime is the durable admission gate. Every turn entry point goes
	// through it, so a service without it starts nothing. New also registers the
	// service's runtime callbacks on it.
	SessionRuntime *sessionruntime.Manager
	// BotPermissions decides whether an account may act on a bot.
	BotPermissions runreq.BotPermissionChecker
	// WorkspaceTargets resolves a request-scoped Computer.
	WorkspaceTargets workspaceTargetResolver
	// ToolApproval stores and resolves tool approval decisions.
	ToolApproval *toolapproval.Service
	// UserInput stores and resolves user input decisions.
	UserInput *userinput.Service
	// ResumeSecret signs the credentials of runs the server starts itself and of
	// resumed sessions.
	ResumeSecret string

	// Optional.

	// Memory provides memory search and write-back; without it runs have no memory.
	Memory *memprovider.Registry
	// Hooks runs user-defined hooks; without it no hook fires.
	Hooks *hooks.Service
	// Skills lists a bot's usable skills; without it requests carry none.
	Skills SkillLoader
	// Assets inlines attachment content for the gateway; without it attachments
	// keep their stored references.
	Assets gatewayAssetLoader
	// PlatformIdentities supplies connected platform accounts for the system
	// prompt; without it the prompt names none.
	PlatformIdentities PlatformIdentitySource
	// Workdirs resolves session workdir bindings; without it bindings are ignored.
	Workdirs sessionWorkdirResolver
	// Compaction runs context compaction; without it nothing is compacted.
	Compaction *compaction.Service
	// Background supplies background task summaries; without it there are none.
	Background *background.Manager
	// Events broadcasts session events such as title updates; without it none
	// are sent.
	Events messageevent.Publisher
	// OutputMedia stores attachments that runs emit; without it an attachment
	// keeps the path it arrived with.
	OutputMedia outputMediaStore
	// OutputSpeech synthesizes spoken replies; without it a speech event is dropped.
	OutputSpeech outputSpeech
	// QueuedInputs is the durable queue for input that meets a busy session;
	// without it such input is refused as busy.
	QueuedInputs *sessionruntime.QueuedInputs
	// ExternalRuntimes are the out-of-process runtime drivers (ACP, codex,
	// claude-code). A session whose runtime type has no driver fails with a
	// stable "runtime unavailable" error.
	ExternalRuntimes []external.Driver
	// ResumeReadiness checks workspace transport before a continuation is
	// claimed; without it resume skips the check.
	ResumeReadiness func(ctx context.Context, botID, targetID string) error
	// TeamScopes is the tenancy provider of a hosted deployment; without it
	// resume and the discuss sweep cover only AllowedTeam.
	TeamScopes runreq.TeamScopes

	// Configuration.

	// ClockLocation is the time zone prompts render in; nil means UTC.
	ClockLocation *time.Location
	// ContextFallbackTokens is the context budget for models without a
	// configured window; zero keeps the shared default.
	ContextFallbackTokens int
	// AllowedTeam restricts the service to one team: the in-process runtime's
	// database pool is bound to a single team, so commands for any other team
	// fail closed (turn.ErrTeamNotServed). Empty means no restriction.
	AllowedTeam string
}

// validate names the first required dependency that is missing.
func (d Deps) validate() error {
	required := []struct {
		name    string
		missing bool
	}{
		{"Logger", d.Logger == nil},
		{"Queries", d.Queries == nil},
		{"Messages", d.Messages == nil},
		{"Sessions", d.Sessions == nil},
		{"Settings", d.Settings == nil},
		{"Accounts", d.Accounts == nil},
		{"Models", d.Models == nil},
		{"Agent", d.Agent == nil},
		{"SessionRuntime", d.SessionRuntime == nil},
		{"BotPermissions", d.BotPermissions == nil},
		{"WorkspaceTargets", d.WorkspaceTargets == nil},
		{"ToolApproval", d.ToolApproval == nil},
		{"UserInput", d.UserInput == nil},
		{"ResumeSecret", d.ResumeSecret == ""},
	}
	for _, r := range required {
		if r.missing {
			return fmt.Errorf("agent application: Deps.%s is required", r.name)
		}
	}
	return nil
}

// New creates an application service backed by the native agent. A missing
// required dependency is an error, so a half-wired service cannot exist.
func New(d Deps) (*Service, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	clockLocation := d.ClockLocation
	if clockLocation == nil {
		clockLocation = time.UTC
	}
	// Streaming requests keep transport establishment bounded, while the
	// application idle watchdog owns first-byte and between-event silence. A
	// client-wide or response-header deadline would otherwise preempt that
	// policy and turn one intentional timeout into repeated transport retries.
	streamTransport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	streamHTTPClient := &http.Client{
		Transport: streamTransport,
	}
	// The summarizer sends one large cold prompt whose first byte routinely
	// takes 60-93s upstream; the interactive 30s header deadline would kill
	// every automatic pass while manual /compact (SDK default client) works.
	compactionTransport := streamTransport.Clone()
	compactionTransport.ResponseHeaderTimeout = 3 * time.Minute
	compactionHTTPClient := &http.Client{
		Transport: compactionTransport,
		Timeout:   10 * time.Minute,
	}

	teamScopes := d.TeamScopes
	if teamScopes == nil {
		teamScopes = runreq.SingleTeam(d.AllowedTeam)
	}

	service := &Service{
		agent:                    d.Agent,
		modelsService:            d.Models,
		queries:                  d.Queries,
		contextLifecycles:        d.Queries,
		messageService:           d.Messages,
		settingsService:          d.Settings,
		accountService:           d.Accounts,
		sessionService:           d.Sessions,
		botPermissions:           d.BotPermissions,
		workspaceTargets:         d.WorkspaceTargets,
		toolApproval:             d.ToolApproval,
		userInput:                d.UserInput,
		resumeSecret:             d.ResumeSecret,
		memoryRegistry:           d.Memory,
		hookService:              d.Hooks,
		skillLoader:              d.Skills,
		assetLoader:              d.Assets,
		platformIdentities:       d.PlatformIdentities,
		workdirs:                 d.Workdirs,
		bgManager:                d.Background,
		eventPublisher:           d.Events,
		outputMedia:              d.OutputMedia,
		outputSpeech:             d.OutputSpeech,
		externalDrivers:          externalDriverMap(d.ExternalRuntimes...),
		resumeReady:              d.ResumeReadiness,
		teamScopes:               teamScopes,
		contextAbsoluteCapTokens: d.ContextFallbackTokens,
		allowedTeam:              d.AllowedTeam,
		streamHTTPClient:         streamHTTPClient,
		compactionHTTPClient:     compactionHTTPClient,
		memorySearchTimeout:      defaultMemorySearchTimeout,
		clockLocation:            clockLocation,
		logger:                   d.Logger.With(slog.String("service", "agent/application")),
	}
	// A typed nil stored in an interface would read as configured.
	if d.Compaction != nil {
		service.compactionService = d.Compaction
	}
	if d.QueuedInputs != nil {
		service.queue = d.QueuedInputs
	}
	service.bindSessionRuntime(d.SessionRuntime)
	return service, nil
}

// contextAbsoluteMaxTokens resolves the budget for models without a window.
func (s *Service) contextAbsoluteMaxTokens() int {
	if s.contextAbsoluteCapTokens > 0 {
		return s.contextAbsoluteCapTokens
	}
	return contextfrag.DefaultContextFallbackTokens
}

// effectiveContextTokenBudget derives the per-turn context budget: the model
// context window, or the configured fallback when the model has none, so a
// missing window never leaves the context unbounded.
func (s *Service) effectiveContextTokenBudget(chatModel models.GetResponse) int {
	budget := contextBudgetFromChatModel(chatModel)
	if budget > 0 {
		return budget
	}
	fallback := s.contextAbsoluteMaxTokens()
	s.logger.Warn("context budget: model context window missing, applying fallback",
		slog.String("model_id", chatModel.ID),
		slog.Int("fallback_tokens", fallback))
	return fallback
}

func contextBudgetFromChatModel(chatModel models.GetResponse) int {
	return chatModel.Config.ContextBudgetMaxTokens()
}

func markRequiredHistoryMessageCurrent(cfg *native.RunConfig, requiredMessageID string) {
	if cfg == nil {
		return
	}
	cfg.ContextCurrentUserMessageIndex = nil
	requiredMessageID = strings.TrimSpace(requiredMessageID)
	if requiredMessageID == "" {
		return
	}
	for index, sourceMessageID := range cfg.ForkContextSourceMessageIDs {
		if strings.TrimSpace(sourceMessageID) != requiredMessageID {
			continue
		}
		if index >= len(cfg.Messages) || cfg.Messages[index].Role != sdk.MessageRoleUser {
			return
		}
		cfg.ContextCurrentUserMessageIndex = runreq.IntPointer(index)
		return
	}
}

// resolve builds the run context for one turn and returns the effective
// request alongside it. Resolution fills in defaults the caller's copy does not
// have — a direct turn on a subagent thread learns its session type, pinned
// model, and skip flags here — so everything that runs after resolution (title
// generation, the step committer, terminal persistence) must use the returned
// request, not the one that went in. Query is deliberately untouched: callers
// that persist a headerified user message still take it from runreq.ResolvedContext.
func (s *Service) resolve(ctx context.Context, req runreq.ChatRequest) (runreq.ResolvedContext, runreq.ChatRequest, error) {
	modelQuery := runreq.ModelQueryText(req)
	// A request whose input is already persisted carries no query of its own:
	// its current message is the history row it names.
	if strings.TrimSpace(modelQuery) == "" && len(req.Attachments) == 0 && !req.ReusePersistedUserMessage {
		return runreq.ResolvedContext{}, req, errors.New("query or attachments is required")
	}
	if strings.TrimSpace(req.BotID) == "" {
		return runreq.ResolvedContext{}, req, errors.New("bot id is required")
	}
	if strings.TrimSpace(req.ChatID) == "" {
		return runreq.ResolvedContext{}, req, errors.New("chat id is required")
	}
	if err := s.rejectRequestedSkillsIfUnsupportedContext(ctx, req); err != nil {
		return runreq.ResolvedContext{}, req, err
	}
	// Session model preference (issue #879 spec v2): loaded once per turn,
	// BEFORE the subagent pin below — the pin is only a session's initial
	// default and must not refill the request over a remembered pair.
	// requestCarriesPair is captured before that mutation so the write-back
	// gate sees the request as it actually arrived: the web composer omits
	// model/effort when the pair is default-sourced, and channel requests
	// structurally never carry them (only the local REST handler ever fills
	// the metadata keys), so "carries" == "explicitly chosen or remembered".
	// Schedule-triggered turns skip the memory level entirely: their model
	// comes from the schedule payload or the bot default, and a pair
	// remembered from a web continuation must not hijack them.
	requestCarriesPair := strings.TrimSpace(req.Model) != "" || strings.TrimSpace(req.ReasoningEffort) != ""
	sessionPrefModelID, sessionPrefEffort := "", ""
	pairMemoryApplies := !strings.EqualFold(strings.TrimSpace(req.SessionType), sessionmode.Schedule)
	if pairMemoryApplies {
		sessionPrefModelID, sessionPrefEffort = s.sessionModelPreference(ctx, req.ThreadID)
	}
	req = s.applySubagentThreadDefaults(ctx, req, sessionPrefModelID != "")
	facts := s.requestRunFacts(ctx, req)

	runCfg, chatModel, provider, err := s.buildBaseRunConfig(ctx, baseRunConfigParams{
		BotID:              req.BotID,
		ChatID:             req.ChatID,
		SessionID:          req.ThreadID,
		RouteID:            req.RouteID,
		UserID:             req.UserID,
		ChannelIdentityID:  req.SourceChannelIdentityID,
		CurrentPlatform:    req.CurrentChannel,
		ReplyTarget:        req.ReplyTarget,
		ConversationType:   req.ConversationType,
		SessionToken:       req.ChatToken,
		SessionType:        req.SessionType,
		Facts:              facts,
		SessionPrefModelID: sessionPrefModelID,
		SessionPrefEffort:  sessionPrefEffort,
		Model:              req.Model,
		Provider:           req.Provider,
		ReasoningEffort:    req.ReasoningEffort,
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "resolve: buildBaseRunConfig failed",
			slog.String("bot_id", req.BotID),
			slog.Any("error", err),
		)
		return runreq.ResolvedContext{}, req, err
	}

	// Persist explicit selections before generation and advance the revision
	// even for matching values. Schedule payloads describe the scheduled turn,
	// not a session preference, and must never enter this write path.
	s.writeBackSessionModelPreference(ctx, req.ThreadID, requestCarriesPair && pairMemoryApplies, chatModel, runCfg.ReasoningConfig)
	if req.OnModelPreferenceSettled != nil {
		req.OnModelPreferenceSettled()
	}
	if strings.EqualFold(strings.TrimSpace(req.SessionType), sessionpkg.TypeSubagent) {
		// A direct turn on a subagent thread runs as the subagent, not as a
		// chat turn that happens to share its history: same restricted tool
		// surface (no nested spawns), same prompt mode.
		runCfg.Identity.IsSubagent = true
	}
	runCfg.RunID = runreq.RunID(req.RunID)
	memoryContext := s.loadMemoryContext(ctx, req)
	if memoryContext.Trace != nil && runCfg.ContextLifecycle != nil {
		runCfg.ContextLifecycle.SetMemoryRecall(*memoryContext.Trace)
	}
	memoryMsg := memoryContext.Message
	reqMessages := runreq.NonNilModelMessages(req.Messages)

	contextTokenBudget := s.effectiveContextTokenBudget(chatModel)
	runCfg.ContextBudgetMaxTokens = contextTokenBudget

	assembled, err := s.assembleContext(ctx, chatContextRequest(contextPurposeRun, req, contextTokenBudget))
	if err != nil {
		s.logger.ErrorContext(ctx, "resolve: assemble context failed",
			slog.String("bot_id", req.BotID),
			slog.String("stage", "initial"),
			slog.Any("error", err),
		)
		return runreq.ResolvedContext{}, req, err
	}
	forkContext := s.subagentForkContextModelMessages(ctx, req)
	notice := s.currentWorkspaceContextMessage(ctx, req)
	requestedSkillMsg := buildRequestedSkillContextMessage(req.RequestedSkills)
	var ownMessages []runreq.ModelMessage
	ownMessages = append(ownMessages, forkContext...)
	for _, message := range []*runreq.ModelMessage{notice, memoryMsg, requestedSkillMsg} {
		if message != nil {
			ownMessages = append(ownMessages, *message)
		}
	}
	if !req.ReusePersistedUserMessage {
		ownMessages = append(ownMessages, reqMessages...)
	}
	estimate := &turnRequestEstimate{
		service:   s,
		ctx:       ctx,
		sessionID: req.ThreadID,
		own:       append(runreq.ModelMessagesToSDKMessages(ownMessages), currentUserMessageEstimate(req, modelQuery)...),
	}
	assembled, err = s.compactBeforeTurn(ctx, req, chatModel.ID, contextTokenBudget, assembled, estimate)
	if err != nil {
		s.logger.ErrorContext(ctx, "resolve: assemble context failed",
			slog.String("bot_id", req.BotID),
			slog.String("stage", "post_compaction"),
			slog.Any("error", err),
		)
		return runreq.ResolvedContext{}, req, err
	}
	pressure := runreq.NewRunPressure(runCfg.ContextLifecycle, estimate.conversation(assembled.Messages))
	messages := assembled.Messages
	historyRecords := assembled.Records
	estimatedTokens := assembled.EstimatedTokens
	if len(forkContext) > 0 {
		// The inherited parent snapshot precedes the thread's own transcript,
		// exactly as parent-driven subagent tasks assemble it.
		messages = append(forkContext, messages...)
	}
	messages = normalizeContextMessages(messages)
	trimmableMessages := len(messages)
	if notice != nil {
		messages = append(messages, *notice)
	}
	var memoryMessageIndex *int
	if memoryMsg != nil {
		messages = append(messages, *memoryMsg)
		memoryMessageIndex = runreq.IntPointer(len(messages) - 1)
	}
	if requestedSkillMsg != nil {
		messages = append(messages, *requestedSkillMsg)
	}
	if !req.ReusePersistedUserMessage {
		messages = append(messages, reqMessages...)
	}
	messages = normalizeContextMessages(messages)
	runCfg.ContextHistoryTokenEstimates = make([]int, len(messages))
	for index := range messages {
		runCfg.ContextHistoryTokenEstimates[index] = estimateMessageTokens(messages[index])
	}
	runCfg.ContextTrimmableMessages = trimmableMessages

	displayName := s.resolveDisplayName(ctx, req)
	mergedAttachments := s.routeAndMergeAttachments(ctx, chatModel, req)

	tz := runCfg.Identity.TimezoneLocation
	if tz == nil {
		tz = time.UTC
	}
	headerInput := turnpkg.UserMessageHeaderInput{
		MessageID:         strings.TrimSpace(req.ExternalMessageID),
		ChannelIdentityID: strings.TrimSpace(req.SourceChannelIdentityID),
		DisplayName:       displayName,
		Channel:           req.CurrentChannel,
		ConversationType:  strings.TrimSpace(req.ConversationType),
		ConversationName:  strings.TrimSpace(req.ConversationName),
		Target:            strings.TrimSpace(req.ReplyTarget),
		AttachmentPaths:   extractAttachmentPaths(mergedAttachments),
		Time:              time.Now().In(tz),
		Timezone:          runCfg.Identity.Timezone,
		ReplyToMessageID:  strings.TrimSpace(req.SourceReplyToMessageID),
		ReplyToSender:     strings.TrimSpace(req.ReplySender),
		ReplyToPreview:    req.ReplyPreview,
		MentionsMe:        req.MentionsBot,
		RepliesToMe:       req.RepliesToBot,
	}
	headerifiedQuery := ""
	if userQueryNeedsHeader(req, len(mergedAttachments)) {
		headerifiedQuery = turnpkg.FormatUserHeader(headerInput, req.Query)
	}
	headerifiedModelQuery := headerifiedQuery
	if strings.TrimSpace(modelQuery) != strings.TrimSpace(req.Query) {
		headerifiedModelQuery = turnpkg.FormatUserHeader(headerInput, modelQuery)
	}
	runCfg.ContextFrags = historyContextFragsForMessages(messages, historyRecords)
	forkMessages := runreq.NonNilModelMessages(messages)
	runCfg.ForkContextSourceMessageIDs = historySourceMessageIDsForMessages(forkMessages, historyRecords)
	runCfg.Messages = runreq.ModelMessagesToSDKMessages(forkMessages)
	runCfg.ContextMemoryMessageIndex = memoryMessageIndex
	if req.ReusePersistedUserMessage {
		markRequiredHistoryMessageCurrent(&runCfg, req.RequiredHistoryMessageID)
	} else {
		runCfg.Query = headerifiedModelQuery
	}
	runCfg.InlineImages = extractNativeImageParts(mergedAttachments)
	runCfg.InlineAttachments = extractNativeAttachmentParts(mergedAttachments)
	runCfg.ContextScope = buildContextFragScope(req, displayName, runCfg.Identity)
	runCfg = runCfg.RefreshContextFrag()

	var injectedRecords *[]runreq.InjectedMessageRecord
	if req.InjectCh != nil {
		agentInjectCh := make(chan native.InjectMessage, cap(req.InjectCh))
		go func() {
			for msg := range req.InjectCh {
				agentMsg := native.InjectMessage{
					Text:            msg.Text,
					HeaderifiedText: msg.HeaderifiedText,
				}
				// Inline any image attachments from the injected message so the
				// model receives them as vision input alongside the text.
				if runCfg.SupportsImageInput && len(msg.Attachments) > 0 {
					agentMsg.ImageParts = s.inlineInjectAttachments(ctx, req.BotID, msg.Attachments)
				}
				agentInjectCh <- agentMsg
			}
			close(agentInjectCh)
		}()
		runCfg.InjectCh = agentInjectCh

		records := make([]runreq.InjectedMessageRecord, 0)
		injectedRecords = &records
		var recMu sync.Mutex
		runCfg.InjectedRecorder = func(headerifiedText string, insertAfter int) {
			recMu.Lock()
			*injectedRecords = append(*injectedRecords, runreq.InjectedMessageRecord{
				HeaderifiedText: headerifiedText,
				InsertAfter:     insertAfter,
			})
			recMu.Unlock()
		}
	}

	return runreq.ResolvedContext{
		RunConfig:          runCfg,
		Model:              chatModel,
		Provider:           provider,
		Query:              headerifiedQuery,
		InjectedRecords:    injectedRecords,
		EstimatedTokens:    estimatedTokens,
		Pressure:           pressure,
		ContextTokenBudget: contextTokenBudget,
	}, req, nil
}

// baseRunConfigParams holds parameters for buildBaseRunConfig that differ
// between chat and discuss callers.
type baseRunConfigParams struct {
	BotID             string
	ChatID            string
	SessionID         string
	RouteID           string
	UserID            string
	ChannelIdentityID string
	CurrentPlatform   string
	ReplyTarget       string
	ConversationType  string
	SessionToken      string //nolint:gosec // session credential material, not a hardcoded secret
	SessionType       string
	// Facts are the session facts the run executes under (see runFacts).
	Facts sessionfacts.Facts
	// SessionPrefModelID/SessionPrefEffort carry the session's persisted
	// (model, effort) pair (issue #879), loaded by the caller before the
	// subagent pin may fill the request. Empty = no memory.
	SessionPrefModelID string
	SessionPrefEffort  string
	Model              string
	Provider           string
	ReasoningEffort    string // caller-provided override (empty = use bot default)
}

// buildBaseRunConfig creates a RunConfig with model, credentials, skills,
// identity and system prompt — everything except Messages/Query/InlineImages.
// Both resolve() and ResolveRunConfig() delegate to this shared builder.
func (s *Service) buildBaseRunConfig(ctx context.Context, p baseRunConfigParams) (native.RunConfig, models.GetResponse, sqlc.Provider, error) {
	botSettings, err := s.loadBotSettings(ctx, p.BotID)
	if err != nil {
		return native.RunConfig{}, models.GetResponse{}, sqlc.Provider{}, err
	}
	botInfo, loopDetectionEnabled := s.loadBotRuntimeInfo(ctx, p.BotID)
	userTimezoneName, userClockLocation := s.resolveTimezone(ctx, p.BotID, p.UserID)

	chatID := p.ChatID
	if chatID == "" {
		chatID = p.BotID
	}

	req := buildModelSelectionRequest(p, chatID)

	chatModel, provider, err := s.selectChatModel(ctx, req, botSettings, p.SessionPrefModelID)
	if err != nil {
		if errors.Is(err, errChatModelNotConfigured) {
			err = apperror.Wrap(apperror.CodeAgentChatModelNotConfigured, err, nil)
		}
		return native.RunConfig{}, models.GetResponse{}, sqlc.Provider{}, err
	}

	authService := providers.NewService(nil, s.queries, "")
	authCtx := oauthctx.WithUserID(ctx, p.UserID)
	creds, err := authService.ResolveModelCredentials(authCtx, provider)
	if err != nil {
		return native.RunConfig{}, models.GetResponse{}, sqlc.Provider{}, fmt.Errorf("resolve provider credentials: %w", err)
	}

	baseURL := providers.ProviderConfigString(provider, "base_url")
	chatCompletionsCompat := models.ResolveChatCompletionsCompat(
		baseURL,
		providers.ProviderConfigString(provider, models.ChatCompletionsCompatConfigKey),
	)

	reasoningConfig := resolveRunReasoningConfig(chatModel, botSettings, p, provider.ClientType)

	sdkModel := models.NewSDKChatModel(models.SDKModelConfig{
		ModelID:               chatModel.ModelID,
		ClientType:            provider.ClientType,
		APIKey:                creds.APIKey,
		CodexAccountID:        creds.CodexAccountID,
		BaseURL:               baseURL,
		ChatCompletionsCompat: chatCompletionsCompat,
		HTTPClient:            s.streamHTTPClient,
		ReasoningConfig:       reasoningConfig,
		ReasoningDialect:      chatModel.Config.ReasoningDialect,
		ReasoningOffSupport:   chatModel.Config.ReasoningOffSupport,
		ReasoningDefaultOn:    chatModel.Config.ReasoningDefaultOn,
		ThinkingBudgetMin:     chatModel.Config.ThinkingBudgetMin,
		ThinkingBudgetMax:     chatModel.Config.ThinkingBudgetMax,
		ContextWindow:         contextBudgetFromChatModel(chatModel),
		MaxOutputTokens:       chatModel.Config.MaxOutputTokensLimit(),
	})

	var agentSkills []native.SkillEntry
	if s.skillLoader != nil {
		entries, skillErr := s.skillLoader.LoadSkills(ctx, p.BotID)
		if skillErr != nil {
			s.logger.WarnContext(ctx, "failed to load skills", slog.String("bot_id", p.BotID), slog.Any("error", skillErr))
		} else {
			for _, e := range entries {
				if skill, ok := normalizeGatewaySkill(e); ok {
					agentSkills = append(agentSkills, skill)
				}
			}
		}
	}
	if agentSkills == nil {
		agentSkills = []native.SkillEntry{}
	}

	cfg := native.RunConfig{
		Model:                    sdkModel,
		CurrentModelUUID:         chatModel.ID,
		CurrentModelID:           chatModel.ModelID,
		CurrentModelProvider:     provider.Name,
		CurrentModelProviderID:   chatModel.ProviderID,
		ReasoningConfig:          reasoningConfig,
		ReasoningStoredEffort:    strings.TrimSpace(botSettings.ReasoningEffort),
		ReasoningRequestedEffort: strings.TrimSpace(p.ReasoningEffort),
		ChatCompletionsCompat:    chatCompletionsCompat,
		PromptCacheTTL:           providers.ProviderConfigString(provider, "prompt_cache_ttl"),
		SessionType:              p.SessionType,
		ReplyDelivery:            p.Facts.ReplyDelivery,
		DecisionMode:             p.Facts.DecisionMode,
		SupportsImageInput:       supportsImageInputForModel(chatModel),
		SupportsFileInput:        supportsFileInputForModel(chatModel),
		SupportsToolCall:         chatModel.HasCompatibility(models.CompatToolCall),
		ModelMaxOutputTokens:     chatModel.Config.MaxOutputTokensLimit(),
		Identity: native.SessionContext{
			BotID:             p.BotID,
			ChatID:            chatID,
			SessionID:         p.SessionID,
			UserID:            strings.TrimSpace(p.UserID),
			ChannelIdentityID: strings.TrimSpace(p.ChannelIdentityID),
			CurrentPlatform:   p.CurrentPlatform,
			ReplyTarget:       strings.TrimSpace(p.ReplyTarget),
			ConversationType:  strings.TrimSpace(p.ConversationType),
			Timezone:          userTimezoneName,
			TimezoneLocation:  userClockLocation,
			SessionToken:      p.SessionToken,
		},
		Bot:               botInfo,
		Skills:            agentSkills,
		LoopDetection:     native.LoopDetectionConfig{Enabled: loopDetectionEnabled},
		BackgroundManager: s.bgManager,
		ContextLifecycle:  contextfrag.NewLifecycleHolder(),
		ContextScope: contextfrag.Scope{
			BotID:             p.BotID,
			ChatID:            chatID,
			SessionID:         p.SessionID,
			ChannelIdentityID: strings.TrimSpace(p.ChannelIdentityID),
			Platform:          strings.TrimSpace(p.CurrentPlatform),
			ConversationType:  strings.TrimSpace(p.ConversationType),
			ReplyTarget:       strings.TrimSpace(p.ReplyTarget),
		},
	}
	if s.toolApproval != nil || s.userInput != nil {
		cfg.ToolApprovalHandler = s.buildToolApprovalHandler(p)
	}
	// Entries here carry no end user to authorize: the target was authorized
	// when the entry (or the session's workdir) chose it, so no principal.
	_, ws, wsErr := s.resolveWorkspace(ctx, workspaceInput{
		BotID:      p.BotID,
		SessionID:  p.SessionID,
		BestEffort: true,
	})
	if wsErr != nil {
		return native.RunConfig{}, models.GetResponse{}, sqlc.Provider{}, wsErr
	}
	cfg.Identity.WorkdirPath = ws.WorkdirPath
	if ws.Target != nil {
		cfg.Identity.WorkspaceTargetID = ws.Target.TargetID
		cfg.Identity.WorkspaceTargetKind = ws.Target.Kind
		cfg.Identity.WorkspaceTargetName = ws.Target.Name
	}

	return cfg, chatModel, provider, nil
}

func (s *Service) canDeliverUserInputStream() bool {
	return s.userInput != nil
}

func supportsImageInputForModel(m models.GetResponse) bool {
	return m.HasCompatibility(models.CompatVision)
}

func supportsFileInputForModel(m models.GetResponse) bool {
	return m.HasCompatibility(models.CompatFileInput)
}

// resolveRunReasoningConfig keeps remembered effort attached to its model UUID.
// An explicit switch starts from the new model default; a slug resolving to
// the same UUID is not a switch and may retain the remembered effort.
func resolveRunReasoningConfig(chatModel models.GetResponse, botSettings settings.Settings, p baseRunConfigParams, clientType string) *models.ReasoningConfig {
	sessionEffort := p.SessionPrefEffort
	if !strings.EqualFold(strings.TrimSpace(chatModel.ID), strings.TrimSpace(p.SessionPrefModelID)) {
		sessionEffort = ""
		previousModel := p.SessionPrefModelID
		if previousModel == "" {
			previousModel = botSettings.ChatModelID
		}
		if strings.TrimSpace(p.Model) != "" && !strings.EqualFold(strings.TrimSpace(chatModel.ID), strings.TrimSpace(previousModel)) {
			botSettings.ReasoningEffort = ""
		}
	}
	return resolveReasoningConfig(chatModel, botSettings, p.ReasoningEffort, sessionEffort, clientType)
}

// resolveReasoningConfig makes the single reasoning decision for a call. The
// decision itself lives in internal/reasoning, which also answers what a picker
// may offer — the two share internals so the options a user sees and the value a
// call sends cannot disagree.
//
// sessionEffort is the session's remembered tier (issue #879). It sits between
// the per-message request and the bot's stored value, implemented by shadowing
// `stored`: identical semantics to a dedicated level, and — critically — it is
// NOT passed as `requested`, because RunConfig.ReasoningRequestedEffort travels
// to spawned subagents as "the user's explicit pick this turn" and memory must
// not leak into that channel.
func resolveReasoningConfig(chatModel models.GetResponse, botSettings settings.Settings, requestedEffort, sessionEffort, clientType string) *models.ReasoningConfig {
	stored := botSettings.ReasoningEffort
	if e := strings.TrimSpace(sessionEffort); e != "" {
		stored = e
	}
	return reasoning.ResolveConfig(
		chatModel.ResolveThinkingMode(),
		chatModel.Config.ReasoningEfforts,
		chatModel.ReasoningOptions(clientType),
		stored,
		requestedEffort,
	)
}

func (s *Service) buildToolApprovalHandler(p baseRunConfigParams) func(context.Context, sdk.ToolCall) (toolexec.ToolApprovalResult, error) {
	return func(ctx context.Context, call sdk.ToolCall) (toolexec.ToolApprovalResult, error) {
		if strings.TrimSpace(call.ToolName) == userinput.ToolNameAskUser {
			if err := userinput.ValidateAskUserInput(toolexec.ArgumentsValue(call.Input)); err != nil {
				// Let the tool's Execute handler return an instructional tool result
				// to the model instead of creating a fake pending request.
				return toolexec.ToolApprovalResult{Decision: toolexec.ToolApprovalDecisionApproved}, nil
			}
			if s.userInput == nil {
				return s.limitToolApprovalResult(toolexec.ToolApprovalResult{
					Decision: toolexec.ToolApprovalDecisionRejected,
					Reason:   "user input service is not configured",
				}, call.ToolName), nil
			}
			if p.Facts.DecisionMode != sessionfacts.DecisionPark {
				return s.limitToolApprovalResult(toolexec.ToolApprovalResult{
					Decision: toolexec.ToolApprovalDecisionRejected,
					Reason:   "user input requested in a non-interactive session",
				}, call.ToolName), nil
			}
			// No ExpiresAt here: application requests have no in-process
			// waiter — the run pauses and resumes whenever the user answers,
			// even much later. Only waiter-backed (ACP/MCP) requests expire.
			req, err := s.userInput.CreatePending(ctx, userinput.CreatePendingInput{
				BotID:                        p.BotID,
				SessionID:                    p.SessionID,
				RouteID:                      p.RouteID,
				ChannelIdentityID:            p.ChannelIdentityID,
				RequestedByChannelIdentityID: p.ChannelIdentityID,
				ToolCallID:                   call.ToolCallID,
				ToolName:                     call.ToolName,
				Input:                        toolexec.ArgumentsValue(call.Input),
				SourcePlatform:               p.CurrentPlatform,
				ReplyTarget:                  p.ReplyTarget,
				ConversationType:             p.ConversationType,
				WorkspaceTargetID:            workspace.WorkspaceTargetFromContext(ctx),
			})
			if err != nil {
				return toolexec.ToolApprovalResult{}, err
			}
			if req.Status != userinput.StatusPending {
				return s.limitToolApprovalResult(toolexec.ToolApprovalResult{
					Decision:   toolexec.ToolApprovalDecisionRejected,
					ApprovalID: req.ID,
					Reason:     "ask_user request is already " + req.Status,
					Metadata:   userinput.DeferredMetadata(req),
				}, call.ToolName), nil
			}
			return toolexec.ToolApprovalResult{
				Decision:   toolexec.ToolApprovalDecisionDeferred,
				ApprovalID: req.ID,
				Metadata:   userinput.DeferredMetadata(req),
			}, nil
		}
		input := toolapproval.CreatePendingInput{
			BotID:                        p.BotID,
			SessionID:                    p.SessionID,
			RouteID:                      p.RouteID,
			ChannelIdentityID:            p.ChannelIdentityID,
			RequestedByChannelIdentityID: p.ChannelIdentityID,
			ToolCallID:                   call.ToolCallID,
			ToolName:                     call.ToolName,
			ToolInput:                    toolexec.ArgumentsValue(call.Input),
			SourcePlatform:               p.CurrentPlatform,
			ReplyTarget:                  p.ReplyTarget,
			ConversationType:             p.ConversationType,
			WorkspaceTargeted:            isWorkspaceTargetTool(call.ToolName),
			WorkspaceTargetID:            workspace.WorkspaceTargetFromContext(ctx),
		}
		forcedApprovalReason, forcedApproval := native.HookForcedApprovalReason(ctx)
		if s.toolApproval == nil {
			if forcedApproval {
				return s.limitToolApprovalResult(toolexec.ToolApprovalResult{
					Decision: toolexec.ToolApprovalDecisionRejected,
					Reason:   runreq.FirstNonEmpty(forcedApprovalReason, "hook requested approval but tool approval is not configured"),
				}, call.ToolName), nil
			}
			return toolexec.ToolApprovalResult{Decision: toolexec.ToolApprovalDecisionApproved}, nil
		}
		eval, err := s.toolApproval.EvaluatePolicy(ctx, input)
		if err != nil {
			if input.WorkspaceTargeted && errors.Is(err, workspace.ErrWorkspaceTargetNotFound) {
				requestedTargetID := ""
				if args, ok := toolexec.ArgumentsValue(call.Input).(map[string]any); ok {
					requestedTargetID = strings.TrimSpace(runreq.ReadAnyString(args["target_id"]))
				}
				if s.logger != nil {
					s.logger.Warn("workspace tool target not found during approval",
						slog.String("bot_id", p.BotID),
						slog.String("tool_name", call.ToolName),
						slog.String("tool_call_id", call.ToolCallID),
						slog.String("requested_target_id", requestedTargetID),
					)
				}
				// Twilight treats approval handler errors as fatal to the whole run.
				// A missing target is invalid model input, so let the tool execute
				// and return its normal, instructional error instead. This is safe
				// only for not-found targets: they cannot execute or bypass policy.
				return toolexec.ToolApprovalResult{Decision: toolexec.ToolApprovalDecisionApproved}, nil
			}
			return toolexec.ToolApprovalResult{}, err
		}
		input.ExecutionLocation = eval.ExecutionLocation
		locationMetadata := executionLocationResultMetadata(eval.ExecutionLocation)
		// The policy resolved the workspace target and pinned it on the
		// argument map it evaluated. The executor takes these arguments for
		// the call, so the tool runs where the policy looked and the persisted
		// call names that target (call.Input is a copy the handler cannot
		// mutate).
		var approvedInput *sdk.ToolArguments
		if input.WorkspaceTargeted {
			rewritten := toolexec.ArgumentsFromValue(input.ToolInput)
			approvedInput = &rewritten
		}
		if eval.Decision == toolapproval.DecisionDeny {
			return s.limitToolApprovalResult(toolexec.ToolApprovalResult{
				Decision: toolexec.ToolApprovalDecisionRejected,
				Reason:   toolapproval.PolicyDeniedReason,
				Metadata: locationMetadata,
			}, call.ToolName), nil
		}
		if eval.Decision == toolapproval.DecisionBypass && !forcedApproval {
			return toolexec.ToolApprovalResult{
				Decision: toolexec.ToolApprovalDecisionApproved,
				Metadata: locationMetadata,
				Input:    approvedInput,
			}, nil
		}
		if p.Facts.DecisionMode != sessionfacts.DecisionPark {
			reason := "tool execution requires approval, but this session type cannot request approval"
			rejected, err := s.toolApproval.CreateSystemRejected(ctx, input, reason)
			if err != nil {
				return toolexec.ToolApprovalResult{}, err
			}
			return s.limitToolApprovalResult(toolexec.ToolApprovalResult{
				Decision:   toolexec.ToolApprovalDecisionRejected,
				ApprovalID: rejected.ID,
				Reason:     reason,
				Metadata:   approvalResultMetadata(rejected),
			}, call.ToolName), nil
		}
		req, err := s.toolApproval.CreatePending(ctx, input)
		if err != nil {
			return toolexec.ToolApprovalResult{}, err
		}
		return toolexec.ToolApprovalResult{
			Decision:   toolexec.ToolApprovalDecisionDeferred,
			ApprovalID: req.ID,
			Metadata:   approvalResultMetadata(req),
			Input:      approvedInput,
		}, nil
	}
}

func isWorkspaceTargetTool(toolName string) bool {
	if toolName == "mcp_manage" || toolName == "app_manage" {
		return false
	}
	_, ok := toolapproval.OperationForTool(toolName)
	return ok
}

func approvalResultMetadata(req toolapproval.Request) map[string]any {
	metadata := map[string]any{
		"short_id":     req.ShortID,
		"status":       req.Status,
		"tool_name":    req.ToolName,
		"operation":    req.Operation,
		"tool_call_id": req.ToolCallID,
		// The nested approval payload is what the stream converter reads for
		// option-aware cards; without it the committed-decision publish path
		// drops the agent's options and the recorded selection.
		"approval": toolapproval.RequestMetadata(req),
	}
	if req.ExecutionLocation != nil {
		metadata[toolapproval.ExecutionLocationMetadataKey] = req.ExecutionLocation
	}
	return metadata
}

func executionLocationResultMetadata(location *toolapproval.ExecutionLocation) map[string]any {
	if location == nil {
		return nil
	}
	return map[string]any{toolapproval.ExecutionLocationMetadataKey: location}
}

// requestRunFacts returns the facts a chat request runs under. A request that
// names no mode is a message answered on the streaming path: its reply reaches
// the sender, who can also answer decisions.
func (s *Service) requestRunFacts(ctx context.Context, req runreq.ChatRequest) sessionfacts.Facts {
	if strings.TrimSpace(req.SessionType) == "" {
		return sessionfacts.Preset(sessionmode.Chat)
	}
	_, _, stored := s.resolveRunConfigSessionDescriptor(ctx, req.ThreadID)
	return runFacts(stored, req.SessionType)
}

// runFacts returns the facts one run executes under: the session's own,
// except that a schedule fire has nobody to answer a decision even in a
// session people converse in.
func runFacts(session sessionfacts.Facts, runMode string) sessionfacts.Facts {
	if strings.TrimSpace(runMode) == sessionmode.Schedule {
		session.DecisionMode = sessionfacts.DecisionAutoReject
	}
	return session
}

func (s *Service) resolveRunConfigSessionDescriptor(ctx context.Context, sessionID string) (string, string, sessionfacts.Facts) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || s == nil || s.sessionService == nil {
		return sessionpkg.TypeChat, sessionpkg.RuntimeModel, sessionfacts.Preset(sessionpkg.TypeChat)
	}
	sess, err := s.sessionService.Get(ctx, sessionID)
	if err != nil {
		if s.logger != nil {
			s.logger.WarnContext(ctx, "ResolveRunConfig: session lookup failed; falling back to chat session type",
				slog.String("session_id", sessionID),
				slog.Any("error", err),
			)
		}
		return sessionpkg.TypeChat, sessionpkg.RuntimeModel, sessionfacts.Preset(sessionpkg.TypeChat)
	}
	typ := strings.TrimSpace(sess.Type)
	if typ == "" {
		typ = sessionpkg.TypeChat
	}
	runtimeType := strings.TrimSpace(sess.RuntimeType)
	if runtimeType == "" {
		if sessionpkg.IsACPRuntime(sess) {
			runtimeType = sessionpkg.RuntimeACPAgent
		} else {
			runtimeType = sessionpkg.RuntimeModel
		}
	}
	return typ, runtimeType, sess.Facts
}

func (s *Service) resolveRunConfigSessionType(ctx context.Context, sessionID string) string {
	typ, _, _ := s.resolveRunConfigSessionDescriptor(ctx, sessionID)
	return typ
}

func buildModelSelectionRequest(p baseRunConfigParams, chatID string) runreq.ChatRequest {
	return runreq.ChatRequest{
		BotID:          p.BotID,
		ChatID:         chatID,
		ThreadID:       p.SessionID,
		CurrentChannel: p.CurrentPlatform,
		Model:          p.Model,
		Provider:       p.Provider,
	}
}

// ResolveRunConfig builds a complete RunConfig (model, system prompt, tools,
// identity) for a bot+session without loading messages or requiring a query.
// The caller is responsible for filling RunConfig.Messages.
// Used by discuss turns to reuse the service's model, tools, and prompt pipeline.
func (s *Service) ResolveRunConfig(ctx context.Context, botID, sessionID, channelIdentityID, currentPlatform, replyTarget, conversationType, chatToken string) (ResolveRunConfigResult, error) {
	if strings.TrimSpace(botID) == "" {
		return ResolveRunConfigResult{}, errors.New("bot id is required")
	}

	sessionType, runtimeType, stored := s.resolveRunConfigSessionDescriptor(ctx, sessionID)
	facts := runFacts(stored, sessionType)
	if runtimeType == sessionpkg.RuntimeACPAgent || sessionpkg.IsDirectRuntimeType(runtimeType) {
		// Agent runtimes (ACP and the direct external agents) resolve their
		// own model and prompt; only the identity travels in the run config.
		cfg := native.RunConfig{
			SessionType:   sessionType,
			ReplyDelivery: facts.ReplyDelivery,
			DecisionMode:  facts.DecisionMode,
			Identity: native.SessionContext{
				BotID:             botID,
				ChatID:            botID,
				SessionID:         sessionID,
				ChannelIdentityID: strings.TrimSpace(channelIdentityID),
				CurrentPlatform:   currentPlatform,
				ReplyTarget:       strings.TrimSpace(replyTarget),
				ConversationType:  strings.TrimSpace(conversationType),
				SessionToken:      chatToken,
			},
		}
		return ResolveRunConfigResult{
			RunConfig:   cfg,
			RuntimeType: runtimeType,
		}, nil
	}
	// Resumed turns (ask_user answers, tool approvals, discuss) carry no
	// request model — the session's remembered pair is their model source
	// (issue #879), with the same schedule exclusion as resolve().
	sessionPrefModelID, sessionPrefEffort := "", ""
	if !strings.EqualFold(strings.TrimSpace(sessionType), sessionmode.Schedule) {
		sessionPrefModelID, sessionPrefEffort = s.sessionModelPreference(ctx, sessionID)
	}
	cfg, chatModel, _, err := s.buildBaseRunConfig(ctx, baseRunConfigParams{
		BotID:              botID,
		SessionID:          sessionID,
		ChannelIdentityID:  channelIdentityID,
		CurrentPlatform:    currentPlatform,
		ReplyTarget:        replyTarget,
		ConversationType:   conversationType,
		SessionToken:       chatToken,
		SessionType:        sessionType,
		Facts:              facts,
		SessionPrefModelID: sessionPrefModelID,
		SessionPrefEffort:  sessionPrefEffort,
	})
	if err != nil {
		return ResolveRunConfigResult{}, err
	}

	contextBudget := s.effectiveContextTokenBudget(chatModel)
	cfg.ContextBudgetMaxTokens = contextBudget
	cfg = s.prepareRunConfig(ctx, cfg)
	return ResolveRunConfigResult{
		RunConfig:              cfg,
		ModelID:                chatModel.ID,
		RuntimeType:            runtimeType,
		ContextBudgetMaxTokens: contextBudget,
	}, nil
}

// prepareRunConfig generates the system prompt and appends the user message.
func (s *Service) prepareRunConfig(ctx context.Context, cfg native.RunConfig) native.RunConfig {
	cfg.ContextHookText = ""
	beforePromptResult := s.runPromptHook(ctx, agentRunConfigView{
		BotID:        cfg.Identity.BotID,
		SessionID:    cfg.Identity.SessionID,
		ChatID:       cfg.Identity.ChatID,
		SessionType:  cfg.SessionType,
		MessageCount: len(cfg.Messages),
	}, hooks.EventBeforePromptBuild)
	beforePromptContext := beforePromptResult.AppendContext
	var files []native.SystemFile
	limits := native.DefaultLimits()
	if s.agent != nil {
		limits = s.agent.Limits()
		nowFn := time.Now
		if cfg.Identity.TimezoneLocation != nil {
			nowFn = func() time.Time { return time.Now().In(cfg.Identity.TimezoneLocation) }
		}
		fs := native.NewFSClient(s.agent.BridgeProvider(), cfg.Identity.BotID, nowFn)
		files = fs.LoadSystemFiles(ctx)
	}

	platformIdentitiesSection := ""
	var platformIdentityItems []native.SystemPromptItem
	if s.platformIdentities != nil {
		identities, err := s.platformIdentities.ListPlatformIdentities(ctx, cfg.Identity.BotID)
		if err != nil {
			s.logger.WarnContext(ctx, "load bot platform identities failed",
				slog.String("bot_id", cfg.Identity.BotID),
				slog.Any("error", err),
			)
		} else {
			platformIdentityItems = buildPlatformIdentityPromptItems(identities)
			platformIdentitiesSection = buildPlatformIdentitiesSectionFromItems(platformIdentityItems)
		}
	}
	systemParams := native.SystemPromptParams{
		SessionType:               cfg.SessionType,
		ReplyDelivery:             cfg.ReplyDelivery,
		Bot:                       cfg.Bot,
		Skills:                    cfg.Skills,
		Files:                     files,
		MaxFilesBytes:             limits.SystemFilesMaxBytes,
		Timezone:                  cfg.Identity.Timezone,
		PlatformIdentitiesSection: platformIdentitiesSection,
		PlatformIdentities:        platformIdentityItems,
	}
	cfg.System = native.GenerateSystemPrompt(systemParams)
	var promptHookTexts []string
	if beforePromptContext != "" {
		text := formatServiceHookContext(hooks.EventBeforePromptBuild, beforePromptContext)
		promptHookTexts = append(promptHookTexts, text)
	}
	beforePromptObservedTexts := append([]string(nil), promptHookTexts...)
	beforePromptObservedTexts = append(beforePromptObservedTexts, hookSystemSectionTexts(beforePromptResult)...)
	afterPromptResult := s.runPromptHook(ctx, agentRunConfigView{
		BotID:        cfg.Identity.BotID,
		SessionID:    cfg.Identity.SessionID,
		ChatID:       cfg.Identity.ChatID,
		SessionType:  cfg.SessionType,
		MessageCount: len(cfg.Messages),
		SystemBytes:  afterPromptHookSystemBytes(cfg.System, beforePromptObservedTexts),
	}, hooks.EventAfterPromptBuild)
	afterPromptContext := afterPromptResult.AppendContext
	if afterPromptContext != "" {
		text := formatServiceHookContext(hooks.EventAfterPromptBuild, afterPromptContext)
		promptHookTexts = append(promptHookTexts, text)
	}
	cfg.ContextHookText = strings.Join(promptHookTexts, "\n\n")

	if cfg.Query != "" {
		var extra []sdk.MessagePart
		for _, img := range cfg.InlineImages {
			if strings.TrimSpace(img.Image) != "" {
				extra = append(extra, img)
			}
		}
		extra = append(extra, cfg.InlineAttachments...)
		cfg.Messages = append(cfg.Messages, sdk.UserMessage(cfg.Query, extra...))
		cfg.ForkContextSourceMessageIDs = append(cfg.ForkContextSourceMessageIDs, "")
		index := len(cfg.Messages) - 1
		cfg.ContextCurrentUserMessageIndex = &index
		cfg.ContextQueryMaterialized = true
	} else if len(cfg.InlineImages) > 0 || len(cfg.InlineAttachments) > 0 {
		// Pipeline path: the user query is already embedded in the RC messages,
		// but media parts are not rendered by the pipeline renderer. Inject the
		// inline media into the last user message so the model receives them.
		imageParts := make([]sdk.MessagePart, 0, len(cfg.InlineImages)+len(cfg.InlineAttachments))
		for _, img := range cfg.InlineImages {
			if strings.TrimSpace(img.Image) != "" {
				imageParts = append(imageParts, img)
			}
		}
		imageParts = append(imageParts, cfg.InlineAttachments...)
		if len(imageParts) > 0 {
			currentIndex := validCurrentUserMessageIndex(
				cfg.Messages,
				cfg.ContextCurrentUserMessageIndex,
				cfg.ContextMemoryMessageIndex,
			)
			injected := false
			for i := len(cfg.Messages) - 1; i >= 0; i-- {
				if cfg.Messages[i].Role == sdk.MessageRoleUser {
					cfg.Messages[i].Content = append(cfg.Messages[i].Content, imageParts...)
					if i < len(cfg.ForkContextSourceMessageIDs) {
						cfg.ForkContextSourceMessageIDs[i] = ""
					}
					injected = true
					break
				}
			}
			if !injected {
				cfg.Messages = append(cfg.Messages, sdk.UserMessage("", imageParts...))
				cfg.ForkContextSourceMessageIDs = append(cfg.ForkContextSourceMessageIDs, "")
				index := len(cfg.Messages) - 1
				currentIndex = &index
			}
			cfg.ContextCurrentUserMessageIndex = currentIndex
			cfg.ContextQueryMaterialized = true
		}
	}

	hookBuild := buildHookSystemSections([]promptHookOutput{
		{Event: hooks.EventBeforePromptBuild, Result: beforePromptResult},
		{Event: hooks.EventAfterPromptBuild, Result: afterPromptResult},
	}, cfg.ContextScope)
	cfg.ContextSourceFrags = buildProviderSourceFrags(
		ctx,
		cfg,
		native.GenerateSystemSections(systemParams),
		hookBuild.Frags,
	)
	cfg.ContextSourceWarnings = hookBuild.Warnings
	return cfg.RefreshContextFrag()
}

func normalizeContextMessages(messages []runreq.ModelMessage) []runreq.ModelMessage {
	return runreq.RepairToolCallClosures(runreq.SanitizeMessages(messages), runreq.SyntheticToolClosureError)
}

func validCurrentUserMessageIndex(messages []sdk.Message, currentIndex, memoryIndex *int) *int {
	if currentIndex != nil && *currentIndex >= 0 && *currentIndex < len(messages) &&
		messages[*currentIndex].Role == sdk.MessageRoleUser && (memoryIndex == nil || *currentIndex != *memoryIndex) {
		return runreq.IntPointer(*currentIndex)
	}
	return latestUserMessageIndex(messages, optionalIndex(memoryIndex)...)
}

func latestUserMessageIndex(messages []sdk.Message, excluded ...int) *int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == sdk.MessageRoleUser && !containsIndex(excluded, i) {
			return runreq.IntPointer(i)
		}
	}
	return nil
}

func optionalIndex(index *int) []int {
	if index == nil {
		return nil
	}
	return []int{*index}
}

func containsIndex(indexes []int, target int) bool {
	for _, index := range indexes {
		if index == target {
			return true
		}
	}
	return false
}

func normalizeGatewaySkill(entry SkillEntry) (native.SkillEntry, bool) {
	name := strings.TrimSpace(entry.Name)
	if name == "" {
		return native.SkillEntry{}, false
	}
	description := strings.TrimSpace(entry.Description)
	if description == "" {
		description = name
	}
	content := strings.TrimSpace(entry.Content)
	if content == "" {
		content = description
	}
	return native.SkillEntry{
		Name:        name,
		Description: description,
		Content:     content,
		Path:        strings.TrimSpace(entry.Path),
		Metadata:    entry.Metadata,
	}, true
}

// userQueryNeedsHeader reports whether resolve should wrap req.Query in the
// user message header (sender/channel/time/attachment paths). Text always
// gets a header. An attachment-only message (no caption) must too: the header
// is the only thing telling the model who sent what file, and the headerified
// query is what makes the user turn (with its asset links) persist to history.
// The one deliberate exception is a no-prompt skill activation, whose stored
// user content must stay empty (its model text travels via ModelQuery, and
// display state via skill_activation metadata).
func userQueryNeedsHeader(req runreq.ChatRequest, attachmentCount int) bool {
	if strings.TrimSpace(req.Query) != "" {
		return true
	}
	return attachmentCount > 0 && req.UserMessageKind != runreq.UserMessageKindSkillActivation
}

// extractAttachmentPaths collects container file paths from ALL gateway
// attachments — both tool_file_ref (fallback) and native images that carry a
// FallbackPath. This ensures the YAML user header always lists every
// attachment the user sent, regardless of whether the model consumes the
// image natively or via the read_media tool.
func extractAttachmentPaths(attachments []any) []string {
	var paths []string
	for _, att := range attachments {
		ga, ok := att.(gatewayAttachment)
		if !ok {
			continue
		}
		if ga.Transport == gatewayTransportToolFileRef && strings.TrimSpace(ga.Payload) != "" {
			paths = append(paths, ga.Payload)
		} else if strings.TrimSpace(ga.FallbackPath) != "" {
			paths = append(paths, ga.FallbackPath)
		}
	}
	return paths
}

// extractNativeImageParts returns sdk.ImagePart entries for attachments that
// the model can consume as inline multimodal input (vision-capable images with
// an inline data URL or public URL payload).
func extractNativeImageParts(attachments []any) []sdk.ImagePart {
	var parts []sdk.ImagePart
	for _, att := range attachments {
		ga, ok := att.(gatewayAttachment)
		if !ok || ga.Type != "image" {
			continue
		}
		transport := strings.ToLower(strings.TrimSpace(ga.Transport))
		if transport != gatewayTransportInlineDataURL && transport != gatewayTransportPublicURL {
			continue
		}
		payload := strings.TrimSpace(ga.Payload)
		if payload == "" {
			continue
		}
		// An animated attachment carries every rendered frame; anything else
		// is its single payload.
		images := ga.Frames
		if len(images) == 0 {
			images = []string{payload}
		}
		for _, image := range images {
			parts = append(parts, sdk.ImagePart{
				Image:     image,
				MediaType: strings.TrimSpace(ga.Mime),
			})
		}
	}
	return parts
}
