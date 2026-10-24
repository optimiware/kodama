package memory_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest/gatescontract"
	"github.com/optimiware/kodama/internal/gates/gatestest/memory"
)

const (
	otherCommit = "2222222222222222222222222222222222222222"
	reason      = "Reviewed the identified source and test evidence."
)

var bg = context.Background()

func stringValue(s string) runtimev1.Value { return runtimev1.StringValue(s) }

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func caller(p string) gates.Caller { return gates.Caller{PrincipalID: p, RequestID: "req_unit"} }

func request(t *testing.T, h *memory.Harness, c gatescontract.Case) *gates.Gate {
	t.Helper()
	g, created, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step)
	if err != nil || !created {
		t.Fatalf("request: %v created=%v", err, created)
	}
	return g
}

func cmdFor(g *gates.Gate, d gates.Decision) gates.DecideCommand {
	return gates.DecideCommand{Decision: d, Reason: reason, ExpectedVersion: g.Version, SubjectDigest: g.SubjectDigest()}
}

func decide(h *memory.Harness, who string, g *gates.Gate, d gates.Decision) (*gates.Gate, error) {
	return h.E.Svc.Decide(bg, caller(who), g.Scope, g.ID, cmdFor(g, d))
}

func current(t *testing.T, h *memory.Harness, g *gates.Gate) *gates.Gate {
	t.Helper()
	cur, err := h.E.Store.Get(bg, g.Scope, g.ID)
	must(t, err)
	return cur
}

func process(t *testing.T, h *memory.Harness, g *gates.Gate) gates.Report {
	t.Helper()
	rep, err := h.E.Svc.ProcessGate(bg, g.Scope, g.ID)
	must(t, err)
	return rep
}

func wantStale(t *testing.T, err error, reason string, definitive bool) *gates.StaleError {
	t.Helper()
	var se *gates.StaleError
	if !errors.As(err, &se) || !errors.Is(err, gates.ErrStaleSubject) || se.Reason != reason || se.Definitive != definitive {
		t.Fatalf("want stale %q (definitive=%v), got %v", reason, definitive, err)
	}
	return se
}

func noGate(t *testing.T, h *memory.Harness, c gatescontract.Case) {
	t.Helper()
	if _, err := h.E.Store.ByStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrNotFound) {
		t.Fatalf("a gate exists for unverifiable facts: %v", err)
	}
	if n, err := h.E.PinCount(bg, c.Scope, gates.GateIDFor(c.Scope, c.Run, c.Step)); err != nil || n != 0 {
		t.Fatalf("%d evidence pins for a gate that was never created: %v", n, err)
	}
}

