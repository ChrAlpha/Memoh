interface CompactionModelLike {
  provider_id?: string | null
  enable?: boolean
  config?: {
    context_window?: number | null
  } | null
}

interface CompactionProviderLike {
  id?: string | null
  client_type?: string
  enable?: boolean
}

const NON_TEXT_CLIENT_SUFFIXES = ['-speech', '-transcription', '-video']

function providerCanSummarize(provider: CompactionProviderLike): boolean {
  if (provider.enable === false) {
    return false
  }
  const clientType = provider.client_type ?? ''
  // openai-codex ignores output caps, so a summary cannot be bounded there.
  if (clientType === 'openai-codex') {
    return false
  }
  return !NON_TEXT_CLIENT_SUFFIXES.some(suffix => clientType.endsWith(suffix))
}

export function filterCompactionModels<T extends CompactionModelLike>(
  models: readonly T[],
  providers: readonly CompactionProviderLike[],
): T[] {
  const eligibleProviderIds = new Set(
    providers
      .filter(providerCanSummarize)
      .map(provider => provider.id)
      .filter((id): id is string => Boolean(id)),
  )
  const knownProviderIds = new Set(
    providers.map(provider => provider.id).filter((id): id is string => Boolean(id)),
  )

  return models.filter((model) => {
    if (model.enable === false) {
      return false
    }
    if (model.provider_id && knownProviderIds.has(model.provider_id) && !eligibleProviderIds.has(model.provider_id)) {
      return false
    }
    // The resolver fails closed on models without a declared context window
    // (the summary budget derives from it), so don't offer them.
    return (model.config?.context_window ?? 0) > 0
  })
}
