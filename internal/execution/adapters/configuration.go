package adapters

import (
	"context"
	"errors"
	"fmt"
	"sort"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/secrets"
)

// Configuration adapts the secrets module's resolver: precedence (default,
// project variable, run input), type and unknown-input rejection, and exact
// secret version pins, without plaintext.
type Configuration struct {
	r *secrets.ConfigurationResolver
}

var _ execution.Configuration = Configuration{}

// NewConfiguration requires the resolver.
func NewConfiguration(r *secrets.ConfigurationResolver) (Configuration, error) {
	if r == nil {
		return Configuration{}, errors.New("execution/adapters: a configuration resolver is required")
	}
	return Configuration{r: r}, nil
}

// Resolve implements execution.Configuration. Inputs are resolved once for the
// whole run. Secret versions are pinned once per distinct reference, so every
// step that uses a reference sees the same version, and the per-step bindings
// (their own names and delivery forms, which repeat across steps) are derived
// from that single pin.
func (c Configuration) Resolve(ctx context.Context, req execution.ConfigRequest) (execution.ConfigResult, error) {
	cr := secrets.ConfigurationRequest{Scope: req.Scope, Explicit: req.Explicit}
	for _, d := range req.Inputs {
		cr.Inputs = append(cr.Inputs, secrets.InputDecl{Name: d.Name, Type: secrets.InputType(d.Type), Default: d.Default, Required: d.Required})
	}
	refs := map[string]string{}
	var order []string
	for _, u := range req.Secrets {
		if _, ok := refs[u.Ref]; !ok {
			refs[u.Ref] = fmt.Sprintf("pin-%d", len(order))
			order = append(order, u.Ref)
		}
	}
	sort.Strings(order)
	for i, ref := range order {
		refs[ref] = fmt.Sprintf("pin-%d", i)
		cr.Secrets = append(cr.Secrets, secrets.SecretUse{Name: refs[ref], Ref: ref, Delivery: runtimev1.DeliveryFile})
	}
	snap, err := c.r.Resolve(ctx, cr)
	if err != nil {
		return execution.ConfigResult{}, mapConfigError(err)
	}
	versions := map[string]int64{}
	for _, b := range snap.SecretBindings() {
		versions[b.Ref] = b.Version
	}
	out := execution.Snapshot{}
	for _, in := range snap.Inputs() {
		out.Inputs = append(out.Inputs, execution.PinnedInput{
			Name: in.Name, Type: string(in.Type), Value: in.Value, Source: string(in.Source), VariableID: in.VariableID, VariableVersion: in.VariableVersion,
		})
	}
	seen := map[string]bool{}
	for _, u := range req.Secrets {
		v, ok := versions[u.Ref]
		if !ok {
			return execution.ConfigResult{}, execution.Unavailablef(nil, "a secret was not pinned")
		}
		key := u.Step + "\x00" + u.Name
		if seen[key] {
			return execution.ConfigResult{}, execution.Refusedf("step %s binds %q twice", u.Step, u.Name)
		}
		seen[key] = true
		b := runtimev1.SecretBinding{Ref: u.Ref, Version: v, Name: u.Name, Delivery: u.Delivery}
		if err := (runtimev1.SecretResolveRequest{Ref: b.Ref, Version: b.Version}).Validate(); err != nil {
			return execution.ConfigResult{}, execution.Refusedf("a secret binding is invalid")
		}
		out.Secrets = append(out.Secrets, execution.StepSecret{Step: u.Step, Binding: b})
	}
	return execution.ConfigResult{Snapshot: out}, nil
}

func mapConfigError(err error) error {
	switch {
	case errors.Is(err, secrets.ErrInvalid), errors.Is(err, secrets.ErrNotFound), errors.Is(err, secrets.ErrNoActiveVersion),
		errors.Is(err, secrets.ErrDeleted), errors.Is(err, secrets.ErrRevoked):
		// Content-free: the message never echoes a value or a foreign reference.
		return execution.Refusedf("the run's inputs or secret references are not admissible")
	}
	return execution.Unavailablef(err, "configuration resolution is unavailable")
}
