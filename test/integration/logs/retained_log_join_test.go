package logs_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
)

var scopeA, scopeB = executiontest.ScopeA, executiontest.ScopeB

const (
	engineer = executiontest.Engineer
	other    = executiontest.OtherEng
)

func bg() context.Context { return context.Background() }

// wantRetained fails unless rec is a retained capture of the given source.
func wantRetained(t *testing.T, rec *logs.Record, err error, src logs.Source) *logs.Record {
	t.Helper()
	if err != nil {
		t.Fatalf("%s capture: %v", src, err)
	}
	if rec == nil || rec.State != logs.StateRetained || rec.Source != src || rec.Link == nil || rec.Link.Meta == nil {
		t.Fatalf("%s capture is not retained: %+v", src, rec)
	}
	return rec
}

// wantBoundSidecar checks that the sidecar is a valid statement bound to the
// retained log artifact and agrees with the recorded linkage.
func wantBoundSidecar(t *testing.T, k *logsKit, a *att, rec *logs.Record, src contract.Source) contract.Metadata {
	t.Helper()
	doc := k.sidecar(a, *rec.Link.Meta)
	if doc.Source != src {
		t.Fatalf("sidecar source %s, want %s", doc.Source, src)
	}
	if err := doc.Bind(rec.Link.Log.ID, rec.Link.Log.Digest, rec.Link.Log.Size); err != nil {
		t.Fatalf("the sidecar is not bound to its log: %v", err)
	}
	if doc.Completeness != rec.Link.Completeness {
		t.Fatalf("sidecar says %s, the record %s", doc.Completeness, rec.Link.Completeness)
	}
	return doc
}

func hasReason(rs []logs.Reason, want logs.Reason) bool {
	for _, r := range rs {
		if r == want {
			return true
		}
	}
	return false
}

