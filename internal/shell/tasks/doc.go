// Package tasks is the shell's task tracker (C3 of the pipeline-runtime
// simplification plan, work management).
//
// A task is a unit of work that selects one project pipeline, by its
// project-unique name resolved to the immutable pipeline ID at creation, and
// exposes the progress of that pipeline's current run. The run, step and wait
// records of the kernel own execution truth; the task only projects it.
//
// # Use cases
//
// Service offers task create, get, list, edit, start, rerun, inbox and resolve,
// plus the projection consumer (Apply) and its rebuild. There is no transition,
// park, blocking-request or resolution-marker operation, and Task has no field
// that could hold such a state: a state machine under another name is what this
// package exists to avoid (shape_test.go pins it).
//
// # Projection
//
// Project derives a Progress (status, current step, waiting assignment, result)
// from a runtime.RunView. Every observation carries ViewVersion, the sum of the
// versions of the run's records, and the Store applies it through one guard
// (advance): an observation of a run that is not the current run, or older than
// the stored one, is stale and changes nothing; an equal one is a replay. An older
// run therefore cannot overwrite a newer view. Selecting a different run is Start
// and Rerun's job only, so a rerun creates a new run, keeps every row of the
// earlier one and restarts the run version. Rebuild recomputes the projection
// from the records through the same function.
//
// # Inbox and resolution
//
// Inbox lists the tasks whose current open wait is assigned to a role the caller
// holds now, verified against the wait's own record. Resolve verifies the caller's
// current authority and the exact run, step and wait of the task, and submits one
// kernel signal (DecideSignal) under the caller's key; the kernel records the
// actor, outcome and answer immutably. Resolution never mutates the task.
//
// # Adapters
//
// Memory and Postgres implement Store and run the same service scenarios (the
// PostgreSQL ones outside the unit layer). Run ownership is runs.task_id, which
// the kernel's admission does not yet write; see Postgres.
package tasks
