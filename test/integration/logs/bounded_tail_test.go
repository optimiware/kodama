package logs_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/logs"
)

// boundedTail is Test_retained_log_join/bounded_tail: the tail, line, byte and
// scan bounds of retained and live reads on real logs, the NDJSON framing (a
// meta record, chunks and a summary whose absence means interruption), rejection
// of oversized requests before any backend read, and the same answers after a
// restart of the composition with the workload gone.
func boundedTail(t *testing.T, s *stack) {
	k := s.kit
	// A retained log of 300 numbered lines and one long final line.
	script := numbered("tail", 300) + `echo "$(printf 'L%.0s' $(seq 1 200))"` + "\n" + finishOK + "\n"
	a := s.start(scopeA, stepSpec{Script: script})
	s.waitTerminal(a, 5*time.Minute)
	rec, err := k.Ctrl.Observe(bg(), a.Scope, a.ID)
	rec = wantRetained(t, rec, err, logs.SourceWrapper)
	if rec.Link.Completeness != logs.Complete {
		t.Fatalf("setup: %+v", rec.Link)
	}
	longLine := strings.Repeat("L", 200)
	q := "attemptId=" + string(a.ID)

	t.Run("defaults_and_explicit_tails", func(t *testing.T) {
		// Default tail: 100 lines (a conservative documented default).
		r := k.get(engineer, a.Scope, a.Run, q)
		if r.status != http.StatusOK {
			t.Fatalf("default tail: %d %s", r.status, truncate(r.raw, 300))
		}
		meta, sum := r.meta(t), r.summary(t)
		lines := r.lines()
		if len(lines) != 100 || lines[0] != "tail-line-0202" || lines[98] != "tail-line-0300" || lines[99] != longLine {
			t.Fatalf("default tail: %d lines, first %q, last %q", len(lines), lines[0], lines[len(lines)-1])
		}
		if meta["origin"] != "wrapper" || meta["completeness"] != "complete" || meta["tail"] != float64(100) || meta["terminal"] != true {
			t.Fatalf("meta: %+v", meta)
		}
		// Selecting the tail is not truncation: `truncated` reports that a query
		// bound (bytes, line length, scan) cut the output, never the tail selection.
		if sum["truncated"] != false || sum["stop"] != "end" || sum["lines"] != float64(100) {
			t.Fatalf("a tail selection ends cleanly: %+v", sum)
		}
		// Explicit tails, including the ceiling.
		r = k.get(engineer, a.Scope, a.Run, q+"&tail=3")
		if got := r.lines(); len(got) != 3 || got[0] != "tail-line-0299" || got[2] != longLine {
			t.Fatalf("tail=3: %v", got)
		}
		r = k.get(engineer, a.Scope, a.Run, q+"&tail=400")
		if got := r.lines(); len(got) != 301 || got[0] != "tail-line-0001" || r.summary(t)["truncated"] != false {
			t.Fatalf("tail=400 reads the whole log: %d lines, summary %+v", len(got), r.summary(t))
		}
	})

	t.Run("oversized_and_malformed_tails_are_rejected_before_any_backend_read", func(t *testing.T) {
		api, opens := len(k.API.Requests()), k.Live.Opens()
		objOpens := 0
		k.Objects.Set(func(f *artifactsFaultObjects) {
			f.OnOpen = func(key string, next func() (artifactsObject, error)) (artifactsObject, error) {
				objOpens++
				return next()
			}
		})
		defer k.Objects.Set(func(f *artifactsFaultObjects) { f.OnOpen = nil })
		for _, tail := range []string{"401", "100000", "0", "-1", "1e2", "abc", "", "%2B5", "%205"} {
			r := k.get(engineer, a.Scope, a.Run, q+"&tail="+tail)
			if r.status != http.StatusBadRequest {
				t.Errorf("tail=%q: status %d %s", tail, r.status, truncate(r.raw, 200))
			}
		}
		if r := k.get(engineer, a.Scope, a.Run, q+"&tail=2&tail=3"); r.status != http.StatusBadRequest {
			t.Errorf("duplicate tail: %d", r.status)
		}
		// The service enforces the same bound when the transport is bypassed.
		if _, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Tail: ptr(401)}); err == nil {
			t.Error("the service accepted an oversized tail instead of rejecting it")
		}
		if got := len(k.API.Requests()); got != api || k.Live.Opens() != opens || objOpens != 0 {
			t.Fatalf("a rejected request reached a backend: api %d->%d, live %d->%d, objects %d", api, got, opens, k.Live.Opens(), objOpens)
		}
	})

	t.Run("line_byte_and_line_count_bounds", func(t *testing.T) {
		lim := logs.Limits{DefaultTail: 10, MaxTail: 20, MaxLines: 20, MaxBytes: 100, MaxLineBytes: 16, MaxScanBytes: 1 << 20, MaxDuration: 20 * time.Second, MaxStreams: 2, WriteTimeout: 5 * time.Second}
		small := s.newLogs(logsOptions{Limits: &lim})
		// The long last line is cut at the line bound and flagged.
		got, err := readService(small.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Tail: ptr(2)})
		if err != nil || len(got.Lines) != 2 || len(got.Lines[1]) > 16 || !got.Cut[1] || got.Cut[0] || got.Summary.Truncated != true {
			t.Fatalf("line bound: %+v %v", got, err)
		}
		// 20 lines of 15 bytes exceed the 100-byte bound: the read stops at it and says so.
		got, err = readService(small.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Tail: ptr(20)})
		if err != nil || got.Summary.Bytes > 100 || got.Summary.Lines >= 20 || !got.Summary.Truncated {
			t.Fatalf("byte bound: %+v %v", got.Summary, err)
		}
		if last := got.Lines[len(got.Lines)-1]; !strings.HasPrefix(last, "tail-line-") && last != strings.Repeat("L", 16) {
			t.Fatalf("the byte bound must keep the newest lines: %q", last)
		}
		// The source scan is bounded too: a tail of a log larger than the scan bound
		// is reported as cut by the scan limit, never presented as the real tail.
		scan := lim
		scan.MaxScanBytes = 1600
		scanned := s.newLogs(logsOptions{Limits: &scan})
		got, err = readService(scanned.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Tail: ptr(2)})
		if err != nil || got.Summary.Stop != logs.StopScanLimit || !got.Summary.Truncated {
			t.Fatalf("scan bound: %+v %v", got.Summary, err)
		}
		over := logs.Limits{DefaultTail: 5, MaxTail: 5, MaxLines: 5, MaxBytes: 1 << 20, MaxDuration: 20 * time.Second, MaxStreams: 2, WriteTimeout: 5 * time.Second}
		lines := s.newLogs(logsOptions{Limits: &over})
		got, err = readService(lines.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Tail: ptr(5)})
		if err != nil || len(got.Lines) != 5 || got.Summary.Lines != 5 {
			t.Fatalf("line-count bound: %+v %v", got, err)
		}
		// The defaults the service reports are the documented ones.
		if lim := k.Svc.Limits(); lim.DefaultTail != logs.DefaultTailLines || lim.MaxTail != 400 {
			t.Fatalf("limits: %+v", lim)
		}
	})

	t.Run("live_tail_of_a_running_attempt", func(t *testing.T) {
		// The command pauses after its numbered lines and then prints a distinct
		// line while the Pod keeps running: a follow subscribed in the meantime
		// can only show it if the stream really follows the container log.
		r := s.start(scopeA, stepSpec{Script: numbered("live", 300) + "sleep " + followDelay + "\necho " + followMarker + "\nsleep 180\n" + finishOK + "\n"})
		s.waitRunning(r, 5*time.Minute)
		s.waitRawLog(r, "live-line-0300", 2*time.Minute)
		lq := "attemptId=" + string(r.ID)
		got := k.get(engineer, r.Scope, r.Run, lq+"&source=live&tail=7")
		if got.status != http.StatusOK {
			t.Fatalf("live tail: %d %s", got.status, truncate(got.raw, 300))
		}
		lines := got.lines()
		if len(lines) != 7 || lines[0] != "live-line-0294" || lines[6] != "live-line-0300" {
			t.Fatalf("live tail: %v", lines)
		}
		if m := got.meta(t); m["origin"] != "live" || m["completeness"] != "unknown" || m["terminal"] != false {
			t.Fatalf("a live read is origin live and completeness unknown: %+v", m)
		}
		// The server, not the caller, built the Kubernetes query: the tail is bounded.
		qs := k.Live.Queries()
		if last := qs[len(qs)-1]; last.TailLines != 7 || last.LimitBytes <= 0 || last.Follow {
			t.Fatalf("live query: %+v", last)
		}
		// A follow streams further records until the client leaves, and a live
		// attempt that is still running has no retained log to fall back to.
		// The delayed line has not been printed yet, so it cannot be part of the
		// tail read at subscription; it must arrive later through the follow.
		if raw := s.rawLog(r); strings.Contains(raw, followMarker) {
			t.Fatalf("setup: the delayed line was already printed before subscribing (%s s delay too short)", followDelay)
		}
		before := len(k.Live.Queries())
		followLive(t, k, r, lq+"&follow=true&tail=3", []string{"live-line-0298", "live-line-0299", "live-line-0300", followMarker})
		// The server asked Kubernetes for a following log, not just an initial read.
		var follows int
		for _, fq := range k.Live.Queries()[before:] {
			if fq.Follow {
				follows++
				if fq.TailLines != 3 || fq.LimitBytes <= 0 {
					t.Fatalf("follow query is not bounded: %+v", fq)
				}
			}
		}
		if follows != 1 {
			t.Fatalf("the follow request made %d following Kubernetes log queries, want 1: %+v", follows, k.Live.Queries()[before:])
		}
		if res := k.get(engineer, r.Scope, r.Run, lq+"&source=wrapper"); res.status != http.StatusNotFound {
			t.Fatalf("a running attempt has no wrapper log: %d", res.status)
		}
		// Tidy: cancel the running attempt through execution.
		if _, err := s.plain.Cancel(bg(), execution.Caller{PrincipalID: engineer, RequestID: "req_t064"}, r.Scope, r.Run); err != nil {
			t.Fatal(err)
		}
		s.waitTerminal(r, 5*time.Minute)
	})

	t.Run("restart_reads_the_same_tail_after_the_workload_is_gone", func(t *testing.T) {
		before := k.get(engineer, a.Scope, a.Run, q+"&tail=5")
		wantTail := before.lines()
		s.removeWorkload(a)
		restarted := s.newLogs(logsOptions{}) // every in-memory piece rebuilt over the same database and object store
		for _, kit := range []*logsKit{k, restarted} {
			res := kit.get(engineer, a.Scope, a.Run, q+"&tail=5")
			if res.status != http.StatusOK {
				t.Fatalf("retained read after workload removal: %d %s", res.status, truncate(res.raw, 300))
			}
			if got := res.lines(); strings.Join(got, "|") != strings.Join(wantTail, "|") || len(got) != 5 {
				t.Fatalf("tail after restart: %v want %v", got, wantTail)
			}
			if m := res.meta(t); m["origin"] != "wrapper" || m["completeness"] != "complete" {
				t.Fatalf("meta after restart: %+v", m)
			}
		}
		// The live source is explicitly unavailable, not an empty success.
		res := restarted.get(engineer, a.Scope, a.Run, q+"&source=live")
		if res.status == http.StatusOK {
			t.Fatalf("a removed workload served a live read: %s", truncate(res.raw, 200))
		}
		// The record itself is unchanged by the restart.
		if again, err := restarted.Ctrl.Observe(bg(), a.Scope, a.ID); err != nil || again.ID != rec.ID || again.Version != rec.Version {
			t.Fatalf("the restarted controller sees a different linkage: %+v %v", again, err)
		}
	})
}

