package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Errors of the Git hosting store. The HTTP layer maps each to a stable code.
var (
	ErrVersionConflict    = errors.New("store: version conflict")
	ErrRepositoryNotReady = errors.New("store: repository not ready")
	ErrRepositoryArchived = errors.New("store: repository archived")
	ErrRepositoryNotEmpty = errors.New("store: repository not empty")
	ErrSeedPending        = errors.New("store: a seed receive is unacknowledged")
	ErrSeedConsumed       = errors.New("store: seed consumed")
)

// ReasonReconfiguration is the reason of a repository put back into provisioning
// so that its worker re-applies a changed default branch.
const ReasonReconfiguration = "reconfiguration"

// Repository status values (the contract's lifecycle, GR-02).
const (
	RepoProvisioning = "provisioning"
	RepoReady        = "ready"
	RepoArchived     = "archived"
)

// Credential kinds.
const (
	CredHuman       = "human"
	CredSeed        = "seed"
	CredRun         = "run"
	CredIntegration = "integration"
)

// seedPendingWindow bounds how long a consumed seed whose outcome was never
// recorded blocks a new grant: the longest receive deadline.
const seedPendingWindow = time.Hour

// RepositoryPolicy is repositories.policy. directPushToDefault defaults to false
// (O-33); integrationPipeline names the process whose runs may advance the
// default branch.
type RepositoryPolicy struct {
	DirectPushToDefault bool   `json:"directPushToDefault"`
	IntegrationPipeline string `json:"integrationPipeline,omitempty"`
}

// Repository is one repositories row. ProjectID is empty for the tenant-level
// repository. Empty and JournalAck are observations reported by the tenant's
// worker; they never change Version.
type Repository struct {
	TenantID      domain.TenantID  `json:"tenantId"`
	ProjectID     domain.ProjectID `json:"projectId,omitempty"`
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	DefaultBranch string           `json:"defaultBranch"`
	Status        string           `json:"status"`
	Reason        string           `json:"reason,omitempty"`
	Policy        RepositoryPolicy `json:"policy"`
	Empty         bool             `json:"empty"`
	JournalAck    int64            `json:"journalAck"`
	Version       domain.Version   `json:"version"`
	CreatedAt     time.Time        `json:"createdAt"`
	UpdatedAt     time.Time        `json:"updatedAt"`
}

// RepositoryPatch is a PATCH body: a nil member is unchanged.
type RepositoryPatch struct {
	DefaultBranch *string
	Policy        *RepositoryPolicy
}

const repoColumns = `project_id,id,name,default_branch,status,coalesce(reason,''),policy::jsonb,empty,journal_ack,version,created_at,updated_at`

type rowScanner interface{ Scan(...any) error }

func scanRepository(tenant domain.TenantID, row rowScanner) (Repository, error) {
	r := Repository{TenantID: tenant}
	var project *string
	var policy []byte
	if err := row.Scan(&project, &r.ID, &r.Name, &r.DefaultBranch, &r.Status, &r.Reason, &policy, &r.Empty,
		&r.JournalAck, &r.Version, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return Repository{}, err
	}
	if project != nil {
		r.ProjectID = domain.ProjectID(*project)
	}
	if err := json.Unmarshal(policy, &r.Policy); err != nil {
		return Repository{}, fmt.Errorf("repository %s: policy: %w", r.ID, err)
	}
	return r, nil
}

