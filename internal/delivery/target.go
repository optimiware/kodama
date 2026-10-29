package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// A deployment target is selected by a versioned, installation-owned target
// profile. The profile names the provider, the artifact kinds it accepts, the
// credentials it needs, the egress profiles it may use and the pinned driver
// that performs provider-specific work. No provider name, command or credential
// kind is hard-coded in an environment or a deployment.
//
// The first profile is the implicit Kubernetes namespace profile used by
// existing environments. Further profiles (for example a Cloudflare Worker
// driven by Wrangler) are configuration, not new server executors.

// KubernetesProfileName is the profile existing environments implicitly use.
const KubernetesProfileName = "kubernetes-namespace"

// ArtifactKind says how a target consumes a built artifact.
type ArtifactKind string

const (
	// ArtifactOCIImage is a digest-pinned OCI image; the Kubernetes profile uses it.
	ArtifactOCIImage ArtifactKind = "ociImage"
	// ArtifactBundle is an immutable file bundle (archive plus manifest) for
	// static, Pages or Worker targets.
	ArtifactBundle ArtifactKind = "bundle"
)

// Valid reports whether k is a known artifact kind.
func (k ArtifactKind) Valid() bool { return k == ArtifactOCIImage || k == ArtifactBundle }

// TargetConfigField is one scalar a target profile accepts in an environment's
// target configuration. Values stay strings on the wire; the declared type is
// validated and integer/boolean values are canonicalized.
type TargetConfigField struct {
	Type      string   `json:"type" yaml:"type"`
	Required  bool     `json:"required,omitempty" yaml:"required,omitempty"`
	Pattern   string   `json:"pattern,omitempty" yaml:"pattern,omitempty"`
	Enum      []string `json:"enum,omitempty" yaml:"enum,omitempty"`
	MaxLength int      `json:"maxLength,omitempty" yaml:"maxLength,omitempty"`

	pattern *regexp.Regexp
}

// CredentialRequirement is a credential binding a target profile needs. It
// names the binding, the secret kind the secrets owner must report and the
// delivery form; it never carries a value or a version.
type CredentialRequirement struct {
	Name     string `json:"name" yaml:"name"`
	Kind     string `json:"kind" yaml:"kind"`
	Delivery string `json:"delivery" yaml:"delivery"`
	Required bool   `json:"required,omitempty" yaml:"required,omitempty"`
}

// TargetDriver is the installation-qualified deployment driver of a profile.
// Mode "script" reuses the existing project deploy scripts. Mode "image" names
// a digest-pinned driver image and the fixed entrypoints it implements; a
// pipeline step may only declare that image for the environment.
type TargetDriver struct {
	Mode     string              `json:"mode" yaml:"mode"`
	Image    string              `json:"image,omitempty" yaml:"image,omitempty"`
	Protocol string              `json:"protocol,omitempty" yaml:"protocol,omitempty"`
	Commands map[string][]string `json:"commands,omitempty" yaml:"commands,omitempty"`
}

// TargetProfile is one installation-approved target type.
type TargetProfile struct {
	Name          string                       `json:"name" yaml:"name"`
	Version       string                       `json:"version" yaml:"version"`
	Provider      string                       `json:"provider" yaml:"provider"`
	ArtifactKinds []ArtifactKind               `json:"artifactKinds" yaml:"artifactKinds"`
	Credentials   []CredentialRequirement      `json:"credentials,omitempty" yaml:"credentials,omitempty"`
	Egress        []string                     `json:"egress,omitempty" yaml:"egress,omitempty"`
	Driver        TargetDriver                 `json:"driver" yaml:"driver"`
	Config        map[string]TargetConfigField `json:"config,omitempty" yaml:"config,omitempty"`
	Description   string                       `json:"description,omitempty" yaml:"description,omitempty"`
}

// TargetProfilesDocument is the YAML document shape.
type TargetProfilesDocument struct {
	APIVersion string          `json:"apiVersion" yaml:"apiVersion"`
	Kind       string          `json:"kind" yaml:"kind"`
	Profiles   []TargetProfile `json:"profiles" yaml:"profiles"`
}

