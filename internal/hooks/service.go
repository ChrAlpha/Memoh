package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/memohai/memoh/internal/prune"
	"github.com/memohai/memoh/internal/workspace/bridge"
)

type ToolRunner interface {
	RunHookTool(ctx context.Context, toolName string, input map[string]any) (any, error)
}

type workspaceTargetIDResolver interface {
	CurrentWorkspaceTargetID(ctx context.Context, botID string) (string, error)
}

type Service struct {
	logger   *slog.Logger
	provider bridge.Provider
}

var emptyConfigFile = []byte("{\n  \"version\": 1,\n  \"enabled\": true,\n  \"hooks\": []\n}\n")

const (
	sourceKindUser = "user"
)

func NewService(log *slog.Logger, provider bridge.Provider) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		logger:   log.With(slog.String("service", "hooks")),
		provider: provider,
	}
}

func (s *Service) Load(ctx context.Context, botID string) (Config, bool, error) {
	if s == nil || s.provider == nil {
		return Config{Version: 1, Enabled: boolPtr(false)}, false, nil
	}
	client, err := s.provider.MCPClient(ctx, strings.TrimSpace(botID))
	if err != nil {
		return Config{}, false, err
	}
	rc, err := client.ReadRaw(ctx, DefaultConfigPath)
	if err != nil {
		if errors.Is(err, bridge.ErrNotFound) {
			if err := client.WriteFile(ctx, DefaultConfigPath, emptyConfigFile); err != nil {
				return Config{}, false, err
			}
			cfg, err := ParseConfig(emptyConfigFile)
			if err != nil {
				return Config{}, false, err
			}
			return cfg, true, nil
		}
		return Config{}, false, err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return Config{}, false, err
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return Config{}, true, err
	}
	return cfg, true, nil
}

func (s *Service) LoadEffective(ctx context.Context, botID string) (Config, bool, error) {
	targetID, err := s.currentWorkspaceTargetID(ctx, botID)
	if err != nil {
		return Config{}, false, err
	}
	ctx = bridge.WithWorkspaceTarget(ctx, targetID)
	userCfg, exists, err := s.Load(ctx, botID)
	if err != nil {
		return Config{}, exists, err
	}
	userCfg.applyDefaults()

	effective := Config{
		Version:  1,
		Enabled:  boolPtr(true),
		Defaults: userCfg.Defaults,
		Env:      cloneStringMap(userCfg.Env),
	}
	if userCfg.enabled() {
		for _, hook := range userCfg.Hooks {
			hook.source = hookSource{Kind: sourceKindUser}
			effective.Hooks = append(effective.Hooks, hook)
		}
	}
	return effective, exists, nil
}

func (s *Service) currentWorkspaceTargetID(ctx context.Context, botID string) (string, error) {
	if targetID := bridge.WorkspaceTargetFromContext(ctx); targetID != "" {
		return targetID, nil
	}
	if s == nil {
		return "native", nil
	}
	if resolver, ok := s.provider.(workspaceTargetIDResolver); ok {
		return resolver.CurrentWorkspaceTargetID(ctx, botID)
	}
	return "native", nil
}

func (s *Service) Run(ctx context.Context, req Request, runner ToolRunner) (Result, error) {
	req.Version = 1
	if strings.TrimSpace(req.Event) == "" {
		return Result{}, errors.New("hook event is required")
	}
	targetID, err := s.currentWorkspaceTargetID(ctx, req.BotID)
	if err != nil {
		return Result{}, err
	}
	ctx = bridge.WithWorkspaceTarget(ctx, targetID)
	cfg, _, err := s.LoadEffective(ctx, req.BotID)
	if err != nil {
		return Result{}, err
	}
	return s.RunConfig(ctx, cfg, req, runner)
}

func (s *Service) RunConfig(ctx context.Context, cfg Config, req Request, runner ToolRunner) (Result, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return Result{}, err
	}
	result := Result{
		Decision:         DecisionAllow,
		RuntimeSupported: RuntimeSupported(req.Event),
	}
	matches := cfg.Match(req)
	result.HooksMatched = len(matches)
	if len(matches) == 0 {
		return result, nil
	}
	result.Metadata = map[string]any{"hook_sources": hookSourceSummaries(matches)}
	for _, hook := range matches {
		hookReq := req
		hookReq.Version = 1
		hookReq.HookName = hook.Name
		for _, action := range hook.Actions {
			actionResult, err := s.runAction(ctx, cfg, hookReq, action, hook.source, runner)
			annotateActionResult(&actionResult, hook)
			result.ActionResults = append(result.ActionResults, actionResult)
			result.ActionsRun++
			if err != nil {
				onError := normalizeOnError(action.OnError)
				if onError == OnErrorIgnore {
					if s != nil && s.logger != nil {
						s.logger.Warn("hook action failed but was ignored",
							slog.String("event", req.Event),
							slog.String("hook", hook.Name),
							slog.String("action", action.Type),
							slog.String("error", err.Error()),
						)
					}
					continue
				}
				if onError == OnErrorBlock {
					result.Decision = DecisionDeny
					result.Reason = firstNonEmpty(actionResult.Reason, err.Error())
					return result, fmt.Errorf("%w: %s", ErrDenied, result.Reason)
				}
				return result, err
			}
			mergeDecision(&result, actionResult, cfg.Defaults.MaxOutputBytes)
			if result.Decision == DecisionDeny {
				return result, fmt.Errorf("%w: %s", ErrDenied, result.Reason)
			}
		}
	}
	return result, nil
}

