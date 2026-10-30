package kube_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

// sharedEnv is a run_shared attempt over a bound run workspace: the run-resource
// adapter ensured the claim, the attempt's committed record carries the launch.
type sharedEnv struct {
	*runEnv
	attempts *kubetest.Attempts
	reg2     *kubetest.Registrar
	rec      kube.AttemptRecord
	launch   execution.SharedLaunch
	ws       execution.ResourceObservation
	rt       *kube.Runtime
}

func builderRole(r execution.RoleShape) execution.RoleShape {
	r.CapabilitiesAdd = []string{"SETUID", "SETGID"}
	r.Seccomp, r.LSM, r.ProcMount = "kodama-rootless-build-v1", "kodama-rootless-build-v1", "unmasked"
	return r
}

func newSharedEnv(t *testing.T, role func(execution.RoleShape) execution.RoleShape, pre ...func(*runEnv)) *sharedEnv {
	t.Helper()
	body := tenantBody()
	if role != nil {
		body.Roles[execution.RoleStep] = role(body.Roles[execution.RoleStep])
	}
	re := newRunEnv(t, body)
	for _, p := range pre {
		p(re)
	}
	ws := re.ensure(re.specs.Workspace(t))
	roleDigest, err := body.Roles[execution.RoleStep].Digest()
	if err != nil {
		t.Fatal(err)
	}
	c := kubetest.Context(t, kubetest.ContextSpec{
		Tenant: re.specs.Scope.TenantID, Project: re.specs.Scope.ProjectID, Run: re.specs.Run, Step: "build", Attempt: "att_1",
		Image: kubetest.WorkloadImage, Access: runtimev1.AccessRead,
	})
	rec := kubetest.Record(t, re.policy, c)
	launch := execution.SharedLaunch{
		Policy: re.specs.Pin, Role: body.Roles[execution.RoleStep], RoleDigest: roleDigest, PodSecurity: body.PodSecurity,
		Workspace: ws.Handle, Network: body.Network, Egress: []string{}, EgressDigests: map[string]string{},
	}
	rec.Shared = &launch
	s := &sharedEnv{runEnv: re, attempts: kubetest.NewAttempts(rec), reg2: kubetest.NewRegistrar(), rec: rec, launch: launch, ws: ws}
	s.rt = s.runtime(t)
	return s
}

