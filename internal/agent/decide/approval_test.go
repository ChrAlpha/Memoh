package decide

import (
	"errors"
	"testing"

	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
)

func TestLegacyPermissionOptionID(t *testing.T) {
	t.Parallel()

	options := []toolapproval.PermissionOption{
		{ID: "allow-session", Kind: toolapproval.OptionKindAllowAlways},
		{ID: "deny-once", Kind: toolapproval.OptionKindRejectOnce},
		{ID: "allow-once", Kind: toolapproval.OptionKindAllowOnce},
	}
	if got, err := legacyPermissionOptionID(options, "approve"); err != nil || got != "allow-once" {
		t.Fatalf("legacy approve = %q, %v", got, err)
	}
	// Without allow_once every remaining allow option carries persistence the
	// binary surface could not show; Memoh never persists ACP grants on the
	// user's behalf, so the binary approve fails with an explicit error.
	if got, err := legacyPermissionOptionID(options[:2], "approve"); got != "" || !errors.Is(err, toolapproval.ErrOptionUnavailable) {
		t.Fatalf("approve without allow_once = %q, %v, want option-unavailable", got, err)
	}
	if got, err := legacyPermissionOptionID(options[2:], "reject"); got != "" || err != nil {
		t.Fatalf("reject without reject_once = %q, %v", got, err)
	}
	// Rejection is always safe: an ambiguous set of several reject_once
	// options degrades to the binary reject (cancelled outcome downstream)
	// instead of dead-ending the request.
	ambiguousReject := []toolapproval.PermissionOption{
		{ID: "reject-a", Kind: toolapproval.OptionKindRejectOnce},
		{ID: "reject-b", Kind: toolapproval.OptionKindRejectOnce},
	}
	if got, err := legacyPermissionOptionID(ambiguousReject, "reject"); got != "" || err != nil {
		t.Fatalf("ambiguous reject = %q, %v, want binary fallback", got, err)
	}
}
