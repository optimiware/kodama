package kube_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

func envOf(c kubernetes.Container) map[string]kubernetes.EnvVar {
	out := map[string]kubernetes.EnvVar{}
	for _, v := range c.Env {
		out[v.Name] = v
	}
	return out
}

func mountOf(c kubernetes.Container, path string) (kubernetes.VolumeMount, bool) {
	for _, m := range c.VolumeMounts {
		if m.MountPath == path {
			return m, true
		}
	}
	return kubernetes.VolumeMount{}, false
}

func volumeOf(p kubernetes.PodSpec, name string) (kubernetes.Volume, bool) {
	for _, v := range p.Volumes {
		if v.Name == name {
			return v, true
		}
	}
	return kubernetes.Volume{}, false
}

// Test_unit_kube_preparation_job_lifecycle: the run's one preparation is a Job
// (backoffLimit 0, a Pod that never restarts) of the policy's digest-pinned
// preparation image running `kodama prepare` through the injected entrypoint, with
// the claim at /workspace and a read-only checkout credential. Its admission
// credential is stored in the record before it is registered and registered
// before the Job exists; a lost registration response repeats the same credential.
func Test_unit_kube_preparation_job_lifecycle(t *testing.T) {
	ctx := context.Background()
	e := newRunEnv(t, tenantBody())
	ws := e.ensure(e.specs.Workspace(t))
	spec := e.specs.Preparation(t)

	// A lost registration response: the credential was stored and registered, the
	// answer lost. Nothing was created yet; the retry registers the same credential.
	e.reg.FailAfter = func(n int) error {
		if n == 1 {
			return errors.New("lost registration response")
		}
		return nil
	}
	if err := e.ensureErr(spec); err == nil {
		t.Fatal("a failed registration must not create the Job")
	}
	if jobs, _ := jobsAndPods(e); len(jobs) != 0 {
		t.Fatalf("a Job was created before its credential was registered: %v", jobs)
	}
	o := e.ensure(spec)
	if len(e.reg.Calls) != 2 || e.reg.Calls[0].Credential.Digest() != e.reg.Calls[1].Credential.Digest() {
		t.Fatalf("the credential was not reused: %d registrations", len(e.reg.Calls))
	}
	if e.fake.Calls("create secret") != 2 { // the workspace record and the preparation record
		t.Fatalf("secrets created: %d", e.fake.Calls("create secret"))
	}
	if cmd := e.reg.Calls[1]; cmd.Key != execution.KeyPreparation || cmd.Scope != spec.Scope || cmd.RunID != spec.RunID || cmd.SpecFingerprint != o.Fingerprint || cmd.ExpiresAt.IsZero() {
		t.Fatalf("registration %+v", cmd)
	}
	// Registered before the Job was created.
	regAt, jobAt := -1, -1
	for i, op := range e.fake.Log {
		if op == "create job" {
			jobAt = i
		}
		if op == "update secret" && regAt < 0 {
			regAt = i
		}
	}
	if regAt < 0 || jobAt < 0 || regAt > jobAt {
		t.Fatalf("order %v", e.fake.Log)
	}

	job, ok := e.fake.JobOf(e.ns, spec.Name)
	if !ok || job.Metadata.UID != o.Handle.UID {
		t.Fatalf("job %+v", job.Metadata)
	}
	js := job.Spec
	if *js.BackoffLimit != 0 || *js.Parallelism != 1 || *js.Completions != 1 || js.ActiveDeadlineSeconds == nil || *js.ActiveDeadlineSeconds != 600 {
		t.Fatalf("job spec %+v", js)
	}
	pod := js.Template.Spec
	if pod.RestartPolicy != "Never" || *pod.AutomountServiceAccountToken || pod.HostUsers == nil || *pod.HostUsers || pod.ServiceAccountName != "t021-runner" ||
		pod.HostNetwork || pod.HostPID || pod.HostIPC {
		t.Fatalf("pod spec %+v", pod)
	}
	if len(pod.Containers) != 1 || len(pod.InitContainers) != 1 {
		t.Fatalf("containers %d init %d", len(pod.Containers), len(pod.InitContainers))
	}
	c := pod.Containers[0]
	if c.Image != spec.Preparation.Image || c.Image != e.body.Roles[execution.RolePreparation].Image || c.Command[0] != "/kodama/bin/kodama" || len(c.Args) != 1 || c.Args[0] != "prepare" {
		t.Fatalf("container %+v", c)
	}
	env := envOf(c)
	for k, want := range map[string]string{
		"KODAMA_SERVER_URL": "https://192.0.2.10:8443", "KODAMA_CA_FILE": "/kodama/bootstrap/ca.crt", "KODAMA_RUN_ID": string(spec.RunID),
		"KODAMA_RESOURCE_KEY": execution.KeyPreparation, "KODAMA_ADMISSION_CREDENTIAL_FILE": "/kodama/bootstrap/admission",
		"KODAMA_WORKLOAD_NAME": spec.Name, "KODAMA_WORKSPACE_DIR": "/workspace", "KODAMA_CHECKOUT_CREDENTIAL_FILE": "/kodama/bootstrap/checkout-credential",
	} {
		if env[k].Value != want {
			t.Fatalf("%s = %q, want %q", k, env[k].Value, want)
		}
	}
	for _, k := range []string{"KODAMA_WORKLOAD_UID", "KODAMA_POD_NAME", "KODAMA_POD_UID"} {
		if env[k].ValueFrom == nil || env[k].ValueFrom.FieldRef == nil {
			t.Fatalf("%s is not a Downward API value", k)
		}
	}
	if m, ok := mountOf(c, "/workspace"); !ok || m.ReadOnly {
		t.Fatalf("workspace mount %+v", m)
	}
	vol, _ := volumeOf(pod, "workspace")
	if vol.PersistentVolumeClaim == nil || vol.PersistentVolumeClaim.ClaimName != ws.Handle.Name {
		t.Fatalf("workspace volume %+v", vol)
	}
	boot, _ := volumeOf(pod, "bootstrap")
	for _, it := range boot.Secret.Items {
		if it.Key == kube.KeyResourceSpec {
			t.Fatal("the stored spec is mounted into the workload")
		}
	}
	if len(boot.Secret.Items) != 4 || boot.Secret.SecretName != spec.Name+"-rec" {
		t.Fatalf("bootstrap %+v", boot.Secret)
	}
	if c.SecurityContext.RunAsNonRoot == nil || !*c.SecurityContext.RunAsNonRoot || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation ||
		c.SecurityContext.Capabilities.Drop[0] != "ALL" || len(c.SecurityContext.Capabilities.Add) != 0 {
		t.Fatalf("preparation security %+v", c.SecurityContext)
	}
	// The read-only checkout credential came through the existing port, for this
	// resource and no attempt, and sits only in the record Secret.
	if len(e.chk.Reqs) != 1 || e.chk.Reqs[0].StepID != execution.KeyPreparation || e.chk.Reqs[0].Access != execution.AccessRead ||
		e.chk.Reqs[0].AttemptID != "" || e.chk.Reqs[0].WorkspaceID != "" || e.chk.Reqs[0].RepositoryURL != spec.Preparation.RepositoryURL {
		t.Fatalf("checkout requests %+v", e.chk.Reqs)
	}
	sec, _ := e.fake.SecretOf(e.ns, spec.Name+"-rec")
	if string(sec.Data[kube.KeyCheckoutCredential]) != "ro-user:ro-secret-value" || len(sec.Data[kube.KeyAdmission]) == 0 || sec.Immutable == nil || !*sec.Immutable {
		t.Fatalf("record data %v", sec.Data)
	}
	rawJob, _ := jsonMarshal(job)
	if strings.Contains(string(rawJob), "ro-secret-value") || strings.Contains(string(rawJob), string(sec.Data[kube.KeyAdmission])) {
		t.Fatal("a credential appears in the Job document")
	}

	// Observation: running is pending, completion is ready, failure is failed.
	h := o.Handle
	if got := e.observe(h); got.Phase != execution.PhasePending || got.Fingerprint != o.Fingerprint {
		t.Fatalf("pending %+v", got)
	}
	e.fake.SetJobStatus(e.ns, spec.Name, func(s *kubernetes.JobStatus) {
		s.Conditions = []kubernetes.JobCondition{{Type: "Complete", Status: "True"}}
	})
	if got := e.observe(h); got.Phase != execution.PhaseReady || got.ReadyAt == nil {
		t.Fatalf("completed %+v", got)
	}
	e.fake.SetJobStatus(e.ns, spec.Name, func(s *kubernetes.JobStatus) {
		s.Conditions = []kubernetes.JobCondition{{Type: "Failed", Status: "True"}}
	})
	if got := e.observe(h); got.Phase != execution.PhaseFailed || got.Reason != execution.ReasonPreparationFailed {
		t.Fatalf("failed %+v", got)
	}

	// Logs: bounded, redacted, never for the workspace.
	e.fake.AddPod(e.ns, job, func(p *kubernetes.Pod) {
		p.Metadata.Labels[kube.LabelResourceOwner] = kube.ValueResourceOwner
		p.Metadata.Labels[kube.LabelRunKey] = kube.RunKey(spec.Scope, spec.RunID)
		p.Metadata.Labels[kube.LabelResourceKind] = string(execution.KindPreparation)
	})
	rr := e.build(func(d *kube.RunResourceDependencies, _ *kube.RunResourceOptions) {
		d.Logs = &podLogsText{text: "cloning with ro-user:ro-secret-value and " + string(sec.Data[kube.KeyAdmission]) + " done\n"}
	})
	logs, err := rr.Logs(ctx, h, execution.LogBound{MaxBytes: 1 << 16})
	if err != nil || strings.Contains(logs.Content, "ro-secret-value") || strings.Contains(logs.Content, string(sec.Data[kube.KeyAdmission])) || !strings.Contains(logs.Content, "done") {
		t.Fatalf("logs %+v %v", logs, err)
	}
	if got, _ := rr.Logs(ctx, h, execution.LogBound{MaxBytes: 8}); !got.Truncated || len(got.Content) != 8 {
		t.Fatalf("not bounded: %+v", got)
	}
	if _, err := rr.Logs(ctx, ws.Handle, execution.LogBound{}); !errors.Is(err, execution.ErrDenied) {
		t.Fatalf("workspace logs: %v", err)
	}

	// Stop deletes the Job by UID, reports the launch evidence its Pod gave and
	// fences the resource: a stopped preparation is never created again.
	e.fake.EditPod(e.ns, e.fake.Names("pod", e.ns)[0], func(p *kubernetes.Pod) {
		yes := true
		p.Status.Phase = "Succeeded"
		p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.PrimaryContainer, Started: &yes, State: kubernetes.ContainerState{Terminated: &kubernetes.ContainerTerminated{}}}}
	})
	ev := e.stop(h)
	if !ev.Stopped || ev.Launch != execution.LaunchStarted {
		t.Fatalf("stop %+v", ev)
	}
	if jobs, pods := jobsAndPods(e); len(jobs) != 0 || len(pods) != 0 {
		t.Fatalf("left jobs %v pods %v", jobs, pods)
	}
	requireIs(t, e.ensureErr(spec), execution.ErrResourceFenced)
	if jobs, _ := jobsAndPods(e); len(jobs) != 0 || e.fake.Calls("create job") != 1 {
		t.Fatal("a stopped preparation was created again")
	}
	if got := e.remove(h); !got.Removed {
		t.Fatalf("remove %+v", got)
	}
	if _, ok := e.fake.SecretOf(e.ns, spec.Name+"-rec"); ok {
		t.Fatal("the credential Secret outlived the resource")
	}
}

