package native

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	userinput "github.com/memohai/memoh/internal/agent/decision/input"
	tools "github.com/memohai/memoh/internal/agent/tool"
	"github.com/memohai/memoh/internal/apperror"
	"github.com/memohai/memoh/internal/hooks"
	"github.com/memohai/memoh/internal/models"
	"github.com/memohai/memoh/internal/workspace/bridge"
)

// Agent is the core agent that handles LLM interactions.
type Agent struct {
	client             *sdk.Client
	toolProviders      []tools.ToolProvider
	bridgeProvider     bridge.Provider
	hookService        *hooks.Service
	logger             *slog.Logger
	limits             Limits
	contextViewApplier ContextViewApplier
	prefixCache        *prefixCacheTracker
	loopReselectMode   LoopReselectMode
}

const streamCancelDrainGrace = 250 * time.Millisecond

// New creates a new Agent with the given dependencies.
func New(deps Deps) *Agent {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Agent{
		client:             sdk.NewClient(),
		bridgeProvider:     deps.BridgeProvider,
		hookService:        deps.HookService,
		logger:             logger.With(slog.String("service", "agent/runtime/native")),
		limits:             deps.Limits.Normalize(),
		contextViewApplier: deps.ContextViewApplier,
		prefixCache:        newPrefixCacheTracker(),
		loopReselectMode:   deps.LoopReselectMode.Normalize(),
	}
}

// LoopReselectMode returns the normalized rollout mode for the in-loop
// context step reselector. A nil Agent defaults to LoopReselectActive so
// tests that construct RunConfig without an Agent keep current behavior.
func (a *Agent) LoopReselectMode() LoopReselectMode {
	if a == nil {
		return LoopReselectActive
	}
	return a.loopReselectMode.Normalize()
}

// applyContextView routes the run config through the injected context view
// applier so selection, placement and the cache plan cover the final provider
// input. Without an applier the legacy compile path keeps the frag view fresh.
func (a *Agent) applyContextView(ctx context.Context, cfg RunConfig) (RunConfig, error) {
	if a != nil && a.contextViewApplier != nil {
		return a.contextViewApplier(ctx, cfg)
	}
	return cfg.RefreshContextFrag(), nil
}

const publicContextPreparationError = "The model context could not be prepared."

func contextViewStreamError(err error) StreamEvent {
	var code apperror.Code
	switch {
	case errors.Is(err, contextfrag.ErrProtectedContextOverflow):
		code = apperror.CodeContextProtectedOverflow
	case errors.Is(err, contextfrag.ErrBudgetUnsatisfied):
		code = apperror.CodeContextBudgetUnsatisfied
	default:
		return StreamEvent{Type: EventError, Error: publicContextPreparationError}
	}
	public, ok := apperror.PublicFrom(apperror.New(code, nil), "")
	if !ok {
		return StreamEvent{Type: EventError, Error: publicContextPreparationError}
	}
	return StreamEvent{
		Type:  EventError,
		Code:  string(public.Code),
		Error: public.Detail,
	}
}

// BridgeProvider returns the underlying bridge provider (workspace manager).
func (a *Agent) BridgeProvider() bridge.Provider {
	return a.bridgeProvider
}

func (a *Agent) Limits() Limits {
	if a == nil {
		return DefaultLimits()
	}
	return a.limits.Normalize()
}

// SetToolProviders sets the tool providers after construction.
// This allows breaking dependency cycles in the DI graph.
func (a *Agent) SetToolProviders(providers []tools.ToolProvider) {
	a.toolProviders = providers
}

// Stream runs the agent in streaming mode, emitting events to the returned channel.
func (a *Agent) Stream(ctx context.Context, cfg RunConfig) <-chan StreamEvent {
	ch := make(chan StreamEvent)
	go func() {
		defer close(ch)
		a.runStream(ctx, cfg, ch)
	}()
	return ch
}

// Generate runs the agent in non-streaming mode, returning the complete result.
func (a *Agent) Generate(ctx context.Context, cfg RunConfig) (*GenerateResult, error) {
	return a.runGenerate(ctx, cfg)
}

func (a *Agent) ExecuteTool(ctx context.Context, cfg RunConfig, call sdk.ToolCall) (sdk.ToolResultPart, error) {
	sdkTools, _, _, err := a.assembleTools(ctx, cfg, nil, false)
	if err != nil {
		return sdk.ToolResultPart{}, fmt.Errorf("assemble tools: %w", err)
	}
	sdkTools, _ = decorateReadMediaTools(cfg.Model, sdkTools)
	sdkTools = tools.WrapToolOutputLimits(sdkTools, a.Limits().ToolOutputLimit())
	for i := range sdkTools {
		tool := sdkTools[i]
		if tool.Name != call.ToolName {
			continue
		}
		if tool.Execute == nil {
			return sdk.ToolResultPart{}, fmt.Errorf("tool %q has no execute handler", call.ToolName)
		}
		execCtx := &sdk.ToolExecContext{
			Context:    ctx,
			ToolCallID: call.ToolCallID,
			ToolName:   call.ToolName,
		}
		output, err := tool.Execute(execCtx, call.Input)
		if err != nil {
			limitedErr := tools.LimitToolError(err, "tool result ("+call.ToolName+")", a.Limits().ToolOutputLimit())
			return sdk.ToolResultPart{
				ToolCallID: call.ToolCallID,
				ToolName:   call.ToolName,
				Result:     limitedErr.Error(),
				IsError:    true,
			}, nil
		}
		return sdk.ToolResultPart{
			ToolCallID: call.ToolCallID,
			ToolName:   call.ToolName,
			Result:     publicReadMediaToolResult(output),
		}, nil
	}
	return sdk.ToolResultPart{}, fmt.Errorf("tool %q not found", call.ToolName)
}

// sendEvent sends an event to the stream channel. It returns false if the
// context was cancelled (consumer stopped reading), allowing the caller to
// abort cleanly instead of leaking the goroutine on a blocked channel send.
func sendEvent(ctx context.Context, ch chan<- StreamEvent, evt StreamEvent) bool {
	select {
	case ch <- evt:
		return true
	case <-ctx.Done():
		return false
	}
}

