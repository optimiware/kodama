package rtest

import (
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/jwt"
)

// MemStack is the runtime-authority service over memory: the memory grant store
// and attempt authority on one memdb kernel, the controlled Job observer, an
// ephemeral signing key ring and the real token verifier. It embeds a Stack
// whose PostgreSQL parts are unset, so the scenario helpers that only need the
// service (HappyContext, Register, AdmitRequest, Workload) work unchanged. The
// embedded Stack's Owner, Runtime and Authority must not be used.
type MemStack struct {
	*Stack
	Env *MemEnv
}

// NewMemStack builds the service. Faults, the clock and the policy are the
// ones the PostgreSQL fixture uses.
func NewMemStack(t testing.TB, opts ...memdb.Option) *MemStack {
	t.Helper()
	m := &MemStack{Env: NewMemEnv(t, opts...)}
	s := &Stack{T: t, Clock: NewClock(), Faults: &Faults{}, Logs: &SafeBuffer{}, Jobs: &Jobs{}}
	key, err := jwt.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if s.Ring, err = jwt.NewKeyRing(1, key); err != nil {
		t.Fatal(err)
	}
	if s.Signer, err = jwt.NewSigner(s.Ring, Issuer); err != nil {
		t.Fatal(err)
	}
	if s.Verifier, err = jwt.NewVerifier(jwt.VerifierConfig{Ring: s.Ring, Issuer: Issuer, MaxLifetime: TestPolicy.MaxSession, Clock: s.Clock.Now}); err != nil {
		t.Fatal(err)
	}
	s.Service = s.NewService(TestPolicy, m.Env.Grants, m.Env.Attempts, s.Jobs, s.Signer)
	m.Stack = s
	return m
}

// SeedAttempt creates an active attempt with the given pinned context in the
// memory authority and installs the cluster state of its Job.
func (m *MemStack) SeedAttempt(c runtimev1.Context, actions runtimeauth.ActionSet) Attempt {
	m.T.Helper()
	scope := c.Scope()
	digest, _, err := runtimeauth.PinContext(c)
	if err != nil {
		m.T.Fatal(err)
	}
	job := runtimeauth.ExpectedJob{Namespace: Namespace, Name: JobName(c.AttemptID)}
	m.Env.SeedAttempt(m.T, scope, c.RunID, c.AttemptID, job, digest, actions)
	return Attempt{Scope: scope, Context: c, Job: job, Actions: actions}
}
