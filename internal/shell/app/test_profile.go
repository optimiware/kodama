package app

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/installation"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
)

// TestConfig is the durable form of ProfileTest: KODAMA.md's single-host test
// environment with PostgreSQL, NATS, the Podman runtime and a local keyfile.
// It shares the durable builder with production (role pools, broker, token and
// OIDC resolver, registry, file log sink, stream provisioning) and differs only
// in its runtime and key adapters. Humans authenticate through OIDC and the
// current database memberships; no development authenticator or seed exists.
//
// A ProfileTest configuration with Test == nil keeps the fully injected
// harness form; the two forms are never mixed.
type TestConfig struct {
	// Runtime and Workspace are the Podman adapter. VolumeProvisioner is the
	// Podman API client the fresh-only workspace decorator creates volumes
	// with. All three are required.
	Runtime           kernelengine.Runtime
	Workspace         kernelengine.Workspace
	VolumeProvisioner VolumeProvisioner
	// RuntimeReady probes the Podman API service for readiness. Required.
	RuntimeReady func(context.Context) error
	// RuntimeCloser releases the runtime client's transport at shutdown. It
	// never stops the daemon or a durable workload.
	RuntimeCloser func(context.Context) error
	// KeyfilePath is the absolute path of the existing local key
	// (Installation.KeyRef names it). The composition only reads it.
	KeyfilePath string
	OIDC        oidc.Config
	Registry    registry.Config
	// NATSTLS optionally verifies the broker against a private CA and presents
	// a client certificate. Nil dials without custom TLS material.
	NATSTLS       *natsdriver.TLSConfig
	ControllerURL string
	PublisherURL  string
	AuthURL       string
	// Streams configures the JetStream streams provisioned at startup. Nil
	// selects the driver defaults.
	Streams *StreamConfig

	// dialNATS is a same-package test seam for the NATS driver dial.
	dialNATS natsDial
}

// durable reports whether the configuration composes the durable adapters:
// production, or the explicit durable form of the test profile.
func (config Config) durable() bool {
	return config.Profile == ProfileProduction || (config.Profile == ProfileTest && config.Test != nil)
}

// validateDurableTest checks the durable test form before any adapter exists.
func (config Config) validateDurableTest() error {
	if err := config.Installation.Preflight(); err != nil {
		return err
	}
	if config.Installation.Environment != installation.EnvironmentTest {
		return fault.New(fault.InvalidInput, "durable test composition requires the test installation environment")
	}
	if config.Installation.Runtime != installation.RuntimePodman {
		return fault.New(fault.InvalidInput, "durable test composition requires the podman runtime adapter")
	}
	if config.Dev != nil {
		return fault.New(fault.InvalidInput, "durable test composition refuses a development authenticator and first-admin seed")
	}
	if config.Production != nil {
		return fault.New(fault.InvalidInput, "durable test composition refuses a production adapter configuration")
	}
	if config.Adapters != nil && !reflect.ValueOf(*config.Adapters).IsZero() {
		return fault.New(fault.InvalidInput, "durable test composition refuses injected adapter overrides")
	}
	if config.WorkspacePVC != nil {
		return fault.New(fault.InvalidInput, "durable test composition refuses a kubernetes workspace claim template")
	}
	if strings.TrimSpace(config.DataDir) == "" || !filepath.IsAbs(config.DataDir) {
		return fault.New(fault.InvalidInput, "durable test composition requires an absolute data directory")
	}
	return config.Test.validate(config.Installation)
}

