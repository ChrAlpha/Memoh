// Package agentapi is the contract between the agent and the code that calls
// it: the types and errors that cross the boundary of the root package
// (internal/agent), each defined once. Handlers and the composition root name
// these; the orchestration packages that produce and consume them import this
// package, never the other way around.
//
// It is a leaf: the standard library and a few foundation or public types that
// the contract itself is made of. Anything that needs more than that, such as
// error translation tables or the Deps ports, lives in a subpackage.
package agentapi
