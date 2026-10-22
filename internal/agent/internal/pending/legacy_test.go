package pending

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
)

func legacyItem(t *testing.T, teamID string) sessionruntime.LegacyFollowUp {
	t.Helper()
	cmd := turn.StartTurnCommand{
		SchemaVersion: turnwire.StartTurnVersion, TeamID: teamID, Mode: turn.ModeChat,
		BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1", Query: "typed before the upgrade",
		// The release that wrote it stored the turn's own credentials.
		Token: "old-token", ChatToken: "old-chat-token",
	}
	payload, err := turnwire.NewQueuedInput(cmd, nil, time.Time{}).Marshal()
	require.NoError(t, err)
	// NewQueuedInput blanks the tokens; put them back the way the old release
	// wrote them.
	input := turnwire.ParseQueuedInput(payload)
	input.Command.Token, input.Command.ChatToken = "old-token", "old-chat-token"
	payload, err = input.Marshal()
	require.NoError(t, err)
	return sessionruntime.LegacyFollowUp{
		Key: sessionruntime.Key{BotID: "bot-1", SessionID: "session-1"}, ID: "legacy-1",
		InvocationID: "follow-up:legacy-1", Payload: payload, EnqueuedDuringRunID: "run-9",
		CreatedAt: time.Unix(1000, 0).UTC(),
	}
}

// An imported follow-up keeps the identity it had, so one moved twice is one
// input, and its stored credentials are dropped on the way in.
func TestLegacyFollowUpIsImportedUnderItsOwnIdentityWithoutItsTokens(t *testing.T) {
	item := legacyItem(t, "team-1")
	require.Contains(t, string(item.Payload), "old-token")

	entry, ok, err := legacyFollowUpEntry("team-1")(item)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, item.Key, entry.Key)
	require.Equal(t, sessionruntime.QueueOriginFollowUp, entry.Origin)
	require.Equal(t, "follow-up:legacy-1", entry.InvocationID)
	require.Equal(t, "run-9", entry.EnqueuedDuringRunID)
	require.True(t, entry.CreatedAt.Equal(item.CreatedAt))
	require.NotContains(t, string(entry.Payload), "old-token")
	require.NotContains(t, string(entry.Payload), "old-chat-token")
	restored, err := turnwire.ParseQueuedInput(entry.Payload).StartCommand()
	require.NoError(t, err)
	require.Equal(t, "typed before the upgrade", restored.Query)
}

func TestLegacyFollowUpOfAnotherTeamIsLeftForItsOwnPass(t *testing.T) {
	_, ok, err := legacyFollowUpEntry("team-2")(legacyItem(t, "team-1"))
	require.NoError(t, err)
	require.False(t, ok, "it stays in Redis")

	_, ok, err = legacyFollowUpEntry("")(legacyItem(t, "team-1"))
	require.NoError(t, err)
	require.True(t, ok, "a pass bound to no team takes every team's")
}

func TestLegacyFollowUpThatCannotBeReadIsAnError(t *testing.T) {
	item := legacyItem(t, "team-1")
	item.Payload = []byte(`{"text":"typed, but never carried a turn"}`)
	_, ok, err := legacyFollowUpEntry("team-1")(item)
	require.Error(t, err, "the import drops it, as the old release rejected it")
	require.False(t, ok)
}
