package main

import (
	"context"
	"fmt"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
	"github.com/memohai/memoh/internal/contextview"
)

const (
	s3WindowTokens          = 32_000
	s3StepCount             = 80
	s3HugeResultCount       = 10
	s3ImageStepCount        = 5
	s3KeepRecentToolResults = 4
	s3MinMessages           = 20
	s3InitialMessageCount   = 7
	s3ImagePayloadBytes     = 4_800

	s3MeasurementScope = "direct_step_reselector_with_mirrored_attempt_allowance"
	s3IsolationCaveat  = "raw_tool_results_bypass_generic_64k_execution_cap; " +
		"real_turn_stops_on_fatal; benchmark_series_resumes_from_frozen_prefix_plus_protected_control_messages_for_synthetic_continuation"
	s3ImageAccounting = "full_provider_payload_bytes_including_literal_image_data"
)

type s3Record struct {
	Scenario                        string         `json:"scenario"`
	Variant                         string         `json:"variant"`
	Step                            int            `json:"step"`
	WindowTokens                    int            `json:"window_tokens"`
	OutputReserveTokens             int            `json:"output_reserve_tokens"`
	InputAllowanceTokens            int            `json:"input_allowance_tokens"`
	FixedPrefixTokens               int            `json:"fixed_prefix_tokens"`
	SuffixBudgetTokens              int            `json:"suffix_budget_tokens"`
	Estimator                       string         `json:"estimator"`
	EstimatorSafetyPercent          int            `json:"estimator_safety_factor_percent"`
	PayloadTokens                   int            `json:"payload_tokens"`
	PayloadBytes                    int            `json:"payload_bytes"`
	PayloadHash                     string         `json:"payload_hash"`
	OverAllowance                   bool           `json:"over_allowance"`
	OverWindow                      bool           `json:"over_window"`
	InjectedMessagesExpected        int            `json:"injected_messages_expected"`
	InjectedMessagesPresent         int            `json:"injected_messages_present"`
	InjectedMessagesStillPresent    bool           `json:"injected_messages_still_present"`
	BackgroundSummaryRevision       int            `json:"background_summary_revision"`
	BackgroundSummaryCount          int            `json:"background_summary_count"`
	BackgroundSummaryCurrent        bool           `json:"background_summary_current_present"`
	ToolClosureValid                bool           `json:"tool_closure_valid"`
	TrimNoticePresent               bool           `json:"trim_notice_present"`
	TrimNoticeCount                 int            `json:"trim_notice_count"`
	PrefixIntact                    bool           `json:"prefix_intact"`
	ProtectedContentViolations      int            `json:"protected_content_violations"`
	ProtectedContentIntact          bool           `json:"protected_content_intact"`
	ImageStep                       bool           `json:"image_step"`
	ImagePartsPresent               int            `json:"image_parts_present"`
	HugeResult                      bool           `json:"huge_result"`
	RawToolResultBytes              int            `json:"raw_tool_result_bytes"`
	Dropped                         int            `json:"dropped"`
	Truncated                       int            `json:"truncated"`
	DropReasons                     map[string]int `json:"drop_reasons,omitempty"`
	ReselectionApplied              bool           `json:"reselection_applied"`
	Fatal                           bool           `json:"fatal"`
	FatalError                      string         `json:"fatal_error,omitempty"`
	ProviderCallAllowed             bool           `json:"provider_call_allowed"`
	SyntheticContinuationAfterFatal bool           `json:"synthetic_continuation_after_fatal"`
	MeasurementScope                string         `json:"measurement_scope"`
	IsolationCaveat                 string         `json:"isolation_caveat"`
	GenericToolOutputCapApplied     bool           `json:"generic_tool_output_cap_applied"`
	ImageAccounting                 string         `json:"image_accounting"`
	AttemptPreflightAllowanceExact  bool           `json:"attempt_preflight_allowance_mirrored"`
}

type s3Step struct {
	Step                  int
	ToolName              string
	ToolResult            string
	ToolResultBytes       int
	HugeResult            bool
	Image                 bool
	Injection             string
	BackgroundRefresh     bool
	BackgroundRevision    int
	BackgroundSummaryText string
}

type s3TypedSetup struct {
	cfg             agentpkg.RunConfig
	prefix          []sdk.Message
	prefixCount     int
	plan            contextfrag.ContextBudgetPlan
	inputAllowance  int
	fixedPrefixCost int
	suffixBudget    int
	legacySystem    string
	legacyMessages  []sdk.Message
}

type s3SelectionAudit struct {
	dropped             int
	truncated           int
	dropReasons         map[string]int
	reselectionApplied  bool
	fatal               bool
	fatalError          string
	syntheticAfterFatal bool
}

