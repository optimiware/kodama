package kube_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

type env struct {
	fixture
	attempts *kubetest.Attempts
	reg      *kubetest.Registrar
	clock    *testClock
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := newFixture(t)
	kubetest.Baseline(f.fake, f.policy, f.tenant)
	e := &env{fixture: f, attempts: kubetest.NewAttempts(f.rec), reg: kubetest.NewRegistrar(), clock: &testClock{t: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}}
	f.fake.Now = e.clock.Now
	return e
}

// runtime builds a fresh runtime over the same cluster and stores: a restart.
func (e *env) runtime(t *testing.T) *kube.Runtime {
	t.Helper()
	x, err := kube.NewRuntime(kube.Dependencies{Policy: e.policy, Cluster: e.fake, Attempts: e.attempts, Registrar: e.reg}, kube.Options{Clock: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func (e *env) intent() execution.AttemptSpec { return kubetest.Intent(e.rec) }

// workload is the handle of the attempt's workload execution passes to Stop.
func (e *env) workload() execution.Workload { return e.intent().Workload("") }

// startErr keeps only the error of a Start.
func startErr(_ execution.Workload, err error) error { return err }

func (e *env) ns() string { return e.exp.Identity.Namespace }

func (e *env) secret(t *testing.T) kubernetes.Secret {
	t.Helper()
	s, ok := e.fake.SecretOf(e.ns(), kube.SecretName(e.rec.JobName))
	if !ok {
		t.Fatal("no admission Secret")
	}
	return s
}

func (e *env) job() (kubernetes.Job, bool) { return e.fake.JobOf(e.ns(), e.rec.JobName) }

func (e *env) mustJob(t *testing.T) kubernetes.Job {
	t.Helper()
	j, ok := e.job()
	if !ok {
		t.Fatal("no Job")
	}
	return j
}

func (e *env) record(t *testing.T) map[string]any {
	t.Helper()
	return parseRecord(t, e.secret(t))
}

func Test_dispatch_creates_one_verified_job(t *testing.T) {
	e := newEnv(t)
	x := e.runtime(t)
	if _, err := x.Start(context.Background(), e.intent()); err != nil {
		t.Fatal(err)
	}
	j := e.mustJob(t)
	if _, err := kube.VerifyJob(e.exp, j); err != nil {
		t.Fatalf("created Job fails its own verification: %v", err)
	}
	rec := e.record(t)
	if rec["phase"] != "owned" || rec["uid"] != j.Metadata.UID {
		t.Fatalf("record: %v", rec)
	}
	if _, ok := e.reg.Digest(e.rec.AttemptID); !ok || e.reg.Calls < 1 {
		t.Fatal("the credential was not registered")
	}
	// The credential in the Secret is the registered one and never leaves it.
	sec := e.secret(t)
	cred, err := runtimeauth.ParseCredential(string(sec.Data[kube.KeyAdmission]))
	if err != nil {
		t.Fatal(err)
	}
	d, _ := e.reg.Digest(e.rec.AttemptID)
	if !cred.Digest().Equal(d) {
		t.Fatal("the stored credential is not the registered one")
	}
	if string(sec.Data[kube.KeyCA]) != string(e.policy.RuntimeCA()) {
		t.Fatal("the bootstrap Secret does not carry the runtime CA")
	}
	for k, v := range sec.Metadata.Annotations {
		if strings.Contains(v, cred.Reveal()) {
			t.Fatalf("credential leaked into annotation %s", k)
		}
	}
	if j.Metadata.Annotations[kube.AnnFingerprint] != e.rec.SpecFingerprint {
		t.Fatal("the Job does not carry the committed fingerprint")
	}
	// Redriving after confirmation loss is idempotent.
	for i := 0; i < 3; i++ {
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); err != nil {
			t.Fatalf("redrive %d: %v", i, err)
		}
	}
	if e.fake.Calls("create job") != 1 {
		t.Fatalf("created %d Jobs", e.fake.Calls("create job"))
	}
}

func Test_dispatch_denials_create_nothing(t *testing.T) {
	check := func(t *testing.T, e *env, in execution.AttemptSpec, want error) {
		t.Helper()
		_, err := e.runtime(t).Start(context.Background(), in)
		if !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
		if _, ok := e.job(); ok {
			t.Fatal("a Job was created")
		}
		if _, ok := e.fake.SecretOf(e.ns(), kube.SecretName(e.rec.JobName)); ok {
			t.Fatal("a Secret was created")
		}
		if e.reg.Calls != 0 {
			t.Fatal("a credential was registered")
		}
	}
	t.Run("cancelled attempt", func(t *testing.T) {
		e := newEnv(t)
		e.attempts.Cancel(e.rec.AttemptID)
		check(t, e, e.intent(), kube.ErrNotDispatchable)
	})
	t.Run("unknown attempt", func(t *testing.T) {
		e := newEnv(t)
		e.attempts.Remove(e.rec.AttemptID)
		check(t, e, e.intent(), kube.ErrNotDispatchable)
	})
	t.Run("intent differs from committed data", func(t *testing.T) {
		for name, mut := range map[string]func(*execution.AttemptSpec){
			"job name":    func(i *execution.AttemptSpec) { i.WorkloadName = "kdm-" + strings.Repeat("1", 40) },
			"fingerprint": func(i *execution.AttemptSpec) { i.SpecFingerprint = "sha256:" + strings.Repeat("1", 64) },
			"digest":      func(i *execution.AttemptSpec) { i.ContextDigest = "sha256:" + strings.Repeat("1", 64) },
			"access":      func(i *execution.AttemptSpec) { i.Access = execution.AccessWrite },
			"step":        func(i *execution.AttemptSpec) { i.StepID = "other" },
			"scope":       func(i *execution.AttemptSpec) { i.Scope.ProjectID = "p_other" },
		} {
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				in := e.intent()
				mut(&in)
				check(t, e, in, kube.ErrNotDispatchable)
			})
		}
	})
	t.Run("tenant without policy", func(t *testing.T) {
		e := newEnv(t)
		p := kubetest.Policy(t, "globex")
		x, err := kube.NewRuntime(kube.Dependencies{Policy: p, Cluster: e.fake, Attempts: e.attempts, Registrar: e.reg}, kube.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := x.Start(context.Background(), e.intent()); !errors.Is(err, kube.ErrPolicy) {
			t.Fatalf("got %v", err)
		}
		if _, ok := e.job(); ok || e.reg.Calls != 0 {
			t.Fatal("effects without policy")
		}
	})
	t.Run("policy drift", func(t *testing.T) {
		e := newEnv(t)
		cfg := kubetest.PolicyConfig(t, "acme")
		cfg.RuntimeCA = e.policy.RuntimeCA()
		cfg.Workload.CPULimit = "4"
		p, err := kube.NewExecutionPolicy(cfg)
		if err != nil {
			t.Fatal(err)
		}
		x, _ := kube.NewRuntime(kube.Dependencies{Policy: p, Cluster: e.fake, Attempts: e.attempts, Registrar: e.reg}, kube.Options{})
		if _, err := x.Start(context.Background(), e.intent()); !errors.Is(err, kube.ErrPolicyDrift) {
			t.Fatalf("got %v", err)
		}
		if _, ok := e.job(); ok {
			t.Fatal("a drifted Job was created")
		}
	})
	t.Run("preflight failure", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Namespaces[e.ns()] = kubernetes.Namespace{Metadata: kubernetes.ObjectMeta{Name: e.ns(), Annotations: map[string]string{kube.AnnNamespaceTenant: string(e.tenant)}}}
		_, err := e.runtime(t).Start(context.Background(), e.intent())
		if !errors.Is(err, kube.ErrPreflight) {
			t.Fatalf("got %v", err)
		}
		if _, ok := e.job(); ok || e.reg.Calls != 0 {
			t.Fatal("effects despite a failed preflight")
		}
		if _, ok := e.fake.SecretOf(e.ns(), kube.SecretName(e.rec.JobName)); ok {
			t.Fatal("a Secret was created despite a failed preflight")
		}
	})
	t.Run("construction requires every dependency", func(t *testing.T) {
		e := newEnv(t)
		var zero kube.ExecutionPolicy
		for name, d := range map[string]kube.Dependencies{
			"policy":    {Cluster: e.fake, Attempts: e.attempts, Registrar: e.reg},
			"zero":      {Policy: zero, Cluster: e.fake, Attempts: e.attempts, Registrar: e.reg},
			"cluster":   {Policy: e.policy, Attempts: e.attempts, Registrar: e.reg},
			"attempts":  {Policy: e.policy, Cluster: e.fake, Registrar: e.reg},
			"registrar": {Policy: e.policy, Cluster: e.fake, Attempts: e.attempts},
		} {
			if _, err := kube.NewRuntime(d, kube.Options{}); err == nil {
				t.Fatalf("%s: accepted", name)
			}
		}
	})
}

