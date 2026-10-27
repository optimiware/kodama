// Package logstest holds the fixtures of the log module's tests: one isolated
// PostgreSQL database migrated with every module the log tables depend on (the
// real execution, identity and artifact stores), an isolated S3 prefix, the
// real PostgreSQL and S3 adapters and the log module wired over them. Doubles
// stand in only for external seams: the Kubernetes API (kubefake, for
// controlled faults), the artifact module's runtime attempt authority and the
// injected redaction values. It contains no production behavior.
//
// Resource prefix t023: tenants, projects, users and S3 subprefixes carry it
// (through the execution fixtures' t020 identifiers and KODAMA_RESOURCE_PREFIX).
package logstest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	arts3 "github.com/optimiware/kodama/internal/artifacts/s3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/adapters"
	"github.com/optimiware/kodama/internal/logs/contract"
	logspg "github.com/optimiware/kodama/internal/logs/postgres"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/storage"
	s3support "github.com/optimiware/kodama/test/support/s3"
)

// Prefix is this task's resource prefix.
const Prefix = "t023"

// Fixture constants.
const (
	Namespace = "t023-runs"
	Container = "step"
	JobUID    = "11111111-2222-3333-4444-555555555555"
)

// Scopes and users come from the execution fixtures: engineers and viewers
// hold run:read and evidence:read; strangers hold nothing.
var (
	ScopeA  = executiontest.ScopeA
	ScopeA2 = executiontest.ScopeA2
	ScopeB  = executiontest.ScopeB
)

const (
	Engineer = executiontest.Engineer
	Viewer   = executiontest.Viewer
	Stranger = executiontest.Stranger
	OtherEng = executiontest.OtherEng
)

// Options tune the environment.
type Options struct {
	// NoS3 skips the object store (database-only tests).
	NoS3 bool
}

// Env is one fully wired log module over real PostgreSQL and S3.
type Env struct {
	*executiontest.Env

	S3      *s3support.Env
	Objects *arts3.Store
	// FaultRepo and FaultObjects wrap the artifact repository and object store
	// the log adapters use, for upload/commit response-loss injection.
	FaultRepo    *artifactstest.FaultRepo
	FaultObjects *artifactstest.FaultObjects
	ArtStore     *artifactspg.Store
	// ArtSvc is the real artifact service a wrapper uses through the runtime
	// interface; ArtAuth is its (double) attempt authority.
	ArtSvc  *artifacts.Service
	ArtAuth *artifactstest.Authority
	ArtRdr  *artifactstest.Reader

	Logs *logspg.Store
	// Identity is the real standalone identity authority (current bindings in
	// the test database); Authority is the log module's adapter over it.
	Identity  identity.Authority
	Authority *adapters.Authority
	Attempts  *adapters.Attempts
	Workloads *adapters.Workloads
	Reader    *adapters.Artifacts
	Retained  *adapters.Retained
	Redact    *Redactions
	HTTP      *http.Client

	ids atomic.Int64
}

