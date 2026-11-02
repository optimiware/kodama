package gitworker_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/gitworker"
)

func mustRender(t *testing.T, tenant domain.TenantID, spec gitworker.WorkerSpec) gitworker.Objects {
	t.Helper()
	o, err := gitworker.Render(tenant, spec)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return o
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Test_unit_gitworker_render_per_tenant (T-129, unit_gitworker_render_per_tenant,
// GR-03, IT-17): every tenant gets its own namespace, labels, names and
// worker configuration, the namespace enforces the restricted Pod Security
// profile at a pinned version, and an unusable specification renders nothing.
func Test_unit_gitworker_render_per_tenant(t *testing.T) {
	spec := testSpec()
	a, b := domain.TenantID("t_Acme"), domain.TenantID("t_Beta")
	oa, ob := mustRender(t, a, spec), mustRender(t, b, spec)

	t.Run("a namespace per tenant", func(t *testing.T) {
		if oa.Namespace.Metadata.Name == ob.Namespace.Metadata.Name {
			t.Fatal("two tenants share a namespace")
		}
		if want := "kodama-git-" + gitworker.TenantKey(a); oa.Namespace.Metadata.Name != want || gitworker.NamespaceOf(a) != want {
			t.Fatalf("namespace %q, want %q", oa.Namespace.Metadata.Name, want)
		}
		if len(oa.Namespace.Metadata.Name) > 63 {
			t.Fatal("the namespace name exceeds the DNS label bound")
		}
		for _, o := range []gitworker.Objects{oa, ob} {
			ns := o.Namespace.Metadata.Name
			for _, m := range []struct{ name, ns string }{
				{o.ServiceAccount.Metadata.Name, o.ServiceAccount.Metadata.Namespace}, {o.Claim.Metadata.Name, o.Claim.Metadata.Namespace},
				{o.Secret.Metadata.Name, o.Secret.Metadata.Namespace}, {o.Service.Metadata.Name, o.Service.Metadata.Namespace},
				{o.Deployment.Metadata.Name, o.Deployment.Metadata.Namespace},
			} {
				if m.ns != ns {
					t.Fatalf("%s is in %q, not in the tenant's namespace %q", m.name, m.ns, ns)
				}
			}
			for _, p := range o.Policies {
				if p.Metadata.Namespace != ns {
					t.Fatalf("policy %s is not in the tenant's namespace", p.Metadata.Name)
				}
			}
		}
	})

	t.Run("restricted Pod Security at a pinned version", func(t *testing.T) {
		l := oa.Namespace.Metadata.Labels
		for _, mode := range []string{"enforce", "audit", "warn"} {
			if l["pod-security.kubernetes.io/"+mode] != "restricted" || l["pod-security.kubernetes.io/"+mode+"-version"] != "v1.32" {
				t.Fatalf("%s is not restricted at v1.32: %v", mode, l)
			}
		}
	})

	t.Run("tenant identity of every object", func(t *testing.T) {
		for _, c := range []struct {
			tenant domain.TenantID
			o      gitworker.Objects
		}{{a, oa}, {b, ob}} {
			metas := []map[string]string{
				c.o.Namespace.Metadata.Annotations, c.o.ServiceAccount.Metadata.Annotations, c.o.Claim.Metadata.Annotations,
				c.o.Secret.Metadata.Annotations, c.o.Service.Metadata.Annotations, c.o.Deployment.Metadata.Annotations,
			}
			for _, p := range c.o.Policies {
				metas = append(metas, p.Metadata.Annotations)
			}
			for _, an := range metas {
				if an[gitworker.AnnTenant] != string(c.tenant) || !strings.HasPrefix(an[gitworker.AnnFingerprint], "sha256:") {
					t.Fatalf("an object of %s lacks its tenant or fingerprint: %v", c.tenant, an)
				}
			}
			var env string
			for _, e := range c.o.Deployment.Spec.Template.Spec.Containers[0].Env {
				if e.Name == gitworker.EnvTenantID {
					env = e.Value
				}
			}
			if env != string(c.tenant) {
				t.Fatalf("the worker of %s is configured for %q", c.tenant, env)
			}
		}
		if oa.SpecFingerprint() == ob.SpecFingerprint() || oa.Deployment.Metadata.Annotations[gitworker.AnnFingerprint] == ob.Deployment.Metadata.Annotations[gitworker.AnnFingerprint] {
			t.Fatal("two tenants share a fingerprint")
		}
		if again := mustRender(t, a, spec); marshal(t, again) != marshal(t, oa) {
			t.Fatal("rendering is not deterministic")
		}
	})

	t.Run("the worker command and environment", func(t *testing.T) {
		c := oa.Deployment.Spec.Template.Spec.Containers[0]
		if len(c.Command) != 1 || c.Command[0] != gitworker.DefaultCommand || strings.Join(c.Args, " ") != "git serve" {
			t.Fatalf("command %v %v", c.Command, c.Args)
		}
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		for k, want := range map[string]string{
			gitworker.EnvDataRoot: "/data", gitworker.EnvListen: ":8443", gitworker.EnvControlURL: spec.ControlPlane.URL,
			gitworker.EnvTokenFile: "/var/run/kodama/identity/token", gitworker.EnvPublicKeys: "/var/run/kodama/identity/public-keys",
			gitworker.EnvCAFile: "/var/run/kodama/identity/ca.crt",
		} {
			if env[k] != want {
				t.Fatalf("%s = %q, want %q", k, env[k], want)
			}
		}
	})

	t.Run("an unusable specification renders nothing", func(t *testing.T) {
		mutate := map[string]func(*gitworker.WorkerSpec){
			"image without a digest":          func(s *gitworker.WorkerSpec) { s.Image = "registry.example/kodama:latest" },
			"tag-only image":                  func(s *gitworker.WorkerSpec) { s.Image = "registry.example/kodama:1.0" },
			"unpinned Pod Security":           func(s *gitworker.WorkerSpec) { s.PodSecurityVersion = "latest" },
			"no storage class":                func(s *gitworker.WorkerSpec) { s.Storage.Class = "" },
			"zero storage":                    func(s *gitworker.WorkerSpec) { s.Storage.Size = "0" },
			"request above limit":             func(s *gitworker.WorkerSpec) { s.Resources.CPURequest = "2" },
			"missing memory limit":            func(s *gitworker.WorkerSpec) { s.Resources.MemoryLimit = "" },
			"privileged port":                 func(s *gitworker.WorkerSpec) { s.ListenPort = 443 },
			"root":                            func(s *gitworker.WorkerSpec) { s.RunAsUser = -1 },
			"no control-plane selector":       func(s *gitworker.WorkerSpec) { s.ControlPlane.PodLabels = nil },
			"control-plane URL with a path":   func(s *gitworker.WorkerSpec) { s.ControlPlane.URL = "https://kodama.kodama.svc/x" },
			"control-plane URL credentials":   func(s *gitworker.WorkerSpec) { s.ControlPlane.URL = "https://u:p@kodama.kodama.svc" },
			"control-plane namespace invalid": func(s *gitworker.WorkerSpec) { s.ControlPlane.Namespace = "Kodama_NS" },
			"unknown scheme":                  func(s *gitworker.WorkerSpec) { s.Scheme = "ftp" },
		}
		for name, m := range mutate {
			s := testSpec()
			m(&s)
			if _, err := gitworker.Render(a, s); err == nil || !isErr(err, gitworker.ErrInvalidSpec) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := gitworker.Render("", spec); !isErr(err, gitworker.ErrInvalidSpec) {
			t.Errorf("empty tenant: %v", err)
		}
		if _, err := gitworker.Render("bad tenant!", spec); !isErr(err, gitworker.ErrInvalidSpec) {
			t.Errorf("malformed tenant: %v", err)
		}
	})
}

// Test_unit_gitworker_render_golden: the rendered objects of two specifications
// are byte for byte the committed golden files. They change only with a
// deliberate change of the rendering (and then every recorded fingerprint
// changes, which replaces the Deployments).
func Test_unit_gitworker_render_golden(t *testing.T) {
	custom := testSpec()
	custom.Scheme = "http"
	custom.ListenPort = 9000
	custom.Command, custom.Args = []string{"/bin/worker"}, []string{"--serve"}
	custom.Storage = gitworker.Storage{Class: "retained-rwop", Size: "100Gi"}
	custom.RunAsUser, custom.RunAsGroup = 10001, 10002
	custom.ControlPlane.PodLabels = map[string]string{"app": "server", "tier": "control"}
	for name, c := range map[string]struct {
		tenant domain.TenantID
		spec   gitworker.WorkerSpec
	}{
		"default": {"t_acme", testSpec()},
		"custom":  {"t_Beta-2", custom},
	} {
		t.Run(name, func(t *testing.T) {
			o := mustRender(t, c.tenant, c.spec)
			doc := map[string]any{
				"namespace": o.Namespace, "serviceAccount": o.ServiceAccount, "claim": o.Claim, "secret": o.Secret,
				"service": o.Service, "policies": o.Policies, "deployment": o.Deployment,
				"specFingerprint": o.SpecFingerprint(), "endpoint": gitworker.EndpointURL(c.tenant, c.spec),
			}
			got := []byte(marshal(t, doc) + "\n")
			path := filepath.Join("testdata", name+".golden.json")
			if os.Getenv("KODAMA_UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("the rendering changed (%s):\n%s", path, got)
			}
		})
	}
}
