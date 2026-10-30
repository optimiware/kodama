package gitpush_test

import (
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/gitpush"
)

func oid(c string) string { return strings.Repeat(c, 40) }

var zero = oid("0")

const repoID = "r_1"

func ptr(s string) *string { return &s }

func human(update ...string) gitv1.GrantClaims {
	return gitv1.GrantClaims{GrantID: "g_h", Audience: gitv1.GrantAudience, TenantID: "t_1", ProjectID: "p_1", Kind: gitv1.GrantHuman,
		Actor:        gitv1.Actor{Type: gitv1.ActorUser, ID: "u_1"},
		Repositories: []gitv1.GrantRepository{{ID: repoID, Read: true, Update: update}}}
}

func engineer() gitv1.GrantClaims { return human(gitv1.HumanUpdateCeiling()...) }

func attempt(run string, update ...string) gitv1.GrantClaims {
	if update == nil {
		update = []string{}
	}
	return gitv1.GrantClaims{GrantID: "g_a", Audience: gitv1.GrantAudience, TenantID: "t_1", ProjectID: "p_1", Kind: gitv1.GrantAttempt,
		Actor:        gitv1.Actor{Type: gitv1.ActorWorkload, ID: "att_1", RunID: ptr(run), AttemptID: ptr("att_1")},
		Repositories: []gitv1.GrantRepository{{ID: repoID, Read: true, Update: update}}}
}

func seed(branch string) gitv1.GrantClaims {
	return gitv1.GrantClaims{GrantID: "g_s", Audience: gitv1.GrantAudience, TenantID: "t_1", ProjectID: "p_1", Kind: gitv1.GrantSeed,
		Actor:    gitv1.Actor{Type: gitv1.ActorUser, ID: "u_1"},
		IssuedAt: "2026-10-30T10:00:00Z", ExpiresAt: "2026-10-30T10:10:00Z",
		Repositories: []gitv1.GrantRepository{{ID: repoID, Read: true, Seed: true, Update: []string{"refs/heads/" + branch}}}}
}

func integration(c gitv1.IntegrationConstraint) gitv1.GrantClaims {
	return gitv1.GrantClaims{GrantID: "g_i", Audience: gitv1.GrantAudience, TenantID: "t_1", ProjectID: "p_1", Kind: gitv1.GrantIntegration,
		Actor:        gitv1.Actor{Type: gitv1.ActorWorkload, ID: "att_i", RunID: ptr("run_i"), AttemptID: ptr("att_i")},
		Repositories: []gitv1.GrantRepository{{ID: repoID, Read: true, Update: []string{c.Ref}, Integration: &c}}}
}

func preparation() gitv1.GrantClaims {
	return gitv1.GrantClaims{GrantID: "g_p", Audience: gitv1.GrantAudience, TenantID: "t_1", ProjectID: "p_1", Kind: gitv1.GrantPreparation,
		Actor:        gitv1.Actor{Type: gitv1.ActorWorkload, ID: "prep_1", RunID: ptr("run_1")},
		Repositories: []gitv1.GrantRepository{{ID: repoID, Read: true, Update: []string{}}}}
}

type update struct {
	ref, old, new string
	ff            *bool
}

func t_(v bool) *bool { return &v }

func (u update) request(empty bool) gitv1.PushAuthorizeRequest {
	return gitv1.PushAuthorizeRequest{RepositoryID: repoID, GrantID: "g", ReceiveID: "rcv_1", PushOptions: []string{},
		Updates:  []gitv1.RefUpdate{{Ref: u.ref, Old: u.old, New: u.new}},
		GitFacts: []gitv1.GitFact{{Ref: u.ref, FastForward: u.ff, RepositoryEmpty: empty}}}
}