// followDelay is how long the running attempt waits before printing
// followMarker. It must exceed the time to subscribe and stay well inside the
// service's follow lifetime (30 s).
const (
	followDelay  = "15"
	followMarker = "live-delayed-after-subscribe"
)

// followLive reads a follow stream until the wanted lines arrived, then leaves.
func followLive(t *testing.T, k *logsKit, a *att, query string, want []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	u := k.HTTP.URL + "/api/v1/tenants/" + string(a.Scope.TenantID) + "/projects/" + string(a.Scope.ProjectID) + "/runs/" + string(a.Run) + "/logs?" + query
	if _, err := url.Parse(u); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+engineer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("follow: %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	var got []string
	started := time.Now()
	if m := firstMeta(t, sc); m["follow"] != true || m["origin"] != "live" || m["terminal"] != false {
		t.Fatalf("follow meta: %+v", m)
	}
	for sc.Scan() && len(got) < len(want) {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("not JSON: %q", sc.Text())
		}
		if rec["type"] == "chunk" {
			got = append(got, strings.TrimSuffix(fmt.Sprint(rec["text"]), "\n"))
		}
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("follow lines %v, want %v (stream error %v after %s)", got, want, sc.Err(), time.Since(started))
	}
}

// firstMeta reads the leading meta record of an NDJSON stream.
func firstMeta(t *testing.T, sc *bufio.Scanner) map[string]any {
	t.Helper()
	if !sc.Scan() {
		t.Fatalf("follow stream ended before its meta record: %v", sc.Err())
	}
	var rec map[string]any
	if err := json.Unmarshal(sc.Bytes(), &rec); err != nil || rec["type"] != "meta" {
		t.Fatalf("first record is not meta: %q", sc.Text())
	}
	return rec
}
