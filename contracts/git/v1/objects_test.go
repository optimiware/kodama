package gitv1

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
)

const sha256Hex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func manifestBase() obj {
	return obj{"repositoryId": "repo_01", "refs": []any{obj{"ref": "refs/heads/main", "commit": c1}, obj{"ref": "refs/kodama/runs/run_01/1", "commit": c2}, obj{"ref": "refs/tags/v1", "commit": c3}},
		"journalHead": 12, "consistent": true, "size": 2048, "sha256": sha256Hex, "workerInstance": "gitworker-7f9c"}
}

func manyPaths(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = fmt.Sprintf(".kodama/p%d.yaml", i)
	}
	return out
}

func replayBase() obj {
	return obj{"repositoryId": "repo_01", "operationId": "op_20", "sha256": sha256Hex}
}

func replayResponseBase() obj {
	return obj{"operationId": "op_20", "created": 2, "unchanged": 1, "conflicting": []any{
		obj{"ref": "refs/heads/main", "current": c2, "bundle": c1, "relation": "repository_ahead"}}, "workerInstance": "gitworker-7f9c"}
}

func fileBase(path, content string) obj {
	sum := sha256.Sum256([]byte(content))
	return obj{"path": path, "content": content, "sha256": hex.EncodeToString(sum[:])}
}

func filesBase() obj {
	return obj{"files": []any{fileBase(".kodama/build.yaml", "name: build\n"), fileBase(".kodama/deploy.yml", "name: deploy\n")}}
}

