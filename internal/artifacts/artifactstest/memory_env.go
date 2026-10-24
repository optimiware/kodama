package artifactstest

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
)

// MemEnv is the whole artifact module in memory: the memory persistence, the
// in-memory object store with its loopback transfer endpoint, the controllable
// authorities and the real artifact Service over them. It needs no database and
// no object store service.
type MemEnv struct {
	T         testing.TB
	DB        *memdb.Store
	Store     *MemStore
	Objects   *MemObjects
	Authority *Authority
	Reader    *Reader
	Registry  *Registry
	Clock     *Clock
	Service   *artifacts.Service
}

// NewMemEnv builds the module over a fresh memdb kernel. The transfer endpoint
// stops when the test ends.
func NewMemEnv(t testing.TB, opts ...memdb.Option) *MemEnv {
	t.Helper()
	e := &MemEnv{T: t, DB: memdb.New(opts...), Authority: NewAuthority(), Reader: NewReader(), Registry: NewRegistry(), Clock: &Clock{}}
	var err error
	if e.Store, err = NewMemStore(e.DB); err != nil {
		t.Fatal(err)
	}
	e.Objects = NewMemObjects(e.Clock.Now)
	if _, err := e.Objects.Serve(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Objects.Close() })
	e.Service, err = artifacts.NewService(artifacts.ServiceConfig{
		Repository: e.Store, Authority: e.Authority, Reader: e.Reader, Objects: e.Objects, Registry: e.Registry,
		Clock: e.Clock.Now, UploadTTL: 10 * time.Minute, DownloadTTL: 5 * time.Minute, ClaimLease: 30 * time.Second, SpoolDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// Attempt registers an active attempt with the declared outputs and returns its
// caller, as Env.Attempt does for PostgreSQL.
func (e *MemEnv) Attempt(scope domain.Scope, attempt domain.AttemptID, outputs ...artifacts.OutputDecl) artifacts.Caller {
	e.T.Helper()
	g := artifacts.AttemptGrant{
		Scope: scope, Attempt: attempt, Run: "run_1", Step: "build", ExpiresAt: e.Clock.Now().Add(time.Hour), Writer: true, Source: Source,
		Outputs: outputs, Limits: artifacts.Limits{OutputBytes: 1 << 20, OutputFiles: 8, DiagnosticBytes: 1 << 20},
	}
	e.Authority.Set(Caller(attempt).Subject, g)
	return Caller(attempt)
}