// Test_dispatch_create_response_loss: a lost create acknowledgment is resolved by
// reading the Job back; a restart adopts the same Job and credential.
func Test_dispatch_create_response_loss(t *testing.T) {
	t.Run("applied then lost, read-back adopts", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "create job" {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}, After: true}
			}
			return nil
		}
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); err != nil {
			t.Fatalf("a lost acknowledgment with a verifiable Job must succeed: %v", err)
		}
		j := e.mustJob(t)
		if e.record(t)["uid"] != j.Metadata.UID {
			t.Fatal("the UID was not confirmed")
		}
	})
	t.Run("lost and unreadable, restart adopts", func(t *testing.T) {
		e := newEnv(t)
		gets := 0
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			switch op {
			case "create job":
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}, After: true}
			case "get job":
				gets++
				if gets == 2 { // the read-back right after the lost create
					return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnavailable}}
				}
			}
			return nil
		}
		first := startErr(e.runtime(t).Start(context.Background(), e.intent()))
		if !errors.Is(first, kube.ErrUnavailable) {
			t.Fatalf("got %v", first)
		}
		if e.record(t)["phase"] != "create_issued" {
			t.Fatalf("the issued create must be recorded: %v", e.record(t))
		}
		// Discard controller memory: a new executor adopts the same Job.
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); err != nil {
			t.Fatalf("restart: %v", err)
		}
		if e.fake.Calls("create job") != 1 {
			t.Fatalf("created %d Jobs", e.fake.Calls("create job"))
		}
		if e.record(t)["uid"] != e.mustJob(t).Metadata.UID {
			t.Fatal("not adopted")
		}
	})
	t.Run("lost and not applied: absence is not permission to create again", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "create job" {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}}
			}
			return nil
		}
		for i := 0; i < 4; i++ {
			_, err := e.runtime(t).Start(context.Background(), e.intent())
			if !errors.Is(err, kube.ErrCreateUncertain) {
				t.Fatalf("attempt %d: %v", i, err)
			}
		}
		if e.fake.Calls("create job") != 1 {
			t.Fatalf("created %d times despite an uncertain outcome", e.fake.Calls("create job"))
		}
		if e.record(t)["uncertain"] != true {
			t.Fatal("uncertainty not preserved")
		}
	})
	t.Run("definite refusal allows a later create", func(t *testing.T) {
		e := newEnv(t)
		n := 0
		e.fake.Fault = func(op string, _ int) *kubetest.Fault {
			if op == "create job" {
				n++
				if n == 1 {
					return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrThrottled, Status: 429}}
				}
			}
			return nil
		}
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); !errors.Is(err, kube.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if e.record(t)["phase"] != "registered" {
			t.Fatalf("a refused create returns to registered: %v", e.record(t))
		}
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); err != nil {
			t.Fatal(err)
		}
		if e.fake.Calls("create job") != 2 {
			t.Fatal("the second create did not happen")
		}
	})
	t.Run("concurrent redrive converges", func(t *testing.T) {
		e := newEnv(t)
		var wg sync.WaitGroup
		errs := make([]error, 6)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = startErr(e.runtime(t).Start(context.Background(), e.intent()))
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil && !errors.Is(err, kube.ErrUnavailable) && !errors.Is(err, kube.ErrCreateUncertain) {
				t.Fatalf("unexpected: %v", err)
			}
		}
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); err != nil {
			t.Fatal(err)
		}
		if e.fake.Calls("create job") != 1 {
			t.Fatalf("created %d Jobs", e.fake.Calls("create job"))
		}
		d, _ := e.reg.Digest(e.rec.AttemptID)
		cred, _ := runtimeauth.ParseCredential(string(e.secret(t).Data[kube.KeyAdmission]))
		if !cred.Digest().Equal(d) {
			t.Fatal("racing controllers diverged on the credential")
		}
	})
}

