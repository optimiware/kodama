package artifactshttp_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/artifacts"
	artifactshttp "github.com/optimiware/kodama/internal/artifacts/http"
)

// TestSourceSetProvenanceRepresentation (DQ-17): an artifact of a source-set run
// shows its set (digest and one commit per repository) in place of a single
// source; an artifact of any other run keeps exactly its earlier members, with
// no sourceSet key.
func TestSourceSetProvenanceRepresentation(t *testing.T) {
	set, err := artifacts.NewSourceSet("sha256:"+strings.Repeat("c", 64), []artifacts.SourceCommit{
		{RepositoryID: "repo_web", Commit: strings.Repeat("2", 40)}, {RepositoryID: "repo_api", Commit: strings.Repeat("1", 40)}})
	if err != nil {
		t.Fatal(err)
	}
	d := artifacts.DigestOf([]byte("manifest"))
	res := artifacts.Reservation{Scope: pubScope, Attempt: "att_1", Run: "run_1", Step: "build", Name: "image", Kind: artifacts.KindOCI, Role: artifacts.RoleOutput,
		Digest: d, Registry: "registry.example.test/team/app", Provenance: artifacts.Provenance{SourceSet: set}}
	a, err := artifacts.NewRegistered("art_set", res, artifacts.OCIVerification{Repository: "registry.example.test/team/app", Digest: d, ManifestSize: 5, Provenance: res.Provenance}, pubT0)
	if err != nil {
		t.Fatal(err)
	}
	get := func(v artifacts.View) (map[string]json.RawMessage, artifactshttp.Artifact) {
		svc := &fakePublic{view: func(publicCall) (artifacts.View, error) { return v, nil }}
		srv := publicServer(t, svc, nil)
		r := call(t, "GET", srv.URL+"/api/v1/tenants/t_acme/projects/p_inventory/artifacts/"+string(v.Artifact.ID()), "user:alice", nil)
		if r.status != 200 {
			t.Fatalf("%d %s", r.status, r.raw)
		}
		var keys map[string]json.RawMessage
		r.json(t, &keys)
		var dto artifactshttp.Artifact
		r.json(t, &dto)
		return keys, dto
	}
	keys, dto := get(artifacts.View{Artifact: a})
	if string(keys["source"]) != "null" || dto.Source != nil || dto.SourceSet == nil {
		t.Fatalf("a source set stands in place of a source: %s", keys["source"])
	}
	if dto.SourceSet.Digest != set.Digest || len(dto.SourceSet.Commits) != 2 || dto.SourceSet.Commits[0].RepositoryId != "repo_api" ||
		dto.SourceSet.Commits[0].Commit != strings.Repeat("1", 40) || dto.SourceSet.Commits[1].RepositoryId != "repo_web" {
		t.Fatalf("the set: %+v", dto.SourceSet)
	}
	keys, dto = get(ociView(t))
	if _, ok := keys["sourceSet"]; ok || dto.SourceSet != nil || dto.Source == nil {
		t.Fatalf("a single-source artifact keeps its representation: %v", keys)
	}
}
