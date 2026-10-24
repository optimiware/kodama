package root

import (
	"flag"
	"strings"
	"time"
)

// Flags holds the common flags every command accepts. Presence is preserved
// (via the FlagSet), so an explicitly empty value is a validation failure
// rather than a silent fallback to a saved value.
type Flags struct {
	fs *flag.FlagSet

	Context               string
	Factory               string
	Tenant                string
	Project               string
	Output                string
	CredentialStore       string
	Issuer                string
	ClientID              string
	AuthorizationEndpoint string
	TokenEndpoint         string
	Scopes                string
	RedirectURI           string
	NonInteractive        bool
	Timeout               time.Duration
}

// BindFlags registers the common flags on fs. Call Overrides after fs.Parse.
func BindFlags(fs *flag.FlagSet) *Flags {
	f := &Flags{fs: fs}
	fs.StringVar(&f.Context, "context", "", "named saved context to use")
	fs.StringVar(&f.Factory, "factory", "", "factory origin, for example https://factory.example.invalid")
	fs.StringVar(&f.Tenant, "tenant", "", "tenant ID")
	fs.StringVar(&f.Project, "project", "", "project ID (requires a tenant)")
	fs.StringVar(&f.Output, "output", "human", "output format: human or json")
	fs.StringVar(&f.CredentialStore, "credential-store", "keyring", "credential store: keyring (default) or file (explicit 0600 file opt-in)")
	fs.StringVar(&f.Issuer, "issuer", "", "OIDC issuer URL")
	fs.StringVar(&f.ClientID, "client-id", "", "public OAuth client ID")
	fs.StringVar(&f.AuthorizationEndpoint, "authorization-endpoint", "", "OAuth authorization endpoint")
	fs.StringVar(&f.TokenEndpoint, "token-endpoint", "", "OAuth token endpoint")
	fs.StringVar(&f.Scopes, "scopes", "", "comma-separated OAuth scopes")
	fs.StringVar(&f.RedirectURI, "redirect-uri", "", "loopback redirect URI registered for the client")
	fs.BoolVar(&f.NonInteractive, "non-interactive", false, "never open a browser or prompt")
	fs.DurationVar(&f.Timeout, "timeout", 0, "overall command timeout (default: none)")
	return f
}

// Overrides records which flags were explicitly set, with their values.
type Overrides struct {
	Context               *string
	Factory               *string
	Tenant                *string
	Project               *string
	Output                *string
	CredentialStore       *string
	Issuer                *string
	ClientID              *string
	AuthorizationEndpoint *string
	TokenEndpoint         *string
	Scopes                *string
	RedirectURI           *string
	NonInteractive        bool
	Timeout               *time.Duration
}

// Overrides returns the explicit flag values after parsing.
func (f *Flags) Overrides() Overrides {
	set := map[string]bool{}
	f.fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	str := func(name string, v *string) *string {
		if set[name] {
			c := *v
			return &c
		}
		return nil
	}
	o := Overrides{
		Context: str("context", &f.Context), Factory: str("factory", &f.Factory),
		Tenant: str("tenant", &f.Tenant), Project: str("project", &f.Project),
		Output: str("output", &f.Output), CredentialStore: str("credential-store", &f.CredentialStore),
		Issuer: str("issuer", &f.Issuer), ClientID: str("client-id", &f.ClientID),
		AuthorizationEndpoint: str("authorization-endpoint", &f.AuthorizationEndpoint),
		TokenEndpoint:         str("token-endpoint", &f.TokenEndpoint),
		Scopes:                str("scopes", &f.Scopes), RedirectURI: str("redirect-uri", &f.RedirectURI),
		NonInteractive: f.NonInteractive,
	}
	if set["timeout"] {
		d := f.Timeout
		o.Timeout = &d
	}
	return o
}

// validate rejects explicitly empty values and a nonpositive timeout.
func (o Overrides) validate() error {
	named := []struct {
		name string
		v    *string
	}{
		{"context", o.Context}, {"factory", o.Factory}, {"tenant", o.Tenant}, {"project", o.Project},
		{"output", o.Output}, {"credential-store", o.CredentialStore}, {"issuer", o.Issuer},
		{"client-id", o.ClientID}, {"authorization-endpoint", o.AuthorizationEndpoint},
		{"token-endpoint", o.TokenEndpoint}, {"scopes", o.Scopes}, {"redirect-uri", o.RedirectURI},
	}
	for _, n := range named {
		if n.v != nil && strings.TrimSpace(*n.v) == "" {
			return Usagef("--%s was given an empty value", n.name)
		}
	}
	if o.Timeout != nil && *o.Timeout <= 0 {
		return Usagef("--timeout must be positive")
	}
	return nil
}

// peekOutput finds --output in raw arguments so a parse failure is still
// rendered in the format the caller asked for.
func peekOutput(args []string) Format {
	for i, a := range args {
		a = strings.TrimLeft(a, "-")
		if a == "output" && i+1 < len(args) && args[i+1] == "json" {
			return FormatJSON
		}
		if a == "output=json" {
			return FormatJSON
		}
	}
	return FormatHuman
}

// parseInterspersed parses args with fs, accepting flags before, between and
// after positional arguments (`work update wi_1042 --if-version 1`). It
// separates flag tokens from positionals using the FlagSet's own definitions,
// then hands only the flag tokens to the standard-library parser, so flag
// syntax and error messages stay those of package flag. A bare "--" ends flag
// parsing: everything after it is positional, verbatim.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var flagArgs, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' { // "-" alone and non-flags are positional
			positional = append(positional, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue // value is attached
		}
		fl := fs.Lookup(name)
		if fl == nil {
			continue // unknown: Parse reports it
		}
		if b, ok := fl.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) { // the next token is this flag's value, whatever it looks like
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}
	return positional, nil
}
