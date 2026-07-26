interface CompactionModelLike {
  provider_id?: string | null
  config?: {
    context_window?: number | null
  } | null
}

interface CompactionProviderLike {
  id?: string | null
  client_type?: string
}

export function filterCompactionModels<T extends CompactionModelLike>(
  models: readonly T[],
  providers: readonly CompactionProviderLike[],
): T[] {
  const unsupportedProviderIds = new Set(
    providers
      .filter(provider => provider.client_type === 'openai-codex')
      .map(provider => provider.id)
      .filter((id): id is string => Boolean(id)),
  )

  return models.filter((model) => {
    if (model.provider_id && unsupportedProviderIds.has(model.provider_id)) {
      return false
    }
    // The resolver fails closed on models without a declared context window
    // (the summary budget derives from it), so don't offer them.
    return (model.config?.context_window ?? 0) > 0
  })
}
