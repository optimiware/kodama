package agents

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ActorType classifies who performed an action. The type comes from the
// identity authority, never from request content.
type ActorType string

const (
	ActorUser     ActorType = "user"
	ActorService  ActorType = "service"
	ActorWorkload ActorType = "workload"
	ActorOperator ActorType = "operator"
)

// Actor is an attributable, authority-resolved identity.
type Actor struct {
	ID   string    `json:"id"`
	Type ActorType `json:"type"`
}

// Validate checks that the actor is usable for attribution.
func (a Actor) Validate() error {
	switch a.Type {
	case ActorUser, ActorService, ActorWorkload, ActorOperator:
	default:
		return fmt.Errorf("%w: unknown actor type", domain.ErrInvalidValue)
	}
	if n := len(a.ID); n < 1 || n > 256 {
		return fmt.Errorf("%w: actor id must be 1..256 bytes", domain.ErrInvalidValue)
	}
	return nil
}

// Permission names an authorization question put to the Authority. The values
// double as the route action identifiers. The identity adapter maps them onto
// the existing fixed-role permissions (reads: run:read, registration:
// run:manage); this module defines no role and grants nothing itself.
type Permission string

const (
	PermList     Permission = "agent_profile.list"
	PermRegister Permission = "agent_profile.register"
)

// Caller is the verified identity of a request as the transport saw it.
type Caller struct {
	PrincipalID string
	RequestID   string
}

func (c Caller) validate() error {
	if c.PrincipalID == "" || len(c.PrincipalID) > 512 {
		return fmt.Errorf("%w: principal must be 1..512 bytes", domain.ErrInvalidValue)
	}
	if len(c.RequestID) > 256 {
		return fmt.Errorf("%w: request id must be at most 256 bytes", domain.ErrInvalidValue)
	}
	return nil
}

// ProfileID is the opaque identifier of one registered profile version.
type ProfileID string

var idRE = regexp.MustCompile(domain.IDPattern)

// Validate checks the opaque-ID syntax.
func (id ProfileID) Validate() error {
	if !idRE.MatchString(string(id)) {
		return fmt.Errorf("%w: profile id must match %s", domain.ErrInvalidValue, domain.IDPattern)
	}
	return nil
}

// Digest is "sha256:" followed by 64 lowercase hexadecimal digits.
type Digest string

var digestRE = regexp.MustCompile(runtimev1.Patterns["Digest"])

// Validate checks the digest syntax.
func (d Digest) Validate() error {
	if !digestRE.MatchString(string(d)) {
		return fmt.Errorf("%w: invalid digest", domain.ErrInvalidValue)
	}
	return nil
}

// Control names a runtime behavior a profile may claim and an installation must
// demonstrably enforce (AR-04). Wall time is enforced by execution deadlines for
// every step and is therefore not a catalogue control.
type Control string

const (
	// ControlModel: the harness uses only the approved model and routes provider
	// traffic through the AI gateway.
	ControlModel Control = "model"
	// ControlRequests: the run's request allowance is enforced by the gateway.
	ControlRequests Control = "requests"
	// ControlTokens: the run's token allowance is enforced by the gateway.
	ControlTokens Control = "tokens"
	// ControlToolCalls: a tool-call ceiling is enforced by the tool proxy.
	ControlToolCalls Control = "tool_calls"
	// ControlToolAllowlist: only the listed tools are offered and admitted.
	ControlToolAllowlist Control = "tool_allowlist"
	// ControlTranscript: the harness records a transcript as a diagnostic
	// artifact when the profile enables it.
	ControlTranscript Control = "transcript"
)

// Controls lists every known control in a stable order.
func Controls() []Control {
	return []Control{ControlModel, ControlRequests, ControlTokens, ControlToolCalls, ControlToolAllowlist, ControlTranscript}
}

// Valid reports whether c is a known control.
func (c Control) Valid() bool {
	for _, k := range Controls() {
		if k == c {
			return true
		}
	}
	return false
}

// Reference syntax. A profile is addressed by the server-returned immutable
// "name@version". A bare name is a moving alias and is never resolved: the
// pipeline module's ProfileLookup contract lets this module define the syntax.
var (
	nameRE      = regexp.MustCompile(runtimev1.Patterns["Name"])
	versionTail = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)
)

// Ref returns the immutable reference "name@version".
func Ref(name string, version domain.Version) string {
	return name + "@" + strconv.FormatInt(int64(version), 10)
}

// ParseReference splits an immutable reference. A bare name (a moving alias),
// a missing or non-canonical version and any other shape is ErrInvalid.
func ParseReference(ref string) (name string, version domain.Version, err error) {
	i := strings.LastIndexByte(ref, '@')
	if i < 0 {
		return "", 0, fmt.Errorf("%w: a profile reference must be an immutable name@version", ErrInvalid)
	}
	name, tail := ref[:i], ref[i+1:]
	if !nameRE.MatchString(name) || !versionTail.MatchString(tail) {
		return "", 0, fmt.Errorf("%w: a profile reference must be an immutable name@version", ErrInvalid)
	}
	v, perr := strconv.ParseInt(tail, 10, 64)
	if perr != nil || v < 1 || v > domain.MaxSafeInteger {
		return "", 0, fmt.Errorf("%w: a profile reference must be an immutable name@version", ErrInvalid)
	}
	return name, domain.Version(v), nil
}
