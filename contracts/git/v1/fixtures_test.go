package gitv1

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// The fixtures of this package are produced from typed bases (valid by
// construction) and path mutations, then published under fixtures/<area>.json.
// The schema judges every published payload independently of the Go code, and a
// drift test requires the checked-in files to equal what the bases produce
// (regenerate with KODAMA_UPDATE_GOLDEN=1).

const (
	tenantT  = "t_acme"
	projectT = "p_inventory"

	c1   = "1111111111111111111111111111111111111111"
	c2   = "2222222222222222222222222222222222222222"
	c3   = "3333333333333333333333333333333333333333"
	c4   = "4444444444444444444444444444444444444444"
	c5   = "5555555555555555555555555555555555555555"
	z40  = "0000000000000000000000000000000000000000"
	c64  = "1111111111111111111111111111111111111111111111111111111111111111"
	z64  = "0000000000000000000000000000000000000000000000000000000000000000"
	agnt = "refs/heads/kodama/agents/wi_1042/run_01/impl"
	task = "refs/heads/kodama/work/wi_1042"
)

type obj = map[string]any

type nullT struct{}

// null sets a member to JSON null (mut deletes the member on a Go nil).
var null = nullT{}

func (nullT) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// mut applies path edits (dot-separated, numeric array indexes) to a deep copy of
// base. A nil value deletes the key.
func mut(t testing.TB, base any, edits ...any) any {
	t.Helper()
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(edits); i += 2 {
		root = setAt(t, root, strings.Split(edits[i].(string), "."), edits[i+1])
	}
	return root
}

func setAt(t testing.TB, node any, segs []string, val any) any {
	t.Helper()
	switch n := node.(type) {
	case map[string]any:
		if len(segs) == 1 {
			if val == nil {
				delete(n, segs[0])
			} else {
				n[segs[0]] = val
			}
			return n
		}
		n[segs[0]] = setAt(t, n[segs[0]], segs[1:], val)
		return n
	case []any:
		i, err := strconv.Atoi(segs[0])
		if err != nil || i > len(n) {
			t.Fatalf("bad index %q", segs[0])
		}
		if i == len(n) && len(segs) == 1 {
			return append(n, val) // one past the end appends
		}
		if i == len(n) {
			t.Fatalf("cannot descend past the end at %v", segs)
		}
		if len(segs) == 1 {
			n[i] = val
			return n
		}
		n[i] = setAt(t, n[i], segs[1:], val)
		return n
	}
	t.Fatalf("cannot descend into %T at %v", node, segs)
	return nil
}

type draftCase struct {
	area, name, typ string
	payload         any
	raw             *string
	valid           bool
	goOnly          bool // the schema cannot express the refusal (cross-field or cross-element)
	code            string
}

type areaCases struct {
	area  string
	cases []draftCase
}

func (a *areaCases) ok(name, typ string, payload any) {
	a.cases = append(a.cases, draftCase{area: a.area, name: name, typ: typ, payload: payload, valid: true})
}

// bad is a refusal the schema must refuse as well.
func (a *areaCases) bad(name, typ string, payload any) {
	a.cases = append(a.cases, draftCase{area: a.area, name: name, typ: typ, payload: payload, code: "invalid_request"})
}

// badGo is a refusal only the Go contract can express.
func (a *areaCases) badGo(name, typ string, payload any) {
	a.cases = append(a.cases, draftCase{area: a.area, name: name, typ: typ, payload: payload, code: "invalid_request", goOnly: true})
}

// rawBad is a refusal of a document the parser refuses before any schema applies.
func (a *areaCases) rawBad(name, typ, raw string) {
	a.cases = append(a.cases, draftCase{area: a.area, name: name, typ: typ, raw: &raw, code: "invalid_request"})
}

func (a *areaCases) rawOK(name, typ, raw string) {
	a.cases = append(a.cases, draftCase{area: a.area, name: name, typ: typ, raw: &raw, valid: true})
}

// ---- bases ----

func repoBase() obj {
	return obj{"id": "repo_01", "name": "backend", "defaultBranch": "main", "hosting": "internal", "status": "ready", "reason": nil,
		"cloneUrl": "https://git.example.invalid/git/acme/inventory/backend.git", "policy": obj{"directPushToDefault": false},
		"empty": false, "version": 3, "createdAt": "2026-10-30T10:00:00Z"}
}

func humanActor() obj {
	return obj{"type": "user", "id": "usr_engineer", "runId": nil, "attemptId": nil}
}
func attemptActor() obj {
	return obj{"type": "workload", "id": "att_01", "runId": "run_01", "attemptId": "att_01"}
}
func prepActor() obj {
	return obj{"type": "workload", "id": "rr_prep_01", "runId": "run_01", "attemptId": nil}
}

func grantRepo(update ...any) obj {
	if update == nil {
		update = []any{}
	}
	return obj{"id": "repo_01", "read": true, "update": update, "seed": false, "integration": nil}
}

func grantBase(kind string, actor obj, project any, repos ...any) obj {
	if repos == nil {
		repos = []any{}
	}
	return obj{"grantId": "gg_01", "audience": "kodama-git", "tenantId": tenantT, "projectId": project, "actor": actor, "kind": kind,
		"repositories": repos, "issuedAt": "2026-10-30T10:00:00Z", "expiresAt": "2026-10-30T10:15:00Z"}
}

func humanRepo(update ...any) obj {
	if update == nil {
		update = []any{"refs/heads/*", "refs/tags/*"}
	}
	return grantRepo(update...)
}
func humanGrant() obj { return grantBase("human", humanActor(), projectT, humanRepo()) }
func attemptGrant() obj {
	return grantBase("attempt", attemptActor(), projectT, grantRepo("refs/heads/kodama/agents/wi_1042/run_01/*", task))
}
func preparationGrant() obj { return grantBase("preparation", prepActor(), projectT, grantRepo()) }
func seedRepo(update ...any) obj {
	r := grantRepo(update...)
	r["seed"] = true
	return r
}
func seedGrant() obj {
	return grantBase("seed", humanActor(), projectT, seedRepo("refs/heads/main"))
}
func seedGrantWorkload() obj {
	return grantBase("seed", attemptActor(), projectT, seedRepo("refs/heads/main"))
}
func integrationGrant() obj {
	r := grantRepo("refs/heads/main")
	r["integration"] = obj{"ref": "refs/heads/main", "from": c2, "to": c3, "gateId": "gate_07"}
	return grantBase("integration", attemptActor(), projectT, r)
}

func secondRepo(update ...any) obj {
	r := grantRepo(update...)
	r["id"] = "repo_02"
	return r
}

func upd(ref, old, new string) obj { return obj{"ref": ref, "old": old, "new": new} }

func authReq() obj {
	return obj{"repositoryId": "repo_01", "grantId": "gg_01", "receiveId": "rcv_8f2c",
		"updates":     []any{upd(agnt, z40, c4)},
		"pushOptions": []any{},
		"gitFacts":    []any{obj{"ref": agnt, "fastForward": nil, "repositoryEmpty": false}}}
}

func authReqFF() obj {
	return obj{"repositoryId": "repo_01", "grantId": "gg_01", "receiveId": "rcv_8f2d",
		"updates":     []any{upd(task, c1, c4), upd("refs/heads/feature/x", z40, c5), upd("refs/tags/v1", c2, z40)},
		"pushOptions": []any{"ci.skip", "topic=demo"},
		"gitFacts": []any{obj{"ref": task, "fastForward": true, "repositoryEmpty": false}, obj{"ref": "refs/heads/feature/x", "fastForward": nil, "repositoryEmpty": false},
			obj{"ref": "refs/tags/v1", "fastForward": nil, "repositoryEmpty": false}}}
}

func authReqSeed() obj {
	return obj{"repositoryId": "repo_01", "grantId": "gg_02", "receiveId": "rcv_seed",
		"updates":     []any{upd("refs/heads/main", z40, c1)},
		"pushOptions": []any{},
		"gitFacts":    []any{obj{"ref": "refs/heads/main", "fastForward": nil, "repositoryEmpty": true}}}
}

func authResp() obj {
	return obj{"pushId": "push_01", "deadline": "2026-10-30T10:10:00Z",
		"updates": []any{obj{"ref": agnt, "verdict": "allow", "reason": nil}}}
}

func authRespDeny(reason string) obj {
	return obj{"pushId": "push_02", "deadline": "2026-10-30T10:10:00Z",
		"updates": []any{obj{"ref": "refs/heads/main", "verdict": "deny", "reason": reason}, obj{"ref": agnt, "verdict": "allow", "reason": nil}}}
}

// entry builds a push entry whose kind follows from old and new (an update of an
// existing ref is a fast forward); entryK states the kind.
func entry(ref, old, new, state string, reason any) obj {
	kind := "fast_forward"
	switch {
	case strings.Trim(old, "0") == "":
		kind = "create"
	case strings.Trim(new, "0") == "":
		kind = "delete"
	}
	return entryK(ref, old, new, kind, state, reason)
}

func entryK(ref, old, new, kind, state string, reason any) obj {
	return obj{"ref": ref, "old": old, "new": new, "kind": kind, "state": state, "reason": reason}
}

func recordBase(actor obj, origin any, status string, entries ...any) obj {
	return obj{"pushId": "push_01", "repositoryId": "repo_01", "receiveId": "rcv_8f2c", "actor": actor, "grantId": "gg_01",
		"originRunId": origin, "status": status, "entries": entries, "createdAt": "2026-10-30T10:05:00Z"}
}

func jEntry(receive, op any, ref, old, new string, observed any, state string, seq int) obj {
	return obj{"receiveId": receive, "operationId": op, "ref": ref, "old": old, "new": new, "observed": observed, "state": state, "sequence": seq}
}

func sourceSetBase() obj {
	return obj{"sources": []any{
		obj{"repositoryId": "repo_01", "commit": c1, "path": "backend", "access": "write", "branch": task},
		obj{"repositoryId": "repo_02", "commit": c5, "path": "frontend", "access": "read", "branch": nil}}}
}

func triggerBase() obj {
	return obj{"triggerId": "trg_01", "repositoryId": "repo_01", "refPatterns": []any{"refs/heads/main"}, "excludeOriginRuns": true,
		"definitionId": "pd_01", "enabled": true}
}

func errBody(code string) obj {
	return obj{"error": obj{"code": code, "message": "refused", "requestId": "req_01", "details": obj{}}}
}

func manyUpdates(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = upd("refs/heads/b"+strconv.Itoa(i), z40, c1)
	}
	return out
}

func manyFacts(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = obj{"ref": "refs/heads/b" + strconv.Itoa(i), "fastForward": nil, "repositoryEmpty": false}
	}
	return out
}

// ---- cases ----

