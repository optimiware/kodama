// Package work holds the thin store-backed adapters for the task, inbox,
// pipeline and trigger ports declared by internal/shell/{task,inbox,pipeline,
// trigger}.
//
// Every adapter maps exactly one capability-built commit unit to exactly one
// store.Unit and calls store.Executor.Execute; reads map to Executor.Read,
// Scanner.Query or, for the one deliberately cross-scope trigger event read,
// store.Controller.Scan. The capability decides authorization, state
// transitions and the exact admitted values; the adapter only persists the
// facts the unit carries and never re-derives policy, parses YAML or issues SQL.
//
// Port -> table mapping (columns from store.SchemaDesign):
//
//	task.CreatePort.Create   tasks insert; optional nested start rows
//	task.StartPort.Start     executions + actions insert, tasks update
//	task.AskPort.Ask         tasks question columns/state, executions cancel,
//	                         action staged-output discard
//	task.ReplyPort.Reply     tasks reopen/clear question, executions + actions
//	                         insert (one fresh queued execution)
//	task.CancelPort.Cancel   tasks cancel, executions cancel + credential clear,
//	                         action staged-output discard
//	task.EditPort.Edit       tasks content/lifecycle columns, task_messages
//	                         append for a comment
//	task.ProgressPort.Apply  tasks state=done at the current execution guard
//	task.Reader              tasks + task_messages + executions (history)
//	inbox.QueryPort.Query    tasks + executions + actions (read-only merge)
//	pipeline.ChangePort      pipelines insert/update/delete with the exact YAML
//	                         bytes and the push_triggers projection
//	pipeline.Reader          pipelines rows incl. push_triggers
//	trigger.CandidateReader  pipelines + resource_bindings (push candidates)
//	trigger.SelectPort       receipts incomplete insert carrying the in-progress
//	                         selection payload + audit/event intent
//	trigger.StartPort        executions + actions insert (or refusal) and the
//	                         common receipt compare-and-set completion
//	trigger.EventReader      events via store.Controller (cross-scope)
//	trigger.SelectionReader  receipts (the common receipt is the durable
//	                         selection; no separate selection table exists)
//	trigger.CompletionPort   event_consumptions marker (id = event id,
//	                         consumer "trigger", version = sequence)
//
// Versioned rows use ExpectVersion; the tasks row has no single version column,
// so it is updated with an explicit Expect guard on the exact
// content_version/lifecycle_version/question_version/current_execution_id the
// unit carries. Every unit keeps its receipt, audit rows and event publication
// intent in the same store.Unit, so Execute commits them together and a stale
// guard leaves no partial row.
//
// An incomplete receipt may carry an in-progress result, so Select persists the
// first selection payload (exact YAML, service, declared inputs, event
// fingerprint and pipeline identity) before admission and Start overwrites it
// with the final result under the compare-and-set. Lookup, Unfinished and an
// equal-key Select replay therefore return the FIRST exact bytes even after the
// definition was edited.
//
// Known limitations:
//
//   - task.ProgressUnit carries no receipt, so ProgressResult.Replayed can never
//     be true; a retried stale progress is a version_conflict.
//   - AskUnit carries no asked-by identity or time; the adapter uses the
//     receipt's principal and commit time for tasks.question_asked_by/at.
//   - task_messages has an id column but task.Message has no identity; a
//     comment edit maps the message row id to its receipt id.
//   - actions has no created_at, so an inbox request item's At is the action
//     deadline (zero when unset).
//   - A new pipeline apply carries no identity; the adapter injects one.
//
// This package is a unit-level adapter. It is not evidence of PostgreSQL RLS,
// isolation or constraint behavior.
package work
