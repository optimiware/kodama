package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// catalog is the fake variable and secret catalog behind World's
// execution.Configuration: project variables with versions and secrets with
// revocable versions. It never holds a secret value: configuration pins
// references and versions only.
type catalog struct {
	mu        sync.Mutex
	variables map[varKey]variable
	secrets   map[secretKey][]bool // per version: revoked
}

type varKey struct {
	scope domain.Scope
	name  string
}

type variable struct {
	id      string
	version int64
	value   runtimev1.Value
}

type secretKey struct {
	scope domain.Scope
	name  string
}

func newCatalog() catalog {
	return catalog{variables: map[varKey]variable{}, secrets: map[secretKey][]bool{}}
}

// SetVariable creates or updates a project variable (its version advances).
func (w *World) SetVariable(scope domain.Scope, name string, value runtimev1.Value) {
	c := &w.catalog
	c.mu.Lock()
	defer c.mu.Unlock()
	k := varKey{scope, name}
	v := c.variables[k]
	c.variables[k] = variable{id: "var_" + name, version: v.version + 1, value: value}
}

// CreateSecret creates the project secret with a first version and returns its
// reference.
func (w *World) CreateSecret(scope domain.Scope, name string) string {
	c := &w.catalog
	c.mu.Lock()
	defer c.mu.Unlock()
	c.secrets[secretKey{scope, name}] = []bool{false}
	return "secret://project/" + name
}

// RotateSecret adds a new version and returns it.
func (w *World) RotateSecret(scope domain.Scope, name string) int64 {
	c := &w.catalog
	c.mu.Lock()
	defer c.mu.Unlock()
	k := secretKey{scope, name}
	c.secrets[k] = append(c.secrets[k], false)
	return int64(len(c.secrets[k]))
}

// RevokeSecretVersion revokes one version.
func (w *World) RevokeSecretVersion(scope domain.Scope, name string, version int64) {
	c := &w.catalog
	c.mu.Lock()
	defer c.mu.Unlock()
	if v := c.secrets[secretKey{scope, name}]; version >= 1 && int(version) <= len(v) {
		v[version-1] = true
	}
}

// usable reports whether the referenced version exists in the scope and is not
// revoked.
func (c *catalog) usable(scope domain.Scope, ref string, version int64) bool {
	name, ok := secretName(ref)
	if !ok {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.secrets[secretKey{scope, name}]
	return version >= 1 && int(version) <= len(v) && !v[version-1]
}

// current returns the highest version when it is not revoked.
func (c *catalog) current(scope domain.Scope, ref string) (int64, bool) {
	name, ok := secretName(ref)
	if !ok {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.secrets[secretKey{scope, name}]
	if len(v) == 0 || v[len(v)-1] {
		return 0, false
	}
	return int64(len(v)), true
}

func typeOK(typ string, v runtimev1.Value) bool {
	switch typ {
	case "string":
		return v.IsString()
	case "boolean":
		return v.IsBool()
	case "integer":
		return v.IsInteger()
	}
	return false
}

// Resolve implements execution.Configuration with the precedence default, then
// project variable, then run input. Unknown or mistyped inputs, a missing
// required value and an unusable secret are admission refusals, with
// content-free messages. Each secret is pinned to the current non-revoked
// version, once per reference, so every step sees the same version.
func (w *World) Resolve(_ context.Context, req execution.ConfigRequest) (execution.ConfigResult, error) {
	if err := req.Scope.Validate(); err != nil {
		return execution.ConfigResult{}, execution.Invalidf("%v", err)
	}
	c := &w.catalog
	decls := map[string]execution.InputDecl{}
	for _, d := range req.Inputs {
		decls[d.Name] = d
	}
	for name := range req.Explicit {
		if _, ok := decls[name]; !ok {
			return execution.ConfigResult{}, execution.Refusedf("a run input is not declared")
		}
	}
	names := make([]string, 0, len(decls))
	for n := range decls {
		names = append(names, n)
	}
	sort.Strings(names)
	var snap execution.Snapshot
	for _, name := range names {
		d := decls[name]
		in := execution.PinnedInput{Name: name, Type: d.Type}
		have := false
		if d.Default != nil {
			if !typeOK(d.Type, *d.Default) {
				return execution.ConfigResult{}, execution.Refusedf("an input default does not match its type")
			}
			in.Value, in.Source, have = *d.Default, "default", true
		}
		c.mu.Lock()
		v, found := c.variables[varKey{req.Scope, name}]
		c.mu.Unlock()
		if found {
			if !typeOK(d.Type, v.value) {
				return execution.ConfigResult{}, execution.Refusedf("a project variable does not match the input type")
			}
			in.Value, in.Source, in.VariableID, in.VariableVersion, have = v.value, "project_variable", v.id, v.version, true
		}
		if ex, ok := req.Explicit[name]; ok {
			if !typeOK(d.Type, ex) {
				return execution.ConfigResult{}, execution.Refusedf("a run input does not match its type")
			}
			in.Value, in.Source, in.VariableID, in.VariableVersion, have = ex, "run_input", "", 0, true
		}
		if !have {
			if d.Required {
				return execution.ConfigResult{}, execution.Refusedf("a required input has no value")
			}
			continue
		}
		snap.Inputs = append(snap.Inputs, in)
	}
	pinned := map[string]int64{}
	seen := map[string]bool{}
	for _, u := range req.Secrets {
		ver, ok := pinned[u.Ref]
		if !ok {
			if ver, ok = c.current(req.Scope, u.Ref); !ok {
				return execution.ConfigResult{}, execution.Refusedf("the run's secret references are not admissible")
			}
			pinned[u.Ref] = ver
		}
		k := u.Step + "\x00" + u.Name
		if seen[k] {
			return execution.ConfigResult{}, execution.Refusedf("step %s binds %q twice", u.Step, u.Name)
		}
		seen[k] = true
		b := runtimev1.SecretBinding{Ref: u.Ref, Version: ver, Name: u.Name, Delivery: u.Delivery}
		if err := (runtimev1.SecretResolveRequest{Ref: b.Ref, Version: b.Version}).Validate(); err != nil {
			return execution.ConfigResult{}, execution.Refusedf("a secret binding is invalid")
		}
		snap.Secrets = append(snap.Secrets, execution.StepSecret{Step: u.Step, Binding: b})
	}
	return execution.ConfigResult{Snapshot: snap}, nil
}

var _ = fmt.Sprint
