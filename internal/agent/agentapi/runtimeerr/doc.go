// Package runtimeerr translates the failures of external agent runtimes into
// their public errors. Handlers use it on paths that reach a runtime driver
// without the agent service; the orchestration packages use the same tables so
// every transport reports a failure with the same code.
package runtimeerr
