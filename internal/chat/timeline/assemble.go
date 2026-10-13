package timeline

// BuildRC folds a session's persisted events into the rendered context. It is
// a pure function of its inputs: the same events and params always yield the
// same result, with no cache in between.
func BuildRC(events []CanonicalEvent, params RenderParams) RenderedContext {
	ic := NewEmptyIC("")
	for _, event := range events {
		reduceInPlace(&ic, event)
	}
	return Render(ic, params)
}
