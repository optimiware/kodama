package runtimev3

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// The fixtures of this package are produced from typed bases (valid by
// construction) and path mutations, then published as fixtures/cases.json. The
// schema judges every published payload independently of the Go code, and a
// drift test requires the checked-in file to equal what the bases produce
// (regenerate with KODAMA_UPDATE_GOLDEN=1).

const (
	c40a = "1111111111111111111111111111111111111111"
	c40b = "2222222222222222222222222222222222222222"
	c40c = "3333333333333333333333333333333333333333"
	c40d = "4444444444444444444444444444444444444444"
	c64a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dg   = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	dgE  = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	dgF  = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	img  = "registry.example.invalid/test-tools@sha256:4444444444444444444444444444444444444444444444444444444444444444"

	gitBase  = "https://git.example.invalid/git"
	urlBack  = gitBase + "/t_acme/p_inventory/backend.git"
	urlFront = gitBase + "/t_acme/p_inventory/frontend.git"

	agentWI  = "refs/heads/kodama/agents/wi_1042/run_01/*"
	agentRun = "refs/heads/kodama/runs/run_01/*"
	taskWI   = "refs/heads/kodama/work/wi_1042"
	runOut   = "refs/heads/kodama/runs/run_01/out"
)

type obj = map[string]any

func tok(aud string) obj {
	return obj{"token": "rt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "tokenType": "Bearer", "audience": aud,
		"expiresAt": "2026-10-30T10:15:00Z", "maxExpiresAt": "2026-10-30T12:00:00Z"}
}

func limits() obj {
	return obj{"timeoutSeconds": 1200, "maxOutputBytes": 104857600, "maxOutputFiles": 100, "maxDiagnosticBytes": 10485760}
}

// spec describes one source of a workspace fixture.
type spec struct {
	repo, url, path, access, branch string
	commit, head                    string
	tip                             string // expectedTip, "" for null
}

func (s spec) obj() obj {
	var branch, tip any = nil, nil
	if s.branch != "" {
		branch = s.branch
	}
	if s.tip != "" {
		tip = s.tip
	}
	return obj{"repositoryId": s.repo, "commit": s.commit, "path": s.path, "access": s.access, "branch": branch,
		"repositoryUrl": s.url, "headCommit": s.head, "expectedTip": tip}
}

func backWrite(head, tip string) spec {
	return spec{repo: "repo_01", url: urlBack, path: "backend", access: "write", branch: taskWI, commit: c40a, head: head, tip: tip}
}
func frontRead() spec {
	return spec{repo: "repo_02", url: urlFront, path: "frontend", access: "read", commit: c40b, head: c40b}
}

// workspace builds a source-backed workspace; the set digest and the
// preparation receipts are derived from the specs, as run admission would.
func workspace(access string, specs []spec, lineage ...any) obj {
	sources := make([]any, len(specs))
	set := make([]gitv1.Source, len(specs))
	prepared := make([]any, len(specs))
	for i, s := range specs {
		sources[i] = s.obj()
		var branch *string
		if s.branch != "" {
			b := s.branch
			branch = &b
		}
		set[i] = gitv1.Source{RepositoryID: s.repo, Commit: s.commit, Path: s.path, Access: gitv1.SourceAccess(s.access), Branch: branch}
		prepared[i] = obj{"path": s.path, "commit": s.commit, "treeId": c40d}
	}
	digest, err := gitv1.SourceSet{Sources: set}.Digest()
	if err != nil {
		panic(err)
	}
	return obj{"access": access, "mountPath": "/workspace", "sources": sources, "sourceSetDigest": digest,
		"preparation": obj{"sources": prepared, "receiptDigest": dgE}, "lineage": append([]any{}, lineage...)}
}

func freeWorkspace() obj {
	return obj{"access": "none", "mountPath": "/workspace", "sources": []any{}, "sourceSetDigest": nil, "preparation": nil, "lineage": []any{}}
}

func commitOut(repo, ref, commit, push string) obj {
	return obj{"repositoryId": repo, "ref": ref, "commit": commit, "pushId": push}
}

func lineage(step, attempt, access, at, state string, ignored bool, published ...any) obj {
	return obj{"stepId": step, "attemptId": attempt, "access": access, "beganAt": at, "workspaceState": state, "ignoredPresent": ignored, "published": append([]any{}, published...)}
}

func access(repo string, update ...string) obj {
	u := make([]any, len(update))
	for i, p := range update {
		u[i] = p
	}
	return obj{"repositoryId": repo, "update": u}
}

func git(repos ...any) obj {
	return obj{"grantId": "gg_01", "baseUrl": gitBase, "credentialFile": "/kodama/git/credential", "caFile": nil, "repositories": append([]any{}, repos...),
		"runtimeGrants": obj{"seed": nil, "integration": nil}}
}

const (
	seedFile        = "/kodama/git/seed-credential"
	integrationFile = "/kodama/git/integration-credential"
)

// gitI is the delivery of a run whose source set has a write source: the
// integration grant is advertised.
func gitI(repos ...any) obj {
	g := git(repos...)
	g["runtimeGrants"] = obj{"seed": nil, "integration": obj{"credentialFile": integrationFile}}
	return g
}

// gitSeed is the delivery of a provisioning run without sources: no static grant
// (none can name a repository that does not exist yet), the seed capability.
func gitSeed() obj {
	return obj{"grantId": nil, "baseUrl": gitBase, "credentialFile": nil, "caFile": nil, "repositories": []any{},
		"runtimeGrants": obj{"seed": obj{"credentialFile": seedFile}, "integration": nil}}
}

func originWI() obj {
	return obj{"kind": "work_item", "workItemId": "wi_1042", "pushId": nil, "repositoryId": nil, "ref": nil}
}
func originPush() obj {
	return obj{"kind": "push", "workItemId": nil, "pushId": "pu_01", "repositoryId": "repo_01", "ref": "refs/heads/main"}
}
func originOf(kind string) obj {
	return obj{"kind": kind, "workItemId": nil, "pushId": nil, "repositoryId": nil, "ref": nil}
}

func workItem() obj {
	return obj{"id": "wi_1042", "contentRevision": 2, "spec": "# Plan\nAdd a scoped review flow.",
		"acceptanceCriteria": []any{obj{"id": "AC-1", "text": "A reviewer resolves an exception with an audit record."}}}
}

func ctxBase(step, attempt string, origin obj, wi any, ws obj, g any) obj {
	return obj{
		"contextVersion": 3, "runModel": "run_shared", "tenantId": "t_acme", "projectId": "p_inventory", "runId": "run_01",
		"stepId": step, "attemptId": attempt, "actionKey": "run_01." + step,
		"definition": obj{"id": "pd_03", "digest": dg}, "origin": origin, "workItem": wi, "designBaseline": nil, "profile": nil,
		"command":   obj{"image": img, "command": []any{"/bin/sh", "-ec"}, "args": []any{"cd /workspace/backend && go test ./..."}, "env": obj{"CI": "true"}},
		"workspace": ws, "git": g,
		"endpoints": obj{}, "egress": []any{},
		"run":     obj{"startedAt": "2026-10-30T10:00:00Z", "deadlineAt": "2026-10-30T11:00:00Z"},
		"policy":  obj{"id": "run-v1", "digest": dgF},
		"inputs":  obj{},
		"outputs": []any{obj{"name": "report", "type": "artifact", "path": "/kodama/outputs/tests.json"}},
		"secrets": []any{}, "environment": nil, "limits": limits(),
	}
}

// ctxWriter is the writer of a work-item run over two repositories: backend is
// written to the task branch, frontend is read.
func ctxWriter() obj {
	return ctxBase("generate", "att_01", originWI(), workItem(),
		workspace("write", []spec{backWrite(c40a, c40a), frontRead()}),
		gitI(access("repo_01", agentWI, taskWI), access("repo_02", agentWI)))
}

// ctxReaderAfterWriter is the next attempt: a reader that sees the commit the
// writer published to the task branch.
func ctxReaderAfterWriter() obj {
	return ctxBase("test", "att_02", originWI(), workItem(),
		workspace("read", []spec{backWrite(c40c, ""), frontRead()},
			lineage("generate", "att_01", "write", "2026-10-30T10:01:00Z", "clean", false, commitOut("repo_01", taskWI, c40c, "pu_07"))),
		gitI(access("repo_01", agentWI), access("repo_02", agentWI)))
}

