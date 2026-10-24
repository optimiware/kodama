package mcpproxy

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
)

// Bounds on registrations.
const (
	MaxTools         = 128
	MaxToolNameBytes = 128
	MaxEndpointBytes = 512
)

var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// ValidateToolName checks a remote tool name. The slash is reserved by the
// qualified form "<registrationID>/<toolName>", so it cannot appear in a name.
func ValidateToolName(name string) error {
	if !toolNameRE.MatchString(name) {
		return invalidf("the tool name is not valid")
	}
	return nil
}

// RegistrationID is the opaque identifier of a registration.
type RegistrationID string

// Validate checks the identifier syntax.
func (id RegistrationID) Validate() error {
	if err := (domain.AttemptID(id)).Validate(); err != nil {
		return invalidf("the registration identifier is not valid")
	}
	return nil
}

// SecretRef is the exact typed credential reference of a registration: the
// name of an MCP-kind project secret and one exact value version. A moving
// "current" alias is never stored, so a rotation cannot change what a binding
// resolves.
type SecretRef struct {
	Name    string
	Version int64
}

// Validate checks the reference syntax.
func (r SecretRef) Validate() error {
	if err := secrets.ValidateSecretName(r.Name); err != nil {
		return invalidf("the credential secret name is not valid")
	}
	if r.Version < 1 || r.Version > domain.MaxSafeInteger {
		return invalidf("the credential version is not valid")
	}
	return nil
}

// Actor is the human who changed a registration. Only users manage them.
type Actor struct{ ID string }

// Validate checks the actor.
func (a Actor) Validate() error {
	if n := len(a.ID); n < 1 || n > 256 {
		return invalidf("the actor identifier must be 1..256 bytes")
	}
	return nil
}

// Draft is the editable content of a registration.
type Draft struct {
	Endpoint string
	Secret   SecretRef
	Tools    []string
}

// NormalizeEndpoint validates the endpoint's syntax and returns it in the form
// stored. It must be an https URL without userinfo, query or fragment: those
// forms could carry credentials or redirect decisions outside the typed secret.
func NormalizeEndpoint(raw string) (string, *url.URL, error) {
	if raw == "" || len(raw) > MaxEndpointBytes {
		return "", nil, invalidf("the endpoint must be 1..%d bytes", MaxEndpointBytes)
	}
	for _, r := range raw {
		if unicode.IsControl(r) || unicode.IsSpace(r) || r > unicode.MaxASCII {
			return "", nil, invalidf("the endpoint must be a plain ASCII URL")
		}
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil, u.Scheme != "https", u.Host == "", u.Hostname() == "":
		return "", nil, invalidf("the endpoint must be an https URL")
	case u.User != nil:
		return "", nil, invalidf("the endpoint must not carry credentials")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "":
		return "", nil, invalidf("the endpoint must not carry a query or fragment")
	}
	u.Host = strings.ToLower(u.Host)
	if p := u.Port(); p != "" && (p == "0" || len(p) > 5) {
		return "", nil, invalidf("the endpoint port is not valid")
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), u, nil
}

// Origin returns "scheme://host[:port]" of a normalized endpoint: the recipient
// an integration credential must be addressed to.
func Origin(u *url.URL) string { return u.Scheme + "://" + u.Host }

// Validate checks the draft and returns its normalized form.
func (d Draft) Validate() (Draft, error) {
	endpoint, _, err := NormalizeEndpoint(d.Endpoint)
	if err != nil {
		return Draft{}, err
	}
	if err := d.Secret.Validate(); err != nil {
		return Draft{}, err
	}
	if len(d.Tools) > MaxTools {
		return Draft{}, invalidf("a registration enables at most %d tools", MaxTools)
	}
	tools := slices.Clone(d.Tools)
	slices.Sort(tools)
	for i, t := range tools {
		if err := ValidateToolName(t); err != nil {
			return Draft{}, err
		}
		if i > 0 && tools[i-1] == t {
			return Draft{}, invalidf("a tool is listed more than once")
		}
	}
	if tools == nil {
		tools = []string{}
	}
	return Draft{Endpoint: endpoint, Secret: d.Secret, Tools: tools}, nil
}