func (s *Service) runAction(ctx context.Context, cfg Config, req Request, action HookAction, _ hookSource, runner ToolRunner) (ActionResult, error) {
	switch strings.TrimSpace(action.Type) {
	case ActionCommand:
		return s.runCommand(ctx, cfg, req, action)
	case ActionTool:
		return s.runTool(ctx, cfg, req, action, runner)
	case ActionMCPTool:
		return ActionResult{ActionType: action.Type, Error: ErrUnsupported.Error()}, ErrUnsupported
	default:
		err := fmt.Errorf("unsupported hook action type %q", action.Type)
		return ActionResult{ActionType: action.Type, Error: err.Error()}, err
	}
}

func (s *Service) runCommand(ctx context.Context, cfg Config, req Request, action HookAction) (ActionResult, error) {
	res := ActionResult{ActionType: ActionCommand, Name: action.Command}
	if s == nil || s.provider == nil {
		err := errors.New("hooks workspace provider is not configured")
		res.Error = err.Error()
		return res, err
	}
	timeout, err := parseTimeout(action.Timeout)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	client, err := s.provider.MCPClient(ctx, req.BotID)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	payload, err := json.Marshal(req)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	workDir := strings.TrimSpace(action.WorkDir)
	if workDir == "" {
		// Hook commands and their config belong to the provider's primary
		// workspace. req.Workspace describes the operation being inspected and
		// may point at a different runtime.
		workDir = DefaultWorkDir
	}
	envMap := cfg.Env
	env := make([]string, 0, len(envMap)+6)
	for key, value := range envMap {
		if strings.TrimSpace(key) == "" {
			continue
		}
		env = append(env, key+"="+value)
	}
	env = append(env,
		"MEMOH_HOOK_EVENT="+req.Event,
		"MEMOH_HOOK_NAME="+req.HookName,
		"MEMOH_BOT_ID="+req.BotID,
		"MEMOH_SESSION_ID="+req.SessionID,
	)
	timeoutUnits := timeout.Round(time.Second) / time.Second
	if timeoutUnits <= 0 {
		timeoutUnits = 1
	}
	if timeoutUnits > math.MaxInt32 {
		timeoutUnits = math.MaxInt32
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout+time.Second)
	defer cancel()
	execResult, err := client.ExecWithStdinEnv(execCtx, action.Command, workDir, int32(timeoutUnits), append(payload, '\n'), env)
	maxOutputBytes := hookMaxOutputBytes(cfg)
	if execResult != nil {
		res.Stdout = limitHookOutputText(execResult.Stdout, maxOutputBytes)
		res.Stderr = limitHookOutputText(execResult.Stderr, maxOutputBytes)
		res.ExitCode = execResult.ExitCode
	}
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	if execResult != nil && execResult.ExitCode != 0 {
		err := fmt.Errorf("hook command exited with code %d", execResult.ExitCode)
		res.Error = err.Error()
		return res, err
	}
	if execResult != nil {
		applyActionOutput(&res, execResult.Stdout, maxOutputBytes)
	} else {
		applyActionOutput(&res, "", maxOutputBytes)
	}
	return res, nil
}

