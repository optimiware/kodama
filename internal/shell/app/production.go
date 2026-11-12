package app

import (
	"context"
	"crypto/rand"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/authorize"
	shellidentity "github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store/postgres"
	identitystore "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/token"
)

// lazyPrincipal defers the production principal resolver until the token
// manager it depends on has been constructed. It fails closed before the
// delegate exists.
type lazyPrincipal struct {
	mu       sync.RWMutex
	delegate authorize.CurrentPrincipal
}

func (lazy *lazyPrincipal) Resolve(ctx context.Context, credential string) (shellidentity.Principal, error) {
	lazy.mu.RLock()
	delegate := lazy.delegate
	lazy.mu.RUnlock()
	if delegate == nil {
		return shellidentity.Principal{}, fault.New(fault.Unauthenticated, "presented credential is invalid")
	}
	return delegate.Resolve(ctx, credential)
}

func (lazy *lazyPrincipal) set(delegate authorize.CurrentPrincipal) {
	lazy.mu.Lock()
	lazy.delegate = delegate
	lazy.mu.Unlock()
}

// postgresPool is the narrow per-role connection seam every production store
// consumer holds. A real installation uses the pgx driver; a unit test replaces
// newPostgresPool with a recording pool whose identity is the DSN it was built
// with, so which pool each consumer actually holds is observable without a live
// server. The four pools carry different least-privilege credentials, and a
// swap would silently give one role another's rights.
type postgresPool = postgres.DB

// newPostgresPool constructs one role's pool.
var newPostgresPool = func(config pgdriver.Config) (postgresPool, error) {
	return pgdriver.New(config)
}

// productionLazyHook, when set, observes the composed broker principal after
// its token-manager delegate is installed. Tests use it to pin that wiring; a
// real installation leaves it nil.
var productionLazyHook func(*lazyPrincipal)

// productionDerivationHook, when set, observes the exact derivation-key
// resolver configuration the production composition installs, including the
// associated identity the resolver is constructed with. Tests use it to pin
// that the server opens exactly the identity bootstrap-admin seals with; a real
// installation leaves it nil.
var productionDerivationHook func(bridges.DerivationKeyConfig)

// productionConsumersHook, when set, observes the store-backed consumers the
// production composition builds but does not expose on the environment: the
// identity adapter, the receipt lookup, the credential deliverer and the
// credential manager/resolver boundaries built from them. Tests use it to
// drive one operation per consumer and to prove which role pool each internal
// seam actually reaches. A real installation leaves it nil.
var productionConsumersHook func(productionConsumers)

// productionConsumers groups the composed consumers that hold a role pool
// behind a port the environment does not expose. Manager and Resolver are the
// composed credential boundaries themselves, so a test can prove the seams
// they were actually constructed with, not only the local variables they were
// built from.
type productionConsumers struct {
	Identity  *identitystore.Adapter
	Receipts  *readers.ReceiptLookup
	Deliverer *bridges.Deliverer
	Manager   *token.Manager
	Resolver  *token.Resolver
}

// tenantDerivationSecretID and tenantDerivationSecretName are the fixed
// envelope associated identity of every tenant derivation key. The first-admin
// bootstrap seals with it and the production server opens with it; one
// definition means the two sides can never drift.
const (
	tenantDerivationSecretID   secret.SecretID = "tenant-derivation-key"
	tenantDerivationSecretName id.SecretName   = "TENANT_DERIVATION_KEY"
)

// tenantDerivationAssociated returns the associated data one tenant derivation
// key is sealed and opened with. The tenant and provider key version are taken
// from the stored key at open time; this template carries the installation and
// secret identity both sides must agree on.
func tenantDerivationAssociated(installationID string, keyVersion secret.KeyVersion) secret.AssociatedData {
	return secret.AssociatedData{
		InstallationID: installationID,
		SecretID:       tenantDerivationSecretID,
		Name:           tenantDerivationSecretName,
		KeyVersion:     keyVersion,
	}
}

