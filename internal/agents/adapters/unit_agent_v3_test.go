package adapters_test

// Context versions of agent attempts (T-153). The policy and metadata readers
// serve the gateway and tool proxy from the immutable context an attempt was
// admitted with; a run that pins a source set (runtime context v3, ADR 0011) is
// read by the same checks as a v1 run, never by looser ones. Nothing here is a
// provider, gateway or live-harness claim (T-085).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/adapters"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

var v3Scope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}

func fixtureCase(t *testing.T, path, name string) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Name == name {
			return c.Payload
		}
	}
	t.Fatalf("no fixture %q in %s", name, path)
	return nil
}

// frozenAgent expands the world's profile and returns its frozen command and the
// pins its payload implies: exactly what admission freezes into a context.
func frozenAgent(t *testing.T, w *world) (runtimev1.Command, runtimev1.Profile, runtimev1.DesignBaseline) {
	t.Helper()
	p := w.register(t, w.draft)
	r, err := adapters.NewResolver(w.svc)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := r.ResolveAgent(context.Background(), scope, agentStep("generate", p.Ref(), 600))
	if err != nil {
		t.Fatal(err)
	}
	cfg, present, err := agents.DecodeArgs(exp.Args)
	if err != nil || !present {
		t.Fatalf("payload: %v %v", present, err)
	}
	profile, baseline := cfg.Pins()
	return runtimev1.Command{Image: exp.Image, Command: exp.Command, Args: exp.Args, Env: map[string]string{}}, profile, baseline
}