func (*Service) runTool(ctx context.Context, cfg Config, _ Request, action HookAction, runner ToolRunner) (ActionResult, error) {
	res := ActionResult{ActionType: ActionTool, Name: action.Tool}
	if runner == nil {
		err := errors.New("hook tool runner is not configured")
		res.Error = err.Error()
		return res, err
	}
	timeout, err := parseTimeout(action.Timeout)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	input := action.Input
	if input == nil {
		input = map[string]any{}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := runner.RunHookTool(runCtx, action.Tool, input)
	res.Result = output
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	applyToolOutput(&res, output, hookMaxOutputBytes(cfg))
	return res, nil
}

func applyActionOutput(result *ActionResult, stdout string, maxOutputBytes int) {
	result.appendContextLimit = maxOutputBytes
	raw := strings.TrimSpace(stdout)
	if raw == "" {
		result.Decision = DecisionAllow
		return
	}
	var output struct {
		Decision      string         `json:"decision"`
		Reason        string         `json:"reason"`
		AppendContext string         `json:"append_context"`
		Metadata      map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		result.Decision = DecisionAllow
		result.Metadata = map[string]any{"raw_stdout": limitHookOutputText(raw, maxOutputBytes)}
		return
	}
	result.Decision = normalizeDecision(output.Decision)
	result.Reason = limitHookOutputText(output.Reason, maxOutputBytes)
	if output.Metadata != nil {
		if result.Metadata == nil {
			result.Metadata = map[string]any{}
		}
		for k, v := range output.Metadata {
			if k == "append_context" {
				if text, ok := v.(string); ok {
					result.appendContextRaw = text
					v = limitHookOutputText(text, maxOutputBytes)
				}
			}
			result.Metadata[k] = v
		}
	}
	if output.AppendContext != "" {
		if result.Metadata == nil {
			result.Metadata = map[string]any{}
		}
		result.appendContextRaw = output.AppendContext
		result.Metadata["append_context"] = limitHookOutputText(output.AppendContext, maxOutputBytes)
	}
}

func applyToolOutput(result *ActionResult, output any, maxOutputBytes int) {
	result.appendContextLimit = maxOutputBytes
	m, ok := output.(map[string]any)
	if !ok {
		raw, err := json.Marshal(output)
		if err == nil {
			_ = json.Unmarshal(raw, &m)
		}
	}
	if m == nil {
		result.Decision = DecisionAllow
		return
	}
	if decision, _ := m["decision"].(string); decision != "" {
		result.Decision = normalizeDecision(decision)
	}
	if reason, _ := m["reason"].(string); reason != "" {
		result.Reason = limitHookOutputText(reason, maxOutputBytes)
	}
	if appendContext, _ := m["append_context"].(string); appendContext != "" {
		result.appendContextRaw = appendContext
		result.Metadata = map[string]any{"append_context": limitHookOutputText(appendContext, maxOutputBytes)}
	}
}

func mergeDecision(result *Result, actionResult ActionResult, maxOutputBytes int) {
	decision := normalizeDecision(actionResult.Decision)
	if decision == "" {
		decision = DecisionAllow
	}
	appendContext := actionResult.appendContextRaw
	if actionResult.Metadata != nil {
		if strings.TrimSpace(appendContext) == "" {
			appendContext, _ = actionResult.Metadata["append_context"].(string)
		}
	}
	if strings.TrimSpace(appendContext) != "" {
		fragmentLimit := actionResult.appendContextLimit
		if fragmentLimit <= 0 {
			fragmentLimit = maxOutputBytes
		}
		result.appendContextLimit = minPositiveLimit(result.appendContextLimit, maxOutputBytes, fragmentLimit)
		if result.appendContextRaw != "" {
			result.appendContextRaw += "\n"
		}
		result.appendContextRaw += appendContext
		result.AppendContext = limitHookOutputText(result.appendContextRaw, firstPositive(result.appendContextLimit, maxOutputBytes))
	}
	switch decision {
	case DecisionDeny:
		result.Decision = DecisionDeny
		result.Reason = firstNonEmpty(actionResult.Reason, "hook denied action")
	case DecisionAskApproval:
		if result.Decision != DecisionDeny {
			result.Decision = DecisionAskApproval
			result.Reason = firstNonEmpty(actionResult.Reason, result.Reason)
		}
	case DecisionAppendContext:
		if result.Decision == "" || result.Decision == DecisionAllow {
			result.Decision = DecisionAppendContext
		}
		result.Reason = firstNonEmpty(actionResult.Reason, result.Reason)
	case DecisionAllow:
		if result.Decision == "" {
			result.Decision = DecisionAllow
		}
	}
}

func normalizeDecision(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case DecisionDeny:
		return DecisionDeny
	case DecisionAskApproval:
		return DecisionAskApproval
	case DecisionAppendContext:
		return DecisionAppendContext
	case DecisionAllow, "":
		return DecisionAllow
	default:
		return DecisionAllow
	}
}

func hookMaxOutputBytes(cfg Config) int {
	return cfg.Defaults.MaxOutputBytes
}

func minPositiveLimit(values ...int) int {
	limit := 0
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if limit == 0 || value < limit {
			limit = value
		}
	}
	return limit
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func limitHookOutputText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 {
		return text
	}
	limited := prune.PruneWithEdges(text, "hook output", prune.Config{
		MaxBytes:  limit,
		MaxLines:  prune.DefaultMaxLines,
		HeadBytes: limit * 3 / 4,
		TailBytes: limit / 4,
		HeadLines: prune.DefaultMaxLines * 3 / 4,
		TailLines: prune.DefaultMaxLines / 4,
	})
	if len(limited) > limit {
		return trimOutput(limited, limit)
	}
	return limited
}

func trimOutput(raw string, limit int) string {
	if limit <= 0 || len(raw) <= limit {
		return raw
	}
	return raw[:limit]
}

func hookSourceSummaries(hooks []Hook) []map[string]any {
	out := make([]map[string]any, 0, len(hooks))
	for _, hook := range hooks {
		source := normalizeHookSource(hook.source)
		item := map[string]any{
			"hook_name":   hook.Name,
			"source_kind": source.Kind,
		}
		out = append(out, item)
	}
	return out
}

func annotateActionResult(result *ActionResult, hook Hook) {
	if result == nil {
		return
	}
	if result.Metadata == nil {
		result.Metadata = map[string]any{}
	}
	source := normalizeHookSource(hook.source)
	result.Metadata["hook_name"] = hook.Name
	result.Metadata["hook_source_kind"] = source.Kind
}

func normalizeHookSource(source hookSource) hookSource {
	if strings.TrimSpace(source.Kind) == "" {
		source.Kind = sourceKindUser
	}
	return source
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
