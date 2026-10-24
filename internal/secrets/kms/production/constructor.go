package production

import (
	"context"
	"crypto/rand"
	"fmt"
	"sort"

	gcpkms "cloud.google.com/go/kms/apiv1"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/pkg/secretkeys"
)

// Clients carries the workload-authenticated cloud clients. Exactly the client
// of the selected provider must be set. The caller builds, authenticates,
// configures (region, endpoint, retry policy) and closes them; this package
// reads no environment variable, credential chain or metadata service and
// never discovers a cloud.
type Clients struct {
	// AWS is an AWS KMS client created with the workload's credentials.
	AWS *kms.Client
	// GCP is a Cloud KMS client created with the workload's credentials. The
	// caller closes it after the Provider is no longer used.
	GCP *gcpkms.KeyManagementClient
	// ScalewayToken is the Key Manager API secret key (sent as X-Auth-Token).
	ScalewayToken string
}

// String and GoString keep the Scaleway token out of logs and diagnostics.
func (Clients) String() string   { return "production.Clients{redacted}" }
func (Clients) GoString() string { return "production.Clients{redacted}" }

// builder constructs the pkg/secretkeys provider of the selected cloud. Tests
// replace it; production uses buildCloud.
type builder func(p plan, c Clients) (secretkeys.Provider, error)

func buildCloud(p plan, c Clients) (secretkeys.Provider, error) {
	retained := make([]string, 0, len(p.retained))
	for _, r := range p.retained {
		retained = append(retained, r.id)
	}
	sort.Strings(retained)
	var (
		inner secretkeys.Provider
		err   error
	)
	switch p.provider {
	case AWS:
		inner, err = secretkeys.NewAWS(c.AWS, p.current.id, retained)
	case GCP:
		inner, err = secretkeys.NewGCP(c.GCP, p.current.id, retained)
	default:
		inner, err = secretkeys.NewScaleway(c.ScalewayToken, p.current.id, retained)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: the %s provider rejected its configuration", ErrInvalidConfig, p.provider)
	}
	return inner, nil
}

// checkClients requires the selected cloud's credential and refuses any other.
func checkClients(p plan, c Clients) error {
	present := map[string]bool{AWS: c.AWS != nil, GCP: c.GCP != nil, Scaleway: c.ScalewayToken != ""}
	if !present[p.provider] {
		return fmt.Errorf("%w: no %s client or credential supplied", ErrCredentials, p.provider)
	}
	for cloud, ok := range present {
		if ok && cloud != p.provider {
			return fmt.Errorf("%w: a %s client was supplied but %s is selected", ErrCredentials, cloud, p.provider)
		}
	}
	return nil
}

// New validates the configuration, constructs only the selected provider, and
// proves it with a bounded, disposable wrap/unwrap round trip of a random
// 32-byte key under the current KEK. Any failure — no or unknown provider,
// missing credentials, denial, outage, mismatch or timeout — returns an error
// and no Provider; nothing falls back to another cloud or the keyfile. The
// probe proves current encrypt/decrypt capability, not that every retained
// backup can still be decrypted.
func New(ctx context.Context, cfg Config, clients Clients) (*Provider, error) {
	return construct(ctx, cfg, clients, buildCloud)
}

func construct(ctx context.Context, cfg Config, clients Clients, build builder) (*Provider, error) {
	p, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	if err := checkClients(p, clients); err != nil {
		return nil, err
	}
	inner, err := build(p, clients)
	if err != nil {
		return nil, err
	}
	bridge := newBridge(p, inner)
	if err := probe(ctx, bridge, p); err != nil {
		return nil, err
	}
	return bridge, nil
}

func probe(ctx context.Context, b *Provider, p plan) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	key := make([]byte, scopeKeySize)
	defer clear(key)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("%w: entropy unavailable", ErrStartupProbe)
	}
	// A reserved context that no stored scope key can use.
	wc := secrets.WrapContext{InstallationID: "kodama-startup-probe", TenantID: "t_startup_probe", Generation: 1, KeyID: "startup_probe"}
	w, err := b.Wrap(ctx, wc, key)
	if err != nil {
		return fmt.Errorf("%w: wrap: %w", ErrStartupProbe, err)
	}
	got, err := b.Unwrap(ctx, wc, w)
	if err != nil {
		return fmt.Errorf("%w: unwrap: %w", ErrStartupProbe, err)
	}
	defer clear(got)
	if !equalKeys(got, key) {
		return fmt.Errorf("%w: recovered key differs", ErrStartupProbe)
	}
	return nil
}
