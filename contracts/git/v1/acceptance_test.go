package gitv1

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
)

// Test_unit_git_contract_repository_fixtures (GR-01, GR-02, GR-19): the
// repository resource, its create and patch bodies and policy defaults agree
// between the JSON Schema and the Go contract.
func Test_unit_git_contract_repository_fixtures(t *testing.T) {
	runArea(t, "repository")

	t.Run("omitted policy is deny", func(t *testing.T) {
		yes, no := true, false
		for name, in := range map[string]*RepositoryPolicyInput{
			"no policy":           nil,
			"empty policy":        {},
			"explicit false":      {DirectPushToDefault: &no},
			"explicit true":       {DirectPushToDefault: &yes},
			"flag without policy": nil,
		} {
			want := in != nil && in.DirectPushToDefault != nil && *in.DirectPushToDefault
			if got := EffectivePolicy(in).DirectPushToDefault; got != want {
				t.Errorf("%s: effective directPushToDefault = %v, want %v", name, got, want)
			}
		}
		req, err := DecodeRequest[CreateRepositoryRequest]([]byte(`{"name":"backend","policy":{}}`))
		if err != nil || EffectivePolicy(req.Policy).DirectPushToDefault {
			t.Fatalf("a create with an empty policy must resolve to false: %v", err)
		}
		if got := req.EffectiveDefaultBranch(); got != "main" {
			t.Fatalf("default branch = %q", got)
		}
		req, _ = DecodeRequest[CreateRepositoryRequest]([]byte(`{"name":"backend","defaultBranch":"trunk"}`))
		if got := req.EffectiveDefaultBranch(); got != "trunk" {
			t.Fatalf("default branch = %q", got)
		}
	})

	t.Run("branch names", func(t *testing.T) {
		for _, ok := range []string{"main", "master", "trunk", "release/1.x", "feature/a-b_c", "v1.0", "x"} {
			if err := ValidateBranchName(ok); err != nil {
				t.Errorf("%q: %v", ok, err)
			}
		}
		for _, bad := range []string{"", "refs/heads/main", "HEAD", "kodama", "kodama/work/x", "-main", ".hidden", "a..b", "a/.b", "main.lock", "a.lock/b", "main.", "main/", "/main",
			"a//b", "a b", "a~1", "a^", "a:b", "a?b", "a*b", "a[b", "a\\b", "a@{1}", "refs/tags/x", strings.Repeat("a", 201), "a\x00b", "é"} {
			if err := ValidateBranchName(bad); err == nil {
				t.Errorf("%q accepted", bad)
			}
		}
	})

	t.Run("status transitions", func(t *testing.T) {
		all := []RepositoryStatus{StatusProvisioning, StatusReady, StatusArchived}
		allowed := map[[2]RepositoryStatus]bool{
			{StatusProvisioning, StatusReady}: true, {StatusReady, StatusArchived}: true, {StatusArchived, StatusReady}: true,
		}
		for _, from := range all {
			for _, to := range all {
				if got := ValidRepositoryTransition(from, to); got != allowed[[2]RepositoryStatus{from, to}] {
					t.Errorf("%s to %s = %v", from, to, got)
				}
			}
		}
	})
}