func (a *Agent) runStream(ctx context.Context, cfg RunConfig, ch chan<- StreamEvent) {
	streamCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	aborted := false
	turnError := ""
	defer func() {
		event := hooks.EventTurnEnd
		if aborted || strings.TrimSpace(turnError) != "" {
			event = hooks.EventTurnError
			if strings.TrimSpace(turnError) == "" {
				turnError = "agent run aborted"
			}
		}
		a.runTurnHook(context.WithoutCancel(ctx), cfg, event, turnError)
	}()
	defer func() {
		a.logContextLifecycle(cfg)
	}()

	// Stream emitter: tools targeting the current conversation push
	// side-effect events (attachments, reactions, speech) directly here.
	// Uses sendEvent to avoid goroutine leaks when the consumer stops reading.
	streamEmitter := tools.StreamEmitter(func(evt tools.ToolStreamEvent) {
		sendEvent(ctx, ch, toolStreamEventToAgentEvent(evt))
	})
	if cfg.ForkContext == nil {
		cfg.ForkContext = tools.NewMessageSnapshotWithSources(cfg.Messages, cfg.ForkContextSourceMessageIDs)
	}

	var sdkTools []sdk.Tool
	if cfg.SupportsToolCall {
		var toolUsage string
		var err error
		var toolDefs []contextfrag.ToolDefAccounting
		sdkTools, toolUsage, toolDefs, err = a.assembleTools(streamCtx, cfg, streamEmitter, cfg.LiveToolStream)
		if err != nil {
			turnError = fmt.Sprintf("assemble tools: %v", err)
			sendEvent(ctx, ch, StreamEvent{Type: EventError, Error: turnError})
			return
		}
		cfg.ContextToolDefs = toolDefs
		if toolUsage != "" {
			// Fragment-first runs hand the usage to the view, which shapes it
			// as its own system fragment; legacy runs append it to the system
			// text so the view can split it back out.
			if len(cfg.ContextSourceFrags) == 0 {
				cfg.System = appendToolUsageToSystem(cfg.System, toolUsage)
			}
			cfg.ContextToolUsage = toolUsage
		}
	}
	limit := a.Limits().ToolOutputLimit()
	sdkTools, readMediaState := decorateReadMediaTools(cfg.Model, sdkTools)
	cfg.ContextDynamicMutators = cfg.contextDynamicMutators(readMediaState != nil, a != nil && a.hookService != nil, true)
	var contextViewErr error
	cfg, contextViewErr = a.applyContextView(streamCtx, cfg)
	if contextViewErr != nil {
		publicError := contextViewStreamError(contextViewErr)
		turnError = publicError.Error
		a.logger.Warn("context view preflight failed", slog.Any("error", contextViewErr))
		sendEvent(ctx, ch, publicError)
		return
	}
	if readMediaState != nil {
		readMediaState.ledger = cfg.ContextMutations
	}
	sdkTools = tools.WrapToolOutputLimits(sdkTools, limit)
	approvalTools := append([]sdk.Tool(nil), sdkTools...)
	sdkTools = a.wrapToolsWithHooks(ctx, cfg, sdkTools)
	sdkTools = tools.WrapToolOutputLimits(sdkTools, limit)
	toolExecutionMetadata := newToolExecutionMetadataRegistry(func(call sdk.ToolCall, metadata map[string]any) {
		sendEvent(ctx, ch, StreamEvent{
			Type:       EventToolCallMetadata,
			ToolName:   call.ToolName,
			ToolCallID: call.ToolCallID,
			Input:      call.Input,
			Metadata:   metadata,
		})
	})
	cfg.ToolApprovalHandler = toolExecutionMetadata.wrap(cfg.ToolApprovalHandler)

	// Loop detection setup
	var textLoopGuard *TextLoopGuard
	var textLoopProbeBuffer *TextLoopProbeBuffer
	var toolLoopGuard *ToolLoopGuard
	toolLoopAbortCallIDs := newToolAbortRegistry()
	if cfg.LoopDetection.Enabled {
		textLoopGuard = NewTextLoopGuard(LoopDetectedStreakThreshold, LoopDetectedMinNewGramsPerChunk, SentialOptions{})
		textLoopProbeBuffer = NewTextLoopProbeBuffer(LoopDetectedProbeChars, func(text string) {
			result := textLoopGuard.Inspect(text)
			if result.Abort {
				a.logger.Warn("text loop detected, will abort")
				aborted = true
				cancel(ErrTextLoopDetected)
			}
		})
		toolLoopGuard = NewToolLoopGuard(ToolLoopRepeatThreshold, ToolLoopWarningsBeforeAbort)
	}

	// Wrap tools with loop detection
	if toolLoopGuard != nil {
		sdkTools = wrapToolsWithLoopGuard(sdkTools, toolLoopGuard, toolLoopAbortCallIDs)
	}

	var prepareStep func(*sdk.GenerateParams) *sdk.GenerateParams
	if readMediaState != nil {
		prepareStep = readMediaState.prepareStep
	}

	initialMsgCount := len(cfg.Messages)

	if cfg.InjectCh != nil {
		basePrepare := prepareStep
		prepareStep = func(p *sdk.GenerateParams) *sdk.GenerateParams {
			if basePrepare != nil {
				if override := basePrepare(p); override != nil {
					p = override
				}
			}
			for {
				select {
				case injected, ok := <-cfg.InjectCh:
					if !ok {
						break
					}
					text := injectedMessageText(injected)
					if text != "" || (cfg.SupportsImageInput && len(injected.ImageParts) > 0) {
						insertAfter := len(p.Messages) - initialMsgCount
						var extra []sdk.MessagePart
						if cfg.SupportsImageInput {
							for _, img := range injected.ImageParts {
								if strings.TrimSpace(img.Image) != "" {
									extra = append(extra, img)
								}
							}
						}
						p.Messages = append(p.Messages, sdk.UserMessage(text, extra...))
						cfg.ContextMutations.Record(contextfrag.MutationInjectedMessage, fmt.Sprintf("bytes=%d", len(text)))
						if cfg.InjectedRecorder != nil {
							cfg.InjectedRecorder(text, insertAfter)
						}
						a.logger.Info("injected user message into agent stream",
							slog.String("bot_id", cfg.Identity.BotID),
							slog.Int("insert_after", insertAfter),
							slog.Int("image_parts", len(extra)),
						)
					}
					continue
				default:
				}
				break
			}
			return p
		}
	}

	prepareStep = a.wrapPrepareStepWithModelHook(streamCtx, cfg, prepareStep)
	var err error
	cfg, err = a.applyBeforeModelCallHook(streamCtx, cfg, 0)
	if err != nil {
		turnError = err.Error()
		sendEvent(ctx, ch, StreamEvent{Type: EventError, Error: turnError})
		return
	}
	if a == nil || a.contextViewApplier == nil {
		cfg = cfg.RefreshContextFrag()
	}
	opts := a.buildGenerateOptions(streamCtx, cfg, sdkTools, approvalTools, prepareStep)
	opts = append(opts, a.onStepOption(streamCtx, cfg, nil))

	retryCfg := cfg.Retry
	if retryCfg.MaxAttempts <= 0 {
		retryCfg = DefaultRetryConfig()
	}

	var streamResult *sdk.StreamResult
	for attempt := 0; attempt < retryCfg.MaxAttempts; attempt++ {
		var err error
		streamResult, err = a.client.StreamText(streamCtx, opts...)
		if err == nil {
			break
		}
		if !isRetryableStreamError(err) {
			turnError = fmt.Sprintf("stream start: %v", err)
			sendEvent(ctx, ch, StreamEvent{Type: EventError, Error: turnError})
			return
		}
		a.logger.Warn("stream start failed, retrying",
			slog.Int("attempt", attempt+1),
			slog.Int("max_attempts", retryCfg.MaxAttempts),
			slog.String("error", err.Error()),
		)
		if !sendEvent(ctx, ch, StreamEvent{
			Type:       EventRetry,
			Attempt:    attempt + 1,
			MaxAttempt: retryCfg.MaxAttempts,
			RetryError: err.Error(),
		}) {
			return
		}
		if attempt+1 >= retryCfg.MaxAttempts {
			turnError = fmt.Sprintf("stream start: all %d attempts failed (last: %v)", retryCfg.MaxAttempts, err)
			sendEvent(ctx, ch, StreamEvent{Type: EventError, Error: turnError})
			return
		}
		delay := retryDelay(attempt, retryCfg)
		if delay > 0 {
			if err := sleepWithContext(streamCtx, delay); err != nil {
				turnError = fmt.Sprintf("stream start: context cancelled during retry: %v", err)
				sendEvent(ctx, ch, StreamEvent{Type: EventError, Error: turnError})
				return
			}
		}
	}

	sendEvent(ctx, ch, StreamEvent{Type: EventAgentStart})

	var allText strings.Builder
	stepNumber := 0

	streamClosed := false
	for !aborted && !streamClosed {
		var part sdk.StreamPart
		select {
		case <-streamCtx.Done():
			aborted = true
			continue
		case next, ok := <-streamResult.Stream:
			if !ok {
				streamClosed = true
				continue
			}
			part = next
		}

		switch p := part.(type) {
		case *sdk.StartPart:
			_ = p // stream start already emitted

		case *sdk.TextStartPart:
			if !sendEvent(ctx, ch, StreamEvent{Type: EventTextStart}) {
				aborted = true
			}

		case *sdk.TextDeltaPart:
			if p.Text != "" {
				if textLoopProbeBuffer != nil {
					textLoopProbeBuffer.Push(p.Text)
				}
				if !sendEvent(ctx, ch, StreamEvent{Type: EventTextDelta, Delta: p.Text}) {
					aborted = true
				}
				allText.WriteString(p.Text)
			}

		case *sdk.TextEndPart:
			if textLoopProbeBuffer != nil {
				textLoopProbeBuffer.Flush()
			}
			stepNumber++
			if !sendEvent(ctx, ch, StreamEvent{Type: EventTextEnd}) ||
				!sendEvent(ctx, ch, StreamEvent{
					Type:           EventProgress,
					StepNumber:     stepNumber,
					ProgressStatus: "text",
				}) {
				aborted = true
			}

		case *sdk.ReasoningStartPart:
			if !sendEvent(ctx, ch, StreamEvent{Type: EventReasoningStart}) {
				aborted = true
			}

		case *sdk.ReasoningDeltaPart:
			if !sendEvent(ctx, ch, StreamEvent{Type: EventReasoningDelta, Delta: p.Text}) {
				aborted = true
			}

		case *sdk.ReasoningEndPart:
			if !sendEvent(ctx, ch, StreamEvent{Type: EventReasoningEnd}) {
				aborted = true
			}

		case *sdk.ToolInputStartPart:
			// ToolInputStartPart fires before tool input args have streamed.
			// We emit a lightweight tool_call_input_start (name + call ID, no
			// input) so the Web UI can render the tool block immediately while
			// arguments are still streaming. StreamToolCallPart below backfills
			// the fully-assembled Input under the same call ID. IM/Discuss
			// adapters do not map tool_call_input_start, so they keep their
			// single-start behavior and avoid duplicate "running" messages.
			if textLoopProbeBuffer != nil {
				textLoopProbeBuffer.Flush()
			}
			if !sendEvent(ctx, ch, StreamEvent{
				Type:       EventToolCallInputStart,
				ToolName:   p.ToolName,
				ToolCallID: p.ID,
			}) {
				aborted = true
			}

		case *sdk.StreamToolCallPart:
			if textLoopProbeBuffer != nil {
				textLoopProbeBuffer.Flush()
			}
			if !sendEvent(ctx, ch, StreamEvent{
				Type:       EventToolCallStart,
				ToolName:   p.ToolName,
				ToolCallID: p.ToolCallID,
				Input:      p.Input,
			}) {
				aborted = true
			}

		case *sdk.ToolProgressPart:
			if !sendEvent(ctx, ch, StreamEvent{
				Type:       EventToolCallProgress,
				ToolName:   p.ToolName,
				ToolCallID: p.ToolCallID,
				Metadata:   toolExecutionMetadata.metadata(p.ToolCallID),
				Progress:   p.Content,
			}) {
				aborted = true
			}

		case *sdk.ToolApprovalRequestPart:
			eventType := EventToolApprovalRequest
			var userInputID string
			var approvalID string
			if isUserInputMetadata(p.Metadata) {
				eventType = EventUserInputRequest
				userInputID = p.ApprovalID
			} else {
				approvalID = p.ApprovalID
			}
			if !sendEvent(ctx, ch, StreamEvent{
				Type:        eventType,
				ToolName:    p.ToolName,
				ToolCallID:  p.ToolCallID,
				ApprovalID:  approvalID,
				UserInputID: userInputID,
				ShortID:     approvalShortID(p.Metadata),
				Status:      "pending",
				Input:       p.Input,
				Metadata:    p.Metadata,
			}) {
				aborted = true
			}

		case *sdk.StreamToolResultPart:
			shouldAbort := toolLoopAbortCallIDs.Take(p.ToolCallID)
			stepNumber++
			if !sendEvent(ctx, ch, StreamEvent{
				Type:       EventToolCallEnd,
				ToolName:   p.ToolName,
				ToolCallID: p.ToolCallID,
				Input:      p.Input,
				Metadata:   toolExecutionMetadata.metadata(p.ToolCallID),
				Result:     p.Output,
			}) || !sendEvent(ctx, ch, StreamEvent{
				Type:           EventProgress,
				StepNumber:     stepNumber,
				ToolName:       p.ToolName,
				ProgressStatus: "tool_result",
			}) {
				aborted = true
			}
			if shouldAbort {
				a.logger.Warn("tool loop abort triggered", slog.String("tool_call_id", p.ToolCallID))
				cancel(ErrToolLoopDetected)
				aborted = true
			}

		case *sdk.StreamToolErrorPart:
			// Take before errors.Is so registry IDs from the loop guard are always cleared.
			tookLoopAbort := toolLoopAbortCallIDs.Take(p.ToolCallID)
			shouldAbort := errors.Is(p.Error, ErrToolLoopDetected) || tookLoopAbort
			if !sendEvent(ctx, ch, StreamEvent{
				Type:       EventToolCallEnd,
				ToolName:   p.ToolName,
				ToolCallID: p.ToolCallID,
				Metadata:   toolExecutionMetadata.metadata(p.ToolCallID),
				Error:      p.Error.Error(),
			}) {
				aborted = true
			}
			if shouldAbort {
				a.logger.Warn("tool loop abort triggered", slog.String("tool_call_id", p.ToolCallID))
				cancel(ErrToolLoopDetected)
				aborted = true
			}

		case *sdk.StreamFilePart:
			mediaType := p.File.MediaType
			if mediaType == "" {
				mediaType = "image/png"
			}
			if !sendEvent(ctx, ch, StreamEvent{
				Type: EventAttachment,
				Attachments: []FileAttachment{{
					Type: "image",
					URL:  fmt.Sprintf("data:%s;base64,%s", mediaType, p.File.Data),
					Mime: mediaType,
				}},
			}) {
				aborted = true
			}

		case *sdk.ErrorPart:
			errMsg := p.Error.Error()
			if isAskUserArgumentParseError(errMsg) {
				continue
			}
			turnError = errMsg
			sendEvent(ctx, ch, StreamEvent{Type: EventError, Error: errMsg})

			// Mid-stream retry: if the error is retryable, attempt to continue
			// the agent run from the accumulated state. This also handles
			// errors at step 0 (e.g. timeout awaiting response headers) since
			// no work has been completed yet and retrying from the start is safe.
			if isRetryableStreamError(p.Error) {
				streamResult, aborted = a.runMidStreamRetry(
					ctx, streamCtx, cancel, toolLoopAbortCallIDs,
					ch, cfg, sdkTools, approvalTools, prepareStep, streamResult,
					stepNumber, errMsg, &allText, textLoopProbeBuffer,
				)
				if !aborted {
					turnError = ""
				}
			} else {
				aborted = true
			}

		case *sdk.AbortPart:
			aborted = true

		case *sdk.FinishPart:
			// handled after loop
		}

		if aborted {
			break
		}
	}

	if aborted && !streamClosed {
		// A provider is expected to close its stream when the context is
		// cancelled, but run termination must not depend on that cooperation.
		// Preserve the final snapshot when it arrives promptly, then stop
		// waiting so the caller can fence and finalize the run as aborted.
		cancel(context.Canceled)
		streamClosed = drainStreamUntilClosed(streamResult.Stream, streamCancelDrainGrace)
	}

	if textLoopProbeBuffer != nil {
		textLoopProbeBuffer.Flush()
	}

	var finalMessages []sdk.Message
	var totalUsage sdk.Usage
	if streamClosed {
		finalMessages = streamResult.Messages
		if readMediaState != nil {
			finalMessages = readMediaState.mergeMessages(streamResult.Steps, finalMessages)
		}
		if streamResult.DeferredToolApproval != nil {
			finalMessages = annotateDeferredApproval(finalMessages, *streamResult.DeferredToolApproval)
		}
		finalMessages = toolExecutionMetadata.annotate(finalMessages)
		totalUsage = aggregateStepUsage(streamResult.Steps)
	}
	usageJSON, _ := json.Marshal(totalUsage)

	termEvent := StreamEvent{
		Messages: mustMarshal(finalMessages),
		Usage:    usageJSON,
	}
	if streamClosed && streamResult.DeferredToolApproval != nil {
		termEvent.ApprovalID = streamResult.DeferredToolApproval.ApprovalID
		if isUserInputMetadata(streamResult.DeferredToolApproval.Metadata) {
			termEvent.UserInputID = streamResult.DeferredToolApproval.ApprovalID
		}
		termEvent.ShortID = approvalShortID(streamResult.DeferredToolApproval.Metadata)
		termEvent.Status = "pending"
		termEvent.Metadata = streamResult.DeferredToolApproval.Metadata
		if toolName, ok := streamResult.DeferredToolApproval.Metadata["tool_name"].(string); ok {
			termEvent.ToolName = toolName
		}
		if toolCallID, ok := streamResult.DeferredToolApproval.Metadata["tool_call_id"].(string); ok {
			termEvent.ToolCallID = toolCallID
		}
	}
	if aborted {
		termEvent.Type = EventAgentAbort
	} else {
		termEvent.Type = EventAgentEnd
		// Warn if LLM produced no text and no tool calls — likely a context overflow.
		if allText.Len() == 0 && stepNumber == 0 {
			a.logger.Warn("agent produced empty response (no text, no tool calls)",
				slog.String("bot_id", cfg.Identity.BotID),
				slog.Int("input_messages", len(cfg.Messages)),
				slog.Int("input_tokens", totalUsage.InputTokens),
			)
		}
	}
	a.observePrefixCache(cfg)
	// Deliver the terminal event using a context that is NOT cancelled when
	// the parent ctx is cancelled (user abort / idle timeout / loop-detect).
	// Otherwise sendEvent would short-circuit on <-ctx.Done() and the consumer
	// would never receive the partial messages accumulated so far, forcing it
	// to fall back to a synthetic placeholder. A 5s deadline guards against
	// a fully-disconnected consumer hanging this goroutine forever.
	deliveryCtx, deliveryCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer deliveryCancel()
	sendEvent(deliveryCtx, ch, termEvent)
}

