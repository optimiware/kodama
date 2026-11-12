package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/id"
	shellapp "github.com/optimiware/kodama/internal/shell/app"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/postgres"
)

// bootstrapAdminRunner is the one-shot bootstrap surface the entrypoint runs.
type bootstrapAdminRunner interface {
	Run(context.Context) (shellapp.BootstrapAdminResult, error)
}

// bootstrapAdminDeps are the injectable bootstrap-admin collaborators. The
// database seam records which connection URL the command actually uses, so the
// runtime database (never another role's connection) is pinned by a test.
type bootstrapAdminDeps struct {
	openExecutor func(context.Context, string) (store.Executor, func(), error)
	newBootstrap func(shellapp.BootstrapAdminConfig, shellapp.BootstrapAdminDependencies) (bootstrapAdminRunner, error)
	// newCipher seals the tenant derivation key. It is a seam only so a test can
	// observe the verified request without a live cloud KMS; production always
	// uses the configured cloud KMS provider.
	newCipher func(keys.Provider) (secret.EnvelopeCipher, error)
}

// bootstrapKeys selects the key provider by the explicit profile. Production
// (also the empty profile) uses the cloud KMS loader, which refuses
// the keyfile adapter; the test profile reads the keyfile and creates it only
// when initialization is explicitly requested.
func bootstrapKeys(getenv productionEnv, options bootstrapKeyOptions) (keys.Provider, secret.KeyVersion, error) {
	switch options.profile {
	case "", "production":
		if options.initKeyfile {
			return nil, 0, fmt.Errorf("keyfile initialization requires the test profile")
		}
		keyset, err := loadProductionKeys(getenv)
		if err != nil {
			return nil, 0, err
		}
		return keyset.provider, keyset.version, nil
	case "test":
		if options.initKeyfile {
			_, path, err := testKeySettings(getenv)
			if err != nil {
				return nil, 0, err
			}
			if err := initializeTestKeyfile(path, rand.Reader); err != nil {
				return nil, 0, err
			}
		}
		keyset, err := loadTestKeys(getenv)
		if err != nil {
			return nil, 0, err
		}
		return keyset.provider, keyset.version, nil
	default:
		return nil, 0, fmt.Errorf("bootstrap-admin supports the production and test profiles only")
	}
}

func defaultBootstrapAdminDeps() bootstrapAdminDeps {
	return bootstrapAdminDeps{
		openExecutor: func(_ context.Context, connString string) (store.Executor, func(), error) {
			db, err := pgdriver.New(pgdriver.Config{ConnString: connString, MaxConns: 2})
			if err != nil {
				return nil, nil, err
			}
			return postgres.New(db), db.Close, nil
		},
		newBootstrap: func(config shellapp.BootstrapAdminConfig, dependencies shellapp.BootstrapAdminDependencies) (bootstrapAdminRunner, error) {
			return shellapp.NewBootstrapAdmin(config, dependencies)
		},
		newCipher: func(provider keys.Provider) (secret.EnvelopeCipher, error) {
			return keys.NewCipher(provider, rand.Reader)
		},
	}
}

