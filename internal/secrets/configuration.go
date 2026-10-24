package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// InputType is the declared type of a pipeline input.
type InputType string

const (
	InputString   InputType = "string"
	InputBoolean  InputType = "boolean"
	InputInteger  InputType = "integer"
	InputArtifact InputType = "artifact"
)

// InputSource says which precedence level supplied an effective value.
type InputSource string

const (
	SourceDefault         InputSource = "default"
	SourceProjectVariable InputSource = "project_variable"
	SourceRunInput        InputSource = "run_input"
)

// InputDecl is one declared pipeline input.
type InputDecl struct {
	Name     string
	Type     InputType
	Default  *runtimev1.Value
	Required bool
}

// SecretUse is one declared secret reference of a step: the binding name under
// which it is delivered, the reference and the delivery form.
type SecretUse struct {
	Name     string
	Ref      string
	Delivery runtimev1.SecretDelivery
}

// ConfigurationRequest is everything resolution needs. It carries no
// plaintext: secret uses are references.
type ConfigurationRequest struct {
	Scope    domain.Scope
	Inputs   []InputDecl
	Explicit map[string]runtimev1.Value
	Secrets  []SecretUse
}

// VariableCatalog reads the project's current variables by name.
type VariableCatalog interface {
	// ActiveVariable returns the current non-deleted variable of that name, or
	// an error wrapping ErrNotFound.
	ActiveVariable(ctx context.Context, scope domain.Scope, name string) (Variable, error)
}

// SecretCatalog reads current secret metadata for pinning.
type SecretCatalog interface {
	// CurrentVersion returns the highest value version of the named, non-deleted
	// secret if it is not revoked; ErrNotFound if the secret does not exist in
	// the project and ErrNoActiveVersion if the current version is revoked.
	CurrentVersion(ctx context.Context, scope domain.Scope, name string) (int64, error)
}

