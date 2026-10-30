package deliveryhttp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	setDigestHTTP = "sha256:" + strings.Repeat("d", 64)
	commitOne     = strings.Repeat("1", 40)
	commitTwo     = strings.Repeat("2", 40)
)

// TestSourceSetRegistrationBodies (DQ-17): exactly one of source and sourceSet
// names the candidate; a set must be valid and ordered by repository; the
// converted request is what the use case compares with the attempt's pin.
func TestSourceSetRegistrationBodies(t *testing.T) {
	img := Image{ArtifactId: "art_1", Digest: "sha256:" + strings.Repeat("b", 64)}
	src := &Source{RepositoryUrl: "https://git.example.invalid/r.git", Commit: commitOne}
	good := &DeploymentSourceSet{Digest: setDigestHTTP, Commits: []DeploymentSourceCommit{{RepositoryId: "repo_api", Commit: commitOne}, {RepositoryId: "repo_web", Commit: commitTwo}}}

	req, err := registrationRequest(src, nil, img)
	if err != nil || req.Source != (delivery.Source{RepositoryURL: src.RepositoryUrl, Commit: commitOne}) || req.Image.ArtifactID != "art_1" {
		t.Fatalf("a single source converts as before: %+v %v", req, err)
	}
	req, err = registrationRequest(nil, good, img)
	if err != nil || req.Source.SourceSet.Digest != setDigestHTTP || len(req.Source.SourceSet.Commits()) != 2 || req.Source.RepositoryURL != "" || req.Source.Commit != "" {
		t.Fatalf("a source set converts: %+v %v", req, err)
	}
	unordered := &DeploymentSourceSet{Digest: setDigestHTTP, Commits: []DeploymentSourceCommit{good.Commits[1], good.Commits[0]}}
	for name, c := range map[string]struct {
		src *Source
		set *DeploymentSourceSet
	}{
		"neither":        {nil, nil},
		"both":           {src, good},
		"an empty set":   {nil, &DeploymentSourceSet{Digest: setDigestHTTP}},
		"a bad digest":   {nil, &DeploymentSourceSet{Digest: "sha256:x", Commits: good.Commits}},
		"an unordered":   {nil, unordered},
		"a branch":       {nil, &DeploymentSourceSet{Digest: setDigestHTTP, Commits: []DeploymentSourceCommit{{RepositoryId: "repo_api", Commit: "main"}}}},
		"a repeated one": {nil, &DeploymentSourceSet{Digest: setDigestHTTP, Commits: []DeploymentSourceCommit{good.Commits[0], good.Commits[0]}}},
	} {
		if _, err := registrationRequest(c.src, c.set, img); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestSourceSetDeploymentRepresentation: a deployment of a source-set run shows
// its set and a null source; a single-source deployment keeps its source member
// and has no sourceSet member.
func TestSourceSetDeploymentRepresentation(t *testing.T) {
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	reg := delivery.Registration{
		Scope: domain.Scope{TenantID: "t_a", ProjectID: "p_1"}, Environment: "env_1", ConfigDigest: runtimev1.Digest("sha256:" + strings.Repeat("a", 64)), Namespace: "preview",
		Source:   delivery.Source{RepositoryURL: "https://git.example.invalid/r.git", Commit: commitOne},
		Image:    delivery.Image{ArtifactID: "art_1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("b", 64))},
		Producer: delivery.Producer{Attempt: "att_build", Run: "run_build", Step: "build", ToolImage: "registry.example.invalid/tools@sha256:" + strings.Repeat("c", 64)},
		Run:      "run_deploy", Step: "deploy", ActionKey: "act:0123456789abcdef", Attempt: "att_deploy",
	}
	single, err := delivery.NewDeployment(reg, now)
	if err != nil {
		t.Fatal(err)
	}
	set, _ := delivery.NewSourceSet(setDigestHTTP, []delivery.SourceCommit{{RepositoryID: "repo_web", Commit: commitTwo}, {RepositoryID: "repo_api", Commit: commitOne}})
	reg.Source = delivery.Source{RepositoryURL: "https://git.example.invalid/git/t/p/api.git", Commit: commitOne, SourceSet: set}
	withSet, err := delivery.NewDeployment(reg, now)
	if err != nil {
		t.Fatal(err)
	}
	keys := func(d delivery.Deployment) map[string]json.RawMessage {
		raw, err := json.Marshal(deploymentDTO(d))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(raw, &m)
		return m
	}
	var got Source
	if m := keys(single); json.Unmarshal(m["source"], &got) != nil || got != (Source{RepositoryUrl: "https://git.example.invalid/r.git", Commit: commitOne}) {
		t.Fatalf("a single source: %s", m["source"])
	} else if _, ok := m["sourceSet"]; ok {
		t.Fatal("a single-source deployment has no sourceSet member")
	}
	m := keys(withSet)
	var gotSet DeploymentSourceSet
	var primary Source
	if json.Unmarshal(m["source"], &primary) != nil || primary != (Source{RepositoryUrl: "https://git.example.invalid/git/t/p/api.git", Commit: commitOne}) {
		t.Fatalf("source is the primary source's repository and commit, never null: %s", m["source"])
	}
	if json.Unmarshal(m["sourceSet"], &gotSet) != nil || gotSet.Digest != setDigestHTTP || len(gotSet.Commits) != 2 ||
		gotSet.Commits[0] != (DeploymentSourceCommit{RepositoryId: "repo_api", Commit: commitOne}) || gotSet.Commits[1] != (DeploymentSourceCommit{RepositoryId: "repo_web", Commit: commitTwo}) {
		t.Fatalf("a source set: sourceSet %s", m["sourceSet"])
	}
	// The members every earlier response had are all still there, in kind.
	for _, k := range []string{"id", "environmentId", "configurationDigest", "namespace", "source", "image", "producer", "runId", "stepId", "actionKey", "attemptId",
		"status", "reason", "outcomeUnknown", "objects", "cleanup", "version", "createdAt", "updatedAt"} {
		if _, ok := m[k]; !ok {
			t.Errorf("the member %q is missing from a source-set deployment", k)
		}
	}
}