// TestUnitGateSubject (DQ-03, DQ-04, DQ-14, PX-13): a gate is one decision about
// one verified subject. A changed source, specification, definition, evidence
// or configuration makes the old subject inapplicable at the decision and at
// consumption; an unverifiable subject never becomes a gate; a revoked or
// demoted reviewer neither decides nor has an approval consumed.
func TestUnitGateSubject(t *testing.T) {
	t.Run("a changed subject refuses the decision and is recorded once", func(t *testing.T) {
		cases := []struct {
			name       string
			deployment bool
			change     func(*memory.Harness, gatescontract.Case)
			reason     string
			check      func(*testing.T, *gates.Gate)
		}{
			{"source", false, func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.EditRun(bg, c.Scope, c.Run, func(a *execution.Aggregate) { a.Run.SourceCommit = otherCommit }))
			}, gates.ReasonCandidateChanged, func(t *testing.T, g *gates.Gate) {
				if g.Invalidation.SupersedingCommit != otherCommit {
					t.Fatalf("the superseding commit was not recorded: %+v", g.Invalidation)
				}
			}},
			{"specification", false, func(h *memory.Harness, c gatescontract.Case) { h.ChangeContent(t, c) }, gates.ReasonContentChanged,
				func(t *testing.T, g *gates.Gate) {
					if g.Invalidation.SupersedingRevision != 2 {
						t.Fatalf("the superseding revision was not recorded: %+v", g.Invalidation)
					}
				}},
			{"definition", false, func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.EditRun(bg, c.Scope, c.Run, func(a *execution.Aggregate) { a.Run.DefinitionID = "pd_other" }))
			}, gates.ReasonDefinitionChanged, nil},
			{"evidence removed", false, func(h *memory.Harness, c gatescontract.Case) { h.ChangeEvidence(t, c) }, gates.ReasonEvidenceChanged, nil},
			{"evidence content replaced", false, func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.ReplaceArtifact(bg, c.Scope, c.Evidence, []byte("a different report\n")))
			}, gates.ReasonEvidenceChanged, nil},
			{"evidence reference re-resolved", false, func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.EditRun(bg, c.Scope, c.Run, func(a *execution.Aggregate) {
					a.Step("test").Outputs["report"] = stringValue("art_somewhere_else")
				}))
			}, gates.ReasonEvidenceChanged, nil},
			{"configuration digest", true, func(h *memory.Harness, c gatescontract.Case) { h.ChangeDeployment(t, c) }, gates.ReasonDeploymentChanged, nil},
			{"deployment removed", true, func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.Deployments.Remove(bg, c.Scope.TenantID, c.Deployment))
			}, gates.ReasonDeploymentChanged, nil},
			{"deployment of another project", true, func(h *memory.Harness, c gatescontract.Case) {
				foreign := domain.Scope{TenantID: c.Scope.TenantID, ProjectID: "p_foreign"}
				must(t, h.E.Deployments.Set(bg, foreign, c.Deployment, memory.Commit, digest("config-1")))
			}, gates.ReasonDeploymentChanged, nil},
			{"deployment recording another candidate", true, func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.Deployments.Set(bg, c.Scope, c.Deployment, otherCommit, digest("config-1")))
			}, gates.ReasonDeploymentChanged, nil},
		}
		for _, tc := range cases {
			t.Run("at the decision: "+tc.name, func(t *testing.T) {
				h := memory.NewHarness(t)
				c := h.NewCase(t, tc.deployment)
				g := request(t, h, c)
				tc.change(h, c)
				_, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
				wantStale(t, err, tc.reason, true)
				cur := current(t, h, g)
				if cur.Status != gates.StatusPending || cur.Decision != nil || cur.Invalidation == nil || cur.Invalidation.Reason != tc.reason {
					t.Fatalf("the refusal must record the staleness and no decision: %+v", cur)
				}
				if tc.check != nil {
					tc.check(t, cur)
				}
				// A rejection of a stale subject is refused too: there is nothing
				// current left to decide about.
				if _, err := decide(h, memory.Reviewer, cur, gates.DecisionReject); !errors.Is(err, gates.ErrStaleSubject) {
					t.Fatalf("rejecting an invalidated gate: %v", err)
				}
				if ev := h.Events(t, c, g.ID); ev != (gatescontract.Events{Requested: 1, Invalidated: 1}) {
					t.Fatalf("events %+v: the staleness must be recorded exactly once", ev)
				}
			})
			t.Run("at consumption: "+tc.name, func(t *testing.T) {
				h := memory.NewHarness(t)
				c := h.NewCase(t, tc.deployment)
				g := request(t, h, c)
				approved, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
				must(t, err)
				tc.change(h, c)
				if rep := process(t, h, g); rep.Completed != 0 || rep.Invalidated != 1 {
					t.Fatalf("a stale approval: %+v", rep)
				}
				cur := current(t, h, g)
				if cur.Status != gates.StatusApproved || *cur.Decision != *approved.Decision || cur.Invalidation == nil || cur.Invalidation.Reason != tc.reason {
					t.Fatalf("the decision stays, the invalidation is separate: %+v", cur)
				}
				if h.StepStatus(t, c) != execution.StepPending || h.Attempts(t, c) != 0 {
					t.Fatalf("the stale approval moved the step")
				}
			})
		}
	})

	t.Run("a non-definitive change is refused without an invalidation", func(t *testing.T) {
		t.Run("cancelled work item", func(t *testing.T) {
			h := memory.NewHarness(t)
			c := h.NewCase(t, false)
			g := request(t, h, c)
			must(t, h.E.CancelWork(bg, c.Scope, c.Item))
			_, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
			wantStale(t, err, gates.ReasonWorkCancelled, false)
			if cur := current(t, h, g); cur.Invalidation != nil || cur.Status != gates.StatusPending {
				t.Fatalf("gate %+v", cur)
			}
		})
		t.Run("run no longer accepts progression", func(t *testing.T) {
			h := memory.NewHarness(t)
			c := h.NewCase(t, false)
			g := request(t, h, c)
			must(t, h.E.EditRun(bg, c.Scope, c.Run, func(a *execution.Aggregate) { a.RequestCancel(execution.ReasonCancelled, h.E.Clock.Now()) }))
			_, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
			wantStale(t, err, gates.ReasonStepNotWaiting, false)
			if cur := current(t, h, g); cur.Invalidation != nil || cur.Status != gates.StatusPending {
				t.Fatalf("gate %+v", cur)
			}
		})
		t.Run("approval cannot complete a cancelled step", func(t *testing.T) {
			h := memory.NewHarness(t)
			c := h.NewCase(t, false)
			g := request(t, h, c)
			_, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
			must(t, err)
			must(t, h.E.EditRun(bg, c.Scope, c.Run, func(a *execution.Aggregate) { a.RequestCancel(execution.ReasonCancelled, h.E.Clock.Now()) }))
			if rep := process(t, h, g); rep.Completed != 0 {
				t.Fatalf("a cancelled step was completed: %+v", rep)
			}
			if got := h.StepStatus(t, c); got != execution.StepCancelled {
				t.Fatalf("terminal step changed to %s", got)
			}
		})
	})

	t.Run("an unverifiable subject never becomes a gate", func(t *testing.T) {
		cases := []struct {
			name   string
			change func(*memory.Harness, gatescontract.Case)
			want   error
		}{
			{"evidence of another run", func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.EditArtifact(bg, c.Scope, c.Evidence, func(m *gates.EvidenceMeta) { m.RunID = "run_other" }))
			}, gates.ErrEvidence},
			{"unusable evidence", func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.EditArtifact(bg, c.Scope, c.Evidence, func(m *gates.EvidenceMeta) { m.Usable = false }))
			}, gates.ErrEvidence},
			{"evidence without a verified digest", func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.EditArtifact(bg, c.Scope, c.Evidence, func(m *gates.EvidenceMeta) { m.Digest = "not-a-digest" }))
			}, gates.ErrEvidence},
			{"evidence produced from another commit", func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.EditArtifact(bg, c.Scope, c.Evidence, func(m *gates.EvidenceMeta) { m.Commit = otherCommit }))
			}, gates.ErrEvidence},
			{"evidence produced from another repository", func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.EditArtifact(bg, c.Scope, c.Evidence, func(m *gates.EvidenceMeta) { m.Repository = "https://git.example.test/other.git" }))
			}, gates.ErrEvidence},
			{"evidence without provenance", func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.EditArtifact(bg, c.Scope, c.Evidence, func(m *gates.EvidenceMeta) { m.Commit, m.Repository = "", "" }))
			}, gates.ErrEvidence},
			{"unknown evidence", func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.EditRun(bg, c.Scope, c.Run, func(a *execution.Aggregate) { a.Step("test").Outputs["report"] = stringValue("art_unknown") }))
			}, gates.ErrEvidence},
			{"candidate outside the retained workspace history", func(h *memory.Harness, c gatescontract.Case) {
				must(t, h.E.EditRun(bg, c.Scope, c.Run, func(a *execution.Aggregate) { a.Run.SourceCommit = otherCommit }))
			}, gates.ErrEvidence},
			{"candidate verification outage", func(h *memory.Harness, c gatescontract.Case) {
				h.E.Candidates.Fail(errors.New("workspace store down"))
			}, gates.ErrUnavailable},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h := memory.NewHarness(t)
				c := h.NewCase(t, false)
				tc.change(h, c)
				if g, created, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, tc.want) || g != nil || created {
					t.Fatalf("request: %v %v %v", g, created, err)
				}
				noGate(t, h, c)
				if h.StepStatus(t, c) != execution.StepPending {
					t.Fatalf("the step moved")
				}
			})
		}
		for name, answer := range map[string]func(*gates.EvidenceMeta){
			"an artifact of another project": func(m *gates.EvidenceMeta) { m.Scope.ProjectID = "p_foreign" },
			"an answer for another artifact": func(m *gates.EvidenceMeta) { m.ID = "art_someone_else" },
		} {
			t.Run("a misbehaving evidence store answers with "+name, func(t *testing.T) {
				h := memory.NewHarness(t)
				c := h.NewCase(t, false)
				d := h.E.Dependencies()
				d.Evidence = rewriting{inner: h.E.Evidence, edit: answer}
				svc, err := gates.NewService(d, h.E.ServiceOptions())
				must(t, err)
				if _, _, err := svc.RequestForStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrEvidence) {
					t.Fatalf("request: %v", err)
				}
				noGate(t, h, c)
			})
		}
		t.Run("deployment recording another candidate", func(t *testing.T) {
			h := memory.NewHarness(t)
			c := h.NewCase(t, true)
			must(t, h.E.Deployments.Set(bg, c.Scope, c.Deployment, otherCommit, digest("config-1")))
			if _, _, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrEvidence) {
				t.Fatalf("request: %v", err)
			}
			noGate(t, h, c)
		})
		t.Run("deployment of another project", func(t *testing.T) {
			h := memory.NewHarness(t)
			c := h.NewCase(t, true)
			foreign := domain.Scope{TenantID: c.Scope.TenantID, ProjectID: "p_foreign"}
			must(t, h.E.Deployments.Set(bg, foreign, c.Deployment, memory.Commit, digest("config-1")))
			if _, _, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrEvidence) {
				t.Fatalf("request: %v", err)
			}
			noGate(t, h, c)
		})
		t.Run("deployment unknown", func(t *testing.T) {
			h := memory.NewHarness(t)
			c := h.NewCase(t, true)
			must(t, h.E.Deployments.Remove(bg, c.Scope.TenantID, c.Deployment))
			if _, _, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrNotFound) {
				t.Fatalf("request: %v", err)
			}
			noGate(t, h, c)
		})
		t.Run("deployment outage", func(t *testing.T) {
			h := memory.NewHarness(t)
			c := h.NewCase(t, true)
			h.DeploymentOutage(t, true)
			if _, _, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrUnavailable) {
				t.Fatalf("request: %v", err)
			}
			noGate(t, h, c)
		})
		t.Run("deployment adapter missing fails closed", func(t *testing.T) {
			h := memory.NewHarness(t)
			c := h.NewCase(t, true)
			d := h.E.Dependencies()
			d.Deployments = nil
			svc, err := gates.NewService(d, h.E.ServiceOptions())
			must(t, err)
			if _, _, err := svc.RequestForStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrNotConfigured) {
				t.Fatalf("request: %v", err)
			}
			noGate(t, h, c)
		})
		t.Run("a source-only gate pins each bound artifact once", func(t *testing.T) {
			h := memory.NewHarness(t)
			c := h.NewCase(t, false)
			g := request(t, h, c)
			if n, err := h.E.PinCount(bg, c.Scope, g.ID); err != nil || n != len(g.Subject.Evidence()) || n != 1 {
				t.Fatalf("pins %d, evidence %d: %v", n, len(g.Subject.Evidence()), err)
			}
			if id, cfg := g.Subject.Deployment(); id != "" || cfg != "" {
				t.Fatalf("a source-only gate binds no deployment: %q %q", id, cfg)
			}
			if g.Subject.CandidateCommit() != memory.Commit || g.Subject.ContentRevision() != 1 {
				t.Fatalf("subject %s %d", g.Subject.CandidateCommit(), g.Subject.ContentRevision())
			}
		})
	})

	t.Run("reviewer authority is current at the decision and at consumption", func(t *testing.T) {
		t.Run("revoked and demoted reviewers do not decide", func(t *testing.T) {
			h := memory.NewHarness(t)
			c := h.NewCase(t, false)
			g := request(t, h, c)
			must(t, h.E.Authority.Grant(bg, c.Scope, "u_gone", memory.RoleReviewer))
			must(t, h.E.Authority.Revoke(bg, c.Scope, "u_gone"))
			if _, err := decide(h, "u_gone", g, gates.DecisionApprove); !errors.Is(err, gates.ErrHidden) {
				t.Fatalf("a revoked reviewer: %v", err)
			}
			must(t, h.E.Authority.Grant(bg, c.Scope, "u_demoted", memory.RoleReviewer, memory.RoleViewer))
			must(t, h.E.Authority.RemoveRole(bg, c.Scope, "u_demoted", memory.RoleReviewer))
			if _, err := decide(h, "u_demoted", g, gates.DecisionApprove); !errors.Is(err, gates.ErrDenied) {
				t.Fatalf("a demoted reviewer: %v", err)
			}
			if cur := current(t, h, g); cur.Status != gates.StatusPending || cur.Version != g.Version {
				t.Fatalf("a refused decision changed the gate: %+v", cur)
			}
			if ev := h.Events(t, c, g.ID); ev.Decided != 0 {
				t.Fatalf("events %+v", ev)
			}
		})
		t.Run("only a human reviewer decides", func(t *testing.T) {
			h := memory.NewHarness(t)
			c := h.NewCase(t, false)
			g := request(t, h, c)
			must(t, h.E.Authority.Grant(bg, c.Scope, "u_admin", memory.RoleAdmin))
			must(t, h.E.Authority.GrantActor(bg, c.Scope, "wl_attempt", gates.ActorWorkload, memory.RoleReviewer))
			must(t, h.E.Authority.GrantActor(bg, c.Scope, "svc_bot", gates.ActorService, memory.RoleReviewer))
			for _, who := range []string{"u_admin", "wl_attempt", "svc_bot"} {
				if _, err := decide(h, who, g, gates.DecisionApprove); !errors.Is(err, gates.ErrDenied) {
					t.Errorf("%s: %v", who, err)
				}
			}
			// An author who is also a reviewer can decide: roles are the rule.
			must(t, h.E.Authority.Grant(bg, c.Scope, "u_author", memory.RoleEngineer, memory.RoleReviewer))
			if got, err := decide(h, "u_author", g, gates.DecisionApprove); err != nil || got.Decision.By.ID != "u_author" {
				t.Fatalf("author-plus-reviewer: %v", err)
			}
		})
		t.Run("a demoted reviewer's approval is not consumed", func(t *testing.T) {
			h := memory.NewHarness(t)
			c := h.NewCase(t, false)
			g := request(t, h, c)
			must(t, h.E.Authority.Grant(bg, c.Scope, "u_temp", memory.RoleReviewer, memory.RoleViewer))
			_, err := decide(h, "u_temp", g, gates.DecisionApprove)
			must(t, err)
			must(t, h.E.Authority.RemoveRole(bg, c.Scope, "u_temp", memory.RoleReviewer))
			rep := process(t, h, g)
			if rep.Completed != 0 || rep.Invalidated != 0 || rep.Waiting != 1 {
				t.Fatalf("%+v", rep)
			}
			if h.StepStatus(t, c) != execution.StepPending {
				t.Fatalf("the demoted reviewer's approval moved the step")
			}
			must(t, h.E.Authority.Grant(bg, c.Scope, "u_temp", memory.RoleReviewer))
			if rep := process(t, h, g); rep.Completed != 1 {
				t.Fatalf("after the role was restored: %+v", rep)
			}
		})
		t.Run("the authority outage denies without a verdict", func(t *testing.T) {
			h := memory.NewHarness(t)
			c := h.NewCase(t, false)
			g := request(t, h, c)
			_, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
			must(t, err)
			d := h.E.Dependencies()
			d.Authority = failingAuthority{}
			svc, err := gates.NewService(d, h.E.ServiceOptions())
			must(t, err)
			if _, err := svc.Decide(bg, caller(memory.Reviewer), c.Scope, g.ID, cmdFor(g, gates.DecisionReject)); !errors.Is(err, gates.ErrUnavailable) {
				t.Fatalf("a decision during an authority outage: %v", err)
			}
			if cur := current(t, h, g); cur.Status != gates.StatusApproved || cur.Invalidation != nil {
				t.Fatalf("an outage changed the gate: %+v", cur)
			}
		})
	})

	t.Run("foreign tenants and scopes are not found", func(t *testing.T) {
		h := memory.NewHarness(t)
		c := h.NewCase(t, false)
		g := request(t, h, c)
		other := domain.Scope{TenantID: c.Scope.TenantID, ProjectID: "p_other"}
		if _, err := h.E.Store.Get(bg, other, g.ID); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("another project: %v", err)
		}
		if _, err := h.E.Store.Decide(bg, &gates.DecisionPlan{Scope: memory.ScopeB, GateID: g.ID, Command: gates.DecideCommand{
			Decision: gates.DecisionApprove, Reason: reason, Actor: gates.Actor{Type: gates.ActorUser, ID: memory.Foreign}, ExpectedVersion: g.Version, SubjectDigest: g.SubjectDigest(),
		}}); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("another tenant decides: %v", err)
		}
		if _, _, err := h.E.Svc.RequestForStep(bg, memory.ScopeB, c.Run, c.Step); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("another tenant requests: %v", err)
		}
	})
}

