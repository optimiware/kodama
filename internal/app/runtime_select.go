package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/kube"
	kubeadapters "github.com/optimiware/kodama/internal/kube/adapters"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/logs"
	logsadapters "github.com/optimiware/kodama/internal/logs/adapters"
	logskube "github.com/optimiware/kodama/internal/logs/kubernetes"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtime/process"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
)

// RuntimeSettings is what a runtime constructor may use. The registrar is the
// runtime-authority service, bound after construction because the authority
// needs the runtime's Job observer and the runtime needs the registrar.
type RuntimeSettings struct {
	Config    RuntimeConfig
	Registrar bootstrap.Registrar
	// Store reads committed attempts for the Kubernetes runtime. It is the
	// trusted execution store, never the guarded one.
	Store execution.Store
	// Checkout issues the read-only checkout credential of source-consuming
	// attempts (Kubernetes and Podman; the Process runtime of the development
	// composition clones nothing).
	Checkout kube.CheckoutProvider
	// Ceiling is the action-ceiling policy of the Process and Podman runtimes
	// (T-096/T-101); nil keeps the default actions. The Kubernetes runtime
	// registers under its execution policy's action lists and refuses one.
	Ceiling *CeilingBinding
	// ResourceRegistrar registers run-resource admission credentials with
	// runtime admission (seam S5); it is bound, like Registrar, after the
	// authority exists. Only the Kubernetes runtime uses it.
	ResourceRegistrar kube.ResourceRegistrar
	Logger            *slog.Logger
}

// issuer is the admission credential issuer of the Process and Podman
// runtimes: the one registrar and, when configured, the ceiling policy that
// chooses each attempt's actions.
func (s RuntimeSettings) issuer() bootstrap.Issuer {
	is := bootstrap.Issuer{Registrar: s.Registrar}
	if s.Ceiling != nil {
		pol := s.Ceiling.Policy
		is.Ceiling = &pol
	}
	return is
}

// attemptActions is the attempt authority's action policy matching issuer: nil
// (the defaults) without a ceiling.
func (s RuntimeSettings) attemptActions() rpg.ActionPolicy {
	if s.Ceiling == nil {
		return nil
	}
	return s.Ceiling.Allowed
}

// RuntimeHooks are the collaborators a runtime's own workers need, available
// once the execution service exists.
type RuntimeHooks struct {
	Observations kube.Observations
	Tenants      kube.Tenants
}

// RuntimeSet is the selected runtime and every seam that belongs to the same
// backend, so a run is never started by one backend and authorized, observed
// or logged through another.
type RuntimeSet struct {
	// Runtime starts, observes, stops and removes workloads.
	Runtime execution.Runtime
	// Jobs answers runtime admission's question whether the claimed Job and
	// Pod are the attempt's.
	Jobs runtimeauth.JobObserver
	// Planner folds the backend's workload shape into the fingerprint (nil
	// selects execution.DefaultPlanner).
	Planner execution.Planner
	// Authority is the lockable attempt view that runtime grants are checked
	// against, built over the execution store.
	Authority func(*execpg.Store) (rtpg.AttemptAuthority, error)
	// Placement and Live serve the log API; Live is nil where live reads are
	// not supported (retained logs still are).
	Placement logsadapters.Placement
	Live      logs.LiveReader
	// Workers builds the backend's own background loops.
	Workers func(RuntimeHooks) ([]NamedWorker, error)
	// Close releases the runtime's resources.
	Close func()
	// Shared is the backend's contribution to the run_shared model; nil when the
	// installation did not configure it or the backend cannot enforce it.
	Shared *SharedRuntime
}

// NamedWorker is one background loop of the assembly.
type NamedWorker struct {
	Name string
	Run  func(ctx context.Context) error
}

// newRuntime selects the workload runtime of the environment (development:
// Process, test: Podman, production: Kubernetes) and applies the construction
// guard to the result.
func newRuntime(env Env, s RuntimeSettings) (RuntimeSet, error) {
	if s.Registrar == nil {
		return RuntimeSet{}, errors.New("a runtime admission registrar is required")
	}
	var (
		set RuntimeSet
		err error
	)
	switch env {
	case Development:
		set, err = newProcessRuntime(s)
	case Test:
		set, err = newPodmanRuntime(s)
	case Production:
		set, err = newKubeRuntime(s)
	default:
		return RuntimeSet{}, ErrEnvUnknown
	}
	if err != nil {
		return RuntimeSet{}, err
	}
	return AdmitRuntime(env, set)
}