// Test_unit_gitpush_ref_policy_matrix pins every row of the "Access and ref
// policy" table of git-repositories.md, with the push refusal reason of each
// refusal (GR-07, GR-19, GR-A05, GR-A15).
func Test_unit_gitpush_ref_policy_matrix(t *testing.T) {
	const (
		main  = "refs/heads/main"
		work  = "refs/heads/kodama/work/wi_1"
		other = "refs/heads/kodama/work/wi_2"
		own   = "refs/heads/kodama/agents/wi_1/run_1/work"
		alien = "refs/heads/kodama/agents/wi_1/run_2/work"
		runbr = "refs/heads/kodama/runs/run_1/scratch"
		feat  = "refs/heads/feature/x"
		tag   = "refs/tags/v1"
		rel   = "refs/tags/release/v1"
		srv   = "refs/kodama/runs/run_1/1"
		notes = "refs/notes/commits"
		bad   = "refs/heads/kodama/work/wi_1/extra" // not the shape of a task branch: an ordinary branch
	)
	ff := func(ref string) update { return update{ref, oid("1"), oid("2"), t_(true)} }
	nff := func(ref string) update { return update{ref, oid("1"), oid("2"), t_(false)} }
	create := func(ref string) update { return update{ref, zero, oid("2"), nil} }
	del := func(ref string) update { return update{ref, oid("1"), zero, nil} }
	c := gitv1.IntegrationConstraint{Ref: main, From: oid("1"), To: oid("2"), GateID: "gate_1"}

	holds := map[string]bool{"wi_1": true}
	cases := []struct {
		name   string
		claims gitv1.GrantClaims
		direct bool
		u      update
		held   map[string]bool
		want   string // "" allow
	}{
		// Default branch.
		{"engineer ff default, policy omitted", engineer(), false, ff(main), nil, gitv1.ReasonRefProtected},
		{"engineer ff default, direct push on", engineer(), true, ff(main), nil, ""},
		{"engineer rewrite default, direct push on", engineer(), true, nff(main), nil, gitv1.ReasonNotFastForward},
		{"engineer delete default, direct push on", engineer(), true, del(main), nil, gitv1.ReasonRefProtected},
		{"engineer create default, direct push on", engineer(), true, create(main), nil, gitv1.ReasonRefProtected},
		{"viewer default", human(), true, ff(main), nil, gitv1.ReasonGrantScope},
		{"agent naming the default branch", attempt("run_1", main), true, ff(main), holds, gitv1.ReasonRefProtected},
		{"integration exact", integration(c), false, ff(main), nil, ""},
		{"integration other target", integration(c), false, update{main, oid("1"), oid("3"), t_(true)}, nil, gitv1.ReasonIntegrationMismatch},
		{"integration other base", integration(c), false, update{main, oid("3"), oid("2"), t_(true)}, nil, gitv1.ReasonIntegrationMismatch},
		{"integration other ref", integration(c), false, ff(feat), nil, gitv1.ReasonIntegrationMismatch},
		{"integration non-fast-forward", integration(c), false, nff(main), nil, gitv1.ReasonNotFastForward},
		{"preparation default", preparation(), true, ff(main), nil, gitv1.ReasonGrantScope},

		// Task branch.
		{"engineer ff task", engineer(), false, ff(work), nil, ""},
		{"engineer rewrite task", engineer(), false, nff(work), nil, ""},
		{"engineer create task", engineer(), false, create(work), nil, ""},
		{"engineer delete task", engineer(), false, del(work), nil, ""},
		{"writer ff task", attempt("run_1", work), false, ff(work), holds, ""},
		{"writer create task", attempt("run_1", work), false, create(work), holds, ""},
		{"writer rewrite task", attempt("run_1", work), false, nff(work), holds, gitv1.ReasonNotFastForward},
		{"writer delete task", attempt("run_1", work), false, del(work), holds, gitv1.ReasonRefProtected},
		{"non-writer ff task", attempt("run_1", work), false, ff(work), nil, gitv1.ReasonRefProtected},
		{"writer of another work item", attempt("run_1", work), false, ff(work), map[string]bool{"wi_2": true}, gitv1.ReasonRefProtected},
		{"writer without the pattern", attempt("run_1"), false, ff(work), holds, gitv1.ReasonGrantScope},
		{"writer of another task branch", attempt("run_1", work, other), false, ff(other), holds, gitv1.ReasonRefProtected},

		// Agent and run branches: any attempt of the named run, whatever its access.
		{"own agent branch create", attempt("run_1"), false, create(own), nil, ""},
		{"own agent branch rewrite", attempt("run_1"), false, nff(own), nil, ""},
		{"own agent branch delete", attempt("run_1"), false, del(own), nil, ""},
		{"another run's agent branch", attempt("run_1"), false, ff(alien), nil, gitv1.ReasonGrantScope},
		{"own run branch", attempt("run_1"), false, create(runbr), nil, ""},
		{"another run's run branch", attempt("run_2"), false, create(runbr), nil, gitv1.ReasonGrantScope},
		{"engineer agent branch update", engineer(), false, ff(alien), nil, ""},
		{"engineer agent branch delete", engineer(), false, del(own), nil, ""},
		{"viewer agent branch", human(), false, ff(own), nil, gitv1.ReasonGrantScope},
		{"preparation agent branch", preparation(), false, create(own), nil, gitv1.ReasonGrantScope},

		// Other branches.
		{"engineer other branch", engineer(), false, nff(feat), nil, ""},
		{"engineer malformed kodama branch", engineer(), false, ff(bad), nil, ""},
		{"workload without the pattern", attempt("run_1", work), false, ff(feat), holds, gitv1.ReasonGrantScope},
		{"workload naming the pattern", attempt("run_1", "refs/heads/feature/*"), false, nff(feat), nil, ""},
		{"workload naming a malformed kodama branch", attempt("run_1"), false, create(bad), nil, gitv1.ReasonGrantScope},

		// Tags.
		{"engineer tag create", engineer(), false, create(tag), nil, ""},
		{"engineer tag move", engineer(), false, nff(tag), nil, ""},
		{"engineer tag delete", engineer(), false, del(tag), nil, ""},
		{"release grant tag create", attempt("run_1", "refs/tags/release/*"), false, create(rel), nil, ""},
		{"release grant tag move", attempt("run_1", "refs/tags/release/*"), false, ff(rel), nil, gitv1.ReasonRefProtected},
		{"release grant tag delete", attempt("run_1", "refs/tags/release/*"), false, del(rel), nil, gitv1.ReasonRefProtected},
		{"workload without a tag pattern", attempt("run_1", own), false, create(tag), nil, gitv1.ReasonGrantScope},
		{"release grant for another tag", attempt("run_1", "refs/tags/release/*"), false, create(tag), nil, gitv1.ReasonGrantScope},

		// Server-only and unclassified refs.
		{"engineer server ref", engineer(), true, create(srv), nil, gitv1.ReasonRefProtected},
		{"workload server ref", attempt("run_1"), true, create(srv), nil, gitv1.ReasonRefProtected},
		{"integration server ref", integration(c), true, create(srv), nil, gitv1.ReasonRefProtected},
		{"engineer notes", engineer(), true, create(notes), nil, gitv1.ReasonRefProtected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := gitpush.Evaluate(gitpush.PolicyInput{
				Repo: gitpush.PolicyRepository{DefaultBranch: "main", DirectPushToDefault: tc.direct}, RepositoryID: repoID,
				Claims: tc.claims, Request: tc.u.request(false), AttemptLive: true, WriterHeld: tc.held,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.SeedReceive || got.Denials[0] != tc.want {
				t.Fatalf("deny = %q (seed %v), want %q", got.Denials[0], got.SeedReceive, tc.want)
			}
		})
	}

	t.Run("archived refuses everything", func(t *testing.T) {
		got, err := gitpush.Evaluate(gitpush.PolicyInput{Repo: gitpush.PolicyRepository{DefaultBranch: "main", Archived: true}, RepositoryID: repoID,
			Claims: engineer(), Request: ff(feat).request(false), AttemptLive: true})
		if err != nil || got.Denials[0] != gitv1.ReasonRepositoryArchived {
			t.Fatalf("%+v %v", got, err)
		}
	})
	t.Run("a cancelled attempt cannot push", func(t *testing.T) {
		got, err := gitpush.Evaluate(gitpush.PolicyInput{Repo: gitpush.PolicyRepository{DefaultBranch: "main"}, RepositoryID: repoID,
			Claims: attempt("run_1"), Request: create(own).request(false), AttemptLive: false})
		if err != nil || got.Denials[0] != gitv1.ReasonAttemptInactive {
			t.Fatalf("%+v %v", got, err)
		}
	})
	t.Run("another default branch name", func(t *testing.T) {
		// refs/heads/main is an ordinary branch of a repository whose default is trunk.
		got, err := gitpush.Evaluate(gitpush.PolicyInput{Repo: gitpush.PolicyRepository{DefaultBranch: "trunk"}, RepositoryID: repoID,
			Claims: engineer(), Request: ff(main).request(false), AttemptLive: true})
		if err != nil || got.Denials[0] != "" {
			t.Fatalf("%+v %v", got, err)
		}
		got, err = gitpush.Evaluate(gitpush.PolicyInput{Repo: gitpush.PolicyRepository{DefaultBranch: "trunk"}, RepositoryID: repoID,
			Claims: engineer(), Request: ff("refs/heads/trunk").request(false), AttemptLive: true})
		if err != nil || got.Denials[0] != gitv1.ReasonRefProtected {
			t.Fatalf("%+v %v", got, err)
		}
	})

	// The seed rules: exactly one receive creating exactly the default branch of
	// an empty repository.
	t.Run("seed", func(t *testing.T) {
		in := func(claims gitv1.GrantClaims, branch string, empty bool, us ...update) gitpush.PolicyResult {
			req := us[0].request(empty)
			for _, u := range us[1:] {
				r := u.request(empty)
				req.Updates = append(req.Updates, r.Updates...)
				req.GitFacts = append(req.GitFacts, r.GitFacts...)
			}
			got, err := gitpush.Evaluate(gitpush.PolicyInput{Repo: gitpush.PolicyRepository{DefaultBranch: branch}, RepositoryID: repoID,
				Claims: claims, Request: req, AttemptLive: true})
			if err != nil {
				t.Fatal(err)
			}
			return got
		}
		if got := in(seed("main"), "main", true, create(main)); got.Denied() || !got.SeedReceive {
			t.Fatalf("the seed receive: %+v", got)
		}
		if got := in(seed("main"), "main", true, create(main), create(tag)); got.Denials[0] != gitv1.ReasonSeedOnly || got.Denials[1] != gitv1.ReasonSeedOnly || got.SeedReceive {
			t.Fatalf("an extra ref refuses the whole receive: %+v", got)
		}
		if got := in(seed("main"), "main", true, create(feat)); got.Denials[0] != gitv1.ReasonSeedOnly {
			t.Fatalf("another ref: %+v", got)
		}
		if got := in(seed("main"), "main", false, create(main)); got.Denials[0] != gitv1.ReasonRefProtected || got.SeedReceive {
			t.Fatalf("a repository with refs is never seedable: %+v", got)
		}
		if got := in(seed("main"), "trunk", true, create("refs/heads/trunk")); got.Denials[0] != gitv1.ReasonGrantScope {
			t.Fatalf("a seed of another default branch: %+v", got)
		}
		if got := in(engineer(), "main", true, create(main)); got.Denials[0] != gitv1.ReasonRefProtected {
			t.Fatalf("an ordinary credential never creates the default branch: %+v", got)
		}
	})
}
