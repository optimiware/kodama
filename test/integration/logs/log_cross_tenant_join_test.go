package logs_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
)

// Test_log_cross_tenant_join (PX-10, AC-06, IT-03, IT-11, VS-11): foreign
// tenants, projects, runs, attempts, artifacts and runtime sessions are denied
// across the live, wrapper, controller and automatic paths before any backend
// (Kubernetes API or object store) is read; foreign linkage and copied
// completeness evidence are never accepted; membership is current on every
// call; and pooled database connections never carry one tenant's context into
// another's request. Real Kubernetes, PostgreSQL non-owner roles, S3 and the
// real handlers throughout.
func Test_log_cross_tenant_join(t *testing.T) {
	s := newStack(t)
	k := s.kit

	// Tenant A: a retained wrapper log and a controller capture. Tenant B: its own
	// retained wrapper log with content that must never reach tenant A.
	a := s.start(scopeA, stepSpec{Script: chatter("tenant-A")})
	s.waitTerminal(a, 5*time.Minute)
	recA, err := k.Ctrl.Observe(bg(), a.Scope, a.ID)
	recA = wantRetained(t, recA, err, logs.SourceWrapper)
	ctrlA, err := k.Ctrl.Collect(bg(), a.Scope, a.ID)
	wantRetained(t, ctrlA, err, logs.SourceController)
	b := s.start(scopeB, stepSpec{Script: "echo tenant-B-secret-output\n" + finishOK + "\n"})
	s.waitTerminal(b, 5*time.Minute)
	recB, err := k.Ctrl.Observe(bg(), b.Scope, b.ID)
	recB = wantRetained(t, recB, err, logs.SourceWrapper)
	if _, err := k.Ctrl.Collect(bg(), b.Scope, b.ID); err != nil {
		t.Fatal(err)
	}
	const foreignText = "tenant-B-secret-output"

	// Positive controls: owners read their own evidence.
	if r := k.get(engineer, a.Scope, a.Run, "attemptId="+string(a.ID)); r.status != http.StatusOK || r.meta(t)["origin"] != "wrapper" {
		t.Fatalf("tenant A cannot read its own log: %d %s", r.status, truncate(r.raw, 300))
	}
	if r := k.get(other, b.Scope, b.Run, "attemptId="+string(b.ID)); r.status != http.StatusOK || !strings.Contains(r.raw, foreignText) {
		t.Fatalf("tenant B cannot read its own log: %d %s", r.status, truncate(r.raw, 300))
	}

	// backendReads sums every read of a backend the log module could cause.
	var objOpens int
	k.Objects.Set(func(f *artifactsFaultObjects) {
		f.OnOpen = func(key string, next func() (artifactsObject, error)) (artifactsObject, error) {
			objOpens++
			return next()
		}
	})
	backend := func() [3]int { return [3]int{len(k.API.Requests()), k.Live.Opens(), objOpens} }

	t.Run("foreign_tenant_project_run_attempt_and_session", func(t *testing.T) {
		tokenA := ""
		if toks := s.srv.tokensOf(a.ID); len(toks) > 0 {
			tokenA = toks[0]
		}
		base := backend()
		sources := []string{"", "&source=live", "&source=wrapper", "&source=controller"}
		cases := []struct {
			name  string
			user  string
			scope domain.Scope
			run   string
			att   string
		}{
			{"another tenant's run under my tenant", engineer, a.Scope, string(b.Run), ""},
			{"another tenant's run with its attempt", engineer, a.Scope, string(b.Run), string(b.ID)},
			{"another tenant's attempt under my run", engineer, a.Scope, string(a.Run), string(b.ID)},
			{"my attempt under another tenant's run", engineer, a.Scope, string(b.Run), string(a.ID)},
			{"a run that does not exist", engineer, a.Scope, "run_missing", ""},
			{"an attempt that does not exist", engineer, a.Scope, string(a.Run), "att_missing"},
			{"another tenant's path", engineer, b.Scope, string(b.Run), string(b.ID)},
			{"another tenant's path with my run", other, b.Scope, string(a.Run), ""},
			{"another tenant's path with my attempt", other, b.Scope, string(a.Run), string(a.ID)},
			{"another project of my tenant", engineer, executiontest.ScopeA2, string(a.Run), string(a.ID)},
			{"a user without membership", executiontest.Stranger, a.Scope, string(a.Run), string(a.ID)},
			{"another tenant's member under my tenant", other, a.Scope, string(a.Run), string(a.ID)},
			{"another tenant's member with their own run", other, a.Scope, string(b.Run), string(b.ID)},
		}
		var message string
		for _, c := range cases {
			for _, src := range sources {
				q := src
				if c.att != "" {
					q = "attemptId=" + c.att + src
				} else {
					q = strings.TrimPrefix(src, "&")
				}
				r := k.get(c.user, c.scope, domain.RunID(c.run), q)
				if r.status != http.StatusNotFound {
					t.Errorf("%s %q: status %d: %s", c.name, src, r.status, truncate(r.raw, 200))
					continue
				}
				if code := r.errorCode(t); code != "not_found" {
					t.Errorf("%s %q: code %s", c.name, src, code)
				}
				var env struct{ Error struct{ Message string } }
				_ = json.Unmarshal([]byte(r.raw), &env)
				if message == "" {
					message = env.Error.Message
				} else if env.Error.Message != message {
					t.Errorf("%s %q: %q differs from %q: absence and foreignness must be indistinguishable", c.name, src, env.Error.Message, message)
				}
				noLeak(t, c.name, r.raw, foreignText, string(b.ID), string(recB.Link.Log.ID))
			}
		}
		if r := k.get("", a.Scope, a.Run, ""); r.status != http.StatusUnauthorized {
			t.Fatalf("no credential: %d", r.status)
		}
		// A runtime session of the attempt authorizes nothing on the log route: the
		// runtime token is not a user, whatever it carries.
		if tokenA != "" {
			if r := k.get(tokenA, a.Scope, a.Run, "attemptId="+string(a.ID)); r.status == http.StatusOK {
				t.Fatalf("a runtime token read logs: %s", truncate(r.raw, 200))
			}
		}
		if got := backend(); got != base {
			t.Fatalf("a denied request reached a backend (api, live, objects): %v -> %v; requests %v", base, got, k.API.Requests()[base[0]:])
		}
	})

	t.Run("invalid_linkage_and_foreign_artifacts", func(t *testing.T) {
		foreign, err := k.Reader.WrapperLog(bg(), b.Scope, b.ID) // tenant B's own, legitimate read
		must(t, err)
		base := backend()
		// Foreign artifact references are not found, under either tenant's attempt.
		if _, err := k.Retained.Open(bg(), a.Scope, a.ID, recB.Link.Log); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("A opening B's artifact: %v", err)
		}
		if _, err := k.Retained.Open(bg(), b.Scope, b.ID, recA.Link.Log); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("B opening A's artifact: %v", err)
		}
		if _, err := k.Retained.Open(bg(), b.Scope, a.ID, recA.Link.Log); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("A's attempt under B's scope: %v", err)
		}
		if _, err := k.Retained.Usable(bg(), a.Scope, a.ID, recB.Link.Log); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("usability of a foreign artifact: %v", err)
		}
		// The controller refuses foreign and crossed references and creates nothing.
		for _, c := range []struct {
			name  string
			scope domain.Scope
			id    domain.AttemptID
		}{{"B's scope with A's attempt", b.Scope, a.ID}, {"A's scope with B's attempt", a.Scope, b.ID}} {
			if _, err := k.Ctrl.Observe(bg(), c.scope, c.id); !errors.Is(err, logs.ErrNotFound) {
				t.Fatalf("%s observe: %v", c.name, err)
			}
			if _, err := k.Ctrl.Collect(bg(), c.scope, c.id); !errors.Is(err, logs.ErrNotFound) {
				t.Fatalf("%s collect: %v", c.name, err)
			}
		}
		if recs, err := k.Store.ForAttempt(bg(), b.Scope, a.ID); err != nil || len(recs) != 0 {
			t.Fatalf("A's records under B's scope: %d %v", len(recs), err)
		}
		if _, err := k.Store.Get(bg(), b.Scope, recA.ID); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("A's record under B's scope: %v", err)
		}
		// The persistence layer refuses a linkage to another tenant's artifacts.
		a3 := s.start(scopeA, stepSpec{Script: chatter("tenant-A3")}, faultSpec{Op: "upload", Name: "command.log", Status: http.StatusForbidden})
		s.waitTerminal(a3, 5*time.Minute)
		info, err := k.Attempts.ByID(bg(), a3.Scope, a3.ID)
		must(t, err)
		tmpl, err := logs.NewRecord(a3.Scope, "logrec_t064_foreign", a3.ID, a3.Run, info.Step, logs.SourceWrapper, 1, time.Now())
		must(t, err)
		rec, err := k.Store.Ensure(bg(), *tmpl)
		must(t, err)
		if _, err := k.Store.Update(bg(), a3.Scope, rec.ID, func(r *logs.Record) (bool, logs.Audit, error) {
			ch, err := r.Observe(foreign, time.Now())
			return ch, logs.Audit{Action: "log.linked"}, err
		}); err == nil {
			t.Fatal("a record of tenant A was linked to tenant B's artifacts")
		}
		if got, err := k.Store.Get(bg(), a3.Scope, rec.ID); err != nil || got.State != logs.StatePending || got.Link != nil {
			t.Fatalf("a rejected linkage must leave the record untouched: %+v %v", got, err)
		}
		if got := backend(); got != base {
			t.Fatalf("a foreign reference reached the object store: %v -> %v", base, got)
		}

		// Row-level security under the non-owner role: a tenant context sees only its
		// own records and artifacts; no context sees none.
		count := func(tenant domain.TenantID, sql string, args ...any) (n int) {
			t.Helper()
			err := s.x.RuntimeTx(tenant, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(&n) })
			must(t, err)
			return n
		}
		if n := count(b.Scope.TenantID, `SELECT count(*) FROM log_records WHERE attempt_id = $1`, string(a.ID)); n != 0 {
			t.Fatalf("tenant B sees %d of A's log records", n)
		}
		if n := count(b.Scope.TenantID, `SELECT count(*) FROM artifacts WHERE attempt_id = $1`, string(a.ID)); n != 0 {
			t.Fatalf("tenant B sees %d of A's artifacts", n)
		}
		if n := count(a.Scope.TenantID, `SELECT count(*) FROM log_records WHERE attempt_id = $1`, string(a.ID)); n != 2 {
			t.Fatalf("tenant A sees %d of its own records, want 2", n)
		}
		var bare int
		must(t, s.x.Runtime.QueryRow(bg(), `SELECT count(*) FROM log_records`).Scan(&bare))
		if bare != 0 {
			t.Fatalf("a connection without tenant context sees %d log records", bare)
		}
	})

	t.Run("copied_sidecar_never_inherits_completeness", func(t *testing.T) {
		// A workload of tenant B holds a valid runtime session and uploads its own log
		// together with a copy of tenant A's complete sidecar: the sidecar is bound to
		// A's artifact (ID, digest, size), so B's log stays unverified.
		b2 := s.start(scopeB, stepSpec{Script: "echo b2-running\nsleep 150\n" + finishOK + "\n"})
		s.waitRunning(b2, 5*time.Minute)
		toks := s.srv.tokensOf(b2.ID)
		if len(toks) == 0 {
			t.Fatal("no session token")
		}
		logBytes := []byte("forged tenant B output\n")
		copied := []byte(k.readArtifact(a, *recA.Link.Meta))
		s.rtUpload(b2, toks[0], contract.CommandLogName, contract.LogMediaType, logBytes)
		sidecarName, err := contract.MetadataName(contract.CommandLogName)
		must(t, err)
		s.rtUpload(b2, toks[0], sidecarName, contract.MediaType, copied)

		link, err := k.Reader.WrapperLog(bg(), b2.Scope, b2.ID)
		if err != nil {
			t.Fatalf("wrapper log: %v", err)
		}
		if link.Completeness == logs.Complete || link.Meta != nil {
			t.Fatalf("a copied sidecar must not establish completeness: %+v", link)
		}
		if link.Completeness != logs.Unknown || !hasReason(link.Reasons, contract.ReasonUnverified) {
			t.Fatalf("unbound evidence is unknown/unverified: %+v", link)
		}
		rec, err := k.Ctrl.Observe(bg(), b2.Scope, b2.ID)
		if err != nil || rec.Link.Completeness != logs.Unknown {
			t.Fatalf("observing a copied sidecar: %+v %v", rec, err)
		}
		if _, err := s.plain.Cancel(bg(), execution.Caller{PrincipalID: other, RequestID: "req_t064"}, b2.Scope, b2.Run); err != nil {
			t.Logf("cancel: %v", err) // the cleanup run has no human owner; the namespace is removed at the end
		}
	})

	t.Run("foreign_log_selector", func(t *testing.T) {
		r := s.start(scopeA, stepSpec{Script: "echo selector-target-output\nsleep 240\n" + finishOK + "\n"})
		s.waitRunning(r, 5*time.Minute)
		s.waitRawLog(r, "selector-target-output", 2*time.Minute)
		info, err := k.Attempts.ByID(bg(), r.Scope, r.ID)
		must(t, err)
		target, err := k.Work.Target(bg(), info)
		must(t, err)
		realPod := s.pod(r).Metadata.Name

		// Decoy Pods: one in the tenant's own namespace and one in another tenant's
		// namespace carrying the real Pod's exact name. Neither is ever read.
		s.decoyPod(s.nsA, "t064-decoy-"+randHex(2))
		s.decoyPod(s.nsB, realPod)
		mark := len(k.API.Requests())
		qa := "attemptId=" + string(r.ID)
		if res := k.get(engineer, r.Scope, r.Run, qa+"&source=live&tail=5"); res.status != http.StatusOK || !strings.Contains(res.raw, "selector-target-output") {
			t.Fatalf("positive control with decoys present: %d %s", res.status, truncate(res.raw, 300))
		}
		for _, req := range k.API.Requests()[mark:] {
			if strings.HasSuffix(req, "/log") && !strings.Contains(req, "/namespaces/"+s.nsA+"/pods/") {
				t.Fatalf("a log was read outside the attempt's namespace: %s", req)
			}
			if strings.HasSuffix(req, "/log") && strings.Contains(req, "decoy") {
				t.Fatalf("a decoy Pod's log was read: %s", req)
			}
		}

		// Caller-supplied selectors are rejected by the transport before any backend.
		base := backend()
		for _, q := range []string{
			qa + "&namespace=kube-system", qa + "&pod=" + realPod, qa + "&container=inject", qa + "&labelSelector=app%3Dvictim",
			qa + "&fieldSelector=metadata.namespace%3Dkube-system", qa + "&selector=tenant%3Db",
			qa + "&url=http%3A%2F%2F169.254.169.254%2F", qa + "&backend=https%3A%2F%2Fevil.example", qa + "&job=other-job",
			qa + "&previous=true", qa + "&timestamps=true", qa + "&limitBytes=999999999", qa + "&tailLines=5",
			qa + "&attemptId=" + string(r.ID), qa + "&attemptId=" + string(b.ID), qa + "&tail=3&tail=4", qa + "&follow=true&follow=false",
			qa + "&source=live&source=wrapper", qa + "&source=pods", qa + "&follow=yes", qa + "&tail=0", qa + "&tail=-1", qa + "&tail=1e3",
			"attemptId=..%2F..%2Fsecrets", "attemptId=att_1%0d%0aHost:evil",
		} {
			if res := k.get(engineer, r.Scope, r.Run, q); res.status != http.StatusBadRequest {
				t.Errorf("%q: status %d (%s)", q, res.status, truncate(res.raw, 200))
			}
		}
		if got := backend(); got != base {
			t.Fatalf("a rejected selector reached a backend: %v -> %v", base, got)
		}

		// The reader itself verifies the workload before reading a byte: another
		// namespace, a replacement Job UID, a container that is not the step's and
		// malformed identifiers are refused without a log request.
		logReads := func() int { return k.API.logRequests() }
		edited := target
		cases := []struct {
			name string
			edit func(*logs.Target)
			want error
		}{
			{"another tenant's namespace", func(x *logs.Target) { x.Namespace = s.nsB }, logs.ErrWorkloadMissing},
			{"a replacement Job UID", func(x *logs.Target) { x.JobUID = "00000000-0000-4000-8000-000000000000" }, logs.ErrWorkloadMismatch},
			{"the init container", func(x *logs.Target) { x.Container = kube.InjectContainer }, nil},
			{"a container that does not exist", func(x *logs.Target) { x.Container = "sidecar" }, nil},
			{"another Job name", func(x *logs.Target) { x.JobName = "kdm-" + strings.Repeat("a", 40) }, logs.ErrWorkloadMissing},
			{"a path in the namespace", func(x *logs.Target) { x.Namespace = "kube-system/pods/coredns" }, logs.ErrInvalid},
			{"a path in the Job name", func(x *logs.Target) { x.JobName = target.JobName + "/../../secrets" }, logs.ErrInvalid},
		}
		for _, c := range cases {
			edited = target
			c.edit(&edited)
			before := logReads()
			rc, err := k.Live.Open(bg(), edited, logs.LiveQuery{TailLines: 5, LimitBytes: 1 << 20})
			if err == nil {
				rc.Close()
				t.Errorf("%s: the reader opened a log", c.name)
				continue
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Errorf("%s: %v, want %v", c.name, err, c.want)
			}
			if got := logReads(); got != before {
				t.Errorf("%s: a log request was made for an unverified workload", c.name)
			}
		}
		// The service never serves, nor explains, a workload that is not the attempt's.
		wrong := &wrongUID{inner: k.Work}
		svc, err := logs.NewService(logs.Config{Authority: k.Authz, Attempts: k.Attempts, Workloads: wrong, Live: k.Live, Retained: k.Retained, Store: k.Store, Redactions: k.Redact, Limits: DefaultLimits()})
		must(t, err)
		before := logReads()
		if _, err := readService(svc, logs.Request{Principal: engineer, Scope: r.Scope, Run: r.Run, Attempt: r.ID, Mode: logs.ModeLive}); !errors.Is(err, logs.ErrUnavailable) {
			t.Fatalf("a replaced Job must be unavailable, not served: %v", err)
		}
		if logReads() != before {
			t.Fatal("a replaced Job's log was requested")
		}
		if _, err := s.plain.Cancel(bg(), execution.Caller{PrincipalID: engineer, RequestID: "req_t064"}, r.Scope, r.Run); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("revoked_membership_and_pooled_tenant_reuse", func(t *testing.T) {
		// One connection serves every tenant in turn: a tenant context never leaks
		// into the next request.
		single := s.newLogs(logsOptions{Pool: s.x.Base.DB.RuntimePool(bg(), 1)})
		for round := 0; round < 3; round++ {
			if r := single.get(engineer, a.Scope, a.Run, "attemptId="+string(a.ID)); r.status != http.StatusOK || strings.Contains(r.raw, foreignText) {
				t.Fatalf("round %d: A reads A: %d", round, r.status)
			}
			if r := single.get(other, b.Scope, b.Run, "attemptId="+string(b.ID)); r.status != http.StatusOK || !strings.Contains(r.raw, foreignText) {
				t.Fatalf("round %d: B reads B: %d", round, r.status)
			}
			if r := single.get(other, a.Scope, a.Run, "attemptId="+string(a.ID)); r.status != http.StatusNotFound || strings.Contains(r.raw, "tenant-A") {
				t.Fatalf("round %d: B reads A: %d", round, r.status)
			}
			if r := single.get(engineer, b.Scope, b.Run, "attemptId="+string(b.ID)); r.status != http.StatusNotFound || strings.Contains(r.raw, foreignText) {
				t.Fatalf("round %d: A reads B: %d", round, r.status)
			}
		}
		// Membership is current on every call: revocation ends access to retained
		// and live reads at once.
		viewer := executiontest.Viewer
		for _, q := range []string{"attemptId=" + string(a.ID), "attemptId=" + string(a.ID) + "&source=live", "attemptId=" + string(a.ID) + "&source=controller"} {
			if r := k.get(viewer, a.Scope, a.Run, q); r.status != http.StatusOK {
				t.Fatalf("viewer %q before revocation: %d %s", q, r.status, truncate(r.raw, 200))
			}
		}
		s.x.Revoke(a.Scope, viewer)
		base := backend()
		for _, q := range []string{"attemptId=" + string(a.ID), "attemptId=" + string(a.ID) + "&source=live", "attemptId=" + string(a.ID) + "&source=controller", "attemptId=" + string(a.ID) + "&source=wrapper"} {
			if r := k.get(viewer, a.Scope, a.Run, q); r.status != http.StatusNotFound {
				t.Fatalf("viewer %q after revocation: %d %s", q, r.status, truncate(r.raw, 200))
			}
		}
		if got := backend(); got != base {
			t.Fatalf("a revoked member's request reached a backend: %v -> %v", base, got)
		}
	})
}

// wrongUID resolves the right workload but with a Job UID that is not the one
// the attempt's Job has (a replacement Job of the same name).
type wrongUID struct{ inner logs.Workloads }

func (w *wrongUID) Target(ctx context.Context, a logs.AttemptInfo) (logs.Target, error) {
	tg, err := w.inner.Target(ctx, a)
	tg.JobUID = "00000000-0000-4000-8000-000000000000"
	return tg, err
}

// decoyPod creates a Pod the Job controller did not create.
func (s *stack) decoyPod(ns, name string) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	must(s.t, s.c.api.create(ctx, "/api/v1/namespaces/"+ns+"/pods", map[string]any{
		"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"restartPolicy": "Never", "automountServiceAccountToken": false, "serviceAccountName": Prefix + "-runner",
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"containers": []map[string]any{{
				"name": kube.PrimaryContainer, "image": s.c.workload, "command": []string{"sh", "-c", "echo decoy-pod-output; sleep 300"},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"resources":       map[string]any{"limits": map[string]string{"cpu": "50m", "memory": "16Mi", "ephemeral-storage": "16Mi"}, "requests": map[string]string{"cpu": "5m", "memory": "8Mi", "ephemeral-storage": "8Mi"}},
			}},
		},
	}))
}

