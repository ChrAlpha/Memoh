import { expect, it, vi } from 'vitest'
import { createRuntimeIntegration, type RuntimeIntegrationDeps } from './runtime-integration'
import { failureStage } from './send'
import { createEmptyRuntimeProjection } from './runtime-projection'
import type { RuntimeCurrentRunView } from '@/composables/api/useChat'

it('模式保存的所有帧均不刷新历史或改写上轮回复', () => {
  const applyRuntimeTranscript = vi.fn()
  const refreshCurrentSession = vi.fn()
  const resyncRuntimeTranscript = vi.fn()
  const deps = {
    bumpProjectionVersion: vi.fn(),
    chatViews: { getSession: () => ({ transcript: { applyRuntimeTranscript } }) },
    decisions: { observeRun: vi.fn() },
    refreshCurrentSession,
    resyncRuntimeTranscript,
  } as unknown as RuntimeIntegrationDeps
  const integration = createRuntimeIntegration(deps)
  const event = {
    type: 'runtime_snapshot' as const, session_id: 'session', epoch: 'epoch', seq: 1,
    snapshot: { bot_id: 'bot', session_id: 'session', epoch: 'epoch', seq: 1, updated_at: '' },
  }
  let previous = createEmptyRuntimeProjection()
  for (const status of ['admitting', 'running', 'finishing', 'completed'] as const) {
    const run: RuntimeCurrentRunView = {
      run_id: 'config', turn_id: 'config-turn', generation: '1', status,
      configuration_only: true, messages: [], started_at: '', updated_at: '',
    }
    const current = { ...createEmptyRuntimeProjection(), currentRunView: run }
    integration.handleProjection('bot', 'session', { previous, current, event })
    previous = current
  }
  integration.handleProjection('bot', 'session', { previous, current: createEmptyRuntimeProjection(), event })
  expect(applyRuntimeTranscript).not.toHaveBeenCalled()
  expect(refreshCurrentSession).not.toHaveBeenCalled()
  expect(resyncRuntimeTranscript).not.toHaveBeenCalled()
  expect(deps.bumpProjectionVersion).toHaveBeenCalledTimes(5)
})

it('does not treat a replacement error code as replacement output', () => {
  const assistant = {
    id: 'assistant', role: 'assistant' as const, messages: [], timestamp: '', streaming: false,
  }
  expect(failureStage(assistant, true, false)).toBe('startup')
  expect(failureStage(assistant, true, true)).toBe('stream')
})

// An error frame is a block source only for a send the server never accepted.
it('leaves an accepted run\'s failure to its run view', () => {
  const frame = { type: 'error' as const, invocation_id: 'inv', run_id: 'run-1', session_id: 'session', code: 'agent.response_timeout', message: 'late' }
  const setup = (runtimeRunId?: string, owned = true, replacesTurn = false) => {
    const assistantTurn = { id: 'assistant', role: 'assistant' as const, messages: [], timestamp: '', streaming: true, runtimeRunId }
    const deps = {
      assistantStreams: {
        invocationIdForEvent: () => 'inv',
        getAssistantStream: () => owned ? { botId: 'bot', sessionId: 'session', assistantTurn, replacesTurn } : undefined,
        rejectAssistantStream: vi.fn(),
      },
      chatViews: { getSession: vi.fn() },
      hasVisibleAssistantBlocks: () => false,
      removeTurnFromSession: vi.fn(),
      sendFailedMessage: () => 'failed',
    } as unknown as RuntimeIntegrationDeps
    createRuntimeIntegration(deps).handleWebSocketEvent(frame, 'bot')
    return { deps, assistantTurn }
  }

  const foreign = setup('run-1', false)
  expect(foreign.deps.chatViews.getSession).not.toHaveBeenCalled()
  expect(foreign.deps.assistantStreams.rejectAssistantStream).not.toHaveBeenCalled()

  // A retry or edit the server accepted fails at startup with no output yet;
  // history keeps its turn, so it stays for the run view to fill.
  for (const replacesTurn of [false, true]) {
    const accepted = setup('run-1', true, replacesTurn)
    expect(accepted.deps.removeTurnFromSession).not.toHaveBeenCalled()
    expect(accepted.assistantTurn.messages).toEqual([])
    expect(accepted.deps.assistantStreams.rejectAssistantStream).toHaveBeenCalledOnce()
  }

  const refused = setup(undefined)
  expect(refused.deps.removeTurnFromSession).toHaveBeenCalledOnce()
})
