package kube

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// This file is the single home of the executor's deployment-supplied settings.
// T-020 pins context and Job identity but supplies no namespace, service
// account, resource quantities or injector image; T-027 composes the values
// below from installation configuration. Nothing here reads the environment.

var (
	kubeNameRe  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	imageRefRe  = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
	pullPolicyS = []string{"IfNotPresent", "Always", "Never"}
)

// TenantTarget is where one tenant's attempts run: the tenant's namespace and its
// least-privilege service account.
type TenantTarget struct {
	Namespace      string
	ServiceAccount string
}

// ResourceBounds are explicit compute and storage bounds of one container. Every
// field is required; requests may not exceed limits.
type ResourceBounds struct {
	CPURequest       string
	CPULimit         string
	MemoryRequest    string
	MemoryLimit      string
	EphemeralRequest string
	EphemeralLimit   string
}

// Validate checks every quantity is positive and requests fit under limits.
func (b ResourceBounds) Validate(name string) error {
	pairs := []struct{ n, req, lim string }{
		{"cpu", b.CPURequest, b.CPULimit}, {"memory", b.MemoryRequest, b.MemoryLimit}, {"ephemeral-storage", b.EphemeralRequest, b.EphemeralLimit},
	}
	for _, p := range pairs {
		if err := mustPositive(name+" "+p.n+" request", p.req); err != nil {
			return err
		}
		if err := mustPositive(name+" "+p.n+" limit", p.lim); err != nil {
			return err
		}
		rq, _ := ParseQuantity(p.req)
		lq, _ := ParseQuantity(p.lim)
		if rq > lq {
			return fmt.Errorf("%s %s request exceeds its limit", name, p.n)
		}
	}
	return nil
}

// VolumeSizes bound the private Pod volumes. Secrets and Identity are
// memory-backed, so their sizes also count against the container memory limit.
type VolumeSizes struct {
	Workspace string
	Outputs   string
	Scratch   string
	Results   string
	Tools     string
	Secrets   string
	Identity  string
}

func (v VolumeSizes) validate() error {
	for name, s := range map[string]string{"workspace": v.Workspace, "outputs": v.Outputs, "scratch": v.Scratch, "results": v.Results, "tools": v.Tools, "secrets": v.Secrets, "identity": v.Identity} {
		if err := mustPositive("volume "+name, s); err != nil {
			return err
		}
	}
	return nil
}

// fit checks the volumes can live inside the workload's bounds: disk-backed
// volumes within its ephemeral-storage limit and memory-backed volumes within
// its memory limit, so a full volume can never exceed what the node reserved.
func (v VolumeSizes) fit(w ResourceBounds) error {
	sum := func(sizes ...string) Quantity {
		var t Quantity
		for _, s := range sizes {
			q, _ := ParseQuantity(s)
			t += q
		}
		return t
	}
	eph, _ := ParseQuantity(w.EphemeralLimit)
	mem, _ := ParseQuantity(w.MemoryLimit)
	if sum(v.Workspace, v.Outputs, v.Scratch, v.Results, v.Tools) > eph {
		return errors.New("disk-backed volumes exceed the workload ephemeral-storage limit")
	}
	if sum(v.Secrets, v.Identity) >= mem {
		return errors.New("memory-backed volumes must leave room in the workload memory limit")
	}
	return nil
}

// EgressRule is one explicitly allowed workload egress destination: an exact
// CIDR and the TCP ports on it. The namespace policy check requires every
// egress rule that selects workload Pods to be covered by one of these rules.
type EgressRule struct {
	CIDR string
	// Protocol is TCP (default) or UDP.
	Protocol string
	Ports    []int32
}

// PolicyConfig is the input of NewExecutionPolicy.
type PolicyConfig struct {
	// Tenants maps each tenant allowed to execute to its target. A tenant
	// absent here is denied.
	Tenants map[domain.TenantID]TenantTarget
	// InjectorImage is the digest-pinned image that provides the entrypoint
	// binary; InjectorCommand copies it into the shared tools volume at
	// /kodama/bin/kodama (for example cp /usr/local/bin/kodama /kodama/bin/).
	InjectorImage   string
	InjectorCommand []string
	// ImagePullPolicy is the pull policy of every container (default
	// IfNotPresent: all images are digest-pinned).
	ImagePullPolicy string
	// RuntimeURL is the HTTPS origin of the runtime API as seen from a Pod, and
	// RuntimeCA the PEM trust roots for it. Plain HTTP is never accepted.
	RuntimeURL string
	RuntimeCA  []byte
	// RuntimeEgress are the only destinations a workload may reach.
	RuntimeEgress []EgressRule
	Workload      ResourceBounds
	Injector      ResourceBounds
	Volumes       VolumeSizes
	// RunAsUser and RunAsGroup are the non-root identity of every container
	// and the Pod's fsGroup.
	RunAsUser  int64
	RunAsGroup int64
	// TerminationGrace is the Pod's termination grace period.
	TerminationGrace time.Duration
	// ReaderActions and WriterActions are the explicit runtime action
	// ceilings of attempts without and with workspace write access. Empty
	// selects the defaults of DefaultActions.
	ReaderActions []runtimeauth.Action
	WriterActions []runtimeauth.Action
}