// GetProjectByKey resolves a project of a tenant by its key.
func (s *Store) GetProjectByKey(ctx context.Context, tenant domain.TenantID, key string) (Project, error) {
	p := Project{TenantID: tenant, Key: key}
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id,display_name,task_workflow::jsonb,settings::jsonb,version,created_at,updated_at
			FROM projects WHERE tenant_id=$1 AND key=$2`, tenant, key).
			Scan(&p.ID, &p.DisplayName, &p.Workflow, &p.Settings, &p.Version, &p.CreatedAt, &p.UpdatedAt)
	})
	return p, translate(err)
}

func nullable(project domain.ProjectID) any { return nullIfEmpty(string(project)) }

// CreateRepository reserves a repository (status provisioning) under the
// requested scope. It is idempotent on (tenant, project, name): a retry returns
// the existing row with created=false, and the caller decides whether the
// request matches it. The project association comes from the requested scope
// and the composite key rejects a project of another tenant.
func (s *Store) CreateRepository(ctx context.Context, r Repository) (Repository, bool, error) {
	if r.TenantID.Validate() != nil || domain.RepositoryID(r.ID).Validate() != nil {
		return Repository{}, false, fmt.Errorf("%w: repository id", domain.ErrInvalidValue)
	}
	policy, err := json.Marshal(r.Policy)
	if err != nil {
		return Repository{}, false, err
	}
	var out Repository
	var created bool
	err = s.inTenant(ctx, r.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO repositories (tenant_id,id,project_id,name,default_branch,policy)
			VALUES ($1,$2,$3,$4,$5,$6::jsonb)
			ON CONFLICT (tenant_id,project_id,name) DO NOTHING`,
			r.TenantID, r.ID, nullable(r.ProjectID), r.Name, r.DefaultBranch, string(policy))
		if err != nil {
			return err
		}
		created = tag.RowsAffected() == 1
		out, err = scanRepository(r.TenantID, tx.QueryRow(ctx, `SELECT `+repoColumns+` FROM repositories
			WHERE tenant_id=$1 AND project_id IS NOT DISTINCT FROM $2 AND name=$3`, r.TenantID, nullable(r.ProjectID), r.Name))
		return err
	})
	return out, created, translate(err)
}

func (s *Store) GetRepository(ctx context.Context, tenant domain.TenantID, id string) (Repository, error) {
	var out Repository
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = scanRepository(tenant, tx.QueryRow(ctx, `SELECT `+repoColumns+` FROM repositories WHERE tenant_id=$1 AND id=$2`, tenant, id))
		return err
	})
	return out, translate(err)
}

// RepositoryByPath resolves <tenant>/<projectKey>/<name>; an empty projectKey is
// the tenant-level repository. A path never creates anything.
func (s *Store) RepositoryByPath(ctx context.Context, tenant domain.TenantID, projectKey, name string) (Repository, error) {
	var out Repository
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if projectKey == "" {
			out, err = scanRepository(tenant, tx.QueryRow(ctx, `SELECT `+repoColumns+` FROM repositories
				WHERE tenant_id=$1 AND project_id IS NULL AND name=$2`, tenant, name))
			return err
		}
		out, err = scanRepository(tenant, tx.QueryRow(ctx, `SELECT r.project_id,r.id,r.name,r.default_branch,r.status,coalesce(r.reason,''),
			r.policy::jsonb,r.empty,r.journal_ack,r.version,r.created_at,r.updated_at
			FROM repositories r JOIN projects p ON p.tenant_id=r.tenant_id AND p.id=r.project_id
			WHERE r.tenant_id=$1 AND p.key=$2 AND r.name=$3`, tenant, projectKey, name))
		return err
	})
	return out, translate(err)
}

// ListRepositories lists a project's repositories, or the tenant-level ones when
// project is empty.
func (s *Store) ListRepositories(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) ([]Repository, error) {
	var out []Repository
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+repoColumns+` FROM repositories
			WHERE tenant_id=$1 AND project_id IS NOT DISTINCT FROM $2 ORDER BY name,id`, tenant, nullable(project))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRepository(tenant, rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, translate(err)
}