var (
	profileVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)
	providerPattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	egressNamePattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	driverImagePattern    = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
	commandNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// BuiltinKubernetesProfile returns the implicit legacy profile. It deliberately
// has no driver image: existing project Kubernetes deploy scripts keep their
// delivery-tools qualification and behavior.
func BuiltinKubernetesProfile() TargetProfile {
	return TargetProfile{
		Name:          KubernetesProfileName,
		Version:       "1",
		Provider:      "kubernetes",
		ArtifactKinds: []ArtifactKind{ArtifactOCIImage},
		Credentials: []CredentialRequirement{
			{Name: DefaultKubeconfig, Kind: KindKubeconfig, Delivery: "file", Required: true},
		},
		Driver:      TargetDriver{Mode: "script"},
		Description: "A namespace-scoped Kubernetes deployment applied by an ordinary project script.",
	}
}

// Validate checks the profile and normalizes its derived fields. It does not
// read a file or a provider.
func (p *TargetProfile) Validate() error {
	if !environmentName.MatchString(p.Name) {
		return invalidf("a target profile name must match %s", environmentName)
	}
	if !profileVersionPattern.MatchString(p.Version) {
		return invalidf("a target profile version must match %s", profileVersionPattern)
	}
	if !providerPattern.MatchString(p.Provider) {
		return invalidf("a target profile provider must match %s", providerPattern)
	}
	if len(p.ArtifactKinds) == 0 || len(p.ArtifactKinds) > 4 {
		return invalidf("a target profile accepts one to four artifact kinds")
	}
	seenKinds := map[ArtifactKind]bool{}
	for _, k := range p.ArtifactKinds {
		if !k.Valid() {
			return invalidf("unknown artifact kind %q", k)
		}
		if seenKinds[k] {
			return invalidf("duplicate artifact kind %q", k)
		}
		seenKinds[k] = true
	}
	if len(p.Credentials) > MaxScriptSecrets+1 {
		return invalidf("a target profile lists at most %d credentials", MaxScriptSecrets+1)
	}
	names := map[string]bool{}
	for i := range p.Credentials {
		c := &p.Credentials[i]
		if !bindingName.MatchString(c.Name) {
			return invalidf("a credential requirement name must match %s", bindingName)
		}
		if names[c.Name] {
			return invalidf("credential requirement names must be unique")
		}
		names[c.Name] = true
		if !providerPattern.MatchString(c.Kind) {
			return invalidf("a credential requirement kind must match %s", providerPattern)
		}
		if c.Delivery != "file" && c.Delivery != "env" {
			return invalidf("a credential requirement is delivered as file or env")
		}
	}
	if len(p.Egress) > 8 {
		return invalidf("a target profile lists at most 8 egress profiles")
	}
	egressSeen := map[string]bool{}
	for _, e := range p.Egress {
		if !egressNamePattern.MatchString(e) {
			return invalidf("an egress profile name must match %s", egressNamePattern)
		}
		if egressSeen[e] {
			return invalidf("egress profile names must be unique")
		}
		egressSeen[e] = true
	}
	switch p.Driver.Mode {
	case "script":
		if p.Driver.Image != "" || len(p.Driver.Commands) != 0 {
			return invalidf("a script driver declares no image or commands")
		}
	case "image":
		if !driverImagePattern.MatchString(p.Driver.Image) {
			return invalidf("a driver image must be digest-pinned")
		}
		if p.Driver.Protocol == "" {
			p.Driver.Protocol = "kodama-deploy/v1"
		}
		if p.Driver.Protocol != "kodama-deploy/v1" {
			return invalidf("unknown driver protocol %q", p.Driver.Protocol)
		}
		if len(p.Driver.Commands) == 0 {
			return invalidf("an image driver declares its entrypoints")
		}
		for name, command := range p.Driver.Commands {
			if !commandNamePattern.MatchString(name) {
				return invalidf("a driver command name must match %s", commandNamePattern)
			}
			if len(command) == 0 || len(command) > 64 {
				return invalidf("driver command %q has 1 to 64 elements", name)
			}
			for _, arg := range command {
				if len(arg) > 8192 || strings.ContainsRune(arg, 0) {
					return invalidf("driver command %q has an invalid argument", name)
				}
			}
		}
		if len(p.Driver.Commands["apply"]) == 0 {
			return invalidf("an image driver declares an apply command")
		}
		if len(p.Driver.Commands["register"]) == 0 {
			p.Driver.Commands["register"] = append([]string(nil), p.Driver.Commands["apply"]...)
		}
	default:
		return invalidf("a target driver mode is script or image")
	}
	if len(p.Config) > MaxConfigEntries {
		return invalidf("a target profile holds at most %d configuration fields", MaxConfigEntries)
	}
	for name, field := range p.Config {
		if !configKey.MatchString(name) {
			return invalidf("a target configuration key must match %s", configKey)
		}
		switch field.Type {
		case "string", "boolean", "integer":
		default:
			return invalidf("target configuration field %q has an unknown type", name)
		}
		if field.MaxLength < 0 || field.MaxLength > MaxConfigValue {
			return invalidf("target configuration field %q has an invalid maximum length", name)
		}
		if field.Pattern != "" {
			re, err := regexp.Compile(field.Pattern)
			if err != nil {
				return invalidf("target configuration field %q has an invalid pattern", name)
			}
			field.pattern = re
		}
		for _, v := range field.Enum {
			if len(v) > MaxConfigValue {
				return invalidf("target configuration field %q has an invalid enum value", name)
			}
		}
		p.Config[name] = field
	}
	return nil
}

