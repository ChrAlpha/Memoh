import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { client } from '@memohai/sdk/client'
import type {
  RuntimeCurrentRunView,
  UIRuntimeDeltaEvent,
  UIRuntimeEvent,
  UIRuntimeSnapshotEvent,
  UIStreamEvent,
} from '@/composables/api/useChat'
import { connectWebSocket } from '@/composables/api/useChat.ws'
import { createRuntimeClient, type RuntimeProjectionChange } from './runtime-client'

// The frames are produced by the Go server (internal/handlers/ws_frames_golden_test.go)
// and read here from the same files, so a server-side shape change that this client
// cannot parse shows up in both suites.
const goldenDir = fileURLToPath(new URL('../../../../../internal/handlers/testdata/ws-frames', import.meta.url))

// Frames are asserted field by field, so they stay loosely typed here.
interface GoldenFrame { [key: string]: any } // eslint-disable-line @typescript-eslint/no-explicit-any
const frames = new Map<string, GoldenFrame>()
for (const file of readdirSync(goldenDir).filter(name => name.endsWith('.json')).sort()) {
  frames.set(file.replace(/\.json$/, ''), JSON.parse(readFileSync(`${goldenDir}/${file}`, 'utf8')))
}

function frame(name: string): GoldenFrame {
  const found = frames.get(name)
  if (!found) throw new Error(`missing golden frame ${name}`)
  return found
}

class MockWebSocket {
  static CONNECTING = 0
  static OPEN = 1
  static CLOSING = 2
  static CLOSED = 3
  static instances: MockWebSocket[] = []