// Test_unit_git_contract_grant_fixtures (GR-06, GR-07): grant claims of every
// kind, with refusals for foreign audience and scope, malformed and forbidden
// ref patterns and kind-specific constraints.
func Test_unit_git_contract_grant_fixtures(t *testing.T) {
	runArea(t, "grants")

	t.Run("scope is tenant, project and repository; a mismatch is not found", func(t *testing.T) {
		g := decodeGrant(t, attemptGrant())
		if err := g.CheckScope(tenantT, projectT, "repo_01"); err != nil {
			t.Fatal(err)
		}
		for name, args := range map[string][3]string{
			"foreign tenant":     {"t_other", projectT, "repo_01"},
			"foreign project":    {tenantT, "p_other", "repo_01"},
			"foreign repository": {tenantT, projectT, "repo_99"},
		} {
			t.Run(name, func(t *testing.T) { wantCode(t, g.CheckScope(args[0], args[1], args[2]), domain.CodeNotFound) })
		}
		// A human credential has the same explicit scope: one project, one repository.
		h := decodeGrant(t, humanGrant())
		if err := h.CheckScope(tenantT, projectT, "repo_01"); err != nil {
			t.Fatal(err)
		}
		for name, args := range map[string][3]string{
			"human, foreign tenant":     {"t_other", projectT, "repo_01"},
			"human, foreign project":    {tenantT, "p_other", "repo_01"},
			"human, another repository": {tenantT, projectT, "repo_02"},
		} {
			t.Run(name, func(t *testing.T) { wantCode(t, h.CheckScope(args[0], args[1], args[2]), domain.CodeNotFound) })
		}
		for kind, g := range map[string]obj{"preparation": preparationGrant(), "seed": seedGrant(), "integration": integrationGrant()} {
			gc := decodeGrant(t, g)
			if err := gc.CheckScope(tenantT, projectT, "repo_01"); err != nil {
				t.Fatalf("%s: %v", kind, err)
			}
			wantCode(t, gc.CheckScope(tenantT, "p_other", "repo_01"), domain.CodeNotFound)
			wantCode(t, gc.CheckScope(tenantT, projectT, "repo_02"), domain.CodeNotFound)
		}
	})

	t.Run("a human grant records the maximum class set and permits no server ref", func(t *testing.T) {
		h := decodeGrant(t, humanGrant())
		for ref, want := range map[string]bool{
			task: true, agnt: true, "refs/heads/main": true, "refs/heads/feature/x": true, "refs/tags/v1": true,
			"refs/kodama/runs/run_01/1": false, "refs/notes/commits": false, "refs/heads/": false, "refs/heads/a..b": false,
		} {
			if got := h.PermitsUpdate("repo_01", ru(ref, z40, c1)); got != want {
				t.Errorf("PermitsUpdate(%s) = %v, want %v", ref, got, want)
			}
		}
		if h.PermitsUpdate("repo_02", ru(task, z40, c1)) {
			t.Error("another repository permitted")
		}
		ro := decodeGrant(t, mut(t, humanGrant(), "repositories.0.update", []any{}).(obj))
		if ro.PermitsUpdate("repo_01", ru(task, z40, c1)) {
			t.Error("a read-only person's grant permits an update")
		}
		if got := HumanUpdateCeiling(); strings.Join(got, ",") != "refs/heads/*,refs/tags/*" {
			t.Errorf("ceiling %v", got)
		}
	})

	t.Run("validity window", func(t *testing.T) {
		g := decodeGrant(t, attemptGrant())
		if err := g.CheckTime(ts(t, "2026-10-30T10:07:00Z")); err != nil {
			t.Fatal(err)
		}
		for name, at := range map[string]string{"expired": "2026-10-30T10:15:00Z", "long expired": "2026-10-31T10:00:00Z", "issued in the future": "2026-10-30T09:50:00Z"} {
			t.Run(name, func(t *testing.T) { wantCode(t, g.CheckTime(ts(t, at)), domain.CodeInvalidCredential) })
		}
	})

	t.Run("lifetime bounds are inclusive", func(t *testing.T) {
		for kind, edit := range map[string][]any{
			"attempt": {"expiresAt", "2026-10-31T10:00:00Z"},
			"human":   {"expiresAt", "2026-10-30T11:00:00Z"},
		} {
			base := attemptGrant()
			if kind == "human" {
				base = humanGrant()
			}
			if _, err := DecodeRequest[GrantClaims](mustJSON(t, mut(t, base, edit...))); err != nil {
				t.Errorf("%s at the maximum lifetime: %v", kind, err)
			}
		}
	})

	t.Run("patterns select refs", func(t *testing.T) {
		g := decodeGrant(t, attemptGrant())
		for ref, want := range map[string]bool{
			agnt: true,
			"refs/heads/kodama/agents/wi_1042/run_01/a/b": true,
			task:                             true,
			"refs/heads/kodama/work/wi_1043": false,
			"refs/heads/kodama/agents/wi_1042/run_02/impl": false,
			"refs/heads/kodama/agents/wi_1042/run_01/":     false,
			"refs/heads/main":           false,
			"refs/tags/v1":              false,
			"refs/kodama/runs/run_01/1": false,
		} {
			if got := g.PermitsUpdate("repo_01", ru(ref, z40, c1)); got != want {
				t.Errorf("PermitsUpdate(%s) = %v, want %v", ref, got, want)
			}
		}
		if g.PermitsUpdate("repo_02", ru(agnt, z40, c1)) {
			t.Error("a repository the grant does not name is never permitted")
		}
	})

	t.Run("an integration grant permits exactly one update", func(t *testing.T) {
		g := decodeGrant(t, integrationGrant())
		if !g.PermitsUpdate("repo_01", ru("refs/heads/main", c2, c3)) {
			t.Fatal("the approved update is refused")
		}
		for name, u := range map[string]RefUpdate{
			"other target":   ru("refs/heads/main", c2, c4),
			"other base":     ru("refs/heads/main", c1, c3),
			"other ref":      ru(task, c2, c3),
			"delete":         ru("refs/heads/main", c2, z40),
			"candidate only": ru("refs/heads/main", z40, c3),
		} {
			if g.PermitsUpdate("repo_01", u) {
				t.Errorf("%s permitted", name)
			}
		}
	})
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Test_unit_git_contract_seed_grant (GR-18, GR-19): the credential exchange, the
// single-ref seed grant and its refusals.
func Test_unit_git_contract_seed_grant(t *testing.T) {
	runArea(t, "seed")

	t.Run("a seed grant creates exactly the default branch", func(t *testing.T) {
		g := decodeGrant(t, seedGrant())
		if err := g.CheckSeed("main"); err != nil {
			t.Fatal(err)
		}
		if err := g.CheckSeed("trunk"); err == nil {
			t.Error("a seed for main accepted for a repository whose default branch is trunk")
		}
		if err := decodeGrant(t, seedGrantWorkload()).CheckSeed("main"); err != nil {
			t.Fatalf("a provisioning run's attempt may seed: %v", err)
		}
		if err := decodeGrant(t, attemptGrant()).CheckSeed("main"); err == nil {
			t.Error("an attempt grant passed CheckSeed")
		}
		if err := g.CheckSeed("refs/heads/main"); err == nil {
			t.Error("a ref path accepted as a default branch")
		}
		if !g.PermitsUpdate("repo_01", ru("refs/heads/main", z40, c1)) {
			t.Error("the seed update is refused")
		}
		for _, ref := range []string{"refs/heads/dev", "refs/tags/v1", "refs/kodama/runs/run_01/1", task} {
			if g.PermitsUpdate("repo_01", ru(ref, z40, c1)) {
				t.Errorf("a seed grant permits %s", ref)
			}
		}
	})

	t.Run("lifetime is at most 15 minutes", func(t *testing.T) {
		if _, err := DecodeRequest[GrantClaims](mustJSON(t, mut(t, seedGrant(), "expiresAt", "2026-10-30T10:15:00Z"))); err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeRequest[GrantClaims](mustJSON(t, mut(t, seedGrant(), "expiresAt", "2026-10-30T10:15:00.000000001Z"))); err == nil {
			t.Fatal("a seed grant lasted longer than 15 minutes")
		}
	})

	t.Run("eligibility", func(t *testing.T) {
		repo := func(edit ...any) Repository {
			var r Repository
			if err := json.Unmarshal(mustJSON(t, mut(t, repoBase(), append([]any{"empty", true}, edit...)...)), &r); err != nil {
				t.Fatal(err)
			}
			return r
		}
		if err := CheckSeedEligible(repo(), false); err != nil {
			t.Fatalf("an empty ready internal repository without a pending seed: %v", err)
		}
		for name, tc := range map[string]struct {
			repo    Repository
			pending bool
			code    domain.ErrorCode
			status  int
		}{
			"has refs":                  {repo("empty", false), false, CodeRepositoryNotEmpty, 409},
			"unsettled seed receive":    {repo(), true, CodeSeedPending, 409},
			"external repository":       {repo("hosting", "external"), false, CodeHostingMismatch, 422},
			"archived":                  {repo("status", "archived"), false, CodeRepositoryArchived, 409},
			"still provisioning":        {repo("status", "provisioning"), false, CodeRepositoryNotReady, 409},
			"non-empty and pending":     {repo("empty", false), true, CodeRepositoryNotEmpty, 409},
			"external that is archived": {repo("hosting", "external", "status", "archived"), false, CodeHostingMismatch, 422},
			"unsettled non-seed push":   {repo(), UnsettledPush([]PushRecord{pushRecordWith(t, "authorized", entry(agnt, z40, c4, "committing", nil))}), CodeSeedPending, 409},
			"unknown non-seed push":     {repo(), UnsettledPush([]PushRecord{pushRecordWith(t, "unknown", entry(agnt, z40, c4, "unknown", "journal_regressed"))}), CodeSeedPending, 409},
		} {
			t.Run(name, func(t *testing.T) {
				err := CheckSeedEligible(tc.repo, tc.pending)
				wantCode(t, err, tc.code)
				if s, _ := Status(tc.code); s != tc.status {
					t.Errorf("status %d, want %d", s, tc.status)
				}
			})
		}
	})
}

// Test_unit_git_contract_forwarding_assertion (GR-03, GR-05): the claims the
// server forwards to a tenant's worker and the identity a worker presents to the
// control plane.
func Test_unit_git_contract_forwarding_assertion(t *testing.T) {
	runArea(t, "assertion")

	decodeA := func(edit ...any) ForwardingAssertion {
		v, err := DecodeRequest[ForwardingAssertion](mustJSON(t, mut(t, assertionBase(), edit...)))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	now := ts(t, "2026-10-30T10:02:00Z")

	t.Run("assertion verification", func(t *testing.T) {
		a := decodeA()
		if err := CheckForwarding(a, tenantT, now); err != nil {
			t.Fatal(err)
		}
		if err := CheckForwarding(a, tenantT, ts(t, "2026-10-30T10:04:59Z")); err != nil {
			t.Fatalf("still valid: %v", err)
		}
		wantCode(t, CheckForwarding(a, "t_other", now), domain.CodeForbidden)
		wantCode(t, CheckForwarding(a, tenantT, ts(t, "2026-10-30T10:05:00Z")), domain.CodeInvalidCredential)
		wantCode(t, CheckForwarding(a, tenantT, ts(t, "2026-10-30T10:30:00Z")), domain.CodeInvalidCredential)
		wantCode(t, CheckForwarding(a, tenantT, ts(t, "2026-10-30T09:59:00Z")), domain.CodeInvalidCredential)
		if err := CheckForwarding(a, tenantT, ts(t, "2026-10-30T09:59:45Z")); err != nil {
			t.Fatalf("issuedAt within the clock skew: %v", err)
		}
		bad := a
		bad.Audience = WorkerAudience("t_other")
		wantCode(t, CheckForwarding(bad, tenantT, now), domain.CodeInvalidCredential)
		bad = a
		bad.ExpiresAt = "2026-10-30T10:10:00Z"
		wantCode(t, CheckForwarding(bad, tenantT, now), domain.CodeInvalidCredential)
	})

	t.Run("audience is per tenant worker", func(t *testing.T) {
		if got := WorkerAudience("t_acme"); got != "kodama-git-worker:t_acme" {
			t.Fatal(got)
		}
		if WorkerAudience("t_acme") == WorkerAudience("t_acme2") {
			t.Fatal("audiences of two tenants are equal")
		}
	})

	t.Run("service permission", func(t *testing.T) {
		rw, ro := decodeA(), decodeA("receivePack", false)
		for service, tc := range map[string][2]bool{"git-upload-pack": {true, true}, "git-receive-pack": {true, false}, "git-upload-archive": {false, false}, "": {false, false}} {
			if got := rw.CheckService(service) == nil; got != tc[0] {
				t.Errorf("%q with receivePack: %v", service, got)
			}
			if got := ro.CheckService(service) == nil; got != tc[1] {
				t.Errorf("%q read-only: %v", service, got)
			}
		}
		wantCode(t, ro.CheckService("git-receive-pack"), domain.CodeForbidden)
	})

	t.Run("worker identity is tenant-bound", func(t *testing.T) {
		w, err := DecodeRequest[WorkerIdentity](mustJSON(t, identityBase()))
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckWorkerCall(w, tenantT, now); err != nil {
			t.Fatal(err)
		}
		wantCode(t, CheckWorkerCall(w, "t_other", now), domain.CodeForbidden)
		wantCode(t, CheckWorkerCall(w, tenantT, ts(t, "2026-10-30T10:30:00Z")), domain.CodeInvalidCredential)
		wantCode(t, CheckWorkerCall(w, tenantT, ts(t, "2026-10-30T09:00:00Z")), domain.CodeInvalidCredential)
		bad := w
		bad.Audience = "kodama-git"
		wantCode(t, CheckWorkerCall(bad, tenantT, now), domain.CodeInvalidCredential)
	})
}

// Test_unit_git_contract_worker_rpc (GR-03, S2, S4): the worker and control-plane
// messages in both directions and the route table that separates them.
func Test_unit_git_contract_worker_rpc(t *testing.T) {
	runArea(t, "worker_rpc")

	t.Run("workers call exactly five control-plane routes", func(t *testing.T) {
		got := map[string]bool{}
		for _, op := range Operations {
			if op.Caller == CallerWorker {
				got[op.ID] = true
			}
		}
		want := []string{"authorizePush", "commitIntent", "pushOutcome", "repositoryInitialized", "getAssignments"}
		if len(got) != len(want) {
			t.Fatalf("worker routes %v", got)
		}
		for _, id := range want {
			if !got[id] {
				t.Errorf("missing worker route %s", id)
			}
		}
	})

	t.Run("route families do not mix", func(t *testing.T) {
		seen := map[string]bool{}
		for _, op := range Operations {
			key := string(op.Caller) + " " + op.Method + " " + op.Path
			if seen[key] {
				t.Errorf("duplicate route %s", key)
			}
			seen[key] = true
			switch op.Caller {
			case CallerPublic:
				if !strings.HasPrefix(op.Path, "/tenants/{tenantId}/") {
					t.Errorf("%s: public path %s is not tenant-rooted", op.ID, op.Path)
				}
			case CallerWorker, CallerControl:
				if strings.Contains(op.Path, "{tenantId}") || strings.HasPrefix(op.Path, "/internal") || strings.HasPrefix(op.Path, "/tenants") {
					t.Errorf("%s: internal path %s must be relative to its base path", op.ID, op.Path)
				}
			default:
				t.Errorf("%s: unknown caller %q", op.ID, op.Caller)
			}
		}
		if !strings.HasPrefix(ControlBasePath, "/internal/git/v1/tenants/") || !strings.HasPrefix(WorkerBasePath, "/internal/") {
			t.Error("internal base paths changed")
		}
	})

	t.Run("control-plane reads address the repository by ID only", func(t *testing.T) {
		for _, op := range Operations {
			if op.Caller != CallerControl {
				continue
			}
			if !strings.HasPrefix(op.Path, "/repositories/{repositoryId}/") {
				t.Errorf("%s: %s", op.ID, op.Path)
			}
		}
	})
}

// Test_unit_git_contract_push_authorization (GR-07, GR-09): the authorization
// exchange, push records and the status derived from their entries.
func Test_unit_git_contract_push_authorization(t *testing.T) {
	runArea(t, "push_authorization")

	t.Run("record status follows the entries", func(t *testing.T) {
		e := func(states ...EntryState) []PushEntry {
			var out []PushEntry
			for _, s := range states {
				out = append(out, PushEntry{State: s})
			}
			return out
		}
		for name, tc := range map[string]struct {
			entries []PushEntry
			want    PushStatus
		}{
			"all authorized":                   {e(EntryAuthorized), PushAuthorized},
			"committing":                       {e(EntryCommitting), PushAuthorized},
			"applied":                          {e(EntryApplied), PushApplied},
			"refused":                          {e(EntryRefused), PushRefused},
			"unknown":                          {e(EntryUnknown), PushUnknown},
			"partial push":                     {e(EntryApplied, EntryRefused), PushApplied},
			"all refused":                      {e(EntryRefused, EntryRefused), PushRefused},
			"applied while another is waiting": {e(EntryApplied, EntryCommitting), PushAuthorized},
			"refused while another is waiting": {e(EntryRefused, EntryAuthorized), PushAuthorized},
			"unknown blocks applied":           {e(EntryApplied, EntryUnknown), PushUnknown},
			"unknown blocks refused":           {e(EntryRefused, EntryUnknown), PushUnknown},
			"unknown beats waiting":            {e(EntryCommitting, EntryUnknown), PushUnknown},
			"mixed":                            {e(EntryApplied, EntryRefused, EntryApplied), PushApplied},
		} {
			if got := DeriveStatus(tc.entries); got != tc.want {
				t.Errorf("%s: %s, want %s", name, got, tc.want)
			}
		}
	})

	t.Run("verdicts answer the request in order", func(t *testing.T) {
		req := decodeJSON[PushAuthorizeRequest](t, authReqFF())
		resp := PushAuthorizeResponse{PushID: "push_01", Deadline: "2026-10-30T10:10:00Z"}
		for _, u := range req.Updates {
			resp.Updates = append(resp.Updates, UpdateVerdict{Ref: u.Ref, Verdict: VerdictAllow})
		}
		if err := CheckAuthorizeResponse(req, resp); err != nil {
			t.Fatal(err)
		}
		if !resp.Allowed() {
			t.Fatal("all allowed is not allowed")
		}
		deny := ReasonRefProtected
		resp.Updates[1].Verdict, resp.Updates[1].Reason = VerdictDeny, &deny
		if resp.Allowed() {
			t.Fatal("a receive with one denied update is allowed as a whole")
		}
		swapped := resp
		swapped.Updates = []UpdateVerdict{resp.Updates[1], resp.Updates[0], resp.Updates[2]}
		if CheckAuthorizeResponse(req, swapped) == nil {
			t.Fatal("verdicts out of order accepted")
		}
		if CheckAuthorizeResponse(req, PushAuthorizeResponse{Updates: resp.Updates[:2]}) == nil {
			t.Fatal("a missing verdict accepted")
		}
		if (PushAuthorizeResponse{}).Allowed() {
			t.Fatal("no verdicts is not an allowance")
		}
	})

	t.Run("update kinds", func(t *testing.T) {
		yes, no := true, false
		for name, tc := range map[string]struct {
			old, new string
			ff       *bool
			want     UpdateKind
		}{
			"create":       {z40, c1, nil, KindCreate},
			"delete":       {c1, z40, nil, KindDelete},
			"fast forward": {c1, c2, &yes, KindFastForward},
			"rewrite":      {c1, c2, &no, KindRewrite},
			"sha256":       {z64, c64, nil, KindCreate},
		} {
			got, err := KindOf(tc.old, tc.new, tc.ff)
			if err != nil || got != tc.want {
				t.Errorf("%s: %s, %v; want %s", name, got, err, tc.want)
			}
		}
		for name, tc := range map[string]struct {
			old, new string
			ff       *bool
		}{"unchanged": {c1, c1, nil}, "both zero": {z40, z40, nil}, "update without a fact": {c1, c2, nil}, "mixed lengths": {c1, c64, &yes}, "malformed": {"x", c1, nil}} {
			if _, err := KindOf(tc.old, tc.new, tc.ff); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
	})

	t.Run("refusal reasons", func(t *testing.T) {
		want := []string{"ref_protected", "not_fast_forward", "grant_scope", "attempt_inactive", "run_inactive", "repository_archived", "seed_only", "seed_consumed", "integration_mismatch"}
		if !sameSet(PushRefusalReasons(), want) {
			t.Fatalf("reasons %v", PushRefusalReasons())
		}
		for _, r := range want {
			if !IsPushRefusalReason(r) || !IsRefusedEntryReason(r) || IsUnknownEntryReason(r) {
				t.Errorf("%s classified wrongly", r)
			}
		}
		for _, r := range []string{"journal_unreadable", "operator_required", "receive_running", ""} {
			if IsPushRefusalReason(r) {
				t.Errorf("%s is not a push refusal", r)
			}
		}
		list := PushRefusalReasons()
		list[0] = "mutated"
		if PushRefusalReasons()[0] == "mutated" {
			t.Error("PushRefusalReasons exposes its backing array")
		}
	})
}

// Test_unit_git_contract_commit_intent (GR-09): commit intent, its refusals and
// the outcome report.
func Test_unit_git_contract_commit_intent(t *testing.T) {
	runArea(t, "commit_intent")

	t.Run("a refusal aborts the transaction with a 409", func(t *testing.T) {
		var op *Operation
		for i := range Operations {
			if Operations[i].ID == "commitIntent" {
				op = &Operations[i]
			}
		}
		if op == nil || op.Caller != CallerWorker || op.Method != "POST" || op.Path != "/pushes/{pushId}/commit-intent" {
			t.Fatalf("operation %+v", op)
		}
		if op.Responses[200] != "CommitIntentResponse" || len(op.Responses) != 1 {
			t.Fatalf("responses %v", op.Responses)
		}
		for _, code := range []domain.ErrorCode{CodePushSettled, CodeAttemptInactive, CodeReceiveExpired} {
			listed := false
			for _, c := range op.Errors {
				listed = listed || c == code
			}
			if s, _ := Status(code); !listed || s != 409 {
				t.Errorf("%s: listed=%v status=%d", code, listed, s)
			}
		}
	})

	t.Run("reasons of a refused commit intent are not push verdict reasons", func(t *testing.T) {
		// attempt_inactive is both a deny reason (authorization) and a 409 code
		// (commit intent); push_settled and receive_expired exist only as codes.
		for _, c := range []string{"push_settled", "receive_expired"} {
			if IsPushRefusalReason(c) {
				t.Errorf("%s is a verdict reason", c)
			}
		}
		if !IsPushRefusalReason("attempt_inactive") {
			t.Error("attempt_inactive is a verdict reason")
		}
	})
}

// Test_unit_git_contract_source_set (PX-23, PX-24, WS-13): pinned source sets with
// ordering, unique non-overlapping safe paths and per-repository access.
func Test_unit_git_contract_source_set(t *testing.T) {
	runArea(t, "source_set")

	mk := func(path, repo string) Source {
		return Source{RepositoryID: repo, Commit: c1, Path: path, Access: AccessRead}
	}

	t.Run("construction orders by path and is deterministic", func(t *testing.T) {
		a, err := NewSourceSet([]Source{mk("web", "repo_02"), mk("api", "repo_01"), mk("libs/core", "repo_03")})
		if err != nil {
			t.Fatal(err)
		}
		if got := []string{a.Sources[0].Path, a.Sources[1].Path, a.Sources[2].Path}; strings.Join(got, ",") != "api,libs/core,web" {
			t.Fatalf("order %v", got)
		}
		da, _ := a.Digest()
		rng := rand.New(rand.NewSource(1))
		for i := 0; i < 20; i++ {
			in := []Source{mk("web", "repo_02"), mk("api", "repo_01"), mk("libs/core", "repo_03")}
			rng.Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })
			b, err := NewSourceSet(in)
			if err != nil {
				t.Fatal(err)
			}
			if db, _ := b.Digest(); db != da {
				t.Fatalf("digest depends on input order: %s %s", db, da)
			}
		}
		changed := []Source{mk("web", "repo_02"), mk("api", "repo_01"), mk("libs/core", "repo_03")}
		changed[1].Commit = c2
		c, _ := NewSourceSet(changed)
		if dc, _ := c.Digest(); dc == da {
			t.Fatal("a different pinned commit has the same digest")
		}
		if _, err := NewSourceSet([]Source{mk("a", "repo_01"), mk("a/b", "repo_02")}); err == nil {
			t.Fatal("overlapping paths accepted by the constructor")
		}
		if _, err := (SourceSet{Sources: []Source{mk("web", "r"), mk("api", "r")}}).Digest(); err == nil {
			t.Fatal("an unordered set has a digest")
		}
	})

	t.Run("digest golden", func(t *testing.T) {
		// The expected value was computed independently of the implementation:
		// Python hashlib over the canonical JSON (sorted keys, no whitespace) of the set.
		set := decodeJSON[SourceSet](t, sourceSetBase())
		got, err := set.Digest()
		if err != nil {
			t.Fatal(err)
		}
		const want = "sha256:d06443c62c06fdcb0479e5d0590b40e4098d3348ba0c461c131f35cf432a02c7"
		if got != want {
			t.Fatalf("digest %s, want %s", got, want)
		}
	})

	t.Run("overlap rule", func(t *testing.T) {
		for _, tc := range []struct {
			a, b    string
			overlap bool
		}{
			{"a", "a", true}, {"a", "a/b", true}, {"a/b", "a", true}, {"a/b", "a/b/c/d", true},
			{"a", "ab", false}, {"a", "a-b", false}, {"a/b", "a/bc", false}, {"a.b", "a", false}, {"a/b", "a/c", false}, {"x", "y/x", false},
		} {
			if got := overlaps(tc.a, tc.b); got != tc.overlap {
				t.Errorf("overlaps(%q, %q) = %v", tc.a, tc.b, got)
			}
		}
	})

	t.Run("lookup", func(t *testing.T) {
		set := decodeJSON[SourceSet](t, sourceSetBase())
		if s, ok := set.Find("frontend"); !ok || s.RepositoryID != "repo_02" || s.Access != AccessRead {
			t.Fatalf("%+v %v", s, ok)
		}
		if _, ok := set.Find("missing"); ok {
			t.Fatal("found a missing path")
		}
	})
}

// Test_unit_git_contract_trigger_config (GR-10, GR-11): trigger selection by
// repository and ref pattern, and the origin filter default.
func Test_unit_git_contract_trigger_config(t *testing.T) {
	runArea(t, "trigger_config")

	t.Run("origin filter defaults to excluding the run's own pushes", func(t *testing.T) {
		var omitted, no, yes TriggerConfig
		omitted = decodeJSON[TriggerConfig](t, mut(t, triggerBase(), "excludeOriginRuns", nil))
		no = decodeJSON[TriggerConfig](t, mut(t, triggerBase(), "excludeOriginRuns", false))
		yes = decodeJSON[TriggerConfig](t, triggerBase())
		if !omitted.Excludes() || !yes.Excludes() || no.Excludes() {
			t.Fatalf("omitted=%v true=%v false=%v", omitted.Excludes(), yes.Excludes(), no.Excludes())
		}
		if out := string(mustJSON(t, omitted)); strings.Contains(out, "excludeOriginRuns") {
			t.Fatalf("an omitted member is written as %s", out)
		}
	})

	t.Run("pattern matching", func(t *testing.T) {
		for _, tc := range []struct {
			pattern, ref string
			want         bool
		}{
			{"refs/heads/main", "refs/heads/main", true},
			{"refs/heads/main", "refs/heads/main2", false},
			{"refs/heads/main", "refs/heads/main/x", false},
			{"refs/heads/kodama/work/*", task, true},
			{"refs/heads/kodama/work/*", "refs/heads/kodama/work/", false},
			{"refs/heads/kodama/work/*", "refs/heads/kodama/work", false},
			{"refs/heads/kodama/work/*", "refs/heads/kodama/workx/a", false},
			{"refs/heads/kodama/agents/*", agnt, true},
			{"refs/heads/release/*", "refs/heads/release/1/2", true},
			{"refs/tags/v1", "refs/tags/v1", true},
			{"refs/tags/*", "refs/tags/v1", false}, // not a valid pattern: it names no component
			{"refs/heads/*", "refs/heads/main", false},
			{"refs/kodama/*", "refs/kodama/runs/run_01/1", false},
			{"refs/heads/main", "main", false},
			{"refs/heads/a/../b", "refs/heads/a/../b", false},
		} {
			if got := MatchRefPattern(tc.pattern, tc.ref); got != tc.want {
				t.Errorf("MatchRefPattern(%q, %q) = %v", tc.pattern, tc.ref, got)
			}
		}
		cfg := decodeJSON[TriggerConfig](t, mut(t, triggerBase(), "refPatterns", []any{"refs/heads/main", "refs/tags/release/*"}))
		if !cfg.Matches("refs/heads/main") || !cfg.Matches("refs/tags/release/1") || cfg.Matches("refs/heads/dev") || cfg.Matches(agnt) {
			t.Fatal("trigger selection")
		}
	})
}

func ru(ref, old, new string) RefUpdate { return RefUpdate{Ref: ref, Old: old, New: new} }

func pushRecordWith(t testing.TB, status string, entries ...any) PushRecord {
	t.Helper()
	return decodeJSON[PushRecord](t, recordBase(attemptActor(), "run_01", status, entries...))
}

// Test_unit_git_contract_seed_unsettled_pushes: a seed is refused while any push
// record of the repository is unsettled, whatever kind of push it was.
func Test_unit_git_contract_seed_unsettled_pushes(t *testing.T) {
	rec := func(status string, entries ...any) PushRecord { return pushRecordWith(t, status, entries...) }
	for name, tc := range map[string]struct {
		records []PushRecord
		want    bool
	}{
		"no records":                       {nil, false},
		"applied":                          {[]PushRecord{rec("applied", entry(agnt, z40, c4, "applied", nil))}, false},
		"refused":                          {[]PushRecord{rec("refused", entry(agnt, z40, c4, "refused", "ref_protected"))}, false},
		"authorized receive in flight":     {[]PushRecord{rec("authorized", entry(agnt, z40, c4, "authorized", nil))}, true},
		"committing receive in flight":     {[]PushRecord{rec("authorized", entry(agnt, z40, c4, "committing", nil))}, true},
		"refused entry beside an open one": {[]PushRecord{rec("authorized", entry(agnt, z40, c4, "refused", "grant_scope"), entry(task, z40, c5, "committing", nil))}, true},
		"unknown outcome":                  {[]PushRecord{rec("unknown", entry(agnt, z40, c4, "unknown", "operator_required"))}, true},
		"applied entry beside an unknown":  {[]PushRecord{rec("unknown", entry(agnt, z40, c4, "applied", nil), entry(task, z40, c5, "unknown", "ref_diverged"))}, true},
		"settled records and one open":     {[]PushRecord{rec("applied", entry(agnt, z40, c4, "applied", nil)), rec("authorized", entry(task, z40, c5, "authorized", nil))}, true},
	} {
		if got := UnsettledPush(tc.records); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
		var repo Repository
		if err := json.Unmarshal(mustJSON(t, mut(t, repoBase(), "empty", true)), &repo); err != nil {
			t.Fatal(err)
		}
		err := CheckSeedEligible(repo, UnsettledPush(tc.records))
		if tc.want {
			wantCode(t, err, CodeSeedPending)
		} else if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Test_unit_git_contract_push_bounds: the per-receive update bound is a contract
// ceiling independent of event size, with an explicit refusal below it.
func Test_unit_git_contract_push_bounds(t *testing.T) {
	compiler := newCompiler(t)
	mk := func(n int) []byte {
		return mustJSON(t, obj{"repositoryId": "repo_01", "grantId": "gg_01", "receiveId": "rcv_big", "updates": manyUpdates(n), "pushOptions": []any{}, "gitFacts": manyFacts(n)})
	}
	for n, want := range map[int]bool{1: true, 64: true, 1024: true, 1025: false, 2000: false} {
		_, err := DecodeRequest[PushAuthorizeRequest](mk(n))
		if (err == nil) != want {
			t.Errorf("%d updates: Go valid=%v (%v)", n, err == nil, err)
		}
		if got := schemaVerdict(t, compiler, "PushAuthorizeRequest", mk(n)); got != want {
			t.Errorf("%d updates: schema valid=%v", n, got)
		}
	}
	if MaxPushUpdates != 1024 {
		t.Fatalf("ceiling %d", MaxPushUpdates)
	}
	for _, tc := range []struct{ updates, max int }{{1, 256}, {256, 256}, {1024, 0}, {1024, 5000}, {1024, 1024}} {
		if err := CheckUpdateBound(tc.updates, tc.max); err != nil {
			t.Errorf("%d under %d: %v", tc.updates, tc.max, err)
		}
	}
	for _, tc := range []struct{ updates, max int }{{257, 256}, {1025, 0}, {1025, 5000}, {2, 1}} {
		err := CheckUpdateBound(tc.updates, tc.max)
		wantCode(t, err, CodeTooManyUpdates)
		if s, _ := Status(CodeTooManyUpdates); s != 422 {
			t.Errorf("status %d", s)
		}
	}
}

// Test_unit_git_contract_pinned_commit (PX-24): a push-originated run pins the
// exact new commit of an applied push record's entry, never a later tip.
func Test_unit_git_contract_pinned_commit(t *testing.T) {
	applied := pushRecordWith(t, "applied", entry(agnt, z40, c4, "applied", nil), entry(task, c1, c5, "refused", "grant_scope"), entry("refs/heads/old", c2, z40, "applied", nil))
	if got, err := PinnedCommit(applied, agnt); err != nil || got != c4 {
		t.Fatalf("%q %v", got, err)
	}
	// The same record and ref always give the same commit: nothing else is read.
	if again, _ := PinnedCommit(applied, agnt); again != c4 {
		t.Fatal("not stable")
	}
	for name, tc := range map[string]struct {
		rec PushRecord
		ref string
	}{
		"refused entry":     {applied, task},
		"deleted ref":       {applied, "refs/heads/old"},
		"ref not in record": {applied, "refs/heads/other"},
		"record in flight":  {pushRecordWith(t, "authorized", entry(agnt, z40, c4, "committing", nil)), agnt},
		"record unknown":    {pushRecordWith(t, "unknown", entry(agnt, z40, c4, "applied", nil), entry(task, c1, c5, "unknown", "ref_diverged")), agnt},
		"record refused":    {pushRecordWith(t, "refused", entry(agnt, z40, c4, "refused", "grant_scope")), agnt},
		"malformed ref":     {applied, "main"},
	} {
		if got, err := PinnedCommit(tc.rec, tc.ref); err == nil {
			t.Errorf("%s: pinned %q", name, got)
		}
	}
}

// Test_unit_git_contract_entry_kinds: each push entry persists the kind the
// worker's git facts gave it at authorization, so a consumer that reloads the
// record after the reference-only event keeps every ref's classification.
func Test_unit_git_contract_entry_kinds(t *testing.T) {
	req := decodeJSON[PushAuthorizeRequest](t, mut(t, authReqFF(), "updates.3", ru("refs/heads/feature/y", c3, c4), "gitFacts.3",
		GitFact{Ref: "refs/heads/feature/y", FastForward: ptr(false)}))
	entries, err := NewPushEntries(req)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]UpdateKind{task: KindFastForward, "refs/heads/feature/x": KindCreate, "refs/tags/v1": KindDelete, "refs/heads/feature/y": KindRewrite}
	if len(entries) != len(want) {
		t.Fatalf("%d entries", len(entries))
	}
	for _, e := range entries {
		if e.State != EntryAuthorized || e.Kind != want[e.Ref] || e.Reason != nil {
			t.Errorf("%+v, want kind %s", e, want[e.Ref])
		}
	}
	if _, err := NewPushEntries(PushAuthorizeRequest{}); err == nil {
		t.Error("an invalid request produced entries")
	}

	t.Run("reloading the record preserves each applied ref's kind", func(t *testing.T) {
		for i := range entries {
			entries[i].State = EntryApplied
		}
		entries[2].State, entries[2].Reason = EntryRefused, ptr(ReasonGrantScope)
		rec := PushRecord{PushID: "push_01", RepositoryID: "repo_01", ReceiveID: "rcv_1", Actor: decodeJSON[Actor](t, humanActor()), GrantID: "gg_01",
			Status: DeriveStatus(entries), Entries: entries, CreatedAt: "2026-10-30T10:05:00Z"}
		raw := mustJSON(t, rec)
		reloaded, err := DecodeResponse[PushRecord](raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range reloaded.Entries {
			if e.Kind != want[e.Ref] {
				t.Errorf("%s reloaded as %s, want %s", e.Ref, e.Kind, want[e.Ref])
			}
		}
		got := AppliedKinds(reloaded.Entries)
		if got != (KindCounts{Create: 1, FastForward: 1, Rewrite: 1, Delete: 0}) || got.Total() != 3 {
			t.Fatalf("applied kinds %+v (the refused delete is not counted)", got)
		}
		pinned, err := PinnedCommit(reloaded, task)
		if err != nil || pinned != c4 {
			t.Fatalf("%q %v", pinned, err)
		}
	})

	t.Run("a kind inconsistent with old and new is refused", func(t *testing.T) {
		for name, e := range map[string]PushEntry{
			"create with old":      {Ref: agnt, Old: c1, New: c2, Kind: KindCreate, State: EntryAuthorized},
			"delete with new":      {Ref: agnt, Old: c1, New: c2, Kind: KindDelete, State: EntryAuthorized},
			"fast forward create":  {Ref: agnt, Old: z40, New: c2, Kind: KindFastForward, State: EntryAuthorized},
			"rewrite delete":       {Ref: agnt, Old: c1, New: z40, Kind: KindRewrite, State: EntryAuthorized},
			"empty kind":           {Ref: agnt, Old: c1, New: c2, State: EntryAuthorized},
			"update called create": {Ref: agnt, Old: c1, New: c2, Kind: "force", State: EntryAuthorized},
		} {
			if e.Validate() == nil {
				t.Errorf("%s accepted", name)
			}
		}
	})
}

func ptr[T any](v T) *T { return &v }
