package externalrun

import (
	"context"
	"sync"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// A scheduled run has nobody to answer a decision, but an interactive External
// Agent turn does: the flag says whether the run can deliver a question, and a
// run that keeps it false never offers the runtime its ask_user tools.
func TestExternalPromptInputCarriesWhetherTheRunCanAskTheUser(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		unattended bool
		want       bool
	}{
		{name: "interactive", unattended: false, want: true},
		{name: "unattended", unattended: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exec := &Executor{host: testHost(t, Deps{}), unattended: tc.unattended}
			got := exec.promptInput(context.Background(), func(native.StreamEvent) {}).CanRequestUserInput
			if got != tc.want {
				t.Fatalf("CanRequestUserInput = %v, want %v", got, tc.want)
			}
		})
	}
}

// The run's own cleanup and the background reconciliation of an unknown round
// both ask for the turn's decision rows to be removed, from different
// goroutines. The rows are removed once, and the callers do not race on that.
func TestExternalProjectionCleanupRunsOnceAcrossGoroutines(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	exec := &Executor{host: testHost(t, Deps{Messages: messages})}
	exec.projections.record("call-1")
	exec.projections.complete("call-1", &messagepkg.Message{ID: "projection-1"})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exec.cleanupProjections(context.Background())
		}()
	}
	wg.Wait()

	if len(messages.Deleted) != 1 || len(messages.Deleted[0]) != 1 || messages.Deleted[0][0] != "projection-1" {
		t.Fatalf("deleted = %v, want the projected row removed once", messages.Deleted)
	}
}