func ctxReaderFirst() obj {
	return ctxBase("test", "att_01", originWI(), workItem(),
		workspace("read", []spec{backWrite(c40a, ""), frontRead()}),
		gitI(access("repo_01", agentWI), access("repo_02", agentWI)))
}

// ctxPushReader is a push-originated run: no work item, one repository checked
// out at the root, the pushed commit pinned.
func ctxPushReader() obj {
	root := spec{repo: "repo_01", url: urlBack, path: ".", access: "read", commit: c40a, head: c40a}
	return ctxBase("verify", "att_01", originPush(), nil, workspace("read", []spec{root}), git(access("repo_01", agentRun)))
}

func ctxPushWriter() obj {
	root := spec{repo: "repo_01", url: urlBack, path: ".", access: "write", branch: runOut, commit: c40a, head: c40a, tip: c40a}
	return ctxBase("fix", "att_01", originPush(), nil, workspace("write", []spec{root}), gitI(access("repo_01", agentRun, runOut)))
}

func ctxManualReader() obj {
	return ctxBase("verify", "att_01", originOf("manual"), nil, workspace("read", []spec{frontRead()}), git(access("repo_02", agentRun)))
}

// ctxProvision is a provisioning run: no source set yet, a Git credential for the
// repositories it creates.
func ctxProvision() obj {
	return ctxBase("seed", "att_01", originOf("provision"), nil, freeWorkspace(), gitSeed())
}

// ctxFree is a source-free run with no Git credential (an internal cleanup run).
func ctxFree() obj {
	return ctxBase("teardown", "att_01", originOf("manual"), nil, freeWorkspace(), nil)
}

// ---- resource subjects ----

func prepSource(repo, url, commit, path string) obj {
	return obj{"repositoryId": repo, "repositoryUrl": url, "commit": commit, "path": path}
}

func prepGit(repos ...any) obj {
	return obj{"grantId": "gg_00", "baseUrl": gitBase, "credentialFile": "/kodama/git/credential", "caFile": nil, "repositories": append([]any{}, repos...)}
}

// prepCtx is the preparation of the run of ctxWriter: one checkout of both sources.
func prepCtx() obj {
	return obj{"contextVersion": 3, "resourceKind": "preparation", "tenantId": "t_acme", "projectId": "p_inventory", "runId": "run_01", "resourceKey": "preparation",
		"preparation": obj{
			"sources":   []any{prepSource("repo_01", urlBack, c40a, "backend"), prepSource("repo_02", urlFront, c40b, "frontend")},
			"mountPath": "/workspace", "timeoutSeconds": 600, "maxBytes": 1073741824, "maxAttempts": 2, "deadlineAt": "2026-10-30T11:00:00Z",
			"git": prepGit("repo_01", "repo_02")},
		"service": nil, "policy": obj{"id": "run-v1", "digest": dgF}}
}

func prepRootCtx() obj {
	return mutObj(prepCtx(), "preparation.sources", []any{prepSource("repo_01", urlBack, c40a, ".")}, "preparation.git.repositories", []any{"repo_01"})
}

// svcCtx is a v2 ServiceContext served on the v3 wire.
func svcCtx() obj {
	return obj{"contextVersion": 3, "resourceKind": "service", "tenantId": "t_acme", "projectId": "p_inventory", "runId": "run_01", "resourceKey": "service.db",
		"preparation": nil,
		"service": obj{"name": "db", "role": "general", "command": obj{"image": img, "command": []any{"/usr/local/bin/docker-entrypoint.sh", "postgres"}, "args": []any{},
			"env": obj{"POSTGRES_USER": "kodama", "POSTGRES_DB": "app", "POSTGRES_PASSWORD_FILE": "/kodama/secrets/postgres-password"}},
			"secrets":    []any{obj{"ref": "secret://project/test-db-password", "version": 3, "name": "postgres-password", "delivery": "file"}},
			"deadlineAt": "2026-10-30T11:00:00Z"},
		"policy": obj{"id": "run-v1", "digest": dgF}}
}

func resTok() obj {
	return obj{"token": "rt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "tokenType": "Bearer", "audience": "kodama-runtime-resource",
		"expiresAt": "2026-10-30T10:15:00Z", "maxExpiresAt": "2026-10-30T11:00:00Z"}
}

func resAdmitResp(ctx obj) obj { return obj{"token": resTok(), "context": ctx} }

func receiptSource(path, commit string) obj {
	return obj{"path": path, "commit": commit, "treeId": c40d}
}

func receiptReq() obj {
	return obj{"sources": []any{receiptSource("backend", c40a), receiptSource("frontend", c40b)},
		"hooksDisabled": true, "submodules": "skipped", "credentialsRemoved": true, "attempts": 1}
}

func seedGrant() obj {
	return obj{"grantId": "gg_seed01", "audience": "kodama-git", "tenantId": "t_acme", "projectId": "p_inventory",
		"actor": obj{"type": "workload", "id": "att_01", "runId": "run_01", "attemptId": "att_01"}, "kind": "seed",
		"repositories": []any{obj{"id": "repo_03", "read": true, "update": []any{"refs/heads/main"}, "seed": true, "integration": nil}},
		"issuedAt":     "2026-10-30T10:00:00Z", "expiresAt": "2026-10-30T10:10:00Z"}
}

func integrationGrant() obj {
	return obj{"grantId": "gg_int01", "audience": "kodama-git", "tenantId": "t_acme", "projectId": "p_inventory",
		"actor": obj{"type": "workload", "id": "att_02", "runId": "run_01", "attemptId": "att_02"}, "kind": "integration",
		"repositories": []any{obj{"id": "repo_01", "read": true, "update": []any{"refs/heads/main"}, "seed": false,
			"integration": obj{"ref": "refs/heads/main", "from": c40a, "to": c40c, "gateId": "gate_07"}}},
		"issuedAt": "2026-10-30T10:00:00Z", "expiresAt": "2026-10-30T11:00:00Z"}
}

func grantResponse(grant obj, file string) obj {
	return obj{"grant": grant, "credential": "gc_aaaaaaaaaaaaaaaaaaaaaaaa", "credentialFile": file}
}

// mutObj applies edits without a testing handle; the bases are valid by construction.
func mutObj(base obj, edits ...any) obj {
	t := &testing.T{}
	return mut(t, base, edits...)
}

func admitResp(ctx obj) obj { return obj{"token": tok("kodama-runtime"), "context": ctx} }

func beginSource(path, head string, clean, untracked, ignored bool) obj {
	return obj{"path": path, "head": head, "trackedClean": clean, "untrackedPresent": untracked, "ignoredPresent": ignored}
}
func beginReq(sources ...any) obj { return obj{"sources": append([]any{}, sources...)} }
func beginResp(attempt, at, state string, ignored bool) obj {
	return obj{"attemptId": attempt, "beganAt": at, "workspaceState": state, "ignoredPresent": ignored}
}

func errBody(code, msg string) obj {
	return obj{"error": obj{"code": code, "message": msg, "requestId": "req_01", "details": obj{}}}
}

func resultCompleted(commits ...any) obj {
	return obj{"outcome": "completed", "exitCode": 0, "reason": nil, "question": nil, "commits": append([]any{}, commits...),
		"outputs": obj{"report": "art_0001"}, "diagnosticArtifactIds": []any{}}
}

// resultRequest wraps a result with the correct hash, computed by the Go code
// from the typed result; canonical.json holds independently derived hashes.
func resultRequest(t testing.TB, result obj) obj {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var r ResultV3
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	h, err := ResultHash(r)
	if err != nil {
		t.Fatalf("fixture result is not valid: %v", err)
	}
	return obj{"resultHash": string(h), "result": result}
}

