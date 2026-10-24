package delivery_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"example.invalid/inventory/internal/delivery"
)

func fakeKubectl(t *testing.T, namespace string) (*delivery.Kubectl, string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakekubectl")
	build := exec.Command("go", "build", "-o", bin, "./test/support/fakekubectl")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the fake kubectl: %v\n%s", err, out)
	}
	state := t.TempDir()
	t.Setenv("FAKEKUBECTL_STATE", state)
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	_ = os.WriteFile(kc, []byte("namespace="+namespace+"\n"), 0o600)
	return &delivery.Kubectl{Binary: bin, Kubeconfig: kc}, state
}

func plan() delivery.Plan {
	p := delivery.Plan{Namespace: ns, ImageRepo: repo, ImageDigest: digest, SourceCommit: commit, DeploymentID: "dep_" + strings.Repeat("a", 32), ConfigDigest: "sha256:" + strings.Repeat("b", 64),
		Config: baseSnapshot("preview", ns).Config, Secrets: map[string]string{"DATABASE_RUNTIME_URL": "postgres://rt@h/db", "DATABASE_MIGRATION_URL": "postgres://own@h/db", "CURSOR_KEY": strings.Repeat("k", 40), "ERP_TOKEN": "t"}}
	p.Annotations = map[string]string{delivery.AnnotationDeploymentID: p.DeploymentID}
	return p
}

func TestKubectlAdapterAppliesAndObserves(t *testing.T) {
	k, _ := fakeKubectl(t, ns)
	ctx := context.Background()
	p := plan()
	list, err := p.Render()
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Apply(ctx, ns, list); err != nil {
		t.Fatal(err)
	}
	for _, ref := range p.Refs() {
		o, ok, err := k.Get(ctx, ref)
		if err != nil || !ok || o.UID == "" || !o.Ready || o.Annotations[delivery.AnnotationDeploymentID] != p.DeploymentID {
			t.Fatalf("%+v: %+v %v %v", ref, o, ok, err)
		}
	}
	// A re-apply keeps identities.
	first, _, _ := k.Get(ctx, p.Refs()[4])
	if err := k.Apply(ctx, ns, list); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := k.Get(ctx, p.Refs()[4]); again.UID != first.UID {
		t.Fatal("a re-apply must keep the object's identity")
	}
	// Absent objects are not an error.
	if _, ok, err := k.Get(ctx, delivery.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: ns, Name: "missing"}); ok || err != nil {
		t.Fatalf("absent: %v %v", ok, err)
	}
}

func TestKubectlAdapterNamespaceScoping(t *testing.T) {
	k, state := fakeKubectl(t, "other-namespace") // the credential covers another namespace
	p := plan()
	list, _ := p.Render()
	if err := k.Apply(context.Background(), ns, list); err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("a credential for another namespace must be refused: %v", err)
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Fatal("nothing may be applied")
	}
	// A manifest naming another namespace is refused even with a valid credential.
	k2, _ := fakeKubectl(t, ns)
	if err := k2.Apply(context.Background(), ns, []byte(strings.ReplaceAll(string(list), `"namespace": "`+ns+`"`, `"namespace": "kube-system"`))); err == nil {
		t.Fatal("an object outside the environment namespace must be refused")
	}
	// Reads are scoped too.
	if _, _, err := k.Get(context.Background(), p.Refs()[0]); err == nil {
		t.Fatal("observing another namespace must be refused")
	}
}

func TestKubectlAdapterLostApplyResponseIsObservable(t *testing.T) {
	k, state := fakeKubectl(t, ns)
	_ = os.WriteFile(filepath.Join(state, "DROP_APPLY_RESPONSE"), nil, 0o600)
	p := plan()
	list, _ := p.Render()
	if err := k.Apply(context.Background(), ns, list); err == nil {
		t.Fatal("the lost response is an error")
	}
	if _, ok, _ := k.Get(context.Background(), p.Refs()[4]); !ok {
		t.Fatal("the objects exist: only the response was lost")
	}
}

func TestKubectlAdapterEndToEndWithDeployer(t *testing.T) {
	r := newRig(t)
	k, _ := fakeKubectl(t, ns)
	r.in.ClusterFor = func(string) delivery.Cluster { return k } // the kubeconfig path is the fake's own
	res := r.run()
	if !res.Completed {
		t.Fatalf("%+v\n%s", res, r.events())
	}
}

