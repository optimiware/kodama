package delivery_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
)

const ns = "preview"

func registration() delivery.Registration {
	exp := now.Add(24 * time.Hour)
	return delivery.Registration{
		Scope: scope, Environment: "env_1", ConfigDigest: runtimev1.Digest("sha256:" + strings.Repeat("a", 64)), Namespace: ns,
		Source:   delivery.Source{RepositoryURL: "https://git.example.invalid/r.git", Commit: strings.Repeat("1", 40)},
		Image:    delivery.Image{ArtifactID: "art_1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("b", 64))},
		Producer: delivery.Producer{Attempt: "att_build", Run: "run_build", Step: "build", ToolImage: "registry.example.invalid/tools@sha256:" + strings.Repeat("c", 64)},
		Run:      "run_deploy", Step: "deploy", ActionKey: "act:0123456789abcdef", Attempt: "att_deploy", ExpiresAt: &exp,
	}
}

func newDeployment(t *testing.T) delivery.Deployment {
	t.Helper()
	d, err := delivery.NewDeployment(registration(), now)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func objs(names ...string) []delivery.ObjectRef {
	var out []delivery.ObjectRef
	for _, n := range names {
		out = append(out, delivery.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: ns, Name: n})
	}
	return out
}

func ready(in []delivery.ObjectRef) []delivery.ObjectRef {
	out := append([]delivery.ObjectRef(nil), in...)
	for i := range out {
		out[i].UID = "00000000-0000-4000-8000-00000000000" + string(rune('0'+i))
		out[i].Ready = true
	}
	return out
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestDeploymentStartsPending(t *testing.T) {
	d := newDeployment(t)
	rec := d.Record()
	if rec.Status != delivery.StatusPending || rec.Reason != delivery.ReasonRegistered || rec.Version != 1 || !d.Dirty() || len(rec.Objects) != 0 {
		t.Fatalf("%+v", rec)
	}
	if d.ID() != delivery.DeploymentIDFor(scope, "act:0123456789abcdef") || delivery.DeploymentIDFor(scope, "act:other") == d.ID() ||
		delivery.DeploymentIDFor(domain2("t035_globex"), "act:0123456789abcdef") == d.ID() {
		t.Fatal("the deployment identity must derive from tenant, project and action key")
	}
	ann := d.Annotations(delivery.DefaultAnnotations)
	if ann[delivery.DefaultAnnotations.Deployment] != string(d.ID()) || ann[delivery.DefaultAnnotations.Action] != rec.ActionKey || ann[delivery.DefaultAnnotations.Configuration] != string(rec.ConfigDigest) {
		t.Fatalf("annotations: %v", ann)
	}
	if err := delivery.DefaultAnnotations.Validate(); err != nil {
		t.Fatal(err)
	}
	if (delivery.AnnotationNames{Deployment: "a", Action: "a", Configuration: "b"}).Validate() == nil || (delivery.AnnotationNames{Deployment: "bad key!", Action: "b", Configuration: "c"}).Validate() == nil {
		t.Fatal("invalid annotation names accepted")
	}
}

func TestFingerprintExcludesCallerAndAttempt(t *testing.T) {
	a := registration()
	b := registration()
	b.Attempt, b.ExpiresAt = "att_retry", nil
	fa, _ := a.Fingerprint()
	fb, _ := b.Fingerprint()
	if fa != fb {
		t.Fatal("a retry attempt changed the request identity")
	}
	for name, mutate := range map[string]func(*delivery.Registration){
		"source":      func(r *delivery.Registration) { r.Source.Commit = strings.Repeat("2", 40) },
		"image":       func(r *delivery.Registration) { r.Image.ArtifactID = "art_2" },
		"digest":      func(r *delivery.Registration) { r.Image.Digest = runtimev1.Digest("sha256:" + strings.Repeat("d", 64)) },
		"config":      func(r *delivery.Registration) { r.ConfigDigest = runtimev1.Digest("sha256:" + strings.Repeat("e", 64)) },
		"environment": func(r *delivery.Registration) { r.Environment = "env_2" },
		"run":         func(r *delivery.Registration) { r.Run = "run_other" },
		"step":        func(r *delivery.Registration) { r.Step = "other" },
		"action":      func(r *delivery.Registration) { r.ActionKey = "act:other" },
		"scope":       func(r *delivery.Registration) { r.Scope.ProjectID = "p035_other" },
	} {
		c := registration()
		mutate(&c)
		if f, _ := c.Fingerprint(); f == fa {
			t.Errorf("a different %s kept the request identity", name)
		}
	}
}

func TestDeploymentStateMachine(t *testing.T) {
	t.Run("pending to running to succeeded", func(t *testing.T) {
		d := newDeployment(t)
		c, err := d.BeginApply(objs("api", "worker"), now)
		if err != nil || !c.StatusChanged() || d.Status() != delivery.StatusRunning || d.Reason() != delivery.ReasonApplying {
			t.Fatalf("begin: %v %+v", err, c)
		}
		c, err = d.Settle(delivery.Settlement{Succeeded: true, Objects: ready(objs("api", "worker"))}, now)
		if err != nil || !c.StatusChanged() || d.Status() != delivery.StatusSucceeded || d.Reason() != delivery.ReasonReady {
			t.Fatalf("settle: %v %+v", err, c)
		}
		// Replays change nothing.
		v := d.Version()
		c, err = d.Settle(delivery.Settlement{Succeeded: true, Objects: ready(objs("api", "worker"))}, now)
		if err != nil || c.Changed || d.Version() != v {
			t.Fatalf("replay: %v %+v", err, c)
		}
	})

	t.Run("intent is recorded first and is immutable", func(t *testing.T) {
		d := newDeployment(t)
		_, err := d.BeginApply(nil, now)
		wantErr(t, err, delivery.ErrInvalid)
		_, err = d.BeginApply(ready(objs("api")), now)
		wantErr(t, err, delivery.ErrInvalid) // intended objects carry no observed state
		_, err = d.BeginApply(append(objs("api"), objs("api")...), now)
		wantErr(t, err, delivery.ErrInvalid)
		other := objs("api")
		other[0].Namespace = "kube-system"
		_, err = d.BeginApply(other, now)
		wantErr(t, err, delivery.ErrDenied)
		if d.Status() != delivery.StatusPending {
			t.Fatal("a refused apply moved the deployment")
		}
		if _, err := d.BeginApply(objs("api"), now); err != nil {
			t.Fatal(err)
		}
		if c, err := d.BeginApply(objs("api"), now); err != nil || c.Changed {
			t.Fatalf("replay: %v", err)
		}
		_, err = d.BeginApply(objs("api", "extra"), now)
		wantErr(t, err, delivery.ErrConflict)
	})

	t.Run("success needs every object observed ready with its UID", func(t *testing.T) {
		d := newDeployment(t)
		_, err := d.Settle(delivery.Settlement{Succeeded: true}, now)
		wantErr(t, err, delivery.ErrInvalidTransition) // it never applied
		if _, err := d.BeginApply(objs("api", "worker"), now); err != nil {
			t.Fatal(err)
		}
		for name, s := range map[string]delivery.Settlement{
			"nothing observed":  {Succeeded: true},
			"partial":           {Succeeded: true, Objects: ready(objs("api"))},
			"no readiness":      {Succeeded: true, Objects: objs("api", "worker")},
			"one not ready":     {Succeeded: true, Objects: append(ready(objs("api")), objs("worker")...)},
			"stranger":          {Succeeded: true, Objects: ready(objs("api", "stranger"))},
			"failed w/o reason": {Succeeded: false},
			"reserved reason":   {Succeeded: false, Reason: delivery.ReasonReady},
			"unknown reserved":  {Succeeded: false, Reason: delivery.ReasonOutcomeUnknown},
			"not a reason code": {Succeeded: false, Reason: "Bad Reason"},
		} {
			if _, err := d.Settle(s, now); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
		if d.Status() != delivery.StatusRunning {
			t.Fatalf("a refused settlement changed the status: %s", d.Status())
		}
	})

	t.Run("an unknown outcome blocks apply and is settled explicitly", func(t *testing.T) {
		d := newDeployment(t)
		_, err := d.MarkOutcomeUnknown(now)
		wantErr(t, err, delivery.ErrInvalidTransition) // only a running deployment
		if _, err := d.BeginApply(objs("api"), now); err != nil {
			t.Fatal(err)
		}
		c, err := d.MarkOutcomeUnknown(now)
		if err != nil || c.StatusChanged() || !d.OutcomeUnknown() || d.Status() != delivery.StatusRunning {
			t.Fatalf("unknown: %v %+v", err, c)
		}
		if c, err := d.MarkOutcomeUnknown(now); err != nil || c.Changed {
			t.Fatalf("repeat: %v", err)
		}
		_, err = d.BeginApply(objs("api"), now)
		wantErr(t, err, delivery.ErrOutcomeUnknown)
		if _, err := d.Settle(delivery.Settlement{Succeeded: true, Objects: ready(objs("api"))}, now); err != nil {
			t.Fatalf("reconciliation: %v", err)
		}
		if d.OutcomeUnknown() {
			t.Fatal("still unknown after settling")
		}
	})

	t.Run("a failure settles from pending or running and never rewrites", func(t *testing.T) {
		d := newDeployment(t)
		if _, err := d.Settle(delivery.Settlement{Reason: "manifest_invalid"}, now); err != nil || d.Status() != delivery.StatusFailed {
			t.Fatalf("pending failure: %v", err)
		}
		if c, err := d.Settle(delivery.Settlement{Reason: "manifest_invalid"}, now); err != nil || c.Changed {
			t.Fatalf("replay: %v", err)
		}
		_, err := d.Settle(delivery.Settlement{Reason: "other_reason"}, now)
		wantErr(t, err, delivery.ErrConflict)
		_, err = d.Settle(delivery.Settlement{Succeeded: true}, now)
		wantErr(t, err, delivery.ErrConflict)
		_, err = d.BeginApply(objs("api"), now)
		wantErr(t, err, delivery.ErrInvalidTransition)
		_, err = d.MarkOutcomeUnknown(now)
		wantErr(t, err, delivery.ErrInvalidTransition)

		// A failure that never applied cannot name observed objects.
		p := newDeployment(t)
		_, err = p.Settle(delivery.Settlement{Reason: "failed_early", Objects: ready(objs("api"))}, now)
		wantErr(t, err, delivery.ErrDenied)
	})

	t.Run("the version bumps once per mutation", func(t *testing.T) {
		d := newDeployment(t)
		d.MarkClean()
		if _, err := d.BeginApply(objs("api"), now); err != nil {
			t.Fatal(err)
		}
		if d.Version() != 2 || !d.Dirty() {
			t.Fatalf("v%d dirty=%v", d.Version(), d.Dirty())
		}
		if _, err := d.MarkOutcomeUnknown(now); err != nil || d.Version() != 2 {
			t.Fatalf("a second change inside one mutation bumped the version: v%d", d.Version())
		}
	})
}

func settled(t *testing.T) delivery.Deployment {
	t.Helper()
	d := newDeployment(t)
	if _, err := d.BeginApply(objs("api", "worker"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Settle(delivery.Settlement{Succeeded: true, Objects: ready(objs("api", "worker"))}, now); err != nil {
		t.Fatal(err)
	}
	d.MarkClean()
	return d
}

func TestCleanupWithoutErasingHistory(t *testing.T) {
	t.Run("requests are idempotent and need a settled or unknown outcome", func(t *testing.T) {
		p := newDeployment(t)
		_, err := p.RequestCleanup(now)
		wantErr(t, err, delivery.ErrInvalidTransition)
		if _, err := p.BeginApply(objs("api"), now); err != nil {
			t.Fatal(err)
		}
		_, err = p.RequestCleanup(now)
		wantErr(t, err, delivery.ErrInvalidTransition)
		if _, err := p.MarkOutcomeUnknown(now); err != nil {
			t.Fatal(err)
		}
		if ok, err := p.RequestCleanup(now); err != nil || !ok {
			t.Fatalf("unknown outcome cleanup: %v", err)
		}

		d := settled(t)
		if ok, err := d.RequestCleanup(now); err != nil || !ok || d.Record().CleanupState != delivery.CleanupRequested {
			t.Fatalf("request: %v", err)
		}
		if ok, err := d.RequestCleanup(now.Add(time.Hour)); err != nil || ok {
			t.Fatalf("a repeated request changed the record: %v", err)
		}
	})

	t.Run("deletion sets deleted_at and keeps the outcome and objects", func(t *testing.T) {
		d := settled(t)
		_, err := d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupDone, Deleted: ready(objs("api", "worker"))}, now)
		wantErr(t, err, delivery.ErrInvalidTransition) // not requested
		if _, err := d.RequestCleanup(now); err != nil {
			t.Fatal(err)
		}
		_, err = d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupDone, Deleted: ready(objs("api"))}, now)
		wantErr(t, err, delivery.ErrInvalid) // partial
		_, err = d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupDone, Deleted: ready(objs("api", "stranger"))}, now)
		wantErr(t, err, delivery.ErrDenied)
		foreign := objs("api", "worker")
		foreign[0].Namespace = "kube-system"
		_, err = d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupDone, Deleted: foreign}, now)
		wantErr(t, err, delivery.ErrDenied)
		wrong := ready(objs("api", "worker"))
		wrong[0].UID = "99999999-0000-4000-8000-000000000000"
		_, err = d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupDone, Deleted: wrong}, now)
		wantErr(t, err, delivery.ErrDenied)

		if ok, err := d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupDone, Deleted: ready(objs("api", "worker"))}, now); err != nil || !ok {
			t.Fatalf("deletion: %v", err)
		}
		rec := d.Record()
		if rec.DeletedAt == nil || rec.Status != delivery.StatusSucceeded || rec.Reason != delivery.ReasonReady || len(rec.Objects) != 2 || !d.Deleted() {
			t.Fatalf("%+v", rec)
		}
		if ok, err := d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupDone, Deleted: ready(objs("api", "worker"))}, now); err != nil || ok {
			t.Fatalf("a repeated acknowledgment changed the record: %v", err)
		}
		_, err = d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupFailed, Reason: "late"}, now)
		wantErr(t, err, delivery.ErrConflict)
		if d.Expired(now.Add(100 * time.Hour)) {
			t.Fatal("a deleted deployment is expired")
		}
	})

	t.Run("exceptions are recorded, repeatable and not rewritten when unchanged", func(t *testing.T) {
		d := settled(t)
		if _, err := d.RequestCleanup(now); err != nil {
			t.Fatal(err)
		}
		if ok, err := d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupFailed, Reason: "delete_failed"}, now); err != nil || !ok {
			t.Fatalf("failed: %v", err)
		}
		if ok, err := d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupFailed, Reason: "delete_failed"}, now); err != nil || ok {
			t.Fatalf("same exception changed: %v", err)
		}
		if ok, err := d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupUnknown, Reason: "api_timeout"}, now); err != nil || !ok || d.Record().CleanupState != delivery.CleanupUnknown {
			t.Fatalf("unknown: %v", err)
		}
		_, err := d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupFailed}, now)
		wantErr(t, err, delivery.ErrInvalid) // an exception names its reason
		_, err = d.RecordCleanup(delivery.CleanupResult{State: "bogus", Reason: "x_y"}, now)
		wantErr(t, err, delivery.ErrInvalid)
		if d.Deleted() || d.Status() != delivery.StatusSucceeded {
			t.Fatal("an exception rewrote the deployment")
		}
	})

	t.Run("expiry is derived from the persisted instant", func(t *testing.T) {
		d := settled(t)
		if d.Expired(now.Add(23*time.Hour)) || !d.Expired(now.Add(24*time.Hour)) {
			t.Fatal("expiry boundary")
		}
		stage := registration()
		stage.ExpiresAt = nil
		s, _ := delivery.NewDeployment(stage, now)
		if s.Expired(now.Add(1000 * time.Hour)) {
			t.Fatal("a deployment without an expiry expired")
		}
	})
}

func TestRestoreRejectsInconsistentState(t *testing.T) {
	good := newDeployment(t).Record()
	for name, mutate := range map[string]func(*delivery.DeploymentRecord){
		"status":     func(r *delivery.DeploymentRecord) { r.Status = "done" },
		"reason":     func(r *delivery.DeploymentRecord) { r.Reason = "Bad" },
		"namespace":  func(r *delivery.DeploymentRecord) { r.Namespace = "Bad_NS" },
		"object ns":  func(r *delivery.DeploymentRecord) { r.Objects = objs("a"); r.Objects[0].Namespace = "other" },
		"tool image": func(r *delivery.DeploymentRecord) { r.Producer.ToolImage = "registry.invalid/tools:latest" },
		"source": func(r *delivery.DeploymentRecord) {
			r.Source.RepositoryURL = "https://user:pw@git.example.invalid/r.git"
		},
		"action key": func(r *delivery.DeploymentRecord) { r.ActionKey = "bad key" },
		"version":    func(r *delivery.DeploymentRecord) { r.Version = 0 },
	} {
		r := good
		mutate(&r)
		if _, err := delivery.RestoreDeployment(r); !errors.Is(err, delivery.ErrInvalid) && !errors.Is(err, delivery.ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