// DefaultActions returns runtime admission's default ceilings
// (runtimeauth.DefaultActions).
func DefaultActions(writer bool) []runtimeauth.Action { return runtimeauth.DefaultActions(writer) }

// ExecutionPolicy is the validated, immutable execution policy. The zero value
// denies everything.
type ExecutionPolicy struct {
	cfg    PolicyConfig
	caHash string
	digest string
	valid  bool
}

// NewExecutionPolicy validates and copies the configuration. It fails closed:
// there are no defaults for namespaces, images, resources or the callback.
func NewExecutionPolicy(cfg PolicyConfig) (ExecutionPolicy, error) {
	c := cfg
	c.Tenants = make(map[domain.TenantID]TenantTarget, len(cfg.Tenants))
	if len(cfg.Tenants) == 0 {
		return ExecutionPolicy{}, wrap(ErrPolicy, "at least one tenant target is required")
	}
	for t, tgt := range cfg.Tenants {
		if err := t.Validate(); err != nil {
			return ExecutionPolicy{}, wrap(ErrPolicy, "tenant id is invalid")
		}
		if !kubeNameRe.MatchString(tgt.Namespace) || !kubeNameRe.MatchString(tgt.ServiceAccount) || len(tgt.Namespace) > 63 {
			return ExecutionPolicy{}, wrap(ErrPolicy, "tenant %s: namespace and service account must be Kubernetes names", t)
		}
		c.Tenants[t] = tgt
	}
	// A namespace serves one tenant: two tenants sharing one would share its
	// quota, network policy and service account.
	seen := map[string]domain.TenantID{}
	for t, tgt := range c.Tenants {
		if other, dup := seen[tgt.Namespace]; dup {
			return ExecutionPolicy{}, wrap(ErrPolicy, "tenants %s and %s share a namespace", other, t)
		}
		seen[tgt.Namespace] = t
	}
	if !imageRefRe.MatchString(c.InjectorImage) {
		return ExecutionPolicy{}, wrap(ErrPolicy, "the injector image must be pinned by digest")
	}
	c.InjectorCommand = slices.Clone(cfg.InjectorCommand)
	if len(c.InjectorCommand) == 0 || slices.Contains(c.InjectorCommand, "") {
		return ExecutionPolicy{}, wrap(ErrPolicy, "the injector command is required")
	}
	if c.ImagePullPolicy == "" {
		c.ImagePullPolicy = "IfNotPresent"
	}
	if !slices.Contains(pullPolicyS, c.ImagePullPolicy) {
		return ExecutionPolicy{}, wrap(ErrPolicy, "unknown image pull policy")
	}
	u, err := url.Parse(c.RuntimeURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return ExecutionPolicy{}, wrap(ErrPolicy, "the runtime URL must be an https origin")
	}
	c.RuntimeURL = "https://" + u.Host
	c.RuntimeCA = slices.Clone(cfg.RuntimeCA)
	if err := validateCA(c.RuntimeCA); err != nil {
		return ExecutionPolicy{}, wrap(ErrPolicy, "%v", err)
	}
	if len(cfg.RuntimeEgress) == 0 {
		return ExecutionPolicy{}, wrap(ErrPolicy, "the runtime egress destination is required")
	}
	c.RuntimeEgress = make([]EgressRule, len(cfg.RuntimeEgress))
	for i, r := range cfg.RuntimeEgress {
		if _, err := netip.ParsePrefix(r.CIDR); err != nil || len(r.Ports) == 0 {
			return ExecutionPolicy{}, wrap(ErrPolicy, "an egress rule needs a CIDR and ports")
		}
		for _, p := range r.Ports {
			if p < 1 || p > 65535 {
				return ExecutionPolicy{}, wrap(ErrPolicy, "an egress port is out of range")
			}
		}
		proto := r.Protocol
		if proto == "" {
			proto = "TCP"
		}
		if proto != "TCP" && proto != "UDP" {
			return ExecutionPolicy{}, wrap(ErrPolicy, "an egress protocol is TCP or UDP")
		}
		c.RuntimeEgress[i] = EgressRule{CIDR: r.CIDR, Protocol: proto, Ports: slices.Clone(r.Ports)}
	}
	if err := cfg.Workload.Validate("workload"); err != nil {
		return ExecutionPolicy{}, wrap(ErrPolicy, "%v", err)
	}
	if err := cfg.Injector.Validate("injector"); err != nil {
		return ExecutionPolicy{}, wrap(ErrPolicy, "%v", err)
	}
	if err := cfg.Volumes.validate(); err != nil {
		return ExecutionPolicy{}, wrap(ErrPolicy, "%v", err)
	}
	if err := cfg.Volumes.fit(cfg.Workload); err != nil {
		return ExecutionPolicy{}, wrap(ErrPolicy, "%v", err)
	}
	if c.RunAsUser < 1 || c.RunAsGroup < 1 {
		return ExecutionPolicy{}, wrap(ErrPolicy, "the workload identity must be a non-root user and group")
	}
	if c.TerminationGrace < time.Second || c.TerminationGrace > 10*time.Minute {
		return ExecutionPolicy{}, wrap(ErrPolicy, "the termination grace period must be between 1s and 10m")
	}
	for _, w := range []bool{false, true} {
		set := c.ReaderActions
		if w {
			set = c.WriterActions
		}
		if len(set) == 0 {
			set = DefaultActions(w)
		}
		as, err := runtimeauth.NewActionSet(set...)
		if err != nil || !as.Contains(runtimeauth.ActionContextRead) || !as.Contains(runtimeauth.ActionResultSubmit) {
			return ExecutionPolicy{}, wrap(ErrPolicy, "an action ceiling must be valid and include context.read and result.submit")
		}
		if !w && as.Contains(runtimeauth.ActionSourcePropose) {
			return ExecutionPolicy{}, wrap(ErrPolicy, "a reader ceiling must not include source.propose")
		}
		acts := make([]runtimeauth.Action, 0, as.Len())
		for _, n := range as.Strings() {
			acts = append(acts, runtimeauth.Action(n))
		}
		if w {
			c.WriterActions = acts
		} else {
			c.ReaderActions = acts
		}
	}
	sum := sha256.Sum256(c.RuntimeCA)
	p := ExecutionPolicy{cfg: c, caHash: hex.EncodeToString(sum[:]), valid: true}
	raw, _ := json.Marshal(c)
	d := sha256.Sum256(raw)
	p.digest = "sha256:" + hex.EncodeToString(d[:])
	return p, nil
}

