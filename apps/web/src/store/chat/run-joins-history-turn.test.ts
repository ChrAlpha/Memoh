import { describe, expect, it, vi } from 'vitest'
import { ref } from 'vue'
import type { UITurn } from '@/composables/api/useChat.types'
import { createBackgroundTaskTracker } from './background-tasks'
import { projectRuntimeTranscript } from './runtime-projection'
import { createTranscriptController } from './transcript'

vi.mock('@/store/user', () => ({
  useUserStore: () => ({ userInfo: { id: 'user-1' } }),
}))

function makeTranscript() {
  const backgroundTasks = createBackgroundTaskTracker()
  return createTranscriptController({
    currentBotId: ref<string | null>('bot-1'),
    sessionId: ref<string | null>('session-1'),
    rememberBackgroundTask: backgroundTasks.rememberBackgroundTask,
    applyPendingBackgroundEventsToTool: backgroundTasks.applyPendingBackgroundEventsToTool,
    bumpFsChangedAtIfFsMutation: vi.fn(),
    fetchMessages: vi.fn().mockResolvedValue([]),
    locateMessage: vi.fn(),
    isTurnLive: () => false,
  })
}

// A run that answers an already-stored request row (a discuss batch, a
// continuation) carries that row's turn id. The history rows and the run's live
// view are then the same (turn_id, role) turn, shown once.
describe('a running run whose turn already has history rows', () => {
  it('renders each step once', () => {
    const transcript = makeTranscript()
    const history: UITurn[] = [
      { id: 'm1', turn_id: 'turn-1', turn_position: 3, role: 'user', text: 'ask', timestamp: '2026-10-21T08:00:00.000Z', platform: 'telegram' },
      {
        id: 'm2',
        turn_id: 'turn-1',
        turn_position: 3,
        role: 'assistant',
        messages: [{ id: 0, type: 'text', content: 'first step' }],
        timestamp: '2026-10-21T08:00:01.000Z',
      },
    ]
    transcript.replaceMessages(history, 'session-1')

    transcript.applyRuntimeTranscript(projectRuntimeTranscript({
      run_id: 'run-1',
      turn_id: 'turn-1',
      turn_position: 3,
      generation: 'generation-1',
      status: 'running',
      started_at: '2026-10-21T08:00:00.000Z',
      updated_at: '2026-10-21T08:00:05.000Z',
      messages: [{ id: 0, type: 'text', content: 'first step' }],
    }))

    expect(transcript.messages.filter(turn => turn.role === 'assistant')).toHaveLength(1)
    expect(transcript.messages.filter(turn => turn.role === 'user')).toHaveLength(1)
  })
})
