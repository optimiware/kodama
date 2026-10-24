package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"

	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/identity/oidc"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtime/process"
	"github.com/optimiware/kodama/internal/secrets"
)

// Port names an outbound port of the composition (docs/implementation/
// environments.md). Each port is selected from the one required environment.
type Port string

// The outbound ports. Persistence stands for every module's repository.
const (
	PortStore      Port = "store"
	PortEvents     Port = "events"
	PortObjects    Port = "objects"
	PortSource     Port = "source"
	PortKeyWrapper Port = "key-wrapper"
	PortIdentity   Port = "identity"
	PortRuntime    Port = "runtime"
)

// Selection is one cell of the per-environment adapter table.
type Selection struct {
	// Adapter names the adapter the environment composes for the port.
	Adapter string
	// Restricted is true for an adapter that exists only for development or
	// test. Production's row never contains one: the table itself is checked
	// against that rule, and Admit enforces it on every constructed value.
	Restricted bool
}

// selections is the adapter table of docs/implementation/environments.md. It is
// documentation the process can test: the constructors below select exactly
// these adapters, and production's row is all unrestricted.
var selections = map[Env]map[Port]Selection{
	Development: {
		PortStore:      {"memdb", true},
		PortEvents:     {"in-process", true},
		PortObjects:    {"in-memory", true},
		PortSource:     {"temporary bare repositories", true},
		PortKeyWrapper: {"ephemeral", true},
		PortIdentity:   {"static, seeded", true},
		PortRuntime:    {process.Backend, true},
	},
	Test: {
		PortStore:      {"postgresql", false},
		PortEvents:     {"jetstream", false},
		PortObjects:    {"localstack s3", true},
		PortSource:     {"https git fixture", true},
		PortKeyWrapper: {"local keyfile", true},
		PortIdentity:   {"oidc fixture", true},
		PortRuntime:    {podman.Backend, true},
	},
	Production: {
		PortStore:      {"postgresql", false},
		PortEvents:     {"jetstream", false},
		PortObjects:    {"s3-compatible", false},
		PortSource:     {"https git host", false},
		PortKeyWrapper: {"cloud kms", false},
		PortIdentity:   {"standalone or optimi oauth", false},
		PortRuntime:    {kube.Backend, false},
	},
}

// Selections returns the adapter table row of an environment.
func Selections(env Env) (map[Port]Selection, error) {
	row, ok := selections[env]
	if !ok {
		return nil, ErrEnvUnknown
	}
	out := make(map[Port]Selection, len(row))
	for p, s := range row {
		out[p] = s
	}
	return out, nil
}

// Ports lists the ports of the table in a stable order.
func Ports() []Port {
	ports := make([]Port, 0, len(selections[Development]))
	for p := range selections[Development] {
		ports = append(ports, p)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports
}

// Constructors is the per-environment constructor table: one entry per port
// whose adapter this package builds from settings. Each entry returns the
// consumer-owned port of its module, never an Env, and every entry applies the
// construction guards of SD-12 (an unknown environment builds nothing and
// production refuses development and test adapters). The table is a struct of
// functions, not a registry: tests replace an entry to inject an adapter, and
// a replaced entry is still passed through Admit by its caller.
type Constructors struct {
	Env Env

	// Runtime builds the workload runtime and the runtime-authority seams that
	// belong to it.
	Runtime func(RuntimeSettings) (RuntimeSet, error)
	// KeyWrapper builds the scope-key wrapper.
	KeyWrapper func(context.Context, SecretsConfig) (secrets.KeyWrapper, func(), error)
	// Verifier builds the bearer-token verifier of the standalone authority.
	Verifier func(OIDCConfig) (identity.CredentialVerifier, error)
	// Objects admits the object store settings.
	Objects func(S3Config) error
}

// NewConstructors returns the table of an environment. It fails for an unknown
// one: there is no fallback table.
func NewConstructors(env Env) (Constructors, error) {
	if !env.Valid() {
		return Constructors{}, ErrEnvUnknown
	}
	c := Constructors{Env: env}
	c.Runtime = func(s RuntimeSettings) (RuntimeSet, error) { return newRuntime(env, s) }
	c.KeyWrapper = func(ctx context.Context, cfg SecretsConfig) (secrets.KeyWrapper, func(), error) {
		return buildSecretsWrapper(ctx, env, cfg, buildKMSClients)
	}
	c.Verifier = func(cfg OIDCConfig) (identity.CredentialVerifier, error) { return newVerifier(env, cfg) }
	c.Objects = func(cfg S3Config) error { return admitObjects(env, cfg) }
	return c, nil
}

// errNotComposed reports an adapter the environment names but this assembly
// does not build: the capability is refused rather than substituted.
var errNotComposed = errors.New("the adapter is not composed by this assembly")

func newVerifier(env Env, cfg OIDCConfig) (identity.CredentialVerifier, error) {
	if env == Development {
		return nil, fmt.Errorf("%w: the %s verifier is supplied by the development composition", errNotComposed, selections[Development][PortIdentity].Adapter)
	}
	if env == Production && cfg.AllowInsecureHTTP {
		return nil, &AdapterError{Env: env, Port: PortIdentity, Adapter: "oidc-fixture", Reason: "an issuer reached over plain HTTP is the test fixture"}
	}
	v, err := oidc.NewVerifier(oidc.Config{
		Issuer: cfg.Issuer, Audience: cfg.Audience, AllowInsecureHTTP: cfg.AllowInsecureHTTP, HTTPClient: cfg.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

func admitObjects(env Env, cfg S3Config) error {
	if env == Development {
		return fmt.Errorf("%w: the in-memory object store is supplied by the development composition", errNotComposed)
	}
	if env == Production {
		return refuseObjectFixture(cfg)
	}
	return nil
}

// refuseObjectFixture rejects the LocalStack fixture by what it is, not by a
// flag: an endpoint that is not https is plain HTTP whatever AllowInsecureHTTP
// says, and every server-side object operation would use it.
func refuseObjectFixture(cfg S3Config) error {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("the object store endpoint is not a valid URL")
	}
	if u.Scheme != "https" || cfg.AllowInsecureHTTP {
		return &AdapterError{Env: Production, Port: PortObjects, Adapter: "localstack-s3", Reason: "an object store reached over plain HTTP is the test fixture"}
	}
	return nil
}
