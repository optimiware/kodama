package chatops

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// DestinationID identifies a destination. Like every identifier it is opaque;
// ownership is never inferred from it.
type DestinationID string

var idPattern = regexp.MustCompile(domain.IDPattern)

// Validate checks the identifier syntax.
func (id DestinationID) Validate() error {
	if !idPattern.MatchString(string(id)) {
		return invalidf("destination id must match %s", domain.IDPattern)
	}
	return nil
}

// NewDestinationID returns a fresh identifier from 128 random bits.
func NewDestinationID() (DestinationID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("chatops: generate destination id: %w", err)
	}
	return DestinationID("cdst_" + hex.EncodeToString(b[:])), nil
}

// Bounds of a destination.
const (
	MaxEndpointBytes   = 2048
	MaxCredentialBytes = 128
	maxVersion         = domain.MaxSafeInteger
)

var (
	namePattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	credentialPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
)

// Destination is one project-scoped generic webhook destination.
//
// Identity (Scope, ID, CreatedAt) is immutable. Name, Events, Enabled,
// Endpoint and Credential change only through Edit, which requires the
// expected Version. BindingRevision advances only when the endpoint or the
// credential reference changes: a delivery records the revision it was created
// under, so an intent is never silently redirected to a different receiver or
// credential (see Delivery.Replay for the one explicit, audited re-binding).
type Destination struct {
	Scope domain.Scope
	ID    DestinationID
	// Name is a short project-unique label.
	Name string
	// Endpoint is an absolute https URL without user-info, query or fragment.
	// The installation's outbound policy (webhook.Policy) must still approve it.
	Endpoint string
	// Events is the sorted, de-duplicated allow-list of event types.
	Events []string
	// Credential references an installation-managed credential bound to this
	// recipient (never a value). Empty means a credential-free receiver.
	Credential      string
	Enabled         bool
	BindingRevision int64
	Version         int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// DestinationInput is a creation request.
type DestinationInput struct {
	Name       string
	Endpoint   string
	Events     []string
	Credential string
	Enabled    bool
}

// NewDestination validates a creation request and returns version 1 at binding
// revision 1.
func NewDestination(scope domain.Scope, id DestinationID, in DestinationInput, now time.Time) (Destination, error) {
	d := Destination{Scope: scope, ID: id, Name: in.Name, Endpoint: in.Endpoint, Events: normalizeEvents(in.Events), Credential: in.Credential,
		Enabled: in.Enabled, BindingRevision: 1, Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	if err := d.Validate(); err != nil {
		return Destination{}, err
	}
	return d, nil
}

// Validate checks every invariant of the entity.
func (d Destination) Validate() error {
	if err := d.Scope.Validate(); err != nil {
		return invalidf("the destination scope is not valid")
	}
	if err := d.ID.Validate(); err != nil {
		return err
	}
	if !namePattern.MatchString(d.Name) {
		return invalidf("a destination name matches %s", namePattern)
	}
	if err := ValidateEndpoint(d.Endpoint); err != nil {
		return err
	}
	if len(d.Events) == 0 || !slices.IsSorted(d.Events) || len(slices.Compact(slices.Clone(d.Events))) != len(d.Events) {
		return invalidf("a destination selects at least one event type, sorted and without repeats")
	}
	for _, e := range d.Events {
		if !IsSupportedEvent(e) {
			return invalidf("event type %q cannot be forwarded", e)
		}
	}
	if d.Credential != "" && !credentialPattern.MatchString(d.Credential) {
		return invalidf("a credential reference matches %s", credentialPattern)
	}
	if d.BindingRevision < 1 || d.BindingRevision > maxVersion || d.Version < 1 || d.Version > maxVersion {
		return invalidf("versions are positive and bounded")
	}
	return nil
}

// ValidateEndpoint checks the generic shape of an endpoint: an absolute https
// URL with a host, no user-info, no query and no fragment. Credentials never
// travel in the URL; the adapter's policy additionally approves the host.
func ValidateEndpoint(raw string) error {
	if raw == "" || len(raw) > MaxEndpointBytes {
		return invalidf("an endpoint is 1 to %d bytes", MaxEndpointBytes)
	}
	for _, r := range raw {
		if r <= 0x20 || r == 0x7f {
			return invalidf("an endpoint contains whitespace or control characters")
		}
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Opaque != "":
		return invalidf("the endpoint is not a valid URL")
	case u.Scheme != "https":
		return invalidf("the endpoint must use https")
	case u.Hostname() == "":
		return invalidf("the endpoint needs a host")
	case u.User != nil:
		return invalidf("the endpoint must not carry user-info")
	case u.Fragment != "" || u.RawFragment != "" || containsHash(raw):
		return invalidf("the endpoint must not carry a fragment")
	case u.RawQuery != "" || u.ForceQuery:
		return invalidf("the endpoint must not carry a query; credentials use a credential reference")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return invalidf("the endpoint port is not valid")
		}
	}
	return nil
}

func containsHash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '#' {
			return true
		}
	}
	return false
}

