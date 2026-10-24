package kube_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

type fixture struct {
	policy kube.ExecutionPolicy
	tenant domain.TenantID
	ctx    runtimev1.Context
	rec    kube.AttemptRecord
	exp    kube.Expectation
	fake   *kubetest.Fake
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	p := kubetest.Policy(t, "acme", "globex")
	tid, _ := kubetest.Tenant("acme")
	c := kubetest.Context(t, kubetest.ContextSpec{Tenant: tid, Project: "p_inv", Run: "run_1", Step: "build", Attempt: "att_1", Image: kubetest.WorkloadImage})
	rec := kubetest.Record(t, p, c)
	exp, err := p.Expect(rec)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{policy: p, tenant: tid, ctx: c, rec: rec, exp: exp, fake: kubetest.NewFake()}
}

// Test_policy_and_fingerprint: deterministic recipes, every security-relevant
// mutation detected, invalid or missing policy denied, and configuration drift
// never silently replans an attempt.
func Test_policy_and_fingerprint(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		f := newFixture(t)
		again, err := kube.NewPlanner(f.policy).Plan(f.tenant, f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if again.SpecFingerprint != f.rec.SpecFingerprint || again.WorkloadName != f.rec.JobName || again.ContextDigest != f.rec.ContextDigest {
			t.Fatal("planning twice yields a different identity")
		}
		other := kubetest.Context(t, kubetest.ContextSpec{Tenant: f.tenant, Project: "p_inv", Run: "run_1", Step: "build", Attempt: "att_2", Image: kubetest.WorkloadImage})
		p2, err := kube.NewPlanner(f.policy).Plan(f.tenant, other)
		if err != nil {
			t.Fatal(err)
		}
		if p2.SpecFingerprint == f.rec.SpecFingerprint || p2.WorkloadName == f.rec.JobName {
			t.Fatal("another attempt shares a Job name or fingerprint")
		}
		// The canonical context and digest are byte-identical to execution's default planner.
		base, _ := execution.DefaultPlanner{}.Plan(f.tenant, f.ctx)
		if string(base.Context) != string(again.Context) || base.ContextDigest != again.ContextDigest {
			t.Fatal("the pinned context differs from the default planner's")
		}
	})

	t.Run("excludes generated state", func(t *testing.T) {
		f := newFixture(t)
		raw, _ := json.Marshal(f.exp.Job)
		for _, generated := range []string{`"uid"`, `"resourceVersion"`, `"creationTimestamp"`, `"deletionTimestamp"`, `"status":{"`, "adm_"} {
			if strings.Contains(string(raw), generated) {
				t.Fatalf("the recipe document carries %s", generated)
			}
		}
	})

	t.Run("denies missing or invalid policy", func(t *testing.T) {
		f := newFixture(t)
		var zero kube.ExecutionPolicy
		if _, err := kube.NewPlanner(zero).Plan(f.tenant, f.ctx); !errors.Is(err, kube.ErrPolicy) {
			t.Fatalf("zero policy planned: %v", err)
		}
		stranger := kubetest.Context(t, kubetest.ContextSpec{Tenant: "t021_stranger", Project: "p_inv", Run: "run_1", Step: "build", Attempt: "att_9", Image: kubetest.WorkloadImage})
		if _, err := kube.NewPlanner(f.policy).Plan("t021_stranger", stranger); !errors.Is(err, kube.ErrPolicy) {
			t.Fatalf("an uncovered tenant planned: %v", err)
		}
		if _, err := kube.NewPlanner(f.policy).Plan("t021_globex", f.ctx); !errors.Is(err, kube.ErrPolicy) {
			t.Fatalf("a context of another tenant planned: %v", err)
		}
		if _, err := zero.Tenant(f.tenant); !errors.Is(err, kube.ErrPolicy) {
			t.Fatal("zero policy resolved a tenant")
		}
		if _, err := zero.Actions(execution.AccessRead); !errors.Is(err, kube.ErrPolicy) {
			t.Fatal("zero policy resolved actions")
		}
		if _, err := zero.Expect(f.rec); !errors.Is(err, kube.ErrPolicy) {
			t.Fatal("zero policy expected a Job")
		}
	})

	t.Run("invalid configuration is rejected", func(t *testing.T) {
		base := func() kube.PolicyConfig { return kubetest.PolicyConfig(t, "acme") }
		cases := map[string]func(*kube.PolicyConfig){
			"no tenants": func(c *kube.PolicyConfig) { c.Tenants = nil },
			"bad namespace": func(c *kube.PolicyConfig) {
				c.Tenants["t021_acme"] = kube.TenantTarget{Namespace: "Bad_NS", ServiceAccount: "x"}
			},
			"shared namespace":      func(c *kube.PolicyConfig) { c.Tenants["t021_other"] = c.Tenants["t021_acme"] },
			"tag image":             func(c *kube.PolicyConfig) { c.InjectorImage = "registry.invalid/injector:latest" },
			"no injector command":   func(c *kube.PolicyConfig) { c.InjectorCommand = nil },
			"unknown pull policy":   func(c *kube.PolicyConfig) { c.ImagePullPolicy = "Sometimes" },
			"plain http callback":   func(c *kube.PolicyConfig) { c.RuntimeURL = "http://192.0.2.10:8080" },
			"callback with path":    func(c *kube.PolicyConfig) { c.RuntimeURL = "https://192.0.2.10:8443/x" },
			"callback credentials":  func(c *kube.PolicyConfig) { c.RuntimeURL = "https://u:p@192.0.2.10" },
			"no CA":                 func(c *kube.PolicyConfig) { c.RuntimeCA = nil },
			"garbage CA":            func(c *kube.PolicyConfig) { c.RuntimeCA = []byte("not pem") },
			"no egress":             func(c *kube.PolicyConfig) { c.RuntimeEgress = nil },
			"egress without ports":  func(c *kube.PolicyConfig) { c.RuntimeEgress[0].Ports = nil },
			"egress bad cidr":       func(c *kube.PolicyConfig) { c.RuntimeEgress[0].CIDR = "everywhere" },
			"missing cpu":           func(c *kube.PolicyConfig) { c.Workload.CPULimit = "" },
			"zero memory":           func(c *kube.PolicyConfig) { c.Workload.MemoryLimit = "0" },
			"request over limit":    func(c *kube.PolicyConfig) { c.Workload.CPURequest = "2" },
			"missing ephemeral":     func(c *kube.PolicyConfig) { c.Injector.EphemeralLimit = "" },
			"missing volume":        func(c *kube.PolicyConfig) { c.Volumes.Workspace = "" },
			"disk over ephemeral":   func(c *kube.PolicyConfig) { c.Volumes.Workspace = "10Gi" },
			"memory volumes oversz": func(c *kube.PolicyConfig) { c.Volumes.Secrets = "512Mi" },
			"root user":             func(c *kube.PolicyConfig) { c.RunAsUser = 0 },
			"root group":            func(c *kube.PolicyConfig) { c.RunAsGroup = 0 },
			"no grace":              func(c *kube.PolicyConfig) { c.TerminationGrace = 0 },
			"reader may propose": func(c *kube.PolicyConfig) {
				c.ReaderActions = []runtimeauth.Action{runtimeauth.ActionContextRead, runtimeauth.ActionResultSubmit, runtimeauth.ActionSourcePropose}
			},
			"no result action": func(c *kube.PolicyConfig) { c.ReaderActions = []runtimeauth.Action{runtimeauth.ActionContextRead} },
			"unknown action": func(c *kube.PolicyConfig) {
				c.WriterActions = []runtimeauth.Action{"admin.everything", runtimeauth.ActionContextRead}
			},
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				cfg := base()
				mutate(&cfg)
				if _, err := kube.NewExecutionPolicy(cfg); !errors.Is(err, kube.ErrPolicy) {
					t.Fatalf("accepted: %v", err)
				}
			})
		}
		if _, err := kube.NewExecutionPolicy(base()); err != nil {
			t.Fatalf("the baseline configuration is rejected: %v", err)
		}
	})

	t.Run("every recipe input is in the fingerprint", func(t *testing.T) {
		f := newFixture(t)
		mutations := map[string]func(*kube.PolicyConfig){
			"namespace": func(c *kube.PolicyConfig) {
				c.Tenants["t021_acme"] = kube.TenantTarget{Namespace: "t021-elsewhere", ServiceAccount: "t021-runner"}
			},
			"service account": func(c *kube.PolicyConfig) {
				c.Tenants["t021_acme"] = kube.TenantTarget{Namespace: "t021-acme", ServiceAccount: "other"}
			},
			"injector image": func(c *kube.PolicyConfig) {
				c.InjectorImage = "registry.t021.invalid/injector@sha256:" + strings.Repeat("3", 64)
			},
			"injector cmd":    func(c *kube.PolicyConfig) { c.InjectorCommand = []string{"cp", "/x", "/kodama/bin/kodama"} },
			"pull policy":     func(c *kube.PolicyConfig) { c.ImagePullPolicy = "Always" },
			"callback":        func(c *kube.PolicyConfig) { c.RuntimeURL = "https://192.0.2.99:8443" },
			"ca":              func(c *kube.PolicyConfig) { c.RuntimeCA = kubetest.CA(t) },
			"cpu limit":       func(c *kube.PolicyConfig) { c.Workload.CPULimit = "2" },
			"memory request":  func(c *kube.PolicyConfig) { c.Workload.MemoryRequest = "256Mi" },
			"ephemeral limit": func(c *kube.PolicyConfig) { c.Workload.EphemeralLimit = "2Gi" },
			"injector memory": func(c *kube.PolicyConfig) { c.Injector.MemoryLimit = "128Mi" },
			"workspace size":  func(c *kube.PolicyConfig) { c.Volumes.Workspace = "128Mi" },
			"secrets size":    func(c *kube.PolicyConfig) { c.Volumes.Secrets = "8Mi" },
			"user":            func(c *kube.PolicyConfig) { c.RunAsUser = 10001 },
			"group":           func(c *kube.PolicyConfig) { c.RunAsGroup = 10001 },
			"grace":           func(c *kube.PolicyConfig) { c.TerminationGrace = time.Minute },
			"reader actions": func(c *kube.PolicyConfig) {
				c.ReaderActions = []runtimeauth.Action{runtimeauth.ActionContextRead, runtimeauth.ActionResultSubmit}
			},
		}
		seen := map[string]string{f.rec.SpecFingerprint: "baseline"}
		for name, mutate := range mutations {
			cfg := kubetest.PolicyConfig(t, "acme")
			// Keep the same CA and tenants as the fixture except where mutated.
			cfg.RuntimeCA = f.policy.RuntimeCA()
			mutate(&cfg)
			p, err := kube.NewExecutionPolicy(cfg)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			plan, err := kube.NewPlanner(p).Plan(f.tenant, f.ctx)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if other, dup := seen[plan.SpecFingerprint]; dup {
				t.Fatalf("mutating %s leaves the fingerprint equal to %s", name, other)
			}
			seen[plan.SpecFingerprint] = name
			// A changed policy never silently replans the attempt.
			if _, err := p.Expect(f.rec); !errors.Is(err, kube.ErrPolicyDrift) {
				t.Fatalf("%s: drift not detected: %v", name, err)
			}
		}
		// The workload image comes from the committed context.
		img := kubetest.Context(t, kubetest.ContextSpec{Tenant: f.tenant, Project: "p_inv", Run: "run_1", Step: "build", Attempt: "att_1",
			Image: "registry.t021.invalid/workload@sha256:" + strings.Repeat("9", 64)})
		plan, _ := kube.NewPlanner(f.policy).Plan(f.tenant, img)
		if plan.SpecFingerprint == f.rec.SpecFingerprint {
			t.Fatal("the workload image is not in the fingerprint")
		}
		// Writers get a different action ceiling than readers.
		wr := kubetest.Context(t, kubetest.ContextSpec{Tenant: f.tenant, Project: "p_inv", Run: "run_1", Step: "build", Attempt: "att_1", Image: kubetest.WorkloadImage, Access: runtimev1.AccessWrite})
		wplan, err := kube.NewPlanner(f.policy).Plan(f.tenant, wr)
		if err != nil || wplan.SpecFingerprint == f.rec.SpecFingerprint {
			t.Fatalf("writer fingerprint: %v", err)
		}
		ra, _ := f.policy.Actions(execution.AccessRead)
		wa, _ := f.policy.Actions(execution.AccessWrite)
		if ra.Contains(runtimeauth.ActionSourcePropose) || !wa.Contains(runtimeauth.ActionSourcePropose) {
			t.Fatal("source.propose belongs to writers only")
		}
	})

	t.Run("expect refuses inconsistent committed data", func(t *testing.T) {
		f := newFixture(t)
		mutate := map[string]func(*kube.AttemptRecord){
			"digest":   func(r *kube.AttemptRecord) { r.ContextDigest = "sha256:" + strings.Repeat("0", 64) },
			"job name": func(r *kube.AttemptRecord) { r.JobName = "kdm-" + strings.Repeat("0", 40) },
			"attempt":  func(r *kube.AttemptRecord) { r.AttemptID = "att_other" },
			"scope":    func(r *kube.AttemptRecord) { r.Scope.ProjectID = "p_other" },
			"context":  func(r *kube.AttemptRecord) { r.Context = []byte(`{}`) },
		}
		for name, m := range mutate {
			r := f.rec
			m(&r)
			if _, err := f.policy.Expect(r); err == nil {
				t.Fatalf("%s: accepted", name)
			}
		}
		r := f.rec
		r.SpecFingerprint = "sha256:" + strings.Repeat("0", 64)
		if _, err := f.policy.Expect(r); !errors.Is(err, kube.ErrPolicyDrift) {
			t.Fatalf("fingerprint mismatch: %v", err)
		}
	})
}

