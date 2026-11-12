// Package process implements the development-only host Process runtime
// adapter behind the declared runtime.Client seam.
//
// The adapter owns preparation materialization, one stable process group per
// persisted effect identity, observation, bounded logs, process-group stop,
// identity-scoped cleanup and credential replacement. Every operating-system
// effect is injected through the Executor, Observer and Files seams so unit
// tests drive deterministic in-memory fakes; this package deliberately does
// not launch a real process. Real shell/process semantics belong to the
// production host boundary behind those seams.
//
// The Process family makes no container, retained-workspace, network-isolation
// or Job-deletion claim. Absence and unknown outcomes never prove
// nonexecution, and a stop acknowledgment is never stop confirmation.
package process
