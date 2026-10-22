package agent

import (
	"context"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/agentapi/ports"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/notify"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
)

// NewBackgroundNotifier returns the notifier for background task events. It is
// built apart from Service because delivering to a channel needs the Channel
// runtime, which itself depends on the turn port Service implements. A nil
// deliver delivers nowhere.
func NewBackgroundNotifier(
	messages messagepkg.Writer,
	sessions ports.SessionService,
	deliver func(context.Context, session.Thread, string) error,
) agentapi.BackgroundNotifier {
	return notify.New(messages, sessions, deliver)
}

// AutoCompactionThreshold is the token level at which automatic compaction
// starts for a model window and the bot's configured threshold, for surfaces
// that show it. Zero means automatic compaction is off.
func AutoCompactionThreshold(userThreshold, contextWindow int) int {
	return compact.AutoThreshold(userThreshold, contextWindow)
}
