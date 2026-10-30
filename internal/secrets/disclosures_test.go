package secrets_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/workspace"
	"github.com/optimiware/kodama/internal/workspace/workspacetest"
)

type mapSource struct {
	values map[string]string
	err    map[string]error
	opens  int
	scans  int
}

// openOnly hides the scan capability of a source.
type openOnly struct{ secrets.VersionSource }

// OpenForScan reads for comparison only: a revoked version is still read.
func (m *mapSource) OpenForScan(ctx context.Context, req secrets.VersionRequest) (secrets.PendingValue, error) {
	m.scans++
	if errors.Is(m.err[req.SecretName], secrets.ErrRevoked) {
		return m.read(req)
	}
	return m.open(ctx, req)
}

func (m *mapSource) Open(ctx context.Context, req secrets.VersionRequest) (secrets.PendingValue, error) {
	return m.open(ctx, req)
}

func (m *mapSource) open(_ context.Context, req secrets.VersionRequest) (secrets.PendingValue, error) {
	m.opens++
	if err := m.err[req.SecretName]; err != nil {
		return nil, err
	}
	return m.read(req)
}

func (m *mapSource) read(req secrets.VersionRequest) (secrets.PendingValue, error) {
	v, ok := m.values[req.SecretName]
	if !ok {
		return nil, secrets.ErrNotFound
	}
	return &fakePending{src: &fakeSource{}, value: []byte(v)}, nil
}

type fixedBindings struct {
	run      domain.RunID
	bindings []runtimev1.SecretBinding
	err      error
	asked    domain.AttemptID
}

func (f *fixedBindings) PriorBindings(_ context.Context, _ domain.Scope, a domain.AttemptID) (domain.RunID, []runtimev1.SecretBinding, error) {
	f.asked = a
	return f.run, f.bindings, f.err
}

const (
	priorSecret  = "prior-step-secret-value-123"
	revokedValue = "revoked-value-12345"
)

