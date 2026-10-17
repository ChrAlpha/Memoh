package transcript

import (
	"log/slog"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
)

// stage4Session is a compaction session whose history is read back through a
// Reader on the same database.
type stage4Session struct {
	*agenttest.CompactionSession
	reader *Reader
}

func newStage4Session(t *testing.T) *stage4Session {
	t.Helper()
	s := agenttest.OpenCompactionSession(t)
	return &stage4Session{
		CompactionSession: s,
		reader: New(Deps{
			Logger:   slog.New(slog.DiscardHandler),
			Messages: s.Messages,
			Queries:  s.Queries,
		}),
	}
}

func (s *stage4Session) Assemble(chat runreq.ChatRequest) Assembled {
	s.T.Helper()
	chat.BotID, chat.ChatID, chat.ThreadID = s.BotID, s.BotID, s.SessionID
	assembled, err := s.reader.Assemble(s.Ctx, ForChat(PurposeRun, chat, 100000))
	if err != nil {
		s.T.Fatalf("assembleContext: %v", err)
	}
	return assembled
}
