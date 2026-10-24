package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	agentspg "github.com/optimiware/kodama/internal/agents/postgres"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	deliverypg "github.com/optimiware/kodama/internal/delivery/postgres"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	gatespg "github.com/optimiware/kodama/internal/gates/postgres"
	gatewaypg "github.com/optimiware/kodama/internal/gateway/postgres"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	chatopspg "github.com/optimiware/kodama/internal/integrations/chatops/postgres"
	gitwebhookpg "github.com/optimiware/kodama/internal/integrations/gitwebhook/postgres"
	logspg "github.com/optimiware/kodama/internal/logs/postgres"
	mcppg "github.com/optimiware/kodama/internal/mcpproxy/postgres"
	metricspg "github.com/optimiware/kodama/internal/metrics/postgres"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	dependenciespg "github.com/optimiware/kodama/internal/work/dependencies/postgres"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
)

// Contributions lists the schema contributions of the core assembly in the
// order they must be applied. The manifest orders contributions as listed, not
// by task number, and the migration ledger refuses a pending migration ordered
// before an applied one: the order is part of the deployed contract and only
// ever grows at the end.
//
//	base → events → HTTP idempotency → identity → work → secrets → pipeline →
//	workspace → artifacts → execution (tables, workspace links, artifact and
//	secret-pin links) → runtime authority → execution workload cleanup →
//	log capture records → agent profiles → delivery (environments, deployments)
//	→ gates → model gateway → MCP proxy → work dependencies → flow summaries →
//	ChatOps → native Git triggers
//
// The runtime-authority schema serves the runtime credentials of the
// execution assembly (T-071) and the execution store of the composition is the
// runtime-enabled one (T-062), whose cancellation revokes grants in the same
// transaction. The log records reference artifacts and attempts, so they come
// next. The manual agent/gate/delivery slice (T-072) is appended at the end:
// delivery follows the execution, artifact and runtime-grant contributions it
// references and gates follow execution; agent profiles reference nothing. The
// order only ever grows at the end. The M2 composition (T-039) appends the
// model gateway and the MCP proxy after them: both reference projects, runs,
// step attempts and the secrets, which every earlier contribution has created.
// The business-loop composition (T-075) appends the dependency graph (it
// references work items and appends to their history), the flow-summary views
// (they read the work history) and ChatOps (it references projects and its
// authorization reads the identity tables) after those.
func Contributions() []storage.Contribution {
	cs := []storage.Contribution{
		storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(), workpg.Migrations(),
		secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(), artifactspg.Migrations(),
	}
	cs = append(cs, execpg.Schema()...)
	cs = append(cs, rtpg.Migrations(), execpg.CleanupMigrations(), logspg.Migrations(), agentspg.Migrations(), deliverypg.Migrations())
	cs = append(cs, gatespg.Migrations()...)
	cs = append(cs, gatewaypg.Migrations(), mcppg.Migrations())
	return append(cs, dependenciespg.Migrations(), metricspg.Migrations(), chatopspg.Migrations(), gitwebhookpg.Migrations())
}

// Manifest validates the contribution list.
func Manifest() (storage.Manifest, error) { return storage.NewManifest(Contributions()...) }

// MigrateConfig names the two database identities of a migration: the owner
// role that applies the schema and the runtime role that is granted access to
// it. They are never the same role.
type MigrateConfig struct {
	// MigrationURL connects as the owner (database-migration-url).
	MigrationURL string
	// RuntimeURL identifies the runtime role by its user name
	// (database-runtime-url); the migrations grant it its minimal privileges.
	// RuntimeRole names the role directly instead, for a migration Pod that
	// must not hold the runtime credential. Exactly one of them is set.
	RuntimeURL  string
	RuntimeRole string
	MaxConns    int32
}

// Migrate applies the manifest in order through the owner identity.
func Migrate(ctx context.Context, cfg MigrateConfig) (storage.MigrateResult, error) {
	if cfg.MigrationURL == "" || (cfg.RuntimeURL == "") == (cfg.RuntimeRole == "") {
		return storage.MigrateResult{}, errors.New("migration requires the owner database URL and exactly one of the runtime database URL or the runtime role name")
	}
	role := cfg.RuntimeRole
	if cfg.RuntimeURL != "" {
		var err error
		if role, err = roleOf(cfg.RuntimeURL); err != nil {
			return storage.MigrateResult{}, err
		}
	}
	if owner, err := roleOf(cfg.MigrationURL); err != nil {
		return storage.MigrateResult{}, err
	} else if owner == role {
		return storage.MigrateResult{}, errors.New("the migration and runtime identities must differ: the serving role must not own the schema")
	}
	manifest, err := Manifest()
	if err != nil {
		return storage.MigrateResult{}, err
	}
	conns := cfg.MaxConns
	if conns == 0 {
		conns = 2
	}
	pool, err := storage.Open(ctx, storage.Config{URL: cfg.MigrationURL, MaxConns: conns, ApplicationName: "kodama-migrate"})
	if err != nil {
		return storage.MigrateResult{}, fmt.Errorf("open migration pool: %w", err)
	}
	defer pool.Close()
	return storage.Migrate(ctx, pool, manifest, storage.MigrateOptions{RuntimeRole: role})
}

func roleOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil || u.User.Username() == "" {
		return "", errors.New("a database URL must name its role (postgres://role:password@host/db)")
	}
	return u.User.Username(), nil
}
