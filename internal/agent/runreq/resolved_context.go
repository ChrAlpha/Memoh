package runreq

import (
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
)

// ResolvedContext is what a run resolved before its first model call: the model and provider, the assembled run configuration and the request pressure.
type ResolvedContext struct {
	RunConfig       native.RunConfig
	Model           models.GetResponse
	Provider        sqlc.Provider
	Query           string // headerified persistable query
	InjectedRecords *[]InjectedMessageRecord
	EstimatedTokens int // estimated input token count for compaction
	// pressure is a native chat run's estimate of its next request, which the
	// background trigger judges the finished run by.
	Pressure           *RunPressure
	ContextTokenBudget int // token budget used to clamp compaction triggers
}
