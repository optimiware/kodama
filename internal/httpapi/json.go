package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
)

// Default bounds. The composition root may override them through RouterConfig.
const (
	DefaultMaxRequestBytes  int64 = 1 << 20
	DefaultMaxResponseBytes       = 4 << 20
)

func setJSONHeaders(h http.Header) {
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
}

// ReadBody reads the whole request body within the router's size bound and
// requires a JSON media type. The bytes are returned unmodified so they can be
// hashed for idempotency before decoding.
func ReadBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return nil, NewError(http.StatusUnsupportedMediaType, "invalid_request", "Content-Type must be application/json")
	}
	limit := settingsFrom(r.Context()).maxRequestBytes
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, NewError(http.StatusRequestEntityTooLarge, "invalid_request", "the request body is too large").
				WithDetails(map[string]any{"maxBytes": limit})
		}
		return nil, BadRequestError("the request body could not be read")
	}
	return body, nil
}

// DecodeJSONBytes decodes exactly one JSON document into dst. Unknown fields,
// trailing data and an empty body are rejected so a typo cannot silently drop
// an edit. Error messages never echo the body.
func DecodeJSONBytes(body []byte, dst any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return BadRequestError("a JSON request body is required")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return BadRequestError("the request body is not valid for this operation")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return BadRequestError("the request body must contain a single JSON document")
	}
	return nil
}

// DecodeJSON is ReadBody followed by DecodeJSONBytes.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	body, err := ReadBody(w, r)
	if err != nil {
		return err
	}
	return DecodeJSONBytes(body, dst)
}

// MarshalJSON serializes v and enforces the response size bound, so the bytes
// can be stored (idempotency) before anything is written to the client.
func MarshalJSON(r *http.Request, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, InternalServerError(err)
	}
	if max := settingsFrom(r.Context()).maxResponseBytes; len(b) > max {
		return nil, InternalServerError(errors.New("response exceeds the configured size bound"))
	}
	return b, nil
}

// WriteJSON writes v with the given status. Serialization and the size bound
// are checked first, so a failure produces a clean error response instead of a
// truncated success.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	b, err := MarshalJSON(r, v)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	setJSONHeaders(w.Header())
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
