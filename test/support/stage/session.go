// Package stage is the fixture of the qualification suites that run against an installation on the stage
// cluster (test/e2e/kube-full, test/e2e/staging and the Kubernetes backend of test/integration/assembly-execution).
//
// The test process runs on the operator's machine. It reaches the cluster through kubectl and the installation
// through its public HTTPS address; the private PostgreSQL instance is reached through a relay Pod and a port
// forward (Installation.DB). The installation under test is a session prepared by deploy/stage/scripts/qualify.sh:
// an isolated database, namespace, keys, Secrets and NATS server of the stage, with the Kodama release installed
// from the committed chart by the tests themselves.
//
// The fixture never weakens what it tests: the server is the real chart's `kodama server --assembly core` under
// KODAMA_ENV=production, and the only test-owned parts on its data path are the database relay (scaled to zero to
// cause an outage) and admission policies that hold back one named Kubernetes operation.
package stage

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tenant is one tenant of the session and the namespace its workloads run in.
type Tenant struct {
	Namespace string `json:"namespace"`
	TenantID  string `json:"tenant_id"`
}

// Session is the handle written by `qualify.sh prepare` (nonsecret).
type Session struct {
	Name           string   `json:"name"`
	InstallationID string   `json:"installation_id"`
	Namespace      string   `json:"namespace"`
	Release        string   `json:"release"`
	Host           string   `json:"host"`
	URL            string   `json:"url"`
	Audience       string   `json:"audience"`
	Issuer         string   `json:"issuer"`
	Database       string   `json:"database"`
	Tenants        []Tenant `json:"tenants"`
	NATSService    string   `json:"nats_service"`
	ProbeService   string   `json:"probe_service"`
	RelayService   string   `json:"relay_service"`
	ProbeSecret    string   `json:"probe_secret"`
	GitOrigin      string   `json:"git_origin"`

	// Dir is the session's state directory (handle, derived contract, Helm values).
	Dir string `json:"-"`
	// ChartDir is the committed chart that is installed.
	ChartDir string `json:"-"`
	// Kubeconfig is the operator kubeconfig of the stage cluster.
	Kubeconfig string `json:"-"`
}

// TenantA and TenantB are the session's two tenants.
func (s *Session) TenantA() Tenant { return s.Tenants[0] }
func (s *Session) TenantB() Tenant { return s.Tenants[1] }

// ValuesFile is the Helm values file of the session's release.
func (s *Session) ValuesFile() string { return filepath.Join(s.Dir, "values.yaml") }

// Environment names the qualification inputs.
const (
	EnvSession    = "KODAMA_QUAL_SESSION"
	EnvStateDir   = "KODAMA_QUAL_STATE_DIR"
	EnvKubeconfig = "KODAMA_TEST_KUBECONFIG"
)

// LoadSession reads the prepared session named by KODAMA_QUAL_SESSION. A missing session or cluster is a failure,
// never a skip: the acceptance cannot pass without the cluster it names.
func LoadSession(t testing.TB) *Session {
	t.Helper()
	name := os.Getenv(EnvSession)
	if name == "" {
		t.Fatalf("%s is not set: prepare a session with deploy/stage/scripts/qualify.sh and name it", EnvSession)
	}
	kc := os.Getenv(EnvKubeconfig)
	if kc == "" {
		kc = os.Getenv("KUBECONFIG")
	}
	if kc == "" {
		t.Fatalf("%s (or KUBECONFIG) is not set: the stage cluster's kubeconfig is required", EnvKubeconfig)
	}
	state := os.Getenv(EnvStateDir)
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		state = filepath.Join(home, ".kube", "kodama-stage", "state")
	}
	dir := filepath.Join(state, "qualification", name)
	data, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatalf("session %q is not prepared (%v): run qualify.sh prepare --session %s", name, err, name)
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil || len(s.Tenants) != 2 || s.Namespace == "" {
		t.Fatalf("session %q handle is invalid: %v", name, err)
	}
	s.Dir, s.Kubeconfig = dir, kc
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatal(err)
	}
	s.ChartDir = filepath.Join(filepath.Dir(strings.TrimSpace(string(out))), "deploy", "helm", "kodama")
	return &s
}

func (s *Session) String() string { return fmt.Sprintf("session %s (%s)", s.Name, s.URL) }
