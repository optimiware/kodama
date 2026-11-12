package api

// HTTPStatusSuccess is the single REST success status. Every successful public
// operation returns 200 with an api.Result body; the envelope, not the status,
// carries the operation name, result body and replay fact.
const HTTPStatusSuccess = 200

// httpStatuses is the one-to-one mapping from every public error code to
// its REST status. The response body is always api.Error with the stable code,
// so a client never infers the public code from the status alone. The mapping is
// injective: no two public codes share a status.
var httpStatuses = []struct {
	code   string
	status int
}{
	{CodeInvalidInput, 400},
	{CodeUnauthenticated, 401},
	{CodeForbidden, 403},
	{CodeNotFound, 404},
	{CodeDeadlineExpired, 408},
	{CodeVersionConflict, 409},
	{CodeChangedIdempotencyContent, 422},
	{CodeInternalFailure, 500},
	{CodeUnsupportedCapability, 501},
	{CodeUncertainExternalEffect, 502},
	{CodeDependencyUnavailable, 503},
}

// HTTPStatus returns the REST status for a public error code. An empty or
// unknown code is refused, and a success status is never returned for an error
// code.
func HTTPStatus(code string) (int, bool) {
	for _, entry := range httpStatuses {
		if entry.code == code {
			return entry.status, true
		}
	}
	return 0, false
}
