package kernelrt

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

const (
	testNS  = "tenant-a"
	testSA  = "kodama-run"
	testUID = int64(65532)
)

var (
	scopeA = domain.Scope{TenantID: "tenant-a", ProjectID: "proj-1"}
	scopeB = domain.Scope{TenantID: "tenant-b", ProjectID: "proj-1"}

	pinnedImage = "registry.example/step@sha256:" + strings.Repeat("a", 64)
	svcImage    = "registry.example/db@sha256:" + strings.Repeat("b", 64)
)

type harness struct {
	t     *testing.T
	cl    *fakeCluster
	clock *fakeClock
	ad    *Adapter
	rt    *Runtime
	res   *Resources
	files *fakeFiles
}

type fakeFiles struct {
	calls int
	data  map[string][]byte
	err   error
}

func (f *fakeFiles) Files(context.Context, runtime.Materialization) (map[string][]byte, error) {
	f.calls++
	return f.data, f.err
}

func testConfig() Config {
	b := Bounds{CPURequest: "100m", CPULimit: "1", MemoryRequest: "128Mi", MemoryLimit: "512Mi", EphemeralRequest: "128Mi", EphemeralLimit: "1Gi"}
	return Config{
		RunAsUser: testUID, RunAsGroup: testUID, Workload: b, Service: b,
		ScratchSize: "1Gi", WorkspaceSize: "5Gi", StorageClass: "fast",
	}
}

func defaultDeny() kubernetes.NetworkPolicy {
	return kubernetes.NetworkPolicy{Metadata: kubernetes.ObjectMeta{Name: "default-deny"},
		Spec: kubernetes.NetworkPolicySpec{PolicyTypes: []string{"Ingress", "Egress"}}}
}

func newHarness(t *testing.T, mod ...func(*Config)) *harness {
	t.Helper()
	clock := newClock()
	cl := newFake(clock)
	cl.namespaces[testNS] = kubernetes.Namespace{Metadata: kubernetes.ObjectMeta{Name: testNS}}
	cl.accounts[key(testNS, testSA)] = true
	cl.netpols[testNS] = []kubernetes.NetworkPolicy{defaultDeny()}
	cl.classes["fast"] = kubernetes.StorageClass{Metadata: kubernetes.ObjectMeta{Name: "fast"}, VolumeBindingMode: "WaitForFirstConsumer"}
	cfg := testConfig()
	for _, m := range mod {
		m(&cfg)
	}
	files := &fakeFiles{data: map[string][]byte{"token": []byte("s3cr3t-value")}}
	ad, err := New(cfg, Dependencies{Cluster: cl, Targets: SingleTarget(Target{Namespace: testNS, ServiceAccount: testSA}), Secrets: files},
		Options{Clock: clock.Now, CreateQuiescence: time.Minute, OrphanGrace: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, cl: cl, clock: clock, ad: ad, rt: ad.Runtime(), res: ad.Resources(), files: files}
}

func (h *harness) workspaceSpec(scope domain.Scope, run domain.RunID, fence runtime.Fence) runtime.ResourceSpec {
	return runtime.ResourceSpec{
		Scope: scope, Owner: runtime.OwnerFor(runtime.LifetimePipeline, scope), RunID: run, Kind: runtime.ResourceWorkspace, Name: "workspace", Lifetime: runtime.LifetimePipeline,
		Identity: "ws-" + string(run), SpecDigest: "ws-digest", Fence: fence,
		Preparation: &runtime.PreparationInputs{Scope: scope, RunID: run, Digest: "prep"},
	}
}

func (h *harness) serviceSpec(scope domain.Scope, run domain.RunID, name string, fence runtime.Fence) runtime.ResourceSpec {
	return runtime.ResourceSpec{
		Scope: scope, Owner: runtime.OwnerFor(runtime.LifetimePipeline, scope), RunID: run, Kind: runtime.ResourceService, Name: name, Lifetime: runtime.LifetimePipeline,
		Identity: "svc-" + name + "-" + string(run), SpecDigest: "svc-digest", Fence: fence,
		Service: &runtime.ServiceSpec{Name: name, Lifetime: runtime.LifetimePipeline, Image: svcImage, Ports: []int{5432}},
	}
}

func (h *harness) ensureWorkspace(scope domain.Scope, run domain.RunID, fence runtime.Fence) runtime.ResourceHandle {
	h.t.Helper()
	spec := h.workspaceSpec(scope, run, fence)
	o, err := h.res.Ensure(context.Background(), spec)
	if err != nil {
		h.t.Fatalf("ensure workspace: %v", err)
	}
	if err := o.Validate(); err != nil {
		h.t.Fatalf("invalid observation: %v", err)
	}
	return o.Handle
}

func binding(scope domain.Scope, run domain.RunID, step string, attempt int, digest string, fence runtime.Fence) runtime.RuntimeBinding {
	return runtime.RuntimeBinding{
		Scope: scope, RunID: run, StepKey: domain.StepID(step), Attempt: attempt,
		WorkloadID: string(run) + "-" + step + "-" + string(rune('0'+attempt)), SpecDigest: digest, Backend: "kubernetes", Fence: fence,
	}
}

func (h *harness) startRequest(b runtime.RuntimeBinding, ws runtime.ResourceHandle) runtime.StartRequest {
	return runtime.StartRequest{
		Binding: b,
		Spec: runtime.WorkloadSpec{
			Image: pinnedImage, Commands: []string{"echo hi", "make test"}, Env: map[string]string{"CI": "1"},
			Timeout: 90 * time.Second,
		},
		Workspace: ws,
	}
}

func (h *harness) start(req runtime.StartRequest) (runtime.WorkloadObservation, error) {
	return h.rt.Start(context.Background(), req)
}

func (h *harness) mustStart(req runtime.StartRequest) runtime.WorkloadObservation {
	h.t.Helper()
	o, err := h.start(req)
	if err != nil {
		h.t.Fatalf("start: %v", err)
	}
	if err := o.Validate(); err != nil {
		h.t.Fatalf("invalid observation: %v", err)
	}
	return o
}

func (h *harness) jobName(b runtime.RuntimeBinding) string { return workloadIdent(b).base() }

func isNotReached(err error) bool { return runtime.ClassifyEffect(err) == runtime.CommandRejected }

func (h *harness) target() Target { return Target{Namespace: testNS, ServiceAccount: testSA} }

func (h *harness) verifyJob(j kubernetes.Job, b runtime.RuntimeBinding) error {
	rec, _, err := h.ad.readRecord(context.Background(), testNS, workloadIdent(b))
	if err != nil {
		return err
	}
	return h.ad.verifyJob(j, workloadIdent(b), h.target(), rec)
}

func (h *harness) verifyPod(p kubernetes.Pod, id ident) error {
	rec, _, err := h.ad.readRecord(context.Background(), testNS, id)
	if err != nil {
		return err
	}
	return h.ad.verifyPod(p, id, h.target(), rec)
}
