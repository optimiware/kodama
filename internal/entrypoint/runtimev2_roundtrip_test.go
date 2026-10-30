package entrypoint

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// The entrypoint's v2 clients against the REAL runtime authority and v2 handler
// (memory adapters): the routes, envelopes, tokens and refusals both sides agree on.

type secretsByBinding map[string]string

func (s secretsByBinding) ResolveResourceSecret(_ context.Context, auth runtimeauth.ResourceAuthorization, req runtimev1.SecretResolveRequest, _ string) (runtimev1.SecretResolveResponse, error) {
	for _, b := range auth.Bindings() {
		if b.Ref == req.Ref && b.Version == req.Version {
			return runtimev1.SecretResolveResponse{Ref: b.Ref, Version: b.Version, Name: b.Name, Delivery: b.Delivery,
				ValueBase64: base64.StdEncoding.EncodeToString([]byte(s[b.Ref]))}, nil
		}
	}
	return runtimev1.SecretResolveResponse{}, &runtimev1.ContractError{Code: domain.CodeNotFound, Message: "no such binding"}
}

type receipts struct {
	got []runtimev2.PreparationReceiptRequest
}

func (r *receipts) RecordPreparationReceipt(_ context.Context, _ runtimeauth.ResourceAuthorization, req runtimev2.PreparationReceiptRequest) (runtimehttp.ReceiptResult, error) {
	for _, g := range r.got {
		if g != req {
			return runtimehttp.ReceiptResult{}, runtimeauth.ErrReceiptConflict
		}
	}
	r.got = append(r.got, req)
	return runtimehttp.ReceiptResult{Digest: "sha256:" + strings.Repeat("e", 64), RecordedAt: time.Now()}, nil
}

type beginOK struct{ n int }

func (b *beginOK) RecordBegin(_ context.Context, auth runtimeauth.Authorization, req runtimev2.BeginRequest) (runtimehttp.BeginResult, error) {
	attempt := auth.AttemptID
	b.n++
	state := runtimev2.StateNone
	if req.Workspace != nil {
		state = runtimev2.StateModified
	}
	return runtimehttp.BeginResult{AttemptID: attempt, BeganAt: time.Now(), WorkspaceState: state}, nil
}