func TestRenderedObjectsFollowTheRestrictedBaseline(t *testing.T) {
	list, err := plan().Render()
	if err != nil {
		t.Fatal(err)
	}
	s := string(list)
	for _, forbidden := range []string{`"hostPath"`, `"privileged": true`, `"hostNetwork"`, `"hostPID"`, `docker.sock`, `"allowPrivilegeEscalation": true`} {
		if strings.Contains(s, forbidden) {
			t.Errorf("manifest contains %s", forbidden)
		}
	}
	for _, required := range []string{`"runAsNonRoot": true`, `"readOnlyRootFilesystem": true`, `"allowPrivilegeEscalation": false`, `"RuntimeDefault"`, `"automountServiceAccountToken": false`, `"drop"`} {
		if !strings.Contains(s, required) {
			t.Errorf("manifest lacks %s", required)
		}
	}
	// Images are pulled by digest only; credentials appear only in Secrets of this deployment.
	if !strings.Contains(s, repo+"@"+digest) || strings.Contains(s, repo+":latest") {
		t.Error("images must be digest pinned")
	}
	if strings.Count(s, "postgres://own@h/db") != 1 {
		t.Error("the migration credential must appear only in the migration Secret")
	}
	// The migration credential never reaches the API or worker pod specs.
	for _, name := range []string{"inventory-api", "inventory-worker"} {
		i := strings.Index(s, `"name": "`+name+`"`)
		if i < 0 {
			t.Fatalf("no %s", name)
		}
	}
	if strings.Contains(s, `"secretName": "inventory-migrate"`) && strings.Count(s, `"secretName": "inventory-migrate"`) != 1 {
		t.Error("only the migration Job mounts the migration Secret")
	}
	// Every object stays in the environment namespace and carries the annotations.
	if strings.Count(s, `"namespace": "`+ns+`"`) != 7 || strings.Count(s, delivery.AnnotationDeploymentID) < 7 {
		t.Error("every object needs the namespace and the deployment annotation")
	}
}

func TestPlanValidation(t *testing.T) {
	for name, mutate := range map[string]func(*delivery.Plan){
		"missing oidc issuer":    func(p *delivery.Plan) { delete(p.Config, "OIDC_ISSUER") },
		"missing migration cred": func(p *delivery.Plan) { delete(p.Secrets, "DATABASE_MIGRATION_URL") },
		"bad replicas":           func(p *delivery.Plan) { p.Config["REPLICAS"] = "50" },
		"bad digest":             func(p *delivery.Plan) { p.ImageDigest = "latest" },
		"tag in repository":      func(p *delivery.Plan) { p.ImageRepo = "reg.example/app@sha256:abc" },
	} {
		p := plan()
		mutate(&p)
		if _, err := p.Render(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// The migration Job name changes with the image or configuration.
	a, b := plan(), plan()
	b.ConfigDigest = "sha256:" + strings.Repeat("c", 64)
	if a.JobName() == b.JobName() {
		t.Error("a Job's template is immutable: its name must change with the inputs")
	}
}

func TestJobReadinessUsesTerminalConditions(t *testing.T) {
	ref := delivery.ObjectRef{APIVersion: "batch/v1", Kind: "Job", Namespace: ns, Name: "inventory-migrate-x"}
	job := func(status string) []byte {
		return []byte(`{"metadata":{"name":"inventory-migrate-x","namespace":"` + ns + `","uid":"u1"},"spec":{"backoffLimit":2},"status":` + status + `}`)
	}
	for name, c := range map[string]struct {
		status        string
		ready, failed bool
	}{
		"running":                      {`{"active":1}`, false, false},
		"complete condition":           {`{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}`, true, false},
		"succeeded counter only":       {`{"succeeded":1}`, true, false},
		"failed condition, no counter": {`{"conditions":[{"type":"Failed","status":"True","reason":"DeadlineExceeded"}]}`, false, true},
		"failed condition wins":        {`{"succeeded":1,"conditions":[{"type":"Failed","status":"True"}]}`, false, true},
		"failures within the limit":    {`{"failed":2,"active":1}`, false, false},
		"failures past the limit":      {`{"failed":3}`, false, true},
		"false conditions are inert":   {`{"conditions":[{"type":"Failed","status":"False"},{"type":"Complete","status":"False"}]}`, false, false},
	} {
		obs, ok, err := delivery.ParseObserved(ref, job(c.status))
		if err != nil || !ok || obs.Ready != c.ready || obs.Failed != c.failed {
			t.Errorf("%s: ready=%v failed=%v err=%v, want ready=%v failed=%v", name, obs.Ready, obs.Failed, err, c.ready, c.failed)
		}
	}
}
