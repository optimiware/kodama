package runtimev2

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
)

// The fixtures of this package are produced from typed bases (valid by
// construction) and path mutations, then published as cases.json and
// scenarios.json. The schema judges every published payload independently of
// the Go code, and a drift test requires the checked-in files to equal what the
// bases produce (regenerate with KODAMA_UPDATE_GOLDEN=1).

const (
	c40a = "1111111111111111111111111111111111111111"
	c40b = "2222222222222222222222222222222222222222"
	c40c = "3333333333333333333333333333333333333333"
	c40d = "4444444444444444444444444444444444444444"
	dg   = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	dgE  = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	dgF  = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	img  = "registry.example.invalid/test-tools@sha256:4444444444444444444444444444444444444444444444444444444444444444"
	repo = "https://git.example.invalid/acme/inventory.git"
)

type obj = map[string]any

func tok(aud string) obj {
	return obj{"token": "rt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "tokenType": "Bearer", "audience": aud,
		"expiresAt": "2026-10-26T10:15:00Z", "maxExpiresAt": "2026-10-26T12:00:00Z"}
}

func limits() obj {
	return obj{"timeoutSeconds": 1200, "maxOutputBytes": 104857600, "maxOutputFiles": 100, "maxDiagnosticBytes": 10485760}
}

func lineage(step, attempt, access, at, state string, ignored bool, published any) obj {
	return obj{"stepId": step, "attemptId": attempt, "access": access, "beganAt": at, "workspaceState": state, "ignoredPresent": ignored, "publishedCommit": published}
}

// ctxBase builds a context; workspace parts vary by caller.
func ctxBase(step, attempt string, ws obj) obj {
	return obj{
		"contextVersion": 2, "runModel": "run_shared", "tenantId": "t_acme", "projectId": "p_inventory", "runId": "run_01",
		"stepId": step, "attemptId": attempt, "actionKey": "run_01." + step,
		"definition": obj{"id": "pd_02", "digest": dg}, "workItem": nil, "designBaseline": nil, "profile": nil,
		"command": obj{"image": img, "command": []any{"/bin/sh", "-ec"}, "args": []any{"cd /workspace && go test ./..."},
			"env": obj{"PGHOST": "s-ab12cd34ef567890-db", "PGPORT": "5432", "PGUSER": "kodama"}},
		"workspace":   ws,
		"endpoints":   obj{"db": obj{"host": "s-ab12cd34ef567890-db", "ports": obj{"sql": 5432}}},
		"egress":      []any{},
		"run":         obj{"startedAt": "2026-10-26T10:00:00Z", "deadlineAt": "2026-10-26T11:00:00Z"},
		"policy":      obj{"id": "run-v1", "digest": dgF},
		"inputs":      obj{},
		"outputs":     []any{obj{"name": "report", "type": "artifact", "path": "/kodama/outputs/tests.json"}},
		"secrets":     []any{obj{"ref": "secret://project/test-db-password", "version": 3, "name": "PGPASSWORD", "delivery": "env"}},
		"environment": nil, "limits": limits(),
	}
}

func prep() obj { return obj{"commit": c40a, "treeId": c40c, "receiptDigest": dgE} }

func readerWS(head string, lin ...any) obj {
	return obj{"access": "read", "mountPath": "/workspace",
		"source":      obj{"repositoryUrl": repo, "inputCommit": c40a, "headCommit": head, "branch": nil, "expectedTip": nil},
		"preparation": prep(), "lineage": append([]any{}, lin...)}
}

func writerWS(head string, lin ...any) obj {
	return obj{"access": "write", "mountPath": "/workspace",
		"source":      obj{"repositoryUrl": repo, "inputCommit": c40a, "headCommit": head, "branch": "refs/heads/kodama/wi-1042", "expectedTip": head},
		"preparation": prep(), "lineage": append([]any{}, lin...)}
}

func freeWS(lin ...any) obj {
	return obj{"access": "none", "mountPath": "/workspace", "source": nil, "preparation": nil, "lineage": append([]any{}, lin...)}
}

// Contexts.
func ctxReader() obj {
	return ctxBase("integration", "att_02", readerWS(c40b,
		lineage("generate", "att_01", "write", "2026-10-26T10:01:00Z", "clean", false, c40b)))
}
func ctxReaderFirst() obj { return ctxBase("generate-first", "att_01", readerWS(c40a)) }
func ctxWriter() obj {
	c := ctxBase("generate", "att_01", writerWS(c40a))
	c["endpoints"] = obj{}
	c["secrets"] = []any{}
	c["command"].(obj)["env"] = obj{}
	return c
}
func ctxFree() obj {
	c := ctxBase("teardown", "att_04", freeWS())
	c["endpoints"], c["secrets"] = obj{}, []any{}
	c["command"].(obj)["env"] = obj{}
	return c
}
func ctxFreeSecond() obj {
	c := ctxBase("verify", "att_05", freeWS(lineage("teardown", "att_04", "none", "2026-10-26T10:06:00Z", "none", false, nil)))
	c["endpoints"], c["secrets"] = obj{}, []any{}
	c["command"] = obj{"image": img, "command": []any{"/bin/sh", "-ec"}, "args": []any{"test -s /workspace/teardown.log"}, "env": obj{}}
	return c
}

func admitResp(ctx obj) obj { return obj{"token": tok("kodama-runtime"), "context": ctx} }

func beginReq(head string, trackedClean, untracked, ignored bool) obj {
	return obj{"workspace": obj{"head": head, "trackedClean": trackedClean, "untrackedPresent": untracked, "ignoredPresent": ignored}}
}
func beginResp(attempt, at, state string, ignored bool) obj {
	return obj{"attemptId": attempt, "beganAt": at, "workspaceState": state, "ignoredPresent": ignored}
}

