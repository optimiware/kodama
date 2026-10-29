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
	Name                string
	Kind                EnvironmentKind
	Target              string
	Namespace           string
	Kubeconfig          Credential
	Secrets             []Credential
	Config              map[string]string
	TargetProfile       string
	TargetProfileDigest string
	TargetProvider      string
	TargetArtifactKinds []ArtifactKind
	TargetEgress        []string
	TargetConfig        map[string]string
	// PreviewTTL defaults to the installation's preview TTL for a preview
	// environment and must be nil for a stage or production environment.
	PreviewTTL *time.Duration
}

// checkCredentials verifies, through the secrets owner and under the project's
// current authority, that every referenced credential exists, is not revoked,
// and that the deployment credential is a kubeconfig secret. It returns the
// current version of each reference.
func (s *Service) checkCredentials(ctx context.Context, scope domain.Scope, kube Credential, extra []Credential) (map[string]int64, error) {
	return s.checkCredentialsFor(ctx, scope, nil, kube, extra)
}

// checkCredentialsFor verifies every binding through the secrets owner. A nil
// profile is the legacy Kubernetes environment: its sole deployment credential
// must be a kubeconfig. A profile environment checks the kind its profile names
// for each binding; a missing required binding is refused before any lookup.
func (s *Service) checkCredentialsFor(ctx context.Context, scope domain.Scope, profile *TargetProfile, kube Credential, extra []Credential) (map[string]int64, error) {
	creds := append([]Credential(nil), extra...)
	if kube.Name != "" || kube.Ref != "" || kube.Delivery != "" {
		creds = append(creds, kube)
	}
	if profile != nil {
		if missing := profile.MissingCredentials(creds); len(missing) != 0 {
			return nil, Fail(ErrCredentialUnavailable, nil, "the environment is missing target credentials")
		}
	}
	versions := map[string]int64{}
	for _, c := range creds {
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
		want := KindKubeconfig
		if profile != nil {
			want = profile.CredentialKind(c.Name)
		}
		if want != "" && info.Kind != want {
			return nil, Fail(ErrCredentialUnavailable, nil, "a credential has the wrong secret kind for this target")
		}
		versions[c.Ref] = info.Version
	}
	return versions, nil
}

// resolveTarget validates an environment's target profile and config and
// returns the pinned values the snapshot and the deployment driver use.
func hasCredential(in []Credential, c Credential) bool {
	for _, got := range in {
		if got == c || (got.Name == c.Name && got.Ref == c.Ref) {
			return true
		}
	}
	return false
}

