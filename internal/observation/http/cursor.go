package observationhttp

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/observation"
)

// cursorSort names the ordering a cursor continues: the project event scope's
// gapless sequence, which is unique per observation.
const cursorSort = "sequence"

// positionVersion versions the observation-specific position carried inside the
// shared codec's module-owned position.
const positionVersion = 1

// maxSafeSequence is the largest sequence a client language can represent.
const maxSafeSequence = 1<<53 - 1

func encodePosition(seq int64) []byte {
	b := make([]byte, 9)
	b[0] = positionVersion
	binary.BigEndian.PutUint64(b[1:], uint64(seq))
	return b
}

func decodePosition(b []byte) (int64, error) {
	if len(b) != 9 || b[0] != positionVersion {
		return 0, errors.New("malformed position")
	}
	seq := binary.BigEndian.Uint64(b[1:])
	if seq > maxSafeSequence {
		return 0, errors.New("position out of range")
	}
	return int64(seq), nil
}

// cursorScope binds a cursor to this call's tenant, project, endpoint, concrete
// run path value and the normalized type filter.
func cursorScope(c *httpapi.Call, scope observation.Scope) httpapi.CursorScope {
	return c.CursorScope(scope.Filters(), cursorSort)
}

func issue(codec *httpapi.CursorCodec, cs httpapi.CursorScope, position int64) (string, error) {
	tok, err := codec.Encode(cs, encodePosition(position))
	if err != nil {
		return "", fmt.Errorf("issuing a cursor: %w", err)
	}
	return tok, nil
}

// resume verifies token under cs and returns its position. Every failure is the
// generic invalid-cursor response.
func resume(codec *httpapi.CursorCodec, cs httpapi.CursorScope, token string) (int64, error) {
	pos, err := codec.Decode(cs, token)
	if err != nil {
		return 0, err
	}
	seq, err := decodePosition(pos)
	if err != nil {
		return 0, invalidCursor()
	}
	return seq, nil
}