func (s *sharedEnv) runtime(t *testing.T) *kube.Runtime {
	t.Helper()
	x, err := kube.NewRuntime(kube.Dependencies{Policy: s.policy, Cluster: s.fake, Attempts: s.attempts, Registrar: s.reg2, Checkout: s.chk, OwnedPolicies: s.rr}, kube.Options{Clock: s.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func (s *sharedEnv) intent() execution.AttemptSpec {
	in := kubetest.Intent(s.rec)
	l := s.launch
	in.Shared = &l
	return in
}

func (s *sharedEnv) job() kubernetes.Job {
	j, _ := s.fake.JobOf(s.ns, s.rec.JobName)
	return j
}

func docOf(t *testing.T, j kubernetes.Job) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(j.Raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// Test_unit_kube_shared_attempt_template: a run_shared attempt Job (seams S1-S3)
// selects the v2 wire, mounts the bound claim at /workspace, keeps outputs,
// secrets, identity, scratch, HOME and the XDG runtime directory attempt-private,
// carries no checkout credential and takes its security shape exactly from the
// pinned step role. The attempt_private Job is unchanged.
func Test_unit_kube_shared_attempt_template(t *testing.T) {
	ctx := context.Background()
	s := newSharedEnv(t, builderRole)
	w, err := s.rt.Start(ctx, s.intent())
	if err != nil || w.UID == "" {
		t.Fatalf("start: %+v %v", w, err)
	}
	job := s.job()
	if job.Metadata.UID != w.UID || job.Metadata.Annotations[kube.AnnRunModel] != kube.ValueRunShared || job.Metadata.Annotations[kube.AnnSharedDigest] == "" {
		t.Fatalf("job metadata %+v", job.Metadata)
	}
	if job.Metadata.Annotations[kube.AnnFingerprint] != s.rec.SpecFingerprint {
		t.Fatal("the fingerprint is not the committed one")
	}
	pod := job.Spec.Template.Spec
	tpl := job.Spec.Template.Metadata.Labels
	key := kube.RunKey(s.rec.Scope, s.rec.RunID)
	if tpl[kube.LabelRunKey] != key || tpl[kube.LabelRole] != kube.RoleLabelStep || tpl[kube.LabelRunShared] != "true" ||
		tpl[kube.LabelWorkload] != kube.WorkloadHash(s.rec.Scope, s.rec.RunID, "build") || tpl[kube.LabelManagedBy] != kube.ValueManagedBy {
		t.Fatalf("pod labels %v", tpl)
	}
	if *job.Spec.BackoffLimit != 0 || pod.RestartPolicy != "Never" || *pod.AutomountServiceAccountToken || len(pod.Containers) != 1 || len(pod.InitContainers) != 1 {
		t.Fatalf("job shape %+v", pod)
	}
	c := pod.Containers[0]
	env := envOf(c)
	for k, v := range map[string]string{
		"KODAMA_RUNTIME_API": "v2", "KODAMA_WORKSPACE_DIR": "/workspace", "HOME": "/kodama/home", "TMPDIR": "/tmp", "XDG_RUNTIME_DIR": "/kodama/run",
	} {
		if env[k].Value != v {
			t.Fatalf("%s = %q, want %q", k, env[k].Value, v)
		}
	}
	for _, a := range c.Args {
		if strings.Contains(a, "checkout") || strings.Contains(a, "git-ca") {
			t.Fatalf("a run_shared attempt receives checkout flags: %v", c.Args)
		}
	}
	ws, ok := volumeOf(pod, "workspace")
	if !ok || ws.PersistentVolumeClaim == nil || ws.PersistentVolumeClaim.ClaimName != s.ws.Handle.Name || ws.EmptyDir != nil {
		t.Fatalf("workspace volume %+v", ws)
	}
	if m, ok := mountOf(c, "/workspace"); !ok || m.Name != "workspace" || m.ReadOnly {
		t.Fatalf("workspace mount %+v", m)
	}
	for _, name := range []string{"outputs", "results", "tmp", "secrets", "identity", "home", "run", "tools"} {
		v, ok := volumeOf(pod, name)
		if !ok || v.EmptyDir == nil || v.EmptyDir.SizeLimit == "" {
			t.Fatalf("%s is not an attempt-private bounded emptyDir: %+v", name, v)
		}
	}
	for _, p := range []string{"/kodama/home", "/kodama/run", "/tmp", "/kodama/outputs", "/kodama/secrets", "/kodama/identity"} {
		if _, ok := mountOf(c, p); !ok {
			t.Fatalf("no mount at %s", p)
		}
	}
	// No checkout credential anywhere: the bootstrap Secret holds the two keys of an
	// attempt without a workspace, though the step reads source.
	sec, _ := s.fake.SecretOf(s.ns, kube.SecretName(s.rec.JobName))
	if len(sec.Data) != 2 || len(s.chk.Reqs) != 0 {
		t.Fatalf("bootstrap keys %d, checkout requests %d", len(sec.Data), len(s.chk.Reqs))
	}
	// S3: exactly the role.
	if pod.HostUsers == nil || *pod.HostUsers {
		t.Fatal("hostUsers must be false")
	}
	ps := pod.SecurityContext
	if ps.RunAsNonRoot == nil || !*ps.RunAsNonRoot || *ps.RunAsUser != 65532 || *ps.RunAsGroup != 65532 || *ps.FSGroup != 65532 {
		t.Fatalf("pod security %+v", ps)
	}
	sc := c.SecurityContext
	if sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 2 || sc.Capabilities.Add[0] != "SETUID" || sc.Capabilities.Add[1] != "SETGID" ||
		sc.AllowPrivilegeEscalation == nil || !*sc.AllowPrivilegeEscalation || *sc.Privileged || !*sc.RunAsNonRoot || *sc.RunAsUser != 65532 {
		t.Fatalf("container security %+v", sc)
	}
	if sc.SeccompProfile.Type != "Localhost" || sc.SeccompProfile.LocalhostProfile != "kodama/kodama-rootless-build-v1.json" ||
		sc.AppArmorProfile.Type != "Localhost" || sc.AppArmorProfile.LocalhostProfile != "kodama-rootless-build-v1" || sc.ProcMount != "Unmasked" {
		t.Fatalf("profiles %+v %+v procMount %q", sc.SeccompProfile, sc.AppArmorProfile, sc.ProcMount)
	}
	if init := pod.InitContainers[0]; *init.SecurityContext.AllowPrivilegeEscalation || len(init.SecurityContext.Capabilities.Add) != 0 || init.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		t.Fatalf("the injector must keep its restricted shape: %+v", init.SecurityContext)
	}
	// A redriven Start adopts the same Job.
	if w2, err := s.runtime(t).Start(ctx, s.intent()); err != nil || w2.UID != w.UID || s.fake.Calls("create job") != 1 {
		t.Fatalf("redrive %+v %v", w2, err)
	}
	// Observe, Stop and Remove work on the shared shape (expectation rebuilt from the committed launch).
	if obs, err := s.rt.Observe(ctx, w); err != nil || obs.WorkloadUID != w.UID {
		t.Fatalf("observe %+v %v", obs, err)
	}
	if ev, err := s.rt.Stop(ctx, w); err != nil {
		t.Fatal(err)
	} else if !ev.Stopped {
		if ev, err = s.rt.Stop(ctx, w); err != nil || !ev.Stopped {
			t.Fatalf("stop %+v %v", ev, err)
		}
	}
	if _, ok := s.fake.JobOf(s.ns, s.rec.JobName); ok {
		t.Fatal("the Job was not deleted")
	}

	// A role without additions: no escalation, no capabilities, default profiles.
	plain := newSharedEnv(t, nil)
	if _, err := plain.rt.Start(ctx, plain.intent()); err != nil {
		t.Fatal(err)
	}
	pc := plain.job().Spec.Template.Spec.Containers[0].SecurityContext
	if pc.AllowPrivilegeEscalation == nil || !*pc.AllowPrivilegeEscalation { // TestPolicy's step role adds SETUID/SETGID
		t.Fatalf("test policy step role: %+v", pc)
	}
	noCaps := newSharedEnv(t, func(r execution.RoleShape) execution.RoleShape { r.CapabilitiesAdd = []string{}; return r })
	if _, err := noCaps.rt.Start(ctx, noCaps.intent()); err != nil {
		t.Fatal(err)
	}
	nc := noCaps.job().Spec.Template.Spec.Containers[0].SecurityContext
	if *nc.AllowPrivilegeEscalation || len(nc.Capabilities.Add) != 0 || nc.SeccompProfile.Type != "RuntimeDefault" || nc.AppArmorProfile.Type != "RuntimeDefault" || nc.ProcMount != "" {
		t.Fatalf("a role without additions: %+v", nc)
	}
}

// Test_unit_kube_attempt_private_job_unchanged: an attempt without a launch gets
// the Job it always did: emptyDir workspace, HOME=/tmp, checkout material, the
// restricted baseline, no user namespace and no run labels.
func Test_unit_kube_attempt_private_job_unchanged(t *testing.T) {
	f := newFixture(t)
	spec := f.exp.Job.Spec.Template.Spec
	if spec.HostUsers != nil {
		t.Fatal("hostUsers set on an attempt_private Job")
	}
	ws, _ := volumeOf(spec, "workspace")
	if ws.EmptyDir == nil || ws.PersistentVolumeClaim != nil {
		t.Fatalf("workspace %+v", ws)
	}
	if _, ok := volumeOf(spec, "home"); ok {
		t.Fatal("a home volume was added")
	}
	env := envOf(spec.Containers[0])
	if env["HOME"].Value != "/tmp" || env["KODAMA_RUNTIME_API"].Value != "" || env["XDG_RUNTIME_DIR"].Value != "" {
		t.Fatalf("env %+v", spec.Containers[0].Env)
	}
	sc := spec.Containers[0].SecurityContext
	if *sc.AllowPrivilegeEscalation || len(sc.Capabilities.Add) != 0 || sc.AppArmorProfile != nil || sc.ProcMount != "" || *sc.ReadOnlyRootFilesystem {
		t.Fatalf("security %+v", sc)
	}
	if spec.SecurityContext.SeccompProfile == nil || spec.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		t.Fatal("pod seccomp")
	}
	if f.exp.Job.Metadata.Annotations[kube.AnnRunModel] != "" || f.exp.Job.Metadata.Labels[kube.LabelRunKey] != "" {
		t.Fatal("run_shared marks on an attempt_private Job")
	}
	raw, _ := json.Marshal(f.exp.Job)
	for _, forbidden := range []string{"hostUsers", "appArmorProfile", "persistentVolumeClaim", "procMount", "activeDeadlineSeconds", "localhostProfile"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("the attempt_private Job document mentions %s", forbidden)
		}
	}
}

// Test_unit_kube_shared_attempt_adoption_verifies_security: an existing Job is
// adopted only if every controlled field equals the run_shared shape: the security
// fields, the volumes and the claim are compared, not just labels and
// fingerprints; a claim of another UID, a foreign or terminating claim and a
// stopped workspace are never mounted.
func Test_unit_kube_shared_attempt_adoption_verifies_security(t *testing.T) {
	ctx := context.Background()
	edit := func(f func(spec map[string]any)) func(map[string]any) {
		return func(d map[string]any) {
			f(d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any))
		}
	}
	container := func(spec map[string]any) map[string]any { return spec["containers"].([]any)[0].(map[string]any) }
	sec := func(spec map[string]any) map[string]any { return container(spec)["securityContext"].(map[string]any) }
	mutations := map[string]func(map[string]any){
		"host users": edit(func(s map[string]any) { s["hostUsers"] = true }),
		"added capability": edit(func(s map[string]any) {
			sec(s)["capabilities"].(map[string]any)["add"] = []any{"SETUID", "SETGID", "SYS_ADMIN"}
		}),
		"escalation flipped": edit(func(s map[string]any) { sec(s)["allowPrivilegeEscalation"] = false }),
		"privileged":         edit(func(s map[string]any) { sec(s)["privileged"] = true }),
		"seccomp unconfined": edit(func(s map[string]any) { sec(s)["seccompProfile"] = map[string]any{"type": "Unconfined"} }),
		"apparmor unconfined": edit(func(s map[string]any) {
			sec(s)["appArmorProfile"] = map[string]any{"type": "Unconfined"}
		}),
		"proc mount dropped": edit(func(s map[string]any) { delete(sec(s), "procMount") }),
		"run as root":        edit(func(s map[string]any) { sec(s)["runAsUser"] = 0 }),
		"other claim": edit(func(s map[string]any) {
			for _, v := range s["volumes"].([]any) {
				if v.(map[string]any)["name"] == "workspace" {
					v.(map[string]any)["persistentVolumeClaim"] = map[string]any{"claimName": "kr-someone-elses"}
				}
			}
		}),
		"private workspace": edit(func(s map[string]any) {
			for _, v := range s["volumes"].([]any) {
				if v.(map[string]any)["name"] == "workspace" {
					delete(v.(map[string]any), "persistentVolumeClaim")
					v.(map[string]any)["emptyDir"] = map[string]any{}
				}
			}
		}),
		"host path": edit(func(s map[string]any) {
			s["volumes"] = append(s["volumes"].([]any), map[string]any{"name": "h", "hostPath": map[string]any{"path": "/"}})
		}),
		"extra container": edit(func(s map[string]any) {
			s["containers"] = append(s["containers"].([]any), map[string]any{"name": "x", "image": "evil"})
		}),
		"host network": edit(func(s map[string]any) { s["hostNetwork"] = true }),
		"wire version": edit(func(s map[string]any) {
			container(s)["env"] = append(container(s)["env"].([]any), map[string]any{"name": "KODAMA_RUNTIME_API", "value": "v1"})
		}),
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s := newSharedEnv(t, builderRole)
			if _, err := s.rt.Start(ctx, s.intent()); err != nil {
				t.Fatal(err)
			}
			s.fake.MutateJobRaw(s.ns, s.rec.JobName, mutate)
			rt := s.runtime(t) // a restarted controller
			_, err := rt.Start(ctx, s.intent())
			requireIs(t, err, execution.ErrConflict)
			requireIs(t, err, kube.ErrForeignWorkload)
			if _, err := rt.Observe(ctx, s.intent().Workload("")); err == nil {
				t.Fatal("observed a Job that is not the attempt's")
			}
		})
	}

	t.Run("a claim waiting for its first consumer is mounted", func(t *testing.T) {
		s := newSharedEnv(t, nil, wffc)
		if pvc, _ := s.fake.PVCOf(s.ns, s.ws.Handle.Name); pvc.Status.Phase != "Pending" {
			t.Fatalf("the claim is expected to wait: %+v", pvc.Status)
		}
		w, err := s.rt.Start(ctx, s.intent())
		if err != nil || w.UID == "" {
			t.Fatalf("the first attempt is the first consumer: %+v %v", w, err)
		}
		// A restart adopts the same Job; the claim binding meanwhile changes nothing.
		s.fake.BindClaim(s.ns, s.ws.Handle.Name)
		if w2, err := s.runtime(t).Start(ctx, s.intent()); err != nil || w2.UID != w.UID {
			t.Fatalf("adoption after the claim bound: %+v %v", w2, err)
		}
	})
	t.Run("claim of another UID", func(t *testing.T) {
		s := newSharedEnv(t, nil)
		// The claim is replaced by another claim of the same name (a new UID): nothing
		// is created against it.
		s.fake.DropPVC(s.ns, s.ws.Handle.Name)
		old, _ := s.fake.PVCOf(s.ns, s.ws.Handle.Name)
		_ = old
		pvc := kubernetes.PersistentVolumeClaim{Metadata: kubernetes.ObjectMeta{Name: s.ws.Handle.Name, Labels: map[string]string{kube.LabelResourceOwner: kube.ValueResourceOwner}}}
		s.fake.PutPVC(s.ns, pvc)
		_, err := s.rt.Start(ctx, s.intent())
		requireIs(t, err, execution.ErrConflict)
		if s.fake.Calls("create job") != 0 || s.fake.Calls("create secret") > 1 || s.reg2.Calls != 0 {
			t.Fatalf("effects against a foreign claim: jobs %d secrets %d registrations %d", s.fake.Calls("create job"), s.fake.Calls("create secret"), s.reg2.Calls)
		}
	})
	t.Run("same name and labels but another UID", func(t *testing.T) {
		s := newSharedEnv(t, nil)
		claim, _ := s.fake.PVCOf(s.ns, s.ws.Handle.Name)
		s.fake.DropPVC(s.ns, s.ws.Handle.Name)
		claim.Metadata.UID = ""
		s.fake.PutPVC(s.ns, claim) // every label and annotation copied, a new UID
		_, err := s.rt.Start(ctx, s.intent())
		requireIs(t, err, execution.ErrConflict)
		requireIs(t, err, kube.ErrForeignWorkload)
		if s.fake.Calls("create job") != 0 {
			t.Fatal("a Job was created against a claim of another UID")
		}
	})
	t.Run("adoption rechecks the claim", func(t *testing.T) {
		s := newSharedEnv(t, nil)
		if _, err := s.rt.Start(ctx, s.intent()); err != nil {
			t.Fatal(err)
		}
		claim, _ := s.fake.PVCOf(s.ns, s.ws.Handle.Name)
		s.fake.DropPVC(s.ns, s.ws.Handle.Name)
		claim.Metadata.UID = ""
		s.fake.PutPVC(s.ns, claim)
		if _, err := s.runtime(t).Start(ctx, s.intent()); err == nil {
			t.Fatal("an existing Job was adopted against a replaced claim")
		}
	})
	t.Run("terminating claim", func(t *testing.T) {
		s := newSharedEnv(t, nil)
		s.fake.MutateRaw("pvc", s.ns, s.ws.Handle.Name, func(d map[string]any) {
			d["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-29T12:00:00Z"
		})
		_, err := s.rt.Start(ctx, s.intent())
		requireIs(t, err, execution.ErrConflict)
	})
	t.Run("stopped workspace", func(t *testing.T) {
		s := newSharedEnv(t, nil)
		s.stop(s.ws.Handle)
		_, err := s.rt.Start(ctx, s.intent())
		requireIs(t, err, execution.ErrConflict)
		if s.fake.Calls("create job") != 0 {
			t.Fatal("a Job was created for a stopped workspace")
		}
	})
	t.Run("missing claim", func(t *testing.T) {
		s := newSharedEnv(t, nil)
		s.fake.DropPVC(s.ns, s.ws.Handle.Name)
		_, err := s.rt.Start(ctx, s.intent())
		requireIs(t, err, execution.ErrConflict)
	})
	t.Run("claim unreadable is an outage", func(t *testing.T) {
		s := newSharedEnv(t, nil)
		s.fake.Fault = func(op string, n int) *kubetestFault {
			if op == "get pvc" {
				return &kubetestFault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnavailable}}
			}
			return nil
		}
		_, err := s.rt.Start(ctx, s.intent())
		if err == nil || strings.Contains(err.Error(), "conflict") || s.fake.Calls("create job") != 0 {
			t.Fatalf("an unreadable claim must be retried, not refused as a conflict: %v", err)
		}
	})
	t.Run("launch other than the committed one", func(t *testing.T) {
		s := newSharedEnv(t, nil)
		in := s.intent()
		l := *in.Shared
		l.Network.Isolation = execution.IsolationRun
		in.Shared = &l
		_, err := s.rt.Start(ctx, in)
		requireIs(t, err, execution.ErrConflict)
		in.Shared = nil
		_, err = s.rt.Start(ctx, in)
		requireIs(t, err, execution.ErrConflict)
	})
	t.Run("cluster without the run surface", func(t *testing.T) {
		s := newSharedEnv(t, nil)
		x, err := kube.NewRuntime(kube.Dependencies{Policy: s.policy, Cluster: struct{ kube.Cluster }{s.fake}, Attempts: s.attempts, Registrar: s.reg2}, kube.Options{Clock: s.clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := x.Start(ctx, s.intent()); err == nil || s.fake.Calls("create job") != 0 {
			t.Fatalf("started a run_shared attempt without a claim check: %v", err)
		}
	})
	t.Run("an attempt_private Job is not a run_shared one", func(t *testing.T) {
		s := newSharedEnv(t, nil)
		legacy := s.rec
		legacy.Shared = nil
		exp, err := s.policy.Expect(legacy)
		if err != nil {
			t.Fatal(err)
		}
		s.fake.PutJob(s.ns, exp.Job)
		if _, err := s.rt.Start(ctx, s.intent()); err == nil {
			t.Fatal("a Job of the attempt_private shape was adopted for a run_shared attempt")
		}
	})
}