// prefixCacheSessionKey derives the per-session key the prefix cache tracker
// keys its entries by. Returns "" when either identifier is blank, meaning
// the run cannot be attributed to a tracked session.
func prefixCacheSessionKey(identity SessionContext) string {
	botID := strings.TrimSpace(identity.BotID)
	sessionID := strings.TrimSpace(identity.SessionID)
	if botID == "" || sessionID == "" {
		return ""
	}
	return botID + ":" + sessionID
}

// observePrefixCache compares this run's rendered stable prefix with the
// previous run of the same session and records the attribution on the
// mutation ledger, so the persisted snapshot explains cache behaviour
// without offline correlation. Subagent runs share the parent session but
// use a different prefix, so they are excluded from the comparison.
func (a *Agent) observePrefixCache(cfg RunConfig) {
	if a == nil || a.prefixCache == nil || cfg.ContextMutations == nil || cfg.Identity.IsSubagent {
		return
	}
	plan := cfg.ContextManifest.CachePlan
	if plan == nil || plan.CacheComparatorPrefixHash == "" {
		return
	}
	key := prefixCacheSessionKey(cfg.Identity)
	if key == "" {
		return
	}
	botID := strings.TrimSpace(cfg.Identity.BotID)
	sessionID := strings.TrimSpace(cfg.Identity.SessionID)
	firstStepCacheRead := 0
	if records := cfg.ContextMutations.CacheUsageRecords(); len(records) > 0 {
		firstStepCacheRead = records[0].CacheReadTokens
	}
	now := time.Now()
	model := modelID(cfg.Model)
	nowCount := cfg.ContextMutations.ComparatorPrefixMessageCount()
	prevBoundaryHash := cfg.ContextMutations.PrevBoundaryHash()
	// Compare against the snapshot this run itself peeked at build time
	// (recordPrefixCacheBoundary), not against observe()'s return value: a
	// concurrent run of the same session can have already overwritten the
	// tracker between this run's peek and this observe, which would race the
	// comparison against a stranger's entry instead of this run's own prior
	// turn. observe() below still performs the store (last-writer-wins).
	peeked := cfg.ContextMutations.PeekedPrevCacheEntry()
	prev := prefixCacheEntry{hash: peeked.Hash, model: peeked.Model, stableCount: peeked.StableCount, at: peeked.At}
	a.prefixCache.observe(key, nowCount, plan.CacheComparatorPrefixHash, model, now)
	comparison := compareCachePrefix(prev, peeked.Found, nowCount, plan.CacheComparatorPrefixHash, model, prevBoundaryHash, firstStepCacheRead, now, promptCacheTTLWindow(cfg.PromptCacheTTL))
	cfg.ContextMutations.SetCacheComparison(comparison)

	if comparison.Outcome == contextfrag.CacheOutcomeMissSamePrefix &&
		a.logger != nil &&
		models.ResolveClientType(cfg.Model) == string(models.ClientTypeAnthropicMessages) &&
		models.NormalizePromptCacheTTL(cfg.PromptCacheTTL) != models.PromptCacheTTLOff {
		a.logger.Warn("prompt cache miss despite unchanged prefix",
			slog.String("bot_id", botID),
			slog.String("session_id", sessionID),
			slog.Int64("prev_age_ms", comparison.PrevAgeMs),
			slog.Int("mutations", len(cfg.ContextMutations.Records())),
		)
	}
}

func promptCacheTTLWindow(ttl string) time.Duration {
	switch models.NormalizePromptCacheTTL(ttl) {
	case models.PromptCacheTTL1h:
		return time.Hour
	case models.PromptCacheTTLOff:
		return 0
	default:
		return 5 * time.Minute
	}
}