type podLogsText struct{ text string }

func (l *podLogsText) PodLog(context.Context, string, string, string) ([]byte, error) {
	return []byte(l.text), nil
}

// Test_unit_kube_preparation_refusals: no Job is created without a checkout
// credential, a registrar, the pinned image or a live workspace.
func Test_unit_kube_preparation_refusals(t *testing.T) {
	t.Run("checkout denied", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		e.ensure(e.specs.Workspace(t))
		e.chk.Err = kube.ErrCheckoutDenied
		err := e.ensureErr(e.specs.Preparation(t))
		requireIs(t, err, kube.ErrCheckoutDenied)
		if jobs, _ := jobsAndPods(e); len(jobs) != 0 || len(e.reg.Calls) != 0 || len(e.fake.Names("secret", e.ns)) != 1 {
			t.Fatalf("effects after a refusal: jobs %v registrations %d", jobs, len(e.reg.Calls))
		}
	})
	t.Run("provider answers for another repository", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		e.ensure(e.specs.Workspace(t))
		e.chk.Wrong = func(m *kube.CheckoutMaterial) { m.RepositoryURL = "https://git.example.invalid/other.git" }
		requireIs(t, e.ensureErr(e.specs.Preparation(t)), kube.ErrCheckoutDenied)
	})
	t.Run("no workspace", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		if err := e.ensureErr(e.specs.Preparation(t)); err == nil {
			t.Fatal("the preparation started without its workspace")
		}
		if jobs, _ := jobsAndPods(e); len(jobs) != 0 {
			t.Fatal("job created")
		}
	})
	t.Run("stopped workspace", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		ws := e.ensure(e.specs.Workspace(t))
		e.stop(ws.Handle)
		requireIs(t, e.ensureErr(e.specs.Preparation(t)), execution.ErrResourceFenced)
	})
	t.Run("image is not the policy's", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		e.ensure(e.specs.Workspace(t))
		spec := e.specs.Preparation(t)
		spec.Preparation.Image = "registry.example.invalid/other@sha256:" + strings.Repeat("d", 64)
		if err := e.ensureErr(spec); err == nil || !errors.Is(err, kube.ErrPolicy) {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("revoked registration", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		e.ensure(e.specs.Workspace(t))
		e.reg.Revoked = true
		requireIs(t, e.ensureErr(e.specs.Preparation(t)), execution.ErrResourceFenced)
		if jobs, _ := jobsAndPods(e); len(jobs) != 0 {
			t.Fatal("job created for a revoked resource")
		}
	})
	_ = kubetest.RunID
}

