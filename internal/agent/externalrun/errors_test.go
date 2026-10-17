package externalrun

import (
	"errors"
	"fmt"
	"testing"

	acpagent "github.com/felinics/memoh/internal/agent/runtime/acp"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
)

func TestPublicRuntimeControlErrorKeepsExternalAgentCodesAndTeamRouting(t *testing.T) {
	missing := fmt.Errorf("resolve: %w", acpagent.ErrAgentNotFound)
	if got := publicRuntimeControlError(missing); apperror.CodeOf(got) != apperror.CodeACPAgentNotFound {
		t.Fatalf("unknown agent = %v (code %q), want %q", got, apperror.CodeOf(got), apperror.CodeACPAgentNotFound)
	}
	if got := publicRuntimeControlError(turn.ErrTeamNotServed); !errors.Is(got, turn.ErrTeamNotServed) {
		t.Fatalf("team routing sentinel was translated: %v", got)
	}
}
