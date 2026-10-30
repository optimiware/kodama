package entrypoint

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

func apiErr(op string, status int, code domain.ErrorCode) error {
	return &APIError{Op: op, Status: status, Code: code, Message: string(code)}
}

func TestRunAdmissionOutcomesMapToExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"cancelled", apiErr("admit", 403, runtimev1.CodeAttemptRevoked), ExitRevoked},
		{"wrong job", apiErr("admit", 403, domain.CodeForbidden), ExitAdmission},
		{"profile unsupported", apiErr("admit", 422, runtimev1.CodeProfileUnsupported), ExitAdmission},
		{"consumed", apiErr("admit", 409, runtimev1.CodeAdmissionConsumed), ExitAdmission},
		{"unreachable", &TransportError{Op: "admit", Err: errors.New("connection refused")}, ExitAdmission},
		{"contract violation", &ProtocolError{Op: "admit", Err: errors.New("bad body")}, ExitAdmission},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newUnit(t, `touch ran`)
			u.rt.admit = func(runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error) {
				return runtimev1.AdmitResponse{}, tc.err
			}
			u.wantExit(u.run(), tc.want)
			if len(u.rt.calls) != 1 {
				t.Fatalf("calls = %v, nothing may follow a refused admission", u.rt.calls)
			}
			if _, err := os.Stat(u.cfg.TokenFile); err == nil {
				t.Fatal("a token file was written")
			}
			if _, err := os.Stat(u.cfg.WorkspaceDir); err == nil {
				t.Fatal("the workspace was prepared")
			}
		})
	}
}

func TestRunAdmitRequestCarriesCredentialFreshNonceVersionAndJob(t *testing.T) {
	u := newUnit(t, `exit 0`)
	u.cfg.ClientVersion = "9.9.9"
	var reqs []runtimev1.AdmitRequest
	u.rt.admit = func(r runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error) {
		reqs = append(reqs, r)
		return runtimev1.AdmitResponse{}, apiErr("admit", 403, runtimev1.CodeAttemptRevoked)
	}
	u.run()
	u.run()
	if len(reqs) != 2 {
		t.Fatalf("admit requests = %d", len(reqs))
	}
	if reqs[0].AdmissionCredential != unitAdmission || reqs[0].ClientVersion != "9.9.9" || reqs[0].Job != unitJob {
		t.Fatalf("request = %+v", reqs[0])
	}
	if reqs[0].Nonce == reqs[1].Nonce || reqs[0].Validate() != nil {
		t.Fatalf("nonces %q %q must be fresh and valid", reqs[0].Nonce, reqs[1].Nonce)
	}
}