func runS3(fixture benchFixture) []s3Record {
	steps := buildS3Steps()
	typedSetup := buildS3TypedSetup(fixture)

	legacySystem := typedSetup.legacySystem
	legacyMessages := cloneMessages(typedSetup.legacyMessages)
	legacyPrefix := cloneMessages(legacyMessages)
	legacyPrefixCount := len(legacyPrefix)
	typedMessages := cloneMessages(typedSetup.prefix)
	typedSeriesSynthetic := false

	expectedInjections := make([]string, 0, 3)
	records := make([]s3Record, 0, s3StepCount*2)
	for _, step := range steps {
		if step.Injection != "" {
			expectedInjections = append(expectedInjections, step.Injection)
		}

		legacyMessages = advanceS3Messages(legacyMessages, legacyPrefixCount, step, false)
		typedCandidate := advanceS3Messages(typedMessages, typedSetup.prefixCount, step, true)

		selection := typedSetup.cfg.ContextStepReselector(context.Background(), typedSetup.selectionInput(typedCandidate))
		audit := s3SelectionAudit{
			dropped:             selection.Dropped,
			truncated:           selection.Truncated,
			dropReasons:         cloneS3Counts(selection.DropReasons),
			syntheticAfterFatal: typedSeriesSynthetic,
		}
		typedRecordMessages := typedCandidate
		switch {
		case selection.FatalError != nil:
			audit.fatal = true
			audit.fatalError = budgetErrorLabel(selection.FatalError)
			typedMessages = recoverS3AfterFatal(typedSetup.prefix, expectedInjections, step.BackgroundSummaryText)
			typedSeriesSynthetic = true
		case selection.Messages != nil && s3PrefixIntact(selection.Messages, typedSetup.prefix):
			typedMessages = selection.Messages
			typedRecordMessages = typedMessages
			audit.reselectionApplied = true
		default:
			typedMessages = typedCandidate
		}

		records = append(records,
			measureS3Record(
				"legacy",
				legacySystem,
				fixture.tools,
				legacyMessages,
				legacyPrefix,
				expectedInjections,
				step,
				typedSetup,
				s3SelectionAudit{},
			),
			measureS3Record(
				"typed",
				typedSetup.cfg.System,
				fixture.tools,
				typedRecordMessages,
				typedSetup.prefix,
				expectedInjections,
				step,
				typedSetup,
				audit,
			),
		)
	}
	return records
}

func s3BenchmarkInput(fixture benchFixture) agentpkg.ContextStepSelectionInput {
	setup := buildS3TypedSetup(fixture)
	messages := cloneMessages(setup.prefix)
	steps := buildS3Steps()
	expectedInjections := make([]string, 0, 3)
	for index, step := range steps[:40] {
		if step.Injection != "" {
			expectedInjections = append(expectedInjections, step.Injection)
		}
		messages = advanceS3Messages(messages, setup.prefixCount, step, true)
		input := setup.selectionInput(messages)
		if index == 39 {
			input.Messages = cloneMessages(input.Messages)
			return input
		}
		selection := setup.cfg.ContextStepReselector(context.Background(), input)
		if selection.FatalError != nil {
			messages = recoverS3AfterFatal(setup.prefix, expectedInjections, step.BackgroundSummaryText)
			continue
		}
		if selection.Messages != nil {
			if !s3PrefixIntact(selection.Messages, setup.prefix) {
				panic(fmt.Sprintf("prepare S3 benchmark input at step %d: selector changed immutable prefix", step.Step))
			}
			messages = selection.Messages
		}
	}
	panic("unreachable: S3 benchmark input requires 40 steps")
}

func buildS3TypedSetup(fixture benchFixture) s3TypedSetup {
	initial := buildS3InitialContext(fixture)
	cfg, err := contextview.ProviderRunConfigApplier(nil)(
		context.Background(),
		typedConfig(fixture, initial.sourceFrags, s3WindowTokens),
	)
	if err != nil {
		panic(fmt.Sprintf("compile S3 typed provider config: %v", err))
	}
	if cfg.ContextStepReselector == nil {
		panic("compile S3 typed provider config: step reselector not installed")
	}
	if cfg.ContextManifest.BudgetPlan == nil {
		panic("compile S3 typed provider config: active budget plan missing")
	}

	plan := *cfg.ContextManifest.BudgetPlan
	inputAllowance := plan.Window - plan.OutputReserve
	prefix := cloneMessages(cfg.Messages)
	_, prefixBytes := contextfrag.ProviderPayloadHashAndBytes(cfg.System, prefix, fixture.tools)
	fixedPrefixCost := contextfrag.ProviderBudgetTokensFromBytes(prefixBytes)
	suffixBudget := inputAllowance - fixedPrefixCost
	if suffixBudget < 1 {
		suffixBudget = 1
	}

	return s3TypedSetup{
		cfg:             cfg,
		prefix:          prefix,
		prefixCount:     len(prefix),
		plan:            plan,
		inputAllowance:  inputAllowance,
		fixedPrefixCost: fixedPrefixCost,
		suffixBudget:    suffixBudget,
		legacySystem:    flattenSystem(initial.systemFrags),
		legacyMessages:  cloneMessages(initial.messages),
	}
}