func buildProductionEnvironment(ctx context.Context, builder *builder) (*environment, error) {
	cfg := builder.cfg
	production := cfg.Production

	stores, err := openPersistentStores(builder, persistentDatabases{
		runtime:    cfg.Installation.DatabaseURL,
		controller: production.ControllerURL,
		publisher:  production.PublisherURL,
		auth:       production.AuthURL,
	})
	if err != nil {
		return nil, err
	}
	controller := stores.controller

	// A multi-tenant installation routes every Kernel operation through the
	// durable owning tenant. The dispatcher is built from the same controller
	// seam the single-tenant composition already uses, so no router state is
	// lost on restart. The credential deliverer and environment.runtime both
	// use this one runtime, and the workspace dispatcher wraps each tenant's
	// own adapter with the existing claim-provisioning decorator.
	runtimeAdapter := production.Runtime
	workspaceAdapter := production.Workspace
	if len(production.Tenants) > 0 {
		scopes, err := newControllerExecutionScopes(controller)
		if err != nil {
			return nil, err
		}
		runtimeAdapter, err = newTenantRuntime(scopes, production.Tenants)
		if err != nil {
			return nil, err
		}
		activation, err := newWorkspaceActivation(controller)
		if err != nil {
			return nil, err
		}
		routedTenants := append([]ProductionTenant(nil), production.Tenants...)
		if production.ClaimProvisioner != nil {
			for index := range routedTenants {
				tenant := routedTenants[index]
				if tenant.WorkspacePVC == nil {
					return nil, fault.New(fault.InvalidInput,
						"production tenant workspace claim template is missing")
				}
				wrapped, err := newKubeProvisioningWorkspace(tenant.Workspace, production.ClaimProvisioner, activation, *tenant.WorkspacePVC)
				if err != nil {
					return nil, err
				}
				routedTenants[index].Workspace = wrapped
			}
		}
		workspaceAdapter, err = newTenantWorkspace(activation, routedTenants)
		if err != nil {
			return nil, err
		}
	} else if production.ClaimProvisioner != nil {
		activation, err := newWorkspaceActivation(controller)
		if err != nil {
			return nil, err
		}
		workspaceAdapter, err = newKubeProvisioningWorkspace(workspaceAdapter, production.ClaimProvisioner, activation, *cfg.WorkspacePVC)
		if err != nil {
			return nil, err
		}
	}

	provider, err := productionKeyProvider(cfg.Installation.Keys, production)
	if err != nil {
		return nil, err
	}
	dial := production.dialNATS
	if dial == nil {
		dial = dialNATSDriver
	}
	environment, err := buildPersistentEnvironment(ctx, builder, stores, persistentServices{
		label:         "production",
		runtime:       runtimeAdapter,
		workspace:     workspaceAdapter,
		provider:      provider,
		keyVersion:    production.Cloud.Current,
		natsTLS:       production.NATSTLS,
		natsAuth:      cfg.NATSAuth,
		dial:          dial,
		oidc:          production.OIDC,
		registry:      production.Registry,
		streams:       production.Streams,
		runtimeCloser: production.RuntimeCloser,
	})
	if err != nil {
		return nil, err
	}
	environment.readiness.kms = production.KMS
	environment.readiness.cloud = production.Cloud
	return environment, nil
}

func productionKeyProvider(adapter installation.KeyAdapter, production *ProductionConfig) (keys.Provider, error) {
	switch adapter {
	case installation.KeyAWSKMS:
		return keys.NewAWS(production.KMS, production.Cloud, rand.Reader)
	case installation.KeyGCPKMS:
		return keys.NewGCP(production.KMS, production.Cloud, rand.Reader)
	case installation.KeyScalewayKMS:
		return keys.NewScaleway(production.KMS, production.Cloud, rand.Reader)
	default:
		return nil, fault.New(fault.InvalidInput, "production requires an explicitly selected cloud KMS key adapter")
	}
}