func TestRunMissingOrEmptyAdmissionCredentialIsUsageError(t *testing.T) {
	u := newUnit(t, `exit 0`)
	if err := os.WriteFile(u.cfg.AdmissionCredentialFile, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u.wantExit(u.run(), ExitUsage)
	if err := os.Remove(u.cfg.AdmissionCredentialFile); err != nil {
		t.Fatal(err)
	}
	u.wantExit(u.run(), ExitUsage)
	if len(u.rt.calls) != 0 {
		t.Fatalf("the runtime was contacted: %v", u.rt.calls)
	}
}

func TestRunRefusesContextsItCannotTrust(t *testing.T) {
	t.Run("admit and GET /context differ", func(t *testing.T) {
		u := newUnit(t, `touch "`+filepath.Join(t.TempDir(), "ran")+`"`)
		other := u.rt.ctx
		other.StepID = "deploy"
		u.rt.fetch = func() (runtimev1.Context, error) { return other, nil }
		u.wantExit(u.run(), ExitOK)
		wantReason(t, u.lastResult(), ReasonContextMismatch)
		if u.rt.count("resolveSecret")+u.rt.count("proposeSource") != 0 {
			t.Fatalf("calls = %v", u.rt.calls)
		}
		if _, err := os.Stat(u.cfg.WorkspaceDir); err == nil {
			t.Fatal("the workspace was prepared for an untrusted context")
		}
	})
	t.Run("admitted context for another attempt", func(t *testing.T) {
		u := newUnit(t, `exit 0`)
		u.rt.ctx.AttemptID = "att_someone_else"
		u.wantExit(u.run(), ExitAdmission)
		if u.rt.count("getContext") != 0 {
			t.Fatalf("calls = %v", u.rt.calls)
		}
	})
	t.Run("invalid context from a non-validating runtime", func(t *testing.T) {
		u := newUnit(t, `exit 0`)
		u.rt.ctx.Workspace = runtimev1.Workspace{Access: runtimev1.AccessWrite} // no source
		u.wantExit(u.run(), ExitAdmission)
		if u.rt.count("submitResult") != 0 {
			t.Fatalf("calls = %v", u.rt.calls)
		}
	})
	t.Run("context fetch denied", func(t *testing.T) {
		u := newUnit(t, `exit 0`)
		u.rt.fetch = func() (runtimev1.Context, error) {
			return runtimev1.Context{}, apiErr("getContext", 403, runtimev1.CodeAttemptRevoked)
		}
		u.wantExit(u.run(), ExitRevoked)
		u.rt.fetch = func() (runtimev1.Context, error) {
			return runtimev1.Context{}, apiErr("getContext", 401, domain.CodeInvalidCredential)
		}
		u.wantExit(u.run(), ExitAdmission)
	})
}

func TestRunCommandOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*runtimev1.Context)
		script  string
		reason  string
		exit    *int64
		hasExit bool
	}{
		{name: "nonzero exit", script: `echo oops >&2; exit 7`, reason: ReasonExitNonzero, exit: ptr(int64(7)), hasExit: true},
		{name: "terminated by a signal", script: `kill -9 $$`, reason: ReasonCommandFailed},
		{name: "timeout", script: `sleep 30`, reason: ReasonTimeout, mutate: func(c *runtimev1.Context) { c.Limits.TimeoutSeconds = 1 }},
		{name: "cannot start", mutate: func(c *runtimev1.Context) {
			c.Command.Command = []string{"/nonexistent/binary"}
			c.Command.Args = []string{}
		}, reason: ReasonCommandStartFailed},
		{name: "no command", mutate: func(c *runtimev1.Context) { c.Command.Command = []string{}; c.Command.Args = []string{} }, reason: ReasonCommandStartFailed},
		{name: "manifest reports failure", script: `printf '{"outcome":"failed","reason":"lint_errors"}' > "$KODAMA_RESULTS_FILE"`, reason: "lint_errors", exit: ptr(int64(0)), hasExit: true},
		{name: "manifest failure without reason", script: `printf '{"outcome":"failed"}' > "$KODAMA_RESULTS_FILE"`, reason: ReasonCommandReportedFail, exit: ptr(int64(0)), hasExit: true},
		{name: "manifest failure with invalid reason", script: `printf '{"outcome":"failed","reason":"Not A Code"}' > "$KODAMA_RESULTS_FILE"`, reason: ReasonOutputInvalid, exit: ptr(int64(0)), hasExit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newUnit(t, tc.script)
			if tc.mutate != nil {
				tc.mutate(&u.rt.ctx)
			}
			u.wantExit(u.run(), ExitOK)
			r := u.lastResult()
			wantReason(t, r, tc.reason)
			if tc.hasExit != (r.ExitCode != nil) || (tc.exit != nil && *r.ExitCode != *tc.exit) {
				t.Fatalf("exit code = %v", r.ExitCode)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestRunExitZeroWithoutDeclaredOutputNeverCompletes(t *testing.T) {
	u := newUnit(t, `exit 0`)
	u.rt.ctx.Outputs = []runtimev1.OutputDecl{{Name: "count", Type: runtimev1.OutputInteger}}
	u.wantExit(u.run(), ExitOK)
	r := u.lastResult()
	wantReason(t, r, ReasonOutputInvalid)
	if r.ExitCode == nil || *r.ExitCode != 0 || len(r.Outputs) != 0 {
		t.Fatalf("result = %+v", r)
	}
	for _, q := range u.rt.reserved {
		// Diagnostics (command log, its completeness sidecar, notes) are
		// expected; no output artifact may be reserved.
		if q.Role != runtimev1.RoleDiagnostic {
			t.Fatalf("output artifacts reserved: %+v", u.rt.reserved)
		}
	}
}

func TestRunCompletedResultCarriesDeclaredOutputsAndDiagnostics(t *testing.T) {
	u := newUnit(t, `echo '{"v":1}' > "$KODAMA_OUTPUTS_DIR/r.json"
printf '{"outcome":"completed","outputs":{"n":4,"ok":true,"s":"x","img":{"digest":"sha256:`+strings.Repeat("a", 64)+`","registryRef":"registry.example.invalid/app"}}}' > "$KODAMA_RESULTS_FILE"
echo logged`)
	u.rt.ctx.Outputs = []runtimev1.OutputDecl{
		{Name: "r", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/r.json")},
		{Name: "n", Type: runtimev1.OutputInteger}, {Name: "ok", Type: runtimev1.OutputBoolean}, {Name: "s", Type: runtimev1.OutputString},
		{Name: "img", Type: runtimev1.OutputOCIArtifact},
	}
	u.wantExit(u.run(), ExitOK)
	r := u.lastResult()
	if r.Outcome != runtimev1.OutcomeCompleted || r.Reason != nil || r.Question != nil || r.SourceCommit != nil || len(r.Outputs) != 5 {
		t.Fatalf("result = %+v\nlogs:\n%s", r, u.logs.String())
	}
	if r.ExitCode == nil || *r.ExitCode != 0 || len(r.DiagnosticArtifactIDs) != 2 {
		t.Fatalf("result = %+v (want command.log and its completeness sidecar)", r)
	}
	if got := string(u.up.object(r.DiagnosticArtifactIDs[0])); !strings.Contains(got, "logged") {
		t.Fatalf("command log = %q", got)
	}
	if !r.Outputs["n"].IsInteger() || !r.Outputs["ok"].IsBool() {
		t.Fatalf("outputs = %+v", r.Outputs)
	}
	var roles []runtimev1.ArtifactRole
	for _, q := range u.rt.reserved {
		roles = append(roles, q.Role)
	}
	if len(roles) != 4 { // command.log, sidecar, one file output, one OCI output
		t.Fatalf("reservations = %v", roles)
	}
}

func TestRunOutputTransferFailureReasons(t *testing.T) {
	setup := func() *unit {
		u := newUnit(t, `echo x > "$KODAMA_OUTPUTS_DIR/r.txt"`)
		u.rt.ctx.Outputs = []runtimev1.OutputDecl{{Name: "r", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/r.txt")}}
		return u
	}
	t.Run("upload fails", func(t *testing.T) {
		u := setup()
		u.up.err = &TransportError{Op: "upload", Err: errors.New("reset")}
		u.wantExit(u.run(), ExitOK)
		wantReason(t, u.lastResult(), ReasonOutputUploadFailed)
	})
	t.Run("server calls the output invalid", func(t *testing.T) {
		u := setup()
		u.rt.complete = func(domain.ArtifactID, runtimev1.ArtifactCompleteRequest) (runtimev1.ArtifactCompleteResponse, error) {
			return runtimev1.ArtifactCompleteResponse{}, apiErr("completeArtifact", 422, runtimev1.CodeOutputInvalid)
		}
		u.wantExit(u.run(), ExitOK)
		wantReason(t, u.lastResult(), ReasonOutputInvalid)
	})
	t.Run("revoked while reserving", func(t *testing.T) {
		u := setup()
		u.rt.reserve = func(runtimev1.ArtifactReserveRequest) (runtimev1.ArtifactReserveResponse, error) {
			return runtimev1.ArtifactReserveResponse{}, apiErr("reserveArtifact", 403, runtimev1.CodeAttemptRevoked)
		}
		u.wantExit(u.run(), ExitRevoked)
		if u.rt.count("submitResult") != 0 {
			t.Fatalf("calls = %v", u.rt.calls)
		}
	})
	t.Run("completion does not match the upload", func(t *testing.T) {
		u := setup()
		u.rt.complete = func(id domain.ArtifactID, r runtimev1.ArtifactCompleteRequest) (runtimev1.ArtifactCompleteResponse, error) {
			return runtimev1.ArtifactCompleteResponse{ArtifactID: id, State: runtimev1.StateUsable, Digest: runtimev1.Digest("sha256:" + strings.Repeat("0", 64)), SizeBytes: r.SizeBytes}, nil
		}
		u.wantExit(u.run(), ExitOK)
		wantReason(t, u.lastResult(), ReasonOutputUploadFailed)
	})
}

func secretBinding(name, ref string, d runtimev1.SecretDelivery) runtimev1.SecretBinding {
	return runtimev1.SecretBinding{Ref: ref, Version: 1, Name: name, Delivery: d}
}

func TestRunSecretsAreDeliveredAndRemovedAndNeverUploaded(t *testing.T) {
	const fileSecret, envSecret = "file-secret-value-9988", "env-secret-value-7766"
	u := newUnit(t, `cat "$KODAMA_SECRETS_DIR/erp-token" > "$KODAMA_OUTPUTS_DIR/seen.txt"; echo "env=$DB_PASS"`)
	f, e := secretBinding("erp-token", "secret://project/erp-token", runtimev1.DeliveryFile), secretBinding("db-pass", "secret://project/db-pass", runtimev1.DeliveryEnv)
	u.rt.ctx.Secrets = []runtimev1.SecretBinding{f, e}
	u.rt.ctx.Outputs = []runtimev1.OutputDecl{{Name: "seen", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/seen.txt")}}
	values := map[string]string{f.Ref: fileSecret, e.Ref: envSecret}
	u.rt.resolve = func(r runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
		for _, b := range u.rt.ctx.Secrets {
			if b.Ref == r.Ref {
				return runtimev1.SecretResolveResponse{Ref: b.Ref, Version: b.Version, Name: b.Name, Delivery: b.Delivery,
					ValueBase64: base64.StdEncoding.EncodeToString([]byte(values[b.Ref]))}, nil
			}
		}
		return runtimev1.SecretResolveResponse{}, apiErr("resolveSecret", 403, domain.CodeForbidden)
	}
	u.wantExit(u.run(), ExitOK)
	if r := u.lastResult(); r.Outcome != runtimev1.OutcomeCompleted {
		t.Fatalf("result = %+v\n%s", r, u.logs.String())
	}
	if _, err := os.Stat(filepath.Join(u.cfg.SecretsDir, "erp-token")); err == nil {
		t.Fatal("the secret file outlived the step")
	}
	if u.rt.count("resolveSecret") != 2 {
		t.Fatalf("calls = %v", u.rt.calls)
	}
	// The secret is visible to the command (it echoed it into its own output
	// artifact, which is the command's choice); it must not appear in anything
	// the entrypoint itself writes: its logs, notes or request payloads.
	if strings.Contains(u.logs.String(), fileSecret) || strings.Contains(u.logs.String(), envSecret) {
		t.Fatal("a secret reached the entrypoint logs")
	}
}

func TestRunSecretFailures(t *testing.T) {
	setup := func() *unit {
		u := newUnit(t, `touch ran-marker`)
		u.rt.ctx.Secrets = []runtimev1.SecretBinding{secretBinding("erp-token", "secret://project/erp-token", runtimev1.DeliveryFile)}
		return u
	}
	t.Run("denied is a failed result", func(t *testing.T) {
		u := setup()
		u.wantExit(u.run(), ExitOK)
		wantReason(t, u.lastResult(), ReasonSecretUnavailable)
		if _, err := os.Stat(filepath.Join(u.cfg.WorkspaceDir, "ran-marker")); err == nil {
			t.Fatal("the command ran without its secret")
		}
	})
	t.Run("revoked aborts without a result", func(t *testing.T) {
		u := setup()
		u.rt.resolve = func(runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
			return runtimev1.SecretResolveResponse{}, apiErr("resolveSecret", 403, runtimev1.CodeAttemptRevoked)
		}
		u.wantExit(u.run(), ExitRevoked)
		if u.rt.count("submitResult") != 0 {
			t.Fatalf("calls = %v", u.rt.calls)
		}
		if _, err := os.Stat(filepath.Join(u.cfg.WorkspaceDir, "ran-marker")); err == nil {
			t.Fatal("the command ran for a revoked attempt")
		}
	})
}

func TestRunInterruptedBeforeLaunchReportsInterrupted(t *testing.T) {
	u := newUnit(t, `touch ran-marker`)
	ctx, cancel := context.WithCancel(context.Background())
	u.rt.fetch = func() (runtimev1.Context, error) { cancel(); return u.rt.ctx, nil }
	u.wantExit(u.build().Run(ctx), ExitOK)
	wantReason(t, u.lastResult(), ReasonInterrupted)
	if _, err := os.Stat(filepath.Join(u.cfg.WorkspaceDir, "ran-marker")); err == nil {
		t.Fatal("the command ran after the interrupt")
	}
}

func TestChildEnvLayering(t *testing.T) {
	u := newUnit(t, `exit 0`)
	u.rt.ctx.Inputs = map[string]runtimev1.Value{
		"my-name": runtimev1.StringValue("x y"), "count": runtimev1.IntValue(5), "flag": runtimev1.BoolValue(true),
	}
	u.rt.ctx.Command.Env = map[string]string{"PATH": "/override", "CUSTOM": "c", "KODAMA_STEP_ID": "spoofed"}
	s := u.build()
	a := &attempt{s: s, c: u.rt.ctx}
	got := map[string]string{}
	for _, kv := range a.childEnv(&secretSet{env: map[string]string{"DB_PASS": "p", "CUSTOM": "from-secret"}}) {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	want := map[string]string{
		"KODAMA_ATTEMPT_ID": "att_unit01", "KODAMA_RUN_ID": "run_01", "KODAMA_STEP_ID": "spoofed", // context env is applied after the base layer
		"KODAMA_SERVER_URL": u.cfg.ServerURL, "KODAMA_RUNTIME_URL": u.cfg.ServerURL,
		"KODAMA_WORKSPACE": u.cfg.WorkspaceDir, "KODAMA_OUTPUTS_DIR": u.cfg.OutputsDir, "KODAMA_INPUTS_DIR": u.cfg.InputsDir,
		"KODAMA_CONTEXT_FILE": filepath.Join(u.cfg.IdentityDir, "context.json"), "KODAMA_TOKEN_FILE": u.cfg.TokenFile, "KODAMA_RESULTS_FILE": u.cfg.ResultsFile,
		"KODAMA_SECRETS_DIR": u.cfg.SecretsDir, "KODAMA_INPUT_MY_NAME": "x y", "KODAMA_INPUT_COUNT": "5", "KODAMA_INPUT_FLAG": "true",
		"PATH": "/override", "CUSTOM": "from-secret", "DB_PASS": "p", "HOME": u.cfg.ChildEnv["HOME"],
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "safe.directory", "GIT_CONFIG_VALUE_0": u.cfg.WorkspaceDir,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected variable %s", k)
		}
	}
	for _, kv := range a.childEnv(&secretSet{env: map[string]string{}}) {
		if strings.Contains(kv, unitAdmission) || strings.Contains(kv, "rt_") {
			t.Fatalf("a credential reached the command environment: %s", kv)
		}
	}
}

func TestSecretNeedlesCoverEverythingIssued(t *testing.T) {
	u := newUnit(t, `exit 0`)
	s := u.build()
	s.remember(unitAdmission)
	s.writeToken("rt_first_token_0123456789")
	s.writeToken("rt_second_token_0123456789")
	a := &attempt{s: s, needles: [][]byte{[]byte("checkout-pass-1234"), []byte("injected-secret-1")}}
	var got []string
	for _, n := range a.secretNeedles() {
		got = append(got, string(n))
	}
	for _, want := range []string{unitAdmission, "rt_first_token_0123456789", "rt_second_token_0123456789", "checkout-pass-1234", "injected-secret-1"} {
		found := false
		for _, g := range got {
			found = found || g == want
		}
		if !found {
			t.Errorf("needle %q missing from %v", want, got)
		}
	}
}

func TestWriteTokenIsAtomicPrivateAndOptional(t *testing.T) {
	u := newUnit(t, `exit 0`)
	s := u.build()
	s.writeToken("rt_aaaa")
	s.writeToken("rt_bbbb")
	b, err := os.ReadFile(u.cfg.TokenFile)
	if err != nil || string(b) != "rt_bbbb" {
		t.Fatalf("token file = %q, %v", b, err)
	}
	if fi, _ := os.Stat(u.cfg.TokenFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	entries, _ := os.ReadDir(u.cfg.IdentityDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".token-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
	s2 := NewStep(Config{TokenFile: "", IdentityDir: t.TempDir()}, u.rt, u.up)
	s2.cfg.TokenFile = ""
	s2.writeToken("rt_cccc") // must not panic or write
	if !strings.Contains(string(s.issued[0]), "rt_aaaa") {
		t.Fatalf("issued = %q", s.issued)
	}
}

func timeTokenInfo(ttl, max time.Duration) runtimev1.TokenInfo {
	return tokenInfo("rt_unit_current_0123456789", ttl, max)
}

func TestRenewLoop(t *testing.T) {
	run := func(t *testing.T, u *unit, tok runtimev1.TokenInfo, within time.Duration) error {
		t.Helper()
		s := u.build()
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		done := make(chan struct{})
		go func() { defer close(done); s.renewLoop(ctx, cancel, tok) }()
		select {
		case <-done:
		case <-time.After(within):
			t.Fatal("renewLoop did not finish")
		}
		return context.Cause(ctx)
	}
	t.Run("denied renewal loses authority", func(t *testing.T) {
		u := newUnit(t, `exit 0`)
		u.rt.renew = func() (runtimev1.TokenResponse, error) {
			return runtimev1.TokenResponse{}, apiErr("renewToken", 403, runtimev1.CodeAttemptRevoked)
		}
		if err := run(t, u, timeTokenInfo(100*time.Millisecond, time.Hour), 5*time.Second); !errors.Is(err, errAuthorityLost) {
			t.Fatalf("cause = %v", err)
		}
	})
	t.Run("token at its maximum lifetime stops the command before expiry", func(t *testing.T) {
		u := newUnit(t, `exit 0`)
		u.cfg.RenewReserve = 100 * time.Millisecond
		began := time.Now()
		tok := timeTokenInfo(400*time.Millisecond, time.Hour)
		tok.MaxExpiresAt = tok.ExpiresAt // the final token: nothing left to renew
		if err := run(t, u, tok, 5*time.Second); !errors.Is(err, errAuthorityLost) {
			t.Fatalf("cause = %v", err)
		}
		if took := time.Since(began); took > 350*time.Millisecond || u.rt.count("renewToken") != 0 {
			t.Fatalf("took %s, renewals %d: the loop must stop one reserve before the maximum", took, u.rt.count("renewToken"))
		}
	})
	t.Run("transport failures are retried until the token expires", func(t *testing.T) {
		u := newUnit(t, `exit 0`)
		u.rt.renew = func() (runtimev1.TokenResponse, error) {
			return runtimev1.TokenResponse{}, &TransportError{Op: "renewToken", Err: errors.New("down")}
		}
		if err := run(t, u, timeTokenInfo(200*time.Millisecond, time.Hour), 10*time.Second); !errors.Is(err, errAuthorityLost) {
			t.Fatalf("cause = %v", err)
		}
		if u.rt.count("renewToken") < 2 {
			t.Fatalf("renewals = %d, want retries", u.rt.count("renewToken"))
		}
	})
	t.Run("successful renewals replace the token file and stop with the context", func(t *testing.T) {
		u := newUnit(t, `exit 0`)
		s := u.build()
		ctx, cancel := context.WithCancelCause(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); s.renewLoop(ctx, cancel, timeTokenInfo(60*time.Millisecond, time.Hour)) }()
		deadline := time.Now().Add(5 * time.Second)
		for u.rt.count("renewToken") < 2 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		cancel(nil)
		<-done
		if b, _ := os.ReadFile(u.cfg.TokenFile); string(b) != "rt_unit_renewed_0123456789abcdef" {
			t.Fatalf("token file = %q", b)
		}
		if errors.Is(context.Cause(ctx), errAuthorityLost) {
			t.Fatal("a clean stop was reported as lost authority")
		}
	})
}

func TestClassify(t *testing.T) {
	u := newUnit(t, `exit 0`)
	a := &attempt{s: u.build(), parent: context.Background()}
	live, cancelLive := context.WithCancelCause(context.Background())
	defer cancelLive(nil)
	a.runCtx = live
	if ab := a.classify(live, errors.New("other")); ab != nil {
		t.Fatalf("an ordinary error aborted: %+v", ab)
	}
	if ab := a.classify(live, apiErr("x", 403, runtimev1.CodeAttemptRevoked)); ab == nil || ab.exit != ExitRevoked {
		t.Fatalf("revoked = %+v", ab)
	}
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if ab := a.classify(done, errors.New("late")); ab == nil || ab.exit != ExitFinalization {
		t.Fatalf("spent budget = %+v", ab)
	}
	cancelLive(errAuthorityLost)
	if ab := a.classify(context.Background(), errors.New("x")); ab == nil || ab.exit != ExitRevoked {
		t.Fatalf("lost authority = %+v", ab)
	}
}
