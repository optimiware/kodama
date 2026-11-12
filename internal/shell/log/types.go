// Package log declares retained/live log chunk values and their bounds.
//
// Core NATS carries live progress while retained logs are fetched separately.
// A chunk is identified by execution, action, stream and monotonic offset so a
// consumer can deduplicate and tolerate reordering without inventing its own
// ordering key.
package log

import (
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
)

// Stream names the captured output stream of one action.
type Stream string

const (
	// StreamStdout is the action's standard output.
	StreamStdout Stream = "stdout"
	// StreamStderr is the action's standard error.
	StreamStderr Stream = "stderr"
)

var streams = [...]Stream{StreamStdout, StreamStderr}

// Streams returns every log stream in declaration order.
func Streams() []Stream {
	return append([]Stream(nil), streams[:]...)
}

// Valid reports whether the stream is one of the declared streams.
func (stream Stream) Valid() bool {
	for _, candidate := range streams {
		if stream == candidate {
			return true
		}
	}
	return false
}

// Bounds is the retained/live log chunk bound set.
type Bounds struct {
	MaxChunkBytes int `json:"max_chunk_bytes"`
}

// DefaultBounds is the declared log chunk bound: 32 KiB per chunk.
var DefaultBounds = Bounds{MaxChunkBytes: 32 << 10}

// CheckChunk refuses a chunk larger than the bound.
func (bounds Bounds) CheckChunk(size int) error {
	if size > bounds.MaxChunkBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("log chunk is %d bytes; at most %d are allowed", size, bounds.MaxChunkBytes))
	}
	return nil
}

// Chunk is one retained or live log chunk. Offset is the monotonic byte offset
// within the stream; Sequence orders chunks of one action.
type Chunk struct {
	ExecutionID id.ExecutionID `json:"execution_id"`
	ActionID    id.ActionID    `json:"action_id"`
	Stream      Stream         `json:"stream"`
	Offset      uint64         `json:"offset"`
	Sequence    uint64         `json:"sequence"`
	Data        []byte         `json:"data"`
}