func (test *TestConfig) validate(installationConfig installation.Config) error {
	if test.Runtime == nil {
		return fault.New(fault.InvalidInput, "durable test composition requires the podman runtime adapter")
	}
	if test.Workspace == nil {
		return fault.New(fault.InvalidInput, "durable test composition requires the podman workspace adapter")
	}
	if test.VolumeProvisioner == nil {
		return fault.New(fault.InvalidInput, "durable test composition requires the podman volume provisioner")
	}
	if test.RuntimeReady == nil {
		return fault.New(fault.InvalidInput, "durable test composition requires a runtime readiness probe")
	}
	if strings.TrimSpace(test.KeyfilePath) == "" || !filepath.IsAbs(test.KeyfilePath) {
		return fault.New(fault.InvalidInput, "durable test composition requires an absolute keyfile path")
	}
	if test.OIDC.JWKSURL == "" {
		return fault.New(fault.InvalidInput, "durable test composition requires an OIDC configuration")
	}
	if test.Registry.Transport == nil {
		return fault.New(fault.InvalidInput, "durable test composition requires a registry transport")
	}
	// The runtime URL is Installation.DatabaseURL and Preflight checked it;
	// every other role URL gets the same test endpoint rule.
	for _, database := range []struct {
		name string
		url  string
	}{
		{"controller", test.ControllerURL},
		{"publisher", test.PublisherURL},
		{"auth", test.AuthURL},
	} {
		if strings.TrimSpace(database.url) == "" {
			return fault.New(fault.InvalidInput, "durable test composition requires a "+database.name+" database URL")
		}
		probe := installationConfig
		probe.DatabaseURL = database.url
		if err := probe.Preflight(); err != nil {
			return fault.Wrap(fault.InvalidInput, "durable test "+database.name+" database URL is invalid", err)
		}
	}
	if tls := test.NATSTLS; tls != nil {
		if (strings.TrimSpace(tls.CertFile) == "") != (strings.TrimSpace(tls.KeyFile) == "") {
			return fault.New(fault.InvalidInput, "durable test broker TLS needs both a client certificate and its key")
		}
	}
	return test.Streams.validate()
}

func buildDurableTestEnvironment(ctx context.Context, builder *builder) (*environment, error) {
	cfg := builder.cfg
	test := cfg.Test
	stores, err := openPersistentStores(builder, persistentDatabases{
		runtime:    cfg.Installation.DatabaseURL,
		controller: test.ControllerURL,
		publisher:  test.PublisherURL,
		auth:       test.AuthURL,
		closePools: true,
	})
	if err != nil {
		return nil, err
	}
	reference := secret.KeyRef(cfg.Installation.KeyRef)
	provider, err := keys.NewKeyfile(reference, test.KeyfilePath, rand.Reader)
	if err != nil {
		return nil, err
	}
	// The Podman adapter's Retain never recreates a lost volume; the
	// decorator provisions it only for a never-activated workspace.
	activation, err := newWorkspaceActivation(stores.controller)
	if err != nil {
		return nil, err
	}
	workspace, err := newProvisioningWorkspace(test.Workspace, test.VolumeProvisioner, activation)
	if err != nil {
		return nil, err
	}
	dial := test.dialNATS
	if dial == nil {
		dial = dialNATSDriver
	}
	environment, err := buildPersistentEnvironment(ctx, builder, stores, persistentServices{
		label:         "test",
		runtime:       test.Runtime,
		workspace:     workspace,
		provider:      provider,
		keyVersion:    secret.MinKeyVersion,
		natsTLS:       test.NATSTLS,
		natsAuth:      cfg.NATSAuth,
		dial:          dial,
		oidc:          test.OIDC,
		registry:      test.Registry,
		streams:       test.Streams,
		runtimeCloser: test.RuntimeCloser,
	})
	if err != nil {
		return nil, err
	}
	environment.readiness.local = []readinessCheck{
		{name: "keyfile", run: keyfileReadinessCheck(reference, test.KeyfilePath)},
		{name: "runtime", run: runtimeReadinessCheck(test.RuntimeReady)},
	}
	return environment, nil
}

// keyfileReadinessCheck re-reads the keyfile with the provider's own guards
// (regular owner-only file of the exact key size) on every probe. The key
// bytes stay inside the discarded provider.
func keyfileReadinessCheck(reference secret.KeyRef, path string) httpapi.ReadinessCheck {
	return func(context.Context) error {
		_, err := keys.NewKeyfile(reference, path, rand.Reader)
		return err
	}
}

// runtimeReadinessCheck runs the runtime's own API probe.
func runtimeReadinessCheck(probe func(context.Context) error) httpapi.ReadinessCheck {
	return func(ctx context.Context) error {
		if probe == nil {
			return fault.New(fault.DependencyUnavailable, "runtime readiness probe is not configured")
		}
		return probe(ctx)
	}
}