// AdmitRuntime applies the construction guard of SD-12 to a runtime set: the
// runtime, its Job observer and its planner must all be admitted, and the set
// must be complete. A runtime that cannot observe its own workloads would let
// runtime admission trust a claim nobody verified.
func AdmitRuntime(env Env, set RuntimeSet) (RuntimeSet, error) {
	switch {
	case set.Runtime == nil:
		return RuntimeSet{}, fmt.Errorf("%w: a runtime is required", execution.ErrNotConfigured)
	case set.Jobs == nil:
		return RuntimeSet{}, fmt.Errorf("%w: the runtime has no Job observer for runtime admission", execution.ErrNotConfigured)
	case set.Authority == nil:
		return RuntimeSet{}, fmt.Errorf("%w: the runtime has no attempt authority", execution.ErrNotConfigured)
	}
	if _, err := Admit(env, PortRuntime, set.Runtime); err != nil {
		return RuntimeSet{}, err
	}
	if _, err := Admit(env, PortRuntime, set.Jobs); err != nil {
		return RuntimeSet{}, err
	}
	if set.Runtime.Capabilities().Backend == "" {
		return RuntimeSet{}, fmt.Errorf("%w: the runtime reports no backend identity", execution.ErrNotConfigured)
	}
	return set, nil
}

func processAuthority(namespace string, actions rpg.ActionPolicy) func(*execpg.Store) (rtpg.AttemptAuthority, error) {
	return func(store *execpg.Store) (rtpg.AttemptAuthority, error) {
		return rpg.NewAttemptAuthority(store, func(domain.Scope) (string, error) { return namespace, nil }, actions)
	}
}

func readOptional(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	return os.ReadFile(path)
}

func newProcessRuntime(s RuntimeSettings) (RuntimeSet, error) {
	c := s.Config
	rt, err := process.New(process.Config{
		Program: process.Entrypoint{Binary: c.EntrypointBinary, ServerURL: c.ServerURL, CAFile: c.CAFile, Issuer: s.issuer()},
		Dir:     c.StateDir,
	})
	if err != nil {
		return RuntimeSet{}, fmt.Errorf("process runtime: %w", err)
	}
	return RuntimeSet{Runtime: rt, Jobs: rt, Authority: processAuthority(process.Namespace, s.attemptActions()), Placement: nil, Live: nil}, nil
}

// podmanProgram is what a Podman attempt's container runs: the step entrypoint
// with the composition's one admission issuer (and so its action ceiling) and
// the one checkout provider (T-098's seam, T-101's binding).
func podmanProgram(s RuntimeSettings, ca []byte) podman.Entrypoint {
	c := s.Config
	return podman.Entrypoint{
		Binary: c.EntrypointBinary, ServerURL: c.ServerURL, CA: ca, Issuer: s.issuer(), Checkout: podmanCheckout(s.Checkout),
	}
}

func newPodmanRuntime(s RuntimeSettings) (RuntimeSet, error) {
	c := s.Config
	ca, err := readOptional(c.CAFile)
	if err != nil {
		return RuntimeSet{}, fmt.Errorf("podman runtime: runtime CA: %w", err)
	}
	rt, err := podman.New(podman.Config{
		Program:  podmanProgram(s, ca),
		StateDir: c.StateDir, Binary: c.Podman.Binary, CPUs: c.Podman.CPUs, Memory: c.Podman.Memory, PIDs: c.Podman.PIDs, Network: c.Podman.Network,
	})
	if err != nil {
		return RuntimeSet{}, fmt.Errorf("podman runtime: %w", err)
	}
	return RuntimeSet{Runtime: rt, Jobs: rt, Authority: processAuthority(podman.Namespace, s.attemptActions())}, nil
}