// Test_dispatch_absence_before_concurrent_adoption forces a redrive to read an
// absent Job before another controller creates and confirms it. The later owned
// record must not turn that earlier absence into evidence of disappearance.
func Test_dispatch_absence_before_concurrent_adoption(t *testing.T) {
	for name, tc := range map[string]struct {
		after      func(*env)
		unreadable bool
		deletes    int
		want       error
	}{
		"same Job is adopted": {},
		"confirmed Job really disappeared": {
			after: func(e *env) { e.fake.DropJob(e.ns(), e.rec.JobName) },
			want:  kube.ErrNotDispatchable,
		},
		"same-name replacement is foreign": {
			after: func(e *env) {
				e.fake.DropJob(e.ns(), e.rec.JobName)
				e.fake.PutJob(e.ns(), e.exp.Job)
			},
			want: kube.ErrForeignWorkload,
		},
		"changed spec is foreign": {
			after: func(e *env) {
				e.fake.MutateJobRaw(e.ns(), e.rec.JobName, func(doc map[string]any) {
					spec := doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
					spec["hostNetwork"] = true
				})
			},
			want: kube.ErrForeignWorkload,
		},
		"read failure is not absence": {unreadable: true, want: kube.ErrUnavailable},
		"cancellation still denies adoption": {
			after:   func(e *env) { e.attempts.Cancel(e.rec.AttemptID) },
			deletes: 1,
			want:    kube.ErrNotDispatchable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			var confirmed kubernetes.Job
			var before kubernetes.Job
			var present bool
			var credential string
			interleaved := false
			e.fake.Fault = func(op string, n int) *kubetest.Fault {
				if op == "get secret" && n == 1 {
					// The first controller already read Job absence. Run the second
					// to completion before letting the first read the admission record.
					if e.fake.Calls("get job") != 1 {
						t.Fatal("the first controller has not read Job absence")
					}
					if _, err := e.runtime(t).Start(context.Background(), e.intent()); err != nil {
						t.Fatalf("concurrent controller: %v", err)
					}
					confirmed = e.mustJob(t)
					credential = string(e.secret(t).Data[kube.KeyAdmission])
					if tc.after != nil {
						tc.after(e)
					}
					before, present = e.job()
					interleaved = true
				}
				if interleaved && tc.unreadable && op == "get job" {
					return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnavailable}}
				}
				return nil
			}
			if _, err := e.runtime(t).Start(context.Background(), e.intent()); !errors.Is(err, tc.want) {
				t.Fatalf("redrive: got %v, want %v", err, tc.want)
			}
			if !interleaved {
				t.Fatal("the competing controller did not run")
			}
			if e.fake.Calls("create job") != 1 {
				t.Fatal("redrive issued another create")
			}
			if e.fake.Calls("delete job") != tc.deletes {
				t.Fatalf("redrive deleted %d Jobs, want %d", e.fake.Calls("delete job"), tc.deletes)
			}
			if tc.deletes == 0 {
				after, found := e.job()
				if found != present || after.Metadata.UID != before.Metadata.UID || string(after.Raw) != string(before.Raw) {
					t.Fatal("redrive modified or replaced the Job")
				}
			}
			if e.record(t)["uid"] != confirmed.Metadata.UID {
				t.Fatal("the confirmed UID changed")
			}
			if string(e.secret(t).Data[kube.KeyAdmission]) != credential {
				t.Fatal("redrive changed the credential")
			}
		})
	}
}