// rewriting is an evidence store that answers with altered metadata.
type rewriting struct {
	inner gates.Evidence
	edit  func(*gates.EvidenceMeta)
}

func (r rewriting) Describe(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (gates.EvidenceMeta, error) {
	m, err := r.inner.Describe(ctx, scope, id)
	r.edit(&m)
	return m, err
}

func (r rewriting) Read(ctx context.Context, scope domain.Scope, meta gates.EvidenceMeta, max int64) ([]byte, error) {
	return r.inner.Read(ctx, scope, meta, max)
}

// failingAuthority is an identity authority outage.
type failingAuthority struct{}

func (failingAuthority) ResolveActor(context.Context, domain.Scope, string) (gates.Actor, error) {
	return gates.Actor{}, errors.New("identity down")
}
func (failingAuthority) Authorize(context.Context, gates.Actor, domain.Scope, gates.Permission) error {
	return errors.New("identity down")
}

// TestUnitGateReplay (DQ-03, DQ-05, PX-13, SD-04): repeated, redelivered and
// concurrent work converges: one gate per run step, one decision per gate, one
// progression per approval. Waiting is an explicit durable state that survives
// a restart and a failed commit, holds no executor capacity and never creates
// an attempt.
func TestUnitGateReplay(t *testing.T) {
	t.Run("reconciliation requests the gate once and then waits", func(t *testing.T) {
		h := memory.NewHarness(t)
		c := h.NewCase(t, false)
		rep, err := h.E.Svc.Reconcile(bg, c.Scope.TenantID)
		must(t, err)
		if rep.Requested != 1 || rep.Waiting != 1 {
			t.Fatalf("first pass: %+v", rep)
		}
		g, err := h.E.Store.ByStep(bg, c.Scope, c.Run, c.Step)
		must(t, err)
		if g.ID != gates.GateIDFor(c.Scope, c.Run, c.Step) {
			t.Fatalf("gate id %s", g.ID)
		}
		for i := 0; i < 3; i++ {
			rep, err := h.E.Svc.Reconcile(bg, c.Scope.TenantID)
			must(t, err)
			if rep.Requested != 0 || rep.Waiting != 1 || rep.Completed != 0 || rep.Failed != 0 {
				t.Fatalf("pass %d: %+v", i+2, rep)
			}
		}
		cur := current(t, h, g)
		if cur.Version != 1 || cur.Status != gates.StatusPending {
			t.Fatalf("waiting rewrote the gate: %+v", cur)
		}
		if ev := h.Events(t, c, g.ID); ev != (gatescontract.Events{Requested: 1}) {
			t.Fatalf("events %+v", ev)
		}
		if n, _ := h.E.PinCount(bg, c.Scope, g.ID); n != 1 {
			t.Fatalf("%d pins", n)
		}
		if h.StepStatus(t, c) != execution.StepPending || h.Attempts(t, c) != 0 || h.E.Reservations() != 0 {
			t.Fatalf("a waiting gate must hold no attempt and no capacity")
		}
	})

	t.Run("an interrupted request converges on the same gate and pins", func(t *testing.T) {
		h := memory.NewHarness(t)
		c := h.NewCase(t, false)
		// The evidence pin commits, the request that binds the gate does not.
		commits := 0
		h.E.SetCommitFault(func() error {
			commits++
			if commits >= 2 {
				return errors.New("crash before the gate committed")
			}
			return nil
		})
		if _, _, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrUnavailable) {
			t.Fatalf("request: %v", err)
		}
		h.E.SetCommitFault(nil)
		id := gates.GateIDFor(c.Scope, c.Run, c.Step)
		if _, err := h.E.Store.ByStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("a rolled-back request left a gate: %v", err)
		}
		if ev := h.Events(t, c, id); ev != (gatescontract.Events{}) {
			t.Fatalf("a rolled-back request left events %+v", ev)
		}
		refs, err := h.E.Store.ReadyGateSteps(bg, c.Scope.TenantID, gates.StepRef{}, 10)
		must(t, err)
		if len(refs) != 1 {
			t.Fatalf("the step must stay ready for the retry: %+v", refs)
		}
		if n, _ := h.E.PinCount(bg, c.Scope, id); n != 1 {
			t.Fatalf("the preparatory pin must survive (it is idempotent): %d", n)
		}
		// The retry yields the identical gate; a second retry changes nothing.
		g, created, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step)
		if err != nil || !created || g.ID != id {
			t.Fatalf("retry: %v created=%v", err, created)
		}
		if g2, created, err := h.E.Svc.RequestForStep(bg, c.Scope, c.Run, c.Step); err != nil || created || g2.ID != id {
			t.Fatalf("second retry: %v created=%v", err, created)
		}
		if n, _ := h.E.PinCount(bg, c.Scope, id); n != 1 {
			t.Fatalf("pins %d", n)
		}
		if ev := h.Events(t, c, id); ev != (gatescontract.Events{Requested: 1}) {
			t.Fatalf("events %+v", ev)
		}
	})

	t.Run("a failed decision commit changes nothing and the retry decides once", func(t *testing.T) {
		h := memory.NewHarness(t)
		c := h.NewCase(t, false)
		g := request(t, h, c)
		h.E.FailCommits(errors.New("commit lost"))
		if _, err := decide(h, memory.Reviewer, g, gates.DecisionApprove); !errors.Is(err, gates.ErrUnavailable) {
			t.Fatalf("decide: %v", err)
		}
		h.E.FailCommits(nil)
		cur := current(t, h, g)
		if cur.Status != gates.StatusPending || cur.Version != g.Version || cur.Decision != nil {
			t.Fatalf("a failed commit left a decision: %+v", cur)
		}
		if ev := h.Events(t, c, g.ID); ev.Decided != 0 {
			t.Fatalf("events %+v", ev)
		}
		got, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
		if err != nil || got.Status != gates.StatusApproved {
			t.Fatalf("retry: %v", err)
		}
		if ev := h.Events(t, c, g.ID); ev != (gatescontract.Events{Requested: 1, Decided: 1}) {
			t.Fatalf("events %+v", ev)
		}
	})

	t.Run("the decision is durable before progression and progression applies once", func(t *testing.T) {
		h := memory.NewHarness(t)
		c := h.NewCase(t, false)
		g := request(t, h, c)
		approved, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
		must(t, err)
		h.E.FailCommits(errors.New("step transition cannot commit"))
		rep, err := h.E.Svc.ProcessGate(bg, c.Scope, g.ID)
		if err == nil || rep.Failed != 1 || rep.Completed != 0 {
			t.Fatalf("progression during a fault: %+v %v", rep, err)
		}
		h.E.FailCommits(nil)
		cur := current(t, h, g)
		if cur.Status != gates.StatusApproved || cur.Version != approved.Version || *cur.Decision != *approved.Decision {
			t.Fatalf("only the transition may roll back: %+v", cur)
		}
		if h.StepStatus(t, c) != execution.StepPending {
			t.Fatalf("a rolled-back transition moved the step")
		}
		if rep := process(t, h, g); rep.Completed != 1 {
			t.Fatalf("redelivery: %+v", rep)
		}
		for i := 0; i < 3; i++ {
			if rep := process(t, h, g); rep.Completed != 0 || rep.Failed != 0 {
				t.Fatalf("redelivery %d: %+v", i, rep)
			}
		}
		if h.StepStatus(t, c) != execution.StepSucceeded || h.Attempts(t, c) != 0 || h.E.Reservations() != 0 {
			t.Fatalf("progression must be an attempt-free, capacity-free step transition")
		}
		if ev := h.Events(t, c, g.ID); ev != (gatescontract.Events{Requested: 1, Decided: 1}) {
			t.Fatalf("events %+v", ev)
		}
	})

	t.Run("waiting and approval survive a restart", func(t *testing.T) {
		h := memory.NewHarness(t)
		c := h.NewCase(t, false)
		g := request(t, h, c)
		h.E.Restart()
		if rep := process(t, h, g); rep.Waiting != 1 {
			t.Fatalf("pending after restart: %+v", rep)
		}
		_, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
		must(t, err)
		h.E.Restart()
		if rep := process(t, h, g); rep.Completed != 1 {
			t.Fatalf("approved after restart: %+v", rep)
		}
		h.E.Restart()
		if rep := process(t, h, g); rep.Completed != 0 || rep.Failed != 0 {
			t.Fatalf("settled after restart: %+v", rep)
		}
		if h.E.Reservations() != 0 || h.Attempts(t, c) != 0 {
			t.Fatalf("restart recovery consumed capacity")
		}
	})

	t.Run("concurrent identical decisions converge on one", func(t *testing.T) {
		h := memory.NewHarness(t)
		c := h.NewCase(t, false)
		g := request(t, h, c)
		const n = 8
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = decide(h, memory.Reviewer, g, gates.DecisionApprove)
			}()
		}
		wg.Wait()
		won := 0
		for i, err := range errs {
			switch {
			case err == nil:
				won++
			case errors.Is(err, gates.ErrVersionConflict), errors.Is(err, gates.ErrInvalidTransition):
			default:
				t.Errorf("decision %d: %v", i, err)
			}
		}
		if won != 1 {
			t.Fatalf("%d decisions won", won)
		}
		if ev := h.Events(t, c, g.ID); ev != (gatescontract.Events{Requested: 1, Decided: 1}) {
			t.Fatalf("events %+v", ev)
		}
	})

	t.Run("concurrent opposite decisions record one", func(t *testing.T) {
		h := memory.NewHarness(t)
		c := h.NewCase(t, false)
		g := request(t, h, c)
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for _, d := range []gates.Decision{gates.DecisionApprove, gates.DecisionReject} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := decide(h, memory.Reviewer, g, d)
				results <- err
			}()
		}
		wg.Wait()
		close(results)
		wins := 0
		for err := range results {
			if err == nil {
				wins++
			}
		}
		if wins != 1 {
			t.Fatalf("%d decisions won", wins)
		}
		cur := current(t, h, g)
		if cur.Decision == nil || cur.Version != g.Version+1 {
			t.Fatalf("gate %+v", cur)
		}
	})

	t.Run("a rejection fails the step once and holds no capacity", func(t *testing.T) {
		h := memory.NewHarness(t)
		c := h.NewCase(t, false)
		g := request(t, h, c)
		_, err := decide(h, memory.Reviewer, g, gates.DecisionReject)
		must(t, err)
		if rep := process(t, h, g); rep.Rejected != 1 {
			t.Fatalf("%+v", rep)
		}
		for i := 0; i < 2; i++ {
			if rep := process(t, h, g); rep.Rejected != 0 || rep.Failed != 0 {
				t.Fatalf("redelivery: %+v", rep)
			}
		}
		if h.StepStatus(t, c) != execution.StepFailed || h.Attempts(t, c) != 0 || h.E.Reservations() != 0 {
			t.Fatalf("rejection")
		}
		agg, err := h.E.Runs.Get(bg, c.Scope, c.Run)
		must(t, err)
		if agg.Run.FailureIntentAt == nil || agg.Run.FailureReason != execution.ReasonGateRejected {
			t.Fatalf("a rejection records the run's failure intent: %+v", agg.Run)
		}
		if d := agg.Step("deploy"); d.AttemptsStarted != 0 || len(agg.AttemptsOf("deploy")) != 0 || d.Status == execution.StepRunning {
			t.Fatalf("a rejection must not start the successor: %+v", d)
		}
	})

	t.Run("a bound waiting gate stays out of the ready scan across pages", func(t *testing.T) {
		h := memory.NewHarness(t)
		var cs []gatescontract.Case
		for i := 0; i < 5; i++ {
			cs = append(cs, h.NewCase(t, false))
		}
		var after gates.StepRef
		seen := map[domain.RunID]bool{}
		for {
			refs, err := h.E.Store.ReadyGateSteps(bg, cs[0].Scope.TenantID, after, 2)
			must(t, err)
			for _, r := range refs {
				if seen[r.RunID] {
					t.Fatalf("page overlap at %s", r.RunID)
				}
				seen[r.RunID] = true
			}
			if len(refs) < 2 {
				break
			}
			after = refs[len(refs)-1]
		}
		if len(seen) != 5 {
			t.Fatalf("paging saw %d of 5 ready steps", len(seen))
		}
		request(t, h, cs[2])
		refs, err := h.E.Store.ReadyGateSteps(bg, cs[0].Scope.TenantID, gates.StepRef{}, 10)
		must(t, err)
		if len(refs) != 4 {
			t.Fatalf("%d ready steps after one gate was bound", len(refs))
		}
	})
}

