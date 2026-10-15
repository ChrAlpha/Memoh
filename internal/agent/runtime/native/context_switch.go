package native

import (
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	sdk "github.com/felinics/twilight/sdk"
)

type ContextSwitchSource struct {
	Ref      contextfrag.ContextRef
	Coverage []contextfrag.ContextRef
}

type ContextSwitchInput struct {
	RunID                string
	StepIndex            int
	Request              sdk.Request
	Sources              []ContextSwitchSource
	Protected            []bool
	InputAllowanceTokens int
}

type ContextSwitchResult struct {
	Messages      []sdk.Message
	SourceIndexes []int
	Sources       []ContextSwitchSource
	OnAdopted     func()
}

type contextSwitchState struct{}