func (s s3TypedSetup) selectionInput(messages []sdk.Message) agentpkg.ContextStepSelectionInput {
	return agentpkg.ContextStepSelectionInput{
		Scope:                 s.cfg.ContextScope,
		InitialMessageCount:   s.prefixCount,
		Messages:              messages,
		BudgetMaxTokens:       s.suffixBudget,
		RecentProtectTokens:   s.cfg.ContextRecentProtectTokens,
		KeepRecentToolResults: s3KeepRecentToolResults,
		MinMessages:           s3MinMessages,
	}
}

func measureS3Record(
	variant string,
	system string,
	tools []sdk.Tool,
	messages []sdk.Message,
	prefix []sdk.Message,
	expectedInjections []string,
	step s3Step,
	setup s3TypedSetup,
	audit s3SelectionAudit,
) s3Record {
	hash, payloadBytes := contextfrag.ProviderPayloadHashAndBytes(system, messages, tools)
	payloadTokens := contextfrag.ProviderBudgetTokensFromBytes(payloadBytes)
	injectedPresent := countS3InjectedMessages(messages, expectedInjections)
	backgroundCount, backgroundCurrent := s3BackgroundSummaryStatus(messages, step.BackgroundSummaryText)
	trimNoticeCount := countS3TrimNotices(messages)
	prefixIntact := s3PrefixIntact(messages, prefix)
	violations := len(expectedInjections) - injectedPresent
	if !prefixIntact {
		violations++
	}
	backgroundValid := backgroundCount == 0
	if step.BackgroundSummaryText != "" {
		backgroundValid = backgroundCurrent
		if variant == "typed" {
			backgroundValid = backgroundCount == 1 && backgroundCurrent
		}
	}
	if !backgroundValid {
		violations++
	}
	suffixBudget := 0
	fixedPrefixTokens := 0
	if variant == "typed" {
		suffixBudget = setup.suffixBudget
		fixedPrefixTokens = setup.fixedPrefixCost
	}
	return s3Record{
		Scenario: "s3_long_loop", Variant: variant, Step: step.Step,
		WindowTokens: s3WindowTokens, OutputReserveTokens: setup.plan.OutputReserve,
		InputAllowanceTokens: setup.inputAllowance, FixedPrefixTokens: fixedPrefixTokens,
		SuffixBudgetTokens: suffixBudget, Estimator: contextfrag.ProviderBudgetEstimator,
		EstimatorSafetyPercent: contextfrag.ProviderBudgetSafetyFactorPercent,
		PayloadTokens:          payloadTokens, PayloadBytes: payloadBytes, PayloadHash: hash,
		OverAllowance: payloadTokens > setup.inputAllowance, OverWindow: payloadTokens > s3WindowTokens,
		InjectedMessagesExpected: len(expectedInjections), InjectedMessagesPresent: injectedPresent,
		InjectedMessagesStillPresent: injectedPresent == len(expectedInjections),
		BackgroundSummaryRevision:    step.BackgroundRevision, BackgroundSummaryCount: backgroundCount,
		BackgroundSummaryCurrent: backgroundCurrent, ToolClosureValid: s3ToolClosureValid(messages),
		TrimNoticePresent: trimNoticeCount > 0, TrimNoticeCount: trimNoticeCount,
		PrefixIntact: prefixIntact, ProtectedContentViolations: violations, ProtectedContentIntact: violations == 0,
		ImageStep: step.Image, ImagePartsPresent: countS3ImageParts(messages),
		HugeResult: step.HugeResult, RawToolResultBytes: step.ToolResultBytes,
		Dropped: audit.dropped, Truncated: audit.truncated, DropReasons: audit.dropReasons,
		ReselectionApplied: audit.reselectionApplied, Fatal: audit.fatal, FatalError: audit.fatalError,
		ProviderCallAllowed: !audit.fatal, SyntheticContinuationAfterFatal: audit.syntheticAfterFatal,
		MeasurementScope: s3MeasurementScope,
		IsolationCaveat:  s3IsolationCaveat, GenericToolOutputCapApplied: false,
		ImageAccounting: s3ImageAccounting, AttemptPreflightAllowanceExact: variant == "typed",
	}
}