// Test_unit_kube_service_pod_and_endpoint: a service is one Pod that never
// restarts plus a ClusterIP Service named by the run-unique endpoint host and an
// ingress policy for its ports: the declared image and argv (or the supervisor
// when secrets are bound), bounded ephemeral storage and /dev/shm, the translated
// readiness probe observed through the Pod's Ready condition, and the pinned
// service role. It never mounts the workspace.
func Test_unit_kube_service_pod_and_endpoint(t *testing.T) {
	e := newRunEnv(t, tenantBody())
	spec := e.specs.Service(t, "db", false)
	o := e.ensure(spec)
	if o.Phase != execution.PhasePending || o.Handle.UID == "" || o.Handle.Key != "service.db" {
		t.Fatalf("observation %+v", o)
	}
	pod, ok := e.fake.PodOf(e.ns, spec.Name)
	if !ok || pod.Metadata.UID != o.Handle.UID {
		t.Fatalf("pod %+v", pod.Metadata)
	}
	ps := pod.Spec
	if ps.RestartPolicy != "Never" || len(ps.InitContainers) != 0 || len(ps.Containers) != 1 || ps.HostUsers == nil || *ps.HostUsers || *ps.AutomountServiceAccountToken {
		t.Fatalf("pod spec %+v", ps)
	}
	c := ps.Containers[0]
	if c.Image != kubetest.ServiceImage || len(c.Command) != 0 || len(c.Args) != 0 {
		t.Fatalf("an image-default service must keep its argv: %+v", c)
	}
	if len(c.Ports) != 1 || c.Ports[0].Name != "sql" || c.Ports[0].ContainerPort != 5432 || c.Ports[0].Protocol != "TCP" {
		t.Fatalf("ports %+v", c.Ports)
	}
	if p := c.ReadinessProbe; p == nil || p.TCPSocket == nil || p.TCPSocket.Port != 5432 || p.PeriodSeconds != 2 || p.TimeoutSeconds != 5 || p.FailureThreshold != 1 {
		t.Fatalf("probe %+v", c.ReadinessProbe)
	}
	if got := c.Resources.Limits; got["cpu"] != "500m" || got["memory"] != "512Mi" || got["ephemeral-storage"] != "1280Mi" {
		t.Fatalf("limits %v", got)
	}
	shm, ok := volumeOf(ps, "shm")
	if !ok || shm.EmptyDir == nil || shm.EmptyDir.Medium != "Memory" || shm.EmptyDir.SizeLimit != "64Mi" {
		t.Fatalf("shm %+v", shm)
	}
	if m, ok := mountOf(c, "/dev/shm"); !ok || m.Name != "shm" {
		t.Fatal("no /dev/shm mount")
	}
	data, _ := volumeOf(ps, "data-0")
	if data.EmptyDir == nil || data.EmptyDir.SizeLimit != "1024Mi" || data.EmptyDir.Medium != "" {
		t.Fatalf("storage %+v", data)
	}
	for _, v := range ps.Volumes {
		if v.PersistentVolumeClaim != nil || v.Secret != nil {
			t.Fatalf("a service mounts a claim or a Secret: %+v", v)
		}
	}
	if _, ok := mountOf(c, "/workspace"); ok {
		t.Fatal("a service mounts /workspace")
	}
	sc := c.SecurityContext
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || len(sc.Capabilities.Add) != 0 || sc.SeccompProfile.Type != "RuntimeDefault" ||
		sc.AppArmorProfile.Type != "RuntimeDefault" || sc.ProcMount != "" || *sc.Privileged {
		t.Fatalf("service security %+v", sc)
	}
	if pod.Metadata.Labels[kube.LabelRole] != kube.RoleLabelService || pod.Metadata.Labels[kube.LabelManagedBy] != "" || pod.Metadata.Labels[kube.LabelRunShared] != "true" {
		t.Fatalf("labels %v", pod.Metadata.Labels)
	}
	if len(e.reg.Calls) != 0 {
		t.Fatal("a service without secret bindings registered a credential")
	}
	if sec, _ := e.fake.SecretOf(e.ns, spec.Name+"-rec"); len(sec.Data[kube.KeyAdmission]) != 0 {
		t.Fatal("a service without secret bindings holds a credential")
	}

	host := execution.EndpointHost(spec.Scope, spec.RunID, "db")
	svc, ok := e.fake.ServiceOf(e.ns, host)
	if !ok || svc.Spec.Type != "ClusterIP" || svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" || len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 5432 ||
		svc.Spec.Selector[kube.LabelRunKey] != kube.RunKey(spec.Scope, spec.RunID) || svc.Spec.Selector[kube.LabelRole] != kube.RoleLabelService {
		t.Fatalf("service %+v", svc)
	}
	pols := e.fake.PoliciesWith(e.ns, kube.LabelResourceKind, "service")
	if len(pols) != 1 || pols[0] != spec.Name+"-in" {
		t.Fatalf("ingress policies %v", pols)
	}

	// Observation: ready only with the Pod ready and the endpoint objects present.
	h := o.Handle
	e.fake.Ready(e.ns, spec.Name)
	if got := e.observe(h); got.Phase != execution.PhaseReady || got.ProbeKind != "tcp" || got.ReadyAt == nil {
		t.Fatalf("ready %+v", got)
	}
	if err := e.fake.DeleteService(context.Background(), e.ns, host, kubernetes.DeleteOptions{UID: svc.Metadata.UID}); err != nil {
		t.Fatal(err)
	}
	if got := e.observe(h); got.Phase != execution.PhasePending {
		t.Fatalf("ready without its endpoint: %+v", got)
	}
	// Observe never creates: the Service is still gone.
	if _, ok := e.fake.ServiceOf(e.ns, host); ok {
		t.Fatal("observe recreated the Service")
	}
	// A repeated Ensure repairs it (idempotent; the Pod is adopted, not recreated).
	if again := e.ensure(spec); again.Handle.UID != h.UID || e.fake.Calls("create pod") != 1 {
		t.Fatalf("ensure %+v", again)
	}
	if _, ok := e.fake.ServiceOf(e.ns, host); !ok {
		t.Fatal("the endpoint was not repaired")
	}
	// A service that exits, whatever its exit code, is lost.
	e.fake.EditPod(e.ns, spec.Name, func(p *kubernetes.Pod) {
		p.Status.Phase = "Succeeded"
		p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: kube.PrimaryContainer, State: kubernetes.ContainerState{Terminated: &kubernetes.ContainerTerminated{ExitCode: 0}}}}
	})
	if got := e.observe(h); got.Phase != execution.PhaseFailed || got.Reason != execution.ReasonServiceLost || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("exited %+v", got)
	}

	// Stop: admission to the endpoint first, then the Pod; a Pod that terminates
	// gracefully keeps the resource not stopped until it is gone.
	e.fake.HoldPods = true
	if ev := e.stop(h); ev.Stopped {
		t.Fatalf("stopped while the Pod is terminating: %+v", ev)
	}
	if _, ok := e.fake.ServiceOf(e.ns, host); ok {
		t.Fatal("the endpoint outlived the stop")
	}
	if pols := e.fake.PoliciesWith(e.ns, kube.LabelResourceKind, "service"); len(pols) != 0 {
		t.Fatalf("ingress policy outlived the stop: %v", pols)
	}
	e.fake.ReleasePods()
	if ev := e.stop(h); !ev.Stopped {
		t.Fatalf("stop after the Pod is gone: %+v", ev)
	}
	requireIs(t, e.ensureErr(spec), execution.ErrResourceFenced)
	if e.fake.Calls("create pod") != 1 {
		t.Fatal("a stopped service was created again")
	}
	if got := e.remove(h); !got.Removed {
		t.Fatalf("remove %+v", got)
	}
	// Complete inventory teardown: only the chart's baseline policies remain.
	if len(e.fake.Names("pod", e.ns))+len(e.fake.Names("service", e.ns))+len(liveSecrets(e)) != 0 ||
		!eqStrings(e.fake.Names("policy", e.ns), []string{"default-deny", "runtime-egress"}) {
		t.Fatalf("inventory left: pods %v services %v secrets %v policies %v", e.fake.Names("pod", e.ns), e.fake.Names("service", e.ns), e.fake.Names("secret", e.ns), e.fake.Names("policy", e.ns))
	}
}