// v3Attempt is an attempt of the writer fixture carrying the frozen agent command and pins.
func v3Attempt(t *testing.T, fixture string, cmd runtimev1.Command, profile runtimev1.Profile, baseline runtimev1.DesignBaseline, mutate func(*runtimev3.ContextV3)) execution.Attempt {
	t.Helper()
	c, err := runtimev3.DecodeRequest[runtimev3.ContextV3](fixtureCase(t, "../../../contracts/runtime/v3/fixtures/cases.json", fixture))
	if err != nil {
		t.Fatal(err)
	}
	c.Command, c.Profile, c.DesignBaseline = cmd, &profile, &baseline
	if mutate != nil {
		mutate(&c)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return execution.Attempt{Scope: c.Scope(), ID: c.AttemptID, RunID: c.RunID, StepID: c.StepID, Context: raw}
}

type oneAttempt execution.Attempt

func (o oneAttempt) Attempt(_ context.Context, sc domain.Scope, run domain.RunID, step string, id domain.AttemptID) (execution.Attempt, error) {
	if o.Scope != sc || o.RunID != run || o.StepID != step || o.ID != id {
		return execution.Attempt{}, execution.ErrNotFound
	}
	return execution.Attempt(o), nil
}

// Test_unit_agent_policy_new_context_version: the agent policy accepts runtime
// context version 3 (a source set, ADR 0011) with every check version 1 has, and
// does not weaken them (AR-01, AR-05, AR-14, AR-15).
func Test_unit_agent_policy_new_context_version(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)

	// The version 1 reading is the reference: same profile, same policy.
	u := admit(t, w, w.register(t, w.draft))
	v1Reader, err := adapters.NewPolicyReader(memSource{u.agg})
	if err != nil {
		t.Fatal(err)
	}
	want, err := v1Reader.PolicyOf(ctx, scope, u.agg.Run.ID, "implement", u.at.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m := u.metadata(t); m.ContextVersion != 1 || m.SourceSetDigest != "" || m.Checkouts != nil {
		t.Fatalf("a v1 attempt has no source set: %+v", m)
	}

	cmd, profile, baseline := frozenAgent(t, w)
	read := func(at execution.Attempt) (agents.Policy, error) {
		r, err := adapters.NewPolicyReader(oneAttempt(at))
		if err != nil {
			t.Fatal(err)
		}
		return r.PolicyOf(ctx, at.Scope, at.RunID, at.StepID, at.ID)
	}
	meta := func(at execution.Attempt) (adapters.AttemptMetadata, error) {
		r, err := adapters.NewMetadataReader(oneAttempt(at))
		if err != nil {
			t.Fatal(err)
		}
		return r.MetadataOf(ctx, at.Scope, at.RunID, at.StepID, at.ID)
	}

	t.Run("a source-set attempt is served the policy it was admitted with", func(t *testing.T) {
		at := v3Attempt(t, "context_v3_work_item_writer_two_repositories", cmd, profile, baseline, nil)
		got, err := read(at)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("the policy of a v3 attempt differs from the v1 one for the same profile:\n%+v\n%+v", got, want)
		}
		m, err := meta(at)
		if err != nil {
			t.Fatal(err)
		}
		if m.ContextVersion != 3 || m.Profile != u.metadata(t).Profile || m.DesignBaselineDigest != baseline.Digest || m.WrapperVersion == "" {
			t.Fatalf("attribution: %+v", m)
		}
		if !strings.HasPrefix(string(m.SourceSetDigest), "sha256:") || len(m.Checkouts) != 2 ||
			m.Checkouts[0].Mode != agents.ModeHead || m.Checkouts[1].Mode != agents.ModeAgentBranch {
			t.Fatalf("source set attribution: %+v", m)
		}
	})

	t.Run("a reader and a run without a work item are served the same policy", func(t *testing.T) {
		for _, fixture := range []string{"context_v3_work_item_reader_first_attempt", "context_v3_push_origin_reader_root_source", "context_v3_push_origin_writer_run_branch"} {
			got, err := read(v3Attempt(t, fixture, cmd, profile, baseline, nil))
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("%s: %+v %v", fixture, got, err)
			}
		}
	})

	t.Run("a version 2 attempt is read by the same checks", func(t *testing.T) {
		c, err := runtimev2.DecodeRequest[runtimev2.ContextV2](fixtureCase(t, "../../../contracts/runtime/v2/fixtures/cases.json", "context_v2_reader_first_attempt"))
		if err != nil {
			t.Fatal(err)
		}
		c.Command, c.Profile, c.DesignBaseline = cmd, &profile, &baseline
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		at := execution.Attempt{Scope: domain.Scope{TenantID: c.TenantID, ProjectID: c.ProjectID}, ID: c.AttemptID, RunID: c.RunID, StepID: c.StepID, Context: raw}
		got, err := read(at)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%+v %v", got, err)
		}
		if m, err := meta(at); err != nil || m.ContextVersion != 2 || m.Checkouts != nil {
			t.Fatalf("%+v %v", m, err)
		}
	})

	t.Run("nothing of version 1 or 3 is weaker: every identity and pin check refuses", func(t *testing.T) {
		good := v3Attempt(t, "context_v3_work_item_writer_two_repositories", cmd, profile, baseline, nil)
		for name, c := range map[string]struct {
			at   execution.Attempt
			want error
		}{
			"another scope":   {at: relocate(good, func(a *execution.Attempt) { a.Scope.ProjectID = "p_other" }), want: agents.ErrNotFound},
			"another run":     {at: relocate(good, func(a *execution.Attempt) { a.RunID = "run_other" }), want: agents.ErrNotFound},
			"another step":    {at: relocate(good, func(a *execution.Attempt) { a.StepID = "other" }), want: agents.ErrNotFound},
			"another attempt": {at: relocate(good, func(a *execution.Attempt) { a.ID = "att_other" }), want: agents.ErrNotFound},
			"an ordinary command": {
				at:   v3Attempt(t, "context_v3_work_item_writer_two_repositories", runtimev1.Command{Image: cmd.Image, Command: []string{"/bin/sh"}, Args: []string{"-c", "true"}, Env: map[string]string{}}, profile, baseline, nil),
				want: agents.ErrNotFound,
			},
			"no profile pin": {
				at:   v3Attempt(t, "context_v3_work_item_writer_two_repositories", cmd, profile, baseline, func(c *runtimev3.ContextV3) { c.Profile = nil }),
				want: agents.ErrUnavailable,
			},
			"no design baseline pin": {
				at:   v3Attempt(t, "context_v3_work_item_writer_two_repositories", cmd, profile, baseline, func(c *runtimev3.ContextV3) { c.DesignBaseline = nil }),
				want: agents.ErrUnavailable,
			},
			"another profile pin": {
				at:   v3Attempt(t, "context_v3_work_item_writer_two_repositories", cmd, runtimev1.Profile{Name: profile.Name, Version: profile.Version + 1, WrapperVersion: profile.WrapperVersion}, baseline, nil),
				want: agents.ErrUnavailable,
			},
			"a payload that does not match the image": {
				at: v3Attempt(t, "context_v3_work_item_writer_two_repositories", cmd, profile, baseline, func(c *runtimev3.ContextV3) {
					c.Command.Image = strings.Replace(c.Command.Image, "sha256:", "sha256:0", 1)[:len(c.Command.Image)]
				}),
				want: agents.ErrUnavailable,
			},
			"a context of a version nobody reads": {at: withContext(good, `{"contextVersion":4}`), want: agents.ErrUnavailable},
			"a version member on a v1 body": {
				at:   withContext(good, string(mustMarshal(t, mutateJSON(t, good.Context, func(m map[string]any) { m["contextVersion"] = 1 })))),
				want: agents.ErrUnavailable,
			},
			"a v3 body without its version": {
				at:   withContext(good, string(mustMarshal(t, mutateJSON(t, good.Context, func(m map[string]any) { delete(m, "contextVersion") })))),
				want: agents.ErrUnavailable,
			},
			"an invalid source set": {
				at: withContext(good, string(mustMarshal(t, mutateJSON(t, good.Context, func(m map[string]any) {
					m["workspace"].(map[string]any)["sources"].([]any)[0].(map[string]any)["path"] = "../escape"
				})))),
				want: agents.ErrUnavailable,
			},
			"a grant that widens the agent namespace": {
				at: withContext(good, string(mustMarshal(t, mutateJSON(t, good.Context, func(m map[string]any) {
					m["git"].(map[string]any)["repositories"].([]any)[1].(map[string]any)["update"] = []any{"refs/heads/*"}
				})))),
				want: agents.ErrUnavailable,
			},
			"garbage": {at: withContext(good, `not json`), want: agents.ErrUnavailable},
		} {
			_, perr := read(c.at)
			_, merr := meta(c.at)
			// A relocated attempt is read under the relocated identity, so the context
			// belongs to someone else: the readers answer as if it did not exist.
			if !errors.Is(perr, c.want) || !errors.Is(merr, c.want) {
				t.Errorf("%s: policy %v metadata %v, want %v", name, perr, merr, c.want)
			}
		}
	})

	t.Run("an agent attempt of a source-free v3 context has a policy but no attributable candidate", func(t *testing.T) {
		at := v3Attempt(t, "context_v3_cleanup_run_manual_origin_source_free_without_git", cmd, profile, baseline, nil)
		if _, err := read(at); err != nil {
			t.Fatalf("the policy does not depend on the workspace: %v", err)
		}
		if _, err := meta(at); !errors.Is(err, agents.ErrUnavailable) {
			t.Fatalf("a source-free agent cannot be planned a candidate: %v", err)
		}
	})
}