// Test_dispatch_credential_recovery: crashes and lost responses never regenerate
// a registered credential or change its admission expiry.
func Test_dispatch_credential_recovery(t *testing.T) {
	stored := func(t *testing.T, e *env) string { return string(e.secret(t).Data[kube.KeyAdmission]) }
	t.Run("registration fails then retries the same credential", func(t *testing.T) {
		e := newEnv(t)
		e.reg.Fail = func(n int) error {
			if n == 1 {
				return runtimeauth.ErrUnavailable
			}
			return nil
		}
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); !errors.Is(err, kube.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		first := stored(t, e)
		if e.record(t)["phase"] != "credentialed" {
			t.Fatalf("record: %v", e.record(t))
		}
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); err != nil {
			t.Fatal(err)
		}
		if stored(t, e) != first {
			t.Fatal("the credential was regenerated")
		}
		d, _ := e.reg.Digest(e.rec.AttemptID)
		cred, _ := runtimeauth.ParseCredential(first)
		if !cred.Digest().Equal(d) {
			t.Fatal("registered a different credential")
		}
	})
	t.Run("registration applied but response lost", func(t *testing.T) {
		e := newEnv(t)
		e.reg.FailAfter = func(n int) error {
			if n == 1 {
				return runtimeauth.ErrUnavailable
			}
			return nil
		}
		_, _ = e.runtime(t).Start(context.Background(), e.intent())
		first := stored(t, e)
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); err != nil {
			t.Fatal(err)
		}
		if stored(t, e) != first {
			t.Fatal("the credential changed after a lost registration response")
		}
	})
	t.Run("secret create response lost", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "create secret" && n == 1 {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}, After: true}
			}
			return nil
		}
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); err != nil {
			t.Fatal(err)
		}
		d, _ := e.reg.Digest(e.rec.AttemptID)
		cred, _ := runtimeauth.ParseCredential(stored(t, e))
		if !cred.Digest().Equal(d) {
			t.Fatal("the registered credential is not the stored one")
		}
	})
	t.Run("secret create lost and not applied", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "create secret" && n == 1 {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}}
			}
			return nil
		}
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); !errors.Is(err, kube.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if e.reg.Calls != 0 {
			t.Fatal("a credential that was never stored was registered")
		}
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("lost secret is never regenerated for a registered attempt", func(t *testing.T) {
		e := newEnv(t)
		e.reg.Fail = nil
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); err != nil {
			t.Fatal(err)
		}
		// The Secret and Job vanish (external deletion); the registration stays.
		sec := e.secret(t)
		_ = e.fake.DeleteSecret(context.Background(), e.ns(), sec.Metadata.Name, kubernetes.DeleteOptions{UID: sec.Metadata.UID})
		e.fake.DropJob(e.ns(), e.rec.JobName)
		_, err := e.runtime(t).Start(context.Background(), e.intent())
		if !errors.Is(err, kube.ErrCredentialConflict) {
			t.Fatalf("got %v", err)
		}
		if _, ok := e.job(); ok {
			t.Fatal("a Job was created with an unregistrable credential")
		}
	})
	t.Run("a revoked attempt cannot register", func(t *testing.T) {
		e := newEnv(t)
		e.reg.Revoke(e.rec.AttemptID)
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); !errors.Is(err, kube.ErrNotDispatchable) {
			t.Fatalf("got %v", err)
		}
		if _, ok := e.job(); ok {
			t.Fatal("a Job was created")
		}
	})
}

