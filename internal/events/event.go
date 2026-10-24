// Package events implements Kodama's durable event delivery behavior: the
// publication and inbox entities, the publisher and consumer decision logic,
// replay and retention rules, and periodic reconciliation.
//
// It follows the outbox -> JetStream -> inbox flow of the event catalog.
// PostgreSQL state and the outbox are authoritative; the broker provides
// at-least-once transport, never the correctness boundary. This package holds
// entities, value objects and small consumer-owned ports and contains no pgx or
// NATS types. Adapters live in the postgres and jetstream sub-packages and are
// wired by the composition root (T-027).
package events

import (
	"fmt"
	"regexp"
	"strings"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// SubjectRoot is the fixed, versioned prefix of every event subject. Adapters
// may prepend a namespace for isolation, but never change this tail.
const SubjectRoot = "factory.v1.tenant"

// subjectTokenPattern is the safe single-token alphabet of a server-assigned
// tenant subject token: no dots, wildcards, spaces or uppercase.
var subjectTokenPattern = regexp.MustCompile(`^[a-z0-9]{16,64}$`)

// ValidateSubjectToken rejects anything that is not a server-assigned tenant
// subject token. Untrusted strings must never construct subjects.
func ValidateSubjectToken(token string) error {
	if !subjectTokenPattern.MatchString(token) {
		return fmt.Errorf("%w: subject token must match %s", domain.ErrInvalidValue, subjectTokenPattern)
	}
	return nil
}

// Scope is the ownership of an event: always a tenant, optionally a project.
// An empty ProjectID marks a tenant-wide event (domain.Scope requires a
// project, so events use this module-local value).
type Scope struct {
	TenantID  domain.TenantID
	ProjectID domain.ProjectID
}

// Validate checks the identifiers; the project is optional.
func (s Scope) Validate() error {
	if err := s.TenantID.Validate(); err != nil {
		return err
	}
	if s.ProjectID != "" {
		return s.ProjectID.Validate()
	}
	return nil
}

// Key is the observation scope key: the project ID, or "" for tenant-wide.
func (s Scope) Key() string { return string(s.ProjectID) }

func (s Scope) String() string {
	if s.ProjectID == "" {
		return string(s.TenantID) + "/*"
	}
	return fmt.Sprintf("%s/%s", s.TenantID, s.ProjectID)
}

// ScopeOf returns the ownership recorded in an envelope.
func ScopeOf(env eventsv1.Envelope) Scope {
	return Scope{TenantID: env.TenantID, ProjectID: env.ProjectID}
}

// Subject composes `[<namespace>.]factory.v1.tenant.<token>.<event-type>`. The
// namespace is operator configuration; the token must be server-assigned and
// the type must belong to the catalog.
func Subject(namespace, token, eventType string) (string, error) {
	if err := ValidateSubjectToken(token); err != nil {
		return "", err
	}
	if _, ok := eventsv1.Spec(eventType); !ok {
		return "", fmt.Errorf("%w: %q", eventsv1.ErrUnsupportedType, eventType)
	}
	root := SubjectRoot
	if namespace != "" {
		root = namespace + "." + SubjectRoot
	}
	return root + "." + token + "." + eventType, nil
}

// ParseSubject splits a subject produced by Subject. It returns the tenant
// token and the (unvalidated) event type tail.
func ParseSubject(namespace, subject string) (token, eventType string, err error) {
	root := SubjectRoot + "."
	if namespace != "" {
		root = namespace + "." + root
	}
	rest, ok := strings.CutPrefix(subject, root)
	if !ok {
		return "", "", fmt.Errorf("%w: subject outside %q", domain.ErrInvalidValue, root)
	}
	token, eventType, ok = strings.Cut(rest, ".")
	if !ok || eventType == "" {
		return "", "", fmt.Errorf("%w: subject has no event type", domain.ErrInvalidValue)
	}
	if err := ValidateSubjectToken(token); err != nil {
		return "", "", err
	}
	return token, eventType, nil
}
