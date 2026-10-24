package root

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/domain"
)

// ContextSetVersion is the only persisted context file version.
const ContextSetVersion = 1

var contextNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Context-set sentinels.
var (
	ErrUnknownContext = errors.New("unknown context")
	ErrContextLocked  = errors.New("context file is locked by another operation")
)

// AuthConfig describes how a factory's users authenticate. It is injected
// configuration (flags or a saved context), not discovered from the factory,
// and never contains a token or client secret: the client is public.
type AuthConfig struct {
	Issuer                string   `json:"issuer,omitempty"`
	ClientID              string   `json:"clientId,omitempty"`
	AuthorizationEndpoint string   `json:"authorizationEndpoint,omitempty"`
	TokenEndpoint         string   `json:"tokenEndpoint,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	RedirectURI           string   `json:"redirectUri,omitempty"`
}

// Validate checks that every field needed for a PKCE login is present.
func (a AuthConfig) Validate() error {
	for _, f := range []struct{ name, v string }{
		{"issuer", a.Issuer}, {"client ID", a.ClientID},
		{"authorization endpoint", a.AuthorizationEndpoint},
		{"token endpoint", a.TokenEndpoint}, {"redirect URI", a.RedirectURI},
	} {
		if f.v == "" {
			return Usagef("authentication configuration is incomplete: the %s is not set", f.name)
		}
	}
	return a.validateSyntax()
}

func (a AuthConfig) validateSyntax() error {
	for _, f := range []struct{ name, v string }{
		{"issuer", a.Issuer}, {"authorization endpoint", a.AuthorizationEndpoint},
		{"token endpoint", a.TokenEndpoint}, {"redirect URI", a.RedirectURI},
	} {
		if f.v == "" {
			continue
		}
		u, err := url.Parse(f.v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" {
			return Usagef("the %s is not a valid absolute URL", f.name)
		}
	}
	for _, s := range a.Scopes {
		if s == "" || strings.ContainsAny(s, " \t\r\n\",\\") {
			return Usagef("invalid OAuth scope %q", s)
		}
	}
	return nil
}

// Context is the persisted selection: a factory, how to authenticate to it,
// and an optional tenant and project. It is routing convenience only; the
// server authorizes every request.
type Context struct {
	Factory string           `json:"factory,omitempty"`
	Auth    AuthConfig       `json:"auth"`
	Tenant  domain.TenantID  `json:"tenant,omitempty"`
	Project domain.ProjectID `json:"project,omitempty"`
}

// normalizeFactory canonicalizes an origin for comparison and key derivation.
func normalizeFactory(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", Usagef("the factory must be an absolute http(s) origin")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", Usagef("the factory must be an origin without credentials, path, query or fragment")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}

// Validate checks the invariants of a context: a project requires a tenant,
// identifiers are well formed, and URLs are syntactically valid.
func (c Context) Validate() error {
	if c.Factory != "" {
		n, err := normalizeFactory(c.Factory)
		if err != nil {
			return err
		}
		if n != c.Factory {
			return Usagef("the factory is not in canonical form")
		}
	}
	if c.Project != "" && c.Tenant == "" {
		return Usagef("a project requires a tenant")
	}
	if c.Tenant != "" {
		if err := c.Tenant.Validate(); err != nil {
			return Usagef("invalid tenant ID: %v", err)
		}
	}
	if c.Project != "" {
		if err := c.Project.Validate(); err != nil {
			return Usagef("invalid project ID: %v", err)
		}
	}
	return c.Auth.validateSyntax()
}

// CredentialKey identifies the stored credential for this factory and
// authentication configuration. It is derived, not chosen, so changing the
// factory, issuer, client or endpoints can never reuse another login's token.
// Tenant and project are deliberately excluded: a login authenticates a
// person to a factory.
func (c Context) CredentialKey() string {
	scopes := append([]string(nil), c.Auth.Scopes...)
	sort.Strings(scopes)
	b, _ := json.Marshal(struct {
		V       int      `json:"v"`
		Factory string   `json:"factory"`
		Issuer  string   `json:"issuer"`
		Client  string   `json:"client"`
		Auth    string   `json:"auth"`
		Token   string   `json:"token"`
		Redir   string   `json:"redirect"`
		Scopes  []string `json:"scopes"`
	}{1, c.Factory, c.Auth.Issuer, c.Auth.ClientID, c.Auth.AuthorizationEndpoint, c.Auth.TokenEndpoint, c.Auth.RedirectURI, scopes})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ContextSet is the persisted collection of named contexts.
type ContextSet struct {
	Version  int                `json:"version"`
	Current  string             `json:"current,omitempty"`
	Contexts map[string]Context `json:"contexts"`
}

// NewContextSet returns an empty set.
func NewContextSet() ContextSet {
	return ContextSet{Version: ContextSetVersion, Contexts: map[string]Context{}}
}

// ValidateName checks a context name.
func ValidateName(name string) error {
	if !contextNamePattern.MatchString(name) {
		return Usagef("invalid context name %q: use 1-64 letters, digits, '.', '_' or '-'", name)
	}
	return nil
}

// Validate checks every context and that the selection exists.
func (s ContextSet) Validate() error {
	if s.Version != ContextSetVersion {
		return fmt.Errorf("unsupported context file version %d", s.Version)
	}
	for name, c := range s.Contexts {
		if err := ValidateName(name); err != nil {
			return err
		}
		if err := c.Validate(); err != nil {
			return fmt.Errorf("context %q: %w", name, err)
		}
	}
	if s.Current != "" {
		if _, ok := s.Contexts[s.Current]; !ok {
			return fmt.Errorf("selected context %q does not exist", s.Current)
		}
	}
	return nil
}

// Names returns the context names sorted.
func (s ContextSet) Names() []string {
	names := make([]string, 0, len(s.Contexts))
	for n := range s.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Define creates or replaces a named context after validating it. It does not
// change the selection.
func (s *ContextSet) Define(name string, c Context) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if s.Contexts == nil {
		s.Contexts = map[string]Context{}
	}
	s.Contexts[name] = c
	return nil
}

// Select makes an existing context current.
func (s *ContextSet) Select(name string) error {
	if _, ok := s.Contexts[name]; !ok {
		return fmt.Errorf("%w %q", ErrUnknownContext, name)
	}
	s.Current = name
	return nil
}

// ContextStore is the consumer-owned persistence port for contexts. Update is
// a serialized read-modify-write: fn runs against the latest complete state,
// and nothing is stored if fn fails. Contention fails with ErrContextLocked.
type ContextStore interface {
	Load(ctx context.Context) (ContextSet, error)
	Update(ctx context.Context, fn func(*ContextSet) error) error
}

// apply merges explicit overrides onto base. Changing the factory clears the
// inherited tenant and project (they belong to the other factory); changing
// the tenant clears an inherited project. Explicit values always win.
func apply(base Context, o Overrides) (Context, error) {
	c := base
	c.Auth.Scopes = append([]string(nil), base.Auth.Scopes...)
	if o.Factory != nil {
		f, err := normalizeFactory(strings.TrimSpace(*o.Factory))
		if err != nil {
			return Context{}, err
		}
		if f != base.Factory {
			c.Tenant, c.Project = "", ""
		}
		c.Factory = f
	}
	if o.Tenant != nil {
		t := domain.TenantID(strings.TrimSpace(*o.Tenant))
		if t != c.Tenant && o.Project == nil {
			c.Project = ""
		}
		c.Tenant = t
	}
	if o.Project != nil {
		c.Project = domain.ProjectID(strings.TrimSpace(*o.Project))
	}
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	set(&c.Auth.Issuer, o.Issuer)
	set(&c.Auth.ClientID, o.ClientID)
	set(&c.Auth.AuthorizationEndpoint, o.AuthorizationEndpoint)
	set(&c.Auth.TokenEndpoint, o.TokenEndpoint)
	set(&c.Auth.RedirectURI, o.RedirectURI)
	if o.Scopes != nil {
		c.Auth.Scopes = nil
		for _, s := range strings.Split(*o.Scopes, ",") {
			c.Auth.Scopes = append(c.Auth.Scopes, strings.TrimSpace(s))
		}
	}
	if err := c.Validate(); err != nil {
		return Context{}, err
	}
	return c, nil
}

// EffectiveContext is what one invocation runs against: the selected saved
// context with explicit flags applied. It is never written back.
type EffectiveContext struct {
	Name string // saved context name, empty when none was selected
	Context
	CredentialStore string // "keyring" (default) or "file"
	Format          Format
	NonInteractive  bool
	Timeout         time.Duration
}

// RequireFactory fails when no factory was selected.
func (e EffectiveContext) RequireFactory() error {
	if e.Factory == "" {
		return Usagef("no factory selected: pass --factory or select a context with `kodama context use`")
	}
	return nil
}

// RequireTenant returns the client scope for a tenant-level request.
func (e EffectiveContext) RequireTenant() (client.Scope, error) {
	if e.Tenant == "" {
		return client.Scope{}, Usagef("no tenant selected: pass --tenant or run `kodama context use --tenant`")
	}
	return client.Scope{Tenant: e.Tenant}, nil
}

// RequireProject returns the client scope for a project-level request.
func (e EffectiveContext) RequireProject() (client.Scope, error) {
	if e.Project == "" {
		return client.Scope{}, Usagef("no project selected: pass --project or run `kodama context use --project`")
	}
	return client.Scope{Tenant: e.Tenant, Project: e.Project}, nil
}

// Credential store kinds accepted by --credential-store.
const (
	StoreKeyring = "keyring"
	StoreFile    = "file"
)

// ResolveContext loads the saved selection and applies explicit overrides.
// An explicit flag always beats the saved value; an explicitly empty flag is a
// validation failure; nothing is written.
func ResolveContext(ctx context.Context, store ContextStore, o Overrides) (EffectiveContext, error) {
	if err := o.validate(); err != nil {
		return EffectiveContext{}, err
	}
	eff := EffectiveContext{Format: FormatHuman, CredentialStore: StoreKeyring, NonInteractive: o.NonInteractive}
	if o.Output != nil {
		f, err := ParseFormat(*o.Output)
		if err != nil {
			return EffectiveContext{}, err
		}
		eff.Format = f
	}
	if o.CredentialStore != nil {
		switch *o.CredentialStore {
		case StoreKeyring, StoreFile:
			eff.CredentialStore = *o.CredentialStore
		default:
			return EffectiveContext{}, Usagef("--credential-store must be keyring or file")
		}
	}
	if o.Timeout != nil {
		eff.Timeout = *o.Timeout
	}
	var set ContextSet
	if store != nil {
		var err error
		if set, err = store.Load(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return EffectiveContext{}, err
			}
			return EffectiveContext{}, NewError(ExitFailure, "context_store_error", "cannot read saved contexts: %v", err).WithCause(err)
		}
	}
	name := set.Current
	if o.Context != nil {
		name = *o.Context
		if _, ok := set.Contexts[name]; !ok {
			return EffectiveContext{}, Usagef("unknown context %q", name).WithCause(ErrUnknownContext)
		}
	}
	base := set.Contexts[name] // zero when no context is selected
	c, err := apply(base, o)
	if err != nil {
		return EffectiveContext{}, err
	}
	eff.Name, eff.Context = name, c
	return eff, nil
}