func (s *Service) resolveTarget(profileName string, config map[string]string, digest string) (TargetProfile, string, map[string]string, error) {
	if profileName == "" {
		if len(config) != 0 || digest != "" {
			return TargetProfile{}, "", nil, invalidf("target configuration needs a target profile")
		}
		return TargetProfile{}, "", nil, nil
	}
	registry := s.cfg.Targets
	if registry == nil {
		registry = DefaultTargetRegistry()
	}
	profile, cfg, err := registry.Resolve(profileName, config)
	if err != nil {
		return TargetProfile{}, "", nil, err
	}
	want, err := profile.Digest()
	if err != nil {
		return TargetProfile{}, "", nil, err
	}
	if digest != "" && digest != want {
		return TargetProfile{}, "", nil, Fail(ErrVersionConflict, nil, "the target profile changed")
	}
	return profile, want, cfg, nil
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
	profile, digest, targetConfig, err := s.resolveTarget(in.TargetProfile, in.TargetConfig, in.TargetProfileDigest)
	if err != nil {
		return Environment{}, err
	}
	id, err := s.cfg.IDs.NewID("env")
	if err != nil {
		return Environment{}, err
	}
	rec := EnvironmentRecord{
		Scope: scope, ID: domain.EnvironmentID(id), Name: in.Name, Kind: in.Kind, Target: in.Target, Namespace: in.Namespace,
		Kubeconfig: in.Kubeconfig, Secrets: in.Secrets, Config: in.Config, PreviewTTL: ttl,
		TargetConfig: targetConfig,
	}
	var profilePtr *TargetProfile
	if profile.Name != "" {
		profilePtr = &profile
		creds := append([]Credential(nil), in.Secrets...)
		if in.Kubeconfig.Name != "" && !hasCredential(creds, in.Kubeconfig) {
			creds = append(creds, in.Kubeconfig)
		}
		if len(creds) == 0 {
			return Environment{}, invalidf("a profile environment needs at least one credential")
		}
		rec.Secrets, rec.Kubeconfig = creds, creds[0]
		rec.TargetProfile, rec.TargetProfileDigest = profile.Name, digest
		rec.TargetProvider, rec.TargetArtifactKinds, rec.TargetEgress = profile.Provider, profile.ArtifactKinds, profile.Egress
		if rec.Target == "" {
			rec.Target = profile.Name
		}
	}
	env, err = NewEnvironment(rec, m.Now)
	if err != nil {
		return Environment{}, err
	}
	if profile.Name != "" || in.Kubeconfig.Name != "" || len(in.Secrets) != 0 {
		legacyKube := in.Kubeconfig
		if profilePtr != nil {
			legacyKube = Credential{}
		}
		if _, err := s.checkCredentialsFor(ctx, scope, profilePtr, legacyKube, rec.Secrets); err != nil {
			return Environment{}, err
		}
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
	targetEdit := edit.TargetProfile != nil || edit.TargetConfig != nil || edit.TargetProfileDigest != nil
	if targetEdit || edit.Kubeconfig != nil || edit.Secrets != nil {
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
		var profile *TargetProfile
		if targetEdit {
			profileName, targetConfig, targetDigest := next.TargetProfile, next.TargetConfig, next.TargetProfileDigest
			if edit.TargetProfile != nil {
				profileName = *edit.TargetProfile
			}
			if edit.TargetConfig != nil {
				targetConfig = *edit.TargetConfig
			}
			if edit.TargetProfileDigest != nil {
				targetDigest = *edit.TargetProfileDigest
			}
			resolved, digest, cfg, err := s.resolveTarget(profileName, targetConfig, targetDigest)
			if err != nil {
				return Environment{}, err
			}
			if resolved.Name == "" {
				empty := ""
				edit.TargetProfile, edit.TargetProfileDigest, edit.TargetProvider = &empty, &empty, &empty
				noKinds, noEgress := []ArtifactKind{}, []string{}
				edit.TargetArtifactKinds, edit.TargetEgress = &noKinds, &noEgress
				edit.TargetConfig = &map[string]string{}
				next.TargetProfile, next.TargetProfileDigest, next.TargetProvider = "", "", ""
				next.TargetArtifactKinds, next.TargetEgress, next.TargetConfig = nil, nil, nil
			} else {
				profile = &resolved
				edit.TargetProfile, edit.TargetProfileDigest = &resolved.Name, &digest
				edit.TargetProvider = &resolved.Provider
				edit.TargetArtifactKinds = &resolved.ArtifactKinds
				edit.TargetEgress = &resolved.Egress
				edit.TargetConfig = &cfg
				next.TargetProfile, next.TargetProfileDigest, next.TargetProvider = resolved.Name, digest, resolved.Provider
				next.TargetArtifactKinds, next.TargetEgress, next.TargetConfig = resolved.ArtifactKinds, resolved.Egress, cfg
			}
		} else if next.TargetProfile != "" {
			resolved, _, _, err := s.resolveTarget(next.TargetProfile, next.TargetConfig, next.TargetProfileDigest)
			if err != nil {
				return Environment{}, err
			}
			profile = &resolved
		}
		if next.TargetProfile != "" {
			if len(next.Secrets) == 0 {
				return Environment{}, invalidf("a profile environment needs at least one credential")
			}
			next.Kubeconfig = next.Secrets[0]
			edit.Kubeconfig = &next.Kubeconfig
			if err := checkCredentials(Credential{}, next.Secrets); err != nil {
				return Environment{}, err
			}
			if _, err := s.checkCredentialsFor(ctx, scope, profile, Credential{}, next.Secrets); err != nil {
				return Environment{}, err
			}
		} else {
			if err := checkCredentials(next.Kubeconfig, next.Secrets); err != nil {
				return Environment{}, err
			}
			if _, err := s.checkCredentialsFor(ctx, scope, nil, next.Kubeconfig, next.Secrets); err != nil {
				return Environment{}, err
			}
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
	var profile *TargetProfile
	if rec.TargetProfile != "" {
		resolved, _, _, err := s.resolveTarget(rec.TargetProfile, rec.TargetConfig, rec.TargetProfileDigest)
		if err != nil {
			return ConfigurationSnapshot{}, err
		}
		profile = &resolved
	}
	legacyKube := rec.Kubeconfig
	if profile != nil {
		legacyKube = Credential{}
	}
	versions, err := s.checkCredentialsFor(ctx, scope, profile, legacyKube, rec.Secrets)
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
