interface CompactionModelLike {
  provider_id?: string | null
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

  return models.filter(model => !model.provider_id || !unsupportedProviderIds.has(model.provider_id))
}
