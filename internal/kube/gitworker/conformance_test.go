package gitworker_test

// Kind conformance for the tenant Git hosting (T-129; GR-03, GR-04, GR-A03,
// IT-17, IT-A13). It runs the reconciler against a real cluster and proves what a
// fake API server cannot. Two tests, two different claims:
//
//   - Test_conformance_gitworker_two_tenants: isolated namespaces, claims and
//     volumes; NetworkPolicy isolation (the control plane reaches a worker, an
//     execution namespace does not); data retention across worker replacement; a
//     verified tenant re-reconciling with no write against the real API; and
//     offboarding. It runs on kind's rancher.io/local-path class, which is NOT a
//     CSI driver and cannot provide ReadWriteOncePod, so it explicitly lists that
//     provisioner as accepted FOR THIS TEST ONLY (Options.RWOPProvisioners is the
//     installation's declaration of what is qualified) and DOES NOT PROVE
//     SINGLE-POD EXCLUSIVITY.
//   - Test_conformance_gitworker_rwop_exclusive: a second writer Pod is refused by
//     ReadWriteOncePod. It needs a qualified CSI class and skips with a message
//     unless KODAMA_TEST_GIT_STORAGE_CLASS names one (T-133 runs it on stage with
//     the Scaleway CSI class).
//
// Run them on a prepared cluster scope (they never provision one):
//
//	make check-go-integration DEV_SCOPE=t129 DEV_STACK=cluster PKGS='./internal/kube/gitworker/...'
//
// Without a cluster, or in the unit layer (-tags unit), they skip; a skip is not
// acceptance evidence. The worker here is the scaffold image running a busybox nc
// responder on the worker port: the real `kodama git serve` is T-128/T-132's. Only the
// objects, the volume and the network are under test.
//
// Environment:
//
//	KODAMA_TEST_GIT_IMAGE          digest-pinned image with sh, nc -e and wget
//	                               (default: the image of the scope's kodama-deps/kodama Deployment)
//	KODAMA_TEST_GIT_STORAGE_CLASS  an existing class with reclaimPolicy Retain. Required by the
//	                               rwop test and then it must be a qualified CSI class supporting
//	                               ReadWriteOncePod; optional for the two-tenant test (default: a
//	                               class that test creates and removes on kind's local-path)
//	KODAMA_TEST_GIT_PROVISIONER    provisioner of the class the two-tenant test creates
//	                               (default: rancher.io/local-path)

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/gitworker"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

var pinnedImage = regexp.MustCompile(`^[A-Za-z0-9./:_-]+@sha256:[0-9a-f]{64}$`)

type kindEnv struct {
	t         *testing.T
	kc        string
	cl        *kubernetes.Client
	image     string
	class     string
	provision string
	id        string
	cpNS      string
	exNS      string
	rec       *gitworker.Reconciler
	spec      gitworker.WorkerSpec
}

const workerScript = `set -e
mkdir -p /data/www
cd /data/www
echo ok > healthz
[ -f marker ] || echo "$KODAMA_GIT_TENANT_ID" > marker
hostname >> boots
cat > /tmp/h.sh <<'EOF'
#!/bin/sh
read -r m p r
while read -r l; do l=${l%?}; [ -z "$l" ] && break; done
f=/data/www$p
if [ -f "$f" ]; then
  printf "HTTP/1.0 200 OK\r\nContent-Length: %s\r\nConnection: close\r\n\r\n" $(wc -c < "$f")
  cat "$f"
else
  printf "HTTP/1.0 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
fi
EOF
chmod +x /tmp/h.sh
exec nc -ll -p 8443 -e /tmp/h.sh
`

type conformanceIdentity struct{}

func (conformanceIdentity) WorkerIdentity(_ context.Context, t domain.TenantID) (gitworker.Material, error) {
	return gitworker.Material{
		TenantID: t, Token: []byte("conformance." + string(t)), ExpiresAt: time.Now().Add(30 * time.Minute),
		PublicKeys: keyMaterial.publicPEM, CA: keyMaterial.certPEM,
	}, nil
}

type allowAll struct{}

func (allowAll) MayDeleteVolume(context.Context, domain.TenantID) (bool, error) { return true, nil }

