package gitv1

import (
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
)

// DecodeRequest strictly decodes and validates a request body (or a claim set or
// record read from storage) with the runtime v1 rules: unknown, misspelled,
// differently cased, duplicate and omitted members are rejected, so a nullable
// member must be an explicit null and an optional member is omitted, never null.
// Failures are *ContractError with an invalid_request code.
func DecodeRequest[T any, P interface {
	*T
	Validate() error
}](data []byte) (T, error) {
	return runtimev1.DecodeRequest[T, P](data)
}

// DecodeResponse decodes and validates a response body for a client: additional
// members are tolerated (compatible additions), required members must be present.
func DecodeResponse[T any, P interface {
	*T
	Validate() error
}](data []byte) (T, error) {
	return runtimev1.DecodeResponse[T, P](data)
}
