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
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// specOf returns the part of a live Job a foreign party could not change without
// it being visible: UID, spec and annotations.
func specOf(t *testing.T, j kubernetes.Job) string {
	t.Helper()
	var doc map[string]any
	must(t, json.Unmarshal(j.Raw, &doc))
	b, _ := json.Marshal(map[string]any{"uid": j.Metadata.UID, "spec": doc["spec"], "annotations": j.Metadata.Annotations, "labels": j.Metadata.Labels})
	return string(b)
}

// Test_foreign_job_collision (PX-03, IT-08): a same-name Job with foreign
// ownership, a changed spec, a forged fingerprint annotation or a conflicting UID
// is rejected: never adopted, never mutated or deleted, and its Pods are never
// admitted.
func Test_foreign_job_collision(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	tenant := executiontest.ScopeA.TenantID

	// build returns a hostile same-name Job derived from the attempt's own recipe.
	type variant struct {
		name string
		edit func(j *kubernetes.Job, owner string)
	}
	variants := []variant{
		{"changed image keeps the forged fingerprint", func(j *kubernetes.Job, _ string) {
			j.Spec.Template.Spec.Containers[0].Image = s.c.injector
		}},
		{"changed command", func(j *kubernetes.Job, _ string) {
			j.Spec.Template.Spec.Containers[0].Command = []string{"/bin/sh", "-c", "sleep 600"}
			j.Spec.Template.Spec.Containers[0].Args = nil
		}},
		{"extra sidecar", func(j *kubernetes.Job, _ string) {
			c := j.Spec.Template.Spec.Containers[0]
			c.Name = "sidecar"
			j.Spec.Template.Spec.Containers = append(j.Spec.Template.Spec.Containers, c)
		}},
		{"foreign annotations and no fingerprint", func(j *kubernetes.Job, _ string) {
			j.Metadata.Annotations = map[string]string{"someone": "else"}
			j.Spec.Template.Spec.Containers[0].Command = []string{"sleep", "600"}
			j.Spec.Template.Spec.Containers[0].Args = nil
		}},
		{"foreign owner", func(j *kubernetes.Job, owner string) {
			yes := true
			j.Metadata.OwnerReferences = []kubernetes.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "t021-owner", UID: owner, Controller: &yes}}
			j.Spec.Template.Spec.Containers[0].Command = []string{"sleep", "600"}
			j.Spec.Template.Spec.Containers[0].Args = nil
		}},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}})
			_ = run
			in := s.claim(tenant)
			ns := s.ns(tenant)
			rec, err := s.attempts.Attempt(ctx, in.Scope, in.AttemptID)
			must(t, err)
			exp, err := s.policy.Expect(rec)
			must(t, err)
			owner := s.ownerUID(ns)
			var hostile kubernetes.Job
			raw, _ := json.Marshal(exp.Job)
			must(t, json.Unmarshal(raw, &hostile))
			v.edit(&hostile, owner)
			before, err := s.c.client.CreateJob(ctx, ns, hostile)
			must(t, err)

			_, err = s.runtime.Start(ctx, *in)
			if !errors.Is(err, kube.ErrForeignWorkload) {
				t.Fatalf("got %v", err)
			}
			after, ok := s.jobOf(in)
			if !ok {
				t.Fatal("the foreign Job was deleted")
			}
			if specOf(t, before) != specOf(t, after) {
				t.Fatal("the foreign Job was modified")
			}
			if s.secretExists(in) || s.grants(in) != 0 {
				t.Fatal("a credential was created or registered for a collision")
			}
			// Its Pods are not admitted: the observer reveals no identity for them.
			for _, p := range s.podsOf(in) {
				ev, err := s.observer.ObserveJob(ctx, runtimeauth.JobObservation{
					Scope: in.Scope, AttemptID: in.AttemptID, Expected: runtimeauth.ExpectedJob{Namespace: ns, Name: in.WorkloadName},
					ClaimedJobName: in.WorkloadName, ClaimedPodName: p.Metadata.Name, ClaimedPodUID: p.Metadata.UID,
				})
				if err != nil || ev.JobUID != "" || ev.PodOwnerUID != "" {
					t.Fatalf("admission evidence for a foreign Pod: %+v %v", ev, err)
				}
			}
			if n := s.admitCount(in); n != 0 {
				t.Fatalf("%d admissions reached the runtime for a foreign Job", n)
			}
			// Termination does not delete it either.
			res, err := s.runtime.Stop(ctx, in.Workload(""))
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := s.jobOf(in); !ok {
				t.Fatal("termination deleted a foreign Job")
			}
			_ = res
			if _, err := s.c.client.GetJob(ctx, ns, in.WorkloadName); err != nil {
				t.Fatal(err)
			}
			s.mustKubectl("-n", ns, "delete", "job", in.WorkloadName, "--wait=false", "--cascade=background")
		})
	}

	t.Run("conflicting UID after adoption", func(t *testing.T) {
		run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: slow(120), Outputs: []string{"v"}})
		_ = run
		in := s.claim(tenant)
		ns := s.ns(tenant)
		if _, err := s.runtime.Start(ctx, *in); err != nil {
			t.Fatal(err)
		}
		s.observeDispatched(in)
		orig, _ := s.jobOf(in)
		// Replace the Job with an identical one: same name, same spec, new UID.
		s.mustKubectl("-n", ns, "delete", "job", in.WorkloadName, "--wait=true", "--cascade=foreground")
		rec, _ := s.attempts.Attempt(ctx, in.Scope, in.AttemptID)
		exp, _ := s.policy.Expect(rec)
		repl, err := s.c.client.CreateJob(ctx, ns, exp.Job)
		must(t, err)
		if repl.Metadata.UID == orig.Metadata.UID {
			t.Fatal("UID did not change")
		}
		before := specOf(t, repl)
		_, err = s.newRuntime(s.c.client).Start(ctx, *in)
		if !errors.Is(err, kube.ErrForeignWorkload) {
			t.Fatalf("UID substitution: %v", err)
		}
		after, ok := s.jobOf(in)
		if !ok || specOf(t, after) != before {
			t.Fatal("the replacement was touched")
		}
		// The recorded UID is the original one and termination leaves the replacement.
		sec, err := s.c.client.GetSecret(ctx, ns, kube.SecretName(in.WorkloadName))
		must(t, err)
		if !strings.Contains(sec.Metadata.Annotations[kube.AnnWorkload], orig.Metadata.UID) {
			t.Fatal("the confirmed UID changed")
		}
		if err := s.svc.ObserveAttempt(ctx, in.Scope, in.AttemptID, execution.Observation{Kind: execution.ObservedDispatched, WorkloadName: in.WorkloadName, SpecFingerprint: in.SpecFingerprint, WorkloadUID: ""}); err != nil {
			t.Fatal(err)
		}
		// Reconciliation never observes the replacement as the attempt's workload,
		// even once its Pod has started or ended: that status is not applied to
		// the attempt.
		atBefore, _ := s.attempt(in)
		if atBefore.WorkloadUID != "" || atBefore.StartedAt != nil {
			t.Fatalf("the attempt already holds a start: uid %q started %v", atBefore.WorkloadUID, atBefore.StartedAt)
		}
		eventually(t, 2*time.Minute, "the replacement's Pod to start or end", func() (bool, string) {
			for _, p := range s.podsOf(in) {
				for _, c := range p.Status.ContainerStatuses {
					if c.Name == kube.PrimaryContainer && (c.State.Running != nil || c.State.Terminated != nil) {
						return true, ""
					}
				}
				if p.Status.Phase == "Failed" || p.Status.Phase == "Succeeded" {
					return true, ""
				}
			}
			return false, "the replacement's Pod is still starting"
		})
		for i := 0; i < 2; i++ {
			s.tick()
		}
		atAfter, _ := s.attempt(in)
		if atAfter.Status != atBefore.Status || atAfter.Reason != atBefore.Reason || atAfter.Class != atBefore.Class ||
			atAfter.WorkloadUID != "" || atAfter.StartedAt != nil {
			t.Fatalf("the replacement's status reached the attempt: %s/%s uid %q started %v -> %s/%s uid %q started %v",
				atBefore.Status, atBefore.Reason, atBefore.WorkloadUID, atBefore.StartedAt,
				atAfter.Status, atAfter.Reason, atAfter.WorkloadUID, atAfter.StartedAt)
		}
		if _, ok := s.jobOf(in); !ok {
			t.Fatal("reconciliation deleted a Job it could not verify")
		}
	})

	t.Run("conflicting UID before first admission", func(t *testing.T) {
		// The Pod never pulls its image, so the credential stays unconsumed until
		// this test presents it.
		_, _ = s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}, Image: unpullable})
		in := s.claim(tenant)
		ns := s.ns(tenant)
		must(t, startErr(s.runtime.Start(ctx, *in)))
		orig, ok := s.jobOf(in)
		if !ok {
			t.Fatal("no Job")
		}
		sec, err := s.c.client.GetSecret(ctx, ns, kube.SecretName(in.WorkloadName))
		must(t, err)
		cred := string(sec.Data[kube.KeyAdmission])
		if !strings.Contains(sec.Metadata.Annotations[kube.AnnWorkload], orig.Metadata.UID) {
			t.Fatal("the original UID was not confirmed")
		}
		// Replace the confirmed Job with an identical one: same name, same spec,
		// new UID.
		s.mustKubectl("-n", ns, "delete", "job", in.WorkloadName, "--wait=true", "--cascade=foreground")
		rec, err := s.attempts.Attempt(ctx, in.Scope, in.AttemptID)
		must(t, err)
		exp, err := s.policy.Expect(rec)
		must(t, err)
		repl, err := s.c.client.CreateJob(ctx, ns, exp.Job)
		must(t, err)
		if repl.Metadata.UID == orig.Metadata.UID {
			t.Fatal("UID did not change")
		}
		var pod kubernetes.Pod
		eventually(t, 2*time.Minute, "the replacement's Pod", func() (bool, string) {
			pods := s.podsOf(in)
			if len(pods) != 1 {
				return false, fmt.Sprintf("%d pods", len(pods))
			}
			pod = pods[0]
			return true, ""
		})
		// The observer gives no admission evidence for the replacement...
		ev, err := s.observer.ObserveJob(ctx, runtimeauth.JobObservation{
			Scope: in.Scope, AttemptID: in.AttemptID, Expected: runtimeauth.ExpectedJob{Namespace: ns, Name: in.WorkloadName},
			ClaimedJobName: in.WorkloadName, ClaimedJobUID: repl.Metadata.UID, ClaimedPodName: pod.Metadata.Name, ClaimedPodUID: pod.Metadata.UID,
		})
		if err != nil || ev.JobUID != "" || ev.PodOwnerUID != "" {
			t.Fatalf("admission evidence for a replacement Job: %+v %v", ev, err)
		}
		// ...so the real runtimeauth denies its Pod and keeps the credential.
		resp, err := s.registry.Admit(ctx, in.AttemptID, runtimev1.AdmitRequest{
			AdmissionCredential: cred, Nonce: "t021-nonce-replacement-abcdefghijkl", ClientVersion: "0.1.0",
			Job: runtimev1.JobIdentity{Name: in.WorkloadName, UID: repl.Metadata.UID, PodName: pod.Metadata.Name, PodUID: pod.Metadata.UID},
		})
		if err == nil || resp.Token.Token != "" || !errors.Is(err, runtimeauth.ErrForbidden) {
			t.Fatalf("the replacement's Pod was not denied: %v", err)
		}
		if s.grantConsumed(in) || s.srv.token(in.AttemptID) != "" {
			t.Fatal("a denied admission consumed the credential")
		}
		s.mustKubectl("-n", ns, "delete", "job", in.WorkloadName, "--wait=false", "--cascade=background")
	})
}