  readyState = MockWebSocket.CONNECTING
  onopen: (() => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null

  readonly url: string
  constructor(url: string) {
    this.url = url
    MockWebSocket.instances.push(this)
  }

  send() {}
  close() {
    this.readyState = MockWebSocket.CLOSED
  }
}

const SESSION = '22222222-2222-2222-2222-222222222222'
const RUN = '33333333-3333-3333-3333-333333333333'
const TURN = '44444444-4444-4444-4444-444444444444'

function harness() {
  const sent: unknown[] = []
  const changes: RuntimeProjectionChange[] = []
  const runtime = createRuntimeClient({
    send: message => sent.push(message),
    onProjection: (_sid, change) => changes.push(change),
    scheduleFrame: callback => callback(),
  })
  runtime.subscribe(SESSION)
  runtime.onConnected()
  return { runtime, sent, changes }
}

function runView(name: string): RuntimeCurrentRunView {
  const run = (frame(name) as UIRuntimeSnapshotEvent).snapshot.current_run_view
  if (!run) throw new Error(`${name} has no run view`)
  return run
}

function applyOnRichSnapshot(deltaName: string) {
  const { runtime, changes } = harness()
  runtime.handleEvent(frame('runtime_snapshot_running_rich') as UIRuntimeEvent)
  runtime.handleEvent(frame(deltaName) as UIRuntimeEvent)
  const projection = runtime.projection(SESSION)!
  return { projection, run: projection.currentRunView!, changes }
}

describe('ws frame goldens', () => {
  beforeEach(() => {
    MockWebSocket.instances = []
    vi.unstubAllGlobals()
    client.setConfig({ baseUrl: '/api' })
    vi.stubGlobal('window', { location: { protocol: 'http:', host: 'localhost:8082' } })
    vi.stubGlobal('localStorage', { getItem: vi.fn(() => '') })
    vi.stubGlobal('WebSocket', MockWebSocket)
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('covers every frame type the socket accepts', () => {
    const types = new Set([...frames.values()].map(f => f.type))
    expect([...types].sort()).toEqual([
      'command_error',
      'command_result',
      'control_ack',
      'error',
      'model_preference_settled',
      'run_accepted',
      'run_rejected',
      'runtime_delta',
      'runtime_dropped',
      'runtime_snapshot',
      'session_created',
    ])
  })

  it.each([...frames.keys()])('delivers %s through the socket unchanged', (name) => {
    const received: UIStreamEvent[] = []
    connectWebSocket('bot-1', event => received.push(event))
    const socket = MockWebSocket.instances[0]!
    socket.onmessage?.({ data: JSON.stringify(frame(name)) })
    expect(received).toEqual([frame(name)])
  })

  describe('non-runtime frames', () => {
    it('run_accepted names the run, turn and position', () => {
      expect(frame('run_accepted')).toMatchObject({
        run_id: RUN, invocation_id: 'inv-1', session_id: SESSION, turn_id: TURN, turn_position: 5, epoch: 'epoch-1', seq: 6,
      })
      expect(frame('run_accepted').duplicate).toBeUndefined()
      expect(frame('run_accepted_duplicate').duplicate).toBe(true)
    })

    it('run_rejected and error carry a code and message', () => {
      expect(frame('run_rejected')).toMatchObject({ invocation_id: 'inv-1', session_id: SESSION, code: 'session_runtime.session_busy' })
      expect(typeof frame('run_rejected').message).toBe('string')
      expect(frame('error')).toMatchObject({ invocation_id: 'inv-1', code: 'session_runtime.session_busy', args: { k: 'v' }, fault: 'client' })
      expect(frame('error_run')).toMatchObject({ run_id: RUN, fault: 'dependency' })
      expect(frame('error_run').invocation_id).toBeUndefined()
    })

    it('control_ack keeps the key the client acknowledges by', () => {
      for (const name of ['control_ack_applied', 'control_ack_not_applied', 'control_ack_failed']) {
        expect(frame(name)).toMatchObject({ run_id: RUN, session_id: SESSION })
        expect(frame(name).control_id).toMatch(/^ctl-/)
      }
      expect(frame('control_ack_applied').applied).toBe(true)
      // The server omits a false `applied`; the client must read absence as false.
      expect('applied' in frame('control_ack_not_applied')).toBe(false)
      expect(frame('control_ack_failed').code).toBe('session_runtime.command_owner_unavailable')
    })

    it('command frames are terminal and keyed by invocation', () => {
      expect(frame('command_result')).toMatchObject({ invocation_id: 'inv-2', terminal: true, result: { kind: 'list' } })
      expect(frame('command_result').result.items[0]).toMatchObject({ id: 's1', title: 'S1' })
      expect(frame('command_error')).toMatchObject({ invocation_id: 'inv-2', terminal: true, code: 'slash.unknown_command' })
    })
  })

  describe('runtime snapshots', () => {
    it('an empty snapshot projects no run', () => {
      const { runtime } = harness()
      runtime.handleEvent(frame('runtime_snapshot_empty') as UIRuntimeEvent)
      const projection = runtime.projection(SESSION)!
      expect(projection).toMatchObject({ botId: '11111111-1111-1111-1111-111111111111', epoch: 'epoch-1', seq: 7, currentRunView: null })
      expect(projection.transcript.turns).toEqual([])
    })

    it('a rich snapshot projects the run identity, turns and every message block', () => {
      const { runtime } = harness()
      runtime.handleEvent(frame('runtime_snapshot_running_rich') as UIRuntimeEvent)
      const { transcript, currentRunView } = runtime.projection(SESSION)!
      expect(transcript).toMatchObject({
        runId: RUN, turnId: TURN, turnPosition: 5, invocationId: 'inv-1', status: 'running', streaming: true,
        operation: { kind: 'edit', replace_from_message_id: 'm1' },
      })
      const user = transcript.turns.find(t => t.role === 'user')
      expect(user).toMatchObject({ turn_id: TURN, text: 'do it', platform: 'web' })
      // Steers split the run's blocks across assistant segments. An error
      // message inside the view is not a block source: a run's error block
      // comes from its error_code, which this running snapshot has none of.
      const assistants = transcript.turns.filter(t => t.role === 'assistant') as Array<{ turn_id: string, messages: Array<{ id: number, type: string }> }>
      expect(assistants.every(t => typeof t.turn_id === 'string')).toBe(true)
      expect(assistants.flatMap(t => t.messages).map(m => m.type)).toEqual([
        'reasoning', 'text', 'tool', 'tool', 'tool', 'attachments', 'notice', 'command', 'status',
      ])
      expect(currentRunView).toMatchObject({
        retry: { attempt: 2, max_attempt: 5, delay_ms: 4000, reason: 'rate_limited' },
      })
      expect(currentRunView!.steer_turns?.map(s => s.status)).toEqual(['claimed', 'applied'])
    })

    it.each(['admitting', 'running', 'waiting_decision', 'aborting', 'finishing', 'completed', 'aborted', 'errored', 'lost'])(
      'status %s survives projection',
      (status) => {
        const { runtime } = harness()
        runtime.handleEvent(frame(`runtime_snapshot_status_${status}`) as UIRuntimeEvent)
        expect(runtime.projection(SESSION)!.transcript.status).toBe(status)
      },
    )

    it('a waiting_decision snapshot keeps the approval and ask_user blocks', () => {
      const run = runView('runtime_snapshot_waiting_decision')
      const tools = run.messages as unknown as GoldenFrame[]
      expect(tools[0]!.approval).toMatchObject({ approval_id: 'ap-1', status: 'pending', pending_alive: true, selected_option_id: 'o1' })
      expect(tools[0]!.approval.options.map((o: { kind: string }) => o.kind)).toEqual(['allow_once', 'reject_once'])
      expect(tools[1]!.user_input).toMatchObject({ user_input_id: 'ui-1', status: 'pending', can_respond: true })
      expect(tools[1]!.user_input.questions[0]).toMatchObject({ kind: 'single_select', required: true })
      const { runtime } = harness()
      runtime.handleEvent(frame('runtime_snapshot_waiting_decision') as UIRuntimeEvent)
      const projection = runtime.projection(SESSION)!
      expect(projection.transcript.status).toBe('waiting_decision')
    })

    it('an errored snapshot carries error_code and an error block', () => {
      const { runtime } = harness()
      runtime.handleEvent(frame('runtime_snapshot_errored') as UIRuntimeEvent)
      const projection = runtime.projection(SESSION)!
      expect(projection.currentRunView).toMatchObject({ status: 'errored', error_code: 'agent.provider_failed' })
      expect(projection.currentRunView!.messages[0]).toMatchObject({ type: 'error', code: 'session_runtime.session_busy' })
    })

    it('a finishing snapshot keeps the proposed terminal status', () => {
      expect(runView('runtime_snapshot_finishing')).toMatchObject({ status: 'finishing', proposed_terminal_status: 'completed' })
      expect(typeof runView('runtime_snapshot_finishing').finish_proposed_at).toBe('string')
    })

    it.each(['rate_limited', 'server_error', 'stream_incomplete', 'network'])('retry reason %s is kept', (reason) => {
      expect(runView(`runtime_snapshot_retry_${reason}`).retry).toMatchObject({ reason, attempt: 1, max_attempt: 3, delay_ms: 1000 })
    })

    it('retry without a reason and configuration-only runs parse', () => {
      expect(runView('runtime_snapshot_retry_without_reason').retry?.reason).toBeUndefined()
      expect(runView('runtime_snapshot_configuration_only').configuration_only).toBe(true)
      expect(runView('runtime_snapshot_operation_retry').operation).toMatchObject({ kind: 'retry', replace_from_message_id: 'm1' })
    })

    it('run views carry the legacy request_user_turn the older clients read', () => {
      const run = runView('runtime_snapshot_running_rich') as RuntimeCurrentRunView & { user_turns?: unknown[] }
      expect(run.request_user_turn).toMatchObject({ turn_id: TURN, role: 'user' })
      expect(run.user_turns).toHaveLength(1)
    })
  })

  describe('runtime deltas', () => {
    it('current_run_view replaces the run', () => {
      const { run, projection } = applyOnRichSnapshot('runtime_delta_current_run_view')
      expect(projection.seq).toBe(8)
      expect(run).toMatchObject({ run_id: RUN, status: 'running' })
      expect(run.messages).toHaveLength(10)
    })

    it('run patch updates status, error_code and lease', () => {
      const { run, projection } = applyOnRichSnapshot('runtime_delta_run_patch')
      expect(run).toMatchObject({ status: 'errored', error_code: 'agent.provider_failed', owner_lease_expires_at: '2026-10-09T08:00:33Z' })
      expect(projection.transcript.status).toBe('errored')
    })

    it('run patch sets and clears retry', () => {
      expect(applyOnRichSnapshot('runtime_delta_run_patch_retry').run.retry).toMatchObject({ attempt: 2, reason: 'network' })
      expect(applyOnRichSnapshot('runtime_delta_run_patch_clear_retry').run.retry).toBeUndefined()
    })

    it('message appends extend text and reasoning by id', () => {
      const { run } = applyOnRichSnapshot('runtime_delta_message_appends')
      const byId = new Map(run.messages.map(m => [m.id, m as { content?: string }]))
      expect(byId.get(1)!.content).toBe('thinking more')
      expect(byId.get(2)!.content).toBe('hello world')
    })

    it('progress appends extend the tool progress', () => {
      const { run } = applyOnRichSnapshot('runtime_delta_progress_appends')
      const tool = run.messages.find(m => m.id === 3) as { progress?: unknown[] }
      expect(tool.progress?.at(-1)).toEqual({ pct: 80 })
      expect(tool.progress!.length).toBeGreaterThan(2)
    })

    it('message upserts replace blocks by id', () => {
      const { run } = applyOnRichSnapshot('runtime_delta_message_upserts')
      expect(run.messages.map(m => m.id)).toEqual([1, 2, 3, 4, 5, 6, 7, 8, 9, 10])
    })

    it('user turn upserts reach the transcript', () => {
      const { projection } = applyOnRichSnapshot('runtime_delta_user_turn_upserts')
      const ids = projection.transcript.turns.map(t => t.turn_id)
      expect(ids).toContain('66666666-6666-6666-6666-666666666666')
      expect(ids).toContain('88888888-8888-8888-8888-888888888888')
    })

    it('steer upserts and removals update the steer set', () => {
      const { run } = applyOnRichSnapshot('runtime_delta_steer_upserts_removals')
      const steers = run.steer_turns ?? []
      expect(steers.find(s => s.item_id === '55555555-5555-5555-5555-555555555555')?.status).toBe('applied')
      expect(steers.some(s => s.item_id === '77777777-7777-7777-7777-777777777777')).toBe(false)
    })

    it('reset_messages clears the message stream', () => {
      expect(applyOnRichSnapshot('runtime_delta_reset_messages').run.messages).toEqual([])
    })

    it('an empty delta only advances the cursor', () => {
      const { projection, run } = applyOnRichSnapshot('runtime_delta_empty')
      expect(projection.seq).toBe(8)
      expect(run.messages).toHaveLength(10)
    })

    it('every delta golden is sequenced right after the snapshot golden', () => {
      for (const [name, f] of frames) {
        if (f.type !== 'runtime_delta') continue
        expect(f.seq, name).toBe(8)
        expect((f as UIRuntimeDeltaEvent).epoch, name).toBe('epoch-1')
      }
    })

    it('runtime_dropped makes the client resubscribe from its cursor', () => {
      const { runtime, sent } = harness()
      runtime.handleEvent(frame('runtime_snapshot_running_rich') as UIRuntimeEvent)
      runtime.handleEvent(frame('runtime_dropped') as UIRuntimeEvent)
      expect(sent.at(-1)).toEqual({ type: 'runtime_subscribe', session_id: SESSION, cursor: { epoch: 'epoch-1', seq: 7 } })
    })
  })
})