// loadKubePolicy decodes the execution policy file strictly: an unknown field
// is a configuration error, so a misspelled bound cannot silently leave a
// default in force.
func loadKubePolicy(path string) (kube.PolicyConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return kube.PolicyConfig{}, fmt.Errorf("execution policy: %w", err)
	}
	var cfg kube.PolicyConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return kube.PolicyConfig{}, fmt.Errorf("execution policy: %w", err)
	}
	if dec.More() {
		return kube.PolicyConfig{}, errors.New("execution policy: trailing data after the document")
	}
	return cfg, nil
}

func newKubeRuntime(s RuntimeSettings) (RuntimeSet, error) {
	k := s.Config.Kubernetes
	if s.Ceiling != nil {
		return RuntimeSet{}, errors.New("the Kubernetes runtime registers attempts under its execution policy's action lists: a per-kind action ceiling is not wired there yet (T-089)")
	}
	if s.Store == nil {
		return RuntimeSet{}, errors.New("the Kubernetes runtime reads committed attempts: an execution store is required")
	}
	var cfg kube.PolicyConfig
	if k.Policy != nil {
		cfg = *k.Policy
	} else {
		var err error
		if cfg, err = loadKubePolicy(k.PolicyFile); err != nil {
			return RuntimeSet{}, err
		}
	}
	if cfg.RuntimeURL != s.Config.ServerURL {
		return RuntimeSet{}, errors.New("execution policy: the runtime callback origin must equal KODAMA_RUNTIME_URL")
	}
	policy, err := kube.NewExecutionPolicy(cfg)
	if err != nil {
		return RuntimeSet{}, fmt.Errorf("execution policy: %w", err)
	}
	httpClient, err := newKubeHTTPClient(k)
	if err != nil {
		return RuntimeSet{}, err
	}
	client, err := kubernetes.NewClient(kubernetes.Config{Origin: k.APIURL, HTTP: httpClient})
	if err != nil {
		return RuntimeSet{}, err
	}
	attempts, err := kubeadapters.NewAttempts(s.Store)
	if err != nil {
		return RuntimeSet{}, err
	}
	deps := kube.Dependencies{Policy: policy, Cluster: client, Attempts: attempts, Registrar: s.Registrar, Checkout: s.Checkout, Logs: client}
	opts := kube.Options{Logger: s.Logger}
	// The run_shared model (O-30) is composed only when the installation's
	// settings document carries its section; otherwise nothing of it exists and
	// new-format admission stays closed.
	var shared *SharedRuntime
	if rs := k.RunShared; rs != nil {
		if shared, err = newKubeShared(s, rs, policy, client, opts, &deps); err != nil {
			return RuntimeSet{}, err
		}
	}
	rt, err := kube.NewRuntime(deps, opts)
	if err != nil {
		return RuntimeSet{}, err
	}
	jobs, err := kube.NewJobObserver(deps, opts)
	if err != nil {
		return RuntimeSet{}, err
	}
	live, err := logskube.NewReader(logskube.Config{Endpoint: k.APIURL, Client: httpClient})
	if err != nil {
		return RuntimeSet{}, err
	}
	var runtime execution.Runtime = rt
	if shared != nil {
		runtime = withRunResources{rt}
	}
	return RuntimeSet{
		Runtime: runtime, Jobs: jobs, Planner: kube.NewPlanner(policy),
		Authority: func(store *execpg.Store) (rtpg.AttemptAuthority, error) {
			return kubeadapters.NewAuthority(store, policy)
		},
		Placement: kubePlacement{policy: policy}, Live: live, Shared: shared,
		Workers: func(h RuntimeHooks) ([]NamedWorker, error) {
			d := deps
			d.Observations, d.Tenants = h.Observations, h.Tenants
			rec, err := kube.NewReconciler(d, opts)
			if err != nil {
				return nil, err
			}
			return []NamedWorker{{Name: "kube-reconciler", Run: func(ctx context.Context) error {
				return loop(ctx, 5*time.Second, func(ctx context.Context) error { _, err := rec.Pass(ctx); return err }, s.Logger, "kubernetes reconcile")
			}}}, nil
		},
	}, nil
}

