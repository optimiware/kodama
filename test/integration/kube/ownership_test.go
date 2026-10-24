package kube_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// handmadePod creates a Pod the Job controller did not create. owner, when set,
// is the controlling owner reference it claims.
func (s *stack) handmadePod(ns, name, ownerName, ownerUID string) {
	s.t.Helper()
	meta := map[string]any{"name": name, "namespace": ns}
	if ownerUID != "" {
		meta["ownerReferences"] = []map[string]any{{"apiVersion": "batch/v1", "kind": "Job", "name": ownerName, "uid": ownerUID, "controller": true, "blockOwnerDeletion": true}}
	}
	doc, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": meta, "spec": map[string]any{
		"restartPolicy": "Never", "automountServiceAccountToken": false, "serviceAccountName": "t021-runner",
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"containers": []map[string]any{{
			"name": "c", "image": s.c.workload, "command": []string{"sleep", "300"},
			"securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}},
			"resources":       map[string]any{"limits": map[string]string{"cpu": "50m", "memory": "16Mi", "ephemeral-storage": "16Mi"}, "requests": map[string]string{"cpu": "5m", "memory": "8Mi", "ephemeral-storage": "8Mi"}},
		}},
	}})
	if out, err := runStdin(string(doc), "kubectl", "--kubeconfig", s.c.kubeconfig, "create", "-f", "-"); err != nil {
		s.t.Fatalf("hand-made pod: %v: %s", err, out)
	}
}

