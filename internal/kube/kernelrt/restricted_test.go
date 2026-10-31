package kernelrt

import (
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

func jobDocument(t *testing.T, j kubernetes.Job) string {
	t.Helper()
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// racyCluster runs a hook just before a Job delete, as a concurrent actor
// replacing the object between the adapter's read and its delete would.
type racyCluster struct {
	*fakeCluster
	beforeDeleteJob func()
}

func (r *racyCluster) DeleteJob(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	if r.beforeDeleteJob != nil {
		r.beforeDeleteJob()
	}
	return r.fakeCluster.DeleteJob(ctx, ns, name, o)
}

// Test_unit_kube_restricted_workload_and_cleanup proves the Job this adapter
// writes is restricted, that adoption re-verifies that shape on the live object
// and refuses a loosened one, and that cleanup deletes only owned objects under
// UID preconditions and reports what it retains. The cluster is an in-memory
// double: this is evidence of the written objects, not of kubelet, admission or
// CSI enforcement.
func Test_unit_kube_restricted_workload_and_cleanup(t *testing.T) {
	ctx := context.Background()

	t.Run("the Job and its Pod are restricted", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		svc, err := h.res.Ensure(ctx, h.serviceSpec(scopeA, "run-1", "db", 1))
		if err != nil {
			t.Fatal(err)
		}
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		req := h.startRequest(b, ws)
		req.Services = []runtime.ResourceHandle{svc.Handle}
		h.mustStart(req)
		job, _ := h.cl.job(testNS, h.jobName(b))

		sp := job.Spec
		if *sp.Parallelism != 1 || *sp.Completions != 1 || *sp.BackoffLimit != 0 || sp.ActiveDeadlineSeconds == nil || *sp.ActiveDeadlineSeconds != 90 {
			t.Fatalf("not a bounded single attempt: %+v", sp)
		}
		pod := sp.Template.Spec
		if *pod.AutomountServiceAccountToken || *pod.EnableServiceLinks {
			t.Fatal("the Pod mounts a service account token or service links")
		}
		if pod.ServiceAccountName != testSA || pod.RestartPolicy != "Never" || pod.HostNetwork || pod.HostPID || pod.HostIPC || pod.NodeName != "" {
			t.Fatalf("pod = %+v", pod)
		}
		if len(pod.InitContainers) != 0 || len(pod.Containers) != 1 {
			t.Fatalf("containers = %d init %d", len(pod.Containers), len(pod.InitContainers))
		}
		ps := pod.SecurityContext
		if !*ps.RunAsNonRoot || *ps.RunAsUser != testUID || *ps.RunAsGroup != testUID || ps.SeccompProfile.Type != "RuntimeDefault" {
			t.Fatalf("pod security = %+v", ps)
		}
		c := pod.Containers[0]
		if !pinned(c.Image) || c.Image != pinnedImage || c.WorkingDir != "/workspace" {
			t.Fatalf("image %q dir %q", c.Image, c.WorkingDir)
		}
		if !slices.Equal(c.Command, []string{"/bin/sh", "-ec"}) || len(c.Args) != 1 || c.Args[0] != "echo hi\nmake test" {
			t.Fatalf("command %v args %q", c.Command, c.Args)
		}
		sc := c.SecurityContext
		if *sc.Privileged || *sc.AllowPrivilegeEscalation || !*sc.RunAsNonRoot || *sc.RunAsUser != testUID ||
			!slices.Equal(sc.Capabilities.Drop, []string{"ALL"}) || len(sc.Capabilities.Add) != 0 || sc.SeccompProfile.Type != "RuntimeDefault" {
			t.Fatalf("container security = %+v", sc)
		}
		for _, k := range []string{"cpu", "memory", "ephemeral-storage"} {
			if c.Resources.Requests[k] == "" || c.Resources.Limits[k] == "" {
				t.Fatalf("no %s bound: %+v", k, c.Resources)
			}
		}
		for _, v := range pod.Volumes {
			switch {
			case v.PersistentVolumeClaim != nil:
				if v.PersistentVolumeClaim.ClaimName != workspaceIdent(h, "run-1").base() || v.PersistentVolumeClaim.ReadOnly {
					t.Fatalf("workspace volume = %+v", v)
				}
			case v.EmptyDir != nil && v.EmptyDir.SizeLimit != "" && v.EmptyDir.Medium == "":
			default:
				t.Fatalf("unexpected volume %+v", v)
			}
		}
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		if env["KODAMA_RUN_ID"] != "run-1" || env["CI"] != "1" || env["HOME"] != "/tmp" {
			t.Fatalf("env = %v", env)
		}
		wantHost := workspaceIdentFor(h, h.serviceSpec(scopeA, "run-1", "db", 1)).base() + "." + testNS + ".svc"
		if env["KODAMA_SERVICE_DB_HOST"] != wantHost {
			t.Fatalf("service host = %q, want %q", env["KODAMA_SERVICE_DB_HOST"], wantHost)
		}
		// The adapter accepts what it writes.
		if err := h.verifyJob(job, b); err != nil {
			t.Fatalf("its own Job does not verify: %v", err)
		}
		// Job and record carry the full ownership evidence.
		a := job.Metadata.Annotations
		if a[annTenant] != "tenant-a" || a[annRun] != "run-1" || a[annIdentity] != b.WorkloadID || a[annDigest] != "d" || a[annFence] != "1" {
			t.Fatalf("annotations = %v", a)
		}
	})

	t.Run("a service Pod is restricted", func(t *testing.T) {
		h := newHarness(t)
		spec := h.serviceSpec(scopeA, "run-1", "db", 1)
		if _, err := h.res.Ensure(ctx, spec); err != nil {
			t.Fatal(err)
		}
		id := workspaceIdentFor(h, spec)
		pod, _ := h.cl.GetPod(ctx, testNS, id.base())
		if err := h.verifyPod(pod, id); err != nil {
			t.Fatal(err)
		}
		c := pod.Spec.Containers[0]
		if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket.Port != 5432 || c.Ports[0].ContainerPort != 5432 {
			t.Fatalf("readiness = %+v ports %+v", c.ReadinessProbe, c.Ports)
		}
		svc, _ := h.cl.GetService(ctx, testNS, id.base())
		if svc.Spec.Type != "ClusterIP" || svc.Spec.Ports[0].Port != 5432 || svc.Spec.Selector[labelObject] != id.base() {
			t.Fatalf("service = %+v", svc.Spec)
		}
	})

	t.Run("adoption refuses a Job that is not the restricted shape", func(t *testing.T) {
		mutations := map[string]func(*kubernetes.Job){
			"privileged": func(j *kubernetes.Job) { j.Spec.Template.Spec.Containers[0].SecurityContext.Privileged = ptr(true) },
			"escalation": func(j *kubernetes.Job) {
				j.Spec.Template.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation = ptr(true)
			},
			"root": func(j *kubernetes.Job) { *j.Spec.Template.Spec.Containers[0].SecurityContext.RunAsUser = 0 },
			"capability": func(j *kubernetes.Job) {
				j.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Add = []string{"SYS_ADMIN"}
			},
			"capabilities kept": func(j *kubernetes.Job) { j.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Drop = nil },
			"unpinned image":    func(j *kubernetes.Job) { j.Spec.Template.Spec.Containers[0].Image = "registry.example/step:latest" },
			"no limits":         func(j *kubernetes.Job) { j.Spec.Template.Spec.Containers[0].Resources = &kubernetes.Resources{} },
			"token":             func(j *kubernetes.Job) { j.Spec.Template.Spec.AutomountServiceAccountToken = ptr(true) },
			"host network":      func(j *kubernetes.Job) { j.Spec.Template.Spec.HostNetwork = true },
			"init container": func(j *kubernetes.Job) {
				j.Spec.Template.Spec.InitContainers = []kubernetes.Container{j.Spec.Template.Spec.Containers[0]}
			},
			"retries": func(j *kubernetes.Job) { j.Spec.BackoffLimit = ptr(int32(6)) },
			"seccomp": func(j *kubernetes.Job) {
				j.Spec.Template.Spec.Containers[0].SecurityContext.SeccompProfile = &kubernetes.SeccompProfile{Type: "Unconfined"}
			},
		}
		for name, mutate := range mutations {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				ws := h.ensureWorkspace(scopeA, "run-1", 1)
				b := binding(scopeA, "run-1", "build", 1, "d", 1)
				h.mustStart(h.startRequest(b, ws))
				h.cl.mu.Lock()
				k := key(testNS, h.jobName(b))
				loose := deepCopyJob(t, h.cl.jobs[k].(kubernetes.Job))
				mutate(&loose)
				h.cl.jobs[k] = loose
				h.cl.mu.Unlock()

				if _, err := h.start(h.startRequest(b, ws)); !errors.Is(err, ErrUnverified) || !errors.Is(err, runtime.ErrSpecMismatch) || !isNotReached(err) {
					t.Fatalf("adopted a loosened Job: %v", err)
				}
				if _, err := h.rt.Observe(ctx, b); !errors.Is(err, ErrUnverified) {
					t.Fatalf("observe = %v", err)
				}
				// It is still ours to remove.
				if _, err := h.rt.Stop(ctx, b); err != nil {
					t.Fatalf("stop of a loosened owned Job: %v", err)
				}
				if h.cl.deleteCount("job") != 1 {
					t.Fatal("the loosened Job was not deleted")
				}
			})
		}
	})

	t.Run("a delete never reaches an object that changed after it was read", func(t *testing.T) {
		h := newHarness(t)
		rc := &racyCluster{fakeCluster: h.cl}
		ad, err := New(testConfig(), Dependencies{Cluster: rc, Targets: SingleTarget(Target{Namespace: testNS, ServiceAccount: testSA})}, Options{Clock: h.clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.mustStart(h.startRequest(b, ws))
		rc.beforeDeleteJob = func() { h.cl.replaceJob(testNS, h.jobName(b)) }
		_, err = ad.Runtime().Stop(ctx, b)
		if !errors.Is(err, kubernetes.ErrConflict) {
			t.Fatalf("stop = %v, want a UID conflict", err)
		}
		if _, ok := h.cl.job(testNS, h.jobName(b)); !ok || h.cl.deleteCount("job") != 0 {
			t.Fatal("an object with another UID was deleted")
		}
	})

	t.Run("cleanup removes owned objects, leaves foreign ones and reports retained ones", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		svc, err := h.res.Ensure(ctx, h.serviceSpec(scopeA, "run-1", "db", 1))
		if err != nil {
			t.Fatal(err)
		}
		ref := runtime.SecretRef{Owner: domain.Owner{TenantID: "tenant-a"}, Name: "TOKEN"}
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		req := h.startRequest(b, ws)
		req.Spec.Secrets = []runtime.SecretRef{ref}
		req.Secrets = runtime.Materialization{Scope: scopeA, RunID: "run-1", StepKey: "build", Directory: "/kodama/secrets", Refs: []runtime.SecretRef{ref}}
		req.Services = []runtime.ResourceHandle{svc.Handle}
		h.mustStart(req)
		h.cl.startPod(testNS, h.jobName(b))

		// Another tenant's run in the same namespace, and a stray object nobody owns.
		wsB := h.ensureWorkspace(scopeB, "run-9", 1)
		stray := kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: "unrelated"}}
		h.cl.putJob(testNS, stray)

		// The workspace volume is retained by the cluster's reclaim policy.
		pvc := workspaceIdent(h, "run-1").base()
		h.cl.setPVC(testNS, pvc, func(p *kubernetes.PersistentVolumeClaim) { p.Spec.VolumeName = "pv-1"; p.Status.Phase = "Bound" })
		pv := kubernetes.PersistentVolume{}
		pv.Spec.PersistentVolumeReclaimPolicy = "Retain"
		h.cl.pvs["pv-1"] = pv

		if o, err := h.rt.Remove(ctx, b); err != nil || o.Phase != runtime.PhaseAbsent {
			t.Fatalf("remove workload = %+v %v", o, err)
		}
		if o, err := h.res.Remove(ctx, svc.Handle); err != nil || !o.ConfirmedRemoved() {
			t.Fatalf("remove service = %+v %v", o, err)
		}
		o, err := h.res.Remove(ctx, ws)
		if err != nil || o.ConfirmedRemoved() || o.State != runtime.ResourceFailed || o.Reason != runtime.ReasonCleanupFailed {
			t.Fatalf("a retained volume must be reported, not removed: %+v %v", o, err)
		}
		if ev, _ := h.res.ReleaseEvidence(ctx, ws); !ev.ClaimGone || !ev.Retained || ev.Released() || !slices.Equal(ev.Volumes, []string{"pv-1"}) {
			t.Fatalf("evidence = %+v", ev)
		}

		// Nothing owned remains except the fence records and the retained volume;
		// the other tenant and the stray object are untouched.
		if n := h.cl.names(h.cl.pods); len(n) != 0 {
			t.Fatalf("pods remain: %v", n)
		}
		if n := h.cl.names(h.cl.services); len(n) != 0 {
			t.Fatalf("services remain: %v", n)
		}
		if n := h.cl.names(h.cl.jobs); !slices.Equal(n, []string{testNS + "/unrelated"}) {
			t.Fatalf("jobs = %v", n)
		}
		for _, n := range h.cl.names(h.cl.secrets) {
			if !strings.HasSuffix(n, "-rec") {
				t.Fatalf("a non-record Secret remains: %s", n)
			}
		}
		if n := h.cl.names(h.cl.pvcs); len(n) != 1 || !strings.HasSuffix(n[0], workspaceIdentB(h).base()) {
			t.Fatalf("pvcs = %v, want only tenant B's", n)
		}
		if o, err := h.res.Observe(ctx, wsB); err != nil || o.State != runtime.ResourceReady {
			t.Fatalf("tenant B's workspace = %+v %v", o, err)
		}
	})

	t.Run("the adapter imports no legacy execution, gate or workspace package", func(t *testing.T) {
		files, err := filepath.Glob("*.go")
		if err != nil || len(files) == 0 {
			t.Fatalf("no sources: %v", err)
		}
		forbidden := []string{
			"github.com/optimiware/kodama/internal/execution",
			"github.com/optimiware/kodama/internal/runtimeauth",
			"github.com/optimiware/kodama/internal/gate",
			"github.com/optimiware/kodama/internal/workspace",
			"github.com/optimiware/kodama/internal/runtime",
			"github.com/optimiware/kodama/internal/shell",
			"github.com/optimiware/kodama/internal/storage",
			"github.com/optimiware/kodama/internal/app",
			"github.com/optimiware/kodama/internal/pipeline",
			"github.com/optimiware/kodama/contracts",
		}
		allowed := map[string]bool{
			"github.com/optimiware/kodama/internal/domain":          true,
			"github.com/optimiware/kodama/internal/kernel/runtime":  true,
			"github.com/optimiware/kodama/internal/kube/kubernetes": true,
		}
		for _, f := range files {
			parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range parsed.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				if !strings.HasPrefix(p, "github.com/optimiware/kodama/") {
					continue
				}
				if !allowed[p] {
					t.Errorf("%s imports %s", f, p)
				}
				for _, bad := range forbidden {
					if p == bad || strings.HasPrefix(p, bad+"/") {
						t.Errorf("%s imports legacy package %s", f, p)
					}
				}
			}
		}
	})
}

func workspaceIdentB(h *harness) ident {
	s := h.workspaceSpec(scopeB, "run-9", 1)
	return resourceIdent(s.Scope, s.RunID, s.Kind, s.Name, s.Identity, "", "kubernetes")
}

func deepCopyJob(t *testing.T, j kubernetes.Job) kubernetes.Job {
	t.Helper()
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var out kubernetes.Job
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
