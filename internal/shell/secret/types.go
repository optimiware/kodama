// Package secret declares stored-secret metadata and value bounds.
//
// Stored secret management reads expose metadata only; the encrypted value is
// resolved at activation by the runtime adapter and never enters an event,
// receipt, task snapshot or log. The bounds here are the declared value-size
// and naming contract shared by the Shell and the custody adapters.
//
// Secret-name authorization is configuration-backed policy, not storage. The
// NamePolicy seam (declared in management.go) decides whether one declared
// secret name may be resolved in one scope from trusted installation
// configuration, and it denies by default: it is never a store table, a
// per-name grant row or a scan, and it can never authorize an undeclared or
// unconfigured name.
package secret

import (
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// SecretID identifies one stored secret.
type SecretID string

// ParseSecretID validates and converts a secret identity.
func ParseSecretID(value string) (SecretID, error) {
	if !validSecretID(value) {
		return "", fault.New(fault.InvalidInput, fmt.Sprintf("invalid secret id %q", value))
	}
	return SecretID(value), nil
}

func validSecretID(value string) bool {
	if value == "" || len(value) > id.MaxIDLength {
		return false
	}
	if value == "." || value == ".." {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// Bounds is the stored-secret value bound set.
type Bounds struct {
	MaxValueBytes int `json:"max_value_bytes"`
}

// DefaultBounds is the declared secret bound set: 64 KiB per stored value.
var DefaultBounds = Bounds{MaxValueBytes: 64 << 10}

// CheckValue refuses a stored value larger than the bound. The check is used
// before custody, so an oversized value never reaches the key adapter.
func (bounds Bounds) CheckValue(size int) error {
	if size > bounds.MaxValueBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("secret value is %d bytes; at most %d are allowed", size, bounds.MaxValueBytes))
	}
	return nil
}

// Metadata is the readable identity of one stored secret. It never carries the
// encrypted or plaintext value.
type Metadata struct {
	ID        SecretID      `json:"id"`
	Scope     tenancy.Scope `json:"scope"`
	Name      id.SecretName `json:"name"`
	Version   uint64        `json:"version"`
	UpdatedAt time.Time     `json:"updated_at"`
}
