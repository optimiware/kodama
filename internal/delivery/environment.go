package delivery

import (
	"regexp"
	"sort"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// EnvironmentKind says what an environment is for. A preview environment has a
// preview TTL (O-11: configurable, 24 hours by default); a stage environment
// has none and is never expired by a schedule.
type EnvironmentKind string

const (
	KindPreview    EnvironmentKind = "preview"
	KindStage      EnvironmentKind = "stage"
	KindProduction EnvironmentKind = "production"
)

// DefaultPreviewTTL is the approved default (O-11). It is configurable per
// installation (Config.DefaultPreviewTTL) and per environment.
const DefaultPreviewTTL = 24 * time.Hour

// Bounds of an environment definition.
const (
	MaxPreviewTTL     = 30 * 24 * time.Hour
	MinPreviewTTL     = time.Minute
	MaxConfigEntries  = 64
	MaxConfigValue    = 1024
	MaxScriptSecrets  = 32
	DefaultKubeconfig = "KUBECONFIG"
)

var (
	environmentName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	targetName      = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,62}$`)
	namespaceName   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	configKey       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	bindingName     = regexp.MustCompile(runtimev1.Patterns["SafeName"])
	projectRef      = regexp.MustCompile(`^secret://project/[a-z][a-z0-9-]{0,62}$`)
)

// Credential is a reference to a project secret and the explicit form in which
// a script receives it (O-19). It never carries a value or a version: versions
// are pinned in the immutable configuration snapshot.
type Credential struct {
	Name     string                   `json:"name"`
	Ref      string                   `json:"ref"`
	Delivery runtimev1.SecretDelivery `json:"delivery"`
}

// Validate checks the credential.
func (c Credential) Validate() error {
	switch {
	case !bindingName.MatchString(c.Name):
		return invalidf("a credential binding name must match %s", runtimev1.Patterns["SafeName"])
	case !projectRef.MatchString(c.Ref):
		return invalidf("a credential reference must be secret://project/<name>")
	case c.Delivery != runtimev1.DeliveryFile && c.Delivery != runtimev1.DeliveryEnv:
		return invalidf("a credential is delivered as file or env")
	}
	return nil
}

