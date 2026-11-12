// Package readers holds the remaining store-backed readers the capability
// services require.
//
// Every reader is deliberately thin. It maps one scoped read request to
// store.Executor.Read or store.Scanner.Query over the declared schema and
// decodes the stored columns back into the capability value. It never issues
// SQL, never imports a concrete persistence adapter and never duplicates
// capability policy: authorization, state transitions and domain validation
// stay in the capability that owns them.
//
// # Ports
//
//	task.ExecutionReader           ExecutionReader.Get
//	log.ExecutionOwnershipReader   OwnershipReader.Ownership
//	event.CursorPort               CursorReader.Cursor
//	event.CredentialValidity       CredentialValidity.ValidUntil
//	token.ReceiptLookup            ReceiptLookup.Lookup
//	verify.BindingReader           BindingReader.ReadBinding
//	verify.VerificationReader      VerificationReader.ReadVerification
//
// # Stop evidence
//
// ExecutionReader derives the task service's predecessor stop evidence from the
// rows the Kernel already persisted. Cleanup complete is affirmative evidence
// that every owned resource was stopped or proven never started. While cleanup
// is still owed, the persisted stop evidence of the execution's possibly
// started container actions is used instead: only an explicit confirmed barrier
// is confirmed. An absent cleanup row, a pending cleanup and a pending or
// absent stop barrier all report unconfirmed evidence, so a task never
// dispatches on an inferred or fabricated stop.
//
// # Scope
//
// Every read is scoped. A row owned by another tenant or project is reported as
// not_found by the adapter, never returned. A missing row is not_found; an
// unreadable stored value is internal_failure rather than a silently empty
// value.
//
// # Verification records
//
// VerificationReader reads one verification record inside the report command's
// owner scope and rebuilds only the fields the platform record adapter persists.
// A record outside the scope or an unknown identity is uniformly not_found.
// The actor's roles, enablement and version are not durable verification
// columns, so they are not re-read: the record is a fact about an accepted
// effect, never current authority.
package readers