func normalizeEvents(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// DestinationEdit is a partial update; nil members are unchanged. Credential
// points at the empty string to clear the reference.
type DestinationEdit struct {
	Name       *string
	Endpoint   *string
	Events     []string
	Credential *string
	Enabled    *bool
}

// Empty reports whether the edit names no member.
func (e DestinationEdit) Empty() bool {
	return e.Name == nil && e.Endpoint == nil && e.Events == nil && e.Credential == nil && e.Enabled == nil
}

// Edit applies a partial update under optimistic concurrency. The expected
// version must be current. An edit that changes nothing leaves the version
// alone. Changing the endpoint or the credential reference advances
// BindingRevision; the result is validated before it replaces the receiver.
func (d *Destination) Edit(expected int64, e DestinationEdit, now time.Time) (changed bool, err error) {
	if expected != d.Version {
		return false, Fail(ErrVersionConflict, nil, "the destination changed")
	}
	next := *d
	next.Events = slices.Clone(d.Events)
	if e.Name != nil {
		next.Name = *e.Name
	}
	if e.Endpoint != nil {
		next.Endpoint = *e.Endpoint
	}
	if e.Events != nil {
		next.Events = normalizeEvents(e.Events)
	}
	if e.Credential != nil {
		next.Credential = *e.Credential
	}
	if e.Enabled != nil {
		next.Enabled = *e.Enabled
	}
	if next.Endpoint != d.Endpoint || next.Credential != d.Credential {
		next.BindingRevision++
	}
	same := next.Name == d.Name && next.Endpoint == d.Endpoint && slices.Equal(next.Events, d.Events) &&
		next.Credential == d.Credential && next.Enabled == d.Enabled
	if same {
		return false, nil
	}
	next.Version++
	next.UpdatedAt = now.UTC()
	if err := next.Validate(); err != nil {
		return false, err
	}
	*d = next
	return true, nil
}

// Selects reports whether the destination is enabled and forwards the type.
func (d Destination) Selects(eventType string) bool {
	return d.Enabled && slices.Contains(d.Events, eventType)
}

// Recipient is the audience a credential must be bound to: the destination, the
// exact binding revision and the origin the credential would be sent to.
type Recipient struct {
	Scope           domain.Scope
	DestinationID   DestinationID
	BindingRevision int64
	// Host and Port are those of the endpoint (default port 443).
	Host string
	Port int
}

// Recipient derives the credential audience from the endpoint.
func (d Destination) Recipient() (Recipient, error) {
	return RecipientFor(d.Scope, d.ID, d.BindingRevision, d.Endpoint)
}

// RecipientFor derives the credential audience of an endpoint.
func RecipientFor(scope domain.Scope, id DestinationID, revision int64, endpoint string) (Recipient, error) {
	if err := ValidateEndpoint(endpoint); err != nil {
		return Recipient{}, err
	}
	u, _ := url.Parse(endpoint)
	port := 443
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	return Recipient{Scope: scope, DestinationID: id, BindingRevision: revision, Host: u.Hostname(), Port: port}, nil
}
