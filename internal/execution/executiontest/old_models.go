package executiontest

import (
	_ "embed"
	"testing"

	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// OldContextsGolden is the captured pre-T-148 output of OldModelContexts.
//
//go:embed testdata/old_contexts.json
var OldContextsGolden []byte

// OldContext is the pinned attempt context of an earlier run model with the
// digest and workload fingerprint derived from it: the bytes a runtime receives.
type OldContext struct {
	Context     string `json:"context"`
	Digest      string `json:"digest"`
	Fingerprint string `json:"fingerprint"`
}

// OldModelContexts builds, over the memory fixtures with controlled time and
// deterministic identities, one attempt of each earlier run model and returns its
// pinned context bytes: the attempt_private model (runtime context v1) with a
// reader and a writer step, and the run_shared model (runtime context v2). The
// golden file testdata/old_contexts.json was captured from the release before
// run origins and source sets existed (T-148); the contexts of those models must
// stay byte-identical.
func OldModelContexts(t *testing.T) map[string]OldContext {
	t.Helper()
	out := map[string]OldContext{}
	take := func(name string, ctxBytes []byte, digest, fingerprint string) {
		out[name] = OldContext{Context: string(ctxBytes), Digest: digest, Fingerprint: fingerprint}
	}
	for _, c := range []struct{ name, access string }{
		{"attempt_private_read", "workspace: {access: read, from: run}"},
		{"attempt_private_write", "workspace: {access: write, from: run}"},
	} {
		m := NewMem(t)
		m.GrantAll()
		p := m.NewProject(ScopeA)
		def := m.Define(ScopeA, pipelinetest.Doc(pipelinetest.Container("a", c.access)))
		m.MustAdmit(p, def, nil)
		in := m.MustClaim(ScopeA.TenantID)
		take(c.name, in.Context, in.ContextDigest, in.SpecFingerprint)
	}
	_, s := NewSharedMem(t)
	run := NewSharedRun(t, s, SharedSpecDoc(true, SharedStep("job", "workspace: {access: read}")))
	run.Up()
	in, err := run.Claim()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	take("run_shared", in.Context, in.ContextDigest, in.SpecFingerprint)
	return out
}