// Test_dispatch_foreign_collisions: same-name objects that are not the attempt's
// are never adopted, modified or deleted.
func Test_dispatch_foreign_collisions(t *testing.T) {
	foreignJob := func(t *testing.T, e *env, edit func(*kubernetes.Job)) kubernetes.Job {
		var j kubernetes.Job
		raw, _ := jsonMarshal(e.exp.Job)
		jsonUnmarshal(t, raw, &j)
		edit(&j)
		return e.fake.PutJob(e.ns(), j)
	}
	cases := map[string]func(*kubernetes.Job){
		"changed spec with forged fingerprint": func(j *kubernetes.Job) {
			j.Spec.Template.Spec.Containers[0].Image = "evil.invalid/x@sha256:" + strings.Repeat("d", 64)
		},
		"privileged": func(j *kubernetes.Job) {
			j.Spec.Template.Spec.Containers[0].SecurityContext.Privileged = new(bool)
			*j.Spec.Template.Spec.Containers[0].SecurityContext.Privileged = true
		},
		"foreign owner": func(j *kubernetes.Job) {
			j.Metadata.OwnerReferences = []kubernetes.OwnerReference{{APIVersion: "batch/v1", Kind: "CronJob", Name: "c", UID: "x"}}
		},
		"foreign annotations": func(j *kubernetes.Job) { j.Metadata.Annotations = map[string]string{"foreign": "yes"} },
		"other attempt": func(j *kubernetes.Job) {
			j.Metadata.Annotations[kube.AnnAttempt] = "att_other"
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			before := foreignJob(t, e, edit)
			_, err := e.runtime(t).Start(context.Background(), e.intent())
			if !errors.Is(err, kube.ErrForeignWorkload) {
				t.Fatalf("got %v", err)
			}
			after := e.mustJob(t)
			if after.Metadata.UID != before.Metadata.UID || string(after.Raw) != string(before.Raw) {
				t.Fatal("the foreign Job was modified or replaced")
			}
			if e.fake.Calls("delete job") != 0 || e.fake.Calls("create job") != 0 {
				t.Fatal("a foreign Job was deleted or a create was attempted over it")
			}
			// A collision is detected before any effect: no Secret, no registration.
			if _, ok := e.fake.SecretOf(e.ns(), kube.SecretName(e.rec.JobName)); ok || e.reg.Calls != 0 {
				t.Fatal("a collision created a Secret or registered a credential")
			}
		})
	}
	t.Run("collision at create time", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "create job" {
				// A foreign Job appears just before our create lands.
				foreignJob(t, e, func(j *kubernetes.Job) { j.Metadata.Labels["x"] = "y"; j.Spec.Template.Spec.HostNetwork = true })
			}
			return nil
		}
		_, err := e.runtime(t).Start(context.Background(), e.intent())
		if !errors.Is(err, kube.ErrForeignWorkload) {
			t.Fatalf("got %v", err)
		}
		if e.record(t)["phase"] != "registered" {
			t.Fatalf("a refused create returns to registered: %v", e.record(t))
		}
		if e.fake.Calls("delete job") != 0 {
			t.Fatal("deleted a foreign Job")
		}
	})
	t.Run("uid substitution after adoption", func(t *testing.T) {
		e := newEnv(t)
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); err != nil {
			t.Fatal(err)
		}
		orig := e.mustJob(t)
		// The Job is replaced by an identical-looking one with another UID.
		e.fake.DropJob(e.ns(), e.rec.JobName)
		replacement := foreignJob(t, e, func(*kubernetes.Job) {})
		if replacement.Metadata.UID == orig.Metadata.UID {
			t.Fatal("fixture UID did not change")
		}
		_, err := e.runtime(t).Start(context.Background(), e.intent())
		if !errors.Is(err, kube.ErrForeignWorkload) {
			t.Fatalf("got %v", err)
		}
		if e.record(t)["uid"] != orig.Metadata.UID {
			t.Fatal("the confirmed UID changed")
		}
		if e.fake.Calls("delete job") != 0 {
			t.Fatal("deleted the replacement")
		}
	})
	t.Run("foreign secret", func(t *testing.T) {
		e := newEnv(t)
		name := kube.SecretName(e.rec.JobName)
		put := e.fake.PutSecret(e.ns(), kubernetes.Secret{Metadata: kubernetes.ObjectMeta{Name: name, Labels: map[string]string{"x": "y"}}, Data: map[string][]byte{"k": []byte("v")}})
		_, err := e.runtime(t).Start(context.Background(), e.intent())
		if !errors.Is(err, kube.ErrForeignWorkload) {
			t.Fatalf("got %v", err)
		}
		got := e.secret(t)
		if got.Metadata.ResourceVersion != put.Metadata.ResourceVersion || string(got.Data["k"]) != "v" {
			t.Fatal("the foreign Secret was modified")
		}
		if e.reg.Calls != 0 {
			t.Fatal("registered over a foreign Secret")
		}
	})
}