// fixtureAreas lists the areas in the order of their acceptance tests.
var fixtureAreas = []string{"repository", "grants", "seed", "assertion", "worker_rpc", "push_authorization", "commit_intent", "ref_journal", "source_set", "trigger_config", "object_ops"}

func allCases(t testing.TB) []areaCases {
	return []areaCases{repositoryCases(t), grantCases(t), seedCases(t), assertionCases(t), workerCases(t), authorizationCases(t), commitIntentCases(t), journalCases(t), sourceSetCases(t), triggerCases(t), objectCases(t)}
}

func repositoryCases(t testing.TB) areaCases {
	a := areaCases{area: "repository"}
	r := repoBase
	a.ok("repository_ready", "Repository", r())
	a.ok("repository_provisioning_with_reason", "Repository", mut(t, r(), "status", "provisioning", "reason", "worker_unavailable", "empty", true))
	a.ok("repository_archived_with_reason", "Repository", mut(t, r(), "status", "archived", "reason", "archived_by_request"))
	a.ok("repository_external", "Repository", mut(t, r(), "hosting", "external", "cloneUrl", "https://git.example.invalid/acme/inventory.git"))
	a.ok("repository_direct_push_enabled", "Repository", mut(t, r(), "policy.directPushToDefault", true))
	a.ok("repository_empty_seedable", "Repository", mut(t, r(), "empty", true))
	a.ok("create_minimal", "CreateRepositoryRequest", obj{"name": "backend"})
	a.ok("create_empty_policy_defaults_to_false", "CreateRepositoryRequest", obj{"name": "backend", "policy": obj{}})
	a.ok("create_full", "CreateRepositoryRequest", obj{"name": "web-ui", "defaultBranch": "trunk", "policy": obj{"directPushToDefault": true}})
	a.ok("create_branch_with_slash", "CreateRepositoryRequest", obj{"name": "x", "defaultBranch": "release/1.x"})
	a.ok("patch_default_branch", "PatchRepositoryRequest", obj{"defaultBranch": "develop"})
	a.ok("patch_policy", "PatchRepositoryRequest", obj{"policy": obj{"directPushToDefault": true}})
	a.ok("list_page", "RepositoryList", obj{"items": []any{r()}, "nextCursor": nil})
	a.ok("list_empty_with_cursor", "RepositoryList", obj{"items": []any{}, "nextCursor": "page_2"})
	a.ok("refs_page", "RefsResponse", obj{"refs": []any{obj{"ref": "refs/heads/main", "commit": c1}, obj{"ref": "refs/tags/v1", "commit": c2}}, "nextCursor": nil, "empty": false})
	a.ok("refs_empty_repository", "RefsResponse", obj{"refs": []any{}, "nextCursor": nil, "empty": true})
	a.ok("refs_sha256_commit", "RefsResponse", obj{"refs": []any{obj{"ref": "refs/heads/main", "commit": c64}}, "nextCursor": nil, "empty": false})
	a.ok("response_tolerates_added_member", "Repository", mut(t, r(), "visibility", "private"))
	a.ok("error_repository_name_taken", "ErrorResponse", errBody("repository_name_taken"))
	a.ok("error_hosting_mismatch", "ErrorResponse", errBody("hosting_mismatch"))
	a.ok("error_repository_not_ready", "ErrorResponse", errBody("repository_not_ready"))
	a.ok("error_repository_archived", "ErrorResponse", errBody("repository_archived"))

	a.bad("create_unknown_field", "CreateRepositoryRequest", obj{"name": "backend", "hosting": "external"})
	a.bad("create_misspelled_field", "CreateRepositoryRequest", obj{"name": "backend", "defaultbranch": "main"})
	a.bad("create_miscased_name", "CreateRepositoryRequest", obj{"Name": "backend"})
	a.bad("create_name_uppercase", "CreateRepositoryRequest", obj{"name": "Backend"})
	a.bad("create_name_leading_digit", "CreateRepositoryRequest", obj{"name": "1backend"})
	a.bad("create_name_too_long", "CreateRepositoryRequest", obj{"name": "a" + strings.Repeat("b", 63)})
	a.bad("create_name_underscore", "CreateRepositoryRequest", obj{"name": "my_repo"})
	a.bad("create_name_path_traversal", "CreateRepositoryRequest", obj{"name": "../etc"})
	a.bad("create_name_missing", "CreateRepositoryRequest", obj{})
	a.bad("create_default_branch_ref_path", "CreateRepositoryRequest", obj{"name": "backend", "defaultBranch": "refs/heads/main"})
	a.bad("create_default_branch_head", "CreateRepositoryRequest", obj{"name": "backend", "defaultBranch": "HEAD"})
	a.bad("create_default_branch_reserved_namespace", "CreateRepositoryRequest", obj{"name": "backend", "defaultBranch": "kodama/work/x"})
	a.bad("create_default_branch_kodama", "CreateRepositoryRequest", obj{"name": "backend", "defaultBranch": "kodama"})
	a.bad("create_default_branch_dotdot", "CreateRepositoryRequest", obj{"name": "backend", "defaultBranch": "a..b"})
	a.bad("create_default_branch_lock", "CreateRepositoryRequest", obj{"name": "backend", "defaultBranch": "main.lock"})
	a.bad("create_default_branch_trailing_slash", "CreateRepositoryRequest", obj{"name": "backend", "defaultBranch": "main/"})
	a.bad("create_default_branch_leading_dash", "CreateRepositoryRequest", obj{"name": "backend", "defaultBranch": "-main"})
	a.bad("create_default_branch_space", "CreateRepositoryRequest", obj{"name": "backend", "defaultBranch": "my branch"})
	a.bad("create_default_branch_null", "CreateRepositoryRequest", obj{"name": "backend", "defaultBranch": null})
	a.bad("create_policy_null", "CreateRepositoryRequest", obj{"name": "backend", "policy": null})
	a.bad("create_policy_flag_null", "CreateRepositoryRequest", obj{"name": "backend", "policy": obj{"directPushToDefault": null}})
	a.bad("create_policy_flag_string", "CreateRepositoryRequest", obj{"name": "backend", "policy": obj{"directPushToDefault": "yes"}})
	a.bad("create_policy_unknown_field", "CreateRepositoryRequest", obj{"name": "backend", "policy": obj{"forcePush": true}})
	a.bad("patch_empty", "PatchRepositoryRequest", obj{})
	a.bad("patch_policy_without_flag", "PatchRepositoryRequest", obj{"policy": obj{}})
	a.bad("patch_unknown_field", "PatchRepositoryRequest", obj{"name": "renamed"})
	a.bad("patch_default_branch_ref_path", "PatchRepositoryRequest", obj{"defaultBranch": "refs/heads/dev"})
	a.bad("repository_ready_with_reason", "Repository", mut(t, r(), "reason", "worker_unavailable"))
	a.bad("repository_reason_not_a_code", "Repository", mut(t, r(), "status", "provisioning", "reason", "Worker Unavailable"))
	a.bad("repository_reason_omitted", "Repository", mut(t, r(), "reason", nil))
	a.bad("repository_unknown_status", "Repository", mut(t, r(), "status", "deleted"))
	a.bad("repository_unknown_hosting", "Repository", mut(t, r(), "hosting", "mirror"))
	a.bad("repository_clone_url_with_credentials", "Repository", mut(t, r(), "cloneUrl", "https://user:secret@git.example.invalid/x.git"))
	a.bad("repository_clone_url_http", "Repository", mut(t, r(), "cloneUrl", "http://git.example.invalid/x.git"))
	a.bad("repository_version_zero", "Repository", mut(t, r(), "version", 0))
	a.bad("repository_timestamp_offset", "Repository", mut(t, r(), "createdAt", "2026-10-30T10:00:00+01:00"))
	a.bad("repository_policy_omitted_flag", "Repository", mut(t, r(), "policy", obj{}))
	a.bad("repository_empty_omitted", "Repository", mut(t, r(), "empty", nil))
	a.bad("repository_id_with_slash", "Repository", mut(t, r(), "id", "repo/01"))
	a.bad("refs_empty_with_refs", "RefsResponse", obj{"refs": []any{obj{"ref": "refs/heads/main", "commit": c1}}, "nextCursor": nil, "empty": true})
	a.badGo("refs_unordered", "RefsResponse", obj{"refs": []any{obj{"ref": "refs/tags/v1", "commit": c2}, obj{"ref": "refs/heads/main", "commit": c1}}, "nextCursor": nil, "empty": false})
	a.bad("refs_zero_tip", "RefsResponse", obj{"refs": []any{obj{"ref": "refs/heads/main", "commit": z40}}, "nextCursor": nil, "empty": false})
	a.bad("refs_malformed_ref", "RefsResponse", obj{"refs": []any{obj{"ref": "refs/heads/a..b", "commit": c1}}, "nextCursor": nil, "empty": false})
	a.bad("error_unknown_code", "ErrorResponse", errBody("no_such_code"))
	a.rawBad("repository_version_fraction", "Repository", `{"id":"repo_01","name":"backend","defaultBranch":"main","hosting":"internal","status":"ready","reason":null,"cloneUrl":"https://git.example.invalid/x.git","policy":{"directPushToDefault":false},"empty":false,"version":3.5,"createdAt":"2026-10-30T10:00:00Z"}`)
	a.rawBad("create_duplicate_name_member", "CreateRepositoryRequest", `{"name":"a","name":"b"}`)
	a.rawBad("create_trailing_data", "CreateRepositoryRequest", `{"name":"a"} {"name":"b"}`)
	return a
}