// UpdateRepository changes the default branch and/or policy under the expected
// version. A stale version is ErrVersionConflict, an unknown repository
// ErrNotFound.
//
// Changing the default branch moves Git's HEAD, which only the tenant's worker
// can do, so the repository goes back to provisioning with the reason
// reconfiguration: the existing idempotent initialize assignment re-applies HEAD
// and its confirmation returns the repository to ready. It is refused while the
// repository has refs (ErrRepositoryNotEmpty), is not ready, or while a seed
// receive is unacknowledged (ErrSeedPending), and it revokes any outstanding seed
// grant, whose claims name the old branch.
func (s *Store) UpdateRepository(ctx context.Context, tenant domain.TenantID, id string, expected domain.Version, patch RepositoryPatch) (Repository, error) {
	var policy any
	if patch.Policy != nil {
		raw, err := json.Marshal(patch.Policy)
		if err != nil {
			return Repository{}, err
		}
		policy = string(raw)
	}
	var out Repository
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := scanRepository(tenant, tx.QueryRow(ctx, `SELECT `+repoColumns+` FROM repositories
			WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, id))
		if err != nil {
			return err
		}
		if cur.Version != expected {
			return ErrVersionConflict
		}
		reconfigure := patch.DefaultBranch != nil && *patch.DefaultBranch != cur.DefaultBranch
		if reconfigure {
			switch {
			case cur.Status == RepoArchived:
				return ErrRepositoryArchived
			case cur.Status != RepoReady:
				return ErrRepositoryNotReady
			case !cur.Empty:
				return ErrRepositoryNotEmpty
			}
			var pending bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM git_credentials WHERE tenant_id=$1 AND repo_id=$2
				AND kind='seed' AND consumed_at IS NOT NULL AND settled_at IS NULL AND consumed_at > now() - $3::interval)`,
				tenant, id, fmt.Sprintf("%d seconds", int(seedPendingWindow.Seconds()))).Scan(&pending); err != nil {
				return err
			}
			if pending {
				return ErrSeedPending
			}
			if _, err := tx.Exec(ctx, `UPDATE git_credentials SET revoked_at=now(), version=version+1
				WHERE tenant_id=$1 AND repo_id=$2 AND kind='seed' AND consumed_at IS NULL AND revoked_at IS NULL`, tenant, id); err != nil {
				return err
			}
		}
		out, err = scanRepository(tenant, tx.QueryRow(ctx, `UPDATE repositories SET
			default_branch = coalesce($3, default_branch), policy = coalesce($4::jsonb, policy),
			status = CASE WHEN $5 THEN 'provisioning' ELSE status END,
			reason = CASE WHEN $5 THEN $6 ELSE reason END,
			version = version + 1, updated_at = now()
			WHERE tenant_id=$1 AND id=$2 RETURNING `+repoColumns, tenant, id, patch.DefaultBranch, policy, reconfigure, ReasonReconfiguration))
		return err
	})
	return out, translate(err)
}