// TestUnitGateConformanceWiring (DQ-14, SD-08): an agent candidate fails closed
// without a conformance adapter and without the producer's accepted publication;
// nothing is substituted.
func TestUnitGateConformanceWiring(t *testing.T) {
	base := baseline(t)
	spec := gatescontract.AgentSpec{Baseline: &base, Declare: true, Supports: 1, Report: func(b runtimev1.DesignBaseline, c string, s []domain.ArtifactID) []byte {
		return reportFor(b, c, s)
	}}
	t.Run("no conformance adapter creates no gate", func(t *testing.T) {
		h := memory.NewHarness(t)
		c := h.NewAgentCase(t, spec)
		d := h.E.Dependencies()
		d.Conformance = nil
		svc, err := gates.NewService(d, h.E.ServiceOptions())
		must(t, err)
		if _, _, err := svc.RequestForStep(bg, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrNotConfigured) {
			t.Fatalf("request: %v", err)
		}
		noGate(t, h, c)
	})
	t.Run("a producer without an accepted publication blocks", func(t *testing.T) {
		h := memory.NewHarness(t)
		c := h.NewAgentCase(t, spec)
		must(t, h.E.EditRun(bg, c.Scope, c.Run, func(a *execution.Aggregate) { a.Attempts[0].Result.SourceCommit = nil }))
		g := request(t, h, c)
		if s := g.Required.Conformance; s == nil || s.Status != gates.ConformanceBlocked || s.Code != gates.CodeConformanceMissing {
			t.Fatalf("snapshot %+v", s)
		}
		_, err := decide(h, memory.Reviewer, g, gates.DecisionApprove)
		if !errors.Is(err, gates.ErrConformance) {
			t.Fatalf("approval: %v", err)
		}
	})
	t.Run("a report of an unusable or foreign artifact blocks", func(t *testing.T) {
		h := memory.NewHarness(t)
		c := h.NewAgentCase(t, spec)
		must(t, h.E.EditArtifact(bg, c.Scope, c.Evidence, func(m *gates.EvidenceMeta) { m.Usable = false }))
		g := request(t, h, c)
		if s := g.Required.Conformance; s == nil || s.Code != gates.CodeConformanceEvidenceInvalid {
			t.Fatalf("snapshot %+v", s)
		}
	})
}

