import { describe, expect, it } from 'vitest'
import { filterCompactionModels } from './compaction-models'

describe('filterCompactionModels', () => {
  it('excludes models owned by openai-codex providers', () => {
    const models = [
      { id: 'codex-model', provider_id: 'codex-provider', type: 'chat' as const },
      { id: 'chat-model', provider_id: 'chat-provider', type: 'chat' as const },
      { id: 'orphan-model', provider_id: 'missing-provider', type: 'chat' as const },
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