// SetRepositoryStatus archives (ready to archived) or restores (archived to
// ready) a repository and commits repository.state_changed with the change. A
// repeat of the current state is a no-op returning the row.
func (s *Store) SetRepositoryStatus(ctx context.Context, tenant domain.TenantID, id, to, reason, actorType, actorID string) (Repository, error) {
	var out Repository
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := scanRepository(tenant, tx.QueryRow(ctx, `SELECT `+repoColumns+` FROM repositories
			WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, id))
		if err != nil {
			return err
		}
		out = cur
		if cur.Status == to {
			return nil
		}
		switch {
		case to == RepoArchived && cur.Status == RepoReady, to == RepoReady && cur.Status == RepoArchived:
		case cur.Status == RepoProvisioning:
			return ErrRepositoryNotReady
		default:
			return ErrConflict
		}
		out, err = scanRepository(tenant, tx.QueryRow(ctx, `UPDATE repositories SET status=$3, reason=nullif($4,''),
			version = version + 1, updated_at = now() WHERE tenant_id=$1 AND id=$2 RETURNING `+repoColumns, tenant, id, to, reason))
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"repositoryId": id, "from": cur.Status, "to": to, "reason": nullIfEmpty(reason)})
		return appendEventTx(ctx, tx, tenant, cur.ProjectID, "", actorType, actorID, "repository.state_changed", "repository", id, payload)
	})
	return out, translate(err)
}

// ConfirmInitialized records the worker's confirmation: provisioning becomes
// ready and repository.created is committed with it; empty is the worker's
// report. A repository that was reconfigured (a changed default branch) returns
// to ready with repository.state_changed instead, since it was created before. A
// repeat changes nothing (idempotent on the repository ID).
func (s *Store) ConfirmInitialized(ctx context.Context, tenant domain.TenantID, id, defaultBranch string, empty bool) (Repository, error) {
	var out Repository
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := scanRepository(tenant, tx.QueryRow(ctx, `SELECT `+repoColumns+` FROM repositories
			WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, id))
		if err != nil {
			return err
		}
		out = cur
		if cur.DefaultBranch != defaultBranch {
			return ErrConflict
		}
		if cur.Status != RepoProvisioning {
			return nil
		}
		out, err = scanRepository(tenant, tx.QueryRow(ctx, `UPDATE repositories SET status='ready', reason=NULL, empty=$3,
			version = version + 1, updated_at = now() WHERE tenant_id=$1 AND id=$2 RETURNING `+repoColumns, tenant, id, empty))
		if err != nil {
			return err
		}
		if cur.Reason == ReasonReconfiguration {
			payload, _ := json.Marshal(map[string]any{"repositoryId": id, "from": RepoProvisioning, "to": RepoReady, "reason": ReasonReconfiguration})
			return appendEventTx(ctx, tx, tenant, cur.ProjectID, "", "system", "git-worker", "repository.state_changed", "repository", id, payload)
		}
		payload, _ := json.Marshal(map[string]any{"repositoryId": id, "name": cur.Name, "defaultBranch": cur.DefaultBranch, "hosting": "internal"})
		return appendEventTx(ctx, tx, tenant, cur.ProjectID, "", "system", "git-worker", "repository.created", "repository", id, payload)
	})
	return out, translate(err)
}

// ProvisioningRepositories lists the repositories the worker must initialize
// (a new one, or one reconfigured for a changed default branch), in ID order.
func (s *Store) ProvisioningRepositories(ctx context.Context, tenant domain.TenantID, limit int) ([]Repository, error) {
	return s.repositoryPage(ctx, tenant, `status='provisioning'`, "", limit)
}

// ReconcilableRepositories lists up to limit repositories after afterID, in ID
// order, whose receive journal the worker reconciles: ready and archived ones.
// An archived repository refuses pushes but not recovery of a receive whose
// outcome was lost before the archive. The caller pages with the last ID.
func (s *Store) ReconcilableRepositories(ctx context.Context, tenant domain.TenantID, afterID string, limit int) ([]Repository, error) {
	return s.repositoryPage(ctx, tenant, `status IN ('ready','archived')`, afterID, limit)
}

