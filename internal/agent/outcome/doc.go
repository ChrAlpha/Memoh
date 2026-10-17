// Package outcome decides and records how an agent run ended: the end it is
// named, the public failure code behind it, the stream events that report it
// and the one result record written per run. The functions are pure; the
// drivers that execute runs call them.
package outcome
