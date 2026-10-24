package kube_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
)

// Test_create_ack_crash (PX-A02, PX-03, PX-04): the Job create's acknowledgment is
// lost and the controller loses its memory; a restarted controller adopts the
// same matching Job and UID. One attempt, one credential, one reservation, one
// Job and one Pod, including under concurrent redrive.
func Test_create_ack_crash(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}})
	in := s.claim(executiontest.ScopeA.TenantID)

	// The create is applied by the API server, its response is lost, and the
	// read-back right after it also fails: the dispatch ends unconfirmed.
	gets := 0
	s.cl.Lose = func(op string, n int) bool { return op == "create job" && n == 1 }
	s.cl.Fail = func(op string, n int) error {
		if op == "get job" {
			gets++
			if gets == 2 { // the first read precedes the create; the second is the read-back
				return &kubeUnavailable
			}
		}
		return nil
	}
	_, err := s.runtime.Start(ctx, *in)
	if !errors.Is(err, kube.ErrUnavailable) {
		t.Fatalf("a lost acknowledgment with no read-back is unconfirmed: %v", err)
	}
	s.cl.reset()
	job, ok := s.jobOf(in)
	if !ok {
		t.Fatal("the create was applied: the Job must exist")
	}
	uid := job.Metadata.UID

	// Controller crash: a fresh executor, no memory, redriven concurrently by
	// several replicas with the same intent.
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = startErr(s.newRuntime(s.c.client).Start(ctx, *in))
		}()
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil && !errors.Is(e, kube.ErrUnavailable) {
			t.Fatalf("redrive %d: %v", i, e)
		}
	}
	if _, err := s.newRuntime(s.c.client).Start(ctx, *in); err != nil {
		t.Fatalf("redrive: %v", err)
	}
	s.observeDispatched(in)

	if got, _ := s.jobOf(in); got.Metadata.UID != uid {
		t.Fatalf("the Job was replaced: %s != %s", got.Metadata.UID, uid)
	}
	if n := s.jobsNamed(in); n != 1 {
		t.Fatalf("%d Jobs", n)
	}
	if s.attemptsOf(run) != 1 || s.grants(in) != 1 || s.held() != 1 {
		t.Fatalf("attempts=%d grants=%d reservations=%d", s.attemptsOf(run), s.grants(in), s.held())
	}
	if !s.secretExists(in) {
		t.Fatal("the admission Secret is missing")
	}

	// The adopted Job runs to a committed result through the real entrypoint.
	s.until(3*time.Minute, "the run to succeed", func() (bool, string) {
		st := s.e.Get(run).Run.Status
		return st == execution.RunSucceeded, string(st)
	})
	if got := strOutput(t, s.e.Get(run), "v"); got != "ok" {
		t.Fatalf("output %q", got)
	}
	if n := len(s.podsOf(in)); n != 1 {
		t.Fatalf("%d Pods", n)
	}
	if s.count(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(run.ID)) != 1 {
		t.Fatal("a redrive created another attempt")
	}
}
