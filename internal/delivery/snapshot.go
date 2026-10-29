package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ConfigurationSnapshot is the immutable, ownership-scoped copy of an
// environment's effective configuration that a run pins at admission. Its
// digest covers the target, namespace, effective nonsecret configuration, the
// preview TTL and the exact credential bindings (reference, pinned version,
// name and delivery form). It never covers secret values or timestamps, so the
// same effective configuration always has the same identity.
type ConfigurationSnapshot struct {
	Scope       domain.Scope
	Environment domain.EnvironmentID
	Digest      runtimev1.Digest
	Kind        EnvironmentKind
	Target      string
	Namespace   string
	Config      map[string]string
	PreviewTTL  time.Duration
	// Kubeconfig names the binding of Secrets that carries the legacy
	// Kubernetes deployment credential.
	Kubeconfig string
	// TargetProfile and TargetProfileDigest pin the target definition;
	// TargetProvider, TargetArtifactKinds and TargetEgress are its resolved
	// values. They are empty for a legacy Kubernetes snapshot, whose digest keeps
	// the v1 format.
	TargetProfile       string
	TargetProfileDigest string
	TargetProvider      string
	TargetArtifactKinds []ArtifactKind
	TargetEgress        []string
	TargetConfig        map[string]string
	// Secrets are the exact bindings, sorted by name.
	Secrets   []runtimev1.SecretBinding
	CreatedAt time.Time
}

// Pin is the environment pin execution persists and carries into context.
func (s ConfigurationSnapshot) Pin() runtimev1.EnvironmentPin {
	return runtimev1.EnvironmentPin{ID: s.Environment, ConfigDigest: s.Digest}
}

type snapshotDocument struct {
	Format      string                    `json:"format"`
	Tenant      string                    `json:"tenant"`
	Project     string                    `json:"project"`
	Environment string                    `json:"environment"`
	Kind        string                    `json:"kind"`
	Target      string                    `json:"target"`
	Namespace   string                    `json:"namespace"`
	Config      map[string]string         `json:"config"`
	TTLSeconds  int64                     `json:"previewTtlSeconds"`
	Kubeconfig  string                    `json:"kubeconfig"`
	Secrets     []runtimev1.SecretBinding `json:"secrets"`
}

// snapshotDocumentV2 adds the target profile to the immutable identity. The v1
// document is left byte-for-byte unchanged so retained legacy snapshots keep
// their digest.
type snapshotDocumentV2 struct {
	Format              string                    `json:"format"`
	Tenant              string                    `json:"tenant"`
	Project             string                    `json:"project"`
	Environment         string                    `json:"environment"`
	Kind                string                    `json:"kind"`
	Target              string                    `json:"target"`
	Namespace           string                    `json:"namespace"`
	Config              map[string]string         `json:"config"`
	TTLSeconds          int64                     `json:"previewTtlSeconds"`
	Kubeconfig          string                    `json:"kubeconfig"`
	Secrets             []runtimev1.SecretBinding `json:"secrets"`
	TargetProfile       string                    `json:"targetProfile"`
	TargetProfileDigest string                    `json:"targetProfileDigest"`
	TargetProvider      string                    `json:"targetProvider"`
	TargetArtifactKinds []ArtifactKind            `json:"targetArtifactKinds"`
	TargetEgress        []string                  `json:"targetEgress"`
	TargetConfig        map[string]string         `json:"targetConfig"`
}