func grantCases(t testing.TB) areaCases {
	a := areaCases{area: "grants"}
	a.ok("human_grant", "GrantClaims", humanGrant())
	a.ok("human_grant_read_only", "GrantClaims", mut(t, humanGrant(), "repositories.0.update", []any{}))
	a.ok("attempt_grant_agent_and_task_branch", "GrantClaims", attemptGrant())
	a.ok("attempt_grant_agent_branch_only", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/heads/kodama/agents/wi_1042/run_01/*"}))
	a.ok("attempt_grant_run_without_work_item", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/heads/kodama/runs/run_01/*"}))
	a.ok("attempt_grant_reader", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{}))
	a.ok("attempt_grant_two_repositories", "GrantClaims", mut(t, attemptGrant(), "repositories.1", secondRepo("refs/heads/kodama/runs/run_01/*")))
	a.ok("attempt_grant_named_branch_pattern", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/heads/release/*", "refs/heads/hotfix"}))
	a.ok("preparation_grant", "GrantClaims", preparationGrant())
	a.ok("integration_grant", "GrantClaims", integrationGrant())
	a.ok("sha256_integration_grant", "GrantClaims", mut(t, integrationGrant(), "repositories.0.integration.from", c64, "repositories.0.integration.to", strings.Repeat("2", 64)))
	a.ok("seed_grant_user", "GrantClaims", seedGrant())

	a.bad("grant_unknown_field", "GrantClaims", mut(t, attemptGrant(), "scope", "all"))
	a.bad("grant_foreign_audience", "GrantClaims", mut(t, attemptGrant(), "audience", "kodama-runtime"))
	a.bad("grant_worker_audience", "GrantClaims", mut(t, attemptGrant(), "audience", "kodama-git-worker:t_acme"))
	a.bad("grant_unknown_kind", "GrantClaims", mut(t, attemptGrant(), "kind", "admin"))
	a.bad("grant_tenant_not_an_id", "GrantClaims", mut(t, attemptGrant(), "tenantId", "t/acme"))
	a.bad("grant_project_omitted", "GrantClaims", mut(t, attemptGrant(), "projectId", nil))
	a.bad("grant_attempt_without_project", "GrantClaims", mut(t, attemptGrant(), "projectId", null))
	a.bad("grant_human_without_project", "GrantClaims", mut(t, humanGrant(), "projectId", null))
	a.bad("grant_human_project_omitted", "GrantClaims", mut(t, humanGrant(), "projectId", nil))
	a.bad("grant_human_without_repository", "GrantClaims", mut(t, humanGrant(), "repositories", []any{}))
	a.bad("grant_human_two_repositories", "GrantClaims", mut(t, humanGrant(), "repositories.1", secondRepo("refs/heads/*", "refs/tags/*")))
	a.bad("grant_human_update_narrowed", "GrantClaims", mut(t, humanGrant(), "repositories.0.update", []any{"refs/heads/kodama/*"}))
	a.bad("grant_human_update_heads_only", "GrantClaims", mut(t, humanGrant(), "repositories.0.update", []any{"refs/heads/*"}))
	a.bad("grant_human_update_reordered", "GrantClaims", mut(t, humanGrant(), "repositories.0.update", []any{"refs/tags/*", "refs/heads/*"}))
	a.bad("grant_human_update_refs_kodama", "GrantClaims", mut(t, humanGrant(), "repositories.0.update", []any{"refs/heads/*", "refs/tags/*", "refs/kodama/*"}))
	a.bad("grant_human_with_seed_flag", "GrantClaims", mut(t, humanGrant(), "repositories.0.seed", true))
	a.bad("grant_human_not_readable", "GrantClaims", mut(t, humanGrant(), "repositories.0.read", false))
	a.bad("grant_attempt_with_human_ceiling", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/heads/*", "refs/tags/*"}))
	a.bad("grant_attempt_with_bare_heads_wildcard_beside_a_pattern", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{task, "refs/heads/*"}))
	a.bad("grant_human_actor_workload", "GrantClaims", mut(t, humanGrant(), "actor", attemptActor()))
	a.bad("grant_attempt_without_repositories", "GrantClaims", mut(t, attemptGrant(), "repositories", []any{}))
	a.bad("grant_attempt_actor_user", "GrantClaims", mut(t, attemptGrant(), "actor", humanActor()))
	a.bad("grant_attempt_actor_without_attempt", "GrantClaims", mut(t, attemptGrant(), "actor", prepActor()))
	a.bad("grant_workload_without_run", "GrantClaims", mut(t, attemptGrant(), "actor.runId", null))
	a.bad("grant_user_with_run", "GrantClaims", mut(t, humanGrant(), "actor.runId", "run_01"))
	a.badGo("grant_duplicate_repository", "GrantClaims", mut(t, attemptGrant(), "repositories.1", grantRepo()))
	a.bad("grant_repository_not_readable", "GrantClaims", mut(t, attemptGrant(), "repositories.0.read", false))
	a.bad("grant_pattern_refs_kodama_run", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/kodama/runs/run_01/1"}))
	a.bad("grant_pattern_refs_kodama_wildcard", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/kodama/*"}))
	a.bad("grant_pattern_bare_heads_wildcard", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/heads/*"}))
	a.bad("grant_pattern_bare_tags_wildcard", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/tags/*"}))
	a.bad("grant_pattern_traversal", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/heads/../kodama/x"}))
	a.bad("grant_pattern_inner_wildcard", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/heads/a/*/b"}))
	a.bad("grant_pattern_glob_in_name", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/heads/feat*"}))
	a.bad("grant_pattern_not_a_ref", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"main"}))
	a.bad("grant_pattern_notes_namespace", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/notes/commits"}))
	a.bad("grant_pattern_trailing_slash", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/heads/work/"}))
	a.bad("grant_pattern_double_slash", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/heads//x"}))
	a.bad("grant_pattern_lock_suffix", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/heads/x.lock"}))
	a.bad("grant_pattern_space", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{"refs/heads/a b"}))
	a.badGo("grant_pattern_repeated", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", []any{task, task}))
	a.bad("grant_too_many_patterns", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", func() []any {
		out := make([]any, 33)
		for i := range out {
			out[i] = "refs/heads/p" + strconv.Itoa(i)
		}
		return out
	}()))
	a.bad("grant_update_omitted", "GrantClaims", mut(t, attemptGrant(), "repositories.0.update", nil))
	a.bad("grant_attempt_with_seed", "GrantClaims", mut(t, attemptGrant(), "repositories.0.seed", true))
	a.bad("grant_attempt_with_integration", "GrantClaims", mut(t, attemptGrant(), "repositories.0.integration", obj{"ref": "refs/heads/main", "from": c2, "to": c3, "gateId": "gate_07"}))
	a.bad("grant_preparation_with_update", "GrantClaims", mut(t, preparationGrant(), "repositories.0.update", []any{task}))
	a.bad("grant_preparation_with_attempt", "GrantClaims", mut(t, preparationGrant(), "actor", attemptActor()))
	a.bad("grant_integration_without_constraint", "GrantClaims", mut(t, integrationGrant(), "repositories.0.integration", null))
	a.bad("grant_integration_two_updates", "GrantClaims", mut(t, integrationGrant(), "repositories.0.update", []any{"refs/heads/main", task}))
	a.badGo("grant_integration_update_differs_from_ref", "GrantClaims", mut(t, integrationGrant(), "repositories.0.update", []any{task}))
	a.badGo("grant_integration_wildcard_update", "GrantClaims", mut(t, integrationGrant(), "repositories.0.update", []any{"refs/heads/kodama/*"}))
	a.badGo("grant_integration_same_commit", "GrantClaims", mut(t, integrationGrant(), "repositories.0.integration.to", c2))
	a.bad("grant_integration_zero_target", "GrantClaims", mut(t, integrationGrant(), "repositories.0.integration.to", z40))
	a.bad("grant_integration_zero_base", "GrantClaims", mut(t, integrationGrant(), "repositories.0.integration.from", z40))
	a.badGo("grant_integration_mixed_lengths", "GrantClaims", mut(t, integrationGrant(), "repositories.0.integration.to", c64))
	a.bad("grant_integration_tag_ref", "GrantClaims", mut(t, integrationGrant(), "repositories.0.integration.ref", "refs/tags/v1"))
	a.bad("grant_integration_gate_not_an_id", "GrantClaims", mut(t, integrationGrant(), "repositories.0.integration.gateId", "gate 07"))
	a.bad("grant_integration_unknown_field", "GrantClaims", mut(t, integrationGrant(), "repositories.0.integration.force", true))
	a.bad("grant_integration_two_repositories", "GrantClaims", mut(t, integrationGrant(), "repositories.1", func() obj {
		r := grantRepo("refs/heads/main")
		r["id"] = "repo_02"
		r["integration"] = obj{"ref": "refs/heads/main", "from": c2, "to": c3, "gateId": "gate_07"}
		return r
	}()))
	a.badGo("grant_expires_before_issued", "GrantClaims", mut(t, attemptGrant(), "expiresAt", "2026-10-30T09:00:00Z"))
	a.badGo("grant_workload_lifetime_too_long", "GrantClaims", mut(t, attemptGrant(), "expiresAt", "2026-10-31T10:00:01Z"))
	a.badGo("grant_human_lifetime_too_long", "GrantClaims", mut(t, humanGrant(), "expiresAt", "2026-10-30T11:00:01Z"))
	a.bad("grant_issued_at_omitted", "GrantClaims", mut(t, attemptGrant(), "issuedAt", nil))
	return a
}

func seedCases(t testing.TB) areaCases {
	a := areaCases{area: "seed"}
	a.ok("credential_request_human", "CredentialRequest", obj{"projectId": projectT, "repositoryId": "repo_01", "seed": false})
	a.ok("credential_request_seed", "CredentialRequest", obj{"projectId": projectT, "repositoryId": "repo_01", "seed": true})
	a.ok("credential_response", "CredentialResponse", obj{"username": "kodama", "password": "kgc_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "grantId": "gg_01", "expiresAt": "2026-10-30T10:15:00Z"})
	a.ok("seed_grant_user", "GrantClaims", seedGrant())
	a.ok("seed_grant_provisioning_attempt", "GrantClaims", seedGrantWorkload())
	a.ok("seed_grant_custom_default_branch", "GrantClaims", mut(t, seedGrant(), "repositories.0.update", []any{"refs/heads/trunk"}))
	a.ok("seed_grant_full_lifetime", "GrantClaims", mut(t, seedGrant(), "expiresAt", "2026-10-30T10:15:00Z"))
	a.ok("error_repository_not_empty", "ErrorResponse", errBody("repository_not_empty"))
	a.ok("error_seed_pending", "ErrorResponse", errBody("seed_pending"))

	a.bad("credential_request_unknown_field", "CredentialRequest", obj{"projectId": projectT, "repositoryId": "repo_01", "seed": false, "ttl": 3600})
	a.bad("credential_request_seed_omitted", "CredentialRequest", obj{"projectId": projectT, "repositoryId": "repo_01"})
	a.bad("credential_request_seed_null", "CredentialRequest", obj{"projectId": projectT, "repositoryId": "repo_01", "seed": null})
	a.bad("credential_request_seed_string", "CredentialRequest", obj{"projectId": projectT, "repositoryId": "repo_01", "seed": "true"})
	a.bad("credential_request_project_omitted", "CredentialRequest", obj{"repositoryId": "repo_01", "seed": false})
	a.bad("credential_request_repository_omitted", "CredentialRequest", obj{"projectId": projectT, "seed": false})
	a.bad("credential_request_tenant_wide", "CredentialRequest", obj{"seed": false})
	a.bad("credential_request_old_nested_seed_shape", "CredentialRequest", obj{"seed": obj{"repositoryId": "repo_01"}})
	a.bad("credential_request_repository_with_slash", "CredentialRequest", obj{"projectId": projectT, "repositoryId": "repo/01", "seed": false})
	a.bad("credential_request_repository_path_not_id", "CredentialRequest", obj{"projectId": projectT, "repositoryId": "git/acme/inventory/backend.git", "seed": true})
	a.bad("credential_response_short_password", "CredentialResponse", obj{"username": "kodama", "password": "short", "grantId": "gg_01", "expiresAt": "2026-10-30T10:15:00Z"})
	a.bad("credential_response_password_with_space", "CredentialResponse", obj{"username": "kodama", "password": "kgc aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "grantId": "gg_01", "expiresAt": "2026-10-30T10:15:00Z"})
	a.bad("credential_response_grant_omitted", "CredentialResponse", obj{"username": "kodama", "password": "kgc_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "expiresAt": "2026-10-30T10:15:00Z"})
	a.bad("seed_grant_extra_ref", "GrantClaims", mut(t, seedGrant(), "repositories.0.update", []any{"refs/heads/main", "refs/heads/dev"}))
	a.bad("seed_grant_extra_tag", "GrantClaims", mut(t, seedGrant(), "repositories.0.update", []any{"refs/heads/main", "refs/tags/v1"}))
	a.bad("seed_grant_no_ref", "GrantClaims", mut(t, seedGrant(), "repositories.0.update", []any{}))
	a.bad("seed_grant_wildcard", "GrantClaims", mut(t, seedGrant(), "repositories.0.update", []any{"refs/heads/main/*"}))
	a.bad("seed_grant_tag", "GrantClaims", mut(t, seedGrant(), "repositories.0.update", []any{"refs/tags/v1"}))
	a.bad("seed_grant_refs_kodama", "GrantClaims", mut(t, seedGrant(), "repositories.0.update", []any{"refs/kodama/runs/run_01/1"}))
	a.bad("seed_grant_kodama_branch_namespace_pattern", "GrantClaims", mut(t, seedGrant(), "repositories.0.update", []any{"refs/heads/kodama/*"}))
	a.badGo("seed_grant_reserved_branch", "GrantClaims", mut(t, seedGrant(), "repositories.0.update", []any{"refs/heads/kodama/work/wi_1"}))
	a.bad("seed_grant_two_repositories", "GrantClaims", mut(t, seedGrant(), "repositories.1", func() obj {
		r := seedRepo("refs/heads/main")
		r["id"] = "repo_02"
		return r
	}()))
	a.bad("seed_grant_seed_flag_false", "GrantClaims", mut(t, seedGrant(), "repositories.0.seed", false))
	a.bad("seed_grant_with_integration", "GrantClaims", mut(t, seedGrant(), "repositories.0.integration", obj{"ref": "refs/heads/main", "from": c2, "to": c3, "gateId": "gate_07"}))
	a.bad("seed_grant_without_project", "GrantClaims", mut(t, seedGrant(), "projectId", null))
	a.badGo("seed_grant_lifetime_over_15_minutes", "GrantClaims", mut(t, seedGrant(), "expiresAt", "2026-10-30T10:15:01Z"))
	a.badGo("seed_grant_workload_without_attempt", "GrantClaims", mut(t, seedGrantWorkload(), "actor", prepActor()))
	a.bad("non_seed_grant_with_seed_flag", "GrantClaims", mut(t, attemptGrant(), "repositories.0.seed", true))
	return a
}

func assertionBase() obj {
	return obj{"audience": "kodama-git-worker:t_acme", "tenantId": tenantT, "repositoryId": "repo_01", "grantId": "gg_01", "receivePack": true,
		"issuedAt": "2026-10-30T10:00:00Z", "expiresAt": "2026-10-30T10:05:00Z", "requestId": "req_01"}
}

func identityBase() obj {
	return obj{"audience": "kodama-git-control", "tenantId": tenantT, "workerInstance": "gitworker-7f9c", "issuedAt": "2026-10-30T10:00:00Z", "expiresAt": "2026-10-30T10:30:00Z"}
}

func assertionCases(t testing.TB) areaCases {
	a := areaCases{area: "assertion"}
	a.ok("forwarding_assertion_receive_pack", "ForwardingAssertion", assertionBase())
	a.ok("forwarding_assertion_read_only", "ForwardingAssertion", mut(t, assertionBase(), "receivePack", false))
	a.ok("forwarding_assertion_max_lifetime", "ForwardingAssertion", mut(t, assertionBase(), "expiresAt", "2026-10-30T10:05:00Z"))
	a.ok("worker_identity", "WorkerIdentity", identityBase())

	a.bad("assertion_unknown_field", "ForwardingAssertion", mut(t, assertionBase(), "credential", "kgc_aaaaaaaaaaaaaaaa"))
	a.bad("assertion_forwards_authorization_header", "ForwardingAssertion", mut(t, assertionBase(), "authorization", "Basic abc"))
	a.badGo("assertion_audience_of_another_tenant", "ForwardingAssertion", mut(t, assertionBase(), "audience", "kodama-git-worker:t_other"))
	a.bad("assertion_audience_without_tenant", "ForwardingAssertion", mut(t, assertionBase(), "audience", "kodama-git-worker"))
	a.bad("assertion_audience_git_grant", "ForwardingAssertion", mut(t, assertionBase(), "audience", "kodama-git"))
	a.bad("assertion_audience_runtime", "ForwardingAssertion", mut(t, assertionBase(), "audience", "kodama-runtime"))
	a.bad("assertion_audience_control", "ForwardingAssertion", mut(t, assertionBase(), "audience", "kodama-git-control"))
	a.badGo("assertion_lifetime_over_5_minutes", "ForwardingAssertion", mut(t, assertionBase(), "expiresAt", "2026-10-30T10:05:01Z"))
	a.badGo("assertion_expires_at_issued", "ForwardingAssertion", mut(t, assertionBase(), "expiresAt", "2026-10-30T10:00:00Z"))
	a.bad("assertion_request_id_omitted", "ForwardingAssertion", mut(t, assertionBase(), "requestId", nil))
	a.bad("assertion_receive_pack_omitted", "ForwardingAssertion", mut(t, assertionBase(), "receivePack", nil))
	a.bad("assertion_receive_pack_string", "ForwardingAssertion", mut(t, assertionBase(), "receivePack", "true"))
	a.bad("assertion_repository_path_traversal", "ForwardingAssertion", mut(t, assertionBase(), "repositoryId", "../repo"))
	a.bad("assertion_tenant_not_an_id", "ForwardingAssertion", mut(t, assertionBase(), "tenantId", "t acme"))
	a.bad("identity_unknown_field", "WorkerIdentity", mut(t, identityBase(), "role", "admin"))
	a.bad("identity_assertion_audience", "WorkerIdentity", mut(t, identityBase(), "audience", "kodama-git-worker:t_acme"))
	a.bad("identity_git_grant_audience", "WorkerIdentity", mut(t, identityBase(), "audience", "kodama-git"))
	a.bad("identity_instance_omitted", "WorkerIdentity", mut(t, identityBase(), "workerInstance", nil))
	a.badGo("identity_lifetime_over_1_hour", "WorkerIdentity", mut(t, identityBase(), "expiresAt", "2026-10-30T11:00:01Z"))
	return a
}

func workerCases(t testing.TB) areaCases {
	a := areaCases{area: "worker_rpc"}
	a.ok("initialized_request", "InitializedRequest", obj{"repositoryId": "repo_01", "defaultBranch": "main", "empty": true})
	a.ok("initialized_response", "InitializedResponse", obj{"repositoryId": "repo_01", "status": "ready", "version": 4})
	a.ok("assignments_response", "AssignmentsResponse", obj{"items": []any{
		obj{"kind": "initialize", "repositoryId": "repo_01", "defaultBranch": "main"}, obj{"kind": "archive", "repositoryId": "repo_02", "defaultBranch": "main"},
		obj{"kind": "maintain", "repositoryId": "repo_03", "defaultBranch": "trunk"}, obj{"kind": "reconcile", "repositoryId": "repo_01", "defaultBranch": "main"}}})
	a.ok("assignments_none", "AssignmentsResponse", obj{"items": []any{}})
	a.ok("assignments_reconcile_ack", "AssignmentsResponse", obj{"items": []any{
		obj{"kind": "reconcile", "repositoryId": "repo_01", "defaultBranch": "main", "journalAck": 41}}})
	a.bad("assignment_ack_negative", "AssignmentsResponse", obj{"items": []any{
		obj{"kind": "reconcile", "repositoryId": "repo_01", "defaultBranch": "main", "journalAck": -1}}})
	a.ok("initialize_request", "InitializeRepositoryRequest", obj{"repositoryId": "repo_01", "defaultBranch": "main"})
	a.ok("initialize_response_created", "InitializeRepositoryResponse", obj{"repositoryId": "repo_01", "defaultBranch": "main", "empty": true, "created": true})
	a.ok("initialize_response_repeat", "InitializeRepositoryResponse", obj{"repositoryId": "repo_01", "defaultBranch": "main", "empty": false, "created": false})
	a.ok("read_refs_request_first_page", "ReadRefsRequest", obj{"repositoryId": "repo_01", "prefix": "refs/heads/", "cursor": nil, "limit": 100})
	a.ok("read_refs_request_server_namespace", "ReadRefsRequest", obj{"repositoryId": "repo_01", "prefix": "refs/kodama/", "cursor": "next_1", "limit": 1000})
	a.ok("read_ref_values_request", "ReadRefValuesRequest", obj{"repositoryId": "repo_01", "refs": []any{agnt, "refs/heads/main"}})
	a.ok("read_ref_values_response", "ReadRefValuesResponse", obj{"values": []any{obj{"ref": agnt, "commit": c4}, obj{"ref": "refs/heads/main", "commit": nil}}, "quiescent": true, "journalHead": 12, "workerInstance": "gitworker-7f9c"})
	a.ok("read_journal_by_receive", "ReadJournalRequest", obj{"repositoryId": "repo_01", "receiveId": "rcv_8f2c", "ref": nil, "sinceSequence": 0, "limit": 100})
	a.ok("read_journal_by_ref", "ReadJournalRequest", obj{"repositoryId": "repo_01", "receiveId": nil, "ref": agnt, "sinceSequence": 7, "limit": 100})
	a.ok("read_journal_response", "ReadJournalResponse", obj{"entries": []any{
		jEntry("rcv_8f2c", nil, agnt, z40, c4, z40, "prepared", 8), jEntry("rcv_8f2c", nil, agnt, z40, c4, nil, "committed", 9)},
		"headSequence": 9, "hasMore": false, "workerInstance": "gitworker-7f9c"})
	a.ok("read_journal_response_empty", "ReadJournalResponse", obj{"entries": []any{}, "headSequence": 0, "hasMore": false, "workerInstance": "gitworker-7f9c"})
	a.ok("receive_liveness_running", "ReceiveLivenessResponse", obj{"receiveId": "rcv_8f2c", "running": true, "workerInstance": "gitworker-7f9c"})
	a.ok("receive_liveness_ended", "ReceiveLivenessResponse", obj{"receiveId": "rcv_8f2c", "running": false, "workerInstance": "gitworker-8a1d"})
	a.ok("set_retained_ref_request", "SetRetainedRefRequest", obj{"repositoryId": "repo_01", "operationId": "op_01", "ref": "refs/kodama/runs/run_01/1", "commit": c1})
	a.ok("set_retained_ref_response", "SetRetainedRefResponse", obj{"ref": "refs/kodama/runs/run_01/1", "commit": c1, "created": true})
	a.ok("delete_retained_ref_request", "DeleteRetainedRefRequest", obj{"repositoryId": "repo_01", "operationId": "op_02", "ref": "refs/kodama/runs/run_01/12", "expectedCommit": c1})
	a.ok("delete_retained_ref_response", "DeleteRetainedRefResponse", obj{"ref": "refs/kodama/runs/run_01/12", "deleted": false})
	a.ok("create_branch_request_task", "CreateBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_10", "ref": task, "commit": c1})
	a.ok("create_branch_request_agent", "CreateBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_11", "ref": agnt, "commit": c1})
	a.ok("create_branch_request_run", "CreateBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_12", "ref": "refs/heads/kodama/runs/run_02/impl", "commit": c64})
	a.ok("create_branch_response_created", "CreateBranchResponse", obj{"ref": task, "commit": c1, "created": true})
	a.ok("create_branch_response_already_there", "CreateBranchResponse", obj{"ref": task, "commit": c1, "created": false})
	a.ok("error_branch_exists", "ErrorResponse", errBody("branch_exists"))
	a.ok("delete_branch_request_agent", "DeleteBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_03", "ref": agnt, "expectedCommit": c4})
	a.ok("delete_branch_request_task", "DeleteBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_04", "ref": task, "expectedCommit": c4})
	a.ok("delete_branch_request_run", "DeleteBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_05", "ref": "refs/heads/kodama/runs/run_02/impl", "expectedCommit": c4})
	a.ok("delete_branch_response", "DeleteBranchResponse", obj{"ref": agnt, "deleted": true})
	a.ok("maintain_request", "MaintainRequest", obj{"repositoryId": "repo_01", "operationId": "op_06"})
	a.ok("maintain_response", "MaintainResponse", obj{"operationId": "op_06", "completedAt": "2026-10-30T11:00:00Z"})
	a.ok("error_ref_changed", "ErrorResponse", errBody("ref_changed"))
	a.ok("error_retained_ref_exists", "ErrorResponse", errBody("retained_ref_exists"))
	a.ok("error_unavailable", "ErrorResponse", errBody("unavailable"))
	a.ok("error_journal_unreadable", "ErrorResponse", errBody("journal_unreadable"))

	a.bad("initialized_unknown_field", "InitializedRequest", obj{"repositoryId": "repo_01", "defaultBranch": "main", "empty": true, "path": "/data/repo_01.git"})
	a.bad("initialized_default_branch_ref_path", "InitializedRequest", obj{"repositoryId": "repo_01", "defaultBranch": "refs/heads/main", "empty": true})
	a.bad("initialized_empty_omitted", "InitializedRequest", obj{"repositoryId": "repo_01", "defaultBranch": "main"})
	a.bad("initialized_response_unknown_status", "InitializedResponse", obj{"repositoryId": "repo_01", "status": "deleted", "version": 4})
	a.bad("initialize_request_path_as_id", "InitializeRepositoryRequest", obj{"repositoryId": "../../etc", "defaultBranch": "main"})
	a.bad("initialize_request_unknown_field", "InitializeRepositoryRequest", obj{"repositoryId": "repo_01", "defaultBranch": "main", "hooks": "/tmp/h"})
	a.bad("assignment_unknown_kind", "AssignmentsResponse", obj{"items": []any{obj{"kind": "delete", "repositoryId": "repo_01", "defaultBranch": "main"}}})
	a.badGo("assignment_duplicate", "AssignmentsResponse", obj{"items": []any{obj{"kind": "archive", "repositoryId": "repo_01", "defaultBranch": "main"}, obj{"kind": "archive", "repositoryId": "repo_01", "defaultBranch": "main"}}})
	a.bad("assignments_items_null", "AssignmentsResponse", obj{"items": null})
	a.bad("read_refs_foreign_prefix", "ReadRefsRequest", obj{"repositoryId": "repo_01", "prefix": "refs/notes/", "cursor": nil, "limit": 100})
	a.bad("read_refs_prefix_without_slash", "ReadRefsRequest", obj{"repositoryId": "repo_01", "prefix": "refs/heads", "cursor": nil, "limit": 100})
	a.bad("read_refs_limit_zero", "ReadRefsRequest", obj{"repositoryId": "repo_01", "prefix": "refs/heads/", "cursor": nil, "limit": 0})
	a.bad("read_refs_limit_too_large", "ReadRefsRequest", obj{"repositoryId": "repo_01", "prefix": "refs/heads/", "cursor": nil, "limit": 1001})
	a.bad("read_refs_cursor_omitted", "ReadRefsRequest", obj{"repositoryId": "repo_01", "prefix": "refs/heads/", "limit": 100})
	a.bad("read_refs_cursor_with_space", "ReadRefsRequest", obj{"repositoryId": "repo_01", "prefix": "refs/heads/", "cursor": "a b", "limit": 100})
	a.bad("read_ref_values_empty", "ReadRefValuesRequest", obj{"repositoryId": "repo_01", "refs": []any{}})
	a.badGo("read_ref_values_duplicate", "ReadRefValuesRequest", obj{"repositoryId": "repo_01", "refs": []any{agnt, agnt}})
	a.bad("read_ref_values_malformed_ref", "ReadRefValuesRequest", obj{"repositoryId": "repo_01", "refs": []any{"refs/heads/../x"}})
	a.bad("read_ref_values_response_zero_commit", "ReadRefValuesResponse", obj{"values": []any{obj{"ref": agnt, "commit": z40}}, "quiescent": true, "journalHead": 1, "workerInstance": "gitworker-7f9c"})
	a.bad("read_ref_values_response_quiescent_omitted", "ReadRefValuesResponse", obj{"values": []any{}, "journalHead": 1, "workerInstance": "gitworker-7f9c"})
	a.bad("read_journal_both_selectors", "ReadJournalRequest", obj{"repositoryId": "repo_01", "receiveId": "rcv_8f2c", "ref": agnt, "sinceSequence": 0, "limit": 100})
	a.bad("read_journal_no_selector", "ReadJournalRequest", obj{"repositoryId": "repo_01", "receiveId": nil, "ref": nil, "sinceSequence": 0, "limit": 100})
	a.bad("read_journal_negative_since", "ReadJournalRequest", obj{"repositoryId": "repo_01", "receiveId": "rcv_8f2c", "ref": nil, "sinceSequence": -1, "limit": 100})
	a.bad("read_journal_limit_zero", "ReadJournalRequest", obj{"repositoryId": "repo_01", "receiveId": "rcv_8f2c", "ref": nil, "sinceSequence": 0, "limit": 0})
	a.badGo("read_journal_response_unordered", "ReadJournalResponse", obj{"entries": []any{
		jEntry("rcv_8f2c", nil, agnt, z40, c4, nil, "committed", 9), jEntry("rcv_8f2c", nil, agnt, z40, c4, z40, "prepared", 8)}, "headSequence": 9, "hasMore": false, "workerInstance": "gitworker-7f9c"})
	a.badGo("read_journal_response_beyond_head", "ReadJournalResponse", obj{"entries": []any{jEntry("rcv_8f2c", nil, agnt, z40, c4, z40, "prepared", 8)}, "headSequence": 7, "hasMore": false, "workerInstance": "gitworker-7f9c"})
	a.bad("read_journal_response_invalid_entry", "ReadJournalResponse", obj{"entries": []any{jEntry("rcv_8f2c", nil, agnt, z40, c4, nil, "prepared", 8)}, "headSequence": 9, "hasMore": false, "workerInstance": "gitworker-7f9c"})
	a.bad("receive_liveness_running_omitted", "ReceiveLivenessResponse", obj{"receiveId": "rcv_8f2c", "workerInstance": "gitworker-7f9c"})
	a.bad("retained_ref_heads_namespace", "SetRetainedRefRequest", obj{"repositoryId": "repo_01", "operationId": "op_01", "ref": "refs/heads/main", "commit": c1})
	a.bad("retained_ref_wrong_namespace", "SetRetainedRefRequest", obj{"repositoryId": "repo_01", "operationId": "op_01", "ref": "refs/kodama/other/1", "commit": c1})
	a.bad("retained_ref_zero_counter", "SetRetainedRefRequest", obj{"repositoryId": "repo_01", "operationId": "op_01", "ref": "refs/kodama/runs/run_01/0", "commit": c1})
	a.bad("retained_ref_leading_zero_counter", "SetRetainedRefRequest", obj{"repositoryId": "repo_01", "operationId": "op_01", "ref": "refs/kodama/runs/run_01/01", "commit": c1})
	a.bad("retained_ref_no_counter", "SetRetainedRefRequest", obj{"repositoryId": "repo_01", "operationId": "op_01", "ref": "refs/kodama/runs/run_01", "commit": c1})
	a.bad("retained_ref_traversal", "SetRetainedRefRequest", obj{"repositoryId": "repo_01", "operationId": "op_01", "ref": "refs/kodama/runs/../1", "commit": c1})
	a.bad("retained_ref_zero_commit", "SetRetainedRefRequest", obj{"repositoryId": "repo_01", "operationId": "op_01", "ref": "refs/kodama/runs/run_01/1", "commit": z40})
	a.bad("retained_ref_short_commit", "SetRetainedRefRequest", obj{"repositoryId": "repo_01", "operationId": "op_01", "ref": "refs/kodama/runs/run_01/1", "commit": "abc123"})
	a.bad("retained_ref_unknown_field", "SetRetainedRefRequest", obj{"repositoryId": "repo_01", "operationId": "op_01", "ref": "refs/kodama/runs/run_01/1", "commit": c1, "force": true})
	a.bad("delete_retained_ref_expected_omitted", "DeleteRetainedRefRequest", obj{"repositoryId": "repo_01", "operationId": "op_02", "ref": "refs/kodama/runs/run_01/1"})
	a.bad("create_branch_default_branch", "CreateBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_10", "ref": "refs/heads/main", "commit": c1})
	a.bad("create_branch_tag", "CreateBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_10", "ref": "refs/tags/v1", "commit": c1})
	a.bad("create_branch_server_ref", "CreateBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_10", "ref": "refs/kodama/runs/run_01/1", "commit": c1})
	a.bad("create_branch_other_branch", "CreateBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_10", "ref": "refs/heads/feature/x", "commit": c1})
	a.bad("create_branch_malformed_task_branch", "CreateBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_10", "ref": "refs/heads/kodama/work/wi/1", "commit": c1})
	a.bad("create_branch_traversal", "CreateBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_10", "ref": "refs/heads/kodama/work/../main", "commit": c1})
	a.bad("create_branch_zero_commit", "CreateBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_10", "ref": task, "commit": z40})
	a.bad("create_branch_symbolic_commit", "CreateBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_10", "ref": task, "commit": "main"})
	a.bad("create_branch_operation_omitted", "CreateBranchRequest", obj{"repositoryId": "repo_01", "ref": task, "commit": c1})
	a.bad("create_branch_unknown_field", "CreateBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_10", "ref": task, "commit": c1, "force": true})
	a.bad("create_branch_response_created_omitted", "CreateBranchResponse", obj{"ref": task, "commit": c1})
	a.bad("delete_branch_default_branch", "DeleteBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_03", "ref": "refs/heads/main", "expectedCommit": c4})
	a.bad("delete_branch_other_branch", "DeleteBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_03", "ref": "refs/heads/feature/x", "expectedCommit": c4})
	a.bad("delete_branch_tag", "DeleteBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_03", "ref": "refs/tags/v1", "expectedCommit": c4})
	a.bad("delete_branch_server_ref", "DeleteBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_03", "ref": "refs/kodama/runs/run_01/1", "expectedCommit": c4})
	a.bad("delete_branch_malformed_agent_branch", "DeleteBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_03", "ref": "refs/heads/kodama/agents/wi_1042/impl", "expectedCommit": c4})
	a.bad("delete_branch_without_expected", "DeleteBranchRequest", obj{"repositoryId": "repo_01", "operationId": "op_03", "ref": agnt, "expectedCommit": z40})
	a.bad("maintain_operation_omitted", "MaintainRequest", obj{"repositoryId": "repo_01"})
	a.bad("maintain_response_time_without_zone", "MaintainResponse", obj{"operationId": "op_06", "completedAt": "2026-10-30T11:00:00"})
	return a
}

