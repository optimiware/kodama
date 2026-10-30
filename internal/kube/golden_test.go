package kube_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

// goldenCA is a fixed throwaway CA certificate (public trust material only): the
// runtime CA is part of the fingerprint, so a golden fingerprint needs a fixed one.
const goldenCA = `-----BEGIN CERTIFICATE-----
MIIBYDCCAQWgAwIBAgIBATAKBggqhkjOPQQDAjAXMRUwEwYDVQQDEwx0MDIxIHRl
c3QgY2EwHhcNMjYxMDI5MjAzNzE0WhcNMjYxMDMwMjEzNzE0WjAXMRUwEwYDVQQD
Ewx0MDIxIHRlc3QgY2EwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAARiW35W2K31
KqGGge+Ojv0ElmiLiQyCdVGdMvE4K7k4EKtH8XZXgEFRO3rs5SGJDjZ1wpiPykH7
dLkCYwziLCHKo0IwQDAOBgNVHQ8BAf8EBAMCAgQwDwYDVR0TAQH/BAUwAwEB/zAd
BgNVHQ4EFgQUnUvcM+fnvlLK3w2PN76S1trLKEwwCgYIKoZIzj0EAwIDSQAwRgIh
AOvxiU1IKym74JyUyY9rIvQSDjNtpw91RTcpdigu5DGYAiEAmT9KQu45FRotjnEp
hAK6vp7Q0bkmtMefHRes8Nq2e4Q=
-----END CERTIFICATE-----
`

// Test_unit_kube_attempt_private_job_golden: the attempt_private Job of a
// source-free, a reader and a writer attempt, and the spec fingerprint it binds
// (an annotation of the Job), are byte for byte what they were before the
// run_shared model existed. The golden files under testdata were generated from the
// commit that preceded T-114 (7d19cb7b); they change only with a deliberate change
// of the attempt_private recipe (and then every persisted fingerprint changes).
func Test_unit_kube_attempt_private_job_golden(t *testing.T) {
	cfg := kubetest.PolicyConfig(t, "acme")
	cfg.RuntimeCA = []byte(goldenCA)
	pol, err := kube.NewExecutionPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tid, _ := kubetest.Tenant("acme")
	for name, access := range map[string]runtimev1.WorkspaceAccess{
		"source_free": runtimev1.AccessNone, "reader": runtimev1.AccessRead, "writer": runtimev1.AccessWrite,
	} {
		t.Run(name, func(t *testing.T) {
			c := kubetest.Context(t, kubetest.ContextSpec{
				Tenant: tid, Project: "p_inv", Run: "run_1", Step: "build", Attempt: "att_1", Image: kubetest.WorkloadImage, Access: access,
			})
			rec := kubetest.Record(t, pol, c)
			exp, err := pol.Expect(rec)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(exp.Job, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", "attempt_private_job_"+name+".golden.json")
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
				t.Fatalf("the attempt_private Job changed (%s):\n%s", path, got)
			}
			if exp.Job.Metadata.Annotations[kube.AnnFingerprint] != rec.SpecFingerprint {
				t.Fatal("the Job does not carry the committed fingerprint")
			}
		})
	}
}