// ArtifactOwnership validates artifact inputs against the owning project.
type ArtifactOwnership interface {
	// Owns reports whether the artifact belongs to the scope.
	Owns(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (bool, error)
}

// EffectiveInput is one resolved input with its provenance.
type EffectiveInput struct {
	Name   string
	Type   InputType
	Value  runtimev1.Value
	Source InputSource
	// VariableID and VariableVersion identify the pinned project variable for
	// SourceProjectVariable (and are empty/zero otherwise).
	VariableID      string
	VariableVersion int64
}

// EffectiveSnapshot is the immutable, deterministic result of resolution: the
// pinned input values with provenance and the pinned secret bindings. It never
// contains secret plaintext. Accessors return copies.
type EffectiveSnapshot struct {
	scope   domain.Scope
	inputs  []EffectiveInput // sorted by name
	secrets []runtimev1.SecretBinding
}

// Scope returns the owning tenant and project.
func (s EffectiveSnapshot) Scope() domain.Scope { return s.scope }

// Inputs returns the effective inputs in name order.
func (s EffectiveSnapshot) Inputs() []EffectiveInput {
	return append([]EffectiveInput(nil), s.inputs...)
}

// Input returns one effective input.
func (s EffectiveSnapshot) Input(name string) (EffectiveInput, bool) {
	i := sort.Search(len(s.inputs), func(i int) bool { return s.inputs[i].Name >= name })
	if i < len(s.inputs) && s.inputs[i].Name == name {
		return s.inputs[i], true
	}
	return EffectiveInput{}, false
}

// Values returns the input values in the shape of the runtime context.
func (s EffectiveSnapshot) Values() map[string]runtimev1.Value {
	out := make(map[string]runtimev1.Value, len(s.inputs))
	for _, in := range s.inputs {
		out[in.Name] = in.Value
	}
	return out
}

// SecretBindings returns the pinned secret references in declaration order.
func (s EffectiveSnapshot) SecretBindings() []runtimev1.SecretBinding {
	return append([]runtimev1.SecretBinding(nil), s.secrets...)
}

// Digest is a stable fingerprint of values, provenance and pinned versions.
func (s EffectiveSnapshot) Digest() (runtimev1.Digest, error) {
	type item struct {
		Name   string          `json:"name"`
		Type   InputType       `json:"type"`
		Value  runtimev1.Value `json:"value"`
		Source InputSource     `json:"source"`
		VarID  string          `json:"variableId,omitempty"`
		VarVer int64           `json:"variableVersion,omitempty"`
	}
	doc := struct {
		Tenant  string                    `json:"tenant"`
		Project string                    `json:"project"`
		Inputs  []item                    `json:"inputs"`
		Secrets []runtimev1.SecretBinding `json:"secrets"`
	}{Tenant: string(s.scope.TenantID), Project: string(s.scope.ProjectID), Secrets: s.secrets}
	for _, in := range s.inputs {
		doc.Inputs = append(doc.Inputs, item{in.Name, in.Type, in.Value, in.Source, in.VariableID, in.VariableVersion})
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return runtimev1.Digest("sha256:" + hex.EncodeToString(sum[:])), nil
}

var safeName = regexp.MustCompile(runtimev1.Patterns["SafeName"])

// ConfigurationResolver resolves declared inputs deterministically.
type ConfigurationResolver struct {
	variables VariableCatalog
	secrets   SecretCatalog
	artifacts ArtifactOwnership
}

// NewConfigurationResolver wires the resolver. artifacts may be nil only if no
// request declares an artifact input; such a request then fails closed.
func NewConfigurationResolver(v VariableCatalog, s SecretCatalog, a ArtifactOwnership) (*ConfigurationResolver, error) {
	if v == nil || s == nil {
		return nil, invalidf("variable and secret catalogs are required")
	}
	return &ConfigurationResolver{variables: v, secrets: s, artifacts: a}, nil
}

// Resolve applies precedence default -> project variable of the declared name
// -> explicit run input to each declared input. Unknown explicit names, type
// mismatches, missing required values and foreign artifacts are admission
// failures. A project variable that is not declared is never an input. Secret
// uses are pinned once, to the current non-revoked version.
func (r *ConfigurationResolver) Resolve(ctx context.Context, req ConfigurationRequest) (EffectiveSnapshot, error) {
	if err := req.Scope.Validate(); err != nil {
		return EffectiveSnapshot{}, err
	}
	decls := make(map[string]InputDecl, len(req.Inputs))
	for _, d := range req.Inputs {
		if err := ValidateVariableName(d.Name); err != nil {
			return EffectiveSnapshot{}, err
		}
		if _, dup := decls[d.Name]; dup {
			return EffectiveSnapshot{}, invalidf("input %q is declared twice", d.Name)
		}
		switch d.Type {
		case InputString, InputBoolean, InputInteger, InputArtifact:
		default:
			return EffectiveSnapshot{}, invalidf("input %q has an unsupported type", d.Name)
		}
		decls[d.Name] = d
	}
	for name := range req.Explicit {
		if _, ok := decls[name]; !ok {
			if ValidateVariableName(name) != nil {
				return EffectiveSnapshot{}, invalidf("a run input name is not valid")
			}
			return EffectiveSnapshot{}, invalidf("run input %q is not declared", name)
		}
	}

	names := make([]string, 0, len(decls))
	for n := range decls {
		names = append(names, n)
	}
	sort.Strings(names)

	snap := EffectiveSnapshot{scope: req.Scope}
	for _, name := range names {
		d := decls[name]
		eff := EffectiveInput{Name: name, Type: d.Type}
		have := false
		if d.Default != nil {
			if err := checkValueType(d, *d.Default); err != nil {
				return EffectiveSnapshot{}, err
			}
			eff.Value, eff.Source, have = *d.Default, SourceDefault, true
		}
		switch v, err := r.variables.ActiveVariable(ctx, req.Scope, name); {
		case err == nil:
			if d.Type == InputArtifact || InputType(v.Type()) != d.Type {
				return EffectiveSnapshot{}, invalidf("project variable %q has type %s, input is declared %s", name, v.Type(), d.Type)
			}
			eff.Value, eff.Source, have = v.Value(), SourceProjectVariable, true
			eff.VariableID, eff.VariableVersion = v.ID(), v.ValueVersion()
		case errors.Is(err, ErrNotFound):
		default:
			return EffectiveSnapshot{}, fmt.Errorf("secrets: read variable: %w", err)
		}
		if v, ok := req.Explicit[name]; ok {
			if err := checkValueType(d, v); err != nil {
				return EffectiveSnapshot{}, err
			}
			eff.Value, eff.Source, have = v, SourceRunInput, true
			eff.VariableID, eff.VariableVersion = "", 0
		}
		if !have {
			if d.Required {
				return EffectiveSnapshot{}, invalidf("required input %q has no value", name)
			}
			continue
		}
		if d.Type == InputArtifact {
			id := domain.ArtifactID(eff.Value.String())
			if err := id.Validate(); err != nil {
				return EffectiveSnapshot{}, invalidf("input %q is not an artifact ID", name)
			}
			if r.artifacts == nil {
				return EffectiveSnapshot{}, ErrAuthorityMissing
			}
			owns, err := r.artifacts.Owns(ctx, req.Scope, id)
			if err != nil {
				return EffectiveSnapshot{}, fmt.Errorf("secrets: check artifact ownership: %w", err)
			}
			if !owns {
				// Foreign and missing artifacts are indistinguishable.
				return EffectiveSnapshot{}, fmt.Errorf("%w: artifact for input %q", ErrNotFound, name)
			}
		}
		snap.inputs = append(snap.inputs, eff)
	}

	seen := map[string]bool{}
	for _, use := range req.Secrets {
		b, err := r.pin(ctx, req.Scope, use)
		if err != nil {
			return EffectiveSnapshot{}, err
		}
		if seen[b.Name] {
			return EffectiveSnapshot{}, invalidf("secret binding %q is declared twice", b.Name)
		}
		seen[b.Name] = true
		snap.secrets = append(snap.secrets, b)
	}
	return snap, nil
}

func (r *ConfigurationResolver) pin(ctx context.Context, scope domain.Scope, use SecretUse) (runtimev1.SecretBinding, error) {
	name, err := ParseSecretRef(use.Ref)
	if err != nil {
		return runtimev1.SecretBinding{}, err
	}
	if use.Delivery != runtimev1.DeliveryFile && use.Delivery != runtimev1.DeliveryEnv {
		return runtimev1.SecretBinding{}, invalidf("unsupported secret delivery")
	}
	b := runtimev1.SecretBinding{Ref: use.Ref, Name: use.Name, Delivery: use.Delivery}
	ver, err := r.secrets.CurrentVersion(ctx, scope, name)
	if err != nil {
		return runtimev1.SecretBinding{}, fmt.Errorf("secrets: pin %s: %w", use.Ref, err)
	}
	b.Version = ver
	if !safeName.MatchString(use.Name) {
		return runtimev1.SecretBinding{}, invalidf("secret binding name must match %s", runtimev1.Patterns["SafeName"])
	}
	if verr := (runtimev1.SecretResolveRequest{Ref: b.Ref, Version: b.Version}).Validate(); verr != nil {
		return runtimev1.SecretBinding{}, invalidf("secret binding: %v", verr)
	}
	return b, nil
}

func checkValueType(d InputDecl, v runtimev1.Value) error {
	if err := v.Validate(); err != nil {
		return invalidf("input %q: %v", d.Name, err)
	}
	ok := false
	switch d.Type {
	case InputString, InputArtifact:
		ok = v.IsString()
	case InputBoolean:
		ok = v.IsBool()
	case InputInteger:
		ok = v.IsInteger()
	}
	if !ok {
		return invalidf("input %q does not match its declared type %s", d.Name, d.Type)
	}
	return nil
}