// runBootstrapAdminEntry is `kodama bootstrap-admin`: create the first tenant
// and its first administrator from a verified OIDC access token. Run it inside
// the server's container so it reads the same environment:
//
//	kubectl exec -i deploy/kodama -- sh -c 'cat >/tmp/t && kodama bootstrap-admin --token-file /tmp/t ...'
func runBootstrapAdminEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("bootstrap-admin", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tenant := flags.String("tenant", os.Getenv("KODAMA_TENANT"), "tenant id to create")
	name := flags.String("tenant-name", "", "tenant display name")
	tokenFile := flags.String("token-file", "", "file holding one OIDC access token of the first administrator")
	profile := flags.String("profile", os.Getenv("KODAMA_PROFILE"), "installation profile (production or test; production when empty)")
	initKeyfile := flags.Bool("init-keyfile", false, "test profile only: create KODAMA_KEYFILE; refused when it already exists")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *tokenFile == "" || *name == "" {
		usage(stderr, "bootstrap-admin requires --tenant-name and --token-file")
		return 2
	}
	options := bootstrapKeyOptions{profile: strings.TrimSpace(*profile), initKeyfile: *initKeyfile}
	switch options.profile {
	case "", "production":
		if options.initKeyfile {
			usage(stderr, "bootstrap-admin --init-keyfile requires --profile test")
			return 2
		}
	case "test":
	default:
		usage(stderr, fmt.Sprintf("bootstrap-admin supports --profile production or test, not %q", options.profile))
		return 2
	}
	tenantID, err := id.ParseTenantID(*tenant)
	if err != nil {
		fmt.Fprintf(stderr, "kodama: tenant: %v\n", err)
		return 2
	}
	result, err := bootstrapAdminKeyed(ctx, os.Getenv, tenantID, *name, *tokenFile, options, defaultBootstrapAdminDeps())
	if err != nil {
		fmt.Fprintf(stderr, "kodama: bootstrap-admin: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant %s created; first administrator %s\n", result.TenantID, result.PrincipalID)
	return 0
}

// bootstrapKeyOptions selects the key adapter bootstrap seals the tenant
// derivation key with. An empty profile selects the production
// behavior; only the explicit test profile reads (or, with initKeyfile,
// creates) the local keyfile.
type bootstrapKeyOptions struct {
	profile     string
	initKeyfile bool
}

func bootstrapAdmin(ctx context.Context, getenv productionEnv, tenant id.TenantID, name, tokenFile string) (shellapp.BootstrapAdminResult, error) {
	return bootstrapAdminWith(ctx, getenv, tenant, name, tokenFile, defaultBootstrapAdminDeps())
}

func bootstrapAdminWith(ctx context.Context, getenv productionEnv, tenant id.TenantID, name, tokenFile string, deps bootstrapAdminDeps) (shellapp.BootstrapAdminResult, error) {
	return bootstrapAdminKeyed(ctx, getenv, tenant, name, tokenFile,
		bootstrapKeyOptions{profile: strings.TrimSpace(getenv("KODAMA_PROFILE"))}, deps)
}

func bootstrapAdminKeyed(ctx context.Context, getenv productionEnv, tenant id.TenantID, name, tokenFile string, options bootstrapKeyOptions, deps bootstrapAdminDeps) (shellapp.BootstrapAdminResult, error) {
	var zero shellapp.BootstrapAdminResult
	need := func(key string) (string, error) {
		if value := strings.TrimSpace(getenv(key)); value != "" {
			return value, nil
		}
		return "", fmt.Errorf("requires %s", key)
	}
	var values = map[string]string{}
	for _, key := range []string{"KODAMA_DATABASE_URL", "KODAMA_INSTALLATION_ID",
		"KODAMA_OIDC_ISSUER", "KODAMA_OIDC_AUDIENCE", "KODAMA_OIDC_JWKS_URL", "KODAMA_OIDC_TENANT_CLAIM"} {
		value, err := need(key)
		if err != nil {
			return zero, err
		}
		values[key] = value
	}
	provider, version, err := bootstrapKeys(getenv, options)
	if err != nil {
		return zero, err
	}
	cipher, err := deps.newCipher(provider)
	if err != nil {
		return zero, err
	}
	client, err := oidcHTTPClient(getenv("KODAMA_OIDC_CA_FILE"))
	if err != nil {
		return zero, err
	}
	verifier, err := oidc.NewVerifier(oidc.Config{
		Issuer: values["KODAMA_OIDC_ISSUER"], Audience: values["KODAMA_OIDC_AUDIENCE"], ClockSkew: time.Minute,
		JWKSURL:            values["KODAMA_OIDC_JWKS_URL"],
		Algorithms:         []string{oidc.AlgorithmRS256, oidc.AlgorithmES256},
		HTTPClient:         client,
		MaxJWKSBytes:       1 << 20,
		MaxTokenBytes:      1 << 20,
		CacheTTL:           10 * time.Minute,
		MinRefreshInterval: 10 * time.Second,
		SubjectClaim:       strings.TrimSpace(getenv("KODAMA_OIDC_SUBJECT_CLAIM")),
		TenantClaim:        values["KODAMA_OIDC_TENANT_CLAIM"],
		ProjectClaim:       strings.TrimSpace(getenv("KODAMA_OIDC_PROJECT_CLAIM")),
	})
	if err != nil {
		return zero, err
	}
	// The first admin lives in the runtime database: the one role that may write
	// tenants and memberships. Reading any other role's URL here would take the
	// bootstrap through a connection that cannot hold the commit unit.
	executor, closeExecutor, err := deps.openExecutor(ctx, values["KODAMA_DATABASE_URL"])
	if err != nil {
		return zero, err
	}
	defer closeExecutor()
	bootstrap, err := deps.newBootstrap(shellapp.BootstrapAdminConfig{
		InstallationID: values["KODAMA_INSTALLATION_ID"],
		TenantID:       tenant,
		TenantName:     name,
		TokenFile:      tokenFile,
		KeyVersion:     secret.KeyVersion(version),
	}, shellapp.BootstrapAdminDependencies{
		Executor: executor,
		IDs:      bridges.NewSecureIDs(),
		Cipher:   cipher,
		Verifier: verifier,
		Clock:    bridges.NewSystemClock(),
	})
	if err != nil {
		return zero, err
	}
	return bootstrap.Run(ctx)
}
