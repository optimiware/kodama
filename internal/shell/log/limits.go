// Retained and live log delivery limits and shared redaction.
//
// A retained sink appends bounded scoped chunks and serves bounded reads; a
// live publisher carries progress asynchronously and never becomes the retained
// source. Both refuse a scope-less or malformed chunk before any storage or
// publication effect.
package log

import (
	"bytes"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Clock is the injected authoritative time source for retention.
type Clock interface {
	Now() time.Time
}

// Limits bounds one retained sink and one live publisher.
type Limits struct {
	MaxChunkBytes   int
	MaxReadChunks   int
	MaxStoredChunks int
	MaxQueuedChunks int
}

// DefaultLimits returns the declared default bounds. They are all positive and
// the chunk bound matches the declared log chunk bound.
func DefaultLimits() Limits {
	return Limits{
		MaxChunkBytes:   DefaultBounds.MaxChunkBytes,
		MaxReadChunks:   1000,
		MaxStoredChunks: 10000,
		MaxQueuedChunks: 1024,
	}
}

// SinkConfig configures one file-backed retained sink. Root is required. A zero
// Retention or Limits uses the declared default. Redact lists configured
// sensitive values that must never be stored, including values split across
// chunk boundaries.
type SinkConfig struct {
	Root      string
	Clock     Clock
	Retention Retention
	Limits    Limits
	Redact    []string
}

// PublisherConfig configures one bounded asynchronous live publisher.
// Downstream is required; PublishLive must never wait for it.
type PublisherConfig struct {
	Downstream LivePublisher
	Limits     Limits
	Redact     []string
}

// resolveLimits fills every zero field from the declared default and refuses a
// non-positive bound.
func resolveLimits(limits Limits) (Limits, error) {
	defaults := DefaultLimits()
	if limits.MaxChunkBytes == 0 {
		limits.MaxChunkBytes = defaults.MaxChunkBytes
	}
	if limits.MaxReadChunks == 0 {
		limits.MaxReadChunks = defaults.MaxReadChunks
	}
	if limits.MaxStoredChunks == 0 {
		limits.MaxStoredChunks = defaults.MaxStoredChunks
	}
	if limits.MaxQueuedChunks == 0 {
		limits.MaxQueuedChunks = defaults.MaxQueuedChunks
	}
	if limits.MaxChunkBytes < 0 || limits.MaxReadChunks < 0 || limits.MaxStoredChunks < 0 || limits.MaxQueuedChunks < 0 {
		return Limits{}, fault.New(fault.InvalidInput, "log limits must not be negative")
	}
	return limits, nil
}

// resolveRetention fills a zero policy from the declared default and refuses a
// non-positive retention period.
func resolveRetention(retention Retention) (Retention, error) {
	if retention == (Retention{}) {
		return DefaultRetention(), nil
	}
	defaults := DefaultRetention()
	if retention.MaxChunkBytes == 0 {
		retention.MaxChunkBytes = defaults.MaxChunkBytes
	}
	if retention.RetainFor == 0 {
		retention.RetainFor = defaults.RetainFor
	}
	if retention.MaxChunkBytes <= 0 || retention.RetainFor <= 0 {
		return Retention{}, fault.New(fault.InvalidInput, "log retention must be positive")
	}
	return retention, nil
}

// validateChunk checks one chunk identity, stream and byte bound. It is shared
// by the retained sink and the live publisher so both refuse the same malformed
// input before any effect.
func validateChunk(chunk Chunk, maxChunkBytes int) error {
	if _, err := id.ParseExecutionID(string(chunk.ExecutionID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "log chunk execution id is invalid", err)
	}
	if _, err := id.ParseActionID(string(chunk.ActionID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "log chunk action id is invalid", err)
	}
	if !chunk.Stream.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("log chunk stream %q is unknown", chunk.Stream))
	}
	if len(chunk.Data) > maxChunkBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("log chunk is %d bytes; at most %d are allowed", len(chunk.Data), maxChunkBytes))
	}
	return nil
}

// validateScope checks the explicit ownership scope of one log operation.
func validateScope(scope tenancy.Scope) error {
	return ValidateScope(scope)
}

// redactionMarker replaces every configured sensitive value. It carries no
// fragment of the removed value.
const redactionMarker = "[REDACTED]"

// redactor replaces configured values in stored and published chunk bytes.
type redactor struct {
	secrets []string
}

// newRedactor validates the configured values and orders them longest-first so
// an overlapping longer value is removed before a shorter prefix of it.
func newRedactor(values []string) (redactor, error) {
	secrets := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			return redactor{}, fault.New(fault.InvalidInput, "log redaction value is empty")
		}
		secrets = append(secrets, value)
	}
	for left := 0; left < len(secrets); left++ {
		for right := left + 1; right < len(secrets); right++ {
			if len(secrets[right]) > len(secrets[left]) {
				secrets[left], secrets[right] = secrets[right], secrets[left]
			}
		}
	}
	return redactor{secrets: secrets}, nil
}

func (redactor redactor) active() bool {
	return len(redactor.secrets) > 0
}

// redactAll removes every complete configured value from data.
func (redactor redactor) redactAll(data []byte) []byte {
	if !redactor.active() || len(data) == 0 {
		return append([]byte(nil), data...)
	}
	output := make([]byte, 0, len(data))
	for index := 0; index < len(data); {
		matched := false
		for _, secret := range redactor.secrets {
			if bytes.HasPrefix(data[index:], []byte(secret)) {
				output = append(output, redactionMarker...)
				index += len(secret)
				matched = true
				break
			}
		}
		if !matched {
			output = append(output, data[index])
			index++
		}
	}
	return output
}

// stream redacts one chunk while holding back a suffix that could be the start
// of a configured value completed by the next chunk. It returns the bytes safe
// to store now and the pending suffix to prepend to the next chunk.
func (redactor redactor) stream(pending, incoming []byte) ([]byte, []byte) {
	combined := make([]byte, 0, len(pending)+len(incoming))
	combined = append(combined, pending...)
	combined = append(combined, incoming...)
	redacted := redactor.redactAll(combined)
	hold := redactor.pendingSuffix(redacted)
	if hold == 0 {
		return redacted, nil
	}
	stored := append([]byte(nil), redacted[:len(redacted)-hold]...)
	next := append([]byte(nil), redacted[len(redacted)-hold:]...)
	return stored, next
}

// pendingSuffix returns the length of the longest suffix of data that is a
// proper prefix of a configured value, so a split value is never stored.
func (redactor redactor) pendingSuffix(data []byte) int {
	best := 0
	for _, secret := range redactor.secrets {
		limit := min(len(secret)-1, len(data))
		for length := limit; length > best; length-- {
			if bytes.HasSuffix(data, []byte(secret[:length])) {
				best = length
				break
			}
		}
	}
	return best
}
