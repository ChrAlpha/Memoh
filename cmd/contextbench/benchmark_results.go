package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const nominalProviderRoundTripNS = 1_000_000_000

var benchmarkOrder = []string{
	"BenchmarkLegacyAssembly",
	"BenchmarkApplyProviderRunConfig",
	"BenchmarkSelectProviderStepMessages",
}

type benchmarkSample struct {
	nsPerOp     float64
	bytesPerOp  float64
	allocsPerOp float64
}

type s4Record struct {
	Scenario                   string  `json:"scenario"`
	Benchmark                  string  `json:"benchmark"`
	FixtureState               string  `json:"fixture_state"`
	Statistic                  string  `json:"statistic"`
	Samples                    int     `json:"samples"`
	NSPerOp                    float64 `json:"ns_per_op"`
	BytesPerOp                 float64 `json:"bytes_per_op"`
	AllocsPerOp                float64 `json:"allocs_per_op"`
	NominalProviderRoundTripNS int     `json:"nominal_provider_round_trip_ns"`
	ProviderRoundTripPercent   float64 `json:"provider_round_trip_percent"`
}

func writeS4Results(outDir, inputPath string) error {
	raw, err := os.ReadFile(inputPath) //nolint:gosec // explicit CLI input is the intended benchmark artifact
	if err != nil {
		return fmt.Errorf("read benchmark output: %w", err)
	}
	records, err := parseBenchmarkResults(string(raw))
	if err != nil {
		return err
	}
	return writeJSONL(filepath.Join(outDir, s4OutputName), records)
}

func parseBenchmarkResults(output string) ([]s4Record, error) {
	samples := make(map[string][]benchmarkSample, len(benchmarkOrder))
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || !strings.HasPrefix(fields[0], "Benchmark") {
			continue
		}
		name := baseBenchmarkName(fields[0])
		if !slicesContains(benchmarkOrder, name) {
			continue
		}
		sample, ok := benchmarkMetrics(fields)
		if ok {
			samples[name] = append(samples[name], sample)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan benchmark output: %w", err)
	}
	records := make([]s4Record, 0, len(benchmarkOrder))
	for _, name := range benchmarkOrder {
		values := samples[name]
		if len(values) == 0 {
			return nil, fmt.Errorf("benchmark output missing %s", name)
		}
		nsValues := metricValues(values, func(sample benchmarkSample) float64 { return sample.nsPerOp })
		byteValues := metricValues(values, func(sample benchmarkSample) float64 { return sample.bytesPerOp })
		allocValues := metricValues(values, func(sample benchmarkSample) float64 { return sample.allocsPerOp })
		ns := median(nsValues)
		records = append(records, s4Record{
			Scenario: "s4_orchestration_overhead", Benchmark: name, FixtureState: benchmarkFixtureState(name),
			Statistic: "median", Samples: len(values),
			NSPerOp: ns, BytesPerOp: median(byteValues), AllocsPerOp: median(allocValues),
			NominalProviderRoundTripNS: nominalProviderRoundTripNS,
			ProviderRoundTripPercent:   ns / nominalProviderRoundTripNS * 100,
		})
	}
	return records, nil
}

func benchmarkFixtureState(name string) string {
	if name == "BenchmarkSelectProviderStepMessages" {
		return "synthetic_s3_40_step_state_after_fail_closed_continuation"
	}
	return "s1_mid_size"
}

func benchmarkMetrics(fields []string) (benchmarkSample, bool) {
	var sample benchmarkSample
	found := 0
	for i := 1; i < len(fields); i++ {
		var target *float64
		switch fields[i] {
		case "ns/op":
			target = &sample.nsPerOp
		case "B/op":
			target = &sample.bytesPerOp
		case "allocs/op":
			target = &sample.allocsPerOp
		default:
			continue
		}
		if i == 0 {
			return benchmarkSample{}, false
		}
		value, err := strconv.ParseFloat(fields[i-1], 64)
		if err != nil {
			return benchmarkSample{}, false
		}
		*target = value
		found++
	}
	return sample, found == 3
}

func baseBenchmarkName(name string) string {
	separator := strings.LastIndexByte(name, '-')
	if separator < 0 || separator == len(name)-1 {
		return name
	}
	if _, err := strconv.Atoi(name[separator+1:]); err == nil {
		return name[:separator]
	}
	return name
}

func metricValues(samples []benchmarkSample, selectMetric func(benchmarkSample) float64) []float64 {
	values := make([]float64, len(samples))
	for i, sample := range samples {
		values[i] = selectMetric(sample)
	}
	return values
}

func median(values []float64) float64 {
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	middle := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[middle]
	}
	return (ordered[middle-1] + ordered[middle]) / 2
}

func slicesContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