// Test_dispatch_cancel_create_race: a cancellation committed while the create is
// in flight deletes the late workload and denies it authority.
func Test_dispatch_cancel_create_race(t *testing.T) {
	t.Run("cancel after the create applied", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "create job" {
				e.attempts.Cancel(e.rec.AttemptID)
				e.reg.Revoke(e.rec.AttemptID)
			}
			return nil
		}
		_, err := e.runtime(t).Start(context.Background(), e.intent())
		if !errors.Is(err, kube.ErrNotDispatchable) {
			t.Fatalf("got %v", err)
		}
		if _, ok := e.job(); ok {
			t.Fatal("the late workload was not deleted")
		}
		if e.fake.Calls("delete job") != 1 {
			t.Fatal("expected one UID-preconditioned deletion")
		}
	})
	t.Run("cancel before the create is sent", func(t *testing.T) {
		e := newEnv(t)
		e.attempts.OnRead = func(n int) {
			// Reads: 1 dispatch, 2 before create. Cancel between them.
			if n == 2 {
				e.attempts.Cancel(e.rec.AttemptID)
			}
		}
		_, err := e.runtime(t).Start(context.Background(), e.intent())
		if !errors.Is(err, kube.ErrNotDispatchable) {
			t.Fatalf("got %v", err)
		}
		if e.fake.Calls("create job") != 0 {
			t.Fatal("a create was sent for a cancelled attempt")
		}
		if e.record(t)["phase"] != "registered" {
			t.Fatalf("an unsent create is a definite non-create: %v", e.record(t))
		}
		// Termination of the same attempt has affirmative evidence that no
		// workload was created, and keeps it across retries and a restart.
		res, err := e.runtime(t).Stop(context.Background(), e.workload())
		if err != nil || !res.Stopped || res.Launch != execution.LaunchNotStarted {
			t.Fatalf("%+v %v", res, err)
		}
		if rec := e.record(t); rec["phase"] != "gone" || rec["launch"] != string(execution.LaunchNotStarted) {
			t.Fatalf("the evidence was not persisted with the transition: %v", rec)
		}
		res, err = e.runtime(t).Stop(context.Background(), e.workload())
		if err != nil || !res.Stopped || res.Launch != execution.LaunchNotStarted {
			t.Fatalf("after restart: %+v %v", res, err)
		}
	})
	t.Run("late deletion failure leaves the intent for termination", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			switch op {
			case "create job":
				e.attempts.Cancel(e.rec.AttemptID)
			case "delete job":
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnavailable}}
			}
			return nil
		}
		if _, err := e.runtime(t).Start(context.Background(), e.intent()); !errors.Is(err, kube.ErrNotDispatchable) {
			t.Fatalf("got %v", err)
		}
		if _, ok := e.job(); !ok {
			t.Fatal("expected the Job to remain until termination deletes it")
		}
		e.fake.Fault = nil
		// Terminate (requested by execution) removes it.
		res, err := e.runtime(t).Stop(context.Background(), e.workload())
		if err != nil {
			t.Fatal(err)
		}
		_ = res
		if _, ok := e.job(); ok {
			t.Fatal("termination did not delete the late Job")
		}
	})
}