// mut applies path edits (dot-separated, numeric array indexes) to a deep copy
// of base. A Go nil deletes the key; use null to set a JSON null.
func mut(t testing.TB, base obj, edits ...any) obj {
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
	return root.(obj)
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
		if err != nil || i >= len(n) {
			t.Fatalf("bad index %q", segs[0])
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

// null sets a field to JSON null (mut deletes on a Go nil).
type nullT struct{}

var null = nullT{}

func (nullT) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

type payloadCase struct {
	Name         string          `json:"name"`
	Type         string          `json:"type"`
	Requirements []string        `json:"requirements"`
	Payload      json.RawMessage `json:"payload"`
	Raw          *string         `json:"raw"`
	SchemaValid  *bool           `json:"schemaValid"`
	Valid        bool            `json:"valid"`
	Code         string          `json:"code"`
}

type draftCase struct {
	name, typ string
	reqs      []string
	payload   any
	raw       *string
	valid     bool
	code      string
}

func ok(name, typ string, payload any, reqs ...string) draftCase {
	return draftCase{name: name, typ: typ, reqs: reqs, payload: payload, valid: true}
}

func bad(name, typ string, payload any, code string, reqs ...string) draftCase {
	return draftCase{name: name, typ: typ, reqs: reqs, payload: payload, code: code}
}

func rawCase(name, typ, raw, code string, reqs ...string) draftCase {
	return draftCase{name: name, typ: typ, reqs: reqs, raw: &raw, code: code}
}

// cases returns the payload fixtures.
func cases(t testing.TB) []draftCase {
	const ir = "invalid_request"
	wm := func(edits ...any) obj { return mut(t, ctxWriter(), edits...) }
	rm := func(edits ...any) obj { return mut(t, ctxReaderAfterWriter(), edits...) }
	pm := func(edits ...any) obj { return mut(t, ctxPushReader(), edits...) }
	c := []draftCase{
		// ---- ContextV3: valid shapes ----
		ok("context_v3_work_item_writer_two_repositories", "ContextV3", ctxWriter(), "PX-23", "GR-06"),
		ok("context_v3_work_item_reader_after_writer", "ContextV3", ctxReaderAfterWriter(), "PX-23", "WS-16"),
		ok("context_v3_work_item_reader_first_attempt", "ContextV3", ctxReaderFirst(), "PX-23"),
		ok("context_v3_push_origin_reader_root_source", "ContextV3", ctxPushReader(), "PX-24"),
		ok("context_v3_push_origin_writer_run_branch", "ContextV3", ctxPushWriter(), "PX-24", "GR-06"),
		ok("context_v3_manual_origin_reader", "ContextV3", ctxManualReader(), "PX-23"),
		ok("context_v3_provision_origin_without_sources", "ContextV3", ctxProvision(), "PX-25"),
		ok("context_v3_cleanup_run_manual_origin_source_free_without_git", "ContextV3", ctxFree(), "PX-23"),
		ok("context_v3_ca_file_delivered", "ContextV3", wm("git.caFile", "/kodama/git/ca.pem"), "GR-06"),
		ok("context_v3_sha256_object_ids", "ContextV3", mut(t, ctxPushReader(), "workspace.sources.0.commit", c64a, "workspace.sources.0.headCommit", c64a,
			"workspace.preparation.sources.0.commit", c64a, "workspace.sourceSetDigest", sourceSetDigestOf(t, ctxPushReader(), "workspace.sources.0.commit", c64a)), "PX-23"),
		ok("context_v3_unknown_response_field_tolerated", "ContextV3", wm("futureField", true), "PX-21"),
		ok("context_v3_reader_lineage_with_agent_branch_push", "ContextV3", rm("workspace.lineage.0.published", []any{
			commitOut("repo_01", "refs/heads/kodama/agents/wi_1042/run_01/scratch", c40d, "pu_08"), commitOut("repo_01", taskWI, c40c, "pu_07")}), "GR-06"),

		// ---- ContextV3: version and shared members ----
		bad("context_v3_version_two", "ContextV3", wm("contextVersion", 2), ir, "PX-21"),
		bad("context_v3_attempt_private_model", "ContextV3", wm("runModel", "attempt_private"), ir, "PX-21"),
		bad("context_v3_omitted_origin", "ContextV3", wm("origin", nil), ir, "PX-23"),
		bad("context_v3_omitted_git", "ContextV3", wm("git", nil), ir, "GR-06"),
		bad("context_v3_omitted_source_set_digest", "ContextV3", wm("workspace.sourceSetDigest", nil), ir, "PX-23"),
		bad("context_v3_v1_field_still_validated", "ContextV3", wm("command.image", "golang:1.24"), ir, "PX-21"),
		bad("context_v3_wrong_mount_path", "ContextV3", wm("workspace.mountPath", "/work"), ir, "PX-23"),

		// ---- ContextV3: origin ----
		bad("context_v3_unknown_origin_kind", "ContextV3", wm("origin.kind", "webhook"), ir, "PX-23"),
		bad("context_v3_work_item_origin_without_work_item_id", "ContextV3", wm("origin.workItemId", null), ir, "PX-23"),
		bad("context_v3_work_item_origin_with_push_id", "ContextV3", wm("origin.pushId", "pu_01"), ir, "PX-23"),
		bad("context_v3_work_item_origin_without_snapshot", "ContextV3", wm("workItem", null), ir, "PX-23"),
		bad("context_v3_work_item_snapshot_of_another_item", "ContextV3", wm("workItem.id", "wi_9999"), ir, "PX-23"),
		bad("context_v3_push_origin_without_push_id", "ContextV3", mut(t, ctxPushReader(), "origin.pushId", null), ir, "PX-24"),
		bad("context_v3_push_origin_without_repository", "ContextV3", mut(t, ctxPushReader(), "origin.repositoryId", null), ir, "PX-24"),
		bad("context_v3_push_origin_without_ref", "ContextV3", mut(t, ctxPushReader(), "origin.ref", null), ir, "PX-24"),
		bad("context_v3_push_origin_with_server_ref", "ContextV3", mut(t, ctxPushReader(), "origin.ref", "refs/kodama/runs/run_01/1"), ir, "PX-24"),
		bad("context_v3_push_origin_bad_ref", "ContextV3", mut(t, ctxPushReader(), "origin.ref", "main"), ir, "PX-24"),
		bad("context_v3_push_origin_repository_not_in_the_set", "ContextV3", mut(t, ctxPushReader(), "origin.repositoryId", "repo_99"), ir, "PX-24"),
		bad("context_v3_push_origin_with_work_item", "ContextV3", mut(t, ctxPushReader(), "workItem", workItem()), ir, "PX-24"),
		bad("context_v3_manual_origin_with_work_item_id", "ContextV3", mut(t, ctxManualReader(), "origin.workItemId", "wi_1042"), ir, "PX-23"),
		bad("context_v3_manual_origin_with_work_item", "ContextV3", mut(t, ctxManualReader(), "workItem", workItem()), ir, "PX-23"),
		ok("context_v3_origin_additive_member_tolerated", "ContextV3", wm("origin.futureField", c40a), "PX-21"),

		// ---- ContextV3: source set ----
		bad("context_v3_sources_not_ordered_by_path", "ContextV3", wm("workspace.sources", []any{frontRead().obj(), backWrite(c40a, c40a).obj()}), ir, "PX-23"),
		bad("context_v3_duplicate_path", "ContextV3", wm("workspace.sources.1.path", "backend"), ir, "PX-23"),
		bad("context_v3_nested_path", "ContextV3", wm("workspace.sources.1.path", "backend/web"), ir, "PX-23"),
		bad("context_v3_root_next_to_another_source", "ContextV3", wm("workspace.sources.0.path", "."), ir, "PX-23"),
		bad("context_v3_hidden_path", "ContextV3", wm("workspace.sources.1.path", ".git"), ir, "PX-23"),
		bad("context_v3_path_escape", "ContextV3", wm("workspace.sources.1.path", "../etc"), ir, "PX-23"),
		bad("context_v3_zero_commit", "ContextV3", wm("workspace.sources.1.commit", strings.Repeat("0", 40)), ir, "PX-23"),
		bad("context_v3_uppercase_commit", "ContextV3", wm("workspace.sources.1.commit", strings.Repeat("A", 40)), ir, "PX-23"),
		bad("context_v3_source_set_digest_of_another_set", "ContextV3", wm("workspace.sourceSetDigest", dg), ir, "PX-23"),
		bad("context_v3_source_set_digest_with_no_sources", "ContextV3", mut(t, ctxProvision(), "workspace.sourceSetDigest", dg), ir, "PX-23"),
		bad("context_v3_sources_null", "ContextV3", wm("workspace.sources", null), ir, "PX-23"),
		bad("context_v3_read_source_with_branch", "ContextV3", wm("workspace.sources.1.branch", "refs/heads/x"), ir, "PX-23"),
		bad("context_v3_write_source_without_branch", "ContextV3", wm("workspace.sources.0.branch", null), ir, "PX-23"),
		bad("context_v3_unknown_source_access", "ContextV3", wm("workspace.sources.1.access", "admin"), ir, "PX-23"),
		bad("context_v3_credentials_in_repository_url", "ContextV3", wm("workspace.sources.1.repositoryUrl", "https://user:token@git.example.invalid/git/t_acme/p_inventory/frontend.git"), ir, "GR-06"),
		bad("context_v3_plain_http_repository_url", "ContextV3", wm("workspace.sources.1.repositoryUrl", "http://git.example.invalid/git/t_acme/p_inventory/frontend.git"), ir, "GR-06"),
		bad("context_v3_source_free_with_read_access", "ContextV3", mut(t, ctxProvision(), "workspace.access", "read"), ir, "PX-23"),
		bad("context_v3_source_backed_with_none", "ContextV3", wm("workspace.access", "none"), ir, "PX-23"),

		// ---- ContextV3: writer and branch rules ----
		bad("context_v3_writer_without_expected_tip", "ContextV3", wm("workspace.sources.0.expectedTip", null), ir, "WS-16"),
		bad("context_v3_writer_expected_tip_on_a_read_source", "ContextV3", wm("workspace.sources.1.expectedTip", c40b), ir, "WS-16"),
		bad("context_v3_reader_with_expected_tip", "ContextV3", rm("workspace.sources.0.expectedTip", c40c), ir, "WS-16"),
		bad("context_v3_writer_without_a_write_source", "ContextV3", wm("workspace.sources.0.access", "read", "workspace.sources.0.branch", null, "workspace.sources.0.expectedTip", null,
			"workspace.sourceSetDigest", sourceSetDigestOf(t, ctxWriter(), "workspace.sources.0.access", "read", "workspace.sources.0.branch", null)), ir, "WS-16"),
		bad("context_v3_write_branch_is_the_default_branch", "ContextV3", withBranch(t, ctxWriter(), "refs/heads/main"), ir, "GR-06"),
		bad("context_v3_write_branch_of_another_work_item", "ContextV3", withBranch(t, ctxWriter(), "refs/heads/kodama/work/wi_9999"), ir, "GR-06"),
		bad("context_v3_push_origin_write_branch_is_a_task_branch", "ContextV3", withBranch(t, ctxPushWriter(), taskWI), ir, "GR-06"),
		bad("context_v3_write_branch_in_another_runs_namespace", "ContextV3", withBranch(t, ctxPushWriter(), "refs/heads/kodama/runs/run_02/out"), ir, "GR-06"),

		// ---- ContextV3: preparation ----
		bad("context_v3_omitted_preparation", "ContextV3", wm("workspace.preparation", nil), ir, "PX-23"),
		bad("context_v3_null_preparation_with_sources", "ContextV3", wm("workspace.preparation", null), ir, "PX-23"),
		bad("context_v3_preparation_for_another_commit", "ContextV3", wm("workspace.preparation.sources.1.commit", c40d), ir, "PX-23"),
		bad("context_v3_preparation_missing_a_source", "ContextV3", wm("workspace.preparation.sources", []any{obj{"path": "backend", "commit": c40a, "treeId": c40d}}), ir, "PX-23"),
		bad("context_v3_preparation_path_mismatch", "ContextV3", wm("workspace.preparation.sources.0.path", "frontend"), ir, "PX-23"),
		bad("context_v3_preparation_bad_tree", "ContextV3", wm("workspace.preparation.sources.0.treeId", "HEAD"), ir, "PX-23"),
		bad("context_v3_source_free_with_preparation", "ContextV3", mut(t, ctxProvision(), "workspace.preparation", wm()["workspace"].(obj)["preparation"]), ir, "PX-23"),

		// ---- ContextV3: lineage ----
		bad("context_v3_head_is_not_the_last_published", "ContextV3", rm("workspace.sources.0.headCommit", c40d), ir, "WS-16"),
		bad("context_v3_head_moved_without_lineage", "ContextV3", mut(t, ctxReaderFirst(), "workspace.sources.0.headCommit", c40b), ir, "WS-16"),
		bad("context_v3_head_of_a_read_source_moved", "ContextV3", rm("workspace.sources.1.headCommit", c40d), ir, "WS-16"),
		bad("context_v3_lineage_publishes_to_a_foreign_repository", "ContextV3", rm("workspace.lineage.0.published.0.repositoryId", "repo_99"), ir, "GR-06"),
		ok("context_v3_lineage_integration_push_to_the_default_branch", "ContextV3", rm("workspace.lineage.0.published", []any{
			commitOut("repo_01", taskWI, c40c, "pu_07"), commitOut("repo_01", "refs/heads/main", c40c, "pu_08")}), "GR-06", "DQ-17"),
		ok("context_v3_provision_lineage_with_the_seeded_commit", "ContextV3", mut(t, ctxProvision(), "attemptId", "att_02", "workspace.lineage", []any{
			lineage("seed", "att_01", "none", "2026-10-30T10:01:00Z", "none", false, commitOut("repo_03", "refs/heads/main", c40c, "pu_10"))}), "PX-25"),
		bad("context_v3_lineage_publishes_to_another_work_items_task_branch", "ContextV3", rm("workspace.lineage.0.published.0.ref", "refs/heads/kodama/work/wi_9999"), ir, "GR-06"),
		bad("context_v3_lineage_default_branch_of_a_read_only_repository", "ContextV3", rm("workspace.lineage.0.published.0.repositoryId", "repo_02", "workspace.lineage.0.published.0.ref", "refs/heads/main"), ir, "GR-06"),
		bad("context_v3_lineage_reader_publishes_to_the_task_branch", "ContextV3", rm("workspace.lineage.0.access", "read"), ir, "GR-06"),
		bad("context_v3_lineage_published_unsorted", "ContextV3", rm("workspace.lineage.0.published", []any{
			commitOut("repo_01", taskWI, c40c, "pu_07"), commitOut("repo_01", "refs/heads/kodama/agents/wi_1042/run_01/scratch", c40d, "pu_08")}), ir, "GR-06"),
		bad("context_v3_lineage_omitted_published", "ContextV3", rm("workspace.lineage.0.published", nil), ir, "GR-06"),
		bad("context_v3_lineage_published_without_push_id", "ContextV3", rm("workspace.lineage.0.published.0.pushId", nil), ir, "GR-06"),
		bad("context_v3_lineage_includes_itself", "ContextV3", rm("workspace.lineage.0.attemptId", "att_02"), ir, "DQ-16"),
		bad("context_v3_lineage_out_of_order", "ContextV3", rm("workspace.lineage", []any{
			lineage("generate", "att_01", "write", "2026-10-30T10:03:00Z", "clean", false, commitOut("repo_01", taskWI, c40c, "pu_07")),
			lineage("lint", "att_03", "read", "2026-10-30T10:02:00Z", "clean", false)}), ir, "DQ-16"),
		bad("context_v3_source_backed_lineage_state_none", "ContextV3", rm("workspace.lineage.0.workspaceState", "none"), ir, "DQ-16"),
		bad("context_v3_source_free_lineage_published", "ContextV3", mut(t, ctxProvision(), "workspace.lineage", []any{
			lineage("seed", "att_00", "none", "2026-10-30T10:01:00Z", "none", false, commitOut("repo_01", taskWI, c40c, "pu_07"))}), ir, "DQ-16"),

		// ---- ContextV3: Git credential delivery ----
		bad("context_v3_sources_without_git", "ContextV3", wm("git", null), ir, "GR-06"),
		bad("context_v3_cleanup_shaped_run_with_git", "ContextV3", mut(t, ctxFree(), "git", git()), ir, "GR-06"),
		bad("context_v3_provision_without_git", "ContextV3", mut(t, ctxProvision(), "git", null), ir, "PX-25"),
		bad("context_v3_provision_without_the_seed_capability", "ContextV3", mut(t, ctxProvision(), "git.runtimeGrants.seed", null), ir, "PX-25"),
		bad("context_v3_provision_with_a_static_grant", "ContextV3", mut(t, ctxProvision(), "git.grantId", "gg_01", "git.credentialFile", "/kodama/git/credential"), ir, "PX-25"),
		bad("context_v3_provision_omitted_runtime_grants", "ContextV3", mut(t, ctxProvision(), "git.runtimeGrants", nil), ir, "PX-25"),
		bad("context_v3_seed_capability_for_a_work_item_run", "ContextV3", wm("git.runtimeGrants.seed", obj{"credentialFile": seedFile}), ir, "PX-25"),
		bad("context_v3_seed_credential_file_outside_the_directory", "ContextV3", mut(t, ctxProvision(), "git.runtimeGrants.seed.credentialFile", "/tmp/seed"), ir, "PX-25"),
		bad("context_v3_seed_file_is_the_ca_file", "ContextV3", mut(t, ctxProvision(), "git.caFile", seedFile), ir, "PX-25"),
		bad("context_v3_sources_with_a_null_grant", "ContextV3", wm("git.grantId", null), ir, "GR-06"),
		bad("context_v3_sources_with_a_null_credential_file", "ContextV3", wm("git.credentialFile", null), ir, "GR-06"),
		bad("context_v3_write_source_without_the_integration_capability", "ContextV3", wm("git.runtimeGrants.integration", null), ir, "DQ-17"),
		bad("context_v3_integration_capability_without_a_write_source", "ContextV3", mut(t, ctxManualReader(), "git.runtimeGrants.integration", obj{"credentialFile": integrationFile}), ir, "DQ-17"),
		bad("context_v3_integration_file_is_the_credential_file", "ContextV3", wm("git.runtimeGrants.integration.credentialFile", "/kodama/git/credential"), ir, "DQ-17"),
		ok("context_v3_provision_with_the_seed_capability_and_a_ca_file", "ContextV3", mut(t, ctxProvision(), "git.caFile", "/kodama/git/ca.pem"), "PX-25"),
		bad("context_v3_provision_git_names_a_repository", "ContextV3", mut(t, ctxProvision(), "git.repositories", []any{access("repo_01", agentRun)}), ir, "PX-25"),
		bad("context_v3_git_base_url_with_credentials", "ContextV3", wm("git.baseUrl", "https://user:token@git.example.invalid/git"), ir, "GR-06"),
		bad("context_v3_git_base_url_trailing_slash", "ContextV3", wm("git.baseUrl", gitBase+"/"), ir, "GR-06"),
		bad("context_v3_git_base_url_plain_http", "ContextV3", wm("git.baseUrl", "http://git.example.invalid/git"), ir, "GR-06"),
		bad("context_v3_source_url_beyond_the_base", "ContextV3", wm("git.baseUrl", "https://git.example.invalid/other"), ir, "GR-06"),
		bad("context_v3_source_url_on_another_host", "ContextV3", wm("workspace.sources.1.repositoryUrl", "https://evil.example.invalid/git/t_acme/p_inventory/frontend.git"), ir, "GR-06"),
		bad("context_v3_credential_file_outside_the_directory", "ContextV3", wm("git.credentialFile", "/tmp/credential"), ir, "GR-06"),
		bad("context_v3_credential_file_traversal", "ContextV3", wm("git.credentialFile", "/kodama/git/../secrets/token"), ir, "GR-06"),
		bad("context_v3_credential_file_in_a_subdirectory", "ContextV3", wm("git.credentialFile", "/kodama/git/a/credential"), ir, "GR-06"),
		bad("context_v3_credential_file_is_the_ca_file", "ContextV3", wm("git.caFile", "/kodama/git/credential"), ir, "GR-06"),
		bad("context_v3_ca_file_outside_the_directory", "ContextV3", wm("git.caFile", "/etc/ssl/ca.pem"), ir, "GR-06"),
		bad("context_v3_omitted_ca_file", "ContextV3", wm("git.caFile", nil), ir, "GR-06"),
		bad("context_v3_git_grant_id_invalid", "ContextV3", wm("git.grantId", "../gg"), ir, "GR-06"),
		bad("context_v3_git_missing_a_repository", "ContextV3", wm("git.repositories", []any{access("repo_01", agentWI, taskWI)}), ir, "GR-06"),
		bad("context_v3_git_repository_of_another_set", "ContextV3", wm("git.repositories.1.repositoryId", "repo_99"), ir, "GR-06"),
		bad("context_v3_git_repositories_out_of_order", "ContextV3", wm("git.repositories", []any{access("repo_02", agentWI), access("repo_01", agentWI, taskWI)}), ir, "GR-06"),
		bad("context_v3_writer_grant_without_the_task_branch", "ContextV3", wm("git.repositories.0.update", []any{agentWI}), ir, "GR-06"),
		bad("context_v3_reader_grant_with_the_task_branch", "ContextV3", rm("git.repositories.0.update", []any{agentWI, taskWI}), ir, "GR-06"),
		bad("context_v3_writer_grant_task_branch_on_a_read_repository", "ContextV3", wm("git.repositories.1.update", []any{agentWI, taskWI}), ir, "GR-06"),
		bad("context_v3_grant_without_the_agent_namespace", "ContextV3", wm("git.repositories.0.update", []any{taskWI}), ir, "GR-06"),
		bad("context_v3_grant_of_another_runs_agent_namespace", "ContextV3", wm("git.repositories.1.update", []any{"refs/heads/kodama/agents/wi_1042/run_02/*"}), ir, "GR-06"),
		bad("context_v3_grant_names_all_branches", "ContextV3", wm("git.repositories.1.update", []any{"refs/heads/*"}), ir, "GR-06"),
		bad("context_v3_grant_names_a_server_ref", "ContextV3", wm("git.repositories.1.update", []any{agentWI, "refs/kodama/runs/run_01/1"}), ir, "GR-06"),
		bad("context_v3_grant_names_the_default_branch", "ContextV3", wm("git.repositories.1.update", []any{agentWI, "refs/heads/main"}), ir, "GR-06"),
		bad("context_v3_grant_update_null", "ContextV3", wm("git.repositories.1.update", null), ir, "GR-06"),
		bad("context_v3_push_origin_grant_uses_the_agent_namespace", "ContextV3", pm("git.repositories.0.update", []any{agentWI}), ir, "GR-06"),

		// ---- AdmitResponseV3 ----
		ok("admit_response_v3_writer", "AdmitResponseV3", admitResp(ctxWriter()), "PX-23"),
		ok("admit_response_v3_push_origin", "AdmitResponseV3", admitResp(ctxPushReader()), "PX-24"),
		ok("admit_response_v3_provision", "AdmitResponseV3", admitResp(ctxProvision()), "PX-25"),
		bad("admit_response_v3_resource_audience", "AdmitResponseV3", mut(t, admitResp(ctxWriter()), "token.audience", "kodama-runtime-resource"), ir, "PX-21"),
		bad("admit_response_v3_context_v2", "AdmitResponseV3", mut(t, admitResp(ctxWriter()), "context.contextVersion", 2), ir, "PX-21"),

		// ---- begin ----
		ok("begin_request_v3_two_sources", "BeginRequestV3", beginReq(beginSource("backend", c40a, true, false, true), beginSource("frontend", c40b, true, false, false)), "DQ-16"),
		ok("begin_request_v3_modified_source", "BeginRequestV3", beginReq(beginSource("backend", c40a, false, true, false), beginSource("frontend", c40b, true, false, false)), "DQ-16"),
		ok("begin_request_v3_source_free", "BeginRequestV3", beginReq(), "DQ-16"),
		bad("begin_request_v3_omitted_sources", "BeginRequestV3", obj{}, ir, "DQ-16"),
		bad("begin_request_v3_null_sources", "BeginRequestV3", obj{"sources": null}, ir, "DQ-16"),
		bad("begin_request_v3_v2_workspace_member", "BeginRequestV3", obj{"workspace": null}, ir, "DQ-16"),
		bad("begin_request_v3_omitted_field", "BeginRequestV3", mut(t, beginReq(beginSource("backend", c40a, true, false, false)), "sources.0.ignoredPresent", nil), ir, "DQ-16"),
		bad("begin_request_v3_unknown_field", "BeginRequestV3", mut(t, beginReq(beginSource("backend", c40a, true, false, false)), "sources.0.dirty", true), ir, "DQ-16"),
		bad("begin_request_v3_client_asserted_state", "BeginRequestV3", mut(t, beginReq(), "workspaceState", "clean"), ir, "DQ-16"),
		bad("begin_request_v3_bad_head", "BeginRequestV3", beginReq(beginSource("backend", "main", true, false, false)), ir, "DQ-16"),
		bad("begin_request_v3_bad_path", "BeginRequestV3", beginReq(beginSource("../backend", c40a, true, false, false)), ir, "DQ-16"),
		ok("begin_response_v3_clean", "BeginResponseV3", beginResp("att_02", "2026-10-30T10:05:00Z", "clean", true), "DQ-16"),
		ok("begin_response_v3_modified", "BeginResponseV3", beginResp("att_03", "2026-10-30T10:06:00Z", "modified", false), "DQ-16"),
		ok("begin_response_v3_source_free", "BeginResponseV3", beginResp("att_05", "2026-10-30T10:07:00Z", "none", false), "DQ-16"),
		bad("begin_response_v3_unknown_state", "BeginResponseV3", beginResp("att_02", "2026-10-30T10:05:00Z", "dirty", false), ir, "DQ-16"),
		bad("begin_response_v3_source_free_ignored", "BeginResponseV3", beginResp("att_05", "2026-10-30T10:07:00Z", "none", true), ir, "DQ-16"),

		// ---- result ----
		ok("result_v3_writer_commit_outputs", "ResultV3", resultCompleted(commitOut("repo_01", taskWI, c40c, "pu_07")), "PX-23", "GR-06"),
		ok("result_v3_two_repositories_and_an_agent_branch", "ResultV3", resultCompleted(
			commitOut("repo_01", "refs/heads/kodama/agents/wi_1042/run_01/notes", c40d, "pu_08"),
			commitOut("repo_01", taskWI, c40c, "pu_07"),
			commitOut("repo_02", "refs/heads/kodama/agents/wi_1042/run_01/notes", c40b, "pu_09")), "PX-23"),
		ok("result_v3_completed_without_commits", "ResultV3", resultCompleted(), "PX-23"),
		ok("result_v3_failed", "ResultV3", obj{"outcome": "failed", "exitCode": 2, "reason": "command_failed", "question": nil, "commits": []any{}, "outputs": obj{}, "diagnosticArtifactIds": []any{"art_0002"}}, "PX-10"),
		ok("result_v3_needs_input", "ResultV3", obj{"outcome": "needs_input", "exitCode": nil, "reason": nil, "question": "Which index should I use?", "commits": []any{}, "outputs": obj{}, "diagnosticArtifactIds": []any{}}, "PX-10"),
		bad("result_v3_source_commit_is_not_a_member", "ResultV3", mut(t, resultCompleted(), "sourceCommit", c40a), ir, "PX-23"),
		bad("result_v3_omitted_commits", "ResultV3", mut(t, resultCompleted(), "commits", nil), ir, "PX-23"),
		bad("result_v3_null_commits", "ResultV3", mut(t, resultCompleted(), "commits", null), ir, "PX-23"),
		bad("result_v3_commit_without_push_id", "ResultV3", mut(t, resultCompleted(commitOut("repo_01", taskWI, c40c, "pu_07")), "commits.0.pushId", nil), ir, "PX-23"),
		bad("result_v3_commit_with_null_push_id", "ResultV3", mut(t, resultCompleted(commitOut("repo_01", taskWI, c40c, "pu_07")), "commits.0.pushId", null), ir, "PX-23"),
		bad("result_v3_commit_with_bad_push_id", "ResultV3", mut(t, resultCompleted(commitOut("repo_01", taskWI, c40c, "pu_07")), "commits.0.pushId", "../pu"), ir, "PX-23"),
		bad("result_v3_commit_with_extra_member", "ResultV3", mut(t, resultCompleted(commitOut("repo_01", taskWI, c40c, "pu_07")), "commits.0.old", c40a), ir, "PX-23"),
		bad("result_v3_commit_is_zero", "ResultV3", resultCompleted(commitOut("repo_01", taskWI, strings.Repeat("0", 40), "pu_07")), ir, "PX-23"),
		bad("result_v3_commit_is_uppercase", "ResultV3", resultCompleted(commitOut("repo_01", taskWI, strings.Repeat("A", 40), "pu_07")), ir, "PX-23"),
		bad("result_v3_commit_ref_is_a_tag", "ResultV3", resultCompleted(commitOut("repo_01", "refs/tags/v1", c40c, "pu_07")), ir, "GR-06"),
		bad("result_v3_commit_ref_is_a_server_ref", "ResultV3", resultCompleted(commitOut("repo_01", "refs/kodama/runs/run_01/1", c40c, "pu_07")), ir, "GR-06"),
		bad("result_v3_commit_ref_not_a_ref", "ResultV3", resultCompleted(commitOut("repo_01", "main", c40c, "pu_07")), ir, "GR-06"),
		bad("result_v3_commits_not_ordered_by_repository", "ResultV3", resultCompleted(commitOut("repo_02", taskWI, c40b, "pu_09"), commitOut("repo_01", taskWI, c40c, "pu_07")), ir, "PX-23"),
		bad("result_v3_commits_not_ordered_by_ref", "ResultV3", resultCompleted(
			commitOut("repo_01", taskWI, c40c, "pu_07"), commitOut("repo_01", "refs/heads/kodama/agents/wi_1042/run_01/notes", c40d, "pu_08")), ir, "PX-23"),
		bad("result_v3_two_outputs_for_one_ref", "ResultV3", resultCompleted(commitOut("repo_01", taskWI, c40c, "pu_07"), commitOut("repo_01", taskWI, c40d, "pu_08")), ir, "PX-23"),
		bad("result_v3_failed_with_commits", "ResultV3", mut(t, obj{"outcome": "failed", "exitCode": 2, "reason": "command_failed", "question": nil, "commits": []any{}, "outputs": obj{}, "diagnosticArtifactIds": []any{}},
			"commits", []any{commitOut("repo_01", taskWI, c40c, "pu_07")}), ir, "PX-10"),
		bad("result_v3_needs_input_with_commits", "ResultV3", mut(t, obj{"outcome": "needs_input", "exitCode": nil, "reason": nil, "question": "Which?", "commits": []any{}, "outputs": obj{}, "diagnosticArtifactIds": []any{}},
			"commits", []any{commitOut("repo_01", taskWI, c40c, "pu_07")}), ir, "PX-10"),
		bad("result_v3_completed_nonzero_exit", "ResultV3", mut(t, resultCompleted(), "exitCode", 1), ir, "PX-10"),
		bad("result_v3_failed_without_reason", "ResultV3", obj{"outcome": "failed", "exitCode": 2, "reason": nil, "question": nil, "commits": []any{}, "outputs": obj{}, "diagnosticArtifactIds": []any{}}, ir, "PX-10"),
		ok("result_request_v3_writer", "ResultRequestV3", resultRequest(t, resultCompleted(commitOut("repo_01", taskWI, c40c, "pu_07"))), "PX-23"),
		ok("result_request_v3_without_commits", "ResultRequestV3", resultRequest(t, resultCompleted()), "PX-23"),
		bad("result_request_v3_hash_mismatch", "ResultRequestV3", mut(t, resultRequest(t, resultCompleted(commitOut("repo_01", taskWI, c40c, "pu_07"))), "resultHash", dg), "result_hash_mismatch", "PX-23"),
		bad("result_request_v3_hash_of_another_commit", "ResultRequestV3", mut(t, resultRequest(t, resultCompleted(commitOut("repo_01", taskWI, c40c, "pu_07"))), "result.commits.0.commit", c40d), "result_hash_mismatch", "PX-23"),
		bad("result_request_v3_v1_result_shape", "ResultRequestV3", obj{"resultHash": dg, "result": obj{"outcome": "completed", "exitCode": 0, "reason": nil, "question": nil, "sourceCommit": c40a, "outputs": obj{}, "diagnosticArtifactIds": []any{}}}, ir, "PX-21"),
		bad("result_request_v3_unknown_field", "ResultRequestV3", mut(t, resultRequest(t, resultCompleted()), "extra", true), ir, "PX-23"),

		// ---- preparation resource ----
		ok("resource_context_v3_preparation_of_every_source", "ResourceContextV3", prepCtx(), "PX-23", "WS-13"),
		ok("resource_context_v3_preparation_single_root_source", "ResourceContextV3", prepRootCtx(), "PX-23"),
		ok("resource_context_v3_preparation_ca_file", "ResourceContextV3", mut(t, prepCtx(), "preparation.git.caFile", "/kodama/git/ca.pem"), "GR-06"),
		ok("resource_context_v3_preparation_additive_member_tolerated", "ResourceContextV3", mut(t, prepCtx(), "preparation.git.futureField", true), "PX-21"),
		ok("resource_context_v3_service_is_the_v2_message", "ResourceContextV3", svcCtx(), "PX-21", "VS-14"),
		ok("resource_context_v3_builder_service", "ResourceContextV3", mut(t, svcCtx(), "service.role", "builder"), "IT-16"),
		bad("resource_context_v3_version_two", "ResourceContextV3", mut(t, prepCtx(), "contextVersion", 2), ir, "PX-21"),
		bad("resource_context_v3_service_version_two", "ResourceContextV3", mut(t, svcCtx(), "contextVersion", 2), ir, "PX-21"),
		bad("resource_context_v3_v2_single_source_shape", "ResourceContextV3", mut(t, prepCtx(), "preparation", obj{"repositoryUrl": urlBack, "commit": c40a, "mountPath": "/workspace",
			"timeoutSeconds": 600, "maxBytes": 1073741824, "maxAttempts": 2, "deadlineAt": "2026-10-30T11:00:00Z"}), ir, "PX-23"),
		bad("resource_context_v3_key_for_another_kind", "ResourceContextV3", mut(t, prepCtx(), "resourceKey", "service.db"), ir, "VS-14"),
		bad("resource_context_v3_both_members", "ResourceContextV3", mut(t, prepCtx(), "service", svcCtx()["service"]), ir, "VS-14"),
		bad("resource_context_v3_neither_member", "ResourceContextV3", mut(t, prepCtx(), "preparation", null), ir, "VS-14"),
		bad("resource_context_v3_service_secrets_without_command", "ResourceContextV3", mut(t, svcCtx(), "service.command.command", null), ir, "VS-14"),
		bad("resource_context_v3_service_key_mismatch", "ResourceContextV3", mut(t, svcCtx(), "resourceKey", "service.cache"), ir, "VS-14"),
		bad("resource_context_v3_no_sources", "ResourceContextV3", mut(t, prepCtx(), "preparation.sources", []any{}, "preparation.git.repositories", []any{}), ir, "PX-23"),
		bad("resource_context_v3_sources_unordered", "ResourceContextV3", mut(t, prepCtx(), "preparation.sources", []any{prepSource("repo_02", urlFront, c40b, "frontend"), prepSource("repo_01", urlBack, c40a, "backend")}), ir, "PX-23"),
		bad("resource_context_v3_nested_paths", "ResourceContextV3", mut(t, prepCtx(), "preparation.sources.1.path", "backend/web"), ir, "PX-23"),
		bad("resource_context_v3_root_next_to_another_source", "ResourceContextV3", mut(t, prepCtx(), "preparation.sources.0.path", "."), ir, "PX-23"),
		bad("resource_context_v3_path_escape", "ResourceContextV3", mut(t, prepCtx(), "preparation.sources.1.path", "../x"), ir, "PX-23"),
		bad("resource_context_v3_zero_commit", "ResourceContextV3", mut(t, prepCtx(), "preparation.sources.1.commit", strings.Repeat("0", 40)), ir, "PX-23"),
		bad("resource_context_v3_credentials_in_url", "ResourceContextV3", mut(t, prepCtx(), "preparation.sources.1.repositoryUrl", "https://user:token@git.example.invalid/git/t_acme/p_inventory/frontend.git"), ir, "GR-06"),
		bad("resource_context_v3_url_beyond_the_base", "ResourceContextV3", mut(t, prepCtx(), "preparation.sources.1.repositoryUrl", "https://evil.example.invalid/git/t_acme/p_inventory/frontend.git"), ir, "GR-06"),
		bad("resource_context_v3_base_url_trailing_slash", "ResourceContextV3", mut(t, prepCtx(), "preparation.git.baseUrl", gitBase+"/"), ir, "GR-06"),
		bad("resource_context_v3_credential_file_outside_the_directory", "ResourceContextV3", mut(t, prepCtx(), "preparation.git.credentialFile", "/tmp/credential"), ir, "GR-06"),
		bad("resource_context_v3_ca_file_is_the_credential", "ResourceContextV3", mut(t, prepCtx(), "preparation.git.caFile", "/kodama/git/credential"), ir, "GR-06"),
		bad("resource_context_v3_omitted_git", "ResourceContextV3", mut(t, prepCtx(), "preparation.git", nil), ir, "GR-06"),
		bad("resource_context_v3_git_repository_missing", "ResourceContextV3", mut(t, prepCtx(), "preparation.git.repositories", []any{"repo_01"}), ir, "GR-06"),
		bad("resource_context_v3_git_repository_listed_twice", "ResourceContextV3", mut(t, prepCtx(), "preparation.git.repositories", []any{"repo_01", "repo_01", "repo_02"}), ir, "GR-06"),
		bad("resource_context_v3_git_repositories_out_of_order", "ResourceContextV3", mut(t, prepCtx(), "preparation.git.repositories", []any{"repo_02", "repo_01"}), ir, "GR-06"),
		bad("resource_context_v3_git_repository_of_another_set", "ResourceContextV3", mut(t, prepCtx(), "preparation.git.repositories", []any{"repo_01", "repo_99"}), ir, "GR-06"),
		bad("resource_context_v3_git_repositories_with_update_rights", "ResourceContextV3", mut(t, prepCtx(), "preparation.git.repositories", []any{access("repo_01", agentWI), access("repo_02", agentWI)}), ir, "GR-06"),
		bad("resource_context_v3_wrong_mount", "ResourceContextV3", mut(t, prepCtx(), "preparation.mountPath", "/work"), ir, "WS-13"),
		bad("resource_context_v3_zero_tries", "ResourceContextV3", mut(t, prepCtx(), "preparation.maxAttempts", 0), ir, "WS-13"),
		bad("resource_context_v3_omitted_timeout", "ResourceContextV3", mut(t, prepCtx(), "preparation.timeoutSeconds", nil), ir, "WS-13"),
		ok("resource_admit_request_v3_is_the_v2_message", "ResourceAdmitRequest", obj{"admissionCredential": "adm_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "nonce": "nnnnnnnnnnnnnnnnnnnnnn", "clientVersion": "1.4.0",
			"identity": obj{"workloadName": "kr-0123456789abcdef01234567", "workloadUid": "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f", "podName": "kr-0123456789abcdef01234567-x7k2p", "podUid": "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60"}}, "VS-14"),
		ok("resource_admit_response_v3_preparation", "ResourceAdmitResponseV3", resAdmitResp(prepCtx()), "PX-23"),
		ok("resource_admit_response_v3_service", "ResourceAdmitResponseV3", resAdmitResp(svcCtx()), "VS-14"),
		bad("resource_admit_response_v3_attempt_audience", "ResourceAdmitResponseV3", mut(t, resAdmitResp(prepCtx()), "token.audience", "kodama-runtime"), ir, "VS-14"),
		bad("resource_admit_response_v3_token_outlives_the_deadline", "ResourceAdmitResponseV3", mut(t, resAdmitResp(prepCtx()), "token.maxExpiresAt", "2026-10-30T11:00:01Z"), ir, "VS-14"),
		bad("resource_admit_response_v3_context_v2", "ResourceAdmitResponseV3", mut(t, resAdmitResp(prepCtx()), "context.contextVersion", 2), ir, "PX-21"),
		ok("resource_token_request_v3_is_the_v2_message", "ResourceTokenRequest", obj{}, "VS-14"),
		bad("resource_token_request_v3_with_body", "ResourceTokenRequest", obj{"token": "x"}, ir, "VS-14"),
		ok("resource_token_response_v3_is_the_v2_message", "ResourceTokenResponse", obj{"token": resTok()}, "VS-14"),

		// ---- preparation receipt ----
		ok("preparation_receipt_v3_every_source", "PreparationReceiptRequestV3", receiptReq(), "WS-13", "PX-23"),
		ok("preparation_receipt_v3_second_try", "PreparationReceiptRequestV3", mut(t, receiptReq(), "attempts", 2), "WS-13"),
		bad("preparation_receipt_v3_v2_single_commit_shape", "PreparationReceiptRequestV3", obj{"commit": c40a, "treeId": c40d, "hooksDisabled": true, "submodules": "skipped", "credentialsRemoved": true, "attempts": 1}, ir, "PX-23"),
		bad("preparation_receipt_v3_omitted_sources", "PreparationReceiptRequestV3", mut(t, receiptReq(), "sources", nil), ir, "PX-23"),
		bad("preparation_receipt_v3_no_sources", "PreparationReceiptRequestV3", mut(t, receiptReq(), "sources", []any{}), ir, "PX-23"),
		bad("preparation_receipt_v3_source_without_tree", "PreparationReceiptRequestV3", mut(t, receiptReq(), "sources.0.treeId", nil), ir, "WS-13"),
		bad("preparation_receipt_v3_source_with_extra_member", "PreparationReceiptRequestV3", mut(t, receiptReq(), "sources.0.url", urlBack), ir, "WS-13"),
		bad("preparation_receipt_v3_bad_path", "PreparationReceiptRequestV3", mut(t, receiptReq(), "sources.0.path", "../backend"), ir, "WS-13"),
		bad("preparation_receipt_v3_hooks_enabled", "PreparationReceiptRequestV3", mut(t, receiptReq(), "hooksDisabled", false), ir, "WS-13"),
		bad("preparation_receipt_v3_submodules_fetched", "PreparationReceiptRequestV3", mut(t, receiptReq(), "submodules", "fetched"), ir, "WS-13"),
		bad("preparation_receipt_v3_credentials_kept", "PreparationReceiptRequestV3", mut(t, receiptReq(), "credentialsRemoved", false), ir, "WS-13"),
		bad("preparation_receipt_v3_zero_tries", "PreparationReceiptRequestV3", mut(t, receiptReq(), "attempts", 0), ir, "WS-13"),
		bad("preparation_receipt_v3_client_digest", "PreparationReceiptRequestV3", mut(t, receiptReq(), "receiptDigest", dgE), ir, "WS-13"),
		ok("preparation_receipt_response_v3_is_the_v2_message", "PreparationReceiptResponse", obj{"receiptDigest": dgE, "recordedAt": "2026-10-30T10:00:30Z"}, "WS-13"),

		// ---- runtime-issued Git grants ----
		ok("git_seed_request", "GitSeedRequest", obj{"repositoryId": "repo_03"}, "PX-25"),
		bad("git_seed_request_omitted_repository", "GitSeedRequest", obj{}, ir, "PX-25"),
		bad("git_seed_request_with_a_default_branch", "GitSeedRequest", obj{"repositoryId": "repo_03", "defaultBranch": "main"}, ir, "PX-25"),
		bad("git_seed_request_bad_repository", "GitSeedRequest", obj{"repositoryId": "../repo"}, ir, "PX-25"),
		ok("git_integration_request", "GitIntegrationRequest", obj{"repositoryId": "repo_01", "gateId": "gate_07"}, "DQ-17"),
		bad("git_integration_request_omitted_gate", "GitIntegrationRequest", obj{"repositoryId": "repo_01"}, ir, "DQ-17"),
		bad("git_integration_request_with_a_commit", "GitIntegrationRequest", obj{"repositoryId": "repo_01", "gateId": "gate_07", "to": c40c}, ir, "DQ-17"),
		ok("git_grant_response_seed", "GitGrantResponse", grantResponse(seedGrant(), seedFile), "PX-25"),
		ok("git_grant_response_integration", "GitGrantResponse", grantResponse(integrationGrant(), integrationFile), "DQ-17"),
		ok("git_grant_response_additive_member_tolerated", "GitGrantResponse", mut(t, grantResponse(seedGrant(), seedFile), "futureField", true), "PX-21"),
		bad("git_grant_response_attempt_kind", "GitGrantResponse", mut(t, grantResponse(seedGrant(), seedFile), "grant.kind", "attempt"), ir, "GR-06"),
		bad("git_grant_response_seed_naming_two_repositories", "GitGrantResponse", mut(t, grantResponse(seedGrant(), seedFile),
			"grant.repositories", []any{seedGrant()["repositories"].([]any)[0], mut(t, obj{"x": seedGrant()["repositories"].([]any)[0]}, "x.id", "repo_04")["x"]}), ir, "PX-25"),
		bad("git_grant_response_seed_with_a_pattern", "GitGrantResponse", mut(t, grantResponse(seedGrant(), seedFile), "grant.repositories.0.update", []any{"refs/heads/*"}), ir, "PX-25"),
		bad("git_grant_response_seed_in_the_kodama_namespace", "GitGrantResponse", mut(t, grantResponse(seedGrant(), seedFile), "grant.repositories.0.update", []any{"refs/kodama/runs/run_01/1"}), ir, "PX-25"),
		bad("git_grant_response_seed_flag_missing", "GitGrantResponse", mut(t, grantResponse(seedGrant(), seedFile), "grant.repositories.0.seed", false), ir, "PX-25"),
		bad("git_grant_response_seed_lifetime_too_long", "GitGrantResponse", mut(t, grantResponse(seedGrant(), seedFile), "grant.expiresAt", "2026-10-30T11:00:00Z"), ir, "PX-25"),
		bad("git_grant_response_integration_without_constraint", "GitGrantResponse", mut(t, grantResponse(integrationGrant(), integrationFile), "grant.repositories.0.integration", null), ir, "DQ-17"),
		bad("git_grant_response_integration_same_from_and_to", "GitGrantResponse", mut(t, grantResponse(integrationGrant(), integrationFile), "grant.repositories.0.integration.to", c40a), ir, "DQ-17"),
		bad("git_grant_response_integration_update_is_a_pattern", "GitGrantResponse", mut(t, grantResponse(integrationGrant(), integrationFile), "grant.repositories.0.update", []any{"refs/heads/*"}), ir, "DQ-17"),
		bad("git_grant_response_short_credential", "GitGrantResponse", mut(t, grantResponse(seedGrant(), seedFile), "credential", "short"), ir, "GR-06"),
		bad("git_grant_response_file_outside_the_directory", "GitGrantResponse", mut(t, grantResponse(seedGrant(), seedFile), "credentialFile", "/tmp/seed"), ir, "GR-06"),
		bad("git_grant_response_omitted_credential_file", "GitGrantResponse", mut(t, grantResponse(seedGrant(), seedFile), "credentialFile", nil), ir, "GR-06"),

		// ---- errors ----
		ok("error_v3_git_seed_refusal", "ErrorResponse", errBody("repository_not_empty", "the repository already has refs"), "PX-25"),
		ok("error_v3_git_seed_pending", "ErrorResponse", errBody("seed_pending", "an earlier push is unsettled"), "PX-25"),
		ok("error_v3_publication_conflict_for_a_moved_base", "ErrorResponse", errBody("publication_conflict", "the target ref moved since the approval"), "DQ-17"),
		bad("error_v3_unknown_git_code", "ErrorResponse", errBody("seed_exploded", "no"), ir, "PX-25"),
		ok("error_v3_context_version_mismatch", "ErrorResponse", errBody("context_version_mismatch", "the attempt belongs to another runtime version"), "PX-21"),
		ok("error_v3_output_invalid", "ErrorResponse", errBody("output_invalid", "a commit output names a ref outside the grant"), "PX-23"),
		rawCase("raw_duplicate_key_in_begin_request_v3", "BeginRequestV3", `{"sources":[],"sources":[]}`, ir, "DQ-16"),
		rawCase("raw_float_in_result_v3", "ResultV3", `{"outcome":"completed","exitCode":0.0,"reason":null,"question":null,"commits":[],"outputs":{},"diagnosticArtifactIds":[]}`, ir, "PX-23"),
	}
	return c
}

// withBranch edits the branch of the first source of a writer context and
// recomputes the set digest, so only the branch rule can fail.
func withBranch(t testing.TB, base obj, branch string) obj {
	t.Helper()
	return mut(t, base, "workspace.sources.0.branch", branch, "workspace.sourceSetDigest", sourceSetDigestOf(t, base, "workspace.sources.0.branch", branch))
}

// sourceSetDigestOf returns the digest of the source set of base's workspace after
// the edits are applied to it, for fixtures whose sources are edited after the
// workspace builder computed the digest.
func sourceSetDigestOf(t testing.TB, base obj, edits ...any) string {
	t.Helper()
	m := mut(t, base, edits...)
	raw, err := json.Marshal(m["workspace"])
	if err != nil {
		t.Fatal(err)
	}
	var w WorkspaceV3
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	set := gitv1.SourceSet{Sources: make([]gitv1.Source, len(w.Sources))}
	for i, s := range w.Sources {
		set.Sources[i] = s.Source
	}
	d, err := set.Digest()
	if err != nil {
		t.Fatalf("edited source set is not a valid set: %v", err)
	}
	return d
}
