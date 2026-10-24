package kube_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

type podLogs struct{ ns, pod, container string }

func (l *podLogs) PodLog(_ context.Context, ns, pod, container string) ([]byte, error) {
	l.ns, l.pod, l.container = ns, pod, container
	return []byte("hello from the pod\n"), nil
}

// Test_runtime_surface: the Kubernetes runtime as execution sees it: Start
// reports the verified Job UID, Observe applies the reconciler's assessment
// to the committed attempt, Logs reads the verified Pod and Remove keeps the
// Job through the retention window, then deletes it and its admission Secret.
func Test_runtime_surface(t *testing.T) {
	ctx := context.Background()
	if c := (&kube.Runtime{}).Capabilities(); c.Backend != kube.Backend || c.Guarantees != execution.AllGuarantees {
		t.Fatalf("capabilities %+v", c)
	}

	t.Run("start reports the adopted Job", func(t *testing.T) {
		e := newEnv(t)
		w, err := e.runtime(t).Start(ctx, e.intent())
		if err != nil || w.UID != e.mustJob(t).Metadata.UID || w != e.intent().Workload(w.UID) {
			t.Fatalf("%+v %v", w, err)
		}
		again, err := e.runtime(t).Start(ctx, e.intent())
		if err != nil || again != w {
			t.Fatalf("redrive: %+v %v", again, err)
		}
		other := e.intent()
		other.SpecFingerprint = "sha256:" + strings.Repeat("f", 64)
		if _, err := e.runtime(t).Start(ctx, other); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("another spec: %v", err)
		}
	})

	t.Run("observe", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		w, err := x.Start(ctx, e.intent())
		if err != nil {
			t.Fatal(err)
		}
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) { r.DispatchConfirmed, r.ObservedJobUID = true, w.UID })
		observe := func() execution.Observation {
			t.Helper()
			obs, err := x.Observe(ctx, w)
			if err != nil {
				t.Fatal(err)
			}
			return obs
		}
		if obs := observe(); obs.Kind != execution.ObservedDispatched || obs.WorkloadUID != w.UID {
			t.Fatalf("created: %+v", obs)
		}
		e.started(t)
		if obs := observe(); obs.Kind != execution.ObservedStarted || obs.Launch != execution.LaunchStarted {
			t.Fatalf("running: %+v", obs)
		}
		e.fake.SetJobStatus(e.ns(), e.rec.JobName, func(s *kubernetes.JobStatus) {
			s.Conditions = append(s.Conditions, kubernetes.JobCondition{Type: "Failed", Status: "True"})
		})
		if obs := observe(); obs.Kind != execution.ObservedFailed || obs.Reason != execution.ReasonWorkloadFailed || obs.Class != execution.ClassInfrastructure {
			t.Fatalf("failed: %+v", obs)
		}
		foreign := w
		foreign.UID = "99999999-2222-4333-8444-555555555555"
		if _, err := x.Observe(ctx, foreign); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("another instance: %v", err)
		}
		e.fake.DropJob(e.ns(), e.rec.JobName)
		if _, err := x.Observe(ctx, w); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("a lost Job: %v", err)
		}
	})

	t.Run("observe never adopts a replacement", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		w, err := x.Start(ctx, e.intent())
		if err != nil {
			t.Fatal(err)
		}
		e.commitStart(w.UID)
		e.replaceJob(t)
		if _, err := x.Observe(ctx, w); !errors.Is(err, execution.ErrConflict) || !errors.Is(err, kube.ErrForeignWorkload) {
			t.Fatalf("replacement: %v", err)
		}
	})

	t.Run("logs", func(t *testing.T) {
		e := newEnv(t)
		w, err := e.runtime(t).Start(ctx, e.intent())
		if err != nil {
			t.Fatal(err)
		}
		if err := e.runtime(t).Logs(ctx, w, &bytes.Buffer{}); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("without a reader: %v", err)
		}
		logs := &podLogs{}
		x, err := kube.NewRuntime(kube.Dependencies{Policy: e.policy, Cluster: e.fake, Attempts: e.attempts, Registrar: e.reg, Logs: logs}, kube.Options{Clock: e.clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		if err := x.Logs(ctx, w, &bytes.Buffer{}); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("no Pod yet: %v", err)
		}
		pod := e.started(t)
		var b bytes.Buffer
		if err := x.Logs(ctx, w, &b); err != nil || b.String() != "hello from the pod\n" || logs.pod != pod.Metadata.Name || logs.container != kube.PrimaryContainer {
			t.Fatalf("%q %v %+v", b.String(), err, logs)
		}
	})

	t.Run("remove keeps the Job through retention", func(t *testing.T) {
		e := newEnv(t)
		x, err := kube.NewRuntime(kube.Dependencies{Policy: e.policy, Cluster: e.fake, Attempts: e.attempts, Registrar: e.reg},
			kube.Options{Clock: e.clock.Now, CompletedRetention: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		w, err := x.Start(ctx, e.intent())
		if err != nil {
			t.Fatal(err)
		}
		e.started(t)
		if err := x.Remove(ctx, w); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("an unsettled attempt: %v", err)
		}
		now := e.clock.Now()
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) {
			r.Status, r.Live, r.Settled, r.SettledAt = execution.AttemptSucceeded, false, true, &now
		})
		if err := x.Remove(ctx, w); !errors.Is(err, execution.ErrBusy) {
			t.Fatalf("within retention: %v", err)
		}
		if _, ok := e.job(); !ok {
			t.Fatal("the Job was deleted within retention")
		}
		e.clock.Advance(time.Hour)
		for i := 0; ; i++ {
			err := x.Remove(ctx, w)
			if err == nil {
				break
			}
			if !errors.Is(err, execution.ErrBusy) || i > 3 {
				t.Fatalf("after retention: %v", err)
			}
		}
		if _, ok := e.job(); ok {
			t.Fatal("the Job survived removal")
		}
		if _, err := e.fake.GetSecret(ctx, e.ns(), kube.SecretName(e.rec.JobName)); !errors.Is(err, kubernetes.ErrNotFound) {
			t.Fatalf("the admission Secret survived removal: %v", err)
		}
		if err := x.Remove(ctx, w); err != nil {
			t.Fatalf("a repeated removal: %v", err)
		}
	})
}