func (s *Store) repositoryPage(ctx context.Context, tenant domain.TenantID, where, afterID string, limit int) ([]Repository, error) {
	var out []Repository
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+repoColumns+` FROM repositories
			WHERE tenant_id=$1 AND `+where+` AND id > $2 ORDER BY id LIMIT $3`, tenant, afterID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRepository(tenant, rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, translate(err)
}

// appendEventTx appends one audit/outbox row inside tx with a generated ID.
func appendEventTx(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, project domain.ProjectID, fixedID, actorType, actorID, eventType, resourceType, resourceID string, payload []byte) error {
	id := fixedID
	if id == "" {
		var err error
		if id, err = newID("evt"); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_events
		(tenant_id,project_id,id,actor_type,actor_id,event_type,resource_type,resource_id,payload)
		VALUES ($1,nullif($2,''),$3,$4,$5,$6,$7,$8,$9::jsonb)`,
		tenant, string(project), id, actorType, actorID, eventType, resourceType, resourceID, string(payload))
	return err
}

// ---- credentials ------------------------------------------------------------

// Credential is one git_credentials row. The secret is never stored: Digest is
// the hex SHA-256 of it. Claims is the issued record (a ceiling, not a frozen
// permission).
type Credential struct {
	TenantID        domain.TenantID  `json:"tenantId"`
	ProjectID       domain.ProjectID `json:"projectId,omitempty"`
	ID              string           `json:"id"`
	RepoID          string           `json:"repositoryId"`
	Kind            string           `json:"kind"`
	Digest          string           `json:"-"`
	Claims          json.RawMessage  `json:"claims"`
	UserID          string           `json:"userId,omitempty"`
	RunID           string           `json:"runId,omitempty"`
	IssuedAt        time.Time        `json:"issuedAt"`
	ExpiresAt       time.Time        `json:"expiresAt"`
	RevokedAt       *time.Time       `json:"revokedAt,omitempty"`
	ConsumedAt      *time.Time       `json:"consumedAt,omitempty"`
	ConsumedReceive string           `json:"consumedReceive,omitempty"`
	SettledAt       *time.Time       `json:"settledAt,omitempty"`
	Version         domain.Version   `json:"version"`
}

// Live reports whether the credential is unrevoked and unexpired at now.
func (c Credential) Live(now time.Time) bool {
	return c.RevokedAt == nil && now.Before(c.ExpiresAt)
}

// DigestOf is the stored form of a credential secret.
func DigestOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

const credColumns = `project_id,id,repo_id,kind,secret_digest,claims::jsonb,coalesce(user_id,''),coalesce(run_id,''),
	issued_at,expires_at,revoked_at,consumed_at,coalesce(consumed_receive,''),settled_at,version`

func scanCredential(tenant domain.TenantID, row rowScanner) (Credential, error) {
	c := Credential{TenantID: tenant}
	var project *string
	if err := row.Scan(&project, &c.ID, &c.RepoID, &c.Kind, &c.Digest, &c.Claims, &c.UserID, &c.RunID, &c.IssuedAt,
		&c.ExpiresAt, &c.RevokedAt, &c.ConsumedAt, &c.ConsumedReceive, &c.SettledAt, &c.Version); err != nil {
		return Credential{}, err
	}
	if project != nil {
		c.ProjectID = domain.ProjectID(*project)
	}
	return c, nil
}

func insertCredential(ctx context.Context, tx pgx.Tx, c Credential) error {
	_, err := tx.Exec(ctx, `INSERT INTO git_credentials
		(tenant_id,project_id,id,repo_id,kind,secret_digest,claims,user_id,run_id,issued_at,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,nullif($8,''),nullif($9,''),$10,$11)`,
		c.TenantID, nullable(c.ProjectID), c.ID, c.RepoID, c.Kind, c.Digest, string(c.Claims), c.UserID, c.RunID, c.IssuedAt, c.ExpiresAt)
	return err
}

// IssueCredential stores a human, run or integration credential.
func (s *Store) IssueCredential(ctx context.Context, c Credential) error {
	if c.Kind == CredSeed {
		return fmt.Errorf("%w: a seed is issued with IssueSeed", domain.ErrInvalidValue)
	}
	return translate(s.inTenant(ctx, c.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return insertCredential(ctx, tx, c)
	}))
}

// IssueSeed stores a seed credential after re-checking eligibility under the
// repository lock: ready (archived and provisioning have their own errors), no
// refs, and no earlier seed receive left unacknowledged. An outstanding
// unconsumed seed is revoked: one request replaces the previous grant.
func (s *Store) IssueSeed(ctx context.Context, c Credential) error {
	return translate(s.inTenant(ctx, c.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var status string
		var empty bool
		if err := tx.QueryRow(ctx, `SELECT status,empty FROM repositories WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			c.TenantID, c.RepoID).Scan(&status, &empty); err != nil {
			return err
		}
		switch {
		case status == RepoArchived:
			return ErrRepositoryArchived
		case status != RepoReady:
			return ErrRepositoryNotReady
		case !empty:
			return ErrRepositoryNotEmpty
		}
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM git_credentials WHERE tenant_id=$1 AND repo_id=$2
			AND kind='seed' AND consumed_at IS NOT NULL AND settled_at IS NULL AND consumed_at > now() - $3::interval)`,
			c.TenantID, c.RepoID, fmt.Sprintf("%d seconds", int(seedPendingWindow.Seconds()))).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return ErrSeedPending
		}
		if _, err := tx.Exec(ctx, `UPDATE git_credentials SET revoked_at=now(), version=version+1
			WHERE tenant_id=$1 AND repo_id=$2 AND kind='seed' AND consumed_at IS NULL AND revoked_at IS NULL`,
			c.TenantID, c.RepoID); err != nil {
			return err
		}
		return insertCredential(ctx, tx, c)
	}))
}

func (s *Store) CredentialByDigest(ctx context.Context, tenant domain.TenantID, digest string) (Credential, error) {
	var c Credential
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, err = scanCredential(tenant, tx.QueryRow(ctx, `SELECT `+credColumns+` FROM git_credentials
			WHERE tenant_id=$1 AND secret_digest=$2`, tenant, digest))
		return err
	})
	return c, translate(err)
}

func (s *Store) GetCredential(ctx context.Context, tenant domain.TenantID, id string) (Credential, error) {
	var c Credential
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, err = scanCredential(tenant, tx.QueryRow(ctx, `SELECT `+credColumns+` FROM git_credentials
			WHERE tenant_id=$1 AND id=$2`, tenant, id))
		return err
	})
	return c, translate(err)
}

// AuthorizeSeed consumes a seed credential for one receive: the first receive
// wins, a repeat of the same receive is idempotent, any other receive with the
// same grant is ErrSeedConsumed, and a different seed of a repository that has
// an unsettled consumed seed (a concurrent seed) is ErrSeedConsumed as well. The
// repository row is locked, so concurrent seeds authorize at most one.
func (s *Store) AuthorizeSeed(ctx context.Context, tenant domain.TenantID, credentialID, receiveID string) error {
	return translate(s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var repo string
		if err := tx.QueryRow(ctx, `SELECT repo_id FROM git_credentials WHERE tenant_id=$1 AND id=$2 AND kind='seed'`,
			tenant, credentialID).Scan(&repo); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM repositories WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, repo); err != nil {
			return err
		}
		var consumed *time.Time
		var receive string
		var revoked *time.Time
		if err := tx.QueryRow(ctx, `SELECT consumed_at, coalesce(consumed_receive,''), revoked_at FROM git_credentials
			WHERE tenant_id=$1 AND id=$2`, tenant, credentialID).Scan(&consumed, &receive, &revoked); err != nil {
			return err
		}
		if consumed != nil {
			if receive == receiveID {
				return nil
			}
			return ErrSeedConsumed
		}
		if revoked != nil {
			return ErrSeedConsumed
		}
		var other bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM git_credentials WHERE tenant_id=$1 AND repo_id=$2
			AND kind='seed' AND id<>$3 AND consumed_at IS NOT NULL AND settled_at IS NULL AND consumed_at > now() - $4::interval)`,
			tenant, repo, credentialID, fmt.Sprintf("%d seconds", int(seedPendingWindow.Seconds()))).Scan(&other); err != nil {
			return err
		}
		if other {
			return ErrSeedConsumed
		}
		_, err := tx.Exec(ctx, `UPDATE git_credentials SET consumed_at=now(), consumed_receive=$3, version=version+1
			WHERE tenant_id=$1 AND id=$2 AND consumed_at IS NULL`, tenant, credentialID, receiveID)
		return err
	}))
}

