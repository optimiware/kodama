//go:build e2e

package harness

import (
	"fmt"
	"sort"

	"github.com/optimiware/kodama/api"
)

// ReassembleLog joins retained chunks of one action stream into their exact
// byte sequence. Offsets are authoritative: a gap, overlap or duplicate is
// refused rather than silently reordered.
func ReassembleLog(chunks []api.LogChunk, actionID, stream string) ([]byte, error) {
	if actionID == "" {
		return nil, fmt.Errorf("log reassembly needs an action identity")
	}
	ordered := make([]api.LogChunk, len(chunks))
	copy(ordered, chunks)
	for index, chunk := range ordered {
		if err := chunk.Validate(); err != nil {
			return nil, fmt.Errorf("log chunk %d: %w", index, err)
		}
		if chunk.ActionID != actionID {
			return nil, fmt.Errorf("log chunk %d belongs to another action", index)
		}
		if chunk.Stream != stream {
			return nil, fmt.Errorf("log chunk %d belongs to another stream", index)
		}
	}
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left].Offset != ordered[right].Offset {
			return ordered[left].Offset < ordered[right].Offset
		}
		return ordered[left].Sequence < ordered[right].Sequence
	})
	var assembled []byte
	for index, chunk := range ordered {
		if chunk.Offset != uint64(len(assembled)) {
			return nil, fmt.Errorf("log chunk %d starts at offset %d; %d bytes were already assembled", index, chunk.Offset, len(assembled))
		}
		assembled = append(assembled, chunk.Data...)
	}
	return assembled, nil
}