// TestUnitGateStoreDetachesInputs: the plan a caller keeps after Request shares
// nothing with the stored gate.
func TestUnitGateStoreDetachesInputs(t *testing.T) {
	h := memory.NewHarness(t)
	c := h.NewCase(t, false)
	agg, err := h.E.Runs.Get(bg, c.Scope, c.Run)
	must(t, err)
	in, err := agg.GateInputs(c.Step)
	must(t, err)
	meta, err := h.E.Evidence.Describe(bg, c.Scope, c.Evidence)
	must(t, err)
	subject, err := gates.NewSubject(gates.SubjectInput{Scope: c.Scope, WorkItemID: in.WorkItemID, ContentRevision: in.ContentRevision,
		CandidateCommit: in.Candidate.Commit, DefinitionID: in.DefinitionID, Evidence: []gates.EvidenceRef{{ID: meta.ID, Digest: meta.Digest}}})
	must(t, err)
	now := h.E.Clock.Now()
	g, err := gates.Request(gates.RequestInput{ID: gates.GateIDFor(c.Scope, c.Run, c.Step), Scope: c.Scope, RunID: c.Run, StepID: c.Step, Subject: subject,
		Required: gates.Required{Evidence: []gates.RequiredEvidence{{Role: gates.RoleEvidence, ID: meta.ID, Digest: meta.Digest, Expression: "${{ steps.test.outputs.report }}", Producer: "test"}}},
		Now:      now, Deadline: now.Add(time.Hour)})
	must(t, err)
	out, created, err := h.E.Store.Request(bg, &gates.RequestPlan{Gate: g, Inputs: in, Actor: gates.System})
	if err != nil || !created {
		t.Fatalf("request: %v %v", err, created)
	}
	want := current(t, h, out)
	// Mutating the retained plan gate or the returned gate changes nothing stored.
	g.Required.Evidence[0].Digest = "tampered"
	out.Required.Evidence[0].ID = "art_tampered"
	if got := current(t, h, out); !reflect.DeepEqual(got, want) {
		t.Fatalf("a retained input changed the stored gate:\n got %+v\nwant %+v", got, want)
	}
}