// RevokeRunCredentials revokes every credential of a run (it ended).
func (s *Store) RevokeRunCredentials(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID string) error {
	return translate(s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return revokeRunCredentialsTx(ctx, tx, tenant, project, runID)
	}))
}

func revokeRunCredentialsTx(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, project domain.ProjectID, runID string) error {
	_, err := tx.Exec(ctx, `UPDATE git_credentials SET revoked_at=now(), version=version+1
		WHERE tenant_id=$1 AND project_id=$2 AND run_id=$3 AND revoked_at IS NULL`, tenant, project, runID)
	return err
}

// RunGitInfo is what Git authority needs of a run, read at every use.
type RunGitInfo struct {
	TenantID     domain.TenantID  `json:"tenantId"`
	ProjectID    domain.ProjectID `json:"projectId"`
	RunID        string           `json:"runId"`
	PipelineName string           `json:"pipelineName"`
	TaskID       string           `json:"taskId,omitempty"`
	Status       string           `json:"status"`
	Cancelling   bool             `json:"cancelling"`
	Sources      json.RawMessage  `json:"sources"`
}

// Active reports whether the run still holds Git authority: queued or running
// and not cancelled.
func (r RunGitInfo) Active() bool {
	return (r.Status == "queued" || r.Status == "running") && !r.Cancelling
}

