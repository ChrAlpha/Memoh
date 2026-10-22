// Package drivetest holds the doubles that exercise the run driver: a scripted
// executor, a recording delivery end and the spec of a scheduled run. It is
// imported by tests only; the doubles that do not need the driver live in
// agenttest, which the packages below the driver can import.
package drivetest
