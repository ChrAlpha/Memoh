import { describe, expect, it } from 'vitest'
import { filterCompactionModels } from './compaction-models'

describe('filterCompactionModels', () => {
  it('offers only models the resolver can accept', () => {
    const models = [
      { id: 'codex-model', provider_id: 'codex-provider', enable: true, config: { context_window: 200000 } },
      { id: 'chat-model', provider_id: 'chat-provider', enable: true, config: { context_window: 200000 } },
      { id: 'disabled-model', provider_id: 'chat-provider', enable: false, config: { context_window: 200000 } },
      { id: 'windowless-model', provider_id: 'chat-provider', enable: true, config: {} },
      { id: 'speech-model', provider_id: 'speech-provider', enable: true, config: { context_window: 16000 } },
      { id: 'off-provider-model', provider_id: 'off-provider', enable: true, config: { context_window: 32000 } },
      { id: 'orphan-model', provider_id: 'missing-provider', enable: true, config: { context_window: 8192 } },
    ]
    const providers = [
      { id: 'codex-provider', client_type: 'openai-codex', enable: true },
      { id: 'chat-provider', client_type: 'openai-responses', enable: true },
      { id: 'speech-provider', client_type: 'edge-speech', enable: true },
      { id: 'off-provider', client_type: 'openai-completions', enable: false },
    ]

    expect(filterCompactionModels(models, providers).map(model => model.id)).toEqual([
      'chat-model',
      'orphan-model',
    ])
  })
})
