// Package fault defines the stable error classes and public codes shared by
// Kernel, Shell and Userland contracts.
//
// A fault carries a public class and code plus a public message. The private
// cause stays reachable through errors.Is/errors.As for diagnostics, but it is
// never rendered by Error() or Message() so credential material in an adapter
// error cannot leak into logs, events or receipts.
package fault

import (
	"errors"
)

// Class is a stable, public error category.
type Class string

// Code is a stable, public error code. REST and MCP edges map it consistently.
type Code string

const (
	// InvalidInput reports a refused request shape or value.
	InvalidInput Class = "invalid_input"
	// Unauthenticated reports a missing or unverifiable principal.
	Unauthenticated Class = "unauthenticated"
	// Forbidden reports an authenticated principal without authority.
	Forbidden Class = "forbidden"
	// NotFound reports a missing entity visible to the caller's scope.
	NotFound Class = "not_found"
	// VersionConflict reports a stale expected version.
	VersionConflict Class = "version_conflict"
	// ChangedIdempotencyContent reports a retry whose content changed.
	ChangedIdempotencyContent Class = "changed_idempotency_content"
	// DeadlineExpired reports work refused at or past its deadline.
	DeadlineExpired Class = "deadline_expired"
	// UnsupportedCapability reports admitted work the selected adapter cannot run.
	UnsupportedCapability Class = "unsupported_capability"
	// DependencyUnavailable reports an unavailable external dependency.
	DependencyUnavailable Class = "dependency_unavailable"
	// UncertainExternalEffect reports an effect whose outcome is unknown.
	UncertainExternalEffect Class = "uncertain_external_effect"
	// InternalFailure reports an unexpected internal failure.
	InternalFailure Class = "internal_failure"
)

const (
	CodeInvalidInput              Code = "invalid_input"
	CodeUnauthenticated           Code = "unauthenticated"
	CodeForbidden                 Code = "forbidden"
	CodeNotFound                  Code = "not_found"
	CodeVersionConflict           Code = "version_conflict"
	CodeChangedIdempotencyContent Code = "changed_idempotency_content"
	CodeDeadlineExpired           Code = "deadline_expired"
	CodeUnsupportedCapability     Code = "unsupported_capability"
	CodeDependencyUnavailable     Code = "dependency_unavailable"
	CodeUncertainExternalEffect   Code = "uncertain_external_effect"
	CodeInternalFailure           Code = "internal_failure"
)

type classCode struct {
	class Class
	code  Code
}

// classCodes is the single ordered class/code table. Classes(), Codes(),
// CodeFor and ClassFor all derive from it so the mapping stays one-to-one.
var classCodes = []classCode{
	{InvalidInput, CodeInvalidInput},
	{Unauthenticated, CodeUnauthenticated},
	{Forbidden, CodeForbidden},
	{NotFound, CodeNotFound},
	{VersionConflict, CodeVersionConflict},
	{ChangedIdempotencyContent, CodeChangedIdempotencyContent},
	{DeadlineExpired, CodeDeadlineExpired},
	{UnsupportedCapability, CodeUnsupportedCapability},
	{DependencyUnavailable, CodeDependencyUnavailable},
	{UncertainExternalEffect, CodeUncertainExternalEffect},
	{InternalFailure, CodeInternalFailure},
}

// Classes returns every stable class in declaration order.
func Classes() []Class {
	classes := make([]Class, len(classCodes))
	for index, entry := range classCodes {
		classes[index] = entry.class
	}
	return classes
}

// Codes returns every stable public code in declaration order.
func Codes() []Code {
	codes := make([]Code, len(classCodes))
	for index, entry := range classCodes {
		codes[index] = entry.code
	}
	return codes
}

// Valid reports whether the class is one of the declared classes.
func (class Class) Valid() bool {
	_, ok := CodeFor(class)
	return ok
}

// Valid reports whether the code is one of the declared public codes.
func (code Code) Valid() bool {
	_, ok := ClassFor(code)
	return ok
}

// CodeFor returns the public code for a class.
func CodeFor(class Class) (Code, bool) {
	for _, entry := range classCodes {
		if entry.class == class {
			return entry.code, true
		}
	}
	return "", false
}

// ClassFor returns the class for a public code.
func ClassFor(code Code) (Class, bool) {
	for _, entry := range classCodes {
		if entry.code == code {
			return entry.class, true
		}
	}
	return "", false
}

// Error is a typed fault with a stable public class/code and a private cause.
type Error struct {
	class   Class
	code    Code
	message string
	cause   error
}

// New builds a fault without a private cause.
func New(class Class, message string) *Error {
	code, _ := CodeFor(class)
	return &Error{class: class, code: code, message: message}
}

// Wrap builds a fault that keeps cause reachable through errors.Is/errors.As.
// The cause is never part of Error() or Message().
func Wrap(class Class, message string, cause error) *Error {
	err := New(class, message)
	err.cause = cause
	return err
}

// Class returns the stable public class.
func (e *Error) Class() Class {
	if e == nil {
		return ""
	}
	return e.class
}

// Code returns the stable public code.
func (e *Error) Code() Code {
	if e == nil {
		return ""
	}
	return e.code
}

// Message returns the public message without the private cause.
func (e *Error) Message() string {
	if e == nil {
		return ""
	}
	return e.message
}

// Error renders the public code and message. It never renders the cause.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	switch {
	case e.code == "":
		return e.message
	case e.message == "":
		return string(e.code)
	default:
		return string(e.code) + ": " + e.message
	}
}

// Unwrap exposes the private cause to errors.Is/errors.As.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// ClassOf returns the fault class of err, if err is or wraps a fault.
func ClassOf(err error) (Class, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.class, true
}

// CodeOf returns the fault code of err, if err is or wraps a fault.
func CodeOf(err error) (Code, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.code, true
}

// MessageOf returns the public message of err without its private cause.
func MessageOf(err error) (string, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.message, true
}
