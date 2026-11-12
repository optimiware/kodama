// Production identity bridge.
//
// One secure-random allocator implements every capability IDSource port. Each
// identity kind keeps its own prefix and a 128-bit random payload, so a value
// can never be confused with another kind and two calls never collide. The
// randomness source is injected for deterministic tests and defaults to
// crypto/rand only through the explicit NewSecureIDs constructor.
package bridges

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/task"
)

// identityBytes is the random payload of one allocated identity.
const identityBytes = 16

// IDs allocates the durable identities declared by the capability ports. It is
// safe for concurrent use; the injected reader is read under one lock so a
// deterministic test sequence stays ordered.
type IDs struct {
	random io.Reader
	mu     sync.Mutex
}

// NewIDs builds an allocator over the supplied randomness source. A nil source
// is refused rather than silently replaced, so production composition makes
// the crypto/rand choice explicit through NewSecureIDs.
func NewIDs(random io.Reader) (*IDs, error) {
	if random == nil {
		return nil, fault.New(fault.InvalidInput, "identity allocator requires a randomness source")
	}
	return &IDs{random: random}, nil
}

// NewSecureIDs returns the production allocator backed by crypto/rand.
func NewSecureIDs() *IDs {
	return &IDs{random: rand.Reader}
}

// NewRowID allocates one opaque store row identity.
func (ids *IDs) NewRowID() (string, error) {
	return ids.allocate("row")
}

// NewContentID allocates one content-record row identity.
func (ids *IDs) NewContentID() (string, error) {
	return ids.allocate("content")
}

// NewTaskID allocates one task identity.
func (ids *IDs) NewTaskID() (id.TaskID, error) {
	value, err := ids.allocate("task")
	return id.TaskID(value), err
}

// NewQuestionID allocates one conversation question identity.
func (ids *IDs) NewQuestionID() (task.QuestionID, error) {
	value, err := ids.allocate("question")
	return task.QuestionID(value), err
}

// NewExecutionID allocates one execution identity.
func (ids *IDs) NewExecutionID() (id.ExecutionID, error) {
	value, err := ids.allocate("execution")
	return id.ExecutionID(value), err
}

// NewPrincipalID allocates one principal identity.
func (ids *IDs) NewPrincipalID() (id.PrincipalID, error) {
	value, err := ids.allocate("principal")
	return id.PrincipalID(value), err
}

// NewEventID allocates one durable event identity.
func (ids *IDs) NewEventID() (id.EventID, error) {
	value, err := ids.allocate("event")
	return id.EventID(value), err
}

// NewReceiptID allocates one idempotency receipt identity.
func (ids *IDs) NewReceiptID() (id.ReceiptID, error) {
	value, err := ids.allocate("receipt")
	return id.ReceiptID(value), err
}

// NewSecretID allocates one stored-secret identity.
func (ids *IDs) NewSecretID() (secret.SecretID, error) {
	value, err := ids.allocate("secret")
	return secret.SecretID(value), err
}

// NewUsageID allocates one model-usage row identity.
func (ids *IDs) NewUsageID() (gateway.UsageID, error) {
	value, err := ids.allocate("usage")
	return gateway.UsageID(value), err
}

// allocate draws one secure random payload and renders the prefixed identity.
func (ids *IDs) allocate(prefix string) (string, error) {
	if ids == nil || ids.random == nil {
		return "", fault.New(fault.InternalFailure, "identity allocator is not configured")
	}
	var raw [identityBytes]byte
	ids.mu.Lock()
	_, err := io.ReadFull(ids.random, raw[:])
	ids.mu.Unlock()
	if err != nil {
		return "", fault.Wrap(fault.InternalFailure, "identity allocation failed", err)
	}
	return prefix + "-" + hex.EncodeToString(raw[:]), nil
}
