package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Response is the metadata of a completed exchange. The same value is returned
// alongside a RemoteError so retry guidance and request IDs stay available.
type Response struct {
	Status int
	// ETag is the raw header; Version is its integer value, or zero when the
	// header is absent or not a strong quoted integer.
	ETag    string
	Version domain.Version
	// RequestID is the server-assigned request ID, empty if none was sent.
	RequestID string
	// RetryAfter is the server's retry guidance, zero when absent.
	RetryAfter time.Duration
	// Replayed reports an idempotent replay of an earlier completed request.
	Replayed bool
	// Body is the complete bounded response body.
	Body []byte
}

func newResponse(res *http.Response, body []byte, now time.Time) *Response {
	r := &Response{
		Status:    res.StatusCode,
		ETag:      res.Header.Get("ETag"),
		RequestID: res.Header.Get("X-Request-ID"),
		Replayed:  res.Header.Get(httpapi.IdempotencyReplayedHeader) == "true",
		Body:      body,
	}
	if v, err := httpapi.ParseETag(r.ETag); err == nil {
		r.Version = v
	}
	r.RetryAfter = parseRetryAfter(res.Header.Get("Retry-After"), now)
	return r
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 || secs > 1<<31 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// readBounded reads at most max bytes and fails when more are present.
func readBounded(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, ErrResponseTooLarge
	}
	return b, nil
}

func isJSONContent(h http.Header) bool {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		return false
	}
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// decodeComplete decodes exactly one JSON value into out. Unknown fields are
// accepted (additive evolution); trailing data or truncation is an error.
func decodeComplete(body []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(out); err != nil {
		return errors.Join(ErrMalformedResponse, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.Join(ErrMalformedResponse, errors.New("trailing data after JSON document"))
	}
	return nil
}

// remoteError classifies a non-2xx response.
func remoteError(res *http.Response, resp *Response, mutation bool) *RemoteError {
	e := &RemoteError{
		Status: res.StatusCode, RequestID: resp.RequestID, RetryAfter: resp.RetryAfter,
		Mutation: mutation,
	}
	var env httpapi.ErrorEnvelope
	if isJSONContent(res.Header) && decodeComplete(resp.Body, &env) == nil && env.Error.Code != "" {
		e.Code = env.Error.Code
		e.Message = env.Error.Message
		e.Details = env.Error.Details
		if env.Error.RequestId != "" {
			e.RequestID = env.Error.RequestId
		}
		return e
	}
	e.Malformed = true
	return e
}