// rtClient reaches the Pod-facing endpoint from the test process, verifying the
// server certificate the Pods trust.
func (s *stack) rtClient() *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(s.srv.CA)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(s.srv.URL, "https://"))
	return &http.Client{Timeout: time.Minute, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: s.c.hostIP, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+port)
		},
	}}
}

// rtUpload acts as a workload holding the attempt's runtime token: reserve a
// diagnostic file, transfer it through the signed instruction and complete it.
func (s *stack) rtUpload(a *att, token, name, mediaType string, data []byte) {
	s.t.Helper()
	hc := s.rtClient()
	size := int64(len(data))
	digest := artifacts.DigestOf(data)
	post := func(path string, body any) (int, []byte) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, s.srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := hc.Do(req)
		must(s.t, err)
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
	code, body := post("/runtime/v1/attempts/"+string(a.ID)+"/artifacts", runtimev1.ArtifactReserveRequest{
		Name: name, Kind: runtimev1.KindFile, Role: runtimev1.RoleDiagnostic, Digest: runtimev1.Digest(digest), MediaType: &mediaType, SizeBytes: &size,
	})
	if code != http.StatusCreated && code != http.StatusOK {
		s.t.Fatalf("reserve %s: %d %s", name, code, truncate(string(body), 300))
	}
	var res runtimev1.ArtifactReserveResponse
	must(s.t, json.Unmarshal(body, &res))
	if res.Upload == nil {
		s.t.Fatal("no upload instruction")
	}
	if _, err := url.Parse(res.Upload.URL); err != nil {
		s.t.Fatal(err)
	}
	req, _ := http.NewRequest(res.Upload.Method, res.Upload.URL, bytes.NewReader(data))
	req.ContentLength = size
	for k, v := range res.Upload.Headers {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Host") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	must(s.t, err)
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("transfer %s: %d %s", name, resp.StatusCode, b)
	}
	code, body = post("/runtime/v1/attempts/"+string(a.ID)+"/artifacts/"+string(res.ArtifactID)+"/complete", runtimev1.ArtifactCompleteRequest{Digest: runtimev1.Digest(digest), SizeBytes: size})
	if code != http.StatusOK {
		s.t.Fatalf("complete %s: %d %s", name, code, truncate(string(body), 300))
	}
}
