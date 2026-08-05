package main

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/contextview"
)

func TestDeterministicScenarioRunsAreByteIdentical(t *testing.T) {
	buildDir := t.TempDir()
	binary := filepath.Join(buildDir, "contextbench")
	build := exec.CommandContext(context.Background(), "go", "build", "-o", binary, ".") //nolint:gosec // fixed local build command
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build contextbench: %v\n%s", err, output)
	}
	benchInput := filepath.Join(buildDir, "bench.txt")
	if err := os.WriteFile(benchInput, []byte(testBenchmarkOutput()), 0o600); err != nil {
		t.Fatalf("write fixed benchmark input: %v", err)
	}
	first := t.TempDir()
	second := t.TempDir()
	for _, outDir := range []string{first, second} {
		command := exec.CommandContext(context.Background(), binary, "-out", outDir, "-bench-input", benchInput) //nolint:gosec // binary and input are fixed files under t.TempDir
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("run contextbench: %v\n%s", err, output)
		}
	}
	for _, name := range []string{s1OutputName, s2OutputName, s3OutputName, s4OutputName} {
		left, err := os.ReadFile(filepath.Join(first, name)) //nolint:gosec // fixed name under t.TempDir
		if err != nil {
			t.Fatalf("read first %s: %v", name, err)
		}
		right, err := os.ReadFile(filepath.Join(second, name)) //nolint:gosec // fixed name under t.TempDir
		if err != nil {
			t.Fatalf("read second %s: %v", name, err)
		}
		if !bytes.Equal(left, right) {
			t.Fatalf("%s differs across full scenario runs", name)
		}
	}
}

func TestParseBenchmarkResultsUsesFiveSampleMedian(t *testing.T) {
	t.Parallel()

	records, err := parseBenchmarkResults(testBenchmarkOutput())
	if err != nil {
		t.Fatalf("parse benchmark output: %v", err)
	}
	if len(records) != len(benchmarkOrder) {
		t.Fatalf("records = %d, want %d", len(records), len(benchmarkOrder))
	}
	for _, record := range records {
		if record.Samples != 5 || record.NSPerOp != 300 || record.BytesPerOp != 12 || record.AllocsPerOp != 4 {
			t.Fatalf("record = %#v", record)
		}
		if math.Abs(record.ProviderRoundTripPercent-0.00003) > 1e-12 {
			t.Fatalf("provider percentage = %v", record.ProviderRoundTripPercent)
		}
		if record.FixtureState != benchmarkFixtureState(record.Benchmark) {
			t.Fatalf("fixture state = %q for %s", record.FixtureState, record.Benchmark)
		}
	}
}

func testBenchmarkOutput() string {
	var output strings.Builder
	for _, benchmark := range benchmarkOrder {
		for i, ns := range []int{500, 100, 300, 200, 400} {
			_, _ = output.WriteString(benchmark + "-16 100 " + strconv.Itoa(ns) + " ns/op " + strconv.Itoa(10+i) + " B/op " + strconv.Itoa(2+i) + " allocs/op\n")
		}
	}
	return output.String()
}