func Test_unit_entrypoint_v2_roundtrip_with_the_runtime_authority(t *testing.T) {
	e := rtest.NewResourceMemStack(t)
	e.Clock.Advance(time.Since(e.Clock.Now())) // the entrypoint runs on the wall clock: so does the authority here
	rcp, begins := &receipts{}, &beginOK{}
	v2h, err := runtimehttp.NewV2Handler(runtimehttp.V2Config{
		Service: e.Service, Begins: begins, Receipts: rcp, ResourceSecrets: secretsByBinding{"secret://project/test-db-password": "pg-secret-value-0123"},
		Logger: slog.New(slog.NewTextHandler(&safeBuf{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(v2h)
	t.Cleanup(srv.Close)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}

	t.Run("service-run against the resource routes", func(t *testing.T) {
		f := rtest.ResourceFixture{Scope: scope, RunID: "run_01", WorkloadUID: "8b3e3f40-5c6d-4e7f-8a01-2c3d4e5f6071", DeadlineAt: e.Clock.Now().Add(time.Hour)}
		binding := runtimev1.SecretBinding{Ref: "secret://project/test-db-password", Version: 3, Name: "postgres-password", Delivery: runtimev1.DeliveryFile}
		st := rtest.ServiceState(f, "db", []runtimev1.SecretBinding{binding, {Ref: "secret://project/other", Version: 1, Name: "OTHER", Delivery: runtimev1.DeliveryEnv}})
		st.Context.Service.Command.Args = []string{"true"}
		e.Resources.Put(st)
		ref := runtimeauth.ResourceRef{Scope: scope, RunID: "run_01", Key: "service.db"}
		cred, _ := runtimeauth.GenerateCredential(nil)
		if err := e.Service.RegisterResource(context.Background(), runtimeauth.ResourceRegisterCommand{
			Scope: scope, RunID: "run_01", Key: "service.db", Credential: cred, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt,
		}); err != nil {
			t.Fatal(err)
		}
		dir := realTempDir(t)
		writeFile(t, filepath.Join(dir, "identity", "admission"), cred.Reveal()+"\n")
		client, err := NewResourceClient(srv.URL, "run_01", "service.db", "", RetryPolicy{MaxAttempts: 1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		logs := &safeBuf{}
		cfg := ResourceConfig{
			ServerURL: srv.URL, RunID: "run_01", Key: "service.db", AdmissionCredentialFile: filepath.Join(dir, "identity", "admission"), ClientVersion: "1.4.0",
			Identity:   runtimev2.ResourceIdentity{WorkloadName: st.WorkloadName, WorkloadUID: st.WorkloadUID},
			SecretsDir: filepath.Join(dir, "secrets"), ChildEnv: []string{"PATH=" + os.Getenv("PATH")},
			Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		}
		out := filepath.Join(dir, "out")
		st.Context.Service.Command.Args = []string{"cat " + filepath.Join(dir, "secrets", "postgres-password") + " > " + out}
		// The committed command is the one registered state holds; rebuild it with the script.
		e.Resources.Put(st)
		argv := append(append([]string{}, st.Context.Service.Command.Command...), st.Context.Service.Command.Args...)
		// The service pins a second binding that the handler's resolver does not hold
		// (a not_found): delivery fails closed and starts nothing.
		if code := newServiceRun(cfg, client, os.Stdout, os.Stderr).run(context.Background(), argv); code != ExitServiceFailed {
			t.Fatalf("a binding the server does not hold: exit %d\n%s", code, logs.String())
		}
		if _, err := os.Stat(out); err == nil {
			t.Fatal("the service ran with an undeliverable binding")
		}
		// Replaying the admission with the same credential and a new process (new
		// nonce) is admission_consumed: a restart never re-admits.
		if code := newServiceRun(cfg, client, os.Stdout, os.Stderr).run(context.Background(), argv); code != ExitAdmission {
			t.Fatalf("a second admission: exit %d", code)
		}
		_ = ref
	})

	t.Run("service-run delivers a pinned secret over the real routes", func(t *testing.T) {
		f := rtest.ResourceFixture{Scope: scope, RunID: "run_03", WorkloadUID: "9c4f4051-6d7e-4f80-9b12-3d4e5f607182", DeadlineAt: e.Clock.Now().Add(time.Hour)}
		binding := runtimev1.SecretBinding{Ref: "secret://project/test-db-password", Version: 3, Name: "postgres-password", Delivery: runtimev1.DeliveryFile}
		dir := realTempDir(t)
		logs := &safeBuf{}
		out := filepath.Join(dir, "out")
		st := rtest.ServiceState(f, "cache", []runtimev1.SecretBinding{binding})
		st.Context.Service.Command.Command = []string{"/bin/sh", "-c"}
		st.Context.Service.Command.Args = []string{"cat " + filepath.Join(dir, "secrets", "postgres-password") + " > " + out}
		e.Resources.Put(st)
		cred, _ := runtimeauth.GenerateCredential(nil)
		if err := e.Service.RegisterResource(context.Background(), runtimeauth.ResourceRegisterCommand{
			Scope: scope, RunID: "run_03", Key: "service.cache", Credential: cred, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt,
		}); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "identity", "admission"), cred.Reveal()+"\n")
		client, err := NewResourceClient(srv.URL, "run_03", "service.cache", "", RetryPolicy{MaxAttempts: 1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		cfg := ResourceConfig{
			ServerURL: srv.URL, RunID: "run_03", Key: "service.cache", AdmissionCredentialFile: filepath.Join(dir, "identity", "admission"), ClientVersion: "1.4.0",
			Identity:   runtimev2.ResourceIdentity{WorkloadName: st.WorkloadName, WorkloadUID: st.WorkloadUID},
			SecretsDir: filepath.Join(dir, "secrets"), ChildEnv: []string{"PATH=" + os.Getenv("PATH")}, Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		}
		argv := append(append([]string{}, st.Context.Service.Command.Command...), st.Context.Service.Command.Args...)
		if code := newServiceRun(cfg, client, os.Stdout, os.Stderr).run(context.Background(), argv); code != 0 {
			t.Fatalf("exit %d\n%s", code, logs.String())
		}
		if b, _ := os.ReadFile(out); string(b) != "pg-secret-value-0123" {
			t.Fatalf("the service read %q", b)
		}
	})

	t.Run("prepare records its receipt through the preparation routes", func(t *testing.T) {
		p := newPrepUnit(t, nil)
		f := rtest.ResourceFixture{Scope: scope, RunID: "run_02", WorkloadUID: "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f", DeadlineAt: e.Clock.Now().Add(time.Hour)}
		st := rtest.PreparationState(f)
		st.Context.Preparation.RepositoryURL = p.url
		st.Context.Preparation.Commit = runtimev1.GitCommit(p.first)
		e.Resources.Put(st)
		podName, podUID := "kr-0123456789abcdef01234567-x7k2p", "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60"
		e.Resources.BindPod(f.WorkloadUID, podName, podUID)
		cred, _ := runtimeauth.GenerateCredential(nil)
		if err := e.Service.RegisterResource(context.Background(), runtimeauth.ResourceRegisterCommand{
			Scope: scope, RunID: "run_02", Key: "preparation", Credential: cred, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt,
		}); err != nil {
			t.Fatal(err)
		}
		writeFile(t, p.cfg.AdmissionCredentialFile, cred.Reveal()+"\n")
		client, err := NewResourceClient(srv.URL, "run_02", "preparation", "", RetryPolicy{MaxAttempts: 1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		cfg := p.cfg
		cfg.ServerURL, cfg.RunID, cfg.ClientVersion = srv.URL, "run_02", "1.4.0"
		cfg.Identity = runtimev2.ResourceIdentity{WorkloadName: st.WorkloadName, WorkloadUID: st.WorkloadUID, PodName: &podName, PodUID: &podUID}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if code := newPreparation(cfg, client).run(ctx); code != ExitOK {
			t.Fatalf("exit %d\n%s", code, p.logs.String())
		}
		if len(rcp.got) != 1 || string(rcp.got[0].Commit) != p.first || rcp.got[0].Attempts != 1 || !rcp.got[0].CredentialsRemoved {
			t.Fatalf("receipts %+v", rcp.got)
		}
		if head := tgit(t, cfg.WorkspaceDir, "rev-parse", "HEAD"); head != p.first {
			t.Fatalf("HEAD %s", head)
		}
		// A second preparation of the same resource cannot admit: the credential is spent.
		writeFile(t, p.cred, "reader:"+prepToken+"\n")
		if code := newPreparation(cfg, client).run(ctx); code != ExitAdmission {
			t.Fatalf("a second run: exit %d", code)
		}
	})

	t.Run("a v2 attempt against the attempt routes", func(t *testing.T) {
		sc := rtest.LoadV2Scenario(t, "begin_source_free")
		c2 := rtest.ContextV2Of(t, sc.Steps[0])
		c2.Command.Command, c2.Command.Args = []string{"/bin/sh", "-c"}, []string{"exit 0"}
		c2.Command.Env = map[string]string{}
		acts, _ := runtimeauth.NewActionSet(runtimeauth.DefaultActions(false)...)
		att := e.SeedAttemptV2(c2, acts)
		cred, _ := runtimeauth.GenerateCredential(nil)
		if _, err := e.RegisterV2With(att, c2, cred, acts); err != nil {
			t.Fatal(err)
		}
		id := e.Workload(att)
		u := newUnit(t, "true")
		u.cfg.RuntimeAPI, u.cfg.ServerURL, u.cfg.AttemptID, u.cfg.Job = RuntimeAPIv2, srv.URL, string(c2.AttemptID), id
		u.cfg.ClientVersion = "1.4.0"
		writeFile(t, u.cfg.AdmissionCredentialFile, cred.Reveal()+"\n")
		if err := os.MkdirAll(u.cfg.WorkspaceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		code := Execute(ctx, u.cfg)
		// The command ran only after begin was recorded. The result route is owned by
		// another module and is not wired into this handler: the exit proves the
		// fence, not the finalization.
		if begins.n != 1 {
			t.Fatalf("begin was recorded %d times (exit %d)\n%s", begins.n, code, u.logs.String())
		}
		if !strings.Contains(u.logs.String(), "command begin acknowledged") || !strings.Contains(u.logs.String(), "launching command") {
			t.Fatalf("logs:\n%s", u.logs.String())
		}
		if code == ExitUsage {
			t.Fatalf("exit %d", code)
		}
		_ = errors.New
	})
}
