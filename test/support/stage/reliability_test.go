package stage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cleanupTB struct {
	testing.TB
	callbacks   []func()
	diagnostics []string
}

func (t *cleanupTB) Cleanup(f func())                  { t.callbacks = append(t.callbacks, f) }
func (t *cleanupTB) Fatalf(format string, args ...any) { panic(fmt.Sprintf(format, args...)) }
func (t *cleanupTB) Fatal(args ...any)                 { panic(fmt.Sprint(args...)) }
func (t *cleanupTB) Errorf(format string, args ...any) {
	t.diagnostics = append(t.diagnostics, fmt.Sprintf(format, args...))
}

func TestFaultRestorationRegisteredBeforeMutation(t *testing.T) {
	for _, fault := range []string{"relay", "server"} {
		t.Run(fault, func(t *testing.T) {
			tb := &cleanupTB{TB: t}
			var calls []string
			i := &Installation{T: tb, S: &Session{Namespace: "kodama-q-fixture", Release: "kodama", RelayService: "relay"}, Kube: Kube{run: func(ctx context.Context, _ []byte, args ...string) (string, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("unbounded command")
				}
				call := strings.Join(args, " ")
				calls = append(calls, call)
				if strings.Contains(call, "--replicas=0") {
					return "", fmt.Errorf("injected mutation failure")
				}
				return "", nil
			}}}
			func() {
				defer func() {
					if recover() == nil {
						t.Error("fault did not fail")
					}
				}()
				if fault == "relay" {
					i.RelayDown(context.Background())
				} else {
					i.Stop(context.Background())
				}
			}()
			if len(tb.callbacks) != 1 {
				t.Fatalf("cleanup registered too late: %d", len(tb.callbacks))
			}
			tb.callbacks[0]()
			if len(calls) < 2 || !strings.Contains(calls[1], "--replicas=1") {
				t.Fatalf("restoration not attempted after failure: %v", calls)
			}
			if len(tb.diagnostics) != 0 {
				t.Fatalf("restoration failed: %v", tb.diagnostics)
			}
		})
	}
}

func TestClusterFailureCannotProveAbsence(t *testing.T) {
	i := &Installation{T: t, Kube: Kube{run: func(context.Context, []byte, ...string) (string, error) { return "", fmt.Errorf("unreachable") }}}
	jobs, err := i.TryJobsOfAttempt("kodama-q-fixture-a", "att-fixture")
	if err == nil || jobs != nil {
		t.Fatal("unreachable cluster was treated as an empty list")
	}
}

func TestSessionBindsHandleContractAndValues(t *testing.T) {
	for _, kind := range []string{"valid", "wrong_namespace", "wrong_name", "wrong_values"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "qualification", "fixture")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			s := Session{Name: "fixture", Release: "kodama", Namespace: "kodama-q-fixture", InstallationID: "installation", URL: "https://fixture.invalid", Tenants: []Tenant{{Namespace: "kodama-q-fixture-a", TenantID: "t_a"}, {Namespace: "kodama-q-fixture-b", TenantID: "t_b"}}}
			if kind == "wrong_namespace" {
				s.Namespace = "kodama-system"
			}
			if kind == "wrong_name" {
				s.Name = "other"
			}
			handle, _ := json.Marshal(s)
			contract, _ := json.Marshal(map[string]any{"installation_id": s.InstallationID, "network": map[string]string{"runtime_url": s.URL}, "tenants": s.Tenants})
			for name, body := range map[string][]byte{"session.json": handle, "contract.json": contract, "values.yaml": []byte("fixture: true\n")} {
				if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			values := filepath.Join(dir, "values.yaml")
			if kind == "wrong_values" {
				values = filepath.Join(root, "other-values.yaml")
				if err := os.WriteFile(values, []byte("different: true\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv(EnvSession, "fixture")
			t.Setenv(EnvStateDir, root)
			t.Setenv(EnvKubeconfig, filepath.Join(root, "unused-kubeconfig"))
			t.Setenv("KODAMA_STAGE_CONTRACT", filepath.Join(dir, "contract.json"))
			t.Setenv("KODAMA_STAGE_VALUES", values)
			tb := &cleanupTB{TB: t}
			var failure any
			func() { defer func() { failure = recover() }(); LoadSession(tb) }()
			if (failure != nil) != (kind != "valid") {
				t.Fatalf("session validation kind=%s failure=%v", kind, failure)
			}
		})
	}
}

func TestCommandErrorsDoNotExposeCredentials(t *testing.T) {
	for _, text := range []string{"Forbidden token=fixture-secret", "NotFound: manifest contains fixture-secret", "provider body fixture-secret", "qualification hold fixture-secret"} {
		err := commandError("kubectl", fmt.Errorf("exit status 1"), text)
		if strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "manifest") || strings.Contains(err.Error(), "provider body") {
			t.Fatalf("sensitive diagnostics: %s", err)
		}
	}
}

func TestCommandTimeoutBoundsCallsWithoutTruncatingRollouts(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want time.Duration
	}{
		{[]string{"get", "jobs"}, 45 * time.Second},
		{[]string{"rollout", "status", "deploy/kodama", "--timeout=6m"}, 6*time.Minute + 15*time.Second},
		{[]string{"upgrade", "--timeout", "8m"}, 8*time.Minute + 15*time.Second},
	} {
		if got := commandTimeout(tc.args); got != tc.want {
			t.Fatalf("timeout %v = %s, want %s", tc.args, got, tc.want)
		}
	}
}

func TestEventFieldsMustMatchOneLine(t *testing.T) {
	fields := map[string]string{"attempt": "att-a", "error": "conflict"}
	if HasEvent("{\"attempt\":\"att-a\"}\n{\"error\":\"conflict\"}", fields) {
		t.Fatal("matched unrelated events")
	}
	if !HasEvent("not json\n{\"attempt\":\"att-a\",\"error\":\"conflict\"}", fields) {
		t.Fatal("missed exact event")
	}
}

func TestLoopbackCertificateTrustAndScope(t *testing.T) {
	cert, key, err := LoopbackCertificate()
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "127.0.0.1"}); err != nil {
		t.Fatal("loopback certificate not trusted", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "kubernetes.default.svc"}); err == nil {
		t.Fatal("loopback certificate trusted for real cluster")
	}
}