func objectCases(t testing.TB) areaCases {
	a := areaCases{area: "object_ops"}
	a.ok("is_ancestor_request", "IsAncestorRequest", obj{"repositoryId": "repo_01", "ancestor": c1, "descendant": c2})
	a.ok("is_ancestor_request_same_commit", "IsAncestorRequest", obj{"repositoryId": "repo_01", "ancestor": c1, "descendant": c1})
	a.ok("is_ancestor_request_sha256", "IsAncestorRequest", obj{"repositoryId": "repo_01", "ancestor": c64, "descendant": c64})
	a.ok("is_ancestor_response_true", "IsAncestorResponse", obj{"isAncestor": true})
	a.ok("is_ancestor_response_false", "IsAncestorResponse", obj{"isAncestor": false})
	a.ok("is_ancestor_response_tolerates_added_member", "IsAncestorResponse", obj{"isAncestor": true, "walked": 12})
	a.ok("has_commit_request", "HasCommitRequest", obj{"repositoryId": "repo_01", "commit": c1})
	a.ok("has_commit_response_present", "HasCommitResponse", obj{"present": true})
	a.ok("has_commit_response_absent", "HasCommitResponse", obj{"present": false})
	a.ok("commit_time_request", "CommitTimeRequest", obj{"repositoryId": "repo_01", "commit": c1})
	a.ok("commit_time_response", "CommitTimeResponse", obj{"committedAt": "2026-10-30T09:15:42Z"})
	a.ok("error_commit_not_found", "ErrorResponse", errBody("commit_not_found"))
	a.ok("read_files_request_directory", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{".kodama/"}})
	a.ok("read_files_request_files", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{".kodama/build.yaml", ".kodama/deploy.yml", ".kodama/nested/x.y.yaml"}})
	a.ok("read_files_request_sha256", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c64, "paths": []any{".kodama/"}})
	a.ok("read_files_response", "ReadFilesResponse", filesBase())
	a.ok("read_files_response_no_files", "ReadFilesResponse", obj{"files": []any{}})
	a.ok("read_files_response_empty_file", "ReadFilesResponse", obj{"files": []any{fileBase(".kodama/empty.yaml", "")}})
	a.ok("read_files_response_tolerates_added_member", "ReadFilesResponse", mut(t, filesBase(), "truncated", false))
	a.ok("create_bundle_request_all", "CreateBundleRequest", obj{"repositoryId": "repo_01", "refs": null})
	a.ok("create_bundle_request_refs", "CreateBundleRequest", obj{"repositoryId": "repo_01", "refs": []any{"refs/heads/main", "refs/kodama/runs/run_01/1"}})
	a.ok("create_bundle_response", "CreateBundleResponse", manifestBase())
	a.ok("create_bundle_response_not_consistent", "CreateBundleResponse", mut(t, manifestBase(), "consistent", false))
	a.ok("create_bundle_response_tolerates_added_member", "CreateBundleResponse", mut(t, manifestBase(), "compression", "none"))
	a.ok("error_repository_empty", "ErrorResponse", errBody("repository_empty"))
	a.ok("error_bundle_too_large", "ErrorResponse", errBody("bundle_too_large"))
	a.ok("replay_bundle_request", "ReplayBundleRequest", replayBase())
	a.ok("replay_bundle_request_without_digest", "ReplayBundleRequest", mut(t, replayBase(), "sha256", null))
	a.ok("replay_bundle_response", "ReplayBundleResponse", replayResponseBase())
	a.ok("replay_bundle_response_no_conflict", "ReplayBundleResponse", mut(t, replayResponseBase(), "conflicting", []any{}))
	for _, rel := range []string{"repository_ahead", "bundle_ahead", "diverged"} {
		a.ok("replay_bundle_response_"+rel, "ReplayBundleResponse", mut(t, replayResponseBase(), "conflicting.0.relation", rel))
	}
	a.ok("error_restore_fence_required", "ErrorResponse", errBody("restore_fence_required"))
	a.ok("restore_fence_enter", "RestoreFenceRequest", obj{"repositoryId": "repo_01", "fenced": true})
	a.ok("restore_fence_leave", "RestoreFenceRequest", obj{"repositoryId": "repo_01", "fenced": false})
	a.ok("restore_fence_response", "RestoreFenceResponse", obj{"repositoryId": "repo_01", "fenced": true, "changed": true, "workerInstance": "gitworker-7f9c"})

	a.bad("is_ancestor_unknown_field", "IsAncestorRequest", obj{"repositoryId": "repo_01", "ancestor": c1, "descendant": c2, "path": "/x"})
	a.bad("is_ancestor_zero_commit", "IsAncestorRequest", obj{"repositoryId": "repo_01", "ancestor": z40, "descendant": c2})
	a.bad("is_ancestor_symbolic", "IsAncestorRequest", obj{"repositoryId": "repo_01", "ancestor": "HEAD", "descendant": c2})
	a.bad("is_ancestor_option_injection", "IsAncestorRequest", obj{"repositoryId": "repo_01", "ancestor": "--all", "descendant": c2})
	a.bad("is_ancestor_descendant_omitted", "IsAncestorRequest", obj{"repositoryId": "repo_01", "ancestor": c1})
	a.bad("is_ancestor_path_as_repository", "IsAncestorRequest", obj{"repositoryId": "../repo", "ancestor": c1, "descendant": c2})
	a.badGo("is_ancestor_mixed_hash_lengths", "IsAncestorRequest", obj{"repositoryId": "repo_01", "ancestor": c1, "descendant": c64})
	a.bad("is_ancestor_response_omitted", "IsAncestorResponse", obj{})
	a.bad("is_ancestor_response_string", "IsAncestorResponse", obj{"isAncestor": "true"})
	a.bad("has_commit_zero", "HasCommitRequest", obj{"repositoryId": "repo_01", "commit": z40})
	a.bad("has_commit_uppercase", "HasCommitRequest", obj{"repositoryId": "repo_01", "commit": strings.Repeat("A", 40)})
	a.bad("has_commit_response_omitted", "HasCommitResponse", obj{})
	a.bad("commit_time_short_id", "CommitTimeRequest", obj{"repositoryId": "repo_01", "commit": "abc123"})
	a.bad("commit_time_response_without_zone", "CommitTimeResponse", obj{"committedAt": "2026-10-30T09:15:42"})
	a.bad("read_files_unknown_field", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{".kodama/"}, "ref": "main"})
	a.bad("read_files_ref_name_as_commit", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": "main", "paths": []any{".kodama/"}})
	a.bad("read_files_zero_commit", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": z40, "paths": []any{".kodama/"}})
	a.bad("read_files_paths_omitted", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1})
	a.bad("read_files_paths_empty", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{}})
	a.bad("read_files_path_outside_directory", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{"src/main.yaml"}})
	a.bad("read_files_path_not_yaml", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{".kodama/notes.txt"}})
	a.bad("read_files_path_traversal", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{".kodama/../x.yaml"}})
	a.bad("read_files_path_traversal_inside_component", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{".kodama/a..b.yaml"}})
	a.bad("read_files_path_hidden_file", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{".kodama/.yaml"}})
	a.bad("read_files_path_absolute", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{"/.kodama/x.yaml"}})
	a.bad("read_files_path_empty_component", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{".kodama//x.yaml"}})
	a.bad("read_files_path_colon", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{".kodama/a:b.yaml"}})
	a.bad("read_files_path_newline", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{".kodama/x\n.yaml"}})
	a.bad("read_files_subdirectory_request", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": []any{".kodama/sub/"}})
	a.bad("read_files_too_many_paths", "ReadFilesRequest", obj{"repositoryId": "repo_01", "commit": c1, "paths": manyPaths(MaxReadFilePaths + 1)})
	a.bad("read_files_response_files_omitted", "ReadFilesResponse", obj{})
	a.bad("read_files_response_files_null", "ReadFilesResponse", obj{"files": null})
	a.bad("read_files_response_path_outside", "ReadFilesResponse", obj{"files": []any{fileBase("src/x.yaml", "x")}})
	a.bad("read_files_response_digest_uppercase", "ReadFilesResponse", mut(t, filesBase(), "files.0.sha256", strings.ToUpper(sha256Hex)))
	a.badGo("read_files_response_digest_mismatch", "ReadFilesResponse", mut(t, filesBase(), "files.0.sha256", sha256Hex))
	a.badGo("read_files_response_unsorted", "ReadFilesResponse", obj{"files": []any{fileBase(".kodama/b.yaml", "b"), fileBase(".kodama/a.yaml", "a")}})
	a.badGo("read_files_response_repeated", "ReadFilesResponse", obj{"files": []any{fileBase(".kodama/a.yaml", "a"), fileBase(".kodama/a.yaml", "a")}})
	a.bad("create_bundle_refs_omitted", "CreateBundleRequest", obj{"repositoryId": "repo_01"})
	a.bad("create_bundle_refs_empty", "CreateBundleRequest", obj{"repositoryId": "repo_01", "refs": []any{}})
	a.bad("create_bundle_ref_not_a_ref", "CreateBundleRequest", obj{"repositoryId": "repo_01", "refs": []any{"main"}})
	a.bad("create_bundle_ref_traversal", "CreateBundleRequest", obj{"repositoryId": "repo_01", "refs": []any{"refs/heads/../x"}})
	a.bad("create_bundle_ref_option", "CreateBundleRequest", obj{"repositoryId": "repo_01", "refs": []any{"--all"}})
	a.badGo("create_bundle_ref_repeated", "CreateBundleRequest", obj{"repositoryId": "repo_01", "refs": []any{"refs/heads/main", "refs/heads/main"}})
	a.bad("create_bundle_unknown_field", "CreateBundleRequest", obj{"repositoryId": "repo_01", "refs": null, "thin": true})
	a.bad("create_bundle_response_no_refs", "CreateBundleResponse", mut(t, manifestBase(), "refs", []any{}))
	a.bad("create_bundle_response_refs_null", "CreateBundleResponse", mut(t, manifestBase(), "refs", null))
	a.badGo("create_bundle_response_refs_unsorted", "CreateBundleResponse", mut(t, manifestBase(), "refs", []any{obj{"ref": "refs/tags/v1", "commit": c3}, obj{"ref": "refs/heads/main", "commit": c1}}))
	a.badGo("create_bundle_response_refs_repeated", "CreateBundleResponse", mut(t, manifestBase(), "refs", []any{obj{"ref": "refs/heads/main", "commit": c1}, obj{"ref": "refs/heads/main", "commit": c1}}))
	a.bad("create_bundle_response_zero_size", "CreateBundleResponse", mut(t, manifestBase(), "size", 0))
	a.bad("create_bundle_response_digest_short", "CreateBundleResponse", mut(t, manifestBase(), "sha256", "abc"))
	a.bad("create_bundle_response_digest_uppercase", "CreateBundleResponse", mut(t, manifestBase(), "sha256", strings.ToUpper(sha256Hex)))
	a.bad("create_bundle_response_consistent_omitted", "CreateBundleResponse", mut(t, manifestBase(), "consistent", nil))
	a.bad("create_bundle_response_negative_head", "CreateBundleResponse", mut(t, manifestBase(), "journalHead", -1))
	a.bad("replay_bundle_operation_omitted", "ReplayBundleRequest", mut(t, replayBase(), "operationId", nil))
	a.bad("replay_bundle_digest_omitted", "ReplayBundleRequest", mut(t, replayBase(), "sha256", nil))
	a.bad("replay_bundle_digest_malformed", "ReplayBundleRequest", mut(t, replayBase(), "sha256", "sha256:"+sha256Hex))
	a.bad("replay_bundle_unknown_field", "ReplayBundleRequest", mut(t, replayBase(), "force", true))
	a.bad("replay_bundle_response_unknown_relation", "ReplayBundleResponse", mut(t, replayResponseBase(), "conflicting.0.relation", "rewound"))
	a.badGo("replay_bundle_response_conflict_equal_values", "ReplayBundleResponse", mut(t, replayResponseBase(), "conflicting.0.bundle", c2))
	a.badGo("replay_bundle_response_conflict_repeated", "ReplayBundleResponse", mut(t, replayResponseBase(), "conflicting.1", obj{"ref": "refs/heads/main", "current": c3, "bundle": c1, "relation": "diverged"}))
	a.bad("replay_bundle_response_conflicting_null", "ReplayBundleResponse", mut(t, replayResponseBase(), "conflicting", null))
	a.bad("replay_bundle_response_negative_created", "ReplayBundleResponse", mut(t, replayResponseBase(), "created", -1))
	a.bad("restore_fence_fenced_omitted", "RestoreFenceRequest", obj{"repositoryId": "repo_01"})
	a.bad("restore_fence_fenced_string", "RestoreFenceRequest", obj{"repositoryId": "repo_01", "fenced": "true"})
	a.bad("restore_fence_response_changed_omitted", "RestoreFenceResponse", obj{"repositoryId": "repo_01", "fenced": true, "workerInstance": "gitworker-7f9c"})
	return a
}

