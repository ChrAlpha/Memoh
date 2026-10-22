// Package runreq holds the value types and pure helpers every stage of an
// agent run shares: the chat request and its resolved context, model-message
// conversion and repair, the ports a run depends on, and the team-scope
// provider. It has no state and imports nothing from the agent packages that
// run turns, so every other agent package can depend on it. ResolvedContext
// lives here and not in runconfig, which builds it, so that the packages below
// runconfig (persist, which runconfig imports) can take it as an argument.
package runreq
