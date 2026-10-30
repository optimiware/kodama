// Package artifactstest builds the isolated test environment of the artifact
// module: a migrated PostgreSQL database with two tenants, an isolated S3
// prefix, the real adapters and controllable stand-ins for the injected
// authorities (attempt authority, read authority, registry). Only the
// authorities are fakes: they are consumer-owned ports whose production
// implementations belong to the execution and integration tasks.
package artifactstest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/artifacts"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	arts3 "github.com/optimiware/kodama/internal/artifacts/s3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
	s3support "github.com/optimiware/kodama/test/support/s3"
)

// Fixed identifiers of the two seeded tenants.
const (
	TenantA   = domain.TenantID("t_acme")
	TenantB   = domain.TenantID("t_globex")
	ProjectA  = domain.ProjectID("p_inventory")
	ProjectA2 = domain.ProjectID("p_billing")
	ProjectB  = domain.ProjectID("p_ops")
)

var (
	ScopeA  = domain.Scope{TenantID: TenantA, ProjectID: ProjectA}
	ScopeA2 = domain.Scope{TenantID: TenantA, ProjectID: ProjectA2}
	ScopeB  = domain.Scope{TenantID: TenantB, ProjectID: ProjectB}
)

// Commit is a fixed trusted source commit.
const Commit = "1111111111111111111111111111111111111111"

// Source is the trusted provenance the default grants carry.
var Source = artifacts.Provenance{RepositoryURL: "https://git.example.test/acme/app.git", Commit: Commit}

// Clock is the shared test clock: the wall clock plus an adjustable offset, so
// signed URLs (which use real signing time) stay consistent with it.
type Clock struct {
	mu     sync.Mutex
	offset time.Duration
}

// Now returns the adjusted time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().UTC().Add(c.offset)
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

// Env is one test's wiring.
type Env struct {
	T         testing.TB
	DB        *pgtest.Database
	Runtime   *pgxpool.Pool
	Owner     *pgxpool.Pool
	S3        *s3support.Env
	Objects   *arts3.Store
	Store     *artifactspg.Store
	Authority *Authority
	Reader    *Reader
	Registry  *Registry
	Clock     *Clock
	Service   *artifacts.Service
	Config    artifacts.ServiceConfig
	HTTP      *http.Client
}

// Options tune the environment.
type Options struct {
	ClaimLease  time.Duration
	UploadTTL   time.Duration
	DownloadTTL time.Duration
	// Verifier replaces the default programmable registry.
	Verifier artifacts.RegistryVerifier
	// WrapObjects wraps the object store (fault injection).
	WrapObjects func(artifacts.ObjectStore) artifacts.ObjectStore
	// WrapRepository wraps the repository (fault injection).
	WrapRepository func(artifacts.Repository) artifacts.Repository
	// RuntimeConns sizes the runtime pool (default 12).
	RuntimeConns int32
	// Extra lists contributions applied after the baseline (for example the
	// workspace provenance of run_shared artifacts).
	Extra []storage.Contribution
}

