package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const (
	s1OutputName = "s1-granularity.jsonl"
	s2OutputName = "s2-prefix-stability.jsonl"
	s3OutputName = "s3-step-governance.jsonl"
	s4OutputName = "s4-overhead.jsonl"
)

func main() {
	outDir := flag.String("out", "benchout", "directory for JSONL output")
	benchInput := flag.String("bench-input", "", "go test -bench output to convert into S4 JSONL")
	flag.Parse()
	if err := runDeterministicScenarios(*outDir); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "contextbench: %v\n", err)
		os.Exit(1)
	}
	if *benchInput != "" {
		if err := writeS4Results(*outDir, *benchInput); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "contextbench: %v\n", err)
			os.Exit(1)
		}
	}
}

func runDeterministicScenarios(outDir string) error {
	if outDir == "" {
		return errors.New("output directory is required")
	}
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	fixture := buildS1Fixture()
	if err := writeJSONL(filepath.Join(outDir, s1OutputName), runS1(fixture)); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(outDir, s2OutputName), runS2(fixture)); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(outDir, s3OutputName), runS3(fixture)); err != nil {
		return err
	}
	return nil
}

func writeJSONL[T any](path string, records []T) error {
	file, err := os.Create(path) //nolint:gosec // caller chooses the benchmark artifact directory
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	writer := bufio.NewWriter(file)
	for _, record := range records {
		raw, marshalErr := marshalStable(record)
		if marshalErr != nil {
			_ = file.Close()
			return fmt.Errorf("marshal %s: %w", path, marshalErr)
		}
		if _, err = writer.Write(append(raw, '\n')); err != nil {
			_ = file.Close()
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	if err = writer.Flush(); err != nil {
		_ = file.Close()
		return fmt.Errorf("flush %s: %w", path, err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}