// Digest is the immutable identity of the normalized profile.
func (p TargetProfile) Digest() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ResolveConfig validates an environment's target configuration against the
// profile and returns its canonical form. Unknown keys, absent required values,
// type mismatches and pattern/enum failures are refused.
func (p TargetProfile) ResolveConfig(in map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for name, value := range in {
		field, ok := p.Config[name]
		if !ok {
			return nil, invalidf("unknown target configuration %q", name)
		}
		if len(value) > MaxConfigValue || strings.ContainsRune(value, 0) {
			return nil, invalidf("target configuration %q is too long", name)
		}
		if field.MaxLength > 0 && len(value) > field.MaxLength {
			return nil, invalidf("target configuration %q exceeds its maximum length", name)
		}
		switch field.Type {
		case "string":
		case "boolean":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return nil, invalidf("target configuration %q must be a boolean", name)
			}
			value = strconv.FormatBool(v)
		case "integer":
			v, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, invalidf("target configuration %q must be an integer", name)
			}
			value = strconv.FormatInt(v, 10)
		}
		if field.pattern != nil && !field.pattern.MatchString(value) {
			return nil, invalidf("target configuration %q does not match its pattern", name)
		}
		if len(field.Enum) > 0 && !containsString(field.Enum, value) {
			return nil, invalidf("target configuration %q is not an allowed value", name)
		}
		out[name] = value
	}
	for name, field := range p.Config {
		if field.Required && out[name] == "" {
			return nil, invalidf("target configuration %q is required", name)
		}
	}
	return out, nil
}

// MissingCredentials lists required credential bindings the environment does
// not provide.
func (p TargetProfile) MissingCredentials(creds []Credential) []string {
	have := map[string]Credential{}
	for _, c := range creds {
		have[c.Name] = c
	}
	var missing []string
	for _, req := range p.Credentials {
		if !req.Required {
			continue
		}
		c, ok := have[req.Name]
		if !ok || c.Name == "" || c.Ref == "" || string(c.Delivery) != req.Delivery {
			missing = append(missing, req.Name)
		}
	}
	sort.Strings(missing)
	return missing
}

// CredentialKind reports the required secret kind of a binding, or "" when the
// profile does not name one.
func (p TargetProfile) CredentialKind(name string) string {
	for _, c := range p.Credentials {
		if c.Name == name {
			return c.Kind
		}
	}
	return ""
}

