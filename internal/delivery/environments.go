package delivery

import (
	"context"
	"errors"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// EnvironmentInput is the definition of a new environment.
type EnvironmentInput struct {
	Name       string
	Kind       EnvironmentKind
	Target     string
	Namespace  string
	Kubeconfig Credential
	Secrets    []Credential
	Config     map[string]string
	// PreviewTTL defaults to the installation's preview TTL for a preview
	// environment and must be nil for a stage environment.
	PreviewTTL *time.Duration
}

// checkCredentials verifies, through the secrets owner and under the project's
// current authority, that every referenced credential exists, is not revoked,
// and that the deployment credential is a kubeconfig secret. It returns the
// current version of each reference.
func (s *Service) checkCredentials(ctx context.Context, scope domain.Scope, kube Credential, extra []Credential) (map[string]int64, error) {
	versions := map[string]int64{}
	for i, c := range append([]Credential{kube}, extra...) {
		info, err := s.cfg.Credentials.Current(ctx, scope, c.Ref)
		if err != nil {
			if errors.Is(err, ErrCredentialUnavailable) || errors.Is(err, ErrNotFound) {
				return nil, Fail(ErrCredentialUnavailable, err, "a credential is not available in this project")
			}
			return nil, Fail(ErrUnavailable, err, "the secrets owner could not answer")
		}
		if info.Ref != c.Ref || info.Version < 1 || info.Version > domain.MaxSafeInteger {
			return nil, Fail(ErrCredentialUnavailable, nil, "a credential is not available in this project")
		}
		if i == 0 && info.Kind != KindKubeconfig {
			return nil, Fail(ErrCredentialUnavailable, nil, "the deployment credential must be a kubeconfig secret")
		}
		versions[c.Ref] = info.Version
	}
	return versions, nil
}

// CreateEnvironment defines a project environment. The deployment credential
// must be a project-owned kubeconfig secret; target access stays
// namespace-scoped by that credential.
func (s *Service) CreateEnvironment(ctx context.Context, c Caller, scope domain.Scope, in EnvironmentInput) (env Environment, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "createEnvironment", err) }()
	if err := s.authorize(ctx, c, scope, PermDeploymentManage); err != nil {
		return Environment{}, err
	}
	m, err := s.mutation(c, "")
	if err != nil {
		return Environment{}, err
	}
	var ttl time.Duration
	switch {
	case in.Kind == KindPreview && in.PreviewTTL == nil:
		ttl = s.cfg.DefaultPreviewTTL
	case in.PreviewTTL != nil:
		ttl = *in.PreviewTTL
	}
	id, err := s.cfg.IDs.NewID("env")
	if err != nil {
		return Environment{}, err
	}
	env, err = NewEnvironment(EnvironmentRecord{
		Scope: scope, ID: domain.EnvironmentID(id), Name: in.Name, Kind: in.Kind, Target: in.Target, Namespace: in.Namespace,
		Kubeconfig: in.Kubeconfig, Secrets: in.Secrets, Config: in.Config, PreviewTTL: ttl,
	}, m.Now)
	if err != nil {
		return Environment{}, err
	}
	if _, err := s.checkCredentials(ctx, scope, in.Kubeconfig, in.Secrets); err != nil {
		return Environment{}, err
	}
	if err := s.cfg.Store.CreateEnvironment(ctx, m, env); err != nil {
		return Environment{}, err
	}
	return env, nil
}

// GetEnvironment returns an environment of the scope.
func (s *Service) GetEnvironment(ctx context.Context, c Caller, scope domain.Scope, id domain.EnvironmentID) (env Environment, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "getEnvironment", err) }()
	if err := s.authorize(ctx, c, scope, PermRunRead); err != nil {
		return Environment{}, err
	}
	if err := id.Validate(); err != nil {
		return Environment{}, Fail(ErrNotFound, err, "not found")
	}
	return s.cfg.Store.GetEnvironment(ctx, scope, id)
}

