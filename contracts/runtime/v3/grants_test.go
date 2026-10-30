package runtimev3

import (
	"encoding/json"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

func grantResp(t testing.TB, o obj) GitGrantResponse {
	t.Helper()
	body, _ := json.Marshal(o)
	r, err := DecodeResponse[GitGrantResponse](body)
	if err != nil {
		t.Fatalf("fixture grant response: %v", err)
	}
	return r
}

func artifactsFor(c ContextV3) map[domain.ArtifactID]runtimev1.ArtifactRef {
	return map[domain.ArtifactID]runtimev1.ArtifactRef{"art_0001": {ID: "art_0001", Scope: c.Scope(), Attempt: c.AttemptID, Kind: runtimev1.KindFile, Usable: true}}
}

func resultOf(outs ...CommitOutput) ResultV3 {
	r := ResultV3{Outcome: OutcomeCompleted, ExitCode: ptr(int64(0)), Commits: append([]CommitOutput{}, outs...), Outputs: map[string]Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	r.Outputs["report"] = runtimev1.StringValue("art_0001")
	return r
}

func claims(t testing.TB, o obj) gitv1.GrantClaims {
	return grantResp(t, grantResponse(o, seedFile)).Grant
}

func wantCode(t *testing.T, name string, err error, code domain.ErrorCode) {
	t.Helper()
	switch {
	case code == "" && err != nil:
		t.Errorf("%s: %v", name, err)
	case code != "" && (err == nil || codeOf(err) != string(code)):
		t.Errorf("%s: got %v, want %s", name, err, code)
	}
}

// Test_unit_runtime_git_runtime_grants: a provisioning run holds no static grant,
// because no grant can name a repository that does not exist at admission; it
// creates the repository during the attempt and then requests the seed grant of
// that repository through POST .../git/seed. An approved integration step
// requests an integration grant through POST .../git/integration. Both are the
// Git contract's own claims (seed, integration kinds), checked here against
// gitv1's validation, and the result's commit outputs are authorized against
// them. PX-25, GR-06, GR-12, DQ-17.
func Test_unit_runtime_git_runtime_grants(t *testing.T) {
	t.Run("fixtures", func(t *testing.T) {
		runCases(t, func(typ string) bool {
			return typ == "GitSeedRequest" || typ == "GitIntegrationRequest" || typ == "GitGrantResponse" || typ == "ErrorResponse"
		})
	})

	t.Run("the provisioning attempt has no static grant and the seed capability", func(t *testing.T) {
		ctx := decodeContext(t, ctxProvision())
		g := ctx.Git
		if g.GrantID != nil || g.CredentialFile != nil || len(g.Repositories) != 0 || g.Runtime.Seed == nil || g.Runtime.Seed.CredentialFile != seedFile || g.Runtime.Integration != nil {
			t.Fatalf("%+v", g)
		}
		if len(g.GrantRepositories()) != 0 {
			t.Fatal("a provisioning run's context lists no repository grant")
		}
		// The writer of a work-item run advertises the integration capability and not seed.
		w := decodeContext(t, ctxWriter()).Git
		if w.Runtime.Seed != nil || w.Runtime.Integration == nil || w.Runtime.Integration.CredentialFile != integrationFile {
			t.Fatalf("%+v", w.Runtime)
		}
	})

	repo := gitv1.Repository{ID: "repo_03", Name: "inventory-api", DefaultBranch: "main", Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady, Empty: true}
	created := []string{"repo_03"}

	t.Run("seed: the grant is the Git contract's seed claim", func(t *testing.T) {
		ctx := decodeContext(t, ctxProvision())
		req := GitSeedRequest{RepositoryID: "repo_03"}
		resp := grantResp(t, grantResponse(seedGrant(), seedFile))
		if err := CheckSeedGrant(ctx, req, resp, created, repo, false); err != nil {
			t.Fatal(err)
		}
		g := resp.Grant
		if err := g.CheckSeed("main"); err != nil || g.Kind != gitv1.GrantSeed || !g.Repositories[0].Seed {
			t.Fatalf("not a seed claim: %v", err)
		}
		if !g.PermitsUpdate("repo_03", gitv1.RefUpdate{Ref: "refs/heads/main", Old: gitv1.ZeroObjectID(40), New: c40c}) {
			t.Fatal("the seed creates the default branch")
		}
		for _, ref := range []string{"refs/heads/other", "refs/heads/kodama/runs/run_01/x", "refs/tags/v1", "refs/kodama/runs/run_01/1"} {
			if g.PermitsUpdate("repo_03", gitv1.RefUpdate{Ref: ref, Old: gitv1.ZeroObjectID(40), New: c40c}) {
				t.Errorf("a seed grant must permit only the default branch: %s", ref)
			}
		}
		if g.PermitsUpdate("repo_04", gitv1.RefUpdate{Ref: "refs/heads/main", Old: gitv1.ZeroObjectID(40), New: c40c}) {
			t.Error("a seed grant never spans repositories")
		}
	})

	t.Run("seed: the control plane's refusals", func(t *testing.T) {
		prov := decodeContext(t, ctxProvision())
		writer := decodeContext(t, ctxWriter())
		req := GitSeedRequest{RepositoryID: "repo_03"}
		good := grantResponse(seedGrant(), seedFile)
		with := func(edits ...any) GitGrantResponse { return grantResp(t, mut(t, good, edits...)) }
		mod := func(f func(*gitv1.Repository)) gitv1.Repository { r := repo; f(&r); return r }
		for name, tc := range map[string]struct {
			ctx       ContextV3
			req       GitSeedRequest
			resp      GitGrantResponse
			created   []string
			repo      gitv1.Repository
			unsettled bool
			code      domain.ErrorCode
		}{
			"the repository this run created":      {prov, req, with(), created, repo, false, ""},
			"a work-item run has no seed route":    {writer, req, with(), created, repo, false, domain.CodeForbidden},
			"a repository this run did not create": {prov, req, with(), []string{"repo_09"}, repo, false, domain.CodeNotFound},
			"no repository created at all":         {prov, req, with(), nil, repo, false, domain.CodeNotFound},
			"another repository than requested":    {prov, req, with(), created, mod(func(r *gitv1.Repository) { r.ID = "repo_04" }), false, domain.CodeNotFound},
			"an invalid request":                   {prov, GitSeedRequest{RepositoryID: "../x"}, with(), created, repo, false, domain.CodeInvalidRequest},
			"a repository with refs":               {prov, req, with(), created, mod(func(r *gitv1.Repository) { r.Empty = false }), false, gitv1.CodeRepositoryNotEmpty},
			"an unsettled push":                    {prov, req, with(), created, repo, true, gitv1.CodeSeedPending},
			"an archived repository":               {prov, req, with(), created, mod(func(r *gitv1.Repository) { r.Status = gitv1.StatusArchived }), false, gitv1.CodeRepositoryArchived},
			"a repository still provisioning":      {prov, req, with(), created, mod(func(r *gitv1.Repository) { r.Status = gitv1.StatusProvisioning }), false, gitv1.CodeRepositoryNotReady},
			"an external repository":               {prov, req, with(), created, mod(func(r *gitv1.Repository) { r.Hosting = gitv1.HostingExternal }), false, gitv1.CodeHostingMismatch},
			"a grant for another default branch":   {prov, req, with(), created, mod(func(r *gitv1.Repository) { r.DefaultBranch = "trunk" }), false, domain.CodeInvalidRequest},
			"a grant for another repository":       {prov, req, with("grant.repositories.0.id", "repo_04"), created, repo, false, domain.CodeForbidden},
			"a grant of another attempt":           {prov, req, with("grant.actor.attemptId", "att_99", "grant.actor.id", "att_99"), created, repo, false, domain.CodeForbidden},
			"a grant of another run":               {prov, req, with("grant.actor.runId", "run_99"), created, repo, false, domain.CodeForbidden},
			"a grant of another project":           {prov, req, with("grant.projectId", "p_other"), created, repo, false, domain.CodeForbidden},
			"a grant of another tenant":            {prov, req, with("grant.tenantId", "t_other"), created, repo, false, domain.CodeForbidden},
			"an integration grant":                 {prov, req, grantResp(t, grantResponse(integrationGrant(), seedFile)), created, repo, false, domain.CodeInvalidRequest},
			"the wrong credential file":            {prov, req, with("credentialFile", integrationFile), created, repo, false, domain.CodeInvalidRequest},
		} {
			wantCode(t, name, CheckSeedGrant(tc.ctx, tc.req, tc.resp, tc.created, tc.repo, tc.unsettled), tc.code)
		}
		// A grant that is not valid by the Git contract never reaches the check:
		// a seed over two repositories fails the response decoding itself.
		two := mut(t, good, "grant.repositories", []any{good["grant"].(obj)["repositories"].([]any)[0], mut(t, obj{"r": good["grant"].(obj)["repositories"].([]any)[0]}, "r.id", "repo_04")["r"]})
		body, _ := json.Marshal(two)
		if _, err := DecodeResponse[GitGrantResponse](body); err == nil {
			t.Fatal("a seed grant names exactly one repository")
		}
	})

	t.Run("seed: the result reports the seeded commit against the seed claim", func(t *testing.T) {
		ctx := decodeContext(t, ctxProvision())
		seed := claims(t, seedGrant())
		out := CommitOutput{RepositoryID: "repo_03", Ref: "refs/heads/main", Commit: c40c, PushID: "pu_10"}
		wantCode(t, "seeded commit", CheckResult(resultOf(out), ctx, artifactsFor(ctx), seed), "")
		wantCode(t, "no commit is required to equal anything", CheckResult(resultOf(CommitOutput{"repo_03", "refs/heads/main", c40d, "pu_10"}), ctx, artifactsFor(ctx), seed), "")
		wantCode(t, "no claims", CheckResult(resultOf(out), ctx, artifactsFor(ctx)), runtimev1.CodeOutputInvalid)
		wantCode(t, "another ref", CheckResult(resultOf(CommitOutput{"repo_03", "refs/heads/other", c40c, "pu_10"}), ctx, artifactsFor(ctx), seed), runtimev1.CodeOutputInvalid)
		wantCode(t, "another repository", CheckResult(resultOf(CommitOutput{"repo_04", "refs/heads/main", c40c, "pu_10"}), ctx, artifactsFor(ctx), seed), runtimev1.CodeOutputInvalid)
		wantCode(t, "an agent branch has no grant", CheckResult(resultOf(CommitOutput{"repo_03", "refs/heads/kodama/runs/run_01/x", c40c, "pu_10"}), ctx, artifactsFor(ctx), seed), runtimev1.CodeOutputInvalid)
		other := seed
		other.Actor.AttemptID = ptr("att_99")
		wantCode(t, "a claim of another attempt", CheckResult(resultOf(out), ctx, artifactsFor(ctx), other), runtimev1.CodeOutputInvalid)
		invalid := seed
		invalid.Repositories = append(append([]gitv1.GrantRepository{}, seed.Repositories...), gitv1.GrantRepository{ID: "repo_04", Read: true, Update: []string{}})
		wantCode(t, "a claim that fails the Git contract", CheckResult(resultOf(out), ctx, artifactsFor(ctx), invalid), runtimev1.CodeOutputInvalid)
		// The capability must be advertised: a work-item run's context refuses a
		// seed claim even if one were presented.
		w := decodeContext(t, ctxWriter())
		wantCode(t, "no seed capability", CheckResult(resultOf(out), w, artifactsFor(w), seed), runtimev1.CodeOutputInvalid)
	})

	// ---- integration ----
	reader := decodeContext(t, ctxReaderAfterWriter()) // att_02: the integration step; the candidate is c40c
	approval := func(edit func(*IntegrationApproval)) *IntegrationApproval {
		a := &IntegrationApproval{GateID: "gate_07", RepositoryID: "repo_01", Ref: "refs/heads/main", Base: c40a, Candidate: c40c, CurrentTip: c40a}
		if edit != nil {
			edit(a)
		}
		return a
	}
	intReq := GitIntegrationRequest{RepositoryID: "repo_01", GateID: "gate_07"}

	t.Run("integration: the grant is exactly the approved update", func(t *testing.T) {
		good := grantResponse(integrationGrant(), integrationFile)
		with := func(edits ...any) GitGrantResponse { return grantResp(t, mut(t, good, edits...)) }
		writer := decodeContext(t, ctxWriter())
		manual := decodeContext(t, ctxManualReader())
		for name, tc := range map[string]struct {
			ctx  ContextV3
			req  GitIntegrationRequest
			resp GitGrantResponse
			appr *IntegrationApproval
			code domain.ErrorCode
		}{
			"an approved candidate":                  {reader, intReq, with(), approval(nil), ""},
			"an unapproved gate: no grant":           {reader, intReq, with(), nil, domain.CodeForbidden},
			"approval of another gate":               {reader, intReq, with(), approval(func(a *IntegrationApproval) { a.GateID = "gate_08" }), domain.CodeForbidden},
			"approval for another repository":        {reader, intReq, with(), approval(func(a *IntegrationApproval) { a.RepositoryID = "repo_02" }), domain.CodeForbidden},
			"a moved base: the default branch moved": {reader, intReq, with(), approval(func(a *IntegrationApproval) { a.CurrentTip = c40b }), runtimev1.CodePublicationConflict},
			"from is not the approved base":          {reader, intReq, with("grant.repositories.0.integration.from", c40b), approval(nil), domain.CodeInvalidRequest},
			"to is not the approved candidate":       {reader, intReq, with("grant.repositories.0.integration.to", c40d), approval(nil), domain.CodeInvalidRequest},
			"another ref":                            {reader, intReq, with("grant.repositories.0.integration.ref", "refs/heads/other", "grant.repositories.0.update", []any{"refs/heads/other"}), approval(nil), domain.CodeInvalidRequest},
			"another gate in the constraint":         {reader, intReq, with("grant.repositories.0.integration.gateId", "gate_08"), approval(nil), domain.CodeInvalidRequest},
			"another repository in the grant":        {reader, intReq, with("grant.repositories.0.id", "repo_04"), approval(nil), domain.CodeInvalidRequest},
			"a grant of another attempt":             {reader, intReq, with("grant.actor.attemptId", "att_99", "grant.actor.id", "att_99"), approval(nil), domain.CodeForbidden},
			"a seed grant":                           {reader, intReq, grantResp(t, grantResponse(seedGrant(), integrationFile)), approval(nil), domain.CodeInvalidRequest},
			"the wrong credential file":              {reader, intReq, with("credentialFile", seedFile), approval(nil), domain.CodeInvalidRequest},
			"a repository without a write source":    {reader, GitIntegrationRequest{RepositoryID: "repo_02", GateID: "gate_07"}, with(), approval(func(a *IntegrationApproval) { a.RepositoryID = "repo_02" }), domain.CodeForbidden},
			"a run without a write source":           {manual, intReq, with(), approval(nil), domain.CodeForbidden},
			"a writer may ask too":                   {writer, intReq, with("grant.actor.attemptId", "att_01", "grant.actor.id", "att_01"), approval(nil), ""},
			"an invalid request":                     {reader, GitIntegrationRequest{RepositoryID: "repo_01"}, with(), approval(nil), domain.CodeInvalidRequest},
		} {
			wantCode(t, name, CheckIntegrationGrant(tc.ctx, tc.req, tc.resp, tc.appr), tc.code)
		}
		// Against the Git contract's own claim: exactly one update, once.
		g := grantResp(t, good).Grant
		update := func(ref, from, to string) bool {
			return g.PermitsUpdate("repo_01", gitv1.RefUpdate{Ref: ref, Old: from, New: to})
		}
		if !update("refs/heads/main", c40a, c40c) || update("refs/heads/main", c40b, c40c) || update("refs/heads/main", c40a, c40d) ||
			update("refs/heads/other", c40a, c40c) || update(taskWI, c40a, c40c) {
			t.Fatal("an integration grant permits exactly the approved update")
		}
		if g.PermitsUpdate("repo_02", gitv1.RefUpdate{Ref: "refs/heads/main", Old: c40a, New: c40c}) {
			t.Fatal("never another repository")
		}
	})

	t.Run("integration: the result reports the applied default-branch push", func(t *testing.T) {
		grant := claims(t, integrationGrant()) // att_02, repo_01, main, c40a -> c40c, gate_07
		main := CommitOutput{"repo_01", "refs/heads/main", c40c, "pu_20"}
		task := CommitOutput{"repo_01", taskWI, c40c, "pu_07"}
		agent := CommitOutput{"repo_01", "refs/heads/kodama/agents/wi_1042/run_01/notes", c40d, "pu_21"}
		art := artifactsFor(reader)
		wantCode(t, "the approved push", CheckResult(resultOf(main), reader, art, grant), "")
		wantCode(t, "with an ordinary agent branch push", CheckResult(resultOf(agent, main), reader, art, grant), "")
		wantCode(t, "commit not equal to the candidate", CheckResult(resultOf(CommitOutput{"repo_01", "refs/heads/main", c40d, "pu_20"}), reader, art, grant), runtimev1.CodeOutputInvalid)
		wantCode(t, "the old base reported as the commit", CheckResult(resultOf(CommitOutput{"repo_01", "refs/heads/main", c40a, "pu_20"}), reader, art, grant), runtimev1.CodeOutputInvalid)
		wantCode(t, "another ref than the integration ref", CheckResult(resultOf(CommitOutput{"repo_01", "refs/heads/other", c40c, "pu_20"}), reader, art, grant), runtimev1.CodeOutputInvalid)
		wantCode(t, "no integration grant: the default branch is not an ordinary attempt's", CheckResult(resultOf(main), reader, art), runtimev1.CodeOutputInvalid)
		wantCode(t, "an ordinary attempt grant never covers it", CheckResult(resultOf(main), decodeContext(t, ctxWriter()), artifactsFor(decodeContext(t, ctxWriter()))), runtimev1.CodeOutputInvalid)
		other := grant
		other.Actor.AttemptID = ptr("att_99")
		wantCode(t, "a grant of another attempt", CheckResult(resultOf(main), reader, art, other), runtimev1.CodeOutputInvalid)
		seedLike := claims(t, seedGrant())
		wantCode(t, "a seed claim does not integrate", CheckResult(resultOf(CommitOutput{"repo_01", "refs/heads/main", c40c, "pu_20"}), reader, art, seedLike), runtimev1.CodeOutputInvalid)
		wrongRepo := grant
		wrongRepo.Repositories = []gitv1.GrantRepository{wrongRepo.Repositories[0]}
		wrongRepo.Repositories[0].ID = "repo_02"
		wantCode(t, "a grant of another repository", CheckResult(resultOf(main), reader, art, wrongRepo), runtimev1.CodeOutputInvalid)
		// Ordinary grants stay narrow: the task branch is not an integration output
		// for a reader whatever claims are presented.
		wantCode(t, "a reader cannot report the task branch", CheckResult(resultOf(task), reader, art, grant), runtimev1.CodeOutputInvalid)
	})
}
