import { ref } from 'vue'
import { expect, it } from 'vitest'
import { createCommandEventRegistry } from './command-events'

it('完成事件归属原会话，过期或已关闭的命令不能覆盖面板', () => {
  const sessionId = ref<string | null>('a')
  const registry = createCommandEventRegistry({ currentBotId: ref('bot'), sessionId })
  const scope = registry.currentCommandScope()
  const completeA = registry.beginCommandEvent({ type: 'command_result', terminal: false, action_id: 'mcp' }, scope)
  const completeB = registry.beginCommandEvent({ type: 'command_result', terminal: false, action_id: 'status' }, scope)
  completeA({ type: 'command_error', terminal: true, code: 'late', message: 'late' })
  expect(registry.commandEventForScope(scope)?.action_id).toBe('status')

  sessionId.value = 'b'
  completeB({ type: 'command_result', terminal: true, action_id: 'status' })
  expect(registry.commandEventForScope(scope)?.terminal).toBe(true)
  expect(registry.commandEvent.value).toBeNull()
  registry.clearCommandEvent(scope)
  completeB({ type: 'command_result', terminal: true })
  expect(registry.commandEventForScope(scope)).toBeNull()
})

it('同会话的 operation 完成前拒绝重复提交并保留首次结果', () => {
  const registry = createCommandEventRegistry({ currentBotId: ref('bot'), sessionId: ref('a') })
  const scope = registry.currentCommandScope()
  const pending = { type: 'command_result' as const, terminal: false, action_id: 'compact' }
  const complete = registry.beginCommandEvent(pending, scope, true)
  const invocation = registry.commandEventForScope(scope)?.invocation_id

  expect(registry.beginCommandEvent(pending, { ...scope, composerScope: 'other-pane' }, true)).toBeNull()
  expect(registry.commandEventForScope(scope)?.invocation_id).toBe(invocation)
  complete!({ type: 'command_result', terminal: true, action_id: 'compact' })
  expect(registry.commandEventForScope(scope)).toMatchObject({ invocation_id: invocation, terminal: true })

  const retry = registry.beginCommandEvent(pending, scope, true)
  expect(retry).toBeTypeOf('function')
  retry!(null)
})

it('关闭面板不会释放进行中的 operation，失败完成后允许重试', () => {
  const registry = createCommandEventRegistry({ currentBotId: ref('bot'), sessionId: ref('a') })
  const scope = registry.currentCommandScope()
  const pending = { type: 'command_result' as const, terminal: false, action_id: 'compact' }
  const complete = registry.beginCommandEvent(pending, scope, true)
  registry.clearCommandEvent(scope)

  const read = registry.beginCommandEvent({ ...pending, action_id: 'status' }, scope)
  read({ type: 'command_result', terminal: true, action_id: 'status' })
  expect(registry.beginCommandEvent(pending, scope, true)).toBeNull()
  complete!({ type: 'command_error', terminal: true, code: 'runtime_control.failed', message: 'failed' })
  expect(registry.commandEventForScope(scope)?.action_id).toBe('status')

  const retry = registry.beginCommandEvent(pending, scope, true)
  expect(retry).toBeTypeOf('function')
  complete!(null)
  expect(registry.beginCommandEvent(pending, scope, true)).toBeNull()
  retry!(null)
})

it('operation 只占用原 bot 和会话，切换后完成不会解锁其他请求', () => {
  const sessionId = ref('a')
  const registry = createCommandEventRegistry({ currentBotId: ref('bot'), sessionId })
  const scopeA = registry.currentCommandScope()
  const pending = { type: 'command_result' as const, terminal: false, action_id: 'compact' }
  const completeA = registry.beginCommandEvent(pending, scopeA, true)
  sessionId.value = 'b'
  const scopeB = registry.currentCommandScope()
  const completeB = registry.beginCommandEvent(pending, scopeB, true)
  const scopeOtherBot = { ...scopeA, botId: 'other-bot' }
  const completeOtherBot = registry.beginCommandEvent(pending, scopeOtherBot, true)

  expect(completeB).toBeTypeOf('function')
  expect(completeOtherBot).toBeTypeOf('function')
  completeA!(null)
  expect(registry.beginCommandEvent(pending, scopeB, true)).toBeNull()
  expect(registry.beginCommandEvent(pending, scopeOtherBot, true)).toBeNull()
  const nextA = registry.beginCommandEvent(pending, scopeA, true)
  expect(nextA).toBeTypeOf('function')
  nextA!(null)
  completeB!(null)
  completeOtherBot!(null)
})