func baseline(t testing.TB) runtimev1.DesignBaseline {
	t.Helper()
	b, err := runtimev1.NewDesignBaseline([]string{"SD-01", "DQ-14"}, []string{"C-16"},
		[]runtimev1.SourceRef{{Repository: "https://git.example.test/acme/design.git", Revision: gatescontract.SourceCommit, Path: "docs/specs/software-design.md"}}, gatescontract.RequiredChecks)
	must(t, err)
	return b
}

func reportFor(b runtimev1.DesignBaseline, candidate string, support []domain.ArtifactID) []byte {
	var checks []runtimev1.ConformanceCheck
	for i, id := range gatescontract.RequiredChecks {
		ev := []domain.ArtifactID{"art_none"}
		if i < len(support) {
			ev = []domain.ArtifactID{support[i]}
		}
		checks = append(checks, runtimev1.ConformanceCheck{ID: id, Outcome: runtimev1.CheckPassed, EvidenceArtifactIDs: ev})
	}
	out, _ := json.Marshal(runtimev1.ConformanceReport{BaselineDigest: b.Digest, CandidateCommit: runtimev1.GitCommit(candidate), Requirements: []string{"SD-01"},
		Checks: checks, UnresolvedDeviations: []runtimev1.Deviation{}, Disposition: runtimev1.Conforms})
	return out
}
