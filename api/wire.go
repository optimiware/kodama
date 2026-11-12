package api

import (
	"bytes"
	"encoding/json"
	"io"
)

// decodeStrict decodes exactly one JSON object into target, refusing unknown
// fields, a non-object body, an empty body and trailing data.
func decodeStrict(data []byte, target any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return invalid("request body is empty")
	}
	if trimmed[0] != '{' {
		return invalid("request body must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return invalid("invalid JSON body: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return invalid("trailing data after the JSON body")
		}
		return invalid("trailing data after the JSON body: %v", err)
	}
	return nil
}

// DecodeRequest decodes one bounded request envelope. Unknown fields, invalid
// versions and invalid idempotency keys use the public invalid_input code.
func DecodeRequest(data []byte) (Request, error) {
	if len(data) > MaxRequestBytes {
		return Request{}, invalid("request body is %d bytes; at most %d are allowed", len(data), MaxRequestBytes)
	}
	var request Request
	if err := decodeStrict(data, &request); err != nil {
		return Request{}, err
	}
	if err := request.Validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

// DecodeResult decodes one bounded result envelope.
func DecodeResult(data []byte) (Result, error) {
	if len(data) > MaxResponseBytes {
		return Result{}, invalid("response body is %d bytes; at most %d are allowed", len(data), MaxResponseBytes)
	}
	var result Result
	if err := decodeStrict(data, &result); err != nil {
		return Result{}, err
	}
	if err := result.Validate(); err != nil {
		return Result{}, err
	}
	return result, nil
}
