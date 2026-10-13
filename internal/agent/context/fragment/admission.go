package contextfrag

// DefaultContextFallbackTokens is the context budget used when a model has no
// configured context window and no explicit `[agent] context_fallback_tokens`
// is set (CM-ADM-001). A model with a known window uses that window as its
// budget. The value is denominated in this ledger's token estimate, so a real
// tokenizer swap revisits it together with EstimateBytesPerToken.
const DefaultContextFallbackTokens = 200_000
