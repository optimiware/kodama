package kube_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

func (e *env) observer(t *testing.T) *kube.JobObserver {
	t.Helper()
	o, err := kube.NewJobObserver(kube.Dependencies{Policy: e.policy, Cluster: e.fake, Attempts: e.attempts}, kube.Options{Clock: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (e *env) observation(pod kubernetes.Pod) runtimeauth.JobObservation {
	return runtimeauth.JobObservation{
		Scope: e.rec.Scope, AttemptID: e.rec.AttemptID, Expected: runtimeauth.ExpectedJob{Namespace: e.ns(), Name: e.rec.JobName},
		ClaimedJobName: e.rec.JobName, ClaimedPodName: pod.Metadata.Name, ClaimedPodUID: pod.Metadata.UID,
	}
}

// Test_runtime_job_ownership_unit: runtime admission's observation denies foreign
// namespaces, unrelated Pods, wrong controlling owners and UID substitution.
func Test_runtime_job_ownership_unit(t *testing.T) {
	ctx := context.Background()
	dispatched := func(t *testing.T) (*env, kubernetes.Pod) {
		e := newEnv(t)
		if _, err := e.runtime(t).Start(ctx, e.intent()); err != nil {
			t.Fatal(err)
		}
		return e, e.started(t)
	}
	t.Run("matching Job and Pod", func(t *testing.T) {
		e, pod := dispatched(t)
		ev, err := e.observer(t).ObserveJob(ctx, e.observation(pod))
		if err != nil {
			t.Fatal(err)
		}
		job := e.mustJob(t)
		want := runtimeauth.JobEvidence{
			Namespace: e.ns(), JobName: e.rec.JobName, JobUID: job.Metadata.UID,
			PodNamespace: e.ns(), PodName: pod.Metadata.Name, PodUID: pod.Metadata.UID, PodOwnerUID: job.Metadata.UID,
		}
		if ev != want {
			t.Fatalf("%+v != %+v", ev, want)
		}
	})
	t.Run("definite absence is ErrNotObserved", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.observer(t).ObserveJob(ctx, e.observation(kubernetes.Pod{Metadata: kubernetes.ObjectMeta{Name: "p"}}))
		if !errors.Is(err, runtimeauth.ErrNotObserved) {
			t.Fatalf("no Job: %v", err)
		}
		e2, pod := dispatched(t)
		obs := e2.observation(pod)
		obs.ClaimedPodName = "no-such-pod"
		if _, err := e2.observer(t).ObserveJob(ctx, obs); !errors.Is(err, runtimeauth.ErrNotObserved) {
			t.Fatalf("no Pod: %v", err)
		}
		obs.ClaimedPodName = ""
		if _, err := e2.observer(t).ObserveJob(ctx, obs); !errors.Is(err, runtimeauth.ErrNotObserved) {
			t.Fatalf("no Pod name: %v", err)
		}
	})
	t.Run("outage is inconclusive, never not-observed", func(t *testing.T) {
		e, pod := dispatched(t)
		for _, op := range []string{"get job", "get pod"} {
			e.fake.Fault = func(o string, n int) *kubetestFault {
				if o == op {
					return &kubetestFault{Err: &kubernetes.Error{Op: o, Kind: kubernetes.ErrUnavailable}}
				}
				return nil
			}
			_, err := e.observer(t).ObserveJob(ctx, e.observation(pod))
			if err == nil || errors.Is(err, runtimeauth.ErrNotObserved) {
				t.Fatalf("%s outage: %v", op, err)
			}
		}
		e.fake.Fault = nil
		e.attempts.Err = errors.New("db down")
		if _, err := e.observer(t).ObserveJob(ctx, e.observation(pod)); err == nil || errors.Is(err, runtimeauth.ErrNotObserved) {
			t.Fatalf("store outage: %v", err)
		}
	})
	t.Run("another namespace or Job name yields evidence that cannot match", func(t *testing.T) {
		e, pod := dispatched(t)
		obs := e.observation(pod)
		obs.Expected.Namespace = "t021-globex"
		ev, err := e.observer(t).ObserveJob(ctx, obs)
		if err != nil || ev.JobUID != "" || ev.PodUID != "" {
			t.Fatalf("%+v %v", ev, err)
		}
		obs = e.observation(pod)
		obs.Expected.Name = "kdm-" + strings.Repeat("0", 40)
		ev, err = e.observer(t).ObserveJob(ctx, obs)
		if err != nil || ev.JobUID != "" {
			t.Fatalf("%+v %v", ev, err)
		}
		// A tenant the policy does not cover.
		obs = e.observation(pod)
		obs.Scope.TenantID = "t021_stranger"
		if _, err := e.observer(t).ObserveJob(ctx, obs); !errors.Is(err, kube.ErrPolicy) {
			t.Fatalf("stranger: %v", err)
		}
	})
	t.Run("a foreign Job at the name reveals no identity", func(t *testing.T) {
		e := newEnv(t)
		var j kubernetes.Job
		raw, _ := jsonMarshal(e.exp.Job)
		jsonUnmarshal(t, raw, &j)
		j.Spec.Template.Spec.HostNetwork = true
		foreign := e.fake.PutJob(e.ns(), j)
		pod := e.fake.AddPod(e.ns(), foreign, nil)
		ev, err := e.observer(t).ObserveJob(ctx, e.observation(pod))
		if err != nil || ev.JobUID != "" || ev.PodOwnerUID != "" {
			t.Fatalf("%+v %v", ev, err)
		}
	})
	t.Run("an unrelated Pod or wrong owner does not match", func(t *testing.T) {
		e, _ := dispatched(t)
		job := e.mustJob(t)
		// A Pod controlled by another object.
		other := e.fake.AddPod(e.ns(), job, func(p *kubernetes.Pod) {
			p.Metadata.OwnerReferences[0].UID = "00000000-0000-4000-8000-0000000000ff"
		})
		ev, err := e.observer(t).ObserveJob(ctx, e.observation(other))
		if err != nil || ev.PodOwnerUID == ev.JobUID {
			t.Fatalf("wrong controller matched: %+v %v", ev, err)
		}
		// A Pod with the right owner but a swapped image.
		hand := e.fake.AddPod(e.ns(), job, func(p *kubernetes.Pod) {
			p.Spec.Containers[0].Image = "evil.invalid/x@sha256:" + strings.Repeat("e", 64)
		})
		ev, err = e.observer(t).ObserveJob(ctx, e.observation(hand))
		if err != nil || ev.PodOwnerUID == ev.JobUID && ev.JobUID != "" {
			t.Fatalf("image swap matched: %+v %v", ev, err)
		}
		// A Pod without the Job controller's labels.
		bare := e.fake.AddPod(e.ns(), job, func(p *kubernetes.Pod) { p.Metadata.Labels = map[string]string{} })
		ev, _ = e.observer(t).ObserveJob(ctx, e.observation(bare))
		if ev.PodOwnerUID == ev.JobUID && ev.JobUID != "" {
			t.Fatal("an unlabelled Pod matched")
		}
		// A Pod in a host namespace.
		host := e.fake.AddPod(e.ns(), job, func(p *kubernetes.Pod) { p.Spec.HostNetwork = true })
		ev, _ = e.observer(t).ObserveJob(ctx, e.observation(host))
		if ev.PodOwnerUID == ev.JobUID && ev.JobUID != "" {
			t.Fatal("a host-network Pod matched")
		}
	})
	t.Run("construction requires policy, cluster and attempts", func(t *testing.T) {
		e := newEnv(t)
		if _, err := kube.NewJobObserver(kube.Dependencies{Cluster: e.fake, Attempts: e.attempts}, kube.Options{}); err == nil {
			t.Fatal("no policy accepted")
		}
	})
}