func newKindEnv(t *testing.T, rwop bool) *kindEnv {
	t.Helper()
	kc := kubetest.Require(t)
	if rwop && os.Getenv("KODAMA_TEST_GIT_STORAGE_CLASS") == "" {
		t.Skip("KODAMA_TEST_GIT_STORAGE_CLASS is not set: single-Pod exclusivity needs a qualified CSI class with reclaimPolicy Retain that supports ReadWriteOncePod (kind's local-path cannot); T-133 runs this on stage")
	}
	cl, err := kubetest.NewClient(kc)
	if err != nil {
		t.Fatalf("cluster client: %v", err)
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	e := &kindEnv{t: t, kc: kc, cl: cl, id: hex.EncodeToString(b[:])}
	e.cpNS, e.exNS = "t129cp-"+e.id, "t129ex-"+e.id
	e.provision = os.Getenv("KODAMA_TEST_GIT_PROVISIONER")
	if e.provision == "" {
		e.provision = "rancher.io/local-path"
	}

	e.image = os.Getenv("KODAMA_TEST_GIT_IMAGE")
	if e.image == "" {
		out, err := kubetest.Kubectl(kc, "-n", "kodama-deps", "get", "deployment", "kodama", "-o", "jsonpath={.spec.template.spec.containers[0].image}")
		if err != nil {
			t.Fatalf("no worker image: set KODAMA_TEST_GIT_IMAGE or prepare the scope (%v)", err)
		}
		e.image = strings.TrimSpace(string(out))
	}
	if !pinnedImage.MatchString(e.image) {
		t.Fatalf("the worker image %q is not pinned by digest", e.image)
	}

	e.class = os.Getenv("KODAMA_TEST_GIT_STORAGE_CLASS")
	if e.class == "" {
		e.class = "t129-retain-" + e.id
		manifest := fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: %s
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
`, e.class, e.provision)
		path := filepath.Join(t.TempDir(), "class.yaml")
		if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := kubetest.Kubectl(kc, "apply", "-f", path); err != nil {
			t.Fatalf("storage class: %v: %s", err, out)
		}
		t.Cleanup(func() { _, _ = kubetest.Kubectl(kc, "delete", "storageclass", e.class, "--ignore-not-found") })
	}
	sc, err := cl.GetStorageClass(context.Background(), e.class)
	if err != nil {
		t.Fatalf("storage class %s: %v", e.class, err)
	}
	e.provision = sc.Provisioner
	if rwop && (e.provision == "rancher.io/local-path" || e.provision == "kubernetes.io/no-provisioner") {
		t.Fatalf("storage class %s (%s) is not a CSI class that can provide ReadWriteOncePod", e.class, e.provision)
	}

	for _, ns := range []string{e.cpNS, e.exNS} {
		if _, err := cl.CreateNamespace(context.Background(), kubernetes.Namespace{Metadata: kubernetes.ObjectMeta{Name: ns, Labels: map[string]string{"kodama.optimi.com/test": "t129"}}}); err != nil {
			t.Fatalf("namespace %s: %v", ns, err)
		}
		ns := ns
		t.Cleanup(func() { _, _ = kubetest.Kubectl(kc, "delete", "namespace", ns, "--ignore-not-found", "--wait=false") })
	}

	e.spec = gitworker.WorkerSpec{
		Image: e.image, Command: []string{"sh", "-ec"}, Args: []string{workerScript}, ImagePullPolicy: "IfNotPresent",
		Resources: gitworker.Resources{
			CPURequest: "10m", CPULimit: "500m", MemoryRequest: "32Mi", MemoryLimit: "128Mi",
			EphemeralRequest: "16Mi", EphemeralLimit: "128Mi", TmpSize: "32Mi",
		},
		Storage: gitworker.Storage{Class: e.class, Size: "100Mi"}, ListenPort: 8443, Scheme: "http",
		ControlPlane: gitworker.ControlPlane{
			Namespace: e.cpNS, PodLabels: map[string]string{"app": "t129-control-plane"}, PodPort: 8080, URL: "http://control-plane." + e.cpNS + ".svc:8080",
		},
		PodSecurityVersion: "v1.32",
	}
	e.rec, err = gitworker.New(gitworker.Dependencies{Cluster: cl, Identity: conformanceIdentity{}, Retention: allowAll{}},
		// Test-only declaration: the installation lists a provisioner here only after
		// qualifying it for ReadWriteOncePod. The two-tenant test lists local-path
		// without that qualification and does not claim single-Pod exclusivity; the
		// rwop test lists a class the operator named as qualified.
		gitworker.Options{RWOPProvisioners: []string{e.provision}, StatusTTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func until(t *testing.T, timeout time.Duration, what string, fn func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		ok, why := fn()
		if ok {
			return
		}
		last = why
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %s", what, last)
		}
		time.Sleep(2 * time.Second)
	}
}

// ready reconciles until the tenant's worker is ready.
func (e *kindEnv) ready(tenant domain.TenantID) gitworker.Status {
	e.t.Helper()
	var st gitworker.Status
	until(e.t, 4*time.Minute, "the worker of "+string(tenant), func() (bool, string) {
		var err error
		st, err = e.rec.Reconcile(context.Background(), tenant, e.spec)
		for _, final := range []error{gitworker.ErrInvalidSpec, gitworker.ErrIdentity, gitworker.ErrStorage, gitworker.ErrForeign, gitworker.ErrDrift} {
			if isErr(err, final) {
				e.t.Fatalf("the worker of %s cannot converge: %v", tenant, err)
			}
		}
		if err != nil {
			return false, err.Error()
		}
		return st.Ready, st.Reason
	})
	return st
}

// probe runs a Pod in ns that fetches url and reports what it saw.
func (e *kindEnv) probe(ns string, labels map[string]string, name, url string) string {
	e.t.Helper()
	script := fmt.Sprintf(`if out=$(wget -q -T 5 -O- %s 2>/dev/null); then echo "REACHED:$out"; else echo BLOCKED; fi`, url)
	_, err := e.cl.CreatePod(context.Background(), ns, kubernetes.Pod{
		Metadata: kubernetes.ObjectMeta{Name: name, Labels: labels},
		Spec: kubernetes.PodSpec{
			RestartPolicy: "Never", AutomountServiceAccountToken: ptrBool(false),
			Containers: []kubernetes.Container{{Name: "probe", Image: e.image, Command: []string{"sh", "-c", script}}},
		},
	})
	if err != nil {
		e.t.Fatalf("probe %s: %v", name, err)
	}
	var phase string
	until(e.t, 3*time.Minute, "probe "+name, func() (bool, string) {
		p, err := e.cl.GetPod(context.Background(), ns, name)
		if err != nil {
			return false, err.Error()
		}
		phase = p.Status.Phase
		return phase == "Succeeded" || phase == "Failed", phase
	})
	log, err := e.cl.PodLog(context.Background(), ns, name, "probe")
	if err != nil {
		e.t.Fatalf("probe %s log: %v", name, err)
	}
	return strings.TrimSpace(string(log))
}

func ptrBool(b bool) *bool { return &b }

// workerURL is the Service URL of the tenant's worker path.
func workerURL(tenant domain.TenantID, path string) string {
	return fmt.Sprintf("http://%s.%s.svc:8443/%s", gitworker.ServiceName, gitworker.NamespaceOf(tenant), path)
}

// Test_conformance_gitworker_two_tenants: see the header of this file. It does not
// prove single-Pod exclusivity (local-path is not a ReadWriteOncePod provider).
func Test_conformance_gitworker_two_tenants(t *testing.T) {
	e := newKindEnv(t, false)
	ctx := context.Background()
	a, b := domain.TenantID("t129a"+e.id), domain.TenantID("t129b"+e.id)
	nsA, nsB := gitworker.NamespaceOf(a), gitworker.NamespaceOf(b)

	t.Cleanup(func() {
		// Explicit offboarding, the only path that removes a claim. The retained
		// volumes' data is released by the storage administrator, so the test
		// removes the volumes it provisioned.
		for _, tenant := range []domain.TenantID{a, b} {
			_ = e.rec.OffboardTenant(context.Background(), tenant)
		}
		pvs, err := e.cl.ListPVs(context.Background())
		if err != nil {
			return
		}
		for _, pv := range pvs {
			if pv.Spec.ClaimRef != nil && (pv.Spec.ClaimRef.Namespace == nsA || pv.Spec.ClaimRef.Namespace == nsB) {
				_, _ = kubetest.Kubectl(e.kc, "delete", "pv", pv.Metadata.Name, "--ignore-not-found", "--wait=false")
			}
		}
	})

	stA, stB := e.ready(a), e.ready(b)

	t.Run("a verified, ready tenant converges without a write", func(t *testing.T) {
		// What the real API server stores must equal what was rendered, field by
		// field this package compares; otherwise every pass would "correct" it.
		for _, tenant := range []domain.TenantID{a, b} {
			st, err := e.rec.Reconcile(ctx, tenant, e.spec)
			if err != nil || !st.Ready || len(st.Actions) != 0 {
				t.Fatalf("%s: %+v %v", tenant, st, err)
			}
		}
	})

	t.Run("two tenants, two namespaces, two volumes", func(t *testing.T) {
		if stA.Namespace == stB.Namespace || stA.Endpoint == stB.Endpoint {
			t.Fatalf("%+v %+v", stA, stB)
		}
		var vols []string
		for _, tenant := range []domain.TenantID{a, b} {
			ns := gitworker.NamespaceOf(tenant)
			n, err := e.cl.GetNamespace(ctx, ns)
			if err != nil {
				t.Fatal(err)
			}
			if n.Metadata.Labels["pod-security.kubernetes.io/enforce"] != "restricted" || n.Metadata.Annotations[gitworker.AnnTenant] != string(tenant) {
				t.Fatalf("namespace %+v", n.Metadata)
			}
			claim, err := e.cl.GetPVC(ctx, ns, gitworker.ClaimName)
			if err != nil || claim.Status.Phase != "Bound" || len(claim.Spec.AccessModes) != 1 || claim.Spec.AccessModes[0] != "ReadWriteOncePod" {
				t.Fatalf("claim %+v %v", claim, err)
			}
			pv, err := e.cl.GetPV(ctx, claim.Spec.VolumeName)
			if err != nil || pv.Spec.PersistentVolumeReclaimPolicy != "Retain" || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Namespace != ns {
				t.Fatalf("volume %+v %v", pv, err)
			}
			vols = append(vols, claim.Spec.VolumeName)
			sa, err := e.cl.GetServiceAccount(ctx, ns, gitworker.ServiceAccountName)
			if err != nil || sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
				t.Fatalf("service account %+v %v", sa, err)
			}
			pods, err := e.cl.ListPods(ctx, ns, "")
			if err != nil || len(pods) != 1 {
				t.Fatalf("pods %d %v", len(pods), err)
			}
			for _, v := range pods[0].Spec.Volumes {
				if strings.HasPrefix(v.Name, "kube-api-access") {
					t.Fatal("the worker Pod mounts a Kubernetes API token")
				}
			}
			if ep, err := e.rec.Endpoint(ctx, tenant); err != nil || ep.BaseURL != fmt.Sprintf("http://git-worker.%s.svc:8443", ns) {
				t.Fatalf("endpoint %+v %v", ep, err)
			}
		}
		if vols[0] == vols[1] {
			t.Fatal("two tenants share a volume")
		}
	})

	t.Run("the control plane reaches a worker, an execution namespace does not", func(t *testing.T) {
		cp := map[string]string{"app": "t129-control-plane"}
		if got := e.probe(e.cpNS, cp, "from-cp-a", workerURL(a, "marker")); got != "REACHED:"+string(a) {
			t.Fatalf("control plane -> tenant A: %q", got)
		}
		if got := e.probe(e.cpNS, cp, "from-cp-b", workerURL(b, "marker")); got != "REACHED:"+string(b) {
			t.Fatalf("control plane -> tenant B: %q (each worker serves its own volume)", got)
		}
		if got := e.probe(e.exNS, nil, "from-ex-a", workerURL(a, "marker")); got != "BLOCKED" {
			t.Fatalf("execution namespace -> tenant A: %q", got)
		}
		if got := e.probe(e.exNS, cp, "from-ex-cplabels-b", workerURL(b, "marker")); got != "BLOCKED" {
			t.Fatalf("an execution namespace Pod with the control plane's labels -> tenant B: %q (the namespace selector must apply)", got)
		}
		if got := e.probe(e.cpNS, map[string]string{"app": "something-else"}, "from-cp-other", workerURL(a, "marker")); got != "BLOCKED" {
			t.Fatalf("another Pod of the control plane's namespace -> tenant A: %q (the Pod selector must apply)", got)
		}
	})

	t.Run("replacing the worker keeps the data", func(t *testing.T) {
		claimBefore, err := e.cl.GetPVC(ctx, nsA, gitworker.ClaimName)
		if err != nil {
			t.Fatal(err)
		}
		podsBefore, err := e.cl.ListPods(ctx, nsA, "")
		if err != nil || len(podsBefore) != 1 {
			t.Fatalf("pods %d %v", len(podsBefore), err)
		}
		depBefore, err := e.cl.GetDeployment(ctx, nsA, gitworker.DeploymentName)
		if err != nil {
			t.Fatal(err)
		}

		// A different command is a different worker: the Deployment is replaced, the
		// Recreate strategy stops the old Pod first and the claim is untouched.
		e.spec.Args = []string{workerScript + "\n# replacement\n"}
		var st gitworker.Status
		until(t, 4*time.Minute, "the replacement worker", func() (bool, string) {
			var err error
			st, err = e.rec.Reconcile(ctx, a, e.spec)
			if err != nil {
				return false, err.Error()
			}
			if st.Ready {
				p, _ := e.cl.ListPods(ctx, nsA, "")
				return len(p) == 1 && p[0].Metadata.UID != podsBefore[0].Metadata.UID, "the old Pod is still running"
			}
			return false, st.Reason
		})
		depAfter, err := e.cl.GetDeployment(ctx, nsA, gitworker.DeploymentName)
		if err != nil || depAfter.Metadata.UID != depBefore.Metadata.UID || depAfter.Generation <= depBefore.Generation {
			t.Fatalf("the Deployment was not replaced in place: %+v %v", depAfter.Metadata, err)
		}
		claimAfter, err := e.cl.GetPVC(ctx, nsA, gitworker.ClaimName)
		if err != nil || claimAfter.Metadata.UID != claimBefore.Metadata.UID || claimAfter.Spec.VolumeName != claimBefore.Spec.VolumeName {
			t.Fatalf("the claim changed: %+v %v", claimAfter, err)
		}
		cp := map[string]string{"app": "t129-control-plane"}
		if got := e.probe(e.cpNS, cp, "after-marker", workerURL(a, "marker")); got != "REACHED:"+string(a) {
			t.Fatalf("the replacement worker lost the marker: %q", got)
		}
		boots := e.probe(e.cpNS, cp, "after-boots", workerURL(a, "boots"))
		lines := strings.Split(strings.TrimPrefix(boots, "REACHED:"), "\n")
		if len(lines) != 2 || lines[0] != podsBefore[0].Metadata.Name {
			t.Fatalf("the replacement worker did not continue the volume's history: %q (first boot was %s)", boots, podsBefore[0].Metadata.Name)
		}
	})

	t.Run("another tenant is unaffected by an offboarding", func(t *testing.T) {
		if err := e.rec.OffboardTenant(ctx, b); err != nil {
			t.Fatal(err)
		}
		until(t, 3*time.Minute, "the namespace of tenant B to go", func() (bool, string) {
			_, err := e.cl.GetNamespace(ctx, nsB)
			return isErr(err, kubernetes.ErrNotFound), fmt.Sprint(err)
		})
		if _, err := e.rec.Endpoint(ctx, b); !isErr(err, gitworker.ErrWorkerUnavailable) {
			t.Fatalf("an offboarded tenant resolves: %v", err)
		}
		if _, err := e.rec.Endpoint(ctx, a); err != nil {
			t.Fatalf("tenant A: %v", err)
		}
		cp := map[string]string{"app": "t129-control-plane"}
		if got := e.probe(e.cpNS, cp, "after-offboard", workerURL(a, "marker")); got != "REACHED:"+string(a) {
			t.Fatalf("tenant A after offboarding B: %q", got)
		}
		// The retained volume of B outlives its claim until the administrator
		// releases it.
		pvs, err := e.cl.ListPVs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, pv := range pvs {
			if pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.Namespace == nsB && pv.Spec.PersistentVolumeReclaimPolicy == "Retain" {
				found = true
			}
		}
		if !found {
			t.Fatal("the retained volume of the offboarded tenant is gone")
		}
	})
}

// Test_conformance_gitworker_rwop_exclusive proves GR-04 on a real
// ReadWriteOncePod provider: while the worker holds the claim, a second Pod that
// names it is refused by the scheduler, and the reconciler reports the foreign Pod.
// It needs KODAMA_TEST_GIT_STORAGE_CLASS (a qualified CSI class, reclaimPolicy
// Retain) and skips with a message without it.
func Test_conformance_gitworker_rwop_exclusive(t *testing.T) {
	e := newKindEnv(t, true)
	ctx := context.Background()
	a := domain.TenantID("t129r" + e.id)
	nsA := gitworker.NamespaceOf(a)
	t.Cleanup(func() {
		_ = e.rec.OffboardTenant(context.Background(), a)
		pvs, err := e.cl.ListPVs(context.Background())
		if err != nil {
			return
		}
		for _, pv := range pvs {
			if pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.Namespace == nsA {
				_, _ = kubetest.Kubectl(e.kc, "delete", "pv", pv.Metadata.Name, "--ignore-not-found", "--wait=false")
			}
		}
	})
	e.ready(a)

	{
		no, yes := false, true
		uid := int64(65532)
		second := kubernetes.Pod{
			Metadata: kubernetes.ObjectMeta{Name: "second-writer"},
			Spec: kubernetes.PodSpec{
				RestartPolicy: "Never", AutomountServiceAccountToken: &no,
				SecurityContext: &kubernetes.PodSecurity{RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid, SeccompProfile: &kubernetes.SeccompProfile{Type: "RuntimeDefault"}},
				Containers: []kubernetes.Container{{
					Name: "second", Image: e.image, Command: []string{"sh", "-c", "echo second > /data/www/second-writer; sleep 3600"},
					VolumeMounts: []kubernetes.VolumeMount{{Name: "data", MountPath: "/data"}},
					SecurityContext: &kubernetes.ContainerSecurity{
						Privileged: &no, AllowPrivilegeEscalation: &no, RunAsNonRoot: &yes, RunAsUser: &uid,
						Capabilities: &kubernetes.Capabilities{Drop: []string{"ALL"}}, SeccompProfile: &kubernetes.SeccompProfile{Type: "RuntimeDefault"},
					},
				}},
				Volumes: []kubernetes.Volume{{Name: "data", PersistentVolumeClaim: &kubernetes.PVCVolume{ClaimName: gitworker.ClaimName}}},
			},
		}
		created, err := e.cl.CreatePod(ctx, nsA, second)
		if err != nil {
			t.Fatalf("a restricted Pod was refused by the namespace: %v", err)
		}
		t.Cleanup(func() {
			_ = e.cl.DeletePod(context.Background(), nsA, "second-writer", kubernetes.DeleteOptions{UID: created.Metadata.UID, Propagation: "Background"})
		})
		var message string
		until(t, 2*time.Minute, "the scheduler to refuse the second writer", func() (bool, string) {
			p, err := e.cl.GetPod(ctx, nsA, "second-writer")
			if err != nil {
				return false, err.Error()
			}
			if p.Status.Phase != "Pending" || p.Spec.NodeName != "" {
				t.Fatalf("a second writer was scheduled: phase %s node %q", p.Status.Phase, p.Spec.NodeName)
			}
			out, err := kubetest.Kubectl(e.kc, "-n", nsA, "get", "pod", "second-writer", "-o", `jsonpath={.status.conditions[?(@.type=="PodScheduled")].message}`)
			if err != nil {
				return false, err.Error()
			}
			message = string(out)
			return strings.Contains(message, "ReadWriteOncePod"), "scheduling message: " + message
		})
		// The reconciler does not tolerate it either, and the worker still serves.
		if _, err := e.rec.Reconcile(ctx, a, e.spec); !isErr(err, gitworker.ErrIsolation) {
			t.Fatalf("reconcile with a second Pod in the namespace: %v", err)
		}
		if _, err := e.rec.Endpoint(ctx, a); !isErr(err, gitworker.ErrWorkerUnavailable) {
			t.Fatalf("endpoint: %v", err)
		}
		if err := e.cl.DeletePod(ctx, nsA, "second-writer", kubernetes.DeleteOptions{UID: created.Metadata.UID, Propagation: "Background"}); err != nil {
			t.Fatal(err)
		}
		until(t, time.Minute, "the second writer to go", func() (bool, string) {
			_, err := e.cl.GetPod(ctx, nsA, "second-writer")
			return isErr(err, kubernetes.ErrNotFound), fmt.Sprint(err)
		})
		e.ready(a)
	}
}