// EnvironmentRecord is the plain persisted state of an Environment.
type EnvironmentRecord struct {
	Scope domain.Scope
	ID    domain.EnvironmentID
	Name  string
	Kind  EnvironmentKind
	// Target is the label of the cluster or target; Namespace the single
	// namespace a deployment may touch. Both are legacy Kubernetes fields.
	Target    string
	Namespace string
	// Kubeconfig is the project-owned, namespace-scoped kubeconfig secret the
	// legacy Kubernetes deployment script uses. Secrets are further explicit
	// script credentials (and, for a target profile, every required binding).
	Kubeconfig Credential
	Secrets    []Credential
	// TargetProfile selects the installation's target profile. Empty means the
	// legacy Kubernetes behavior. TargetProfileDigest pins the profile content;
	// TargetProvider, TargetArtifactKinds and TargetEgress are its resolved
	// values, copied into every snapshot so a later profile edit cannot change an
	// admitted run.
	TargetProfile       string
	TargetProfileDigest string
	TargetProvider      string
	TargetArtifactKinds []ArtifactKind
	TargetEgress        []string
	TargetConfig        map[string]string
	// Config is explicit nonsecret runtime configuration.
	Config     map[string]string
	PreviewTTL time.Duration
	Version    domain.Version
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Environment is a named delivery target of a project: the aggregate that owns
// the editable definition. Snapshots (immutable) are derived from it.
type Environment struct{ rec EnvironmentRecord }

// NewEnvironment validates and creates version 1.
func NewEnvironment(rec EnvironmentRecord, now time.Time) (Environment, error) {
	if rec.TargetProfile != "" && rec.Target == "" {
		rec.Target = rec.TargetProfile
	}
	rec.Version, rec.CreatedAt, rec.UpdatedAt = 1, now.UTC(), now.UTC()
	return RestoreEnvironment(rec)
}

// RestoreEnvironment rebuilds an environment from persisted state.
func RestoreEnvironment(rec EnvironmentRecord) (Environment, error) {
	if err := rec.Scope.Validate(); err != nil {
		return Environment{}, invalidf("%v", err)
	}
	if err := rec.ID.Validate(); err != nil {
		return Environment{}, invalidf("%v", err)
	}
	if !environmentName.MatchString(rec.Name) {
		return Environment{}, invalidf("an environment name must match %s", environmentName)
	}
	switch rec.Kind {
	case KindPreview:
		if rec.PreviewTTL < MinPreviewTTL || rec.PreviewTTL > MaxPreviewTTL {
			return Environment{}, invalidf("a preview TTL must be between %s and %s", MinPreviewTTL, MaxPreviewTTL)
		}
	case KindStage, KindProduction:
		if rec.PreviewTTL != 0 {
			return Environment{}, invalidf("a %s environment has no preview TTL", rec.Kind)
		}
	default:
		return Environment{}, invalidf("an environment is a preview, stage or production")
	}
	if rec.PreviewTTL%time.Second != 0 {
		return Environment{}, invalidf("a preview TTL is a whole number of seconds")
	}
	if rec.TargetProfile == "" {
		// Legacy Kubernetes environment: the namespace and namespace-scoped
		// kubeconfig are the target.
		if !targetName.MatchString(rec.Target) {
			return Environment{}, invalidf("an environment target must match %s", targetName)
		}
		if !namespaceName.MatchString(rec.Namespace) {
			return Environment{}, invalidf("an environment namespace must be a Kubernetes DNS label")
		}
		if rec.Kubeconfig.Name == "" {
			return Environment{}, invalidf("a Kubernetes environment needs a kubeconfig credential")
		}
		if err := checkCredentials(rec.Kubeconfig, rec.Secrets); err != nil {
			return Environment{}, err
		}
	} else {
		if !environmentName.MatchString(rec.TargetProfile) {
			return Environment{}, invalidf("a target profile name must match %s", environmentName)
		}
		if rec.TargetProfileDigest == "" || !digestPattern.MatchString(rec.TargetProfileDigest) {
			return Environment{}, invalidf("a target profile digest is required")
		}
		if !providerPattern.MatchString(rec.TargetProvider) {
			return Environment{}, invalidf("a target provider must match %s", providerPattern)
		}
		if len(rec.TargetArtifactKinds) == 0 {
			return Environment{}, invalidf("a target profile accepts at least one artifact kind")
		}
		for _, k := range rec.TargetArtifactKinds {
			if !k.Valid() {
				return Environment{}, invalidf("unknown artifact kind %q", k)
			}
		}
		if len(rec.TargetEgress) > 8 {
			return Environment{}, invalidf("an environment lists at most 8 egress profiles")
		}
		for _, e := range rec.TargetEgress {
			if !egressNamePattern.MatchString(e) {
				return Environment{}, invalidf("an egress profile name must match %s", egressNamePattern)
			}
		}
		if err := checkConfig(rec.TargetConfig); err != nil {
			return Environment{}, err
		}
		if rec.Namespace != "" {
			return Environment{}, invalidf("a profile environment has no Kubernetes namespace")
		}
		if len(rec.Secrets) == 0 {
			return Environment{}, invalidf("a profile environment needs at least one credential")
		}
		if err := checkCredentials(Credential{}, rec.Secrets); err != nil {
			return Environment{}, err
		}
		if rec.Kubeconfig.Name != "" {
			if err := rec.Kubeconfig.Validate(); err != nil {
				return Environment{}, err
			}
		}
	}
	if err := checkConfig(rec.Config); err != nil {
		return Environment{}, err
	}
	if err := rec.Version.Validate(); err != nil {
		return Environment{}, invalidf("%v", err)
	}
	if rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() {
		return Environment{}, invalidf("environment timestamps are required")
	}
	rec.Secrets = append([]Credential(nil), rec.Secrets...)
	rec.Config = cloneConfig(rec.Config)
	return Environment{rec: rec}, nil
}

// checkCredentials enforces the binding rules execution relies on: unique
// names, and one binding per reference. A runtime resolves a credential by
// reference, so one reference under two names or forms could not be delivered.
func checkCredentials(kube Credential, extra []Credential) error {
	if kube.Name != "" || kube.Ref != "" || kube.Delivery != "" {
		if err := kube.Validate(); err != nil {
			return err
		}
	}
	if len(extra) > MaxScriptSecrets {
		return invalidf("an environment lists at most %d script credentials", MaxScriptSecrets)
	}
	names, refs := map[string]bool{}, map[string]bool{}
	if kube.Name != "" {
		names[kube.Name], refs[kube.Ref] = true, true
	}
	for _, c := range extra {
		if err := c.Validate(); err != nil {
			return err
		}
		if names[c.Name] {
			return invalidf("credential binding names must be unique")
		}
		if refs[c.Ref] {
			return invalidf("a credential reference is bound once")
		}
		names[c.Name], refs[c.Ref] = true, true
	}
	return nil
}

func checkConfig(cfg map[string]string) error {
	if len(cfg) > MaxConfigEntries {
		return invalidf("an environment holds at most %d configuration entries", MaxConfigEntries)
	}
	for k, v := range cfg {
		if !configKey.MatchString(k) {
			return invalidf("a configuration key must match %s", configKey)
		}
		if len(v) > MaxConfigValue || strings.ContainsRune(v, 0) {
			return invalidf("a configuration value is at most %d bytes of text", MaxConfigValue)
		}
	}
	return nil
}

func cloneConfig(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Record returns a copy of the persisted state.
func (e Environment) Record() EnvironmentRecord {
	r := e.rec
	r.Secrets = append([]Credential(nil), r.Secrets...)
	r.Config = cloneConfig(r.Config)
	r.TargetArtifactKinds = append([]ArtifactKind(nil), r.TargetArtifactKinds...)
	r.TargetEgress = append([]string(nil), r.TargetEgress...)
	r.TargetConfig = cloneConfig(r.TargetConfig)
	return r
}

func (e Environment) Scope() domain.Scope      { return e.rec.Scope }
func (e Environment) ID() domain.EnvironmentID { return e.rec.ID }
func (e Environment) Name() string             { return e.rec.Name }
func (e Environment) Version() domain.Version  { return e.rec.Version }

// EnvironmentEdit lists the editable members; nil leaves one unchanged. The
// identity, name and kind never change.
type EnvironmentEdit struct {
	Target              *string
	Namespace           *string
	Kubeconfig          *Credential
	Secrets             *[]Credential
	Config              *map[string]string
	PreviewTTL          *time.Duration
	TargetProfile       *string
	TargetProfileDigest *string
	TargetProvider      *string
	TargetArtifactKinds *[]ArtifactKind
	TargetEgress        *[]string
	TargetConfig        *map[string]string
}

// Edit applies an edit if expected is the current resource version. An edit
// that changes nothing leaves the version alone. Snapshots already retained are
// unaffected: they are immutable copies.
func (e *Environment) Edit(expected domain.Version, edit EnvironmentEdit, now time.Time) (changed bool, err error) {
	if expected != e.rec.Version {
		return false, Fail(ErrVersionConflict, nil, "the environment changed")
	}
	next := e.Record()
	if edit.Target != nil {
		next.Target = *edit.Target
	}
	if edit.Namespace != nil {
		next.Namespace = *edit.Namespace
	}
	if edit.Kubeconfig != nil {
		next.Kubeconfig = *edit.Kubeconfig
	}
	if edit.Secrets != nil {
		next.Secrets = append([]Credential(nil), (*edit.Secrets)...)
	}
	if edit.Config != nil {
		next.Config = cloneConfig(*edit.Config)
	}
	if edit.PreviewTTL != nil {
		next.PreviewTTL = *edit.PreviewTTL
	}
	if edit.TargetProfile != nil {
		next.TargetProfile = *edit.TargetProfile
	}
	if edit.TargetProfileDigest != nil {
		next.TargetProfileDigest = *edit.TargetProfileDigest
	}
	if edit.TargetProvider != nil {
		next.TargetProvider = *edit.TargetProvider
	}
	if edit.TargetArtifactKinds != nil {
		next.TargetArtifactKinds = append([]ArtifactKind(nil), (*edit.TargetArtifactKinds)...)
	}
	if edit.TargetEgress != nil {
		next.TargetEgress = append([]string(nil), (*edit.TargetEgress)...)
	}
	if edit.TargetConfig != nil {
		next.TargetConfig = cloneConfig(*edit.TargetConfig)
	}
	if next.TargetProfile != "" && next.Target == "" {
		next.Target = next.TargetProfile
	}
	if sameEnvironment(e.rec, next) {
		return false, nil
	}
	next.Version++
	next.UpdatedAt = now.UTC()
	restored, err := RestoreEnvironment(next)
	if err != nil {
		return false, err
	}
	*e = restored
	return true, nil
}

func sameEnvironment(a, b EnvironmentRecord) bool {
	if a.Target != b.Target || a.Namespace != b.Namespace || a.Kubeconfig != b.Kubeconfig || a.PreviewTTL != b.PreviewTTL ||
		a.TargetProfile != b.TargetProfile || a.TargetProfileDigest != b.TargetProfileDigest || a.TargetProvider != b.TargetProvider ||
		len(a.Secrets) != len(b.Secrets) || len(a.Config) != len(b.Config) || len(a.TargetConfig) != len(b.TargetConfig) ||
		len(a.TargetArtifactKinds) != len(b.TargetArtifactKinds) || len(a.TargetEgress) != len(b.TargetEgress) {
		return false
	}
	for i := range a.Secrets {
		if a.Secrets[i] != b.Secrets[i] {
			return false
		}
	}
	for k, v := range a.Config {
		if w, ok := b.Config[k]; !ok || w != v {
			return false
		}
	}
	for k, v := range a.TargetConfig {
		if w, ok := b.TargetConfig[k]; !ok || w != v {
			return false
		}
	}
	for i := range a.TargetArtifactKinds {
		if a.TargetArtifactKinds[i] != b.TargetArtifactKinds[i] {
			return false
		}
	}
	for i := range a.TargetEgress {
		if a.TargetEgress[i] != b.TargetEgress[i] {
			return false
		}
	}
	return true
}

// credentials lists every credential in a stable order.
func (e Environment) credentials() []Credential {
	out := append([]Credential(nil), e.rec.Secrets...)
	// A profile environment keeps the legacy kubeconfig field as a projection of
	// its primary credential; the secrets list is the authoritative binding set.
	if e.rec.TargetProfile == "" && e.rec.Kubeconfig.Name != "" {
		out = append(out, e.rec.Kubeconfig)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
