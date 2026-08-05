package main

import (
	"context"
	"testing"

	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
	"github.com/memohai/memoh/internal/contextview"
)

var (
	benchmarkPayloadSink   providerPayload
	benchmarkRunConfigSink agentpkg.RunConfig
	benchmarkSelectionSink agentpkg.ContextStepSelectionResult
)

func BenchmarkLegacyAssembly(b *testing.B) {
	fixture := buildS1Fixture()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		benchmarkPayloadSink = legacyPayload(fixture)
	}
}

func BenchmarkApplyProviderRunConfig(b *testing.B) {
	fixture := buildS1Fixture()
	cfg := typedConfig(fixture, fixture.sourceFrags, 64_000)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		benchmarkRunConfigSink = contextview.ApplyProviderRunConfig(context.Background(), nil, cfg)
	}
}

func BenchmarkSelectProviderStepMessages(b *testing.B) {
	fixture := buildS1Fixture()
	input := s3BenchmarkInput(fixture)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		benchmarkSelectionSink = contextview.SelectProviderStepMessages(context.Background(), input)
	}
}