func (s *Store) RunGitInfo(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID string) (RunGitInfo, error) {
	r := RunGitInfo{TenantID: tenant, ProjectID: project, RunID: runID}
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT p.name, coalesce(r.task_id,''), r.status, r.cancel_requested_at IS NOT NULL, r.sources::jsonb
			FROM runs r JOIN pipelines p ON p.tenant_id=r.tenant_id AND p.project_id=r.project_id AND p.id=r.pipeline_id
			WHERE r.tenant_id=$1 AND r.project_id=$2 AND r.id=$3`, tenant, project, runID).
			Scan(&r.PipelineName, &r.TaskID, &r.Status, &r.Cancelling, &r.Sources)
	})
	return r, translate(err)
}

// ---- receive outcome --------------------------------------------------------

// ReceiveUpdate is one applied update with its derived kind.
type ReceiveUpdate struct {
	Ref  string `json:"ref"`
	Old  string `json:"old"`
	New  string `json:"new"`
	Kind string `json:"kind"`
}

// ReceiveCommit is an outcome to record: the applied updates of one receive and
// the worker's journal sequence (0: none to acknowledge).
type ReceiveCommit struct {
	TenantID        domain.TenantID
	RepositoryID    string
	ReceiveID       string
	ActorType       string // user | workload
	ActorID         string
	OriginRunID     string
	Updates         []ReceiveUpdate
	JournalSequence int64
	// JournalPosition is the receive's own position in the worker's journal
	// (gitv1.OutcomeRequest.JournalPosition); 0: unknown. It rides on the event.
	JournalPosition int64
}

// ReceiveResult reports the recorded outcome. Created is false when the receive
// was already recorded.
type ReceiveResult struct {
	EventID    string
	Created    bool
	JournalAck int64
}

// GitReceiveEventID is the deterministic audit_events ID of a receive: the same
// (repository, receive) always yields the same row, so a retried or re-reported
// outcome emits exactly one event.
func GitReceiveEventID(repositoryID, receiveID string) string {
	sum := sha256.Sum256([]byte(repositoryID + "\x00" + receiveID))
	return "gp_" + hex.EncodeToString(sum[:20])
}

func isZeroOID(oid string) bool {
	for _, c := range oid {
		if c != '0' {
			return false
		}
	}
	return oid != ""
}

// eventUpdates are the updates a git.refs_updated event carries: every applied
// update except one of a server ref (refs/kodama/...), which is never in an event.
func eventUpdates(updates []ReceiveUpdate) []ReceiveUpdate {
	out := make([]ReceiveUpdate, 0, len(updates))
	for _, u := range updates {
		if !strings.HasPrefix(u.Ref, "refs/kodama/") {
			out = append(out, u)
		}
	}
	return out
}

// GitRefsUpdatedPayload is the payload of git.refs_updated, in the inline form of
// the event contract (eventsv1.GitRefsUpdated): the receive, the repository, who
// pushed (originRunId an explicit null for a person) and the applied updates. The
// payload is validated against the contract before it is stored, so an event a
// consumer would refuse is never committed.
func GitRefsUpdatedPayload(c ReceiveCommit) ([]byte, error) {
	var origin *string
	if c.OriginRunID != "" {
		origin = &c.OriginRunID
	}
	updates := eventUpdates(c.Updates)
	var position *int64
	if c.JournalPosition > 0 {
		position = &c.JournalPosition
	}
	ev := eventsv1.GitRefsUpdated{JournalPosition: position, ReceiveID: c.ReceiveID, RepositoryID: c.RepositoryID,
		Actor: eventsv1.GitRefActor{Type: c.ActorType, ID: c.ActorID}, OriginRunID: origin,
		Updates: make([]eventsv1.GitRefUpdate, len(updates))}
	for i, u := range updates {
		ev.Updates[i] = eventsv1.GitRefUpdate{Ref: u.Ref, Old: u.Old, New: u.New, Kind: u.Kind}
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	if _, err := eventsv1.DecodeGitRefsUpdated(raw); err != nil {
		return nil, fmt.Errorf("%w: git.refs_updated: %v", domain.ErrInvalidValue, err)
	}
	return raw, nil
}

// CommitReceive records a receive outcome in one transaction: the
// git.refs_updated row under its deterministic ID (when something applied), the
// repository's journal_ack (never lowered) and emptiness, and the settlement of
// a seed whose receive this was.
func (s *Store) CommitReceive(ctx context.Context, c ReceiveCommit) (ReceiveResult, error) {
	res := ReceiveResult{}
	var payload []byte
	if len(eventUpdates(c.Updates)) > 0 {
		res.EventID = GitReceiveEventID(c.RepositoryID, c.ReceiveID)
		var perr error
		if payload, perr = GitRefsUpdatedPayload(c); perr != nil {
			return ReceiveResult{}, perr
		}
	}
	applied := false
	for _, u := range c.Updates {
		if !isZeroOID(u.New) {
			applied = true
		}
	}
	err := s.inTenant(ctx, c.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var project *string
		if err := tx.QueryRow(ctx, `SELECT project_id FROM repositories WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			c.TenantID, c.RepositoryID).Scan(&project); err != nil {
			return err
		}
		if payload != nil {
			tag, err := tx.Exec(ctx, `INSERT INTO audit_events
				(tenant_id,project_id,id,actor_type,actor_id,event_type,resource_type,resource_id,payload)
				VALUES ($1,$2,$3,$4,$5,'git.refs_updated','git_push',$6,$7::jsonb)
				ON CONFLICT (tenant_id,id) DO NOTHING`,
				c.TenantID, project, res.EventID, c.ActorType, c.ActorID, c.ReceiveID, string(payload))
			if err != nil {
				return err
			}
			res.Created = tag.RowsAffected() == 1
		}
		if err := tx.QueryRow(ctx, `UPDATE repositories SET journal_ack = GREATEST(journal_ack,$3),
			empty = empty AND NOT $4 WHERE tenant_id=$1 AND id=$2 RETURNING journal_ack`,
			c.TenantID, c.RepositoryID, c.JournalSequence, applied).Scan(&res.JournalAck); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE git_credentials SET settled_at=now(), version=version+1
			WHERE tenant_id=$1 AND repo_id=$2 AND consumed_receive=$3 AND settled_at IS NULL`,
			c.TenantID, c.RepositoryID, c.ReceiveID)
		return err
	})
	return res, translate(err)
}