// Test_runtime_job_ownership (PX-03, PX-04, IT-07): the runtime's admission denies
// foreign namespaces, unrelated Pods, wrong controlling owners and UID
// substitution, and cancelled admission and refresh, through the real runtimeauth
// service and the executor's Job observer. Labels and workload-supplied names
// never authorize by themselves.
func Test_runtime_job_ownership(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	tenant := executiontest.ScopeA.TenantID
	// The Pod exists but its image never pulls: the credential stays unconsumed
	// until this test presents it.
	run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}, Image: unpullable})
	in := s.claim(tenant)
	must(t, startErr(s.runtime.Start(ctx, *in)))
	var pod struct{ name, uid string }
	eventually(t, 2*time.Minute, "the Job controller's Pod", func() (bool, string) {
		pods := s.podsOf(in)
		if len(pods) != 1 {
			return false, fmt.Sprintf("%d pods", len(pods))
		}
		pod.name, pod.uid = pods[0].Metadata.Name, pods[0].Metadata.UID
		return true, ""
	})
	job, ok := s.jobOf(in)
	if !ok {
		t.Fatal("no Job")
	}
	sec, err := s.c.client.GetSecret(ctx, s.nsA, kube.SecretName(in.WorkloadName))
	must(t, err)
	cred := string(sec.Data[kube.KeyAdmission])

	good := runtimev1.JobIdentity{Name: in.WorkloadName, UID: job.Metadata.UID, PodName: pod.name, PodUID: pod.uid}
	var nonce int
	admit := func(id runtimev1.JobIdentity) (runtimev1.AdmitResponse, error) {
		nonce++
		return s.registry.Admit(ctx, in.AttemptID, runtimev1.AdmitRequest{
			AdmissionCredential: cred, Nonce: fmt.Sprintf("t021-nonce-%04d-abcdefghijklmnop", nonce), ClientVersion: "0.1.0", Job: id,
		})
	}
	mustDeny := func(name string, id runtimev1.JobIdentity) {
		t.Helper()
		resp, err := admit(id)
		if err == nil || resp.Token.Token != "" {
			t.Fatalf("%s: admitted", name)
		}
		if !errors.Is(err, runtimeauth.ErrForbidden) {
			t.Fatalf("%s: %v, want forbidden", name, err)
		}
	}
	// A well-formed UID that differs from the real one in one digit.
	otherUID := func(u string) string {
		if u[0] == '0' {
			return "1" + u[1:]
		}
		return "0" + u[1:]
	}

	t.Run("unrelated and substituted identities are denied", func(t *testing.T) {
		// A Pod in the right namespace with no owner, and one that claims the
		// Job as its controller without being the Job controller's Pod.
		s.handmadePod(s.nsA, "t021-unrelated", "", "")
		s.handmadePod(s.nsA, "t021-forged-owner", in.WorkloadName, job.Metadata.UID)
		unrelated := s.mustKubectl("-n", s.nsA, "get", "pod", "t021-unrelated", "-o", "jsonpath={.metadata.uid}")
		forged := s.mustKubectl("-n", s.nsA, "get", "pod", "t021-forged-owner", "-o", "jsonpath={.metadata.uid}")

		bad := good
		bad.PodName, bad.PodUID = "t021-unrelated", unrelated
		mustDeny("unrelated Pod", bad)
		bad.PodName, bad.PodUID = "t021-forged-owner", forged
		mustDeny("Pod forging the controlling owner", bad)
		bad = good
		bad.PodName = "t021-absent"
		mustDeny("absent Pod", bad)
		bad = good
		bad.PodUID = otherUID(pod.uid)
		mustDeny("Pod UID substitution", bad)
		bad = good
		bad.UID = otherUID(job.Metadata.UID)
		mustDeny("Job UID substitution", bad)
		bad = good
		bad.Name = "kdm-" + strings.Repeat("a", 40)
		mustDeny("another Job name", bad)
		if s.srv.token(in.AttemptID) != "" || s.grantConsumed(in) {
			t.Fatal("a denied admission consumed the credential")
		}
	})

	t.Run("foreign namespaces", func(t *testing.T) {
		// A same-named Pod in another tenant's namespace is not looked up.
		s.handmadePod(s.nsB, pod.name, "", "")
		foreignUID := s.mustKubectl("-n", s.nsB, "get", "pod", pod.name, "-o", "jsonpath={.metadata.uid}")
		bad := good
		bad.PodUID = foreignUID
		mustDeny("Pod of the same name in another tenant namespace", bad)

		// The observer maps the tenant to its namespace itself: an expected
		// namespace supplied by a caller cannot redirect the lookup.
		scope := in.Scope
		ev, err := s.observer.ObserveJob(ctx, runtimeauth.JobObservation{
			Scope: scope, AttemptID: in.AttemptID, Expected: runtimeauth.ExpectedJob{Namespace: s.nsB, Name: in.WorkloadName},
			ClaimedJobName: in.WorkloadName, ClaimedJobUID: good.UID, ClaimedPodName: pod.name, ClaimedPodUID: foreignUID,
		})
		if err != nil || ev.JobUID != "" || ev.PodUID != "" || ev.Namespace != s.nsA {
			t.Fatalf("observation for another namespace: %+v %v", ev, err)
		}
		// Another tenant cannot observe this attempt at all.
		if _, err := s.observer.ObserveJob(ctx, runtimeauth.JobObservation{
			Scope: executiontest.ScopeB, AttemptID: in.AttemptID, Expected: runtimeauth.ExpectedJob{Namespace: s.nsB, Name: in.WorkloadName},
			ClaimedJobName: in.WorkloadName, ClaimedJobUID: good.UID, ClaimedPodName: pod.name, ClaimedPodUID: pod.uid,
		}); err == nil {
			t.Fatal("tenant B observed tenant A's attempt")
		}
	})

	t.Run("the real identity is admitted, then cancellation ends admission and refresh", func(t *testing.T) {
		resp, err := admit(good)
		if err != nil {
			t.Fatalf("admission of the real Pod: %v", err)
		}
		token := resp.Token.Token
		if _, err := s.registry.Renew(ctx, in.AttemptID, token); err != nil {
			t.Fatalf("refresh before cancellation: %v", err)
		}
		if _, err := s.svc.Cancel(ctx, s.caller(), run.Scope, run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.registry.Renew(ctx, in.AttemptID, token); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
			t.Fatalf("refresh after cancellation: %v", err)
		}
		if _, err := s.registry.Admit(ctx, in.AttemptID, runtimev1.AdmitRequest{
			AdmissionCredential: cred, Nonce: "t021-nonce-replay-abcdefghijklmnop", ClientVersion: "0.1.0", Job: good,
		}); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
			t.Fatalf("admission after cancellation: %v", err)
		}
		// result.submit deliberately stays authorized on a terminal attempt so an
		// accepted-result receipt can be replayed; every other action ends.
		if _, err := s.registry.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: in.AttemptID, Bearer: token, Action: runtimeauth.ActionContextRead}); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
			t.Fatalf("authorization after cancellation: %v", err)
		}
	})
}

// grantConsumed reports whether the attempt's admission credential was consumed.
func (s *stack) grantConsumed(in *execution.AttemptSpec) bool {
	var n int
	must(s.t, s.e.Migration.QueryRow(context.Background(),
		`SELECT count(*) FROM runtime_grants WHERE attempt_id = $1 AND consumed_at IS NOT NULL`, string(in.AttemptID)).Scan(&n))
	return n > 0
}
