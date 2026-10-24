package identity

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
)

// UserID is the stable opaque identifier of a person. It is what httpapi calls
// the principal ID after authentication.
type UserID string

// BindingID identifies one fixed-role binding (a membership resource).
type BindingID string

var idPattern = regexp.MustCompile(domain.IDPattern)

// Validate checks the opaque-ID syntax.
func (id UserID) Validate() error {
	if !idPattern.MatchString(string(id)) {
		return invalid("user id must match %s", domain.IDPattern)
	}
	return nil
}

// Validate checks the opaque-ID syntax.
func (id BindingID) Validate() error {
	if !idPattern.MatchString(string(id)) {
		return invalid("binding id must match %s", domain.IDPattern)
	}
	return nil
}

// MaxIssuerBytes and MaxSubjectBytes bound the verified identity pair.
const (
	MaxIssuerBytes  = 512
	MaxSubjectBytes = 512
)

// Principal is a verified (issuer, subject) pair. Both come from a verified
// access token; neither an email address, a display name nor any role claim
// identifies a person or grants membership.
type Principal struct {
	Issuer  string
	Subject string
}

// Validate bounds and sanitizes the pair.
func (p Principal) Validate() error {
	for _, f := range []struct {
		name, v string
		max     int
	}{{"issuer", p.Issuer, MaxIssuerBytes}, {"subject", p.Subject, MaxSubjectBytes}} {
		if f.v == "" || len(f.v) > f.max || !utf8.ValidString(f.v) || strings.ContainsFunc(f.v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return invalid("%s must be 1..%d bytes of UTF-8 without control characters", f.name, f.max)
		}
	}
	return nil
}

// ActorType classifies who performs an action. Only users hold human roles;
// workload identities get explicit attempt-scoped actions elsewhere.
type ActorType string

const (
	ActorUser     ActorType = "user"
	ActorService  ActorType = "service"
	ActorWorkload ActorType = "workload"
	ActorOperator ActorType = "operator"
)

// Actor is the verified identity behind an action.
type Actor struct {
	ID   string
	Type ActorType
}

// IsHuman reports whether the actor is a person.
func (a Actor) IsHuman() bool { return a.Type == ActorUser }

// Validate checks that the actor is attributable.
func (a Actor) Validate() error {
	switch a.Type {
	case ActorUser, ActorService, ActorWorkload, ActorOperator:
	default:
		return invalid("unknown actor type %q", string(a.Type))
	}
	if !idPattern.MatchString(a.ID) {
		return invalid("actor id must match %s", domain.IDPattern)
	}
	return nil
}

// UserActor is the actor of an authenticated person.
func UserActor(id UserID) Actor { return Actor{ID: string(id), Type: ActorUser} }

// RandomID returns prefix plus 32 random hex characters.
func RandomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}