// ListEnvironments returns up to limit environments ordered by name.
func (s *Service) ListEnvironments(ctx context.Context, c Caller, scope domain.Scope, afterName string, limit int) (out []Environment, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "listEnvironments", err) }()
	if err := s.authorize(ctx, c, scope, PermRunRead); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 201 {
		return nil, invalidf("the page size is out of range")
	}
	return s.cfg.Store.ListEnvironments(ctx, scope, afterName, limit)
}

// UpdateEnvironment edits an environment if expected is its current version.
// Retained snapshots are immutable copies and are not affected: admitted runs
// keep their pinned configuration.
func (s *Service) UpdateEnvironment(ctx context.Context, c Caller, scope domain.Scope, id domain.EnvironmentID, expected domain.Version, edit EnvironmentEdit) (env Environment, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "updateEnvironment", err) }()
	if err := s.authorize(ctx, c, scope, PermDeploymentManage); err != nil {
		return Environment{}, err
	}
	if err := id.Validate(); err != nil {
		return Environment{}, Fail(ErrNotFound, err, "not found")
	}
	if err := expected.Validate(); err != nil {
		return Environment{}, invalidf("%v", err)
	}
	m, err := s.mutation(c, "")
	if err != nil {
		return Environment{}, err
	}
	if edit.Kubeconfig != nil || edit.Secrets != nil {
		// Only the references changing in this edit need a current lookup; the
		// store rechecks the merged definition's structure in the write.
		cur, err := s.cfg.Store.GetEnvironment(ctx, scope, id)
		if err != nil {
			return Environment{}, err
		}
		next := cur.Record()
		if edit.Kubeconfig != nil {
			next.Kubeconfig = *edit.Kubeconfig
		}
		if edit.Secrets != nil {
			next.Secrets = *edit.Secrets
		}
		if err := checkCredentials(next.Kubeconfig, next.Secrets); err != nil {
			return Environment{}, err
		}
		if _, err := s.checkCredentials(ctx, scope, next.Kubeconfig, next.Secrets); err != nil {
			return Environment{}, err
		}
	}
	return s.cfg.Store.UpdateEnvironment(ctx, m, scope, id, expected, edit)
}

// ResolveSnapshot is the admission-time resolution of a declared environment:
// under the actor's current authority it reads the environment's current
// definition and the current non-revoked version of every credential,
// derives the immutable configuration snapshot and retains it. A retained
// snapshot never changes afterwards. This is the only place current versions are
// read; callbacks and cleanup use retained snapshots only.
func (s *Service) ResolveSnapshot(ctx context.Context, scope domain.Scope, actor Actor, id domain.EnvironmentID) (snap ConfigurationSnapshot, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "resolveEnvironment", err) }()
	if err := s.authorize(ctx, Caller{Actor: actor}, scope, PermDeploymentManage); err != nil {
		return ConfigurationSnapshot{}, err
	}
	if err := id.Validate(); err != nil {
		return ConfigurationSnapshot{}, Fail(ErrNotFound, err, "not found")
	}
	env, err := s.cfg.Store.GetEnvironment(ctx, scope, id)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	rec := env.Record()
	versions, err := s.checkCredentials(ctx, scope, rec.Kubeconfig, rec.Secrets)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	snap, err = NewSnapshot(env, versions, s.now())
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	snap.CreatedAt = snap.CreatedAt.Truncate(time.Microsecond)
	retained, err := s.cfg.Store.RetainSnapshot(ctx, snap)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	if retained.Digest != snap.Digest || retained.Scope != scope || retained.Environment != id {
		return ConfigurationSnapshot{}, Fail(ErrUnavailable, nil, "the retained snapshot does not match")
	}
	return retained, nil
}

// RetainedSnapshot returns a retained snapshot by its pin. It is how delivery
// operations and the cleanup binding read the configuration an admitted run
// pinned.
func (s *Service) RetainedSnapshot(ctx context.Context, scope domain.Scope, env domain.EnvironmentID, digest runtimev1.Digest) (ConfigurationSnapshot, error) {
	snap, err := s.cfg.Store.GetSnapshot(ctx, scope, env, digest)
	return snap, Classify(err)
}
