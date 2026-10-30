package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
)

// RunSharedConfig is the installation's opt-in to the run_shared model (O-30):
// the append-only list of execution policies, in installation order, the newest
// being the one new admissions pin. It exists only when the settings document
// carries a `runShared` section; an installation that does not configure it
// keeps new-format registration and admission closed exactly as before (the
// execution service refuses kodama/v1alpha2 and the pipeline validator rejects
// it at registration).
//
// Configuring it is not enabling it: the current policy's
// `admission.runShared` still decides whether new runs are admitted, so an
// installation can serve the v2 runtime routes and keep running runs through a
// cutover while admission is closed.
type RunSharedConfig struct {
	// Policies are the retained execution policies, oldest first. The last is the
	// current policy. Every policy a nonterminal run pins must stay in the list
	// (a run whose pin is missing stops its resources and fails with
	// policy_unavailable); the controller never edits a retained body.
	Policies []execution.ExecutionPolicy
	// ClusterCIDRs are address blocks public egress never reaches (pod, Service
	// and node ranges that are not already private), rendered into the per-run
	// network policies.
	ClusterCIDRs []string
	// BuilderRootless vouches for the builder_rootless capability after the
	// installation qualified a real rootless build under the pinned step role
	// (T-121). It is false until then, and a policy that requires the capability
	// is then refused at admission.
	BuilderRootless bool
}

// Validate checks the list as an installation statement.
func (c RunSharedConfig) Validate() error {
	if len(c.Policies) == 0 {
		return errors.New("run_shared: at least one execution policy is required")
	}
	ids, digests := map[string]bool{}, map[string]bool{}
	for i, p := range c.Policies {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("run_shared: policy %d: %w", i, err)
		}
		pin, err := p.Pin()
		if err != nil {
			return fmt.Errorf("run_shared: policy %d: %w", i, err)
		}
		// A retained body is immutable: reusing an id for another body would let a
		// pin resolve to something it did not name, so the list refuses it.
		if ids[pin.ID] || digests[pin.Digest] {
			return fmt.Errorf("run_shared: policy %q is listed twice: the list is append-only and every policy has its own id", p.ID)
		}
		ids[pin.ID], digests[pin.Digest] = true, true
	}
	for _, c := range c.ClusterCIDRs {
		p, err := netip.ParsePrefix(c)
		if err != nil || !p.Addr().Is4() || p.Bits() == 0 {
			return fmt.Errorf("run_shared: cluster CIDR %q is not an IPv4 block", c)
		}
	}
	return nil
}

// Current is the newest policy: the one new admissions pin. It is a copy: the
// caller cannot reach the retained body through it.
func (c RunSharedConfig) Current() execution.ExecutionPolicy {
	return clonePolicy(c.Policies[len(c.Policies)-1])
}

// clonePolicy is a deep copy of a policy body: every map, slice and pointer of it
// is its own, and a nil collection stays nil while an empty one stays empty (the
// digest of a policy distinguishes them, so a copy with a different shape would
// not be the body its pin names).
func clonePolicy(p execution.ExecutionPolicy) execution.ExecutionPolicy {
	return deepCopy(reflect.ValueOf(p)).Interface().(execution.ExecutionPolicy)
}

func deepCopy(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(it.Key(), deepCopy(it.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(deepCopy(v.Index(i)))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(deepCopy(v.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v) // plain fields; the collections below are replaced by their copies
		for i := 0; i < v.NumField(); i++ {
			if f := out.Field(i); f.CanSet() {
				f.Set(deepCopy(v.Field(i)))
			}
		}
		return out
	default:
		return v
	}
}

// PolicyList is the installation's retained policies as both of the consumers'
// ports: execution.Policies (the newest is Current) and kube.PolicyCatalog (a
// retained body by its pin; an unretained pin is kube.ErrPolicyUnavailable).
type PolicyList struct {
	list   []execution.ExecutionPolicy
	byPin  map[execution.PolicyPin]execution.ExecutionPolicy
	latest execution.ExecutionPolicy
}

var (
	_ execution.Policies = (*PolicyList)(nil)
	_ kube.PolicyCatalog = (*PolicyList)(nil)
)

// NewPolicyList validates the configuration and indexes the retained bodies.
func NewPolicyList(c RunSharedConfig) (*PolicyList, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	l := &PolicyList{byPin: map[execution.PolicyPin]execution.ExecutionPolicy{}}
	for _, p := range c.Policies {
		// The list owns its bodies: nothing the caller keeps reaches them.
		p = clonePolicy(p)
		pin, _ := p.Pin()
		l.byPin[pin] = p
		l.list = append(l.list, p)
	}
	l.latest = l.list[len(l.list)-1]
	return l, nil
}

// Current implements execution.Policies.
func (l *PolicyList) Current(context.Context) (execution.ExecutionPolicy, error) {
	return clonePolicy(l.latest), nil
}

// Policy implements kube.PolicyCatalog. The body is resolved by pin only: the
// current policy is never substituted for a pin that was not retained.
func (l *PolicyList) Policy(_ context.Context, pin execution.PolicyPin) (execution.ExecutionPolicy, error) {
	p, ok := l.byPin[pin]
	if !ok {
		return execution.ExecutionPolicy{}, kube.ErrPolicyUnavailable
	}
	return clonePolicy(p), nil
}

// Pins lists the retained pins, oldest first.
func (l *PolicyList) Pins() []execution.PolicyPin {
	out := make([]execution.PolicyPin, 0, len(l.list))
	for _, p := range l.list {
		pin, _ := p.Pin()
		out = append(out, pin)
	}
	return out
}

// ---- the settings document's section -----------------------------------------

// ChartRunShared is the `runShared` section of the settings document. Absent
// means the installation did not configure run_shared. Policies are the
// execution policies in the policy contract's own JSON shape
// (docs/contracts/run-resources.md); they are decoded strictly into
// execution.ExecutionPolicy, so a misspelled bound cannot silently leave a
// default in force.
type ChartRunShared struct {
	// Identity is the non-root user, group and volume group of run_shared
	// workloads (kube.PolicyConfig.RunShared).
	Identity struct {
		RunAsUser  int64 `yaml:"runAsUser"`
		RunAsGroup int64 `yaml:"runAsGroup"`
		FSGroup    int64 `yaml:"fsGroup"`
	} `yaml:"identity"`
	ClusterCIDRs    []string         `yaml:"clusterCIDRs"`
	BuilderRootless bool             `yaml:"builderRootless"`
	Policies        []map[string]any `yaml:"policies"`
}

// RunShared builds the installation's run_shared configuration from the
// document, or nil when the document has no section.
func (c ChartConfig) RunShared() (*RunSharedConfig, error) {
	rs := c.RunSharedSection
	if rs == nil {
		return nil, nil
	}
	out := &RunSharedConfig{ClusterCIDRs: rs.ClusterCIDRs, BuilderRootless: rs.BuilderRootless}
	for i, raw := range rs.Policies {
		body, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("run_shared: policy %d: %w", i, err)
		}
		var p execution.ExecutionPolicy
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("run_shared: policy %d: %w", i, err)
		}
		out.Policies = append(out.Policies, p)
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}