// Test_unit_prior_disclosure_redaction: a value disclosed to an earlier step or a
// service of the run is refused in a later writer's source and removed from a
// later attempt's logs, although that attempt was never given it (VS-14, WS-16).
func Test_unit_prior_disclosure_redaction(t *testing.T) {
	ctx := context.Background()
	bindings := &fixedBindings{run: "run_1", bindings: []runtimev1.SecretBinding{
		{Ref: "secret://project/db-password", Version: 3, Name: "PGPASSWORD", Delivery: runtimev1.DeliveryEnv},
		{Ref: "secret://project/db-password", Version: 3, Name: "OTHERNAME", Delivery: runtimev1.DeliveryFile}, // the same pin twice
		{Ref: "secret://project/revoked", Version: 1, Name: "OLD", Delivery: runtimev1.DeliveryEnv},
	}}
	src := &mapSource{values: map[string]string{"db-password": priorSecret, "revoked": revokedValue}, err: map[string]error{"revoked": secrets.ErrRevoked}}
	d, err := secrets.NewDisclosures(bindings, src)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("the values come from the pinned versions, once each", func(t *testing.T) {
		got, err := d.Prior(ctx, scopeA, "att_2")
		if err != nil {
			t.Fatal(err)
		}
		// The revoked version is still a value an earlier step received.
		if len(got) != 2 || string(got[0]) != priorSecret || string(got[1]) != revokedValue {
			t.Fatalf("prior values %q", got)
		}
		if bindings.asked != "att_2" || src.scans != 2 {
			t.Fatalf("asked %s, scanned %d versions", bindings.asked, src.scans)
		}
	})

	t.Run("an attempt outside a run_shared run has none", func(t *testing.T) {
		none, _ := secrets.NewDisclosures(&fixedBindings{}, src)
		got, err := none.Prior(ctx, scopeA, "att_1")
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("%v %v", got, err)
		}
	})

	t.Run("a failure never narrows the set", func(t *testing.T) {
		broken, _ := secrets.NewDisclosures(&fixedBindings{err: errors.New("outage")}, src)
		if _, err := broken.Prior(ctx, scopeA, "att_2"); !errors.Is(err, secrets.ErrUnavailable) {
			t.Fatalf("bindings outage: %v", err)
		}
		flaky, _ := secrets.NewDisclosures(bindings, &mapSource{values: map[string]string{"db-password": priorSecret}, err: map[string]error{"revoked": errors.New("kms down")}})
		if _, err := flaky.Prior(ctx, scopeA, "att_2"); !errors.Is(err, secrets.ErrUnavailable) {
			t.Fatalf("a value that cannot be read: %v", err)
		}
		if _, err := secrets.NewDisclosures(nil, src); err == nil {
			t.Fatal("no bindings port was accepted")
		}
	})

	t.Run("a later writer cannot publish a prior step's secret", func(t *testing.T) {
		e := workspacetest.NewMemServiceEnv(t)
		w := e.Ready(workspacetest.ScopeA, "wi_1")
		e.Writer(w, "att_2")
		files := workspacetest.File{Path: "dump.txt", Content: "PGPASSWORD=" + priorSecret}

		// Without the run's prior disclosures the writer's own (empty) set misses it.
		in := e.Proposal(w, "att_2", e.Repo.Initial, "leak", files)
		decorated, err := workspace.WithPriorDisclosures(e.Secrets, d)
		if err != nil {
			t.Fatal(err)
		}
		needles, err := decorated.Needles(ctx, w.Scope, "att_2")
		if err != nil || len(needles) != 2 || string(needles[0]) != priorSecret {
			t.Fatalf("needles %q %v", needles, err)
		}
		svc, err := workspace.NewService(workspace.Dependencies{
			Store: e.Store, Tenants: e.Tenants, Authorizer: e.Authz, WorkItems: e.Items, Repositories: e.Repos, Attempts: e.Attempts,
			Bundles: e.Bundles, Secrets: decorated, Refs: e.Git.Reader(), Source: e.Git.Preparer(), Publisher: e.Git.Publisher(), Clock: e.Clock.Now,
		}, workspacetest.DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ProposeSource(ctx, in); !errors.Is(err, workspace.ErrSecretInSource) {
			t.Fatalf("the prior step's value reached the source: %v", err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != e.Repo.Initial {
			t.Fatalf("a refused proposal moved the branch to %q", tip)
		}
		// Benign work from the same attempt publishes.
		ok := e.Proposal(w, "att_2", e.Repo.Initial, "fine", workspacetest.File{Path: "a.txt", Content: "a"})
		if res, err := svc.ProposeSource(ctx, ok); err != nil || res.Status != workspace.ProposeAccepted {
			t.Fatalf("benign proposal: %+v %v", res, err)
		}
	})

	t.Run("both halves are required and a failure fails closed", func(t *testing.T) {
		own := workspacetest.NewSecrets()
		if _, err := workspace.WithPriorDisclosures(nil, d); !errors.Is(err, workspace.ErrNotConfigured) {
			t.Fatal(err)
		}
		if _, err := workspace.WithPriorDisclosures(own, nil); !errors.Is(err, workspace.ErrNotConfigured) {
			t.Fatal(err)
		}
		broken, _ := secrets.NewDisclosures(&fixedBindings{err: errors.New("outage")}, src)
		n, _ := workspace.WithPriorDisclosures(own, broken)
		if _, err := n.Needles(ctx, scopeA, "att_2"); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("an outage of the disclosures: %v", err)
		}
	})

	t.Run("a later attempt's logs lose the prior step's secret", func(t *testing.T) {
		own := ownValues{"att_3": {[]byte("its-own-injected-value")}}
		red, err := logs.WithPriorDisclosures(own, d)
		if err != nil {
			t.Fatal(err)
		}
		vals, err := red.Values(ctx, scopeA, "att_3")
		if err != nil || len(vals) != 3 { // its own, the prior step's and the revoked one
			t.Fatalf("values %q %v", vals, err)
		}
		r, err := logs.NewRedactor(vals)
		if err != nil {
			t.Fatal(err)
		}
		line := "connect: PGPASSWORD=" + priorSecret + " token=its-own-injected-value\n"
		var out []byte
		// A value split across writes is redacted too.
		half := len(line) / 2
		out = append(out, r.Write([]byte(line[:half]))...)
		out = append(out, r.Write([]byte(line[half:]))...)
		out = append(out, r.Flush()...)
		if bytes.Contains(out, []byte(priorSecret)) || bytes.Contains(out, []byte("its-own-injected-value")) || !bytes.Contains(out, []byte(logs.Marker)) {
			t.Fatalf("redacted output %q", out)
		}
		if _, err := logs.WithPriorDisclosures(nil, d); !errors.Is(err, logs.ErrNotConfigured) {
			t.Fatal(err)
		}
		broken, _ := secrets.NewDisclosures(&fixedBindings{err: errors.New("outage")}, src)
		fail, _ := logs.WithPriorDisclosures(own, broken)
		if _, err := fail.Values(ctx, scopeA, "att_3"); !errors.Is(err, logs.ErrUnavailable) {
			t.Fatalf("an outage of the disclosures must fail the read closed: %v", err)
		}
	})
}

type ownValues map[domain.AttemptID][][]byte

func (o ownValues) Values(_ context.Context, _ domain.Scope, a domain.AttemptID) ([][]byte, error) {
	return o[a], nil
}

// Review finding 6: revocation ends USE of a secret, not the duty to keep a value
// an earlier step already received out of source and logs. Disclosure ->
// revocation -> later publication / log read must still scan and redact it, and
// when the value cannot be obtained the publication and the read are refused.
func Test_unit_prior_disclosure_survives_revocation(t *testing.T) {
	ctx := context.Background()
	bindings := &fixedBindings{run: "run_1", bindings: []runtimev1.SecretBinding{
		{Ref: "secret://project/revoked", Version: 1, Name: "OLD", Delivery: runtimev1.DeliveryEnv},
	}}
	revoked := &mapSource{values: map[string]string{"revoked": revokedValue}, err: map[string]error{"revoked": secrets.ErrRevoked}}

	t.Run("the revoked value is still refused in source and removed from logs", func(t *testing.T) {
		d, _ := secrets.NewDisclosures(bindings, revoked)
		// The ordinary (use) path refuses the revoked version: revocation works.
		if _, err := revoked.Open(ctx, secrets.VersionRequest{Scope: scopeA, RunID: "run_1", SecretName: "revoked", Version: 1}); !errors.Is(err, secrets.ErrRevoked) {
			t.Fatalf("use of a revoked version: %v", err)
		}
		e := workspacetest.NewMemServiceEnv(t)
		w := e.Ready(workspacetest.ScopeA, "wi_1")
		e.Writer(w, "att_2")
		needles, _ := workspace.WithPriorDisclosures(e.Secrets, d)
		svc, err := workspace.NewService(workspace.Dependencies{
			Store: e.Store, Tenants: e.Tenants, Authorizer: e.Authz, WorkItems: e.Items, Repositories: e.Repos, Attempts: e.Attempts,
			Bundles: e.Bundles, Secrets: needles, Refs: e.Git.Reader(), Source: e.Git.Preparer(), Publisher: e.Git.Publisher(), Clock: e.Clock.Now,
		}, workspacetest.DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		in := e.Proposal(w, "att_2", e.Repo.Initial, "leak", workspacetest.File{Path: "env.txt", Content: "OLD=" + revokedValue})
		if _, err := svc.ProposeSource(ctx, in); !errors.Is(err, workspace.ErrSecretInSource) {
			t.Fatalf("a revoked, previously disclosed value reached the source: %v", err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != e.Repo.Initial {
			t.Fatalf("the branch moved to %q", tip)
		}
		red, _ := logs.WithPriorDisclosures(ownValues{}, d)
		vals, err := red.Values(ctx, scopeA, "att_3")
		if err != nil || len(vals) != 1 {
			t.Fatalf("values %q %v", vals, err)
		}
		r, _ := logs.NewRedactor(vals)
		out := append(r.Write([]byte("echo "+revokedValue+"\n")), r.Flush()...)
		if bytes.Contains(out, []byte(revokedValue)) {
			t.Fatalf("a revoked disclosed value reached the log: %q", out)
		}
	})

	t.Run("a source that cannot scan a revoked version fails the publication and the log read closed", func(t *testing.T) {
		d, _ := secrets.NewDisclosures(bindings, openOnly{revoked})
		if _, err := d.Prior(ctx, scopeA, "att_2"); !errors.Is(err, secrets.ErrUnavailable) {
			t.Fatalf("Prior: %v", err)
		}
		e := workspacetest.NewMemServiceEnv(t)
		w := e.Ready(workspacetest.ScopeA, "wi_1")
		e.Writer(w, "att_2")
		needles, _ := workspace.WithPriorDisclosures(e.Secrets, d)
		svc, err := workspace.NewService(workspace.Dependencies{
			Store: e.Store, Tenants: e.Tenants, Authorizer: e.Authz, WorkItems: e.Items, Repositories: e.Repos, Attempts: e.Attempts,
			Bundles: e.Bundles, Secrets: needles, Refs: e.Git.Reader(), Source: e.Git.Preparer(), Publisher: e.Git.Publisher(), Clock: e.Clock.Now,
		}, workspacetest.DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		// Even a harmless proposal is refused: no value, no verdict.
		in := e.Proposal(w, "att_2", e.Repo.Initial, "fine", workspacetest.File{Path: "a.txt", Content: "a"})
		if _, err := svc.ProposeSource(ctx, in); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("a publication without its scan set: %v", err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != e.Repo.Initial {
			t.Fatalf("the branch moved to %q", tip)
		}
		red, _ := logs.WithPriorDisclosures(ownValues{}, d)
		if _, err := red.Values(ctx, scopeA, "att_3"); !errors.Is(err, logs.ErrUnavailable) {
			t.Fatalf("a log read without its redaction set: %v", err)
		}
	})
}
