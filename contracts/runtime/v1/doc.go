// Package runtimev1 is the frozen wire contract between the in-Pod step
// entrypoint and the control plane's runtime interface (/runtime/v1).
//
// It defines request/response DTOs, strict decoding, validation, the
// context-dependent result checks, error codes and statuses, the operation
// table, and canonical result hashing. It contains no server, client or
// storage code. Consumers (entrypoint, runtimeauth, artifacts, results) own
// their own ports and import this package for shapes only.
//
// Every DTO marshals every field, writing explicit nulls, so a payload that
// round-trips through DecodeRequest is exactly the payload the schema accepts.
package runtimev1

// Interface constants.
const (
	Version  = "v1"
	BasePath = "/runtime/v1/attempts/{attemptId}"
	Audience = "kodama-runtime"
)