// Test_unit_git_contract_object_ops (GR-13, GR-15, T-160): the object-level
// worker operations, their fixtures and the framing of bundle bodies.
func Test_unit_git_contract_object_ops(t *testing.T) {
	runArea(t, "object_ops")

	t.Run("operations are control-plane routes with codes defined", func(t *testing.T) {
		want := map[string]string{"isAncestor": "/objects/is-ancestor", "hasCommit": "/objects/has-commit", "commitTime": "/objects/commit-time",
			"readFiles": "/files", "createBundle": "/bundles/create", "replayBundle": "/bundles/replay", "setRestoreFence": "/restore-fence"}
		got := map[string]Operation{}
		for _, op := range Operations {
			got[op.ID] = op
		}
		for id, suffix := range want {
			op, ok := got[id]
			if !ok || op.Caller != CallerControl || op.Method != "POST" || op.Path != "/repositories/{repositoryId}"+suffix {
				t.Errorf("%s: %+v", id, op)
			}
		}
		for _, code := range []string{"commit_not_found", "repository_empty", "bundle_too_large", "restore_fence_required"} {
			if _, ok := Status(domain.ErrorCode(code)); !ok {
				t.Errorf("code %s has no status", code)
			}
		}
	})

	t.Run("file reads are bounded and checked", func(t *testing.T) {
		if MaxReadFilePaths != 64 || MaxReadFileBytes != 256<<10 || MaxReadFilesBytes != 1<<20 {
			t.Fatal("the file read bounds changed")
		}
		file := func(path, content string) DefinitionFile {
			sum := sha256.Sum256([]byte(content))
			return DefinitionFile{Path: path, Content: content, SHA256: hex.EncodeToString(sum[:])}
		}
		ok := ReadFilesResponse{Files: []DefinitionFile{file(".kodama/a.yaml", strings.Repeat("a", MaxReadFileBytes))}}
		if err := ok.Validate(); err != nil {
			t.Fatalf("a file at the bound: %v", err)
		}
		over := ReadFilesResponse{Files: []DefinitionFile{file(".kodama/a.yaml", strings.Repeat("a", MaxReadFileBytes+1))}}
		if over.Validate() == nil {
			t.Error("a file over the bound")
		}
		var many []DefinitionFile
		for i := 0; i < 5; i++ {
			many = append(many, file(fmt.Sprintf(".kodama/f%d.yaml", i), strings.Repeat("a", MaxReadFileBytes)))
		}
		if (ReadFilesResponse{Files: many}).Validate() == nil {
			t.Error("files over the total bound")
		}
		if (ReadFilesResponse{Files: many[:4]}).Validate() != nil {
			t.Error("files at the total bound")
		}
		var sixtyFive []DefinitionFile
		for i := 0; i <= MaxReadFilePaths; i++ {
			sixtyFive = append(sixtyFive, file(fmt.Sprintf(".kodama/f%03d.yaml", i), "x"))
		}
		if (ReadFilesResponse{Files: sixtyFive}).Validate() == nil || (ReadFilesResponse{Files: sixtyFive[:MaxReadFilePaths]}).Validate() != nil {
			t.Error("the file count bound")
		}
	})

	t.Run("a frame is one JSON line followed by the raw bundle", func(t *testing.T) {
		line, err := FrameLine(ReplayBundleRequest{RepositoryID: "repo_01", OperationID: "op_20"})
		if err != nil || line[len(line)-1] != '\n' || bytes.Count(line, []byte("\n")) != 1 {
			t.Fatalf("frame line %q: %v", line, err)
		}
		bundle := []byte("# v2 git bundle\n\nPACK\x00\n\n")
		r := bufio.NewReader(bytes.NewReader(append(append([]byte{}, line...), bundle...)))
		got, err := ReadFrameLine(r)
		if err != nil {
			t.Fatal(err)
		}
		req, err := DecodeRequest[ReplayBundleRequest](got)
		if err != nil || req.OperationID != "op_20" || req.SHA256 != nil {
			t.Fatalf("decoded %+v: %v", req, err)
		}
		rest := new(bytes.Buffer)
		_, _ = rest.ReadFrom(r)
		if !bytes.Equal(rest.Bytes(), bundle) {
			t.Errorf("the bundle bytes were not left untouched: %q", rest.Bytes())
		}
		// A body without a terminated line, and an oversized line, are refused.
		if _, err := ReadFrameLine(bufio.NewReader(strings.NewReader(`{"a":1}`))); err == nil {
			t.Error("accepted an unterminated line")
		}
		if _, err := ReadFrameLine(bufio.NewReader(strings.NewReader(strings.Repeat("x", MaxFrameLineBytes+10) + "\n"))); err == nil {
			t.Error("accepted an oversized line")
		}
		if _, err := ReadFrameLine(bufio.NewReaderSize(strings.NewReader(""), 16)); err == nil {
			t.Error("accepted an empty body")
		}
		// A long but bounded line is read in full with a small buffer.
		long, _ := json.Marshal(map[string]string{"k": strings.Repeat("v", 100000)})
		if got, err := ReadFrameLine(bufio.NewReaderSize(bytes.NewReader(append(long, '\n')), 16)); err != nil || !bytes.Equal(got, long) {
			t.Errorf("long line: %v", err)
		}
	})
}