func containsString(in []string, value string) bool {
	for _, v := range in {
		if v == value {
			return true
		}
	}
	return false
}

// TargetRegistry is the installation's immutable set of profiles.
type TargetRegistry struct {
	byName map[string]TargetProfile
}

// NewTargetRegistry validates profiles and adds the builtin Kubernetes profile
// when the document does not define it. A document may override the builtin
// name deliberately.
func NewTargetRegistry(profiles []TargetProfile) (*TargetRegistry, error) {
	r := &TargetRegistry{byName: map[string]TargetProfile{}}
	for i := range profiles {
		p := profiles[i]
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("delivery: target profile %q: %w", p.Name, err)
		}
		if _, ok := r.byName[p.Name]; ok {
			return nil, fmt.Errorf("delivery: duplicate target profile %q", p.Name)
		}
		r.byName[p.Name] = p
	}
	if _, ok := r.byName[KubernetesProfileName]; !ok {
		p := BuiltinKubernetesProfile()
		if err := p.Validate(); err != nil {
			return nil, err
		}
		r.byName[p.Name] = p
	}
	return r, nil
}

// DefaultTargetRegistry is the builtin set: only the Kubernetes profile.
func DefaultTargetRegistry() *TargetRegistry {
	r, err := NewTargetRegistry(nil)
	if err != nil {
		panic("delivery: the builtin target profile is invalid: " + err.Error())
	}
	return r
}

// Get returns one profile.
func (r *TargetRegistry) Get(name string) (TargetProfile, bool) {
	if r == nil {
		r = DefaultTargetRegistry()
	}
	if name == "" {
		name = KubernetesProfileName
	}
	p, ok := r.byName[name]
	return p, ok
}

// Profiles returns a copy of every profile ordered by name.
func (r *TargetRegistry) Profiles() []TargetProfile {
	if r == nil {
		r = DefaultTargetRegistry()
	}
	out := make([]TargetProfile, 0, len(r.byName))
	for _, p := range r.byName {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve validates a profile name and target config together.
func (r *TargetRegistry) Resolve(name string, config map[string]string) (TargetProfile, map[string]string, error) {
	p, ok := r.Get(name)
	if !ok {
		return TargetProfile{}, nil, invalidf("unknown target profile %q", name)
	}
	cfg, err := p.ResolveConfig(config)
	if err != nil {
		return TargetProfile{}, nil, err
	}
	return p, cfg, nil
}

// LoadTargetProfilesYAML reads the single YAML document. Unknown fields are
// refused; the file is installation configuration, not tenant input.
func LoadTargetProfilesYAML(rd io.Reader) ([]TargetProfile, error) {
	dec := yaml.NewDecoder(rd)
	dec.KnownFields(true)
	var doc TargetProfilesDocument
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("delivery: target profiles: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("delivery: target profiles: trailing document")
		}
		return nil, fmt.Errorf("delivery: target profiles: %w", err)
	}
	if doc.APIVersion != "kodama/v1alpha1" || doc.Kind != "TargetProfiles" {
		return nil, errors.New("delivery: target profiles: apiVersion kodama/v1alpha1 and kind TargetProfiles are required")
	}
	if len(doc.Profiles) == 0 {
		return nil, errors.New("delivery: target profiles: at least one profile is required")
	}
	for i := range doc.Profiles {
		if err := doc.Profiles[i].Validate(); err != nil {
			return nil, fmt.Errorf("delivery: target profile %q: %w", doc.Profiles[i].Name, err)
		}
	}
	return doc.Profiles, nil
}

// LoadTargetProfilesFile reads a YAML target profile file.
func LoadTargetProfilesFile(path string) (*TargetRegistry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("delivery: read target profiles: %w", err)
	}
	defer f.Close()
	profiles, err := LoadTargetProfilesYAML(f)
	if err != nil {
		return nil, err
	}
	return NewTargetRegistry(profiles)
}