// ComputeDigest returns the canonical SHA-256 identity of the snapshot's
// content. encoding/json writes map keys in sorted order, so the encoding is
// deterministic.
func (s ConfigurationSnapshot) ComputeDigest() (runtimev1.Digest, error) {
	secrets := append([]runtimev1.SecretBinding{}, s.Secrets...)
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Name < secrets[j].Name })
	cfg := s.Config
	if cfg == nil {
		cfg = map[string]string{}
	}
	var doc any
	if s.TargetProfile == "" && s.TargetProfileDigest == "" && s.TargetProvider == "" && len(s.TargetArtifactKinds) == 0 && len(s.TargetEgress) == 0 && len(s.TargetConfig) == 0 {
		doc = snapshotDocument{
			Format: "kodama/environment-snapshot/v1", Tenant: string(s.Scope.TenantID), Project: string(s.Scope.ProjectID),
			Environment: string(s.Environment), Kind: string(s.Kind), Target: s.Target, Namespace: s.Namespace, Config: cfg,
			TTLSeconds: int64(s.PreviewTTL / time.Second), Kubeconfig: s.Kubeconfig, Secrets: secrets,
		}
	} else {
		targetCfg := s.TargetConfig
		if targetCfg == nil {
			targetCfg = map[string]string{}
		}
		kinds := append([]ArtifactKind{}, s.TargetArtifactKinds...)
		sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
		egress := append([]string{}, s.TargetEgress...)
		sort.Strings(egress)
		doc = snapshotDocumentV2{
			Format: "kodama/environment-snapshot/v2", Tenant: string(s.Scope.TenantID), Project: string(s.Scope.ProjectID),
			Environment: string(s.Environment), Kind: string(s.Kind), Target: s.Target, Namespace: s.Namespace, Config: cfg,
			TTLSeconds: int64(s.PreviewTTL / time.Second), Kubeconfig: s.Kubeconfig, Secrets: secrets,
			TargetProfile: s.TargetProfile, TargetProfileDigest: s.TargetProfileDigest, TargetProvider: s.TargetProvider,
			TargetArtifactKinds: kinds, TargetEgress: egress, TargetConfig: targetCfg,
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return runtimev1.Digest("sha256:" + hex.EncodeToString(sum[:])), nil
}

// NewSnapshot derives the snapshot of env with the credential versions the
// caller verified are current. Every credential of the environment needs exactly
// one version.
func NewSnapshot(env Environment, versions map[string]int64, now time.Time) (ConfigurationSnapshot, error) {
	rec := env.Record()
	s := ConfigurationSnapshot{
		Scope: rec.Scope, Environment: rec.ID, Kind: rec.Kind, Target: rec.Target, Namespace: rec.Namespace,
		Config: rec.Config, PreviewTTL: rec.PreviewTTL, Kubeconfig: rec.Kubeconfig.Name, CreatedAt: now.UTC(),
		TargetProfile: rec.TargetProfile, TargetProfileDigest: rec.TargetProfileDigest, TargetProvider: rec.TargetProvider,
		TargetArtifactKinds: rec.TargetArtifactKinds, TargetEgress: rec.TargetEgress, TargetConfig: rec.TargetConfig,
	}
	if s.TargetProfile != "" {
		// The legacy kubeconfig field is only a projection for old clients; the
		// profile's secrets list is the authoritative binding set.
		s.Kubeconfig = ""
	}
	for _, c := range env.credentials() {
		v, ok := versions[c.Ref]
		if !ok || v < 1 || v > domain.MaxSafeInteger {
			return ConfigurationSnapshot{}, Fail(ErrCredentialUnavailable, nil, "a credential has no pinned version")
		}
		s.Secrets = append(s.Secrets, runtimev1.SecretBinding{Ref: c.Ref, Version: v, Name: c.Name, Delivery: c.Delivery})
	}
	d, err := s.ComputeDigest()
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	s.Digest = d
	return s, nil
}

// Validate checks a restored or supplied snapshot, including that its digest
// covers its content.
func (s ConfigurationSnapshot) Validate() error {
	if err := s.Scope.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := s.Environment.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := s.Digest.Validate(); err != nil {
		return invalidf("the snapshot digest is malformed")
	}
	want, err := s.ComputeDigest()
	if err != nil || want != s.Digest {
		return Fail(ErrConflict, nil, "the snapshot digest does not cover its content")
	}
	if len(s.Secrets) == 0 || len(s.Secrets) > MaxScriptSecrets+1 {
		return invalidf("a snapshot carries its credential bindings")
	}
	if s.TargetProfile != "" {
		if !environmentName.MatchString(s.TargetProfile) || !digestPattern.MatchString(s.TargetProfileDigest) {
			return invalidf("a snapshot target profile is not pinned")
		}
		if !providerPattern.MatchString(s.TargetProvider) || len(s.TargetArtifactKinds) == 0 {
			return invalidf("a snapshot target is not resolved")
		}
		for _, k := range s.TargetArtifactKinds {
			if !k.Valid() {
				return invalidf("a snapshot carries an unknown artifact kind")
			}
		}
		if err := checkConfig(s.TargetConfig); err != nil {
			return err
		}
	} else if s.TargetProfileDigest != "" || s.TargetProvider != "" || len(s.TargetArtifactKinds) != 0 || len(s.TargetEgress) != 0 || len(s.TargetConfig) != 0 {
		return invalidf("a snapshot carries target values without a target profile")
	}
	return nil
}

// KubeconfigBinding returns the binding of the deployment credential.
func (s ConfigurationSnapshot) KubeconfigBinding() (runtimev1.SecretBinding, bool) {
	for _, b := range s.Secrets {
		if b.Name == s.Kubeconfig {
			return b, true
		}
	}
	return runtimev1.SecretBinding{}, false
}
