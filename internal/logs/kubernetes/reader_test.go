package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/logstest/kubefake"
)

const (
	jobUID  = "11111111-2222-3333-4444-555555555555"
	otherID = "99999999-2222-3333-4444-555555555555"
)

var target = logs.Target{Namespace: "kodama-runs", JobName: "kodama-att-1", JobUID: jobUID, Container: "step"}

func newFake(t *testing.T) (*kubefake.API, *Reader) {
	t.Helper()
	f := kubefake.New(t, target.Namespace, target.JobName, jobUID, "step")
	r, err := NewReader(Config{Endpoint: f.URL(), Client: f.Client(), AllowInsecure: true, MaxObjectBytes: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	return f, r
}

func jobObject(uid string) map[string]any {
	return kubefake.JobObject(target.Namespace, target.JobName, uid)
}

func podObject(name, created, ownerUID string, controller bool, containers ...string) map[string]any {
	return kubefake.PodObject(target.Namespace, target.JobName, name, created, ownerUID, controller, containers...)
}

func read(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReaderOpensVerifiedWorkload(t *testing.T) {
	f, r := newFake(t)
	rc, err := r.Open(context.Background(), target, logs.LiveQuery{TailLines: 5, LimitBytes: 1024, Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, rc); got != "one\ntwo\n" {
		t.Fatalf("got %q", got)
	}
	reqs := f.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %v", reqs)
	}
	if !strings.Contains(reqs[1], "labelSelector=batch.kubernetes.io%2Fcontroller-uid%3D"+jobUID) {
		t.Fatalf("the selector is built from the Job's own labels: %s", reqs[1])
	}
	for _, want := range []string{"container=step", "tailLines=5", "limitBytes=1024", "follow=true"} {
		if !strings.Contains(reqs[2], want) {
			t.Fatalf("log request %s lacks %s", reqs[2], want)
		}
	}
}

func TestKubernetesOwnershipFake(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *kubefake.API)
		want  error
	}{
		{"job absent", func(f *kubefake.API) { f.Job = nil }, logs.ErrWorkloadMissing},
		{"job has another uid (recreated)", func(f *kubefake.API) { f.Job = jobObject(otherID) }, logs.ErrWorkloadMismatch},
		{"no pods", func(f *kubefake.API) { f.Pods = nil }, logs.ErrWorkloadMissing},
		{"pod controlled by another job uid", func(f *kubefake.API) {
			f.Pods = []map[string]any{podObject("pod-x", "2026-01-01T00:00:00Z", otherID, true, "step")}
		}, logs.ErrWorkloadMissing},
		{"pod owner is not the controller", func(f *kubefake.API) {
			f.Pods = []map[string]any{podObject("pod-x", "2026-01-01T00:00:00Z", jobUID, false, "step")}
		}, logs.ErrWorkloadMissing},
		{"wrong container", func(f *kubefake.API) {
			f.Pods = []map[string]any{podObject("pod-x", "2026-01-01T00:00:00Z", jobUID, true, "sidecar")}
		}, logs.ErrWorkloadMismatch},
		{"selector missing", func(f *kubefake.API) {
			f.Job["spec"] = map[string]any{"selector": map[string]any{}}
		}, logs.ErrWorkloadMismatch},
		{"selector with an injected operator", func(f *kubefake.API) {
			f.Job["spec"] = map[string]any{"selector": map[string]any{"matchLabels": map[string]string{"a": "b,tenant!=x"}}}
		}, logs.ErrWorkloadMismatch},
		{"log not started", func(f *kubefake.API) { f.LogCode = http.StatusBadRequest }, logs.ErrWorkloadMissing},
		{"log gone", func(f *kubefake.API) { f.LogCode = http.StatusNotFound }, logs.ErrWorkloadMissing},
		{"kubelet failure", func(f *kubefake.API) { f.LogCode = http.StatusInternalServerError }, logs.ErrUnavailable},
		{"oversized object response", func(f *kubefake.API) { f.JobBytes = 1 << 17 }, logs.ErrUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, r := newFake(t)
			c.setup(f)
			rc, err := r.Open(context.Background(), target, logs.LiveQuery{})
			if rc != nil {
				rc.Close()
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if err != nil && (strings.Contains(err.Error(), "secret detail") || strings.Contains(err.Error(), "credentials=abc")) {
				t.Fatalf("provider text leaked into the error: %v", err)
			}
		})
	}
}

func TestReaderPicksNewestOwnedPodOnly(t *testing.T) {
	f, r := newFake(t)
	f.Pods = []map[string]any{
		podObject("pod-old", "2026-01-01T00:00:00Z", jobUID, true, "step"),
		podObject("pod-new", "2026-01-02T00:00:00Z", jobUID, true, "step"),
		podObject("pod-impostor", "2026-01-03T00:00:00Z", otherID, true, "step"), // matches the label, not the owner
	}
	rc, err := r.Open(context.Background(), target, logs.LiveQuery{})
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	reqs := f.Requests()
	if !strings.Contains(reqs[len(reqs)-1], "/pods/pod-new/log") {
		t.Fatalf("selected %s", reqs[len(reqs)-1])
	}
}

func TestReaderRejectsForgedTargetBeforeAnyRequest(t *testing.T) {
	f, r := newFake(t)
	for _, bad := range []logs.Target{
		{Namespace: "a/b", JobName: target.JobName, JobUID: jobUID, Container: "step"},
		{Namespace: "..", JobName: target.JobName, JobUID: jobUID, Container: "step"},
		{Namespace: target.Namespace, JobName: "x/../y", JobUID: jobUID, Container: "step"},
		{Namespace: target.Namespace, JobName: target.JobName, JobUID: "not-a-uid", Container: "step"},
		{Namespace: target.Namespace, JobName: target.JobName, JobUID: jobUID, Container: "step&follow=true"},
		{Namespace: "ns?labelSelector=x", JobName: target.JobName, JobUID: jobUID, Container: "step"},
	} {
		if _, err := r.Open(context.Background(), bad, logs.LiveQuery{}); !errors.Is(err, logs.ErrInvalid) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if reqs := f.Requests(); len(reqs) != 0 {
		t.Fatalf("a malformed target must never reach the API: %v", reqs)
	}
	if _, err := r.Open(context.Background(), target, logs.LiveQuery{TailLines: -1}); !errors.Is(err, logs.ErrInvalid) {
		t.Fatal("negative bound")
	}
}

func TestReaderBoundsTheStreamLocally(t *testing.T) {
	f, r := newFake(t)
	f.LogBody = strings.Repeat("0123456789", 1000) // the fake ignores limitBytes
	rc, err := r.Open(context.Background(), target, logs.LiveQuery{LimitBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, rc); len(got) != 100 {
		t.Fatalf("local bound not applied: %d bytes", len(got))
	}
}

func TestReaderCloseCancelsUpstream(t *testing.T) {
	f, r := newFake(t)
	f.Hold, f.Held, f.Gone = make(chan struct{}), make(chan struct{}), make(chan struct{})
	rc, err := r.Open(context.Background(), target, logs.LiveQuery{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	<-f.Held
	rc.Close()
	select {
	case <-f.Gone:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the stream must end the upstream request")
	}
}

func TestReaderContextEndsFollow(t *testing.T) {
	f, r := newFake(t)
	f.Hold, f.Held, f.Gone = make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	rc, err := r.Open(ctx, target, logs.LiveQuery{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	<-f.Held
	cancel()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("a cancelled read must fail, not look like end of log")
	}
}

func TestReaderUnreachableAPI(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	r, _ := NewReader(Config{Endpoint: url, Client: http.DefaultClient, AllowInsecure: true})
	_, err := r.Open(context.Background(), target, logs.LiveQuery{})
	if !errors.Is(err, logs.ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), url) {
		t.Fatalf("the endpoint must not appear in errors: %v", err)
	}
}

func TestNewReaderValidation(t *testing.T) {
	for _, cfg := range []Config{
		{Endpoint: "https://k8s.example", Client: nil},
		{Endpoint: "http://k8s.example", Client: http.DefaultClient},
		{Endpoint: "https://user:pw@k8s.example", Client: http.DefaultClient},
		{Endpoint: "https://k8s.example/prefix", Client: http.DefaultClient},
		{Endpoint: "https://k8s.example?x=1", Client: http.DefaultClient},
		{Endpoint: "", Client: http.DefaultClient},
	} {
		if _, err := NewReader(cfg); err == nil {
			t.Fatalf("%+v must be refused", cfg)
		}
	}
	if _, err := NewReader(Config{Endpoint: "https://k8s.example:6443/", Client: http.DefaultClient}); err != nil {
		t.Fatal(err)
	}
	_ = fmt.Sprint
}
