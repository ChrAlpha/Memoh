import { describe, expect, it } from 'vitest'
import { filterCompactionModels } from './compaction-models'

describe('filterCompactionModels', () => {
  it('offers only models the resolver can accept', () => {
    const models = [
      { id: 'codex-model', provider_id: 'codex-provider', type: 'chat' as const, config: { context_window: 200000 } },
      { id: 'chat-model', provider_id: 'chat-provider', type: 'chat' as const, config: { context_window: 200000 } },
      { id: 'windowless-model', provider_id: 'chat-provider', type: 'chat' as const, config: {} },
      { id: 'orphan-model', provider_id: 'missing-provider', type: 'chat' as const, config: { context_window: 8192 } },
    ]
    const providers = [
      { id: 'codex-provider', client_type: 'openai-codex' },
      { id: 'chat-provider', client_type: 'openai-responses' },
    ]

    expect(filterCompactionModels(models, providers).map(model => model.id)).toEqual([
      'chat-model',
      'orphan-model',
    ])
  })
})