// logContextLifecycle emits the one-line audit summary linking the context
// view manifest to the final provider input: what was selected, what the
// cache plan pinned, and which mutations ran after the view.
func (a *Agent) logContextLifecycle(cfg RunConfig) {
	if a == nil || a.logger == nil || cfg.ContextMutations == nil {
		return
	}
	cacheOutcome := ""
	if comparison := cfg.ContextMutations.CacheComparisonValue(); comparison != nil {
		cacheOutcome = comparison.Outcome
	}
	a.logger.Debug("context lifecycle",
		slog.String("view", string(cfg.ContextManifest.View)),
		slog.Int("manifest_items", len(cfg.ContextManifest.Items)),
		slog.String("stable_prefix_hash", cfg.ContextCachePlan.StablePrefixHash),
		slog.Int("mutations", len(cfg.ContextMutations.Records())),
		slog.String("cache_outcome", cacheOutcome),
		slog.String("final_input_hash", cfg.ContextMutations.FinalInputHash()),
	)
}

func drainStreamUntilClosed(stream <-chan sdk.StreamPart, grace time.Duration) bool {
	if stream == nil {
		return true
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for {
		select {
		case _, ok := <-stream:
			if !ok {
				return true
			}
		case <-timer.C:
			return false
		}
	}
}

func (a *Agent) runGenerate(ctx context.Context, cfg RunConfig) (result *GenerateResult, retErr error) {
	genCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer func() {
		event := hooks.EventTurnEnd
		errMsg := ""
		if retErr != nil {
			event = hooks.EventTurnError
			errMsg = retErr.Error()
		}
		a.runTurnHook(context.WithoutCancel(ctx), cfg, event, errMsg)
	}()
	defer func() {
		a.logContextLifecycle(cfg)
	}()
	loopAbort := newLoopAbortState()

	// Collecting emitter: tools push side-effect events here during generation.
	collected := newToolEventCollector()
	defer collected.Close()
	collectEmitter := tools.StreamEmitter(func(evt tools.ToolStreamEvent) {
		collected.Add(evt)
	})
	if cfg.ForkContext == nil {
		cfg.ForkContext = tools.NewMessageSnapshotWithSources(cfg.Messages, cfg.ForkContextSourceMessageIDs)
	}

	var sdkTools []sdk.Tool
	if cfg.SupportsToolCall {
		var toolUsage string
		var err error
		var toolDefs []contextfrag.ToolDefAccounting
		sdkTools, toolUsage, toolDefs, err = a.assembleTools(genCtx, cfg, collectEmitter, false)
		if err != nil {
			return nil, fmt.Errorf("assemble tools: %w", err)
		}
		cfg.ContextToolDefs = toolDefs
		if toolUsage != "" {
			// Fragment-first runs hand the usage to the view, which shapes it
			// as its own system fragment; legacy runs append it to the system
			// text so the view can split it back out.
			if len(cfg.ContextSourceFrags) == 0 {
				cfg.System = appendToolUsageToSystem(cfg.System, toolUsage)
			}
			cfg.ContextToolUsage = toolUsage
		}
	}
	limit := a.Limits().ToolOutputLimit()
	sdkTools, readMediaState := decorateReadMediaTools(cfg.Model, sdkTools)
	cfg.ContextDynamicMutators = cfg.contextDynamicMutators(readMediaState != nil, a != nil && a.hookService != nil, false)
	var contextViewErr error
	cfg, contextViewErr = a.applyContextView(genCtx, cfg)
	if contextViewErr != nil {
		return nil, contextViewErr
	}
	if readMediaState != nil {
		readMediaState.ledger = cfg.ContextMutations
	}
	sdkTools = tools.WrapToolOutputLimits(sdkTools, limit)
	approvalTools := append([]sdk.Tool(nil), sdkTools...)
	sdkTools = a.wrapToolsWithHooks(ctx, cfg, sdkTools)
	sdkTools = tools.WrapToolOutputLimits(sdkTools, limit)
	toolExecutionMetadata := newToolExecutionMetadataRegistry(nil)
	cfg.ToolApprovalHandler = toolExecutionMetadata.wrap(cfg.ToolApprovalHandler)

	var toolLoopGuard *ToolLoopGuard
	var textLoopGuard *TextLoopGuard
	toolLoopAbortCallIDs := newToolAbortRegistry()
	if cfg.LoopDetection.Enabled {
		toolLoopGuard = NewToolLoopGuard(ToolLoopRepeatThreshold, ToolLoopWarningsBeforeAbort)
		textLoopGuard = NewTextLoopGuard(LoopDetectedStreakThreshold, LoopDetectedMinNewGramsPerChunk, SentialOptions{})
	}

	if toolLoopGuard != nil {
		sdkTools = wrapToolsWithLoopGuard(sdkTools, toolLoopGuard, toolLoopAbortCallIDs)
	}

	var prepareStep func(*sdk.GenerateParams) *sdk.GenerateParams
	if readMediaState != nil {
		prepareStep = readMediaState.prepareStep
	}

	prepareStep = a.wrapPrepareStepWithModelHook(genCtx, cfg, prepareStep)
	cfg, err := a.applyBeforeModelCallHook(genCtx, cfg, 0)
	if err != nil {
		return nil, err
	}
	if a == nil || a.contextViewApplier == nil {
		cfg = cfg.RefreshContextFrag()
	}
	opts := a.buildGenerateOptions(genCtx, cfg, sdkTools, approvalTools, prepareStep)
	opts = append(opts, a.onStepOption(genCtx, cfg, func(step *sdk.StepResult) *sdk.GenerateParams {
		if cfg.LoopDetection.Enabled {
			if toolLoopAbortCallIDs.Any() {
				loopAbort.Set(ErrToolLoopDetected)
				cancel(ErrToolLoopDetected)
				return nil
			}
			if textLoopGuard != nil && isNonEmptyString(step.Text) {
				result := textLoopGuard.Inspect(step.Text)
				if result.Abort {
					loopAbort.Set(ErrTextLoopDetected)
					cancel(ErrTextLoopDetected)
					return nil
				}
			}
		}
		return nil
	}))

	genResult, err := a.client.GenerateTextResult(genCtx, opts...)
	if err != nil {
		if loopErr := detectGenerateLoopAbort(genCtx, err); loopErr != nil {
			return nil, loopErr
		}
		return nil, fmt.Errorf("generate: %w", err)
	}
	if loopErr := loopAbort.Err(); loopErr != nil {
		return nil, loopErr
	}
	if len(genResult.Steps) > 0 {
		genResult.Usage = aggregateStepUsage(genResult.Steps)
	}

	// Drain collected tool-emitted side effects into the result.
	collectedEvents := collected.CloseAndSnapshot()
	var attachments []FileAttachment
	var reactions []ReactionItem
	var speeches []SpeechItem
	for _, evt := range collectedEvents {
		switch evt.Type {
		case tools.StreamEventAttachment:
			for _, a := range evt.Attachments {
				attachments = append(attachments, fileAttachmentFromToolAttachment(a))
			}
		case tools.StreamEventReaction:
			for _, r := range evt.Reactions {
				reactions = append(reactions, ReactionItem{Emoji: r.Emoji})
			}
		case tools.StreamEventSpeech:
			for _, s := range evt.Speeches {
				speeches = append(speeches, SpeechItem{Text: s.Text})
			}
		}
	}

	finalMessages := genResult.Messages
	if readMediaState != nil {
		finalMessages = readMediaState.mergeMessages(genResult.Steps, finalMessages)
	}
	finalMessages = toolExecutionMetadata.annotate(finalMessages)
	a.observePrefixCache(cfg)
	return &GenerateResult{
		Messages:    finalMessages,
		Text:        genResult.Text,
		Attachments: attachments,
		Reactions:   reactions,
		Speeches:    speeches,
		Usage:       &genResult.Usage,
	}, nil
}

func (a *Agent) buildGenerateOptions(ctx context.Context, cfg RunConfig, tools []sdk.Tool, approvalTools []sdk.Tool, prepareStep func(*sdk.GenerateParams) *sdk.GenerateParams) []sdk.GenerateOption {
	cfg.ContextMutations.SetModelInfo(modelID(cfg.Model), models.ResolveClientType(cfg.Model))
	loopReselectMode := a.LoopReselectMode()
	if loopReselectMode == LoopReselectOff {
		cfg.ContextStepReselector = nil
	}
	switch {
	case cfg.ContextStepReselector == nil:
		cfg.ContextMutations.SetLoopSelectionMode(contextfrag.LoopSelectionLegacyPrune)
	case loopReselectMode == LoopReselectShadow:
		cfg.ContextMutations.SetLoopSelectionMode(contextfrag.LoopSelectionSuffixOnlyShadow)
	default:
		cfg.ContextMutations.SetLoopSelectionMode(contextfrag.LoopSelectionSuffixOnly)
	}
	// The prefix-cache comparator hashes the pre-decoration payload, not the
	// cache_control-decorated one: Anthropic's message-level breakpoint moves
	// forward every turn as history grows, so hashing after decoration would
	// make byte-identical cached content serialize differently turn to turn
	// and defeat growth-hit detection below (recordPrefixCacheBoundary /
	// observePrefixCache).
	rawPrefixCount := clampStableMessageCount(cfg.ContextCachePlan.StableMessageCount, len(cfg.Messages))
	a.recordPrefixCacheBoundary(cfg, cfg.System, cfg.Messages, tools, rawPrefixCount)
	plan := contextCachePlanWithComparatorPrefix(cfg.ContextCachePlan, cfg.System, cfg.Messages, tools, rawPrefixCount)

	system, messages, decoratedTools, systemPrepended, actualStableCount := models.ApplyPromptCacheWithPlan(
		cfg.Model, cfg.PromptCacheTTL, plan, cfg.System, cfg.Messages, tools,
	)
	// Honesty: reflect where the breakpoint actually landed (models.ApplyPromptCacheWithPlan
	// may have fallen back to an earlier message, or found none) rather than the
	// upstream claim.
	plan.StableMessageCount = actualStableCount
	tools = decoratedTools
	plan.DecoratedProviderPrefixHash = decoratedProviderPrefixHash(system, messages, tools, actualStableCount, systemPrepended)
	initialProviderMessageCount := len(messages)
	publishContextCachePlan(cfg, plan)
	finalHash, _ := contextfrag.ProviderPayloadHashAndBytes(system, messages, tools)
	cfg.ContextMutations.SetFinalInputHash(finalHash)
	// The twilight SDK only invokes PrepareStep for model steps > 0 (step 0
	// gets this decorated payload as-is), so step 0's own snapshot has to be
	// recorded here rather than from within the PrepareStep closure below —
	// otherwise step 0 has no snapshot and every later step's PrepareStep
	// call is misattributed one model call ahead of the usage it pairs with.
	cfg.ContextMutations.AppendStepSnapshot(contextfrag.StepSnapshot{StepIndex: 0, PostPrepareInputHash: finalHash})
	if cfg.ForkContext != nil {
		_ = cfg.ForkContext.Store(messages)
	}
	if cfg.BackgroundManager != nil {
		basePrepare := prepareStep
		prepareStep = func(p *sdk.GenerateParams) *sdk.GenerateParams {
			p.Messages = removeBackgroundSummaryMessages(p.Messages, initialProviderMessageCount)
			if basePrepare != nil {
				if override := basePrepare(p); override != nil {
					p = override
				}
			}
			if summary := strings.TrimSpace(cfg.BackgroundManager.RunningTasksSummary(cfg.Identity.BotID, cfg.Identity.SessionID)); summary != "" {
				cfg.ContextMutations.Record(contextfrag.MutationBackgroundSummary, fmt.Sprintf("bytes=%d", len(summary)))
				p.Messages = append(p.Messages, backgroundSummaryMessage(summary))
			}
			return p
		}
	}
	opts := []sdk.GenerateOption{
		sdk.WithModel(cfg.Model),
		sdk.WithMessages(messages),
		sdk.WithSystem(system),
		sdk.WithMaxSteps(-1),
	}
	if len(tools) > 0 && cfg.SupportsToolCall {
		opts = append(opts, sdk.WithTools(tools))
	}
	approvalHandler := cfg.ToolApprovalHandler
	if a != nil && a.hookService != nil {
		approvalHandler = a.wrapApprovalHandlerWithHooks(cfg, approvalTools, approvalHandler)
	}
	if approvalHandler != nil {
		opts = append(opts, sdk.WithApprovalHandler(approvalHandler))
	}

	prepareIndex := 0
	basePrepare := prepareStep
	stepPrepare := func(p *sdk.GenerateParams) *sdk.GenerateParams {
		if basePrepare != nil {
			if override := basePrepare(p); override != nil {
				p = override
			}
		}
		if p == nil {
			return nil
		}
		defer func() { prepareIndex++ }()
		if cfg.ContextStepReselector != nil {
			beforeMessages := append([]sdk.Message(nil), p.Messages...)
			selection := cfg.ContextStepReselector(ctx, ContextStepSelectionInput{
				Scope:                 cfg.ContextScope,
				InitialMessageCount:   initialProviderMessageCount,
				Messages:              p.Messages,
				BudgetMaxTokens:       remainingStepBudget(stepReselectionAllowance(cfg), p, initialProviderMessageCount),
				RecentProtectTokens:   cfg.ContextRecentProtectTokens,
				KeepRecentToolResults: stepReselectKeepRecentToolResults,
				MinMessages:           stepReselectMinMessages,
			})
			// PrepareStep call k feeds model call k+1's input (see the step-0
			// snapshot recorded in buildGenerateOptions above).
			snapshot := contextfrag.StepSnapshot{StepIndex: prepareIndex + 1}
			switch {
			case loopReselectMode == LoopReselectShadow:
				// Shadow never applies the selection: the snapshot carries the
				// reselector's would-be verdict and the provider input stays
				// append-only.
				snapshot.Dropped = selection.Dropped
				snapshot.Truncated = selection.Truncated
				snapshot.DropReasons = copyDropReasons(selection.DropReasons)
			case selection.Messages != nil && stepSelectionPreservesPrefix(beforeMessages, selection.Messages, initialProviderMessageCount):
				p.Messages = selection.Messages
				snapshot.ReselectionApplied = true
				snapshot.Dropped = selection.Dropped
				snapshot.Truncated = selection.Truncated
				snapshot.DropReasons = copyDropReasons(selection.DropReasons)
				if selection.Dropped > 0 || selection.Truncated > 0 {
					cfg.ContextMutations.Record(contextfrag.MutationLoopStepReselection, contextStepSelectionDetail(selection))
				}
			}
			recordPreparedProviderInputHash(cfg.ContextMutations, p, snapshot)
			return p
		}
		recordPreparedProviderInputHash(cfg.ContextMutations, p, contextfrag.StepSnapshot{StepIndex: prepareIndex + 1})
		return p
	}
	opts = append(opts, sdk.WithPrepareStep(wrapPrepareStepWithForkSnapshot(stepPrepare, cfg.ForkContext)))

	opts = append(opts, models.BuildReasoningOptions(models.SDKModelConfig{
		ClientType:            models.ResolveClientType(cfg.Model),
		ChatCompletionsCompat: cfg.ChatCompletionsCompat,
		ReasoningConfig: &models.ReasoningConfig{
			Active:    cfg.ReasoningActive,
			Disabled:  cfg.ReasoningDisabled,
			Adaptive:  cfg.ReasoningAdaptive,
			Effort:    cfg.ReasoningEffort,
			OffEffort: cfg.ReasoningOffEffort,
		},
	})...)
	return opts
}

// recordPrefixCacheBoundary hands this turn's raw (pre-decoration)
// stable-prefix message count to the mutation ledger, snapshots the
// previous-turn tracker entry this run peeked (so observePrefixCache later
// compares against what THIS run actually saw rather than racing a
// concurrent run's write — see PeekedPrevCacheEntry), and, when the previous
// turn's stable prefix is still within this turn's stable range, re-hashes
// that previous boundary against this turn's raw messages/system/tools.
// observePrefixCache compares that boundary hash against the peeked entry's
// hash to recognize prefix-preserving growth (the breakpoint moved forward
// but the previously-cached bytes are unchanged) as a cache hit rather than
// misclassifying it as prefix_changed. Using the raw, undecorated payload
// for both hashes keeps the comparison decoration-agnostic: Anthropic's
// cache_control breakpoint moving off a message between turns must not
// change that message's contribution to the hash.
func (a *Agent) recordPrefixCacheBoundary(cfg RunConfig, system string, messages []sdk.Message, tools []sdk.Tool, prefixCount int) {
	if a == nil || a.prefixCache == nil || cfg.ContextMutations == nil || cfg.Identity.IsSubagent {
		return
	}
	key := prefixCacheSessionKey(cfg.Identity)
	if key == "" {
		return
	}
	cfg.ContextMutations.SetComparatorPrefixMessageCount(prefixCount)
	prev, ok := a.prefixCache.peek(key)
	cfg.ContextMutations.SetPeekedPrevCacheEntry(contextfrag.PeekedPrevCacheEntry{
		Found:       ok,
		Hash:        prev.hash,
		Model:       prev.model,
		StableCount: prev.stableCount,
		At:          prev.at,
	})
	// compareCachePrefix's equal-prefix branch (prev.stableCount == nowCount)
	// never reads the boundary hash — only the growth branch
	// (prev.stableCount < nowCount) does — so skip the wasted hash when the
	// stable count hasn't grown.
	if !ok || prev.stableCount >= prefixCount {
		return
	}
	hash, _ := contextfrag.ProviderPayloadHashAndBytes(system, messages[:prev.stableCount], tools)
	cfg.ContextMutations.SetPrevBoundaryHash(hash)
}

// contextCachePlanWithComparatorPrefix computes the plan's cache-comparator
// hash from the raw (pre-decoration) system/messages/tools sliced to
// prefixCount, so the stored hash never depends on where (or whether) a
// vendor-specific cache_control breakpoint landed.
func contextCachePlanWithComparatorPrefix(plan contextfrag.CachePlan, system string, messages []sdk.Message, tools []sdk.Tool, prefixCount int) contextfrag.CachePlan {
	prefixMessages := append([]sdk.Message(nil), messages[:prefixCount]...)
	hash, bytes := contextfrag.ProviderPayloadHashAndBytes(system, prefixMessages, tools)
	plan.CacheComparatorPrefixHash = hash
	plan.CacheComparatorPrefixBytes = bytes
	plan.CacheComparatorPrefixTokenEstimate = tokenEstimateFromBytes(bytes)
	return plan
}

// decoratedProviderPrefixHash hashes the POST-decoration provider payload
// prefix actually sent to the vendor: system/messages/tools after
// models.ApplyPromptCacheWithPlan has run, sliced to the span that call
// reports as covered by the applied cache breakpoint (actualStableCount),
// plus the prepended system message when the vendor promoted one
// (systemPrepended), unlike the pre-decoration comparator hash above.
func decoratedProviderPrefixHash(system string, messages []sdk.Message, tools []sdk.Tool, actualStableCount int, systemPrepended bool) string {
	count := actualStableCount
	if systemPrepended {
		count++
	}
	if count < 0 {
		count = 0
	}
	if count > len(messages) {
		count = len(messages)
	}
	prefixMessages := append([]sdk.Message(nil), messages[:count]...)
	hash, _ := contextfrag.ProviderPayloadHashAndBytes(system, prefixMessages, tools)
	return hash
}

// clampStableMessageCount bounds a stable-message count reported by context
// selection to the actual message slice length, guarding against off-range
// values (e.g. selection racing with a concurrent trim).
func clampStableMessageCount(count, total int) int {
	if count < 0 {
		return 0
	}
	if count > total {
		return total
	}
	return count
}

func tokenEstimateFromBytes(bytes int) int {
	return contextfrag.TokensFromBytes(bytes)
}

func recordPreparedProviderInputHash(ledger *contextfrag.MutationLedger, params *sdk.GenerateParams, snapshot contextfrag.StepSnapshot) {
	if params == nil {
		return
	}
	hash, _ := contextfrag.ProviderPayloadHashAndBytes(params.System, params.Messages, params.Tools)
	ledger.SetFinalInputHash(hash)
	snapshot.PostPrepareInputHash = hash
	ledger.AppendStepSnapshot(snapshot)
}

func copyDropReasons(reasons map[string]int) map[string]int {
	if len(reasons) == 0 {
		return nil
	}
	out := make(map[string]int, len(reasons))
	for reason, count := range reasons {
		out[reason] = count
	}
	return out
}

func stepReselectionAllowance(cfg RunConfig) int {
	if plan := cfg.ContextManifest.BudgetPlan; plan != nil {
		allowance := plan.Window - plan.OutputReserve
		if allowance < 1 {
			return 1
		}
		return allowance
	}
	return cfg.EffectiveHistoryBudgetTokens()
}

func remainingStepBudget(maxTokens int, params *sdk.GenerateParams, prefixCount int) int {
	if maxTokens <= 0 || params == nil {
		return 0
	}
	if prefixCount < 0 {
		prefixCount = 0
	}
	if prefixCount > len(params.Messages) {
		prefixCount = len(params.Messages)
	}
	prefixMessages := append([]sdk.Message(nil), params.Messages[:prefixCount]...)
	_, bytes := contextfrag.ProviderPayloadHashAndBytes(params.System, prefixMessages, params.Tools)
	remaining := maxTokens - tokenEstimateFromBytes(bytes)
	if remaining < 1 {
		return 1
	}
	return remaining
}

func stepSelectionPreservesPrefix(before, after []sdk.Message, count int) bool {
	if count < 0 || count > len(before) || count > len(after) {
		return false
	}
	return reflect.DeepEqual(before[:count], after[:count])
}

func contextStepSelectionDetail(selection ContextStepSelectionResult) string {
	if len(selection.DropReasons) == 0 {
		return fmt.Sprintf("dropped=%d truncated=%d", selection.Dropped, selection.Truncated)
	}
	reasons := make([]string, 0, len(selection.DropReasons))
	for reason := range selection.DropReasons {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%s:%d", reason, selection.DropReasons[reason]))
	}
	return fmt.Sprintf("dropped=%d truncated=%d reasons=%s", selection.Dropped, selection.Truncated, strings.Join(parts, ","))
}

func publishContextCachePlan(cfg RunConfig, plan contextfrag.CachePlan) {
	if cfg.ContextManifest.CachePlan != nil {
		*cfg.ContextManifest.CachePlan = plan
	} else {
		cfg.ContextManifest.CachePlan = &plan
	}
	if cfg.ContextLifecycle != nil {
		cfg.ContextLifecycle.SetManifest(cfg.ContextManifest)
	}
}

// onStepOption builds the sdk.WithOnStep option shared by every model-call
// site (initial stream, initial generate, and each mid-stream retry
// attempt), owning a fresh per-attempt modelStepIndex counter so cache-usage
// recording and after-model-call hooks run identically everywhere — retry
// attempts previously ran neither, because runMidStreamRetry built its
// stream from buildGenerateOptions alone. after, when non-nil, runs once the
// shared bookkeeping is done and may itself return a *sdk.GenerateParams
// override (used by runGenerate for loop-detection cancellation).
func (a *Agent) onStepOption(ctx context.Context, cfg RunConfig, after func(*sdk.StepResult) *sdk.GenerateParams) sdk.GenerateOption {
	modelStepIndex := 0
	return sdk.WithOnStep(func(step *sdk.StepResult) *sdk.GenerateParams {
		recordContextCacheUsage(cfg.ContextMutations, modelStepIndex, step)
		a.runAfterModelCallHook(ctx, cfg, step, modelStepIndex)
		modelStepIndex++
		if after != nil {
			return after(step)
		}
		return nil
	})
}

func recordContextCacheUsage(ledger *contextfrag.MutationLedger, stepIndex int, step *sdk.StepResult) {
	if ledger == nil || step == nil {
		return
	}
	detail := step.Usage.InputTokenDetails
	if step.Usage.CachedInputTokens == 0 &&
		detail.NoCacheTokens == 0 &&
		detail.CacheReadTokens == 0 &&
		detail.CacheWriteTokens == 0 &&
		detail.CacheWrite5mTokens == 0 &&
		detail.CacheWrite1hTokens == 0 {
		return
	}
	ledger.RecordCacheUsage(contextfrag.CacheUsageRecord{
		StepIndex:          stepIndex,
		NoCacheTokens:      detail.NoCacheTokens,
		CacheReadTokens:    detail.CacheReadTokens,
		CacheWriteTokens:   detail.CacheWriteTokens,
		CacheWrite5mTokens: detail.CacheWrite5mTokens,
		CacheWrite1hTokens: detail.CacheWrite1hTokens,
	})
}

func aggregateStepUsage(steps []sdk.StepResult) sdk.Usage {
	var total sdk.Usage
	for _, step := range steps {
		total.InputTokens += step.Usage.InputTokens
		total.OutputTokens += step.Usage.OutputTokens
		total.TotalTokens += step.Usage.TotalTokens
		total.ReasoningTokens += step.Usage.ReasoningTokens
		total.CachedInputTokens += step.Usage.CachedInputTokens
		total.InputTokenDetails.NoCacheTokens += step.Usage.InputTokenDetails.NoCacheTokens
		total.InputTokenDetails.CacheReadTokens += step.Usage.InputTokenDetails.CacheReadTokens
		total.InputTokenDetails.CacheWriteTokens += step.Usage.InputTokenDetails.CacheWriteTokens
		total.InputTokenDetails.CacheWrite5mTokens += step.Usage.InputTokenDetails.CacheWrite5mTokens
		total.InputTokenDetails.CacheWrite1hTokens += step.Usage.InputTokenDetails.CacheWrite1hTokens
		total.OutputTokenDetails.TextTokens += step.Usage.OutputTokenDetails.TextTokens
		total.OutputTokenDetails.ReasoningTokens += step.Usage.OutputTokenDetails.ReasoningTokens
	}
	return total
}

// assembleTools collects tools from all registered ToolProviders, along with
// the group-level usage guidance contributed by providers that also implement
// tools.ToolUsage. Usage guidance is gathered only from providers that actually
// returned tools for this session, so it stays in lockstep with registration
// (see tools.ToolUsage). emitter is injected into the session context so that
// tools targeting the current conversation can push side-effect events
// (attachments, reactions, speech) directly into the agent stream.
func (a *Agent) assembleTools(ctx context.Context, cfg RunConfig, emitter tools.StreamEmitter, liveStream bool) ([]sdk.Tool, string, []contextfrag.ToolDefAccounting, error) {
	if len(a.toolProviders) == 0 {
		return nil, "", nil, nil
	}
	skillsMap := make(map[string]tools.SkillDetail, len(cfg.Skills))
	for _, s := range cfg.Skills {
		skillsMap[s.Name] = tools.SkillDetail{
			Description: s.Description,
			Content:     s.Content,
			Path:        s.Path,
		}
	}
	session := tools.SessionContext{
		BotID:                     cfg.Identity.BotID,
		ChatID:                    cfg.Identity.ChatID,
		SessionID:                 cfg.Identity.SessionID,
		SessionType:               cfg.SessionType,
		UserID:                    cfg.Identity.UserID,
		ChannelIdentityID:         cfg.Identity.ChannelIdentityID,
		SessionToken:              cfg.Identity.SessionToken,
		WorkspaceTargetID:         cfg.Identity.WorkspaceTargetID,
		WorkspaceTargetKind:       cfg.Identity.WorkspaceTargetKind,
		WorkspaceTargetName:       cfg.Identity.WorkspaceTargetName,
		CurrentPlatform:           cfg.Identity.CurrentPlatform,
		ReplyTarget:               cfg.Identity.ReplyTarget,
		ConversationType:          cfg.Identity.ConversationType,
		CanRequestUserInput:       cfg.CanRequestUserInput,
		SupportsImageInput:        cfg.SupportsImageInput,
		IsSubagent:                cfg.Identity.IsSubagent,
		CurrentModelUUID:          cfg.CurrentModelUUID,
		CurrentModelID:            cfg.CurrentModelID,
		CurrentModelProvider:      cfg.CurrentModelProvider,
		ForkContext:               cfg.ForkContext,
		Skills:                    skillsMap,
		TimezoneLocation:          cfg.Identity.TimezoneLocation,
		Emitter:                   emitter,
		LiveStream:                liveStream,
		ContextBudgetMaxTokens:    cfg.ContextBudgetMaxTokens,
		ContextToolExchangePolicy: cfg.ContextToolExchangePolicy,
	}

	var allTools []sdk.Tool
	var toolDefs []contextfrag.ToolDefAccounting
	type usageRegistration struct {
		provider tools.ToolUsage
	}
	var usageRegistrations []usageRegistration
	var usageSections []string
	for _, provider := range a.toolProviders {
		providerTools, err := provider.Tools(ctx, session)
		if err != nil {
			a.logger.Warn("tool provider failed", slog.Any("error", err))
			continue
		}
		if session.IsSubagent {
			providerTools = tools.FilterSubagentTools(providerTools)
		}
		if len(providerTools) == 0 {
			continue
		}
		label := "native"
		if labeler, ok := provider.(tools.ProviderLabeler); ok {
			label = labeler.ProviderLabel()
		}
		for _, tool := range providerTools {
			toolDefs = append(toolDefs, contextfrag.ToolDefAccountingFor(label, tool))
		}
		allTools = append(allTools, providerTools...)
		// Collect group-level usage guidance only from providers that actually
		// contributed tools this session, so guidance and registration share
		// one gating decision and cannot drift apart.
		if usageProvider, ok := provider.(tools.ToolUsage); ok {
			usageRegistrations = append(usageRegistrations, usageRegistration{provider: usageProvider})
		}
	}
	if cfg.ToolApprovalHandler != nil || a.hookService != nil {
		allTools = markApprovalTools(allTools)
	}
	availableTools := tools.NewAvailableTools(allTools)
	for _, registration := range usageRegistrations {
		if text := strings.TrimSpace(registration.provider.Usage(ctx, session, availableTools)); text != "" {
			usageSections = append(usageSections, text)
		}
	}
	usage := ""
	if len(usageSections) > 0 {
		usage = "## Tool usage\n\n" + strings.Join(usageSections, "\n\n")
	}
	return allTools, usage, toolDefs, nil
}

func appendToolUsageToSystem(system, toolUsage string) string {
	system = strings.TrimSpace(system)
	toolUsage = strings.TrimSpace(toolUsage)
	if toolUsage == "" {
		return system
	}
	if system == "" {
		return toolUsage
	}
	if idx := strings.Index(system, contextfrag.WorkspaceInstructionAnchor); idx >= 0 {
		return strings.TrimSpace(system[:idx]) + "\n\n" + toolUsage + "\n" + system[idx:]
	}
	return strings.TrimSpace(system + "\n\n" + toolUsage)
}

func markApprovalTools(sdkTools []sdk.Tool) []sdk.Tool {
	for i := range sdkTools {
		switch sdkTools[i].Name {
		case tools.ToolRead().String(), tools.ToolList().String(), tools.ToolWrite().String(), tools.ToolEdit().String(), tools.ToolApplyPatch().String(), tools.ToolExec().String():
			sdkTools[i].RequireApproval = true
		}
	}
	return sdkTools
}

func approvalShortID(metadata map[string]any) int {
	if metadata == nil {
		return 0
	}
	switch v := metadata["short_id"].(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	default:
		return 0
	}
}

func annotateDeferredApproval(messages []sdk.Message, approval sdk.ToolApprovalResult) []sdk.Message {
	if approval.ApprovalID == "" {
		return messages
	}
	toolCallID, _ := approval.Metadata["tool_call_id"].(string)
	if strings.TrimSpace(toolCallID) == "" {
		return messages
	}
	annotated := make([]sdk.Message, len(messages))
	copy(annotated, messages)
	for msgIdx := range annotated {
		if annotated[msgIdx].Role != sdk.MessageRoleAssistant {
			continue
		}
		for partIdx := range annotated[msgIdx].Content {
			call, ok := annotated[msgIdx].Content[partIdx].(sdk.ToolCallPart)
			if !ok || strings.TrimSpace(call.ToolCallID) != strings.TrimSpace(toolCallID) {
				continue
			}
			if call.ProviderMetadata == nil {
				call.ProviderMetadata = map[string]any{}
			}
			if isUserInputMetadata(approval.Metadata) {
				call.ProviderMetadata["user_input"] = map[string]any{
					"user_input_id": approval.ApprovalID,
					"short_id":      approvalShortID(approval.Metadata),
					"status":        "pending",
					"ui_payload":    approval.Metadata["ui_payload"],
				}
			} else {
				call.ProviderMetadata["approval"] = map[string]any{
					"approval_id": approval.ApprovalID,
					"short_id":    approvalShortID(approval.Metadata),
					"status":      "pending",
					"can_approve": true,
					"operation":   approval.Metadata["operation"],
				}
			}
			annotated[msgIdx].Content[partIdx] = call
			return annotated
		}
	}
	return annotated
}

func isUserInputMetadata(metadata map[string]any) bool {
	if metadata == nil {
		return false
	}
	kind, _ := metadata["kind"].(string)
	return strings.TrimSpace(kind) == userinput.DeferredKind
}

func isAskUserArgumentParseError(message string) bool {
	return strings.Contains(message, `unmarshal tool call arguments for "`+tools.ToolAskUser().String()+`"`)
}

// toolStreamEventToAgentEvent converts a tool-layer ToolStreamEvent into an
// agent-layer StreamEvent suitable for the output channel.
func toolStreamEventToAgentEvent(evt tools.ToolStreamEvent) StreamEvent {
	switch evt.Type {
	case tools.StreamEventAttachment:
		atts := make([]FileAttachment, 0, len(evt.Attachments))
		for _, a := range evt.Attachments {
			atts = append(atts, fileAttachmentFromToolAttachment(a))
		}
		return StreamEvent{Type: EventAttachment, ToolCallID: evt.ToolCallID, Attachments: atts}
	case tools.StreamEventReaction:
		rs := make([]ReactionItem, 0, len(evt.Reactions))
		for _, r := range evt.Reactions {
			rs = append(rs, ReactionItem{Emoji: r.Emoji})
		}
		return StreamEvent{Type: EventReaction, Reactions: rs}
	case tools.StreamEventSpeech:
		ss := make([]SpeechItem, 0, len(evt.Speeches))
		for _, s := range evt.Speeches {
			ss = append(ss, SpeechItem{Text: s.Text})
		}
		return StreamEvent{Type: EventSpeech, Speeches: ss}
	case tools.StreamEventSpawnHeartbeat:
		return StreamEvent{Type: EventProgress, ProgressStatus: "spawn_running"}
	default:
		return StreamEvent{}
	}
}

func backgroundSummaryMessage(summary string) sdk.Message {
	return sdk.UserMessage(contextfrag.BackgroundSummaryMessagePrefix + summary)
}

// removeBackgroundSummaryMessages strips summary carrier messages appended by
// earlier steps so each step rebuilds exactly one fresh summary. keepPrefix
// guards the compiled initial context: only loop-appended messages match.
func removeBackgroundSummaryMessages(messages []sdk.Message, keepPrefix int) []sdk.Message {
	if keepPrefix < 0 {
		keepPrefix = 0
	}
	for i := keepPrefix; i < len(messages); i++ {
		if !contextfrag.IsBackgroundSummaryCarrier(messages[i]) {
			continue
		}
		out := make([]sdk.Message, 0, len(messages)-1)
		out = append(out, messages[:i]...)
		for _, msg := range messages[i+1:] {
			if !contextfrag.IsBackgroundSummaryCarrier(msg) {
				out = append(out, msg)
			}
		}
		return out
	}
	return messages
}

// injectedMessageText prefers the headerified rendering; when it falls back to
// raw text it guards the reserved background-summary prefix so an injected
// user message can never masquerade as a summary carrier, which the next
// step's rebuild would silently remove.
func injectedMessageText(injected InjectMessage) string {
	if text := strings.TrimSpace(injected.HeaderifiedText); text != "" {
		return text
	}
	text := strings.TrimSpace(injected.Text)
	if strings.HasPrefix(text, contextfrag.BackgroundSummaryMessagePrefix) {
		return "[injected]\n" + text
	}
	return text
}

func wrapToolsWithLoopGuard(tools []sdk.Tool, guard *ToolLoopGuard, abortCallIDs *toolAbortRegistry) []sdk.Tool {
	wrapped := make([]sdk.Tool, len(tools))
	for i, tool := range tools {
		originalExecute := tool.Execute
		toolName := tool.Name
		wrapped[i] = tool
		wrapped[i].Execute = func(ctx *sdk.ToolExecContext, input any) (any, error) {
			warn, abort := guard.Guard(toolName, input)
			if abort {
				abortCallIDs.Add(ctx.ToolCallID)
				return map[string]any{
					"isError": true,
					"content": []map[string]any{{
						"type": "text",
						"text": ToolLoopDetectedAbortMessage,
					}},
				}, ErrToolLoopDetected
			}
			if warn {
				return map[string]any{
					ToolLoopWarningKey: true,
					"content": []map[string]any{{
						"type": "text",
						"text": ToolLoopWarningText,
					}},
				}, nil
			}
			return originalExecute(ctx, input)
		}
	}
	return wrapped
}

const (
	// stepReselectKeepRecentToolResults hints the step reselector to keep the
	// most recent tool-call cycles intact when it considers mid-run drops.
	stepReselectKeepRecentToolResults = 4
	// stepReselectMinMessages is the minimum provider message count before the
	// step reselector considers dropping anything at all.
	stepReselectMinMessages = 20
)

func wrapPrepareStepWithForkSnapshot(
	prepareStep func(*sdk.GenerateParams) *sdk.GenerateParams,
	forkContext *tools.MessageSnapshot,
) func(*sdk.GenerateParams) *sdk.GenerateParams {
	if forkContext == nil {
		return prepareStep
	}
	return func(p *sdk.GenerateParams) *sdk.GenerateParams {
		if prepareStep != nil {
			if override := prepareStep(p); override != nil {
				p = override
			}
		}
		_ = forkContext.Store(p.Messages)
		return p
	}
}

// runMidStreamRetry attempts to continue the agent stream after a retryable
// mid-stream error. It re-invokes StreamText with the accumulated messages
// and drains the new stream into the same output channel.
//
// sendCtx is used for sendEvent so consumer disconnect (parent ctx) still
// controls channel back-pressure; streamCtx is passed to the SDK for the same
// cancellation semantics as the main stream (including loop-detect cancel).
func (a *Agent) runMidStreamRetry(
	sendCtx context.Context,
	streamCtx context.Context,
	cancel context.CancelCauseFunc,
	toolLoopAbortCallIDs *toolAbortRegistry,
	ch chan<- StreamEvent,
	cfg RunConfig,
	sdkTools []sdk.Tool,
	approvalTools []sdk.Tool,
	prepareStep func(*sdk.GenerateParams) *sdk.GenerateParams,
	prevResult *sdk.StreamResult,
	stepNumber int,
	errMsg string,
	allText *strings.Builder,
	textLoopProbeBuffer *TextLoopProbeBuffer,
) (*sdk.StreamResult, bool) {
	// Drain the previous stream before reading prevResult.Messages.
	// This avoids racing with the SDK's final StreamResult write.
	if prevResult.Stream != nil {
		for range prevResult.Stream {
		}
	}

	retryCfg := DefaultRetryConfig()
	for attempt := 0; attempt < retryCfg.MaxAttempts; attempt++ {
		a.logger.Warn("mid-stream error, retrying",
			slog.Int("step", stepNumber),
			slog.Int("attempt", attempt+1),
			slog.Int("max_attempts", retryCfg.MaxAttempts),
			slog.String("error", errMsg),
		)
		if !sendEvent(sendCtx, ch, StreamEvent{
			Type:       EventRetry,
			Attempt:    attempt + 1,
			MaxAttempt: retryCfg.MaxAttempts,
			RetryError: errMsg,
		}) {
			return prevResult, true
		}

		delay := retryDelay(attempt, retryCfg)
		if delay > 0 {
			if err := sleepWithContext(streamCtx, delay); err != nil {
				return prevResult, true // aborted
			}
		}

		// Re-invoke StreamText with the original conversation plus the output
		// accumulated before the failure. Use buildGenerateOptions so retry
		// benefits from mid-task pruning, media resolution, and other
		// prepare-step logic — same as initial stream.
		retryCfgCopy := prepareMidStreamRetryConfig(cfg, prevResult.Messages, errMsg)
		if a == nil || a.contextViewApplier == nil {
			retryCfgCopy = retryCfgCopy.RefreshContextFrag()
		}
		retryOpts := a.buildGenerateOptions(streamCtx, retryCfgCopy, sdkTools, approvalTools, prepareStep)
		retryOpts = append(retryOpts, a.onStepOption(streamCtx, retryCfgCopy, nil))

		retryResult, retryErr := a.client.StreamText(streamCtx, retryOpts...)
		if retryErr != nil {
			a.logger.Warn("mid-stream retry failed to start",
				slog.Int("attempt", attempt+1),
				slog.String("error", retryErr.Error()),
			)
			// Update errMsg so the next retry event shows the latest error.
			errMsg = retryErr.Error()
			continue
		}

		// Drain the retry stream into the main event loop
		aborted := false
		for retryPart := range retryResult.Stream {
			if streamCtx.Err() != nil {
				aborted = true
				break
			}
			switch rp := retryPart.(type) {
			case *sdk.TextStartPart:
				if !sendEvent(sendCtx, ch, StreamEvent{Type: EventTextStart}) {
					aborted = true
				}
			case *sdk.TextDeltaPart:
				if rp.Text != "" {
					if textLoopProbeBuffer != nil {
						textLoopProbeBuffer.Push(rp.Text)
					}
					if !sendEvent(sendCtx, ch, StreamEvent{Type: EventTextDelta, Delta: rp.Text}) {
						aborted = true
					}
					allText.WriteString(rp.Text)
				}
			case *sdk.TextEndPart:
				if textLoopProbeBuffer != nil {
					textLoopProbeBuffer.Flush()
				}
				stepNumber++
				if !sendEvent(sendCtx, ch, StreamEvent{Type: EventTextEnd}) {
					aborted = true
				}
			case *sdk.ToolInputStartPart:
				// See ToolInputStartPart note above: emit a lightweight
				// tool_call_input_start so the Web UI shows the tool block while
				// arguments stream; StreamToolCallPart backfills the Input.
				if textLoopProbeBuffer != nil {
					textLoopProbeBuffer.Flush()
				}
				if !sendEvent(sendCtx, ch, StreamEvent{
					Type:       EventToolCallInputStart,
					ToolName:   rp.ToolName,
					ToolCallID: rp.ID,
				}) {
					aborted = true
				}
			case *sdk.StreamToolCallPart:
				if textLoopProbeBuffer != nil {
					textLoopProbeBuffer.Flush()
				}
				if !sendEvent(sendCtx, ch, StreamEvent{
					Type:       EventToolCallStart,
					ToolName:   rp.ToolName,
					ToolCallID: rp.ToolCallID,
					Input:      rp.Input,
				}) {
					aborted = true
				}
			case *sdk.StreamToolResultPart:
				shouldAbort := toolLoopAbortCallIDs.Take(rp.ToolCallID)
				stepNumber++
				if !sendEvent(sendCtx, ch, StreamEvent{
					Type:       EventToolCallEnd,
					ToolName:   rp.ToolName,
					ToolCallID: rp.ToolCallID,
					Input:      rp.Input,
					Result:     rp.Output,
				}) || !sendEvent(sendCtx, ch, StreamEvent{
					Type:           EventProgress,
					StepNumber:     stepNumber,
					ToolName:       rp.ToolName,
					ProgressStatus: "tool_result",
				}) {
					aborted = true
				}
				if shouldAbort {
					a.logger.Warn("tool loop abort triggered", slog.String("tool_call_id", rp.ToolCallID))
					cancel(ErrToolLoopDetected)
					aborted = true
				}
			case *sdk.StreamToolErrorPart:
				tookLoopAbort := toolLoopAbortCallIDs.Take(rp.ToolCallID)
				shouldAbort := errors.Is(rp.Error, ErrToolLoopDetected) || tookLoopAbort
				if !sendEvent(sendCtx, ch, StreamEvent{
					Type:       EventToolCallEnd,
					ToolName:   rp.ToolName,
					ToolCallID: rp.ToolCallID,
					Error:      rp.Error.Error(),
				}) {
					aborted = true
				}
				if shouldAbort {
					a.logger.Warn("tool loop abort triggered", slog.String("tool_call_id", rp.ToolCallID))
					cancel(ErrToolLoopDetected)
					aborted = true
				}
			case *sdk.ErrorPart:
				errMsg := rp.Error.Error()
				if isAskUserArgumentParseError(errMsg) {
					continue
				}
				sendEvent(sendCtx, ch, StreamEvent{Type: EventError, Error: errMsg})
				aborted = true
			case *sdk.AbortPart:
				aborted = true
			case *sdk.FinishPart:
				// handled after loop
			}
			if aborted {
				break
			}
		}
		if aborted {
			for range retryResult.Stream {
			}
		}
		// Merge prev messages into retryResult so the caller sees the full
		// accumulated history (initial run + retry continuation). The SDK's
		// StreamResult.Messages only contains messages produced within that
		// StreamText call, so without this merge the original steps before
		// the mid-stream error would be lost when the retry result becomes
		// the new streamResult.
		if len(prevResult.Messages) > 0 {
			merged := make([]sdk.Message, 0, len(prevResult.Messages)+len(retryResult.Messages))
			merged = append(merged, prevResult.Messages...)
			merged = append(merged, retryResult.Messages...)
			retryResult.Messages = merged
		}
		return retryResult, aborted || detectGenerateLoopAbort(streamCtx, streamCtx.Err()) != nil
	}
	// All retry attempts failed to even start a new stream — return the
	// previous (already drained) result so its accumulated messages are
	// preserved as the final partial state.
	return prevResult, true
}

// prepareMidStreamRetryConfig rebuilds the retry request as the original
// input conversation plus the output accumulated before the failure.
// StreamResult.Messages excludes the input messages, so reusing it alone
// would drop the history and the current query. The input prefix is
// preserved unchanged, which keeps the placement-derived cache plan valid.
func prepareMidStreamRetryConfig(cfg RunConfig, accumulated []sdk.Message, errMsg string) RunConfig {
	merged := make([]sdk.Message, 0, len(cfg.Messages)+len(accumulated))
	merged = append(merged, cfg.Messages...)
	merged = append(merged, accumulated...)
	cfg.Messages = merged
	attempt := cfg.ContextMutations.AdvanceAttempt()
	errorHash := sha256.Sum256([]byte(strings.TrimSpace(errMsg)))
	cfg.ContextMutations.Record(contextfrag.MutationMidStreamRetry,
		fmt.Sprintf("attempt=%d accumulated=%d error_sha256=%x", attempt, len(accumulated), errorHash))
	return cfg
}

// sleepWithContext sleeps for the given duration or returns context error.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func detectGenerateLoopAbort(ctx context.Context, err error) error {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}

	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, ErrToolLoopDetected):
		return ErrToolLoopDetected
	case errors.Is(cause, ErrTextLoopDetected):
		return ErrTextLoopDetected
	default:
		return nil
	}
}

type loopAbortState struct {
	mu  sync.Mutex
	err error
}

func newLoopAbortState() *loopAbortState {
	return &loopAbortState{}
}

func (s *loopAbortState) Set(err error) {
	if s == nil || err == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *loopAbortState) Err() error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