func identityJob() obj {
	return obj{"workloadName": "kr-0123456789abcdef01234567", "workloadUid": "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f",
		"podName": "kr-0123456789abcdef01234567-x7k2p", "podUid": "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60"}
}
func identityServicePod() obj {
	return obj{"workloadName": "s-ab12cd34ef567890-db", "workloadUid": "8b3e3f40-5c6d-4e7f-8a01-2c3d4e5f6071", "podName": nil, "podUid": nil}
}
func identityPodman() obj {
	return obj{"workloadName": "kr-fedcba9876543210fedcba98", "workloadUid": "3f9a2c4b6d8e1f0a7b5c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e3f2a", "podName": nil, "podUid": nil}
}
func identityProcess() obj {
	return obj{"workloadName": "kr-process-1", "workloadUid": "pid-4242-start-1761473000", "podName": nil, "podUid": nil}
}
func resAdmit(id obj) obj {
	return obj{"admissionCredential": "adm_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "nonce": "nnnnnnnnnnnnnnnnnnnnnn", "clientVersion": "1.4.0", "identity": id}
}

func prepCtx() obj {
	return obj{"contextVersion": 2, "resourceKind": "preparation", "tenantId": "t_acme", "projectId": "p_inventory", "runId": "run_01", "resourceKey": "preparation",
		"preparation": obj{"repositoryUrl": repo, "commit": c40a, "mountPath": "/workspace", "timeoutSeconds": 600, "maxBytes": 1073741824, "maxAttempts": 2, "deadlineAt": "2026-10-26T11:00:00Z"},
		"service":     nil, "policy": obj{"id": "run-v1", "digest": dgF}}
}
func svcDefaultsCtx() obj {
	return obj{"contextVersion": 2, "resourceKind": "service", "tenantId": "t_acme", "projectId": "p_inventory", "runId": "run_01", "resourceKey": "service.cache",
		"preparation": nil,
		"service": obj{"name": "cache", "role": "general", "command": obj{"image": img, "command": nil, "args": []any{}, "env": obj{"MODE": "ephemeral"}},
			"secrets": []any{}, "deadlineAt": "2026-10-26T11:00:00Z"},
		"policy": obj{"id": "run-v1", "digest": dgF}}
}
func svcSecretsCtx() obj {
	return obj{"contextVersion": 2, "resourceKind": "service", "tenantId": "t_acme", "projectId": "p_inventory", "runId": "run_01", "resourceKey": "service.db",
		"preparation": nil,
		"service": obj{"name": "db", "role": "general", "command": obj{"image": img, "command": []any{"/usr/local/bin/docker-entrypoint.sh", "postgres"}, "args": []any{},
			"env": obj{"POSTGRES_USER": "kodama", "POSTGRES_DB": "app", "POSTGRES_PASSWORD_FILE": "/kodama/secrets/postgres-password"}},
			"secrets":    []any{obj{"ref": "secret://project/test-db-password", "version": 3, "name": "postgres-password", "delivery": "file"}},
			"deadlineAt": "2026-10-26T11:00:00Z"},
		"policy": obj{"id": "run-v1", "digest": dgF}}
}
func receiptReq() obj {
	return obj{"commit": c40a, "treeId": c40c, "hooksDisabled": true, "submodules": "skipped", "credentialsRemoved": true, "attempts": 1}
}
func errBody(code, msg string) obj {
	return obj{"error": obj{"code": code, "message": msg, "requestId": "req_01", "details": obj{}}}
}

// mut applies path edits (dot-separated, numeric array indexes) to a deep copy
// of base. A nil value deletes the key.
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
	ctxSF := ctxFree()
	c := []draftCase{
		// ---- ContextV2 ----
		ok("context_v2_reader_with_lineage", "ContextV2", ctxReader(), "PX-21", "WS-13", "DQ-16"),
		ok("context_v2_reader_first_attempt", "ContextV2", ctxReaderFirst(), "WS-13"),
		ok("context_v2_writer_branch_and_expected_tip", "ContextV2", ctxWriter(), "WS-16"),
		ok("context_v2_source_free", "ContextV2", ctxSF, "WS-13", "PX-21"),
		ok("context_v2_source_free_second_step_sees_first", "ContextV2", ctxFreeSecond(), "WS-13", "DQ-16"),
		ok("context_v2_two_services_endpoints", "ContextV2", mut(t, ctxReader(),
			"endpoints", obj{"db": obj{"host": "s-ab12cd34ef567890-db", "ports": obj{"sql": 5432, "admin": 5433}}, "cache": obj{"host": "s-ab12cd34ef567890-cache", "ports": obj{"kv": 6379}}},
			"egress", []any{"registry", "preview"}), "PX-17", "IT-14"),
		ok("context_v2_modified_and_ignored_lineage", "ContextV2", mut(t, ctxReader(), "workspace.lineage", []any{
			lineage("generate", "att_01", "write", "2026-10-26T10:01:00Z", "modified", true, nil),
			lineage("lint", "att_03", "read", "2026-10-26T10:02:00Z", "clean", true, nil)},
			"workspace.source.headCommit", c40a), "DQ-16"),
		ok("context_v2_unknown_response_field_tolerated", "ContextV2", mut(t, ctxReader(), "futureField", true), "PX-21"),
		bad("context_v2_version_one", "ContextV2", mut(t, ctxReader(), "contextVersion", 1), "invalid_request", "PX-21"),
		bad("context_v2_attempt_private_model", "ContextV2", mut(t, ctxReader(), "runModel", "attempt_private"), "invalid_request", "PX-21"),
		bad("context_v2_missing_run_model", "ContextV2", mut(t, ctxReader(), "runModel", nil), "invalid_request", "PX-21"),
		bad("context_v2_omitted_nullable_profile", "ContextV2", mut(t, ctxReader(), "profile", nil), "invalid_request", "PX-21"),
		bad("context_v2_omitted_lineage", "ContextV2", mut(t, ctxReader(), "workspace.lineage", nil), "invalid_request", "DQ-16"),
		bad("context_v2_null_lineage", "ContextV2", mut(t, ctxReader(), "workspace.lineage", null), "invalid_request", "DQ-16"),
		bad("context_v2_omitted_preparation", "ContextV2", mut(t, ctxReader(), "workspace.preparation", nil), "invalid_request", "WS-13"),
		bad("context_v2_source_free_with_read_access", "ContextV2", mut(t, ctxSF, "workspace.access", "read"), "invalid_request", "WS-13"),
		bad("context_v2_source_free_with_preparation", "ContextV2", mut(t, ctxSF, "workspace.preparation", prep()), "invalid_request", "WS-13"),
		bad("context_v2_source_backed_with_none", "ContextV2", mut(t, ctxReader(), "workspace.access", "none"), "invalid_request", "WS-13"),
		bad("context_v2_source_backed_without_preparation", "ContextV2", mut(t, ctxReader(), "workspace.preparation", null), "invalid_request", "WS-13"),
		bad("context_v2_preparation_for_another_commit", "ContextV2", mut(t, ctxReader(), "workspace.preparation.commit", c40d), "invalid_request", "WS-13"),
		bad("context_v2_writer_without_branch", "ContextV2", mut(t, ctxWriter(), "workspace.source.branch", null), "invalid_request", "WS-16"),
		bad("context_v2_writer_without_expected_tip", "ContextV2", mut(t, ctxWriter(), "workspace.source.expectedTip", null), "invalid_request", "WS-16"),
		bad("context_v2_reader_with_branch", "ContextV2", mut(t, ctxReader(), "workspace.source.branch", "refs/heads/x"), "invalid_request", "WS-16"),
		bad("context_v2_head_is_not_the_last_published", "ContextV2", mut(t, ctxReader(), "workspace.source.headCommit", c40d), "invalid_request", "WS-16"),
		bad("context_v2_head_moved_without_lineage", "ContextV2", mut(t, ctxReaderFirst(), "workspace.source.headCommit", c40b), "invalid_request", "WS-16"),
		bad("context_v2_wrong_mount_path", "ContextV2", mut(t, ctxReader(), "workspace.mountPath", "/work"), "invalid_request", "WS-13"),
		bad("context_v2_lineage_out_of_order", "ContextV2", mut(t, ctxReader(), "workspace.lineage", []any{
			lineage("generate", "att_01", "write", "2026-10-26T10:03:00Z", "clean", false, c40b),
			lineage("lint", "att_03", "read", "2026-10-26T10:02:00Z", "clean", false, nil)}), "invalid_request", "DQ-16"),
		bad("context_v2_lineage_includes_itself", "ContextV2", mut(t, ctxReader(), "workspace.lineage.0.attemptId", "att_02"), "invalid_request", "DQ-16"),
		bad("context_v2_lineage_duplicate_attempt", "ContextV2", mut(t, ctxReader(), "workspace.lineage", []any{
			lineage("generate", "att_01", "write", "2026-10-26T10:01:00Z", "clean", false, c40b),
			lineage("generate", "att_01", "read", "2026-10-26T10:02:00Z", "clean", false, nil)}), "invalid_request", "DQ-16"),
		bad("context_v2_lineage_missing_ignored_present", "ContextV2", mut(t, ctxReader(), "workspace.lineage.0.ignoredPresent", nil), "invalid_request", "DQ-16"),
		bad("context_v2_lineage_missing_began_at", "ContextV2", mut(t, ctxReader(), "workspace.lineage.0.beganAt", nil), "invalid_request", "DQ-16"),
		bad("context_v2_lineage_unknown_state", "ContextV2", mut(t, ctxReader(), "workspace.lineage.0.workspaceState", "dirty"), "invalid_request", "DQ-16"),
		bad("context_v2_lineage_reader_published", "ContextV2", mut(t, ctxReader(), "workspace.lineage.0.access", "read"), "invalid_request", "DQ-16"),
		bad("context_v2_source_backed_lineage_state_none", "ContextV2", mut(t, ctxReader(), "workspace.lineage.0.workspaceState", "none"), "invalid_request", "DQ-16"),
		bad("context_v2_source_free_lineage_clean", "ContextV2", mut(t, ctxFreeSecond(), "workspace.lineage.0.workspaceState", "clean"), "invalid_request", "DQ-16"),
		bad("context_v2_source_free_lineage_ignored", "ContextV2", mut(t, ctxFreeSecond(), "workspace.lineage.0.ignoredPresent", true), "invalid_request", "DQ-16"),
		bad("context_v2_source_free_lineage_published", "ContextV2", mut(t, ctxFreeSecond(), "workspace.lineage.0.publishedCommit", c40b), "invalid_request", "DQ-16"),
		bad("context_v2_endpoint_host_not_a_label", "ContextV2", mut(t, ctxReader(), "endpoints.db.host", "db.example.com"), "invalid_request", "PX-17"),
		bad("context_v2_endpoint_port_range", "ContextV2", mut(t, ctxReader(), "endpoints.db.ports.sql", 70000), "invalid_request", "PX-17"),
		bad("context_v2_endpoint_without_ports", "ContextV2", mut(t, ctxReader(), "endpoints.db.ports", obj{}), "invalid_request", "PX-17"),
		bad("context_v2_omitted_endpoints", "ContextV2", mut(t, ctxReader(), "endpoints", nil), "invalid_request", "PX-17"),
		bad("context_v2_duplicate_egress", "ContextV2", mut(t, ctxReader(), "egress", []any{"registry", "registry"}), "invalid_request", "IT-14"),
		bad("context_v2_egress_is_a_host", "ContextV2", mut(t, ctxReader(), "egress", []any{"registry.example.com"}), "invalid_request", "IT-14"),
		bad("context_v2_deadline_before_start", "ContextV2", mut(t, ctxReader(), "run.deadlineAt", "2026-10-26T09:00:00Z"), "invalid_request", "PX-20"),
		bad("context_v2_omitted_policy", "ContextV2", mut(t, ctxReader(), "policy", nil), "invalid_request", "PX-21"),
		bad("context_v2_bad_policy_digest", "ContextV2", mut(t, ctxReader(), "policy.digest", "sha256:abc"), "invalid_request", "PX-21"),
		bad("context_v2_v1_field_still_validated", "ContextV2", mut(t, ctxReader(), "command.image", "golang:1.24"), "invalid_request", "PX-21"),
		bad("context_v2_v1_limits_still_validated", "ContextV2", mut(t, ctxReader(), "limits.timeoutSeconds", 0), "invalid_request", "PX-21"),
		ok("context_v2_env_text_is_opaque_to_the_wire", "ContextV2", mut(t, ctxReader(), "command.env", obj{"PGHOST": "${{ services.db.host }}"}), "PX-17"),

		// ---- AdmitResponseV2 ----
		ok("admit_response_v2_reader", "AdmitResponseV2", admitResp(ctxReader()), "PX-21"),
		ok("admit_response_v2_source_free", "AdmitResponseV2", admitResp(ctxFree()), "PX-21"),
		bad("admit_response_v2_resource_audience", "AdmitResponseV2", mut(t, admitResp(ctxReader()), "token.audience", "kodama-runtime-resource"), "invalid_request", "IT-14"),
		bad("admit_response_v2_context_v1", "AdmitResponseV2", mut(t, admitResp(ctxReader()), "context.contextVersion", 1), "invalid_request", "PX-21"),

		// ---- begin ----
		ok("begin_request_clean", "BeginRequest", beginReq(c40b, true, false, true), "DQ-16"),
		ok("begin_request_modified", "BeginRequest", beginReq(c40b, false, true, false), "DQ-16"),
		ok("begin_request_source_free", "BeginRequest", obj{"workspace": null}, "DQ-16"),
		bad("begin_request_omitted_workspace", "BeginRequest", obj{}, "invalid_request", "DQ-16"),
		bad("begin_request_omitted_field", "BeginRequest", mut(t, beginReq(c40b, true, false, false), "workspace.ignoredPresent", nil), "invalid_request", "DQ-16"),
		bad("begin_request_unknown_field", "BeginRequest", mut(t, beginReq(c40b, true, false, false), "workspace.dirty", true), "invalid_request", "DQ-16"),
		bad("begin_request_client_asserted_state", "BeginRequest", mut(t, beginReq(c40b, true, false, false), "workspaceState", "clean"), "invalid_request", "DQ-16"),
		bad("begin_request_bad_head", "BeginRequest", mut(t, beginReq(c40b, true, false, false), "workspace.head", "main"), "invalid_request", "DQ-16"),
		bad("begin_request_string_boolean", "BeginRequest", mut(t, beginReq(c40b, true, false, false), "workspace.trackedClean", "true"), "invalid_request", "DQ-16"),
		ok("begin_response_clean", "BeginResponse", beginResp("att_02", "2026-10-26T10:05:00Z", "clean", true), "DQ-16"),
		ok("begin_response_modified", "BeginResponse", beginResp("att_03", "2026-10-26T10:06:00Z", "modified", false), "DQ-16"),
		ok("begin_response_source_free", "BeginResponse", beginResp("att_05", "2026-10-26T10:07:00Z", "none", false), "DQ-16"),
		bad("begin_response_unknown_state", "BeginResponse", beginResp("att_02", "2026-10-26T10:05:00Z", "dirty", false), "invalid_request", "DQ-16"),
		bad("begin_response_source_free_ignored", "BeginResponse", beginResp("att_05", "2026-10-26T10:07:00Z", "none", true), "invalid_request", "DQ-16"),
		bad("begin_response_omitted_began_at", "BeginResponse", mut(t, beginResp("att_02", "2026-10-26T10:05:00Z", "clean", false), "beganAt", nil), "invalid_request", "DQ-16"),
		bad("begin_response_bad_time", "BeginResponse", beginResp("att_02", "yesterday", "clean", false), "invalid_request", "DQ-16"),

		// ---- resource admission ----
		ok("resource_admit_request_job_with_pod", "ResourceAdmitRequest", resAdmit(identityJob()), "IT-13", "VS-14"),
		ok("resource_admit_request_service_pod_without_mapping", "ResourceAdmitRequest", resAdmit(identityServicePod()), "VS-14"),
		ok("resource_admit_request_podman", "ResourceAdmitRequest", resAdmit(identityPodman()), "VS-14"),
		ok("resource_admit_request_process", "ResourceAdmitRequest", resAdmit(identityProcess()), "VS-14"),
		bad("resource_admit_request_pod_name_only", "ResourceAdmitRequest", mut(t, resAdmit(identityJob()), "identity.podUid", null), "invalid_request", "VS-14"),
		bad("resource_admit_request_pod_uid_only", "ResourceAdmitRequest", mut(t, resAdmit(identityServicePod()), "identity.podUid", "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60"), "invalid_request", "VS-14"),
		bad("resource_admit_request_omitted_pod_fields", "ResourceAdmitRequest", mut(t, resAdmit(identityServicePod()), "identity.podName", nil, "identity.podUid", nil), "invalid_request", "VS-14"),
		bad("resource_admit_request_job_shaped_identity", "ResourceAdmitRequest", mut(t, resAdmit(identityJob()), "identity", obj{"name": "x", "uid": "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f", "podName": "p", "podUid": "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60"}), "invalid_request", "VS-14"),
		bad("resource_admit_request_caller_namespace", "ResourceAdmitRequest", mut(t, resAdmit(identityJob()), "identity.namespace", "tenant-a"), "invalid_request", "IT-13"),
		bad("resource_admit_request_bad_workload_uid", "ResourceAdmitRequest", mut(t, resAdmit(identityJob()), "identity.workloadUid", "../x"), "invalid_request", "VS-14"),
		bad("resource_admit_request_short_credential", "ResourceAdmitRequest", mut(t, resAdmit(identityJob()), "admissionCredential", "short"), "invalid_request", "VS-14"),
		ok("resource_admit_response_preparation", "ResourceAdmitResponse", obj{"token": tok("kodama-runtime-resource"), "context": prepCtx()}, "VS-14"),
		ok("resource_admit_response_service", "ResourceAdmitResponse", obj{"token": tok("kodama-runtime-resource"), "context": svcSecretsCtx()}, "VS-14"),
		bad("resource_admit_response_attempt_audience", "ResourceAdmitResponse", obj{"token": tok("kodama-runtime"), "context": prepCtx()}, "invalid_request", "VS-14"),
		bad("resource_admit_response_token_type", "ResourceAdmitResponse", mut(t, obj{"token": tok("kodama-runtime-resource"), "context": prepCtx()}, "token.tokenType", "Basic"), "invalid_request", "VS-14"),
		bad("resource_admit_response_token_beyond_maximum", "ResourceAdmitResponse", mut(t, obj{"token": tok("kodama-runtime-resource"), "context": prepCtx()}, "token.expiresAt", "2026-10-26T13:00:00Z"), "invalid_request", "VS-14"),
		ok("resource_token_request", "ResourceTokenRequest", obj{}, "VS-14"),
		bad("resource_token_request_with_body", "ResourceTokenRequest", obj{"token": "x"}, "invalid_request", "VS-14"),
		ok("resource_token_response", "ResourceTokenResponse", obj{"token": tok("kodama-runtime-resource")}, "VS-14"),
		bad("resource_token_response_attempt_audience", "ResourceTokenResponse", obj{"token": tok("kodama-runtime")}, "invalid_request", "VS-14"),

		// ---- resource contexts ----
		ok("resource_context_preparation", "ResourceContext", prepCtx(), "WS-13"),
		ok("resource_context_service_defaults_only", "ResourceContext", svcDefaultsCtx(), "PX-17", "IT-14"),
		ok("resource_context_service_command_and_secrets", "ResourceContext", svcSecretsCtx(), "VS-14"),
		ok("resource_context_builder_service", "ResourceContext", mut(t, svcSecretsCtx(), "service.role", "builder"), "IT-16"),
		bad("resource_context_version_one", "ResourceContext", mut(t, prepCtx(), "contextVersion", 1), "invalid_request", "PX-21"),
		bad("resource_context_key_for_other_kind", "ResourceContext", mut(t, prepCtx(), "resourceKey", "service.db"), "invalid_request", "VS-14"),
		bad("resource_context_service_key_mismatch", "ResourceContext", mut(t, svcSecretsCtx(), "resourceKey", "service.cache"), "invalid_request", "VS-14"),
		bad("resource_context_workspace_key", "ResourceContext", mut(t, prepCtx(), "resourceKey", "workspace"), "invalid_request", "VS-14"),
		bad("resource_context_both_members", "ResourceContext", mut(t, prepCtx(), "service", svcDefaultsCtx()["service"]), "invalid_request", "VS-14"),
		bad("resource_context_neither_member", "ResourceContext", mut(t, prepCtx(), "preparation", null), "invalid_request", "VS-14"),
		bad("resource_context_omitted_service_member", "ResourceContext", mut(t, prepCtx(), "service", nil), "invalid_request", "VS-14"),
		bad("resource_context_secrets_without_command", "ResourceContext", mut(t, svcSecretsCtx(), "service.command.command", null), "invalid_request", "VS-14"),
		bad("resource_context_empty_command", "ResourceContext", mut(t, svcDefaultsCtx(), "service.command.command", []any{}), "invalid_request", "PX-17"),
		bad("resource_context_null_args", "ResourceContext", mut(t, svcDefaultsCtx(), "service.command.args", null), "invalid_request", "PX-17"),
		bad("resource_context_omitted_args", "ResourceContext", mut(t, svcDefaultsCtx(), "service.command.args", nil), "invalid_request", "PX-17"),
		bad("resource_context_duplicate_secret_name", "ResourceContext", mut(t, svcSecretsCtx(), "service.secrets", []any{
			obj{"ref": "secret://project/a", "version": 1, "name": "x", "delivery": "file"}, obj{"ref": "secret://project/b", "version": 1, "name": "x", "delivery": "file"}}), "invalid_request", "VS-14"),
		bad("resource_context_preparation_attempts_zero", "ResourceContext", mut(t, prepCtx(), "preparation.maxAttempts", 0), "invalid_request", "WS-13"),
		bad("resource_context_preparation_timeout_missing", "ResourceContext", mut(t, prepCtx(), "preparation.timeoutSeconds", nil), "invalid_request", "WS-13"),
		bad("resource_context_preparation_mount", "ResourceContext", mut(t, prepCtx(), "preparation.mountPath", "/work"), "invalid_request", "WS-13"),
		bad("resource_context_checkout_credentials_in_url", "ResourceContext", mut(t, prepCtx(), "preparation.repositoryUrl", "https://user:token@git.example.invalid/r.git"), "invalid_request", "VS-14"),

		// ---- preparation receipt ----
		ok("preparation_receipt_request", "PreparationReceiptRequest", receiptReq(), "WS-13"),
		ok("preparation_receipt_request_second_try", "PreparationReceiptRequest", mut(t, receiptReq(), "attempts", 2), "WS-13"),
		bad("preparation_receipt_hooks_enabled", "PreparationReceiptRequest", mut(t, receiptReq(), "hooksDisabled", false), "invalid_request", "WS-13"),
		bad("preparation_receipt_submodules_fetched", "PreparationReceiptRequest", mut(t, receiptReq(), "submodules", "fetched"), "invalid_request", "WS-13"),
		bad("preparation_receipt_credentials_kept", "PreparationReceiptRequest", mut(t, receiptReq(), "credentialsRemoved", false), "invalid_request", "WS-13"),
		bad("preparation_receipt_zero_tries", "PreparationReceiptRequest", mut(t, receiptReq(), "attempts", 0), "invalid_request", "WS-13"),
		bad("preparation_receipt_omitted_attempts", "PreparationReceiptRequest", mut(t, receiptReq(), "attempts", nil), "invalid_request", "WS-13"),
		bad("preparation_receipt_client_digest", "PreparationReceiptRequest", mut(t, receiptReq(), "receiptDigest", dgE), "invalid_request", "WS-13"),
		bad("preparation_receipt_bad_tree", "PreparationReceiptRequest", mut(t, receiptReq(), "treeId", "HEAD"), "invalid_request", "WS-13"),
		ok("preparation_receipt_response", "PreparationReceiptResponse", obj{"receiptDigest": dgE, "recordedAt": "2026-10-26T10:00:30Z"}, "WS-13"),
		bad("preparation_receipt_response_bad_digest", "PreparationReceiptResponse", obj{"receiptDigest": "sha256:xyz", "recordedAt": "2026-10-26T10:00:30Z"}, "invalid_request", "WS-13"),

		// ---- errors ----
		ok("error_context_version_mismatch", "ErrorResponse", errBody("context_version_mismatch", "the attempt belongs to another runtime version"), "PX-21"),
		ok("error_resource_unbound", "ErrorResponse", errBody("resource_unbound", "the resource instance is not bound yet"), "VS-14"),
		ok("error_resource_revoked", "ErrorResponse", errBody("resource_revoked", "the resource is no longer authorized"), "VS-14"),
		ok("error_receipt_conflict", "ErrorResponse", errBody("receipt_conflict", "a different receipt is recorded"), "WS-13"),
		ok("error_v1_code_unchanged", "ErrorResponse", errBody("attempt_revoked", "the attempt is revoked"), "PX-21"),
		bad("error_unknown_code", "ErrorResponse", errBody("resource_exploded", "no"), "invalid_request", "PX-21"),
		rawCase("raw_duplicate_key_in_begin_request", "BeginRequest", `{"workspace":null,"workspace":null}`, "invalid_request", "DQ-16"),
		rawCase("raw_float_in_receipt", "PreparationReceiptRequest", `{"commit":"`+c40a+`","treeId":"`+c40c+`","hooksDisabled":true,"submodules":"skipped","credentialsRemoved":true,"attempts":1.0}`, "invalid_request", "WS-13"),
	}
	return c
}