// NewDB creates the migrated database, the store and the authorities without
// any object store. Tests that need bytes use New.
func NewDB(t testing.TB, opts Options) *Env {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conns := opts.RuntimeConns
	if conns == 0 {
		conns = 12
	}
	e := &Env{T: t, DB: db, Owner: db.MigrationPool(ctx, 4), Runtime: db.RuntimePool(ctx, conns), Clock: &Clock{},
		Authority: NewAuthority(), Reader: NewReader(), HTTP: &http.Client{Timeout: 20 * time.Second}}
	manifest, err := storage.NewManifest(append([]storage.Contribution{storage.BaseContribution()}, opts.Extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, e.Owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// The baseline carries the execution link that makes an attempt row the
	// producer of its artifacts. This fixture exercises the artifact module
	// without execution (its authority is in memory), so it drops that one
	// constraint; the execution composition tests cover it against real attempts.
	pgtest.DropExecutionLinks(t, e.Owner, "artifacts", "artifacts_producer_attempt_fk")
	e.seed(TenantA, ProjectA, ProjectA2)
	e.seed(TenantB, ProjectB)
	if e.Store, err = artifactspg.NewStore(artifactspg.Config{DB: e.Runtime}); err != nil {
		t.Fatal(err)
	}
	e.Registry = NewRegistry()
	return e
}

// New creates the full environment. It skips (or fails with KODAMA_REQUIRE_S3=1)
// when the S3 fixture is not configured.
func New(t testing.TB, opts Options) *Env {
	t.Helper()
	e := NewDB(t, opts)
	e.S3 = s3support.New(t)
	var err error
	e.Objects, err = arts3.NewStore(arts3.Config{Client: e.S3.Client, Bucket: e.S3.Bucket(), Prefix: e.S3.Prefix, AllowInsecure: true, Clock: e.Clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	e.Build(opts)
	return e
}

// Build (re)constructs the service from the current adapters and options.
func (e *Env) Build(opts Options) {
	e.T.Helper()
	cfg := artifacts.ServiceConfig{
		Repository: artifacts.Repository(e.Store), Authority: e.Authority, Reader: e.Reader,
		Objects: artifacts.ObjectStore(e.Objects), Registry: artifacts.RegistryVerifier(e.Registry),
		Clock: e.Clock.Now, UploadTTL: or(opts.UploadTTL, 10*time.Minute), DownloadTTL: or(opts.DownloadTTL, 5*time.Minute),
		ClaimLease: or(opts.ClaimLease, 30*time.Second), SpoolDir: e.T.TempDir(),
	}
	if opts.Verifier != nil {
		cfg.Registry = opts.Verifier
	}
	if opts.WrapObjects != nil {
		cfg.Objects = opts.WrapObjects(cfg.Objects)
	}
	if opts.WrapRepository != nil {
		cfg.Repository = opts.WrapRepository(cfg.Repository)
	}
	svc, err := artifacts.NewService(cfg)
	if err != nil {
		e.T.Fatal(err)
	}
	e.Config, e.Service = cfg, svc
}

func or(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

func (e *Env) seed(tenant domain.TenantID, projects ...domain.ProjectID) {
	e.T.Helper()
	ctx := context.Background()
	if _, err := e.Owner.Exec(ctx, `INSERT INTO tenants (id, key) VALUES ($1::text, $1::text)`, string(tenant)); err != nil {
		e.T.Fatalf("seed tenant: %v", err)
	}
	for _, p := range projects {
		if _, err := e.Owner.Exec(ctx, `INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5)`, string(tenant), string(p)); err != nil {
			e.T.Fatalf("seed project: %v", err)
		}
	}
}

// Ctx returns a bounded context for the test.
func (e *Env) Ctx() context.Context {
	e.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	e.T.Cleanup(cancel)
	return ctx
}

// Caller returns the runtime caller bound to an attempt by Attempt.
func Caller(attempt domain.AttemptID) artifacts.Caller {
	return artifacts.Caller{Subject: "attempt:" + string(attempt)}
}

// Attempt registers an active attempt with the given declared outputs and
// returns its caller.
func (e *Env) Attempt(scope domain.Scope, attempt domain.AttemptID, outputs ...artifacts.OutputDecl) artifacts.Caller {
	e.T.Helper()
	g := artifacts.AttemptGrant{
		Scope: scope, Attempt: attempt, Run: "run_1", Step: "build", ExpiresAt: e.Clock.Now().Add(time.Hour), Writer: true, Source: Source,
		Outputs: outputs, Limits: artifacts.Limits{OutputBytes: 1 << 20, OutputFiles: 8, DiagnosticBytes: 1 << 20},
	}
	e.Authority.Set(Caller(attempt).Subject, g)
	return Caller(attempt)
}

// Renew extends the attempt's authority to an hour from the (possibly advanced)
// test clock.
func (e *Env) Renew(caller artifacts.Caller) {
	e.T.Helper()
	e.Authority.Update(caller.Subject, func(g *artifacts.AttemptGrant) { g.ExpiresAt = e.Clock.Now().Add(time.Hour) })
}

// Upload performs the reservation's transfer with the issued instruction.
func (e *Env) Upload(up artifacts.Transfer, data []byte) (int, string) {
	e.T.Helper()
	req, err := http.NewRequest(up.Method, up.URL, bytes.NewReader(data))
	if err != nil {
		e.T.Fatal(err)
	}
	req.ContentLength = int64(len(data))
	for k, v := range up.Headers {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Host") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		e.T.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	return resp.StatusCode, string(b)
}

// Download fetches a download instruction's content.
func (e *Env) Download(t artifacts.Transfer) (int, []byte) {
	e.T.Helper()
	req, err := http.NewRequest(t.Method, t.URL, nil)
	if err != nil {
		e.T.Fatal(err)
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		e.T.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// FileInput is a reservation input for in-memory content.
func FileInput(name string, role artifacts.Role, data []byte) artifacts.ReserveInput {
	mt, size := "text/plain", int64(len(data))
	return artifacts.ReserveInput{Name: name, Kind: artifacts.KindFile, Role: role, Digest: artifacts.DigestOf(data), MediaType: &mt, Size: &size}
}

// ReserveAndUpload reserves, uploads and returns the artifact ID.
func (e *Env) ReserveAndUpload(caller artifacts.Caller, attempt domain.AttemptID, name string, role artifacts.Role, data []byte) domain.ArtifactID {
	e.T.Helper()
	out, err := e.Service.Reserve(e.Ctx(), caller, attempt, FileInput(name, role, data), "req_test")
	if err != nil {
		e.T.Fatalf("reserve %s: %v", name, err)
	}
	if out.Upload == nil {
		e.T.Fatalf("reserve %s: no upload", name)
	}
	if code, body := e.Upload(*out.Upload, data); code != 200 {
		e.T.Fatalf("upload %s: %d %s", name, code, body)
	}
	return out.ID
}

// Complete completes an uploaded file with its own declaration.
func (e *Env) Complete(caller artifacts.Caller, attempt domain.AttemptID, id domain.ArtifactID, data []byte) (artifacts.Completion, error) {
	return e.Service.Complete(e.Ctx(), caller, attempt, id, artifacts.CompleteInput{Digest: artifacts.DigestOf(data), Size: int64(len(data))}, "req_test")
}

// Count runs a count query as the owner (every tenant visible).
func (e *Env) Count(query string, args ...any) int {
	e.T.Helper()
	var n int
	if err := e.Owner.QueryRow(e.Ctx(), query, args...).Scan(&n); err != nil {
		e.T.Fatalf("count: %v\n%s", err, query)
	}
	return n
}

// Exec runs a statement as the owner.
func (e *Env) Exec(query string, args ...any) {
	e.T.Helper()
	if _, err := e.Owner.Exec(e.Ctx(), query, args...); err != nil {
		e.T.Fatalf("exec: %v\n%s", err, query)
	}
}

// ObjectExists reports whether the sealed or staging object is present in S3.
func (e *Env) ObjectExists(key string) bool {
	e.T.Helper()
	obj, err := e.Objects.Open(e.Ctx(), key)
	if err != nil {
		return false
	}
	obj.Body.Close()
	return true
}

// ObjectBytes returns the object's bytes.
func (e *Env) ObjectBytes(key string) []byte {
	e.T.Helper()
	obj, err := e.Objects.Open(e.Ctx(), key)
	if err != nil {
		e.T.Fatalf("open %s: %v", key, err)
	}
	defer obj.Body.Close()
	b, _ := io.ReadAll(obj.Body)
	return b
}

// KeysOf returns (state, key) pairs of the artifact's object records.
func (e *Env) ObjectRecords(id domain.ArtifactID) map[string]string {
	e.T.Helper()
	rows, err := e.Owner.Query(e.Ctx(), `SELECT key, state || CASE WHEN winner THEN ':winner' ELSE '' END FROM artifact_objects WHERE artifact_id = $1`, string(id))
	if err != nil {
		e.T.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, s string
		if err := rows.Scan(&k, &s); err != nil {
			e.T.Fatal(err)
		}
		out[k] = s
	}
	return out
}

// ObjectRecordsIn is ObjectRecords of one project's artifact: the same ID may
// exist in several projects of a tenant, so the records are filtered by tenant,
// project and artifact ID, read in the tenant's context.
func (e *Env) ObjectRecordsIn(scope domain.Scope, id domain.ArtifactID) map[string]string {
	e.T.Helper()
	out := map[string]string{}
	err := storage.WithTenant(e.Ctx(), e.Owner, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT key, state || CASE WHEN winner THEN ':winner' ELSE '' END FROM artifact_objects
WHERE tenant_id = $1 AND project_id = $2 AND artifact_id = $3`, string(scope.TenantID), string(scope.ProjectID), string(id))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k, s string
			if err := rows.Scan(&k, &s); err != nil {
				return err
			}
			out[k] = s
		}
		return rows.Err()
	})
	if err != nil {
		e.T.Fatal(err)
	}
	return out
}

// Fatalf is a convenience for helpers.
func (e *Env) Fatalf(format string, args ...any) { e.T.Helper(); e.T.Fatalf(format, args...) }

// ---- authorities ----------------------------------------------------------

// Authority is a controllable artifacts.AttemptAuthority.
type Authority struct {
	mu      sync.Mutex
	grants  map[string]artifacts.AttemptGrant // subject -> grant
	revoked map[domain.AttemptID]bool
	failure error
	calls   int
}

// NewAuthority returns an empty authority: everything is denied until Set.
func NewAuthority() *Authority {
	return &Authority{grants: map[string]artifacts.AttemptGrant{}, revoked: map[domain.AttemptID]bool{}}
}

// Set binds the subject to the grant's attempt.
func (a *Authority) Set(subject string, g artifacts.AttemptGrant) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.grants[subject] = g
}

// Revoke makes the attempt inactive.
func (a *Authority) Revoke(attempt domain.AttemptID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.revoked[attempt] = true
}

// Fail makes every call fail with err (an outage); nil restores service.
func (a *Authority) Fail(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failure = err
}

// Calls counts Grant calls: replays and repeats must reauthorize.
func (a *Authority) Calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// Update mutates a registered grant.
func (a *Authority) Update(subject string, fn func(*artifacts.AttemptGrant)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.grants[subject]
	fn(&g)
	a.grants[subject] = g
}

// Grant implements artifacts.AttemptAuthority.
func (a *Authority) Grant(_ context.Context, caller artifacts.Caller, attempt domain.AttemptID) (artifacts.AttemptGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.failure != nil {
		return artifacts.AttemptGrant{}, a.failure
	}
	g, ok := a.grants[caller.Subject]
	if !ok {
		return artifacts.AttemptGrant{}, artifacts.ErrAuthorityMissing
	}
	if g.Attempt != attempt {
		// A token for one attempt addressing another is indistinguishable from
		// an unknown attempt.
		return artifacts.AttemptGrant{}, artifacts.ErrNotFound
	}
	if a.revoked[attempt] {
		return artifacts.AttemptGrant{}, artifacts.ErrAttemptInactive
	}
	return g, nil
}

// Reader is a controllable artifacts.ReadAuthority.
type Reader struct {
	mu       sync.Mutex
	scopes   map[string]map[domain.Scope]bool
	noDiag   map[string]bool
	failure  error
	calls    int
	subjects []artifacts.ReadSubject
}

// NewReader returns a reader that denies everything until Allow.
func NewReader() *Reader {
	return &Reader{scopes: map[string]map[domain.Scope]bool{}, noDiag: map[string]bool{}}
}

// Allow grants the subject read access to the scope.
func (r *Reader) Allow(subject string, scope domain.Scope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.scopes[subject] == nil {
		r.scopes[subject] = map[domain.Scope]bool{}
	}
	r.scopes[subject][scope] = true
}

// Revoke removes the subject's access to the scope.
func (r *Reader) Revoke(subject string, scope domain.Scope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.scopes[subject], scope)
}

// DenyDiagnostics hides diagnostic artifacts from the subject.
func (r *Reader) DenyDiagnostics(subject string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noDiag[subject] = true
}

// Fail makes every call fail with err (an outage).
func (r *Reader) Fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failure = err
}

// Calls returns the number of authorizations and the subjects asked about.
func (r *Reader) Calls() (int, []artifacts.ReadSubject) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, append([]artifacts.ReadSubject(nil), r.subjects...)
}

// AuthorizeRead implements artifacts.ReadAuthority.
func (r *Reader) AuthorizeRead(_ context.Context, caller artifacts.Caller, scope domain.Scope, subject artifacts.ReadSubject) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.subjects = append(r.subjects, subject)
	if r.failure != nil {
		return r.failure
	}
	if !r.scopes[caller.Subject][scope] {
		return artifacts.ErrNotFound
	}
	if subject.Role == artifacts.RoleDiagnostic && r.noDiag[caller.Subject] {
		return artifacts.ErrForbidden
	}
	return nil
}

// Registry is a programmable artifacts.RegistryVerifier.
type Registry struct {
	mu      sync.Mutex
	known   map[string]artifacts.OCIVerification
	failure error
	calls   int
}

// NewRegistry returns a verifier that knows no content.
func NewRegistry() *Registry { return &Registry{known: map[string]artifacts.OCIVerification{}} }

// Add makes the registry/digest verifiable.
func (r *Registry) Add(registry string, digest artifacts.Digest, prov artifacts.Provenance) {
	ref, err := artifacts.ParseRegistryRef(registry)
	if err != nil {
		panic(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.known[ref.String()+"@"+string(digest)] = artifacts.OCIVerification{Repository: ref.String(), Digest: digest,
		ManifestMediaType: "application/vnd.oci.image.manifest.v1+json", ManifestSize: 512, Provenance: prov}
}

// Fail makes verification fail with err.
func (r *Registry) Fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failure = err
}

// Calls counts verifications.
func (r *Registry) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// Verify implements artifacts.RegistryVerifier.
func (r *Registry) Verify(_ context.Context, req artifacts.OCIRequest) (artifacts.OCIVerification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.failure != nil {
		return artifacts.OCIVerification{}, r.failure
	}
	ref, err := artifacts.ParseRegistryRef(req.Registry)
	if err != nil {
		return artifacts.OCIVerification{}, artifacts.ErrRegistryDenied
	}
	v, ok := r.known[ref.String()+"@"+string(req.Digest)]
	if !ok {
		return artifacts.OCIVerification{}, artifacts.ErrRegistryContent
	}
	return v, nil
}

// GrantRead is the consumer-read variant of Grant. The fixture deliberately
// applies the same grant and lets the test set the Inputs it authorizes.
func (a *Authority) GrantRead(ctx context.Context, caller artifacts.Caller, attempt domain.AttemptID) (artifacts.AttemptGrant, error) {
	return a.Grant(ctx, caller, attempt)
}
