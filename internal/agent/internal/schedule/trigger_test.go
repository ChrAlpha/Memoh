package schedule

import (
	"testing"

	schedulepkg "github.com/felinics/memoh/internal/schedule"
)

func TestScheduleInvocationUsesFireIdentity(t *testing.T) {
	a := scheduleInvocationID(schedulepkg.TriggerPayload{ID: "schedule", SessionID: "shared", FireID: "fire-a"})
	b := scheduleInvocationID(schedulepkg.TriggerPayload{ID: "schedule", SessionID: "shared", FireID: "fire-b"})
	if a == b {
		t.Fatal("different fires collided in the same session")
	}
	if a != scheduleInvocationID(schedulepkg.TriggerPayload{ID: "schedule", SessionID: "shared", FireID: "fire-a"}) {
		t.Fatal("same fire lost retry identity")
	}
}