// Registration is a project's approved remote MCP server: endpoint, typed
// credential reference and enabled tool names. Tenant, project and identifier
// are immutable. Content changes are explicit, expected-version edits; deletion
// is a tombstone that denies all further use and is irreversible.
type Registration struct {
	Scope     domain.Scope
	ID        RegistrationID
	Endpoint  string
	Secret    SecretRef
	Tools     []string
	Version   domain.Version
	CreatedBy Actor
	UpdatedBy Actor
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

func micros(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// NewRegistration creates version 1.
func NewRegistration(scope domain.Scope, id RegistrationID, d Draft, by Actor, now time.Time) (Registration, error) {
	if err := scope.Validate(); err != nil {
		return Registration{}, invalidf("the scope is not valid")
	}
	if err := id.Validate(); err != nil {
		return Registration{}, err
	}
	if err := by.Validate(); err != nil {
		return Registration{}, err
	}
	nd, err := d.Validate()
	if err != nil {
		return Registration{}, err
	}
	now = micros(now)
	return Registration{Scope: scope, ID: id, Endpoint: nd.Endpoint, Secret: nd.Secret, Tools: nd.Tools, Version: 1,
		CreatedBy: by, UpdatedBy: by, CreatedAt: now, UpdatedAt: now}, nil
}

// Validate checks a restored registration.
func (r Registration) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return invalidf("the scope is not valid")
	}
	if err := r.ID.Validate(); err != nil {
		return err
	}
	if err := r.Version.Validate(); err != nil {
		return invalidf("the version is not valid")
	}
	if _, err := (Draft{Endpoint: r.Endpoint, Secret: r.Secret, Tools: r.Tools}).Validate(); err != nil {
		return err
	}
	return nil
}

// Deleted reports whether the registration is a tombstone.
func (r Registration) Deleted() bool { return r.DeletedAt != nil }

// Enables reports whether the registration enables the remote tool.
func (r Registration) Enables(tool string) bool {
	return !r.Deleted() && slices.Contains(r.Tools, tool)
}

// Edit returns the next version with new content. Ownership never changes. A
// stale expected version is ErrVersionConflict and a tombstone cannot change.
func (r Registration) Edit(expected domain.Version, d Draft, by Actor, now time.Time) (Registration, error) {
	if r.Deleted() {
		return Registration{}, transitionf("a deleted registration cannot change")
	}
	if expected != r.Version {
		return Registration{}, ErrVersionConflict
	}
	if err := by.Validate(); err != nil {
		return Registration{}, err
	}
	nd, err := d.Validate()
	if err != nil {
		return Registration{}, err
	}
	if r.Version >= domain.MaxSafeInteger {
		return Registration{}, transitionf("the version space is exhausted")
	}
	next := r
	next.Endpoint, next.Secret, next.Tools = nd.Endpoint, nd.Secret, nd.Tools
	next.Version++
	next.UpdatedBy, next.UpdatedAt = by, micros(now)
	return next, nil
}

// Delete returns the tombstone. Deleting twice is a transition error, not a
// silent success: the caller learns the expected version was stale.
func (r Registration) Delete(expected domain.Version, by Actor, now time.Time) (Registration, error) {
	if r.Deleted() {
		return Registration{}, transitionf("the registration is already deleted")
	}
	if expected != r.Version {
		return Registration{}, ErrVersionConflict
	}
	if err := by.Validate(); err != nil {
		return Registration{}, err
	}
	next := r
	t := micros(now)
	next.Version++
	next.UpdatedBy, next.UpdatedAt, next.DeletedAt = by, t, &t
	return next, nil
}

func (r Registration) String() string { return fmt.Sprintf("registration %s/%s", r.Scope, r.ID) }