func relocate(a execution.Attempt, f func(*execution.Attempt)) execution.Attempt {
	cp := a
	f(&cp)
	return cp
}

func withContext(a execution.Attempt, raw string) execution.Attempt {
	cp := a
	cp.Context = []byte(raw)
	return cp
}

func mutateJSON(t *testing.T, raw []byte, f func(map[string]any)) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	return m
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Test_unit_agent_harness_contract_additive: the harness contract directory grew
// source-set cases next to the original evidence, which stays byte-identical, and
// every source-set case holds against the real planner.
func Test_unit_agent_harness_contract_additive(t *testing.T) {
	dir := "../../../contracts/harness-opencode/"
	t.Run("the original evidence is unchanged", func(t *testing.T) {
		var golden struct {
			Files map[string]string `json:"files"`
		}
		raw, err := os.ReadFile(dir + "golden.json")
		if err != nil || json.Unmarshal(raw, &golden) != nil || len(golden.Files) != 2 {
			t.Fatalf("golden: %v", err)
		}
		for name, want := range golden.Files {
			b, err := os.ReadFile(dir + name)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(b)
			if got := "sha256:" + hex.EncodeToString(sum[:]); got != want {
				t.Errorf("%s changed: %s, golden %s", name, got, want)
			}
		}
	})

	t.Run("every source-set case holds", func(t *testing.T) {
		var doc struct {
			Version int `json:"version"`
			Cases   []struct {
				ID      string `json:"id"`
				Context struct {
					Path     string `json:"path"`
					Selector string `json:"selector"`
				} `json:"context"`
				Mutation *string `json:"mutation"`
				Expected struct {
					Plan      string `json:"plan"`
					Checkouts []struct {
						Path         string `json:"path"`
						RepositoryID string `json:"repositoryId"`
						Mode         string `json:"mode"`
						Ref          string `json:"ref"`
					} `json:"checkouts"`
				} `json:"expected"`
			} `json:"cases"`
		}
		raw, err := os.ReadFile(dir + "source-sets.json")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &doc); err != nil || doc.Version != 1 || len(doc.Cases) < 4 {
			t.Fatalf("source-sets.json: %v", err)
		}
		for _, c := range doc.Cases {
			name, ok := strings.CutPrefix(c.Context.Selector, "name=")
			if !ok || !strings.HasPrefix(c.Context.Path, "contracts/") {
				t.Fatalf("%s: selector %q path %q", c.ID, c.Context.Selector, c.Context.Path)
			}
			ctxv3, err := runtimev3.DecodeRequest[runtimev3.ContextV3](fixtureCase(t, "../../../"+c.Context.Path, name))
			if err != nil {
				t.Fatalf("%s: %v", c.ID, err)
			}
			if c.Mutation != nil {
				if *c.Mutation != "narrow-grant-of-last-repository" {
					t.Fatalf("%s: unknown mutation %q", c.ID, *c.Mutation)
				}
				ctxv3.Git.Repositories[len(ctxv3.Git.Repositories)-1].Update = []string{"refs/heads/kodama/agents/wi_1042/run_01/other"}
			}
			plan, err := agents.PlanCheckouts(ctxv3)
			if c.Expected.Plan == "refused" {
				if err == nil {
					t.Errorf("%s: planned", c.ID)
				}
				continue
			}
			if err != nil || len(plan) != len(c.Expected.Checkouts) {
				t.Fatalf("%s: %v %+v", c.ID, err, plan)
			}
			for i, want := range c.Expected.Checkouts {
				got := plan[i]
				if got.Path != want.Path || got.RepositoryID != want.RepositoryID || string(got.Mode) != want.Mode || got.Ref != want.Ref {
					t.Errorf("%s: checkout %d is %+v, want %+v", c.ID, i, got, want)
				}
			}
		}
	})
}
