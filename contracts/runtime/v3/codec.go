package runtimev3

import (
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
)

// DecodeRequest strictly decodes and validates a request body with the v1
// rules: unknown, misspelled, differently cased, duplicate and omitted fields
// are rejected, so a nullable field must be an explicit null. v3 adds no
// parsing rules of its own.
func DecodeRequest[T any, P interface {
	*T
	Validate() error
}](data []byte) (T, error) {
	return runtimev1.DecodeRequest[T, P](data)
}

// DecodeResponse decodes and validates a response body for a client: additional
// fields are tolerated (compatible additions), required fields must be present.
func DecodeResponse[T any, P interface {
	*T
	Validate() error
}](data []byte) (T, error) {
	return runtimev1.DecodeResponse[T, P](data)
}