func TestS3TraceMeetsGovernanceContract(t *testing.T) {
	t.Parallel()

	records := runS3(buildS1Fixture())
	if len(records) != s3StepCount*2 {
		t.Fatalf("records = %d, want %d", len(records), s3StepCount*2)
	}

	counts := make(map[string]int)
	hugeCounts := make(map[string]int)
	imageCounts := make(map[string]int)
	lastLegacyTokens := 0
	typedFatals := 0
	typedFatalSeen := false
	for _, record := range records {
		counts[record.Variant]++
		if record.HugeResult {
			hugeCounts[record.Variant]++
		}
		if record.ImageStep {
			imageCounts[record.Variant]++
		}
		if !record.ToolClosureValid || !record.PrefixIntact || !record.InjectedMessagesStillPresent {
			t.Fatalf("%s step %d violated closure/prefix/injection: %#v", record.Variant, record.Step, record)
		}
		switch record.Variant {
		case "legacy":
			if record.PayloadTokens < lastLegacyTokens {
				t.Fatalf("legacy payload fell at step %d: %d < %d", record.Step, record.PayloadTokens, lastLegacyTokens)
			}
			lastLegacyTokens = record.PayloadTokens
			if record.AttemptPreflightAllowanceExact {
				t.Fatalf("legacy step %d claims typed attempt-preflight allowance", record.Step)
			}
			if record.SyntheticContinuationAfterFatal {
				t.Fatalf("legacy step %d claims synthetic typed continuation", record.Step)
			}
		case "typed":
			if record.ProtectedContentViolations != 0 || !record.ProtectedContentIntact {
				t.Fatalf("typed step %d lost protected content: %#v", record.Step, record)
			}
			if !record.AttemptPreflightAllowanceExact {
				t.Fatalf("typed step %d did not mirror attempt-preflight allowance", record.Step)
			}
			if record.SyntheticContinuationAfterFatal != typedFatalSeen {
				t.Fatalf("typed step %d synthetic continuation = %v, want %v", record.Step, record.SyntheticContinuationAfterFatal, typedFatalSeen)
			}
			if record.Fatal {
				typedFatals++
				typedFatalSeen = true
				if record.ProviderCallAllowed {
					t.Fatalf("typed fatal step %d allowed a provider call", record.Step)
				}
			} else if !record.ProviderCallAllowed {
				t.Fatalf("typed accepted step %d blocked the provider: %#v", record.Step, record)
			}
		default:
			t.Fatalf("unexpected variant %q", record.Variant)
		}
	}
	for _, variant := range []string{"legacy", "typed"} {
		if counts[variant] != s3StepCount || hugeCounts[variant] != s3HugeResultCount || imageCounts[variant] != s3ImageStepCount {
			t.Fatalf("%s counts: rows=%d huge=%d images=%d", variant, counts[variant], hugeCounts[variant], imageCounts[variant])
		}
	}
	if typedFatals == 0 {
		t.Fatal("typed trace did not exercise fail-closed protected overflow")
	}
}

func TestS1DropOrderAuditRequiresCompleteTrace(t *testing.T) {
	t.Parallel()

	fixture := buildS1Fixture()
	cfg, err := contextview.ProviderRunConfigApplier(nil)(context.Background(), typedConfig(fixture, fixture.sourceFrags, 8_000))
	if err != nil {
		t.Fatalf("compile typed fixture: %v", err)
	}
	correct, actual := validateSystemDropOrder(fixture.systemFrags, cfg.ContextManifest)
	if !correct || len(actual) == 0 {
		t.Fatalf("complete drop trace: correct=%v actual=%v", correct, actual)
	}
	manifestWithoutTrace := cfg.ContextManifest
	manifestWithoutTrace.EditTrace = nil
	if correct, _ = validateSystemDropOrder(fixture.systemFrags, manifestWithoutTrace); correct {
		t.Fatal("incomplete drop trace passed the drop-order audit")
	}
}

func TestS1FixtureMeetsDistributionContract(t *testing.T) {
	t.Parallel()

	fixture := buildS1Fixture()
	kindCounts := make(map[string]int)
	multilingualSkills := 0
	for _, frag := range fixture.systemFrags {
		kindCounts[string(frag.Kind)]++
		if frag.Kind == "skills_catalog" && stringsInFrag(frag, "🙂") {
			multilingualSkills++
		}
	}
	if kindCounts["skills_catalog"] != 41 || kindCounts["workspace_instruction"] != 6 || kindCounts["platform_identity"] != 9 || kindCounts["tool_usage"] != 11 {
		t.Fatalf("fixture system counts = %#v", kindCounts)
	}
	if multilingualSkills < 12 {
		t.Fatalf("multilingual skills = %d, want at least 12", multilingualSkills)
	}
	if len(fixture.messages) != 31 || len(fixture.tools) < 10 {
		t.Fatalf("messages/tools = %d/%d", len(fixture.messages), len(fixture.tools))
	}
}

func stringsInFrag(frag contextfrag.ContextFrag, value string) bool {
	for _, part := range frag.Parts {
		if strings.Contains(part.Text, value) {
			return true
		}
	}
	return false
}