// Test_unit_kube_service_supervised_and_probes: a service with secret bindings
// runs `kodama service-run -- argv` through the injected entrypoint with the
// resource environment and a credential registered before the Pod exists; readiness
// probes translate for tcp, http and exec; unsupported shapes are refused.
func Test_unit_kube_service_supervised_and_probes(t *testing.T) {
	e := newRunEnv(t, tenantBody())
	spec := e.specs.Service(t, "db", true)
	spec.Service.Args = []string{"-c", "fsync=off"}
	o := e.ensure(spec)
	pod, _ := e.fake.PodOf(e.ns, spec.Name)
	ps := pod.Spec
	c := ps.Containers[0]
	if len(ps.InitContainers) != 1 || c.Command[0] != "/kodama/bin/kodama" {
		t.Fatalf("not supervised: %+v", c)
	}
	want := []string{"service-run", "--", "/usr/local/bin/docker-entrypoint.sh", "postgres", "-c", "fsync=off"}
	if strings.Join(c.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args %v", c.Args)
	}
	env := envOf(c)
	for k, w := range map[string]string{
		"KODAMA_SERVER_URL": "https://192.0.2.10:8443", "KODAMA_CA_FILE": "/kodama/bootstrap/ca.crt", "KODAMA_RUN_ID": string(spec.RunID),
		"KODAMA_RESOURCE_KEY": "service.db", "KODAMA_ADMISSION_CREDENTIAL_FILE": "/kodama/bootstrap/admission", "KODAMA_WORKLOAD_NAME": spec.Name, "POSTGRES_DB": "app",
	} {
		if env[k].Value != w {
			t.Fatalf("%s = %q", k, env[k].Value)
		}
	}
	if env["KODAMA_WORKLOAD_UID"].ValueFrom == nil || env["KODAMA_WORKLOAD_UID"].ValueFrom.FieldRef.FieldPath != "metadata.uid" {
		t.Fatalf("workload uid %+v", env["KODAMA_WORKLOAD_UID"])
	}
	if _, ok := env["KODAMA_POD_NAME"]; ok {
		t.Fatal("a Pod-owned workload reports a Pod mapping")
	}
	if pod.Metadata.Labels[kube.LabelManagedBy] != kube.ValueManagedBy {
		t.Fatalf("the callback label is missing: %v", pod.Metadata.Labels)
	}
	boot, _ := volumeOf(ps, "bootstrap")
	if len(boot.Secret.Items) != 2 {
		t.Fatalf("a service receives checkout material: %+v", boot.Secret.Items)
	}
	if len(e.reg.Calls) != 1 || e.reg.Calls[0].Key != "service.db" || !e.reg.Registered(spec.Scope, spec.RunID, "service.db") {
		t.Fatalf("registrations %+v", e.reg.Calls)
	}
	_ = o

	probe := func(name string, mod func(*execution.ServiceSpec)) (*kubernetes.Probe, error) {
		s := e.specs.Service(t, name, false)
		mod(s.Service)
		got, err := e.rr.Ensure(context.Background(), s)
		if err != nil {
			return nil, err
		}
		p, _ := e.fake.PodOf(e.ns, got.Handle.Name)
		return p.Spec.Containers[0].ReadinessProbe, nil
	}
	if p, err := probe("web", func(s *execution.ServiceSpec) {
		s.Readiness.Probe, s.Readiness.Path = "http", "/health"
	}); err != nil || p.HTTPGet == nil || p.HTTPGet.Path != "/health" || p.HTTPGet.Port != 5432 || p.HTTPGet.Scheme != "HTTP" {
		t.Fatalf("http probe %+v %v", p, err)
	}
	if p, err := probe("pg", func(s *execution.ServiceSpec) {
		s.Readiness.Probe, s.Readiness.Exec = "exec", []string{"pg_isready", "-U", "kodama"}
	}); err != nil || p.Exec == nil || len(p.Exec.Command) != 3 {
		t.Fatalf("exec probe %+v %v", p, err)
	}
	refuse := map[string]func(*execution.ServiceSpec){
		"unknown probe port":      func(s *execution.ServiceSpec) { s.Readiness.Port = "nope" },
		"reserved environment":    func(s *execution.ServiceSpec) { s.Env["KODAMA_SERVER_URL"] = "https://evil.invalid" },
		"secrets without command": func(s *execution.ServiceSpec) { s.Secrets = e.specs.Service(t, "x", true).Service.Secrets },
		"builder role":            func(s *execution.ServiceSpec) { s.Role = "builder" },
		"invalid port name":       func(s *execution.ServiceSpec) { s.Ports = map[string]int64{"sql-": 5432}; s.Readiness.Port = "sql-" },
	}
	i := 0
	for name, mod := range refuse {
		i++
		before := len(e.fake.Names("secret", e.ns))
		_, err := probe(fmt.Sprintf("r%d", i), mod)
		if err == nil || (!errors.Is(err, kube.ErrPolicy) && !errors.Is(err, execution.ErrInvalid)) {
			t.Fatalf("%s: %v", name, err)
		}
		// Refused before any effect: no record, no registration, no object.
		if len(e.fake.Names("secret", e.ns)) != before {
			t.Fatalf("%s: a record was created for a refused spec", name)
		}
	}
}