func authorizationCases(t testing.TB) areaCases {
	a := areaCases{area: "push_authorization"}
	a.ok("authorize_create_agent_branch", "PushAuthorizeRequest", authReq())
	a.ok("authorize_multi_ref_with_options", "PushAuthorizeRequest", authReqFF())
	a.ok("authorize_seed_into_empty_repository", "PushAuthorizeRequest", authReqSeed())
	a.ok("authorize_sha256_objects", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.old", z64, "updates.0.new", c64))
	a.ok("authorize_delete_branch", "PushAuthorizeRequest", mut(t, authReq(), "updates.0", upd(agnt, c1, z40)))
	a.ok("authorize_rewrite_task_branch", "PushAuthorizeRequest", mut(t, authReqFF(), "gitFacts.0.fastForward", false))
	a.ok("authorize_server_ref_is_requestable", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.ref", "refs/kodama/runs/run_01/1", "gitFacts.0.ref", "refs/kodama/runs/run_01/1"))
	a.ok("error_too_many_updates", "ErrorResponse", errBody("too_many_updates"))
	a.ok("authorize_response_allow", "PushAuthorizeResponse", authResp())
	for _, r := range []string{"ref_protected", "not_fast_forward", "grant_scope", "attempt_inactive", "repository_archived", "seed_only", "seed_consumed", "integration_mismatch"} {
		a.ok("authorize_response_deny_"+r, "PushAuthorizeResponse", authRespDeny(r))
	}
	a.ok("authorize_response_tolerates_added_member", "PushAuthorizeResponse", mut(t, authResp(), "hint", "x"))
	a.ok("record_authorized", "PushRecord", recordBase(attemptActor(), "run_01", "authorized", entry(agnt, z40, c4, "authorized", nil)))
	a.ok("record_committing_is_authorized", "PushRecord", recordBase(attemptActor(), "run_01", "authorized", entry(agnt, z40, c4, "committing", nil)))
	a.ok("record_applied", "PushRecord", recordBase(humanActor(), nil, "applied", entry("refs/heads/feature/x", z40, c4, "applied", nil)))
	a.ok("record_partial_push_applied", "PushRecord", recordBase(attemptActor(), "run_01", "applied",
		entry(agnt, z40, c4, "applied", nil), entry(task, c1, c5, "refused", "grant_scope")))
	a.ok("record_refused", "PushRecord", recordBase(attemptActor(), "run_01", "refused", entry("refs/heads/main", c1, c2, "refused", "ref_protected")))
	a.ok("record_refused_by_settlement", "PushRecord", recordBase(attemptActor(), "run_01", "refused", entry(agnt, z40, c4, "refused", "no_commit_intent")))
	a.ok("record_unknown", "PushRecord", recordBase(attemptActor(), "run_01", "unknown", entry(agnt, z40, c4, "unknown", "journal_regressed")))
	a.ok("record_unknown_blocks_applied", "PushRecord", recordBase(attemptActor(), "run_01", "unknown",
		entry(agnt, z40, c4, "applied", nil), entry(task, c1, c5, "unknown", "conflicting_evidence")))
	a.ok("record_tolerates_added_member", "PushRecord", mut(t, recordBase(humanActor(), nil, "authorized", entry(agnt, z40, c4, "authorized", nil)), "note", "x"))
	a.ok("record_reload_preserves_each_kind", "PushRecord", recordBase(humanActor(), nil, "applied",
		entryK(task, c1, c2, "fast_forward", "applied", nil), entryK("refs/heads/feature/x", c3, c4, "rewrite", "applied", nil),
		entryK("refs/heads/feature/new", z40, c5, "create", "applied", nil), entryK("refs/tags/old", c2, z40, "delete", "applied", nil),
		entryK("refs/heads/feature/y", c1, c3, "rewrite", "refused", "ref_protected")))
	a.ok("record_list", "PushRecordList", obj{"items": []any{recordBase(humanActor(), nil, "applied", entry("refs/heads/x", z40, c4, "applied", nil))}, "nextCursor": nil})

	a.bad("authorize_unknown_field", "PushAuthorizeRequest", mut(t, authReq(), "force", true))
	a.bad("authorize_no_updates", "PushAuthorizeRequest", mut(t, authReq(), "updates", []any{}, "gitFacts", []any{}))
	a.badGo("authorize_duplicate_ref", "PushAuthorizeRequest", mut(t, authReq(), "updates.1", upd(agnt, z40, c5), "gitFacts.1", obj{"ref": agnt, "fastForward": nil, "repositoryEmpty": false}))
	a.badGo("authorize_old_equals_new", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.old", c4))
	a.bad("authorize_ref_not_a_ref", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.ref", "heads/main"))
	a.bad("authorize_ref_traversal", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.ref", "refs/heads/../kodama/x", "gitFacts.0.ref", "refs/heads/../kodama/x"))
	a.bad("authorize_ref_with_space", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.ref", "refs/heads/a b", "gitFacts.0.ref", "refs/heads/a b"))
	a.bad("authorize_ref_double_slash", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.ref", "refs/heads//x", "gitFacts.0.ref", "refs/heads//x"))
	a.bad("authorize_ref_trailing_slash", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.ref", "refs/heads/x/", "gitFacts.0.ref", "refs/heads/x/"))
	a.bad("authorize_ref_lock_suffix", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.ref", "refs/heads/x.lock", "gitFacts.0.ref", "refs/heads/x.lock"))
	a.bad("authorize_ref_control_character", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.ref", "refs/heads/a\u0001b", "gitFacts.0.ref", "refs/heads/a\u0001b"))
	a.bad("authorize_ref_wildcard", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.ref", "refs/heads/*", "gitFacts.0.ref", "refs/heads/*"))
	a.bad("authorize_ref_head", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.ref", "HEAD", "gitFacts.0.ref", "HEAD"))
	a.bad("authorize_commit_uppercase", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.new", strings.ToUpper("abcdefabcdefabcdefabcdefabcdefabcdefabcd")))
	a.bad("authorize_commit_short", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.new", "abc123"))
	a.bad("authorize_commit_39_characters", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.new", c4[:39]))
	a.bad("authorize_commit_not_hex", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.new", strings.Repeat("g", 40)))
	a.bad("authorize_commit_symbolic", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.new", "HEAD"))
	a.badGo("authorize_mixed_object_id_lengths", "PushAuthorizeRequest", mut(t, authReq(), "updates.0.new", c64))
	a.bad("authorize_facts_omitted", "PushAuthorizeRequest", mut(t, authReq(), "gitFacts", nil))
	a.bad("authorize_options_null", "PushAuthorizeRequest", mut(t, authReq(), "pushOptions", null))
	a.badGo("authorize_fact_count_differs", "PushAuthorizeRequest", mut(t, authReqFF(), "gitFacts", authReqFF()["gitFacts"].([]any)[:2]))
	a.badGo("authorize_fact_ref_differs", "PushAuthorizeRequest", mut(t, authReq(), "gitFacts.0.ref", "refs/heads/other"))
	a.badGo("authorize_fast_forward_on_create", "PushAuthorizeRequest", mut(t, authReq(), "gitFacts.0.fastForward", true))
	a.badGo("authorize_fast_forward_unknown_for_update", "PushAuthorizeRequest", mut(t, authReqFF(), "gitFacts.0.fastForward", null))
	a.badGo("authorize_empty_repository_with_update_of_existing_ref", "PushAuthorizeRequest", mut(t, authReqFF(), "gitFacts.0.repositoryEmpty", true, "gitFacts.1.repositoryEmpty", true, "gitFacts.2.repositoryEmpty", true))
	a.badGo("authorize_empty_repository_inconsistent", "PushAuthorizeRequest", mut(t, authReqFF(), "gitFacts.1.repositoryEmpty", true))
	a.bad("authorize_fact_unknown_field", "PushAuthorizeRequest", mut(t, authReq(), "gitFacts.0.force", true))
	a.bad("authorize_option_with_newline", "PushAuthorizeRequest", mut(t, authReq(), "pushOptions", []any{"a\nb"}))
	a.bad("authorize_too_many_options", "PushAuthorizeRequest", mut(t, authReq(), "pushOptions", func() []any {
		out := make([]any, 17)
		for i := range out {
			out[i] = "o"
		}
		return out
	}()))
	a.bad("authorize_option_too_long", "PushAuthorizeRequest", mut(t, authReq(), "pushOptions", []any{strings.Repeat("x", 1025)}))
	a.bad("authorize_receive_id_omitted", "PushAuthorizeRequest", mut(t, authReq(), "receiveId", nil))
	a.bad("authorize_grant_not_an_id", "PushAuthorizeRequest", mut(t, authReq(), "grantId", "gg 01"))
	a.bad("response_deny_without_reason", "PushAuthorizeResponse", mut(t, authRespDeny("ref_protected"), "updates.0.reason", null))
	a.bad("response_deny_unknown_reason", "PushAuthorizeResponse", mut(t, authRespDeny("ref_protected"), "updates.0.reason", "because"))
	a.bad("response_deny_settlement_reason", "PushAuthorizeResponse", mut(t, authRespDeny("ref_protected"), "updates.0.reason", "no_commit_intent"))
	a.bad("response_allow_with_reason", "PushAuthorizeResponse", mut(t, authResp(), "updates.0.reason", "grant_scope"))
	a.bad("response_unknown_verdict", "PushAuthorizeResponse", mut(t, authResp(), "updates.0.verdict", "maybe"))
	a.bad("response_no_verdicts", "PushAuthorizeResponse", mut(t, authResp(), "updates", []any{}))
	a.bad("response_deadline_omitted", "PushAuthorizeResponse", mut(t, authResp(), "deadline", nil))
	a.badGo("response_duplicate_ref", "PushAuthorizeResponse", mut(t, authResp(), "updates.1", obj{"ref": agnt, "verdict": "allow", "reason": nil}))
	a.badGo("record_status_not_derived_applied", "PushRecord", recordBase(attemptActor(), "run_01", "applied", entry(agnt, z40, c4, "committing", nil)))
	a.badGo("record_status_not_derived_unknown_blocks", "PushRecord", recordBase(attemptActor(), "run_01", "applied", entry(agnt, z40, c4, "applied", nil), entry(task, c1, c5, "unknown", "conflicting_evidence")))
	a.badGo("record_status_refused_with_applied_entry", "PushRecord", recordBase(attemptActor(), "run_01", "refused", entry(agnt, z40, c4, "applied", nil)))
	a.bad("record_refused_entry_without_reason", "PushRecord", recordBase(attemptActor(), "run_01", "refused", entry(agnt, z40, c4, "refused", nil)))
	a.bad("record_refused_entry_unknown_reason", "PushRecord", recordBase(attemptActor(), "run_01", "refused", entry(agnt, z40, c4, "refused", "nope")))
	a.bad("record_refused_entry_unknown_reason_code", "PushRecord", recordBase(attemptActor(), "run_01", "unknown", entry(agnt, z40, c4, "unknown", "ref_protected")))
	a.bad("record_applied_entry_with_reason", "PushRecord", recordBase(attemptActor(), "run_01", "applied", entry(agnt, z40, c4, "applied", "grant_scope")))
	a.bad("record_entry_kind_omitted", "PushRecord", mut(t, recordBase(humanActor(), nil, "authorized", entry(agnt, z40, c4, "authorized", nil)), "entries.0.kind", nil))
	a.bad("record_entry_kind_null", "PushRecord", mut(t, recordBase(humanActor(), nil, "authorized", entry(agnt, z40, c4, "authorized", nil)), "entries.0.kind", null))
	a.bad("record_entry_kind_unknown", "PushRecord", recordBase(humanActor(), nil, "authorized", entryK(agnt, z40, c4, "force", "authorized", nil)))
	a.bad("record_entry_create_with_old_value", "PushRecord", recordBase(humanActor(), nil, "authorized", entryK(agnt, c1, c4, "create", "authorized", nil)))
	a.bad("record_entry_delete_with_new_value", "PushRecord", recordBase(humanActor(), nil, "authorized", entryK(agnt, c1, c4, "delete", "authorized", nil)))
	a.bad("record_entry_fast_forward_from_nothing", "PushRecord", recordBase(humanActor(), nil, "authorized", entryK(agnt, z40, c4, "fast_forward", "authorized", nil)))
	a.bad("record_entry_rewrite_to_nothing", "PushRecord", recordBase(humanActor(), nil, "authorized", entryK(agnt, c1, z40, "rewrite", "authorized", nil)))
	a.bad("record_entry_update_classified_create", "PushRecord", recordBase(humanActor(), nil, "authorized", entryK(agnt, c1, c4, "create", "authorized", nil)))
	a.bad("record_unknown_entry_state", "PushRecord", recordBase(attemptActor(), "run_01", "applied", entry(agnt, z40, c4, "done", nil)))
	a.bad("record_workload_without_origin", "PushRecord", recordBase(attemptActor(), null, "authorized", entry(agnt, z40, c4, "authorized", nil)))
	a.badGo("record_origin_differs_from_actor_run", "PushRecord", recordBase(attemptActor(), "run_99", "authorized", entry(agnt, z40, c4, "authorized", nil)))
	a.bad("record_user_with_origin", "PushRecord", recordBase(humanActor(), "run_01", "authorized", entry(agnt, z40, c4, "authorized", nil)))
	a.bad("record_no_entries", "PushRecord", recordBase(humanActor(), nil, "authorized"))
	a.badGo("record_duplicate_entry_ref", "PushRecord", recordBase(humanActor(), nil, "authorized", entry(agnt, z40, c4, "authorized", nil), entry(agnt, z40, c5, "authorized", nil)))
	return a
}

func commitIntentCases(t testing.TB) areaCases {
	a := areaCases{area: "commit_intent"}
	req := func() obj { return obj{"receiveId": "rcv_8f2c", "updates": []any{upd(agnt, z40, c4)}} }
	a.ok("commit_intent_request", "CommitIntentRequest", req())
	a.ok("commit_intent_request_multi_ref_transaction", "CommitIntentRequest", mut(t, req(), "updates.1", upd(task, c1, c5)))
	a.ok("commit_intent_response", "CommitIntentResponse", obj{"committing": true})
	a.ok("commit_intent_response_tolerates_added_member", "CommitIntentResponse", obj{"committing": true, "deadline": "2026-10-30T10:10:00Z"})
	a.ok("error_push_settled", "ErrorResponse", errBody("push_settled"))
	a.ok("error_attempt_inactive", "ErrorResponse", errBody("attempt_inactive"))
	a.ok("error_receive_expired", "ErrorResponse", errBody("receive_expired"))
	a.ok("outcome_request_applied", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": []any{upd(agnt, z40, c4)}})
	a.ok("outcome_request_nothing_applied", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": []any{}})
	a.ok("outcome_request_journal_position", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": []any{upd(agnt, z40, c4)}, "journalPosition": 7})
	a.ok("outcome_request_position_and_sequence", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": []any{upd(agnt, z40, c4)}, "journalPosition": 7, "journalSequence": 12})
	a.ok("outcome_request_journal_sequence", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": []any{upd(agnt, z40, c4)}, "journalSequence": 12})
	a.ok("outcome_response", "OutcomeResponse", obj{"pushId": "push_01", "status": "applied"})

	a.bad("commit_intent_unknown_field", "CommitIntentRequest", mut(t, req(), "pushId", "push_01"))
	a.bad("commit_intent_receive_omitted", "CommitIntentRequest", mut(t, req(), "receiveId", nil))
	a.bad("commit_intent_no_updates", "CommitIntentRequest", mut(t, req(), "updates", []any{}))
	a.bad("commit_intent_updates_null", "CommitIntentRequest", mut(t, req(), "updates", null))
	a.badGo("commit_intent_duplicate_ref", "CommitIntentRequest", mut(t, req(), "updates.1", upd(agnt, z40, c5)))
	a.bad("commit_intent_malformed_ref", "CommitIntentRequest", mut(t, req(), "updates.0.ref", "refs/heads/a..b"))
	a.bad("commit_intent_malformed_commit", "CommitIntentRequest", mut(t, req(), "updates.0.new", "xyz"))
	a.badGo("commit_intent_no_change", "CommitIntentRequest", mut(t, req(), "updates.0.new", z40))
	a.bad("commit_intent_response_not_committing", "CommitIntentResponse", obj{"committing": false})
	a.bad("commit_intent_response_omitted", "CommitIntentResponse", obj{})
	a.bad("outcome_journal_sequence_zero", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": []any{}, "journalSequence": 0})
	a.bad("outcome_journal_position_zero", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": []any{}, "journalPosition": 0})
	a.bad("outcome_journal_position_null", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": []any{}, "journalPosition": null})
	a.bad("outcome_journal_position_string", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": []any{}, "journalPosition": "7"})
	a.bad("outcome_journal_sequence_null", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": []any{}, "journalSequence": null})
	a.bad("outcome_unknown_field", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": []any{}, "status": "ok"})
	a.bad("outcome_applied_null", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": null})
	a.badGo("outcome_applied_duplicate", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": []any{upd(agnt, z40, c4), upd(agnt, z40, c4)}})
	a.bad("outcome_malformed_ref", "OutcomeRequest", obj{"receiveId": "rcv_8f2c", "applied": []any{upd("agnt", z40, c4)}})
	a.bad("outcome_response_unknown_status", "OutcomeResponse", obj{"pushId": "push_01", "status": "done"})
	return a
}

func journalCases(t testing.TB) areaCases {
	a := areaCases{area: "ref_journal"}
	a.ok("journal_prepared_receive", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z40, c4, z40, "prepared", 1))
	a.ok("journal_prepared_forced_update_observed_differs", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z40, c4, c1, "prepared", 2))
	a.ok("journal_prepared_server_operation", "JournalEntry", jEntry(nil, "op_01", "refs/kodama/runs/run_01/1", z40, c1, z40, "prepared", 3))
	a.ok("journal_committed", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z40, c4, nil, "committed", 4))
	a.ok("journal_aborted", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z40, c4, nil, "aborted", 5))
	a.ok("journal_delete_prepared", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, c1, z40, c1, "prepared", 6))
	a.ok("journal_sha256", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z64, c64, z64, "prepared", 7))

	a.bad("journal_unknown_field", "JournalEntry", mut(t, jEntry("rcv_8f2c", nil, agnt, z40, c4, z40, "prepared", 1), "at", "2026-10-30T10:00:00Z"))
	a.bad("journal_receive_and_operation", "JournalEntry", jEntry("rcv_8f2c", "op_01", agnt, z40, c4, z40, "prepared", 1))
	a.bad("journal_neither_receive_nor_operation", "JournalEntry", jEntry(nil, nil, agnt, z40, c4, z40, "prepared", 1))
	a.bad("journal_prepared_without_observed", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z40, c4, nil, "prepared", 1))
	a.bad("journal_observed_omitted", "JournalEntry", mut(t, jEntry("rcv_8f2c", nil, agnt, z40, c4, z40, "prepared", 1), "observed", nil))
	a.bad("journal_committed_with_observed", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z40, c4, z40, "committed", 4))
	a.bad("journal_aborted_with_observed", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z40, c4, c1, "aborted", 5))
	a.badGo("journal_observed_other_length", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z40, c4, z64, "prepared", 1))
	a.bad("journal_observed_uppercase", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z40, c4, strings.ToUpper("abcdefabcdefabcdefabcdefabcdefabcdefabcd"), "prepared", 1))
	a.bad("journal_sequence_zero", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z40, c4, z40, "prepared", 0))
	a.bad("journal_sequence_negative", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z40, c4, z40, "prepared", -4))
	a.bad("journal_sequence_beyond_safe_integer", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z40, c4, z40, "prepared", 9007199254740992))
	a.bad("journal_unknown_state", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, z40, c4, z40, "applied", 1))
	a.bad("journal_malformed_ref", "JournalEntry", jEntry("rcv_8f2c", nil, "refs/heads/a..b", z40, c4, z40, "prepared", 1))
	a.badGo("journal_no_change", "JournalEntry", jEntry("rcv_8f2c", nil, agnt, c4, c4, c4, "prepared", 1))
	a.bad("journal_receive_not_an_id", "JournalEntry", jEntry("rcv 8f2c", nil, agnt, z40, c4, z40, "prepared", 1))
	a.rawBad("journal_sequence_float", "JournalEntry", `{"receiveId":"rcv_8f2c","operationId":null,"ref":"refs/heads/x","old":"0000000000000000000000000000000000000000","new":"1111111111111111111111111111111111111111","observed":"0000000000000000000000000000000000000000","state":"prepared","sequence":1.0}`)
	return a
}

func sourceSetCases(t testing.TB) areaCases {
	a := areaCases{area: "source_set"}
	src := func(repo, commit, path, access string, branch any) obj {
		return obj{"repositoryId": repo, "commit": commit, "path": path, "access": access, "branch": branch}
	}
	set := func(sources ...any) obj { return obj{"sources": sources} }
	a.ok("source_set_two_repositories", "SourceSet", sourceSetBase())
	a.ok("source_set_single_root", "SourceSet", set(src("repo_01", c1, ".", "write", task)))
	a.ok("source_set_single_root_read", "SourceSet", set(src("repo_01", c1, ".", "read", nil)))
	a.ok("source_set_single_named", "SourceSet", set(src("repo_01", c1, "backend", "read", nil)))
	a.ok("source_set_nested_paths", "SourceSet", set(src("repo_01", c1, "libs/core", "read", nil), src("repo_02", c2, "libs/web", "read", nil)))
	a.ok("source_set_prefix_names_do_not_overlap", "SourceSet", set(src("repo_01", c1, "a", "read", nil), src("repo_02", c2, "a-b", "read", nil), src("repo_03", c3, "ab", "read", nil)))
	a.ok("source_set_same_repository_twice", "SourceSet", set(src("repo_01", c2, "new", "read", nil), src("repo_01", c1, "old", "read", nil)))
	a.ok("source_set_sha256_commit", "SourceSet", set(src("repo_01", c64, "backend", "read", nil)))
	a.ok("source_set_dotted_names", "SourceSet", set(src("repo_01", c1, "my.app/v1.2", "read", nil)))
	a.ok("source_set_run_branch_writer", "SourceSet", set(src("repo_01", c1, "backend", "write", "refs/heads/kodama/runs/run_01/impl")))

	a.bad("source_set_unknown_field", "SourceSet", mut(t, sourceSetBase(), "mode", "shallow"))
	a.bad("source_unknown_field", "SourceSet", mut(t, sourceSetBase(), "sources.0.depth", 1))
	a.bad("source_set_empty", "SourceSet", set())
	a.bad("source_set_null", "SourceSet", obj{"sources": null})
	a.bad("source_set_too_many", "SourceSet", func() obj {
		out := make([]any, 33)
		for i := range out {
			out[i] = src("repo_01", c1, "p"+strconv.Itoa(1000+i), "read", nil)
		}
		return set(out...)
	}())
	a.bad("source_commit_short", "SourceSet", mut(t, sourceSetBase(), "sources.0.commit", "abc123"))
	a.bad("source_commit_uppercase", "SourceSet", mut(t, sourceSetBase(), "sources.0.commit", strings.ToUpper("abcdefabcdefabcdefabcdefabcdefabcdefabcd")))
	a.bad("source_commit_branch_name", "SourceSet", mut(t, sourceSetBase(), "sources.0.commit", "main"))
	a.bad("source_commit_zero", "SourceSet", mut(t, sourceSetBase(), "sources.0.commit", z40))
	a.bad("source_commit_omitted", "SourceSet", mut(t, sourceSetBase(), "sources.0.commit", nil))
	a.bad("source_path_absolute", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", "/backend"))
	a.bad("source_path_traversal", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", "../backend"))
	a.bad("source_path_inner_traversal", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", "a/../b"))
	a.bad("source_path_only_dotdot", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", ".."))
	a.bad("source_path_dot_prefix", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", "./backend"))
	a.bad("source_path_inner_dot", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", "a/./b"))
	a.bad("source_path_double_slash", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", "a//b"))
	a.bad("source_path_trailing_slash", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", "backend/"))
	a.bad("source_path_backslash", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", "a\\b"))
	a.bad("source_path_dot_git", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", ".git"))
	a.bad("source_path_inside_dot_git", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", "backend/.git/hooks"))
	a.bad("source_path_hidden", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", ".hidden"))
	a.bad("source_path_empty", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", ""))
	a.bad("source_path_space", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", "my app"))
	a.bad("source_path_control_character", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", "a\u0000b"))
	a.bad("source_path_dotdot_in_name", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", "a..b"))
	a.bad("source_path_too_long", "SourceSet", mut(t, sourceSetBase(), "sources.0.path", strings.Repeat("a", 256)))
	a.badGo("source_paths_duplicate", "SourceSet", set(src("repo_01", c1, "app", "read", nil), src("repo_02", c2, "app", "read", nil)))
	a.badGo("source_paths_overlap_prefix_directory", "SourceSet", set(src("repo_01", c1, "app", "read", nil), src("repo_02", c2, "app/web", "read", nil)))
	a.badGo("source_paths_overlap_deep", "SourceSet", set(src("repo_01", c1, "a", "read", nil), src("repo_02", c2, "a-b", "read", nil), src("repo_03", c3, "a/b/c", "read", nil)))
	a.bad("source_root_with_other_path", "SourceSet", set(src("repo_01", c1, ".", "read", nil), src("repo_02", c2, "web", "read", nil)))
	a.bad("source_root_twice", "SourceSet", set(src("repo_01", c1, ".", "read", nil), src("repo_02", c2, ".", "read", nil)))
	a.badGo("source_paths_unordered", "SourceSet", set(src("repo_02", c5, "frontend", "read", nil), src("repo_01", c1, "backend", "write", task)))
	a.bad("source_write_without_branch", "SourceSet", mut(t, sourceSetBase(), "sources.0.branch", null))
	a.bad("source_read_with_branch", "SourceSet", mut(t, sourceSetBase(), "sources.1.branch", task))
	a.bad("source_branch_omitted", "SourceSet", mut(t, sourceSetBase(), "sources.1.branch", nil))
	a.bad("source_write_branch_is_tag", "SourceSet", mut(t, sourceSetBase(), "sources.0.branch", "refs/tags/v1"))
	a.bad("source_write_branch_not_a_ref", "SourceSet", mut(t, sourceSetBase(), "sources.0.branch", "main"))
	a.bad("source_write_branch_traversal", "SourceSet", mut(t, sourceSetBase(), "sources.0.branch", "refs/heads/../x"))
	a.bad("source_write_branch_server_ref", "SourceSet", mut(t, sourceSetBase(), "sources.0.branch", "refs/kodama/runs/run_01/1"))
	a.bad("source_unknown_access", "SourceSet", mut(t, sourceSetBase(), "sources.0.access", "admin"))
	a.bad("source_repository_not_an_id", "SourceSet", mut(t, sourceSetBase(), "sources.0.repositoryId", "repo/01"))
	return a
}

func triggerCases(t testing.TB) areaCases {
	a := areaCases{area: "trigger_config"}
	a.ok("trigger_default_branch", "TriggerConfig", triggerBase())
	a.ok("trigger_origin_filter_omitted_defaults_to_true", "TriggerConfig", mut(t, triggerBase(), "excludeOriginRuns", nil))
	a.ok("trigger_includes_origin_runs", "TriggerConfig", mut(t, triggerBase(), "excludeOriginRuns", false))
	a.ok("trigger_task_and_agent_branches", "TriggerConfig", mut(t, triggerBase(), "refPatterns", []any{"refs/heads/kodama/work/*", "refs/heads/kodama/agents/*"}))
	a.ok("trigger_tags", "TriggerConfig", mut(t, triggerBase(), "refPatterns", []any{"refs/tags/v1", "refs/tags/release/*"}))
	a.ok("trigger_disabled", "TriggerConfig", mut(t, triggerBase(), "enabled", false))

	a.bad("trigger_unknown_field", "TriggerConfig", mut(t, triggerBase(), "debounce", 5))
	a.bad("trigger_origin_filter_null", "TriggerConfig", mut(t, triggerBase(), "excludeOriginRuns", null))
	a.bad("trigger_origin_filter_string", "TriggerConfig", mut(t, triggerBase(), "excludeOriginRuns", "true"))
	a.bad("trigger_enabled_omitted", "TriggerConfig", mut(t, triggerBase(), "enabled", nil))
	a.bad("trigger_no_patterns", "TriggerConfig", mut(t, triggerBase(), "refPatterns", []any{}))
	a.bad("trigger_patterns_null", "TriggerConfig", mut(t, triggerBase(), "refPatterns", null))
	a.bad("trigger_pattern_refs_kodama", "TriggerConfig", mut(t, triggerBase(), "refPatterns", []any{"refs/kodama/*"}))
	a.bad("trigger_pattern_retained_ref", "TriggerConfig", mut(t, triggerBase(), "refPatterns", []any{"refs/kodama/runs/run_01/1"}))
	a.bad("trigger_pattern_all_branches", "TriggerConfig", mut(t, triggerBase(), "refPatterns", []any{"refs/heads/*"}))
	a.bad("trigger_pattern_branch_name", "TriggerConfig", mut(t, triggerBase(), "refPatterns", []any{"main"}))
	a.bad("trigger_pattern_traversal", "TriggerConfig", mut(t, triggerBase(), "refPatterns", []any{"refs/heads/../x"}))
	a.bad("trigger_pattern_inner_wildcard", "TriggerConfig", mut(t, triggerBase(), "refPatterns", []any{"refs/heads/a/*/b"}))
	a.badGo("trigger_pattern_repeated", "TriggerConfig", mut(t, triggerBase(), "refPatterns", []any{"refs/heads/main", "refs/heads/main"}))
	a.bad("trigger_too_many_patterns", "TriggerConfig", mut(t, triggerBase(), "refPatterns", func() []any {
		out := make([]any, 33)
		for i := range out {
			out[i] = "refs/heads/p" + strconv.Itoa(i)
		}
		return out
	}()))
	a.bad("trigger_repository_not_an_id", "TriggerConfig", mut(t, triggerBase(), "repositoryId", "repo/01"))
	a.bad("trigger_definition_omitted", "TriggerConfig", mut(t, triggerBase(), "definitionId", nil))
	return a
}