// newKubeShared builds the Kubernetes run-resource adapter over the retained
// policy list and joins it to the attempt runtime's dependencies: the same
// adapter verifies the per-run network policies of the namespace during a
// run_shared attempt's preflight (Dependencies.OwnedPolicies), without which a
// namespace holding run policies would fail it. The Kubernetes API client is the
// cluster of both.
func newKubeShared(s RuntimeSettings, rs *RunSharedConfig, policy kube.ExecutionPolicy, client *kubernetes.Client, opts kube.Options, deps *kube.Dependencies) (*SharedRuntime, error) {
	if s.ResourceRegistrar == nil {
		return nil, errors.New("run_shared needs the runtime admission registrar of run resources")
	}
	list, err := NewPolicyList(*rs)
	if err != nil {
		return nil, err
	}
	var qualified execution.ResourceCapabilitySet
	if rs.BuilderRootless {
		qualified = execution.Set(execution.CapBuilderRootless)
	}
	rr, err := kube.NewRunResources(kube.RunResourceDependencies{
		Policy: policy, Cluster: client, Catalog: list, Registrar: s.ResourceRegistrar, Checkout: s.Checkout, Logs: client,
	}, kube.RunResourceOptions{Options: opts, ClusterCIDRs: rs.ClusterCIDRs, Qualified: qualified})
	if err != nil {
		return nil, fmt.Errorf("run resources: %w", err)
	}
	deps.OwnedPolicies = rr
	return &SharedRuntime{Resources: rr, Workloads: kubeWorkloads{rr: rr}, Policies: list}, nil
}

// withRunResources declares the run-resources marker on a runtime whose backend
// also implements the companion port. The attempt runtime's own capabilities are
// the original six guarantees; every run_shared run requires the marker too
// ([run resources](docs/contracts/run-resources.md#capabilities)), so without this
// declaration a composed Kubernetes backend would refuse every run_shared
// admission with capability_unsupported.
type withRunResources struct{ execution.Runtime }

func (w withRunResources) Capabilities() execution.Capabilities {
	c := w.Runtime.Capabilities()
	c.Guarantees = c.Guarantees.WithRunResources()
	return c
}

// kubePlacement resolves the trusted namespace of an attempt's Job from the
// execution policy; the container is the step's primary container.
type kubePlacement struct{ policy kube.ExecutionPolicy }

var _ logsadapters.Placement = kubePlacement{}

func (p kubePlacement) Placement(_ context.Context, a logs.AttemptInfo) (string, string, error) {
	t, err := p.policy.Tenant(a.Scope.TenantID)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", logs.ErrNotConfigured, err)
	}
	return t.Namespace, kube.PrimaryContainer, nil
}

// newKubeHTTPClient authenticates every API request with the service account
// token, read from its file per request because the kubelet rotates it, and
// trusts only the cluster CA. Redirects are never followed: a redirect would
// forward the token to another origin.
func newKubeHTTPClient(k KubernetesConfig) (*http.Client, error) {
	caPEM, err := os.ReadFile(k.CAFile)
	if err != nil {
		return nil, fmt.Errorf("kubernetes CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("kubernetes CA: no certificate in the file")
	}
	if _, err := readToken(k.TokenFile); err != nil {
		return nil, err
	}
	base := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, ResponseHeaderTimeout: 30 * time.Second}
	return &http.Client{
		Transport:     &bearerTransport{base: base, tokenFile: k.TokenFile},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("kubernetes token: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("kubernetes token: the file is empty")
	}
	return t, nil
}

type bearerTransport struct {
	base      http.RoundTripper
	tokenFile string
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	token, err := readToken(t.tokenFile)
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(r)
}

// loop runs fn now and then every interval until ctx ends. A failed pass is
// logged and repeated: every pass reads durable state, so a failure loses
// nothing a later pass will not repair.
func loop(ctx context.Context, interval time.Duration, fn func(context.Context) error, log *slog.Logger, what string) error {
	if interval <= 0 {
		return errors.New("a loop needs a positive interval")
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := fn(ctx); err != nil && ctx.Err() == nil && log != nil {
			log.Warn(what+" pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