// restrictedJob checks the generated Job has the restricted shape of PX-04 and
// IT-07.
func Test_job_shape(t *testing.T) {
	f := newFixture(t)
	j := f.exp.Job
	if *j.Spec.BackoffLimit != 0 || *j.Spec.Parallelism != 1 || *j.Spec.Completions != 1 {
		t.Fatal("a Job is one attempt: parallelism 1, completions 1, backoffLimit 0")
	}
	spec := j.Spec.Template.Spec
	if len(spec.Containers) != 1 || len(spec.InitContainers) != 1 {
		t.Fatal("one primary container and one injector")
	}
	if spec.RestartPolicy != "Never" || *spec.AutomountServiceAccountToken || *spec.EnableServiceLinks || spec.HostNetwork || spec.HostPID || spec.HostIPC {
		t.Fatal("restart policy, token automount, service links or host namespaces")
	}
	if !*spec.SecurityContext.RunAsNonRoot || spec.SecurityContext.SeccompProfile.Type != "RuntimeDefault" || *spec.SecurityContext.RunAsUser == 0 {
		t.Fatal("pod security context")
	}
	for _, c := range append(append([]kubernetes.Container{}, spec.InitContainers...), spec.Containers...) {
		sc := c.SecurityContext
		if *sc.Privileged || *sc.AllowPrivilegeEscalation || !*sc.RunAsNonRoot || sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 0 || sc.SeccompProfile.Type != "RuntimeDefault" {
			t.Fatalf("container %s security", c.Name)
		}
		for _, k := range []string{"cpu", "memory", "ephemeral-storage"} {
			if c.Resources.Requests[k] == "" || c.Resources.Limits[k] == "" {
				t.Fatalf("container %s has no %s bound", c.Name, k)
			}
		}
		if !strings.Contains(c.Image, "@sha256:") {
			t.Fatalf("container %s image is not pinned", c.Name)
		}
	}
	for _, v := range spec.Volumes {
		if v.EmptyDir == nil && v.Secret == nil {
			t.Fatalf("volume %s is neither private scratch nor the bootstrap Secret", v.Name)
		}
		if v.EmptyDir != nil && v.EmptyDir.SizeLimit == "" {
			t.Fatalf("volume %s is unbounded", v.Name)
		}
	}
	for _, name := range []string{"secrets", "identity"} {
		for _, v := range spec.Volumes {
			if v.Name == name && v.EmptyDir.Medium != "Memory" {
				t.Fatalf("%s must be memory backed", name)
			}
		}
	}
	// The tools volume is read-only to the command.
	for _, m := range spec.Containers[0].VolumeMounts {
		if m.MountPath == "/kodama/bin" && !m.ReadOnly {
			t.Fatal("/kodama/bin is writable by the command")
		}
	}
	if spec.Containers[0].Command[0] != "/kodama/bin/kodama" || spec.Containers[0].Args[0] != "step" {
		t.Fatal("the entrypoint is not /kodama/bin/kodama step")
	}
	// T-016's configuration names, https callback, bootstrap files.
	joined := strings.Join(spec.Containers[0].Args, " ")
	for _, want := range []string{"--server-url=https://", "--attempt-id=att_1", "--admission-credential-file=/kodama/bootstrap/admission", "--ca-file=/kodama/bootstrap/ca.crt"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %s", want, joined)
		}
	}
	if j.Spec.Template.Spec.Containers[0].Env[1].ValueFrom == nil {
		t.Fatal("the Job UID must come from the Job controller's Pod label through the Downward API")
	}
	for k := range j.Metadata.Annotations {
		if strings.Contains(strings.ToLower(k), "credential") {
			t.Fatal("credential in annotations")
		}
	}
}