func validateCA(pemBytes []byte) error { return validatePEMCerts(pemBytes, "runtime CA bundle") }

// validatePEMCerts requires a bounded PEM bundle of at least one certificate and
// nothing else (no private keys). The messages carry the label only.
func validatePEMCerts(pemBytes []byte, what string) error {
	if len(pemBytes) == 0 || len(pemBytes) > 64<<10 {
		return fmt.Errorf("the %s is required", what)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return fmt.Errorf("the %s holds no certificate", what)
	}
	rest := pemBytes
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return fmt.Errorf("the %s may hold certificates only", what)
		}
	}
	return nil
}

// Valid reports whether the policy came from NewExecutionPolicy.
func (p ExecutionPolicy) Valid() bool { return p.valid }

// Digest identifies the policy content for diagnostics. It is not authority.
func (p ExecutionPolicy) Digest() string { return p.digest }

// Tenant returns a tenant's target, or ErrPolicy when the tenant is not allowed
// or the policy is the zero value.
func (p ExecutionPolicy) Tenant(t domain.TenantID) (TenantTarget, error) {
	if !p.valid {
		return TenantTarget{}, wrap(ErrPolicy, "no execution policy is configured")
	}
	tgt, ok := p.cfg.Tenants[t]
	if !ok {
		return TenantTarget{}, wrap(ErrPolicy, "the tenant has no execution target")
	}
	return tgt, nil
}

// Tenants lists the configured tenants in a stable order.
func (p ExecutionPolicy) Tenants() []domain.TenantID {
	out := make([]domain.TenantID, 0, len(p.cfg.Tenants))
	for t := range p.cfg.Tenants {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TenantOfNamespace maps a namespace back to its tenant.
func (p ExecutionPolicy) TenantOfNamespace(ns string) (domain.TenantID, bool) {
	for t, tgt := range p.cfg.Tenants {
		if tgt.Namespace == ns {
			return t, true
		}
	}
	return "", false
}

// Actions returns the explicit runtime action ceiling of an attempt.
func (p ExecutionPolicy) Actions(access execution.Access) (runtimeauth.ActionSet, error) {
	if !p.valid {
		return runtimeauth.ActionSet{}, wrap(ErrPolicy, "no execution policy is configured")
	}
	set := p.cfg.ReaderActions
	if access == execution.AccessWrite {
		set = p.cfg.WriterActions
	}
	return runtimeauth.NewActionSet(set...)
}

// RuntimeEgress returns the allowed workload destinations.
func (p ExecutionPolicy) RuntimeEgress() []EgressRule {
	out := make([]EgressRule, len(p.cfg.RuntimeEgress))
	for i, r := range p.cfg.RuntimeEgress {
		out[i] = EgressRule{CIDR: r.CIDR, Protocol: r.Protocol, Ports: slices.Clone(r.Ports)}
	}
	return out
}

// RuntimeCA returns a copy of the trust roots handed to workloads.
func (p ExecutionPolicy) RuntimeCA() []byte { return slices.Clone(p.cfg.RuntimeCA) }