type draftStep struct {
	API      string          `json:"api"`
	Op       string          `json:"op"`
	Kind     string          `json:"resourceKind,omitempty"`
	Audience string          `json:"tokenAudience,omitempty"`
	Request  json.RawMessage `json:"request"`
	Status   int             `json:"status"`
	Body     json.RawMessage `json:"body"`
}

type draftScenario struct {
	Name         string      `json:"name"`
	Requirements []string    `json:"requirements"`
	Steps        []draftStep `json:"steps"`
}

func raw(t testing.TB, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func step(t testing.TB, api, op string, req any, status int, body any) draftStep {
	return draftStep{API: api, Op: op, Request: raw(t, req), Status: status, Body: raw(t, body)}
}

func rstep(t testing.TB, op, kind string, req any, status int, body any) draftStep {
	s := step(t, "v2", op, req, status, body)
	s.Kind = kind
	return s
}

func wrongAudience(s draftStep, aud string) draftStep {
	s.Audience = aud
	return s
}

// scenarios returns the replayable exchanges.
func scenarios(t testing.TB) []draftScenario {
	var nilBody any
	tokenResp := obj{"token": tok("kodama-runtime")}
	resTokenResp := obj{"token": tok("kodama-runtime-resource")}
	admitReq := obj{"admissionCredential": "adm_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "nonce": "nnnnnnnnnnnnnnnnnnnnnn", "clientVersion": "1.4.0",
		"job": obj{"name": "kodama-att-02", "uid": "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f", "podName": "kodama-att-02-x7k2p", "podUid": "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60"}}
	admitFree := mut(t, admitReq, "job.name", "kodama-att-04")
	resReq := resAdmit(identityJob())
	return []draftScenario{
		{Name: "begin_clean_then_result_hash_is_the_v1_hash", Requirements: []string{"DQ-16", "PX-21"}, Steps: []draftStep{
			step(t, "v2", "admit", admitReq, 200, admitResp(ctxReader())),
			step(t, "v2", "begin", beginReq(c40b, true, false, true), 200, beginResp("att_02", "2026-10-26T10:05:00Z", "clean", true)),
			step(t, "v2", "begin", beginReq(c40b, true, false, true), 200, beginResp("att_02", "2026-10-26T10:05:00Z", "clean", true)),
		}},
		{Name: "begin_modified_workspace", Requirements: []string{"DQ-16"}, Steps: []draftStep{
			step(t, "v2", "admit", admitReq, 200, admitResp(ctxReader())),
			step(t, "v2", "begin", beginReq(c40b, false, true, false), 200, beginResp("att_02", "2026-10-26T10:05:00Z", "modified", false)),
		}},
		{Name: "begin_wrong_head_is_modified", Requirements: []string{"DQ-16"}, Steps: []draftStep{
			step(t, "v2", "admit", admitReq, 200, admitResp(ctxReader())),
			step(t, "v2", "begin", beginReq(c40d, true, false, false), 200, beginResp("att_02", "2026-10-26T10:05:00Z", "modified", false)),
		}},
		{Name: "begin_source_free", Requirements: []string{"DQ-16", "WS-13"}, Steps: []draftStep{
			step(t, "v2", "admit", admitFree, 200, admitResp(ctxFree())),
			step(t, "v2", "begin", obj{"workspace": null}, 200, beginResp("att_04", "2026-10-26T10:06:00Z", "none", false)),
		}},
		{Name: "begin_replay_with_a_different_body_conflicts", Requirements: []string{"DQ-16"}, Steps: []draftStep{
			step(t, "v2", "admit", admitReq, 200, admitResp(ctxReader())),
			step(t, "v2", "begin", beginReq(c40b, true, false, false), 200, beginResp("att_02", "2026-10-26T10:05:00Z", "clean", false)),
			step(t, "v2", "begin", beginReq(c40b, false, false, false), 409, errBody("idempotency_conflict", "a different begin is already recorded")),
		}},
		{Name: "begin_null_body_for_a_source_backed_attempt", Requirements: []string{"DQ-16"}, Steps: []draftStep{
			step(t, "v2", "admit", admitReq, 200, admitResp(ctxReader())),
			step(t, "v2", "begin", obj{"workspace": null}, 400, errBody("invalid_request", "a source-backed attempt reports its workspace")),
		}},
		{Name: "begin_object_for_a_source_free_attempt", Requirements: []string{"DQ-16"}, Steps: []draftStep{
			step(t, "v2", "admit", admitFree, 200, admitResp(ctxFree())),
			step(t, "v2", "begin", beginReq(c40b, true, false, false), 400, errBody("invalid_request", "a source-free attempt reports a null workspace")),
		}},
		{Name: "cancelled_before_begin_starts_nothing", Requirements: []string{"DQ-16", "PX-22"}, Steps: []draftStep{
			step(t, "v2", "admit", admitReq, 200, admitResp(ctxReader())),
			step(t, "v2", "begin", beginReq(c40b, true, false, false), 403, errBody("attempt_revoked", "the attempt is revoked")),
			step(t, "v2", "submitResult", resultRequest(t), 403, errBody("attempt_revoked", "the attempt is revoked")),
		}},
		{Name: "lost_begin_acknowledgment_leaves_the_record", Requirements: []string{"DQ-16", "PX-22"}, Steps: []draftStep{
			// The first response is lost to the client (no step records a body for it);
			// the replay returns the original record, so the attempt is treated as begun.
			step(t, "v2", "admit", admitReq, 200, admitResp(ctxReader())),
			step(t, "v2", "begin", beginReq(c40b, true, false, false), 200, beginResp("att_02", "2026-10-26T10:05:00Z", "clean", false)),
			step(t, "v2", "begin", beginReq(c40b, true, false, false), 200, beginResp("att_02", "2026-10-26T10:05:00Z", "clean", false)),
		}},
		{Name: "two_step_source_free_sequence_keeps_the_scratch", Requirements: []string{"WS-13", "DQ-16"}, Steps: []draftStep{
			step(t, "v2", "admit", admitFree, 200, admitResp(ctxFree())),
			step(t, "v2", "begin", obj{"workspace": null}, 200, beginResp("att_04", "2026-10-26T10:06:00Z", "none", false)),
			step(t, "v2", "admit", mut(t, admitFree, "job.name", "kodama-att-05"), 200, admitResp(ctxFreeSecond())),
			step(t, "v2", "begin", obj{"workspace": null}, 200, beginResp("att_05", "2026-10-26T10:07:00Z", "none", false)),
		}},
		{Name: "v2_attempt_on_a_v1_route", Requirements: []string{"PX-21"}, Steps: []draftStep{
			step(t, "v1", "getContext", nilBody, 409, errBody("context_version_mismatch", "the attempt belongs to another runtime version")),
			step(t, "v1", "renewToken", obj{}, 409, errBody("context_version_mismatch", "the attempt belongs to another runtime version")),
		}},
		{Name: "v1_attempt_on_a_v2_route", Requirements: []string{"PX-21"}, Steps: []draftStep{
			step(t, "v2", "getContext", nilBody, 409, errBody("context_version_mismatch", "the attempt belongs to another runtime version")),
			step(t, "v2", "begin", obj{"workspace": null}, 409, errBody("context_version_mismatch", "the attempt belongs to another runtime version")),
		}},
		{Name: "resource_token_on_an_attempt_route", Requirements: []string{"VS-14", "IT-13"}, Steps: []draftStep{
			wrongAudience(step(t, "v2", "getContext", nilBody, 403, errBody("forbidden", "the token is not valid for this route family")), "kodama-runtime-resource"),
			wrongAudience(step(t, "v2", "begin", obj{"workspace": null}, 403, errBody("forbidden", "the token is not valid for this route family")), "kodama-runtime-resource"),
		}},
		{Name: "attempt_token_on_a_resource_route", Requirements: []string{"VS-14", "IT-13"}, Steps: []draftStep{
			wrongAudience(rstep(t, "getResourceContext", "service", nilBody, 403, errBody("forbidden", "the token is not valid for this route family")), "kodama-runtime"),
			wrongAudience(rstep(t, "resolveResourceSecret", "service", obj{"ref": "secret://project/test-db-password", "version": 3}, 403, errBody("forbidden", "the token is not valid for this route family")), "kodama-runtime"),
		}},
		{Name: "preparation_admission_context_receipt", Requirements: []string{"WS-13", "VS-14"}, Steps: []draftStep{
			rstep(t, "admitResource", "preparation", resReq, 200, obj{"token": tok("kodama-runtime-resource"), "context": prepCtx()}),
			rstep(t, "getResourceContext", "preparation", nilBody, 200, prepCtx()),
			rstep(t, "renewResourceToken", "preparation", obj{}, 200, resTokenResp),
			rstep(t, "completePreparation", "preparation", receiptReq(), 200, obj{"receiptDigest": dgE, "recordedAt": "2026-10-26T10:00:30Z"}),
			rstep(t, "completePreparation", "preparation", receiptReq(), 200, obj{"receiptDigest": dgE, "recordedAt": "2026-10-26T10:00:30Z"}),
			rstep(t, "completePreparation", "preparation", mut(t, receiptReq(), "treeId", c40d), 409, errBody("receipt_conflict", "a different receipt is recorded")),
		}},
		{Name: "preparation_has_no_secret_route", Requirements: []string{"VS-14"}, Steps: []draftStep{
			rstep(t, "resolveResourceSecret", "preparation", obj{"ref": "secret://project/test-db-password", "version": 3}, 403, errBody("forbidden", "this resource kind may not call the route")),
		}},
		{Name: "service_cannot_complete_preparation", Requirements: []string{"VS-14"}, Steps: []draftStep{
			rstep(t, "completePreparation", "service", receiptReq(), 403, errBody("forbidden", "this resource kind may not call the route")),
		}},
		{Name: "service_admission_and_secret_resolution", Requirements: []string{"VS-14", "PX-17"}, Steps: []draftStep{
			rstep(t, "admitResource", "service", resAdmit(identityServicePod()), 200, obj{"token": tok("kodama-runtime-resource"), "context": svcSecretsCtx()}),
			rstep(t, "resolveResourceSecret", "service", obj{"ref": "secret://project/test-db-password", "version": 3}, 200,
				obj{"ref": "secret://project/test-db-password", "version": 3, "name": "postgres-password", "delivery": "file", "valueBase64": "c2VjcmV0"}),
			rstep(t, "resolveResourceSecret", "service", obj{"ref": "secret://project/other", "version": 1}, 404, errBody("not_found", "no such binding")),
		}},
		{Name: "resource_admission_replay_and_consumption", Requirements: []string{"VS-14"}, Steps: []draftStep{
			rstep(t, "admitResource", "service", resAdmit(identityPodman()), 200, obj{"token": tok("kodama-runtime-resource"), "context": svcDefaultsCtx()}),
			rstep(t, "admitResource", "service", resAdmit(identityPodman()), 200, obj{"token": tok("kodama-runtime-resource"), "context": svcDefaultsCtx()}),
			rstep(t, "admitResource", "service", mut(t, resAdmit(identityPodman()), "nonce", "mmmmmmmmmmmmmmmmmmmmmm"), 409, errBody("admission_consumed", "the credential was consumed with another nonce")),
		}},
		{Name: "resource_identity_mismatch_unbound_and_revoked", Requirements: []string{"VS-14", "IT-13"}, Steps: []draftStep{
			rstep(t, "admitResource", "service", mut(t, resAdmit(identityServicePod()), "identity.workloadUid", "9c4f4051-6d7e-4f80-9b12-3d4e5f607182"), 403, errBody("forbidden", "the identity does not match the committed resource")),
			rstep(t, "admitResource", "preparation", mut(t, resAdmit(identityJob()), "identity.podName", "kr-other-pod"), 403, errBody("forbidden", "the pod does not belong to the workload")),
			rstep(t, "admitResource", "service", resAdmit(identityProcess()), 409, errBody("resource_unbound", "the resource instance is not bound yet")),
			rstep(t, "renewResourceToken", "service", obj{}, 403, errBody("resource_revoked", "the resource is no longer authorized")),
			rstep(t, "getResourceContext", "service", nilBody, 403, errBody("resource_revoked", "the resource is no longer authorized")),
		}},
		{Name: "resource_incompatible_client", Requirements: []string{"VS-14"}, Steps: []draftStep{
			rstep(t, "admitResource", "service", mut(t, resAdmit(identityServicePod()), "clientVersion", "0.0.1"), 422, errBody("profile_unsupported", "the client version is not supported")),
		}},
		{Name: "attempt_token_renewal_keeps_the_attempt_audience", Requirements: []string{"PX-21"}, Steps: []draftStep{
			step(t, "v2", "admit", admitReq, 200, admitResp(ctxReader())),
			step(t, "v2", "renewToken", obj{}, 200, tokenResp),
		}},
	}
}

// resultRequest is a valid v1 ResultRequest (the result object and its hash are
// unchanged in v2).
func resultRequest(t testing.TB) obj {
	t.Helper()
	zero := int64(0)
	res := Result{Outcome: "completed", ExitCode: &zero, Outputs: map[string]Value{}, DiagnosticArtifactIDs: nil}
	res.DiagnosticArtifactIDs = []domain.ArtifactID{}
	h, err := ResultHash(res)
	if err != nil {
		t.Fatal(err)
	}
	var r obj
	if err := json.Unmarshal(raw(t, ResultRequest{ResultHash: h, Result: res}), &r); err != nil {
		t.Fatal(err)
	}
	return r
}