// Test_unit_kube_verify_resource_pod: a Pod a preparation claims for itself is
// confirmed only when it lives in the tenant's namespace and is controlled by the
// Job whose name and UID execution bound; claimed names and UIDs are evidence,
// never authority. Absence and mismatch are definite, an outage is not.
func Test_unit_kube_verify_resource_pod(t *testing.T) {
	ctx := context.Background()
	e := newRunEnv(t, tenantBody())
	e.ensure(e.specs.Workspace(t))
	spec := e.specs.Preparation(t)
	o := e.ensure(spec)
	h := o.Handle
	job, _ := e.fake.JobOf(e.ns, spec.Name)
	pod := e.fake.AddPod(e.ns, job, func(p *kubernetes.Pod) {
		p.Metadata.Labels[kube.LabelResourceOwner] = kube.ValueResourceOwner
		p.Metadata.Labels[kube.LabelRunKey] = kube.RunKey(spec.Scope, spec.RunID)
	})
	if err := e.rr.VerifyResourcePod(ctx, h, pod.Metadata.Name, pod.Metadata.UID); err != nil {
		t.Fatalf("the Job's own Pod: %v", err)
	}

	// An outage concludes nothing: it is not a "not owned" answer.
	for _, op := range []string{"get pod", "get job"} {
		e.fake.Fault = outage(op)
		if err := e.rr.VerifyResourcePod(ctx, h, pod.Metadata.Name, pod.Metadata.UID); err == nil || errors.Is(err, kube.ErrPodNotOwned) || !errors.Is(err, kube.ErrUnavailable) {
			t.Fatalf("%s outage: %v", op, err)
		}
	}
	e.fake.Fault = nil

	notOwned := func(name string, h execution.ResourceHandle, podName, podUID string) {
		t.Helper()
		if err := e.rr.VerifyResourcePod(ctx, h, podName, podUID); !errors.Is(err, kube.ErrPodNotOwned) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	notOwned("wrong Pod UID", h, pod.Metadata.Name, "00000000-0000-4000-8000-0000000000ff")
	notOwned("unknown Pod", h, "no-such-pod", pod.Metadata.UID)
	notOwned("no claimed Pod", h, "", "")
	unbound := h
	unbound.UID = ""
	notOwned("unbound workload", unbound, pod.Metadata.Name, pod.Metadata.UID)
	other := h
	other.UID = "00000000-0000-4000-8000-0000000000aa"
	notOwned("another Job UID", other, pod.Metadata.Name, pod.Metadata.UID)

	// A Pod of the same name controlled by another Job (a replaced Job's Pod).
	foreignJob := kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: "someone-elses-job", UID: "00000000-0000-4000-8000-0000000000bb"}}
	impostor := e.fake.AddPod(e.ns, foreignJob, func(p *kubernetes.Pod) {
		p.Metadata.Labels[kube.LabelResourceOwner] = kube.ValueResourceOwner
		p.Metadata.Labels[kube.LabelRunKey] = kube.RunKey(spec.Scope, spec.RunID)
	})
	notOwned("a Pod of another Job", h, impostor.Metadata.Name, impostor.Metadata.UID)
	// A Pod that copies the owner reference of ours but not our marks.
	copied := e.fake.AddPod(e.ns, job, func(p *kubernetes.Pod) { delete(p.Metadata.Labels, kube.LabelRunKey) })
	notOwned("a Pod without the run's marks", h, copied.Metadata.Name, copied.Metadata.UID)
	// A Pod claimed under a name that is not its own.
	notOwned("a Pod claimed under another name", h, impostor.Metadata.Name, pod.Metadata.UID)

	// The Job of the handle must be the bound instance: a replaced Job's Pods are not ours.
	e.fake.DropJob(e.ns, spec.Name)
	notOwned("a Job that is gone", h, pod.Metadata.Name, pod.Metadata.UID)

	// A service has no Job-owned Pod; another tenant's handle names another namespace.
	svc := e.ensure(e.specs.Service(t, "db", false))
	notOwned("a service", svc.Handle, svc.Handle.Name, svc.Handle.UID)
	stranger := h
	stranger.Scope.TenantID = "t021_unknown"
	stranger.Name = execution.ResourceName(stranger.Scope, stranger.RunID, stranger.Kind, stranger.Key)
	if err := e.rr.VerifyResourcePod(ctx, stranger, pod.Metadata.Name, pod.Metadata.UID); err == nil || errors.Is(err, kube.ErrPodNotOwned) {
		t.Fatalf("a tenant with no target: %v", err)
	}

}