// New builds the environment. PostgreSQL and S3 follow test/support: a test
// skips when the fixture is unavailable and fails instead when
// KODAMA_REQUIRE_POSTGRES=1 / KODAMA_REQUIRE_S3=1.
func New(t testing.TB, o ...Options) *Env {
	t.Helper()
	var opt Options
	if len(o) > 0 {
		opt = o[0]
	}
	ex := executiontest.New(t)
	ex.GrantAll()
	lim := executiontest.DefaultLimit()
	lim.TenantConcurrency, lim.ProjectConcurrency = 1000, 500 // many attempts stay running across subtests
	ex.Limits.Set(lim)
	e := &Env{Env: ex, Redact: &Redactions{}, HTTP: &http.Client{Timeout: 20 * time.Second}}

	manifest, err := storage.NewManifest(storage.BaseContribution(), execpg.RunResourceMigrations())
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := storage.Migrate(ctx, ex.Migration, manifest, storage.MigrateOptions{RuntimeRole: ex.Base.DB.RuntimeRole}); err != nil {
		t.Fatalf("migrate logs: %v", err)
	}

	if e.ArtStore, err = artifactspg.NewStore(artifactspg.Config{DB: ex.Runtime}); err != nil {
		t.Fatal(err)
	}
	if e.Logs, err = logspg.NewStore(logspg.Config{DB: ex.Runtime}); err != nil {
		t.Fatal(err)
	}
	if !opt.NoS3 {
		e.S3 = s3support.New(t)
		if e.Objects, err = arts3.NewStore(arts3.Config{Client: e.S3.Client, Bucket: e.S3.Bucket(), Prefix: e.S3.Prefix, AllowInsecure: true, Clock: ex.Clock.Now}); err != nil {
			t.Fatal(err)
		}
		e.FaultRepo = &artifactstest.FaultRepo{Repository: e.ArtStore}
		e.FaultObjects = &artifactstest.FaultObjects{ObjectStore: e.Objects}
		e.ArtAuth, e.ArtRdr = artifactstest.NewAuthority(), artifactstest.NewReader()
		e.ArtSvc, err = artifacts.NewService(artifacts.ServiceConfig{
			Repository: e.ArtStore, Authority: e.ArtAuth, Reader: e.ArtRdr, Objects: e.Objects, Registry: artifactstest.NewRegistry(),
			Clock: ex.Clock.Now, UploadTTL: 10 * time.Minute, DownloadTTL: 5 * time.Minute, ClaimLease: 30 * time.Second, SpoolDir: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if e.Reader, err = adapters.NewArtifacts(e.FaultRepo, e.FaultObjects); err != nil {
			t.Fatal(err)
		}
		if e.Retained, err = adapters.NewRetained(e.Reader, adapters.WriterConfig{
			Repository: e.FaultRepo, Objects: e.FaultObjects, Clock: ex.Clock.Now,
			Limits: artifacts.Limits{DiagnosticBytes: 1 << 24, DiagnosticFiles: 32}, SpoolDir: t.TempDir(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	reader, err := identitypg.NewStore(ex.Runtime, ex.Events)
	if err != nil {
		t.Fatal(err)
	}
	std, err := identity.NewStandaloneAuthority(reader)
	if err != nil {
		t.Fatal(err)
	}
	e.Identity = std
	if e.Authority, err = adapters.NewAuthority(std, reader); err != nil {
		t.Fatal(err)
	}
	if e.Attempts, err = adapters.NewAttempts(ex.Store); err != nil {
		t.Fatal(err)
	}
	if e.Workloads, err = adapters.NewWorkloads(adapters.StaticPlacement{Namespace: Namespace, Container: Container}); err != nil {
		t.Fatal(err)
	}
	return e
}

// Limits are the test ceilings: small enough to hit, explicit in every test.
func Limits() logs.Limits {
	return logs.Limits{MaxLines: 1000, MaxBytes: 64 << 10, MaxLineBytes: 1024, MaxDuration: 20 * time.Second, MaxStreams: 4, WriteTimeout: 2 * time.Second}
}

// Service builds the read service over the real ports and the given live
// reader.
func (e *Env) Service(live logs.LiveReader, lim logs.Limits) *logs.Service {
	e.T.Helper()
	svc, err := logs.NewService(logs.Config{Authority: e.Authority, Attempts: e.Attempts, Workloads: e.Workloads, Live: live, Retained: e.Retained,
		Store: e.Logs, Redactions: e.Redact, Limits: lim, Clock: e.Clock.Now})
	if err != nil {
		e.T.Fatal(err)
	}
	return svc
}

// ControllerOptions tune the fallback.
type ControllerOptions struct {
	CaptureBytes int64
	Lease        time.Duration
	Retained     logs.Retained
	Store        logs.Store
	Redactions   logs.Redactions
}

// Controller builds the controller over the real ports and the given live
// reader.
func (e *Env) Controller(live logs.LiveReader, o ControllerOptions) *logs.Controller {
	e.T.Helper()
	if o.CaptureBytes == 0 {
		o.CaptureBytes = 1 << 20
	}
	if o.Lease == 0 {
		o.Lease = 30 * time.Second
	}
	var ret logs.Retained = e.Retained
	if o.Retained != nil {
		ret = o.Retained
	}
	var store logs.Store = e.Logs
	if o.Store != nil {
		store = o.Store
	}
	var red logs.Redactions = e.Redact
	if o.Redactions != nil {
		red = o.Redactions
	}
	c, err := logs.NewController(logs.ControllerConfig{Attempts: e.Attempts, Workloads: e.Workloads, Live: live, Retained: ret, Store: store,
		Redactions: red, Clock: e.Clock.Now, CaptureBytes: o.CaptureBytes, CaptureTimeout: 20 * time.Second, Lease: o.Lease, SpoolDir: e.T.TempDir()})
	if err != nil {
		e.T.Fatal(err)
	}
	return c
}

// Attempt is one real attempt of a real run.
type Attempt struct {
	Scope  domain.Scope
	Run    domain.RunID
	ID     domain.AttemptID
	Intent *execution.AttemptSpec
	Job    string
}

// Info returns the attempt as the log module sees it.
func (e *Env) Info(a Attempt) logs.AttemptInfo {
	e.T.Helper()
	info, err := e.Attempts.ByID(context.Background(), a.Scope, a.ID)
	if err != nil {
		e.T.Fatalf("attempt info: %v", err)
	}
	return info
}

// NewAttempt admits a run in the scope and claims its single step. With start
// the Job is observed (its UID persisted), so a live workload can be selected.
func (e *Env) NewAttempt(scope domain.Scope, start bool) Attempt {
	e.T.Helper()
	if scope.TenantID != ScopeA.TenantID {
		// The execution fixtures admit as the engineer, who is bound only in
		// tenant A: bind for the admission and revoke, so tests of tenant B keep
		// OtherEng as its only member.
		e.Grant(scope, Engineer, identity.RoleEngineer)
		defer e.Revoke(scope, Engineer)
	}
	p := e.NewProject(scope)
	def := e.Define(scope, pipelinetest.Doc(pipelinetest.Container("build", "workspace: {access: read, from: run}")))
	run := e.MustAdmit(p, def, nil)
	in := e.MustClaim(scope.TenantID)
	if start {
		e.Start(in)
	}
	return Attempt{Scope: scope, Run: run.ID, ID: in.AttemptID, Intent: in, Job: in.WorkloadName}
}

// NewAttempts admits one run of n independent steps and claims all of them, so
// the run has n attempts and a log read must name one.
func (e *Env) NewAttempts(scope domain.Scope, n int, start bool) []Attempt {
	e.T.Helper()
	if scope.TenantID != ScopeA.TenantID {
		e.Grant(scope, Engineer, identity.RoleEngineer)
		defer e.Revoke(scope, Engineer)
	}
	p := e.NewProject(scope)
	var steps []string
	for i := 0; i < n; i++ {
		steps = append(steps, pipelinetest.Container(fmt.Sprintf("step%d", i), "workspace: {access: read, from: run}"))
	}
	def := e.Define(scope, pipelinetest.Doc(steps...))
	run := e.MustAdmit(p, def, nil)
	var out []Attempt
	for i := 0; i < n; i++ {
		in := e.MustClaim(scope.TenantID)
		if start {
			e.Start(in)
		}
		out = append(out, Attempt{Scope: scope, Run: run.ID, ID: in.AttemptID, Intent: in, Job: in.WorkloadName})
	}
	return out
}

// Finish ends the attempt with a failed result: logs outlive the command.
func (e *Env) Finish(a Attempt) {
	e.T.Helper()
	if _, err := e.Fail(a.Intent); err != nil {
		e.T.Fatalf("finish: %v", err)
	}
}

// Target is the workload the log module resolves for the attempt.
func (e *Env) Target(a Attempt) logs.Target {
	e.T.Helper()
	tgt, err := e.Workloads.Target(context.Background(), e.Info(a))
	if err != nil {
		e.T.Fatalf("target: %v", err)
	}
	return tgt
}

// WrapperUpload performs the wrapper's runtime upload of one diagnostic file
// through the real artifact service: reserve, signed transfer, complete.
func (e *Env) WrapperUpload(a Attempt, name string, mediaType string, data []byte) domain.ArtifactID {
	e.T.Helper()
	caller := artifactstest.Caller(a.ID)
	e.ArtAuth.Set(caller.Subject, artifacts.AttemptGrant{Scope: a.Scope, Attempt: a.ID, Run: a.Run, Step: "build", ExpiresAt: e.Clock.Now().Add(time.Hour),
		Limits: artifacts.Limits{OutputBytes: 1 << 20, OutputFiles: 8, DiagnosticBytes: 1 << 24}})
	size := int64(len(data))
	ctx := context.Background()
	out, err := e.ArtSvc.Reserve(ctx, caller, a.ID, artifacts.ReserveInput{Name: name, Kind: artifacts.KindFile, Role: artifacts.RoleDiagnostic,
		Digest: artifacts.DigestOf(data), MediaType: &mediaType, Size: &size}, "req_t023")
	if err != nil {
		e.T.Fatalf("reserve %s: %v", name, err)
	}
	e.put(*out.Upload, data)
	if _, err := e.ArtSvc.Complete(ctx, caller, a.ID, out.ID, artifacts.CompleteInput{Digest: artifacts.DigestOf(data), Size: size}, "req_t023"); err != nil {
		e.T.Fatalf("complete %s: %v", name, err)
	}
	return out.ID
}

func (e *Env) put(up artifacts.Transfer, data []byte) {
	e.T.Helper()
	req, err := http.NewRequest(up.Method, up.URL, bytesReader(data))
	if err != nil {
		e.T.Fatal(err)
	}
	req.ContentLength = int64(len(data))
	for k, v := range up.Headers {
		if k == "Content-Length" || k == "Host" {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		e.T.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		e.T.Fatalf("upload: %d %s", resp.StatusCode, b)
	}
}

// WrapperLog uploads the wrapper's command log and, when meta is non-nil, a
// sidecar built from it for the uploaded log (completeness c with reasons).
func (e *Env) WrapperLog(a Attempt, text string, c contract.Completeness, reasons ...contract.Reason) domain.ArtifactID {
	e.T.Helper()
	data := []byte(text)
	id := e.WrapperUpload(a, contract.CommandLogName, contract.LogMediaType, data)
	doc := contract.Metadata{Version: contract.Version, Source: contract.SourceWrapper, LogName: contract.CommandLogName,
		Log:          contract.Ref{ArtifactID: id, Digest: string(artifacts.DigestOf(data)), SizeBytes: int64(len(data))},
		Completeness: c, Reasons: reasons}
	b, err := contract.Encode(doc)
	if err != nil {
		e.T.Fatal(err)
	}
	name, _ := contract.MetadataName(contract.CommandLogName)
	e.WrapperUpload(a, name, contract.MediaType, b)
	return id
}

// LegacyWrapperLog uploads only the plain-text command log, as the existing
// entrypoint does.
func (e *Env) LegacyWrapperLog(a Attempt, text string) domain.ArtifactID {
	e.T.Helper()
	return e.WrapperUpload(a, contract.CommandLogName, contract.LogMediaType, []byte(text))
}

// Redactions is a controllable logs.Redactions.
type Redactions struct {
	mu     sync.Mutex
	values [][]byte
	err    error
}

// Set replaces the known values.
func (r *Redactions) Set(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = nil
	for _, v := range values {
		r.values = append(r.values, []byte(v))
	}
}

// Fail makes the source fail.
func (r *Redactions) Fail(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

// Values implements logs.Redactions.
func (r *Redactions) Values(context.Context, domain.Scope, domain.AttemptID) ([][]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.values, r.err
}
