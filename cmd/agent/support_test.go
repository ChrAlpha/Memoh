package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRunAccountCommandValidatesRecoveryInputBeforeOpeningDatabase(t *testing.T) {
	t.Setenv("CONFIG_PATH", "/path/that/does/not/exist")

	for name, tc := range map[string]struct {
		args     []string
		password string
		want     string
	}{
		"command":  {args: []string{"unknown"}, want: "usage:"},
		"identity": {args: []string{"recover-admin", " "}, password: "secret", want: "username or email is required"},
		"password": {args: []string{"recover-admin", "admin"}, password: "\n", want: "new password is required"},
		"limit":    {args: []string{"recover-admin", "admin"}, password: strings.Repeat("x", 4097), want: "password exceeds"},
	} {
		t.Run(name, func(t *testing.T) {
			err := runAccountCommand(tc.args, strings.NewReader(tc.password))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runAccountCommand() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

type batchedAdopter struct {
	batches []int64
	calls   int
	err     error
}

func (a *batchedAdopter) AdoptSessionRuntimeOwnersFromMetadata(context.Context, int32) (int64, error) {
	if a.err != nil {
		return 0, a.err
	}
	if a.calls >= len(a.batches) {
		return 0, nil
	}
	a.calls++
	return a.batches[a.calls-1], nil
}

func TestAdoptRuntimeOwnersRepeatsUntilABatchIsEmpty(t *testing.T) {
	adopter := &batchedAdopter{batches: []int64{500, 500, 12}}
	total, err := adoptRuntimeOwners(t.Context(), adopter, 500)
	if err != nil || total != 1012 {
		t.Fatalf("adoptRuntimeOwners() = %d, %v; want 1012 and no error", total, err)
	}
	if adopter.calls != 3 {
		t.Fatalf("batches taken = %d, want 3", adopter.calls)
	}
	again, err := adoptRuntimeOwners(t.Context(), adopter, 500)
	if err != nil || again != 0 {
		t.Fatalf("a second pass = %d, %v; want nothing left", again, err)
	}
}

func TestAdoptRuntimeOwnersStopsOnAnError(t *testing.T) {
	boom := errors.New("boom")
	total, err := adoptRuntimeOwners(t.Context(), &batchedAdopter{err: boom}, 500)
	if !errors.Is(err, boom) || total != 0 {
		t.Fatalf("adoptRuntimeOwners() = %d, %v; want the error", total, err)
	}
}

func TestRunBackfillCommandRejectsAnUnknownPass(t *testing.T) {
	err := runBackfillCommand([]string{"unknown"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("runBackfillCommand() error = %v, want the usage", err)
	}
}