// Test_retained_log_join (PX-10, AC-06, IT-03, IT-11, VS-11): ordinary command
// output reaches the real Kubernetes log through the entrypoint's redacting
// mirror, the wrapper's real upload (log plus bound sidecar) is observed after
// finalization and read back through the service, a failed upload falls back to
// the controller's server-side collection with explicit (never complete)
// evidence, duplicate and lost-response linkage is stable, execution results
// are never changed by log collection, and a restarted composition reads the
// same retained evidence after the workload is gone.
//
// Real kind Jobs and Pods, PostgreSQL non-owner roles, S3, and the real
// runtime/results/artifact/log handlers.
func Test_retained_log_join(t *testing.T) {
	s := newStack(t)
	k := s.kit

	t.Run("wrapper_retention_and_redaction", func(t *testing.T) {
		a := s.start(scopeA, stepSpec{Script: chatter("alpha")})
		at := s.waitTerminal(a, 5*time.Minute)
		if at.Status != execution.AttemptSucceeded {
			t.Fatalf("attempt %s (%s/%s)", at.Status, at.Class, at.Reason)
		}
		before := s.snapshot(a)
		delivered := s.deliveredOf(a)
		if len(delivered) < 2 {
			t.Fatalf("the runtime delivered %d credentials/tokens to the attempt, want a credential and a token", len(delivered))
		}

		// Ordinary command output reached the real Kubernetes log; the delivered
		// credentials the command printed did not.
		raw := s.rawLog(a)
		for _, want := range []string{"alpha stdout line one", "alpha stderr line two", "alpha done"} {
			if !strings.Contains(raw, want) {
				t.Fatalf("the Kubernetes container log lacks %q:\n%s", want, truncate(raw, 2000))
			}
		}
		if !strings.Contains(raw, "identity-file /kodama/identity/token="+logs.Marker) {
			t.Fatalf("positive control: the command printed the token file and the mirror must have redacted it:\n%s", truncate(raw, 2000))
		}
		noLeak(t, "the Kubernetes container log", raw, delivered...)

		// Finalization barrier: the attempt is terminal, so the wrapper's upload and
		// the result both happened. Only then is the upload observed.
		rec, err := k.Ctrl.Observe(bg(), a.Scope, a.ID)
		rec = wantRetained(t, rec, err, logs.SourceWrapper)
		if rec.Link.Completeness != logs.Complete || len(rec.Link.Reasons) != 0 {
			t.Fatalf("a wrapper log of a command that ran to its end is complete, got %s %v", rec.Link.Completeness, rec.Link.Reasons)
		}
		wantBoundSidecar(t, k, a, rec, contract.SourceWrapper)
		retained := k.readArtifact(a, rec.Link.Log)
		for _, want := range []string{"alpha stdout line one", "alpha stderr line two", "alpha done"} {
			if !strings.Contains(retained, want) {
				t.Fatalf("the retained log lacks %q", want)
			}
		}
		noLeak(t, "the retained command.log", retained, delivered...)
		// The retained command output is carried by the mirror line for line.
		for _, line := range strings.Split(strings.TrimSpace(retained), "\n") {
			if !strings.Contains(raw, line) {
				t.Fatalf("a retained line is missing from the Kubernetes log: %q", line)
			}
		}
		if got := at.Result; got == nil || !containsID(got.DiagnosticArtifactIDs, rec.Link.Log.ID) {
			t.Fatalf("the accepted result does not cite the retained log: %+v", got)
		}
		if names := s.artifactNames(a, "controller-%"); len(names) != 0 {
			t.Fatalf("observing a wrapper upload created controller artifacts: %v", names)
		}

		// The same bytes through the service: retained, complete, wrapper-origin.
		for _, mode := range []logs.Mode{logs.ModeWrapper, logs.ModeAuto} {
			got, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Mode: mode})
			if err != nil {
				t.Fatalf("%s read: %v", mode, err)
			}
			if got.Meta.Origin != logs.OriginWrapper || got.Meta.Completeness != logs.Complete || got.Summary == nil || got.Summary.Stop != logs.StopEnd {
				t.Fatalf("%s read: %+v %+v", mode, got.Meta, got.Summary)
			}
			noLeak(t, string(mode)+" read", got.joined(), delivered...)
			if !strings.Contains(got.joined(), "alpha done") {
				t.Fatalf("%s read lacks the output", mode)
			}
		}
		// The live Kubernetes log of the finished attempt is explicitly unknown.
		live, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Mode: logs.ModeLive})
		if err != nil {
			t.Fatalf("live read of the retained Job: %v", err)
		}
		if live.Meta.Origin != logs.OriginLive || live.Meta.Completeness != logs.Unknown {
			t.Fatalf("a live read can never claim completeness: %+v", live.Meta)
		}
		noLeak(t, "the live read", live.joined(), delivered...)

		// Replay is stable: the identical observation returns the identical record.
		again, err := k.Ctrl.Observe(bg(), a.Scope, a.ID)
		if err != nil || again.ID != rec.ID || again.Version != rec.Version || again.Link.Log != rec.Link.Log {
			t.Fatalf("re-observing changed the linkage: %+v %v", again, err)
		}
		if after := s.snapshot(a); after != before {
			t.Fatalf("log collection changed the attempt: %+v -> %+v", before, after)
		}
	})

	t.Run("delivered_secrets_never_leak", func(t *testing.T) {
		// Real secrets, sealed and pinned for the run, resolved through the real
		// resolver and delivered by the real entrypoint as a file and as an
		// environment value. The command prints both.
		fileValue, envValue := "t064-file-secret-"+randHex(8), "t064-env-secret-"+randHex(8)
		script := `printf 'file-secret=%s\n' "$(cat "$KODAMA_SECRETS_DIR/credfile")"
printf 'env-secret=%s\n' "$CRED_ENV"
` + chatter("secrets")
		a := s.start(scopeA, stepSpec{Script: script, Secrets: []secretUse{{File: "credfile", Value: fileValue}, {Env: "CRED_ENV", Value: envValue}}})
		at := s.waitTerminal(a, 5*time.Minute)
		if at.Status != execution.AttemptSucceeded {
			t.Fatalf("attempt %s (%s/%s); container log:\n%s\nevents %+v\nartifacts %v\nnotes %q", at.Status, at.Class, at.Reason, s.rawLog(a), s.srv.eventsOf(a.ID, ""), s.artifactNames(a, "%"), s.notes(a))
		}
		delivered := s.deliveredOf(a)
		if n := len(s.srv.eventsOf(a.ID, "secret")); n != 2 {
			t.Fatalf("the entrypoint must resolve both secrets through the runtime route, got %d requests", n)
		}
		for _, e := range s.srv.eventsOf(a.ID, "secret") {
			if e.Status != http.StatusOK {
				t.Fatalf("secret resolution: %+v", e)
			}
		}
		raw := s.rawLog(a)
		for _, want := range []string{"file-secret=" + logs.Marker, "env-secret=" + logs.Marker} {
			if !strings.Contains(raw, want) {
				t.Fatalf("positive control: the command printed the secret and the mirror must show %q:\n%s", want, truncate(raw, 1500))
			}
		}
		noLeak(t, "the Kubernetes container log", raw, delivered...)

		// Plaintext never reaches the Job or Pod manifests either.
		ns := s.ns(a.Scope.TenantID)
		for _, path := range []string{"/apis/batch/v1/namespaces/" + ns + "/jobs/" + a.Job, "/api/v1/namespaces/" + ns + "/pods?labelSelector=" + kube.LabelJob + "%3D" + a.Job} {
			ctx, cancel := context.WithTimeout(bg(), 30*time.Second)
			code, body, err := s.c.api.do(ctx, http.MethodGet, path, nil)
			cancel()
			if err != nil || code != http.StatusOK {
				t.Fatalf("GET %s: %d %v", path, code, err)
			}
			noLeak(t, "the workload manifest", string(body), fileValue, envValue)
		}

		rec, err := k.Ctrl.Observe(bg(), a.Scope, a.ID)
		rec = wantRetained(t, rec, err, logs.SourceWrapper)
		retained := k.readArtifact(a, rec.Link.Log)
		if !strings.Contains(retained, "file-secret="+logs.Marker) || !strings.Contains(retained, "env-secret="+logs.Marker) {
			t.Fatalf("the retained log must carry the redaction markers: %q", truncate(retained, 600))
		}
		noLeak(t, "the retained command.log", retained, delivered...)
		crec, err := k.Ctrl.Collect(bg(), a.Scope, a.ID)
		crec = wantRetained(t, crec, err, logs.SourceController)
		noLeak(t, "the controller capture", k.readArtifact(a, crec.Link.Log), delivered...)
		for _, mode := range []logs.Mode{logs.ModeLive, logs.ModeWrapper, logs.ModeController, logs.ModeAuto} {
			got, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Mode: mode, Tail: ptr(400)})
			if err != nil {
				t.Fatalf("%s read: %v", mode, err)
			}
			noLeak(t, string(mode)+" read", got.joined(), delivered...)
		}
	})

	t.Run("nonzero_exit_and_failed_result", func(t *testing.T) {
		a := s.start(scopeA, stepSpec{Script: "echo before-failure\necho the-error >&2\nexit 3\n"})
		at := s.waitTerminal(a, 5*time.Minute)
		if at.Status != execution.AttemptFailed {
			t.Fatalf("attempt %s (%s/%s), want failed", at.Status, at.Class, at.Reason)
		}
		rec, err := k.Ctrl.Observe(bg(), a.Scope, a.ID)
		rec = wantRetained(t, rec, err, logs.SourceWrapper)
		if rec.Link.Completeness != logs.Complete {
			t.Fatalf("a failed command's log is still complete evidence of what it printed: %s %v", rec.Link.Completeness, rec.Link.Reasons)
		}
		retained := k.readArtifact(a, rec.Link.Log)
		if !strings.Contains(retained, "before-failure") || !strings.Contains(retained, "the-error") {
			t.Fatalf("diagnostics are not readable after a failed result: %q", retained)
		}
		if at.Result == nil || !containsID(at.Result.DiagnosticArtifactIDs, rec.Link.Log.ID) {
			t.Fatalf("the failed result must keep the diagnostic reference: %+v", at.Result)
		}
		got, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID})
		if err != nil || got.Meta.Origin != logs.OriginWrapper || got.Meta.Completeness != logs.Complete || !strings.Contains(got.joined(), "before-failure") {
			t.Fatalf("service read of a failed attempt: %+v %v", got, err)
		}
	})

	t.Run("missing_output_keeps_diagnostics", func(t *testing.T) {
		// The command exits zero but its declared output was never produced: the
		// result is a validation failure, and the retained log is the evidence.
		a := s.start(scopeA, stepSpec{Script: "echo produced-nothing-declared\n", Outputs: []string{"v"}})
		at := s.waitTerminal(a, 5*time.Minute)
		if at.Status != execution.AttemptFailed {
			t.Fatalf("a missing declared output must not succeed: %s (%s/%s)", at.Status, at.Class, at.Reason)
		}
		rec, err := k.Ctrl.Observe(bg(), a.Scope, a.ID)
		rec = wantRetained(t, rec, err, logs.SourceWrapper)
		if !strings.Contains(k.readArtifact(a, rec.Link.Log), "produced-nothing-declared") {
			t.Fatal("the retained log of the failed validation is missing its output")
		}
	})

	t.Run("wrapper_without_sidecar_is_unknown", func(t *testing.T) {
		// The log uploads, its sidecar does not: a wrapper log without a verified
		// sidecar has unknown completeness, never complete.
		a := s.start(scopeA, stepSpec{Script: chatter("nosidecar")}, faultSpec{Op: "upload", Name: "command.log.meta.json", Status: http.StatusForbidden})
		at := s.waitTerminal(a, 5*time.Minute)
		if at.Status != execution.AttemptSucceeded {
			t.Fatalf("a failed sidecar upload must not fail the step: %s (%s/%s)", at.Status, at.Class, at.Reason)
		}
		rec, err := k.Ctrl.Observe(bg(), a.Scope, a.ID)
		if err != nil || rec == nil || rec.State != logs.StateRetained {
			t.Fatalf("observe: %+v %v", rec, err)
		}
		if rec.Link.Meta != nil || rec.Link.Completeness != logs.Unknown || !hasReason(rec.Link.Reasons, contract.ReasonUnverified) {
			t.Fatalf("a log without a sidecar must be unknown/unverified: %+v", rec.Link)
		}
		got, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID})
		if err != nil || got.Meta.Origin != logs.OriginWrapper || got.Meta.Completeness != logs.Unknown {
			t.Fatalf("read: %+v %v", got, err)
		}
		// The late arrival of a verified sidecar would be a conflict: evidence is
		// pinned at first observation and never upgraded.
	})

	t.Run("observation_before_the_sidecar_is_pinned_unknown", func(t *testing.T) {
		// The finalization barrier matters: the sidecar's reservation is refused a
		// few times, so for a moment the log is usable and its sidecar is not.
		// Observing in that window pins the wrapper link as unknown/unverified, and
		// the later, verified sidecar cannot upgrade it (a changed linkage is a
		// conflict, never a replacement).
		a := s.start(scopeA, stepSpec{Script: chatter("early")}, faultSpec{Op: "reserve", Name: "command.log.meta.json", Status: http.StatusServiceUnavailable, Times: 4})
		var early *logs.Record
		eventually(t, 3*time.Minute, "the log to be usable before its sidecar", func() (bool, string) {
			done := false
			for _, e := range s.srv.eventsOf(a.ID, "complete") {
				done = done || (e.Name == "command.log" && e.Status == 200)
			}
			if !done {
				s.tick()
				return false, "command.log not complete"
			}
			rec, err := k.Ctrl.Observe(bg(), a.Scope, a.ID)
			if err != nil {
				return false, err.Error()
			}
			early = rec
			return true, ""
		})
		if early.Link.Completeness != logs.Unknown || early.Link.Meta != nil || !hasReason(early.Link.Reasons, contract.ReasonUnverified) {
			t.Fatalf("a log observed before its sidecar is unknown/unverified: %+v", early.Link)
		}
		at := s.waitTerminal(a, 5*time.Minute)
		if at.Status != execution.AttemptSucceeded {
			t.Fatalf("attempt %s (%s/%s)", at.Status, at.Class, at.Reason)
		}
		if names := s.artifactNames(a, "command.log%"); len(names) != 2 {
			t.Fatalf("the sidecar must have been uploaded after the retries: %v", names)
		}
		if _, err := k.Ctrl.Observe(bg(), a.Scope, a.ID); !errors.Is(err, logs.ErrConflict) {
			t.Fatalf("a later verified sidecar must not replace pinned evidence: %v", err)
		}
		got, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID})
		if err != nil || got.Meta.Completeness != logs.Unknown {
			t.Fatalf("the served evidence stays as pinned: %+v %v", got, err)
		}
		if again := k.record(a, logs.SourceWrapper); again.ID != early.ID || again.Version != early.Version {
			t.Fatalf("the pinned record changed: %+v", again)
		}
	})

	t.Run("truncation_and_mirror_markers", func(t *testing.T) {
		a := s.start(scopeA, stepSpec{Script: bulk("trunc") + finishOK + "\n"})
		at := s.waitTerminal(a, 8*time.Minute)
		if at.Status != execution.AttemptSucceeded {
			t.Fatalf("attempt %s (%s/%s)", at.Status, at.Class, at.Reason)
		}
		raw := s.rawLog(a)
		// The mirror is bounded transport: it ends with the truncation marker (a
		// loss indicator) instead of carrying more than the retained capacity.
		if !strings.Contains(raw, entrypoint.MirrorTruncationMarker) {
			t.Fatalf("the oversized output must end the mirror with %q; log has %d bytes, tail %q", entrypoint.MirrorTruncationMarker, len(raw), raw[max(0, len(raw)-300):])
		}
		if len(raw) > 1100<<10 {
			t.Fatalf("the mirror is not bounded by the retained capacity: %d bytes", len(raw))
		}
		if strings.Contains(raw, "trunc-end") {
			t.Fatal("output beyond the bound reached the mirror")
		}

		// The wrapper declares the truncation itself.
		rec, err := k.Ctrl.Observe(bg(), a.Scope, a.ID)
		rec = wantRetained(t, rec, err, logs.SourceWrapper)
		doc := wantBoundSidecar(t, k, a, rec, contract.SourceWrapper)
		if rec.Link.Completeness != logs.Partial || !hasReason(rec.Link.Reasons, contract.ReasonTruncated) || doc.Completeness != contract.Partial {
			t.Fatalf("a truncated wrapper capture is partial/truncated: %+v", rec.Link)
		}

		// The controller reads the mirror. The mirror's loss markers are indicators,
		// not a completeness statement: the retained controller capture preserves
		// them for the reader and is never complete. Marker-derived `partial`
		// classification is blocker B-2 (the controller records clean EOF as
		// unknown/controller_capture); this test does not require it.
		crec, err := k.Ctrl.Collect(bg(), a.Scope, a.ID)
		crec = wantRetained(t, crec, err, logs.SourceController)
		if crec.Link.Completeness == logs.Complete {
			t.Fatalf("a controller capture must never be complete: %+v", crec.Link)
		}
		if !hasReason(crec.Link.Reasons, contract.ReasonControllerCapture) {
			t.Fatalf("controller evidence must say so: %+v", crec.Link)
		}
		ctrl := k.readArtifact(a, crec.Link.Log)
		if !strings.Contains(ctrl, entrypoint.MirrorTruncationMarker) {
			t.Fatal("the loss marker must survive into the retained controller capture so a reader can see the loss")
		}
		t.Logf("B-2: controller capture of a marker-bearing mirror is %s %v (marker-derived partial is not implemented by the controller)", crec.Link.Completeness, crec.Link.Reasons)
		// Sources are never merged: automatic selection prefers the wrapper capture
		// even though it is partial, and says so.
		got, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Tail: ptr(5)})
		if err != nil || got.Meta.Origin != logs.OriginWrapper || got.Meta.Completeness != logs.Partial {
			t.Fatalf("auto read: %+v %v", got.Meta, err)
		}
	})

	t.Run("failed_upload_controller_fallback", func(t *testing.T) {
		// The wrapper's log upload is refused for good (a non-retryable response):
		// the step still succeeds, nothing is observable as a wrapper log, and the
		// controller collects the real Kubernetes log.
		a := s.start(scopeA, stepSpec{Script: chatter("fallback")}, faultSpec{Op: "upload", Name: "command.log", Status: http.StatusForbidden})
		at := s.waitTerminal(a, 5*time.Minute)
		if at.Status != execution.AttemptSucceeded {
			t.Fatalf("a failed log upload must not fail the step: %s (%s/%s)", at.Status, at.Class, at.Reason)
		}
		if ev := s.srv.eventsOf(a.ID, "upload"); len(ev) == 0 || ev[0].Faulted != "status" {
			t.Fatalf("the wrapper never attempted its upload: %+v", ev)
		}
		before := s.snapshot(a)
		delivered := s.deliveredOf(a)

		if _, err := k.Ctrl.Observe(bg(), a.Scope, a.ID); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("a refused upload leaves no wrapper log to observe: %v", err)
		}
		if _, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Mode: logs.ModeWrapper}); !errors.Is(err, logs.ErrNoLog) {
			t.Fatalf("a wrapper read without a wrapper log must say so: %v", err)
		}
		opens := k.Live.Opens()
		rec, err := k.Ctrl.Collect(bg(), a.Scope, a.ID)
		rec = wantRetained(t, rec, err, logs.SourceController)
		if k.Live.Opens() != opens+1 {
			t.Fatalf("the Kubernetes log must be read exactly once: %d -> %d", opens, k.Live.Opens())
		}
		if rec.Generation != 1 || rec.Link.Completeness != logs.Unknown || !hasReason(rec.Link.Reasons, contract.ReasonControllerCapture) {
			t.Fatalf("a controller capture is unknown/controller_capture, never complete: %+v", rec.Link)
		}
		doc := wantBoundSidecar(t, k, a, rec, contract.SourceController)
		if doc.Completeness == contract.Complete {
			t.Fatal("the controller's sidecar claims completeness")
		}
		got := k.readArtifact(a, rec.Link.Log)
		raw := s.rawLog(a)
		if got != raw {
			t.Fatalf("the controller capture is not the Kubernetes log:\n%q\nvs\n%q", truncate(got, 500), truncate(raw, 500))
		}
		for _, want := range []string{"fallback stdout line one", "fallback stderr line two", "fallback done"} {
			if !strings.Contains(got, want) {
				t.Fatalf("the controller capture lacks %q", want)
			}
		}
		noLeak(t, "the controller capture", got, delivered...)
		if names := s.artifactNames(a, "controller-%"); len(names) != 2 || names[0] != "controller-g1.log" || names[1] != "controller-g1.log.meta.json" {
			t.Fatalf("controller artifacts: %v", names)
		}

		// The service now serves the controller's capture with its explicit evidence.
		for _, mode := range []logs.Mode{logs.ModeController, logs.ModeAuto} {
			r, err := readService(k.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID, Mode: mode, Tail: ptr(400)})
			if err != nil {
				t.Fatalf("%s read: %v", mode, err)
			}
			if r.Meta.Origin != logs.OriginController || r.Meta.Completeness != logs.Unknown || !hasReason(r.Meta.Reasons, contract.ReasonControllerCapture) {
				t.Fatalf("%s read: %+v", mode, r.Meta)
			}
			noLeak(t, string(mode)+" read", r.joined(), delivered...)
		}
		if after := s.snapshot(a); after != before {
			t.Fatalf("the fallback changed the execution result: %+v -> %+v", before, after)
		}
	})

	t.Run("duplicate_and_lost_response_linkage", func(t *testing.T) {
		t.Run("wrapper reserve and complete responses lost", func(t *testing.T) {
			// The server processes the wrapper's reserve and complete calls and the
			// responses are lost: the entrypoint repeats them and the linkage ends as
			// one verified log with one bound sidecar.
			a := s.start(scopeA, stepSpec{Script: chatter("lostwrap")},
				faultSpec{Op: "reserve", Name: "command.log", Lose: true, Times: 1},
				faultSpec{Op: "complete", Name: "command.log", Lose: true, Times: 1},
				faultSpec{Op: "complete", Name: "command.log.meta.json", Lose: true, Times: 1})
			at := s.waitTerminal(a, 5*time.Minute)
			if at.Status != execution.AttemptSucceeded {
				t.Fatalf("attempt %s (%s/%s)", at.Status, at.Class, at.Reason)
			}
			lost := 0
			for _, e := range s.srv.eventsOf(a.ID, "") {
				if e.Faulted == "lose" {
					lost++
				}
			}
			if lost != 3 {
				t.Fatalf("expected three lost responses, got %d: %+v", lost, s.srv.eventsOf(a.ID, ""))
			}
			rec, err := k.Ctrl.Observe(bg(), a.Scope, a.ID)
			rec = wantRetained(t, rec, err, logs.SourceWrapper)
			if rec.Link.Completeness != logs.Complete {
				t.Fatalf("replayed finalization must still end complete: %+v", rec.Link)
			}
			wantBoundSidecar(t, k, a, rec, contract.SourceWrapper)
			if names := s.artifactNames(a, "%"); len(names) != 2 {
				t.Fatalf("exactly one log and one sidecar are expected, got %v", names)
			}
		})

		t.Run("controller upload and commit responses lost", func(t *testing.T) {
			a := s.start(scopeA, stepSpec{Script: chatter("lostctrl")}, faultSpec{Op: "upload", Name: "command.log", Status: http.StatusForbidden})
			s.waitTerminal(a, 5*time.Minute)
			before := s.snapshot(a)
			var putOnce, commitOnce sync.Once
			k.Objects.Set(func(f *artifactsFaultObjects) {
				f.OnPut = func(spec artifacts.PutSpec, next func() error) error {
					if !strings.Contains(spec.Key, "/staging/") {
						return next()
					}
					var err error
					putOnce.Do(func() {
						if perr := next(); perr != nil { // the object lands, the response is lost
							t.Errorf("put: %v", perr)
						}
						err = errors.New("connection reset by peer")
					})
					if err != nil {
						return err
					}
					return next()
				}
			})
			k.Repo.Set(func(f *artifactsFaultRepo) {
				f.OnCommit = func(cmd artifacts.CommitCommand, next func() (artifacts.Artifact, error)) (artifacts.Artifact, error) {
					lose := false
					commitOnce.Do(func() { lose = true })
					art, err := next()
					if lose && err == nil {
						return artifacts.Artifact{}, errors.New("response lost after commit")
					}
					return art, err
				}
			})
			t.Cleanup(func() {
				k.Objects.Set(func(f *artifactsFaultObjects) { f.OnPut = nil })
				k.Repo.Set(func(f *artifactsFaultRepo) { f.OnCommit = nil })
			})
			opens := k.Live.Opens()
			var rec *logs.Record
			var err error
			for i := 0; i < 4; i++ { // a lost response is unknown: re-read, never assumed
				rec, err = k.Ctrl.Collect(bg(), a.Scope, a.ID)
				if err == nil && rec != nil && rec.State == logs.StateRetained {
					break
				}
			}
			rec = wantRetained(t, rec, err, logs.SourceController)
			if got := k.Live.Opens() - opens; got != 1 {
				t.Fatalf("the Kubernetes log must be read once, not once per retry: %d", got)
			}
			if names := s.artifactNames(a, "controller-%"); len(names) != 2 {
				t.Fatalf("exactly one controller log and sidecar: %v", names)
			}
			wantBoundSidecar(t, k, a, rec, contract.SourceController)
			if after := s.snapshot(a); after != before {
				t.Fatalf("collection changed the execution result: %+v -> %+v", before, after)
			}
		})

		t.Run("concurrent observation and collection on one connection", func(t *testing.T) {
			// Every store shares a single PostgreSQL connection: any path that holds a
			// connection (a transaction) while asking a peer for another would
			// deadlock, so a network read or object transfer inside a transaction
			// cannot pass.
			single := s.newLogs(logsOptions{Pool: s.x.Base.DB.RuntimePool(bg(), 1)})
			a := s.start(scopeA, stepSpec{Script: chatter("concurrent")})
			s.waitTerminal(a, 5*time.Minute)
			before := s.snapshot(a)
			var wg sync.WaitGroup
			errs := make(chan error, 16)
			for i := 0; i < 6; i++ {
				wg.Add(2)
				go func() {
					defer wg.Done()
					if _, err := single.Ctrl.Observe(bg(), a.Scope, a.ID); err != nil {
						errs <- fmt.Errorf("observe: %w", err)
					}
				}()
				go func() {
					defer wg.Done()
					if _, err := single.Ctrl.Collect(bg(), a.Scope, a.ID); err != nil && !errors.Is(err, logs.ErrBusy) {
						errs <- fmt.Errorf("collect: %w", err)
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
			w, err := single.Ctrl.Observe(bg(), a.Scope, a.ID)
			w = wantRetained(t, w, err, logs.SourceWrapper)
			c, err := single.Ctrl.Collect(bg(), a.Scope, a.ID)
			c = wantRetained(t, c, err, logs.SourceController)
			if w.ID == c.ID || w.Link.Log.ID == c.Link.Log.ID {
				t.Fatal("wrapper and controller captures must stay separate records and artifacts")
			}
			if recs := single.records(a); len(recs) != 2 {
				t.Fatalf("one wrapper and one controller record, got %d", len(recs))
			}
			if names := s.artifactNames(a, "%"); len(names) != 4 {
				t.Fatalf("command.log, its sidecar, controller-g1.log and its sidecar: %v", names)
			}
			if single.Live.Opens() != 1 {
				t.Fatalf("concurrent collectors read the Kubernetes log %d times", single.Live.Opens())
			}
			// Automatic selection prefers the verified complete wrapper capture and
			// never merges the sources.
			got, err := readService(single.Svc, logs.Request{Principal: engineer, Scope: a.Scope, Run: a.Run, Attempt: a.ID})
			if err != nil || got.Meta.Origin != logs.OriginWrapper || got.Meta.Completeness != logs.Complete {
				t.Fatalf("auto read: %+v %v", got, err)
			}
			if after := s.snapshot(a); after != before {
				t.Fatalf("log collection changed the execution result: %+v -> %+v", before, after)
			}
		})
	})

	t.Run("bounded_tail", func(t *testing.T) { boundedTail(t, s) })
}

func containsID(ids []domain.ArtifactID, id domain.ArtifactID) bool {
	for _, i := range ids {
		if i == id {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }
