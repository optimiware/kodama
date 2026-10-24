package logshttp_test

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
	"github.com/optimiware/kodama/internal/logs/logstest"
)

func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %06d\n", i)
	}
	return b.String()
}

// Test_foreign_log_selector (IT-03, VS-11): copied foreign tenant, project, run,
// attempt and artifact references are denied by real membership and scoped
// persistence; arbitrary Kubernetes selectors never reach Kubernetes; missing
// and foreign resources answer identically. Real PostgreSQL and S3; the
// Kubernetes API is the recording fake and the real reader.
func Test_foreign_log_selector(t *testing.T) {
	e := logstest.New(t)
	a := e.NewAttempt(logstest.ScopeA, true)
	b := e.NewAttempt(logstest.ScopeB, true)
	e.WrapperLog(b, "tenant B secret output\n", contract.Complete)
	r := newRig(t, a, e, rigOptions{})

	base := r.read(logstest.Engineer, a.Scope, a.Run, "attemptId="+string(a.ID))
	if base.status != http.StatusOK {
		t.Fatalf("baseline: %d %s", base.status, trunc(base.raw))
	}
	baseline := len(r.kube.Requests())
	if baseline != 3 {
		t.Fatalf("a legitimate read asks the API for the Job, its Pods and the log: %v", r.kube.Requests())
	}
	for _, req := range r.kube.Requests() {
		if !strings.Contains(req, "/namespaces/"+logstest.Namespace+"/") {
			t.Fatalf("every request stays in the trusted namespace: %s", req)
		}
	}

	t.Run("foreign references are the same 404 as missing ones", func(t *testing.T) {
		cases := map[string]struct {
			user  string
			scope domain.Scope
			run   domain.RunID
			query string
		}{
			"another tenant's run under my tenant":       {logstest.Engineer, a.Scope, b.Run, ""},
			"another tenant's run with its attempt":      {logstest.Engineer, a.Scope, b.Run, "attemptId=" + string(b.ID)},
			"another tenant's attempt under my run":      {logstest.Engineer, a.Scope, a.Run, "attemptId=" + string(b.ID)},
			"my attempt under another tenant's run":      {logstest.Engineer, a.Scope, b.Run, "attemptId=" + string(a.ID)},
			"a run that does not exist":                  {logstest.Engineer, a.Scope, "run_missing", ""},
			"an attempt that does not exist":             {logstest.Engineer, a.Scope, a.Run, "attemptId=att_missing"},
			"another tenant's path":                      {logstest.Engineer, b.Scope, b.Run, ""},
			"another tenant's path with my run":          {logstest.OtherEng, b.Scope, a.Run, ""},
			"another project of my tenant":               {logstest.Engineer, logstest.ScopeA2, a.Run, ""},
			"a user with no membership":                  {logstest.Stranger, a.Scope, a.Run, ""},
			"another tenant's member under my tenant":    {logstest.OtherEng, a.Scope, a.Run, ""},
			"another tenant's member with their own run": {logstest.OtherEng, a.Scope, b.Run, "attemptId=" + string(b.ID)},
		}
		var message string
		for name, c := range cases {
			res := r.read(c.user, c.scope, c.run, c.query)
			if res.status != http.StatusNotFound {
				t.Errorf("%s: status %d: %s", name, res.status, trunc(res.raw))
				continue
			}
			code, msg := res.errorCode(t)
			if code != "not_found" {
				t.Errorf("%s: code %s", name, code)
			}
			if message == "" {
				message = msg
			} else if msg != message {
				t.Errorf("%s: %q differs from %q: absence and foreignness must be indistinguishable", name, msg, message)
			}
			if strings.Contains(res.raw, "tenant B secret") || strings.Contains(res.raw, string(b.ID)) {
				t.Errorf("%s: the response discloses foreign data: %s", name, res.raw)
			}
		}
		if res := r.read("", a.Scope, a.Run, ""); res.status != http.StatusUnauthorized {
			t.Fatalf("no credential: %d", res.status)
		}
		if got := len(r.kube.Requests()); got != baseline {
			t.Fatalf("a denied or foreign request reached Kubernetes: %v", r.kube.Requests()[baseline:])
		}
	})

	t.Run("arbitrary selectors never reach Kubernetes", func(t *testing.T) {
		attempt := "attemptId=" + string(a.ID)
		for _, q := range []string{
			attempt + "&namespace=kube-system",
			attempt + "&pod=coredns-1",
			attempt + "&container=sidecar",
			attempt + "&labelSelector=app%3Dvictim",
			attempt + "&fieldSelector=metadata.namespace%3Dkube-system",
			attempt + "&selector=tenant%3Db",
			attempt + "&url=http%3A%2F%2F169.254.169.254%2F",
			attempt + "&backend=https%3A%2F%2Fevil.example",
			attempt + "&job=other-job",
			attempt + "&previous=true",
			attempt + "&timestamps=true",
			attempt + "&limitBytes=999999999",
			attempt + "&tailLines=5",
			attempt + "&attemptId=" + string(a.ID), // duplicate
			attempt + "&attemptId=" + string(b.ID), // ambiguous selection
			attempt + "&tail=3&tail=4",
			attempt + "&follow=true&follow=false",
			attempt + "&source=live&source=wrapper",
			attempt + "&source=pods",
			attempt + "&follow=yes",
			attempt + "&tail=0", attempt + "&tail=-1", attempt + "&tail=1e3", attempt + "&tail=%20", attempt + "&tail=", attempt + "&tail=%2B5",
			"attemptId=..%2F..%2Fsecrets",
			"attemptId=att_1%0d%0aHost:evil",
		} {
			res := r.read(logstest.Engineer, a.Scope, a.Run, q)
			if res.status != http.StatusBadRequest {
				t.Errorf("%q: status %d (%s)", q, res.status, trunc(res.raw))
			}
		}
		if got := len(r.kube.Requests()); got != baseline {
			t.Fatalf("a rejected request reached Kubernetes: %v", r.kube.Requests()[baseline:])
		}
	})

	t.Run("a foreign tenant's retained log is never served", func(t *testing.T) {
		if res := r.read(logstest.Engineer, a.Scope, a.Run, "attemptId="+string(a.ID)+"&source=wrapper"); res.status != http.StatusNotFound || strings.Contains(res.raw, "tenant B secret") {
			t.Fatalf("%d %s", res.status, trunc(res.raw))
		}
		// Positive control: the owner reads its own retained log.
		e.Finish(b)
		rb := newRig(t, b, e, rigOptions{})
		res := rb.read(logstest.OtherEng, b.Scope, b.Run, "attemptId="+string(b.ID))
		if res.status != http.StatusOK || res.meta(t)["origin"] != "wrapper" || len(res.lines()) != 1 || res.lines()[0] != "tenant B secret output" {
			t.Fatalf("%d %s", res.status, trunc(res.raw))
		}
		// The persistence layer refuses a record that links another tenant's artifact.
		info := e.Info(a)
		tmpl, _ := logs.NewRecord(a.Scope, "logrec_foreign_link", a.ID, a.Run, info.Step, logs.SourceWrapper, 1, time.Now())
		rec, err := e.Logs.Ensure(bg(), *tmpl)
		if err != nil {
			t.Fatal(err)
		}
		foreign, err := rb.env.Reader.WrapperLog(bg(), b.Scope, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Logs.Update(bg(), a.Scope, rec.ID, func(r *logs.Record) (bool, logs.Audit, error) {
			ch, err := r.Observe(foreign, time.Now())
			return ch, logs.Audit{Action: "log.linked"}, err
		}); err == nil {
			t.Fatal("tenant A's record linked tenant B's artifact")
		}
	})

	t.Run("several attempts need an explicit one", func(t *testing.T) {
		two := e.NewAttempts(logstest.ScopeA, 2, true)
		rr := newRig(t, two[0], e, rigOptions{})
		res := rr.read(logstest.Engineer, two[0].Scope, two[0].Run, "")
		if res.status != http.StatusBadRequest || !strings.Contains(res.raw, "attemptId is required") {
			t.Fatalf("%d %s", res.status, trunc(res.raw))
		}
		if len(rr.kube.Requests()) != 0 {
			t.Fatal("an ambiguous selection must not reach Kubernetes")
		}
		if res := rr.read(logstest.Engineer, two[0].Scope, two[0].Run, "attemptId="+string(two[0].ID)); res.status != http.StatusOK {
			t.Fatalf("%d %s", res.status, trunc(res.raw))
		}
	})
}

// Test_bounded_tail (IT-11, AC-06): tail, byte, line, duration and concurrency
// bounds hold on the live path through the real reader and on the retained path
// through real artifacts, with explicit truncation, slow-consumer and
// disconnect handling, bounded framing and split-secret redaction.
func Test_bounded_tail(t *testing.T) {
	e := logstest.New(t)
	a := e.NewAttempt(logstest.ScopeA, true)
	q := "attemptId=" + string(a.ID)

	limits := func(mut func(*logs.Limits)) logs.Limits {
		l := logstest.Limits()
		if mut != nil {
			mut(&l)
		}
		return l
	}

	t.Run("the default tail is conservative and is sent to Kubernetes", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{})
		r.kube.Set(func(k *kubefakeAPI) { k.LogBody = numbered(10) })
		res := r.read(logstest.Engineer, a.Scope, a.Run, q)
		if res.status != http.StatusOK {
			t.Fatalf("%d %s", res.status, trunc(res.raw))
		}
		m := res.meta(t)
		if m["tail"] != float64(100) || m["origin"] != "live" || m["completeness"] != "unknown" || m["follow"] != false {
			t.Fatalf("meta = %v", m)
		}
		logReq := r.kube.LogRequests()
		if len(logReq) != 1 || !strings.Contains(logReq[0], "tailLines=100") || !strings.Contains(logReq[0], "container=step") || strings.Contains(logReq[0], "follow") {
			t.Fatalf("log request = %v", logReq)
		}
		s := res.summary(t)
		if s["stop"] != "end" || s["truncated"] != false || s["lines"] != float64(10) {
			t.Fatalf("summary = %v", s)
		}
		for k, want := range map[string]string{"Content-Type": "application/x-ndjson", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"} {
			if res.header.Get(k) != want {
				t.Errorf("%s = %q", k, res.header.Get(k))
			}
		}
		if !strings.HasPrefix(res.header.Get("X-Request-ID"), "req_") {
			t.Error("a request id is returned")
		}
	})

	t.Run("a tail above the ceiling is rejected, never clamped", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{limits: limits(func(l *logs.Limits) { l.MaxTail, l.DefaultTail = 50, 20 })})
		if res := r.read(logstest.Engineer, a.Scope, a.Run, q+"&tail=51"); res.status != http.StatusBadRequest {
			t.Fatalf("%d", res.status)
		}
		r.kube.Set(func(k *kubefakeAPI) { k.LogBody = numbered(3) })
		res := r.read(logstest.Engineer, a.Scope, a.Run, q+"&tail=50")
		if res.status != http.StatusOK || !strings.Contains(r.kube.LogRequests()[0], "tailLines=50") {
			t.Fatalf("%d %v", res.status, r.kube.LogRequests())
		}
	})

	t.Run("the line bound ends a read that ignores the tail", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{limits: limits(func(l *logs.Limits) { l.MaxLines = 200 })})
		r.kube.Set(func(k *kubefakeAPI) { k.LogBody = numbered(5000) }) // a server that ignores tailLines
		res := r.read(logstest.Engineer, a.Scope, a.Run, q+"&tail=200")
		s := res.summary(t)
		if len(res.lines()) != 200 || s["stop"] != "line_limit" || s["truncated"] != true {
			t.Fatalf("lines=%d summary=%v", len(res.lines()), s)
		}
	})

	t.Run("the byte bound is explicit", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{limits: limits(func(l *logs.Limits) { l.MaxBytes = 4096; l.MaxLineBytes = 512 })})
		r.kube.Set(func(k *kubefakeAPI) { k.LogBody = strings.Repeat(strings.Repeat("x", 99)+"\n", 500) })
		res := r.read(logstest.Engineer, a.Scope, a.Run, q)
		s := res.summary(t)
		total := 0
		for _, l := range res.lines() {
			total += len(l)
		}
		if total > 4096 || s["stop"] != "byte_limit" || s["truncated"] != true || s["bytes"].(float64) > 4096 {
			t.Fatalf("bytes=%d summary=%v", total, s)
		}
	})

	t.Run("oversized lines are cut and framing stays bounded", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{limits: limits(func(l *logs.Limits) { l.MaxLineBytes = 1024 })})
		r.kube.Set(func(k *kubefakeAPI) { k.LogBody = strings.Repeat("y", 200<<10) + "\nafter\n" })
		res := r.read(logstest.Engineer, a.Scope, a.Run, q)
		lines := res.lines()
		if len(lines) != 2 || len(lines[0]) != 1024 || lines[1] != "after" {
			t.Fatalf("lines = %d (%d bytes first)", len(lines), len(lines[0]))
		}
		if res.records[1]["lineTruncated"] != true || res.summary(t)["truncated"] != true {
			t.Fatal("the cut must be explicit")
		}
		for _, ln := range strings.Split(res.raw, "\n") {
			if len(ln) > 8*1024 {
				t.Fatalf("a framing record of %d bytes exceeds the bound", len(ln))
			}
		}
	})

	t.Run("an endless line stops at the scan bound and closes the upstream", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{limits: limits(func(l *logs.Limits) { l.MaxBytes = 32 << 10; l.MaxLineBytes = 256 })})
		gone := make(chan struct{})
		var written int64
		var mu sync.Mutex
		r.kube.Set(func(k *kubefakeAPI) {
			k.LogHandler = func(w http.ResponseWriter, req *http.Request) {
				defer close(gone)
				chunk := []byte(strings.Repeat("z", 4096))
				for req.Context().Err() == nil {
					n, err := w.Write(chunk)
					mu.Lock()
					written += int64(n)
					mu.Unlock()
					if err != nil {
						return
					}
				}
			}
		})
		res := r.read(logstest.Engineer, a.Scope, a.Run, q)
		s := res.summary(t)
		if s["stop"] != "scan_limit" || s["truncated"] != true {
			t.Fatalf("summary = %v", s)
		}
		waitClosed(t, gone, "the upstream request")
		mu.Lock()
		defer mu.Unlock()
		if written > 8<<20 {
			t.Fatalf("an endless source was read for %d bytes despite a %d byte scan bound", written, 16*(32<<10))
		}
	})

	t.Run("the duration bound ends a follow and closes the upstream", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{limits: limits(func(l *logs.Limits) { l.MaxDuration = time.Second })})
		gone := make(chan struct{})
		r.kube.Set(func(k *kubefakeAPI) {
			k.LogHandler = func(w http.ResponseWriter, req *http.Request) {
				defer close(gone)
				fmt.Fprint(w, "first\n")
				w.(http.Flusher).Flush()
				<-req.Context().Done()
			}
		})
		start := time.Now()
		res := r.read(logstest.Engineer, a.Scope, a.Run, q+"&follow=true")
		if res.summary(t)["stop"] != "duration_limit" || time.Since(start) > 10*time.Second {
			t.Fatalf("%v after %s", res.summary(t), time.Since(start))
		}
		if got := res.lines(); len(got) != 1 || got[0] != "first" {
			t.Fatalf("lines = %v", got)
		}
		waitClosed(t, gone, "the upstream request")
		if !strings.Contains(r.kube.LogRequests()[0], "follow=true") {
			t.Fatal("follow must be forwarded")
		}
	})

	t.Run("a slow consumer is cut off and the upstream closed", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{
			limits: limits(func(l *logs.Limits) {
				l.MaxBytes = 1 << 30
				l.MaxLines = 1 << 24
				l.MaxScanBytes = 1 << 40
				l.MaxDuration = 60 * time.Second
			}),
			writeTimeout: 300 * time.Millisecond,
		})
		gone := make(chan struct{})
		r.kube.Set(func(k *kubefakeAPI) {
			k.LogHandler = func(w http.ResponseWriter, req *http.Request) {
				defer close(gone)
				line := []byte(strings.Repeat("s", 1023) + "\n")
				for req.Context().Err() == nil {
					if _, err := w.Write(line); err != nil {
						return
					}
				}
			}
		})
		conn, err := net.Dial("tcp", r.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.(*net.TCPConn).SetReadBuffer(1024)
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\n\r\n", strings.TrimPrefix(r.url(a.Scope, a.Run, q), r.srv.URL), logstest.Engineer)
		// The client never reads: the server must give up on its own.
		waitClosedFor(t, gone, 30*time.Second, "the upstream request of a client that stopped reading")
	})

	t.Run("a disconnect releases the upstream and the stream slot", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{limits: limits(func(l *logs.Limits) { l.MaxStreams = 1 })})
		gone := make(chan struct{})
		r.kube.Set(func(k *kubefakeAPI) {
			k.LogHandler = func(w http.ResponseWriter, req *http.Request) {
				defer close(gone)
				for req.Context().Err() == nil {
					fmt.Fprint(w, "tick\n")
					w.(http.Flusher).Flush()
					time.Sleep(20 * time.Millisecond)
				}
			}
		})
		resp := r.get(logstest.Engineer, a.Scope, a.Run, q+"&follow=true")
		buf := make([]byte, 64)
		if _, err := resp.Body.Read(buf); err != nil {
			t.Fatal(err)
		}
		// The only slot is taken: a second read is throttled, not queued.
		second := r.read(logstest.Engineer, a.Scope, a.Run, q)
		if second.status != http.StatusTooManyRequests || second.header.Get("Retry-After") == "" {
			t.Fatalf("second stream: %d %s", second.status, trunc(second.raw))
		}
		if code, _ := second.errorCode(t); code != "rate_limited" {
			t.Fatalf("code = %s", code)
		}
		resp.Body.Close()
		waitClosed(t, gone, "the upstream request after the client left")
		r.kube.Set(func(k *kubefakeAPI) { k.LogHandler = nil; k.LogBody = "ok\n" })
		deadline := time.Now().Add(10 * time.Second)
		for {
			res := r.read(logstest.Engineer, a.Scope, a.Run, q)
			if res.status == http.StatusOK {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the slot was never released: %d", res.status)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("a known value split across chunks never appears", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{})
		e.Redact.Set("hunter2-secret")
		t.Cleanup(func() { e.Redact.Set() })
		r.kube.Set(func(k *kubefakeAPI) {
			k.LogHandler = func(w http.ResponseWriter, req *http.Request) {
				for _, part := range []string{"pwd=hun", "ter2-", "sec", "ret ok\nsecond hunter2", "-secret done\n"} {
					fmt.Fprint(w, part)
					w.(http.Flusher).Flush()
					time.Sleep(20 * time.Millisecond)
				}
			}
		})
		res := r.read(logstest.Engineer, a.Scope, a.Run, q)
		if strings.Contains(res.raw, "hunter2") || strings.Contains(res.raw, "secret ok") {
			t.Fatalf("a secret leaked: %s", res.raw)
		}
		got := res.lines()
		if len(got) != 2 || got[0] != "pwd=[REDACTED] ok" || got[1] != "second [REDACTED] done" {
			t.Fatalf("lines = %q", got)
		}
		// When the values cannot be established nothing is emitted.
		e.Redact.Fail(fmt.Errorf("secret store down"))
		bad := r.read(logstest.Engineer, a.Scope, a.Run, q)
		e.Redact.Fail(nil)
		if bad.status != http.StatusServiceUnavailable || strings.Contains(bad.raw, "pwd=") {
			t.Fatalf("%d %s", bad.status, trunc(bad.raw))
		}
	})

	t.Run("the retained path returns exactly the requested tail of a finished attempt", func(t *testing.T) {
		fin := e.NewAttempt(logstest.ScopeA, true)
		e.WrapperLog(fin, numbered(3000), contract.Complete)
		e.Finish(fin)
		r := newRig(t, fin, e, rigOptions{})
		fq := "attemptId=" + string(fin.ID)

		res := r.read(logstest.Engineer, fin.Scope, fin.Run, fq+"&tail=3")
		m := res.meta(t)
		if m["origin"] != "wrapper" || m["completeness"] != "complete" || m["terminal"] != true {
			t.Fatalf("meta = %v", m)
		}
		if got := res.lines(); len(got) != 3 || got[0] != "line 002998" || got[2] != "line 003000" {
			t.Fatalf("lines = %v", got)
		}
		if s := res.summary(t); s["stop"] != "end" || s["truncated"] != false {
			t.Fatalf("summary = %v", s)
		}
		def := r.read(logstest.Engineer, fin.Scope, fin.Run, fq)
		if got := def.lines(); len(got) != 100 || got[0] != "line 002901" || got[99] != "line 003000" {
			t.Fatalf("the default tail is 100 lines: %d", len(got))
		}
		if len(r.kube.Requests()) != 0 {
			t.Fatalf("a retained read of a finished attempt does not touch Kubernetes: %v", r.kube.Requests())
		}
		// follow only makes sense for a running attempt.
		if res := r.read(logstest.Engineer, fin.Scope, fin.Run, fq+"&follow=true"); res.status != http.StatusBadRequest {
			t.Fatalf("follow on a finished attempt: %d", res.status)
		}
		if res := r.read(logstest.Engineer, fin.Scope, fin.Run, fq+"&source=wrapper&follow=true"); res.status != http.StatusBadRequest {
			t.Fatalf("follow on a retained source: %d", res.status)
		}
	})

	t.Run("retained output honors the byte bound while keeping the newest lines", func(t *testing.T) {
		fin := e.NewAttempt(logstest.ScopeA, true)
		e.WrapperLog(fin, numbered(500), contract.Partial, contract.ReasonTruncated)
		e.Finish(fin)
		r := newRig(t, fin, e, rigOptions{limits: limits(func(l *logs.Limits) { l.MaxBytes = 1024; l.MaxLineBytes = 64 })})
		res := r.read(logstest.Engineer, fin.Scope, fin.Run, "attemptId="+string(fin.ID)+"&tail=100")
		if res.meta(t)["completeness"] != "partial" {
			t.Fatalf("a partial capture says so: %v", res.meta(t))
		}
		got := res.lines()
		if len(got) == 0 || got[len(got)-1] != "line 000500" || res.summary(t)["truncated"] != true {
			t.Fatalf("lines=%d last=%v summary=%v", len(got), got[len(got)-1:], res.summary(t))
		}
		total := 0
		for _, l := range got {
			total += len(l)
		}
		if total > 1024 {
			t.Fatalf("emitted %d bytes under a 1024 bound", total)
		}
	})

	t.Run("a legacy log without metadata is unknown, and sources are not merged", func(t *testing.T) {
		fin := e.NewAttempt(logstest.ScopeA, true)
		e.LegacyWrapperLog(fin, "legacy line\n")
		e.Finish(fin)
		r := newRig(t, fin, e, rigOptions{})
		res := r.read(logstest.Engineer, fin.Scope, fin.Run, "attemptId="+string(fin.ID))
		m := res.meta(t)
		if m["completeness"] != "unknown" || m["origin"] != "wrapper" || len(m["reasons"].([]any)) != 1 {
			t.Fatalf("meta = %v", m)
		}
		if res := r.read(logstest.Engineer, fin.Scope, fin.Run, "attemptId="+string(fin.ID)+"&source=controller"); res.status != http.StatusNotFound {
			t.Fatalf("no controller capture exists and none is invented: %d", res.status)
		}
	})
}
