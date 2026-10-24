package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/execution/executiontest"
)

// TestUnknownWrapper verifies AR-02 / AR-A01 against real PostgreSQL: a profile
// whose image/wrapper/harness tuple, wrapper command or claimed control has no
// compatibility evidence fails registration and writes neither a profile nor an
// audit row; a qualified fixture tuple registers. The fixture catalogue is test
// evidence only: real OpenCode support additionally needs T-031's smoke result.
func TestUnknownWrapper(t *testing.T) {
	scope := executiontest.ScopeA
	counts := func(e *env) (profiles, audits int) {
		return e.Count(`SELECT count(*) FROM agent_profiles`), e.Count(`SELECT count(*) FROM audit WHERE resource_type = 'agent_profile'`)
	}

	e := newEnv(t)
	reject := func(name string, mutate func(*agents.Draft)) {
		t.Run(name, func(t *testing.T) {
			d := e.Draft
			mutate(&d)
			_, err := e.registerAs(executiontest.Engineer, scope, d)
			if !errors.Is(err, agents.ErrUnsupported) {
				t.Fatalf("expected an unsupported combination, got %v", err)
			}
			if p, a := counts(e); p != 0 || a != 0 {
				t.Fatalf("a rejected registration wrote %d profiles and %d audit rows", p, a)
			}
		})
	}
	reject("an unknown wrapper version", func(d *agents.Draft) { d.Wrapper.Version = "9.9.9" })
	reject("an unknown wrapper", func(d *agents.Draft) { d.Wrapper.ID = "unknown-wrapper" })
	reject("an unknown harness version", func(d *agents.Draft) { d.Wrapper.Harness.Version = "0.0.1" })
	reject("a mismatched image digest", func(d *agents.Draft) {
		d.Image = "registry.example.invalid/t030-agent@sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
	})
	reject("a different wrapper entrypoint", func(d *agents.Draft) { d.Wrapper.Command = []string{"/bin/sh", "-c", "opencode"} })

	t.Run("missing compatibility evidence", func(t *testing.T) {
		empty := newEnv(t, envOptions{Catalogue: agentstest.Catalogue(t)})
		if _, err := empty.registerAs(executiontest.Engineer, scope, empty.Draft); !errors.Is(err, agents.ErrUnsupported) {
			t.Fatalf("an empty catalogue must reject registration: %v", err)
		}
		if empty.Count(`SELECT count(*) FROM agent_profiles`) != 0 || empty.Count(`SELECT count(*) FROM audit WHERE resource_type = 'agent_profile'`) != 0 {
			t.Fatal("nothing may be written")
		}
		none := newEnv(t, envOptions{Catalogue: nilCompat{}})
		if _, err := none.registerAs(executiontest.Engineer, scope, none.Draft); !errors.Is(err, agents.ErrUnavailable) {
			t.Fatalf("an unavailable catalogue must fail closed: %v", err)
		}
	})

	t.Run("an unsupported claimed control", func(t *testing.T) {
		entry := agentstest.Entry(e.Draft)
		entry.Controls = []agents.Control{agents.ControlModel, agents.ControlToolAllowlist, agents.ControlTranscript} // no requests, tokens or tool calls
		cat, err := agents.NewCatalogue(entry)
		if err != nil {
			t.Fatal(err)
		}
		g := newEnv(t, envOptions{Catalogue: cat})
		if _, err := g.registerAs(executiontest.Engineer, scope, g.Draft); !errors.Is(err, agents.ErrUnsupported) {
			t.Fatalf("a claimed limit nothing enforces must fail registration: %v", err)
		}
		if p, a := counts(g); p != 0 || a != 0 {
			t.Fatalf("wrote %d profiles and %d audit rows", p, a)
		}
		narrow := g.Draft
		narrow.Limits.MaxRequests, narrow.Limits.MaxTokens, narrow.Limits.MaxToolCalls = nil, nil, nil
		if _, err := g.registerAs(executiontest.Engineer, scope, narrow); err != nil {
			t.Fatalf("a profile claiming only what is enforced registers: %v", err)
		}
	})

	t.Run("a qualified tuple registers once, attributably", func(t *testing.T) {
		p := e.register(scope, e.Draft)
		if p.Version != 1 || p.Ref() != "coding@1" || p.CreatedBy.ID != executiontest.Engineer || p.CreatedBy.Type != agents.ActorUser {
			t.Fatalf("profile: %+v", p)
		}
		if pr, a := counts(e); pr != 1 || a != 1 {
			t.Fatalf("expected one profile and one audit row, got %d and %d", pr, a)
		}
		again := e.register(scope, e.Draft)
		if again.ID != p.ID {
			t.Fatal("identical content must converge")
		}
		if pr, a := counts(e); pr != 1 || a != 1 {
			t.Fatalf("a duplicate wrote rows: %d %d", pr, a)
		}
	})

	t.Run("registration follows the identity authority", func(t *testing.T) {
		if _, err := e.registerAs(executiontest.Viewer, scope, e.variant(1)); !errors.Is(err, agents.ErrDenied) {
			t.Fatalf("a viewer may read but not register: %v", err)
		}
		if _, err := e.registerAs(executiontest.Stranger, scope, e.variant(1)); !errors.Is(err, agents.ErrHidden) {
			t.Fatalf("a stranger learns nothing: %v", err)
		}
		if _, err := e.registerAs(executiontest.OtherEng, scope, e.variant(1)); !errors.Is(err, agents.ErrHidden) {
			t.Fatalf("an engineer of another tenant learns nothing: %v", err)
		}
		if pr, _ := counts(e); pr != 1 {
			t.Fatalf("refused callers wrote %d profiles", pr)
		}
	})
}

type nilCompat struct{}

func (nilCompat) Qualify(context.Context, agents.Tuple) (agents.Qualification, error) {
	return agents.Qualification{}, errors.New("catalogue backend down")
}
