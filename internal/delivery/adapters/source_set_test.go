package adapters_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// v3Context returns the payload of a published runtime v3 context fixture: two
// repositories (backend written, frontend read) with their source set digest.
func v3Context(t *testing.T, name string) (payload []byte, doc map[string]any) {
	t.Helper()
	raw, err := os.ReadFile("../../../contracts/runtime/v3/fixtures/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Name == name {
			if err := json.Unmarshal(c.Payload, &doc); err != nil {
				t.Fatal(err)
			}
			return c.Payload, doc
		}
	}
	t.Fatal("the context fixture is missing")
	return nil, nil
}

type fakeRuns struct {
	attempts []*execution.Attempt
}

func (f fakeRuns) RunOf(_ context.Context, scope domain.Scope, id domain.AttemptID) (domain.RunID, error) {
	for _, a := range f.attempts {
		if a.ID == id && a.Scope == scope {
			return a.RunID, nil
		}
	}
	return "", execution.ErrNotFound
}

func (f fakeRuns) Get(context.Context, domain.Scope, domain.RunID) (*execution.Aggregate, error) {
	return &execution.Aggregate{Attempts: f.attempts}, nil
}

// TestPinnedSourceSetFromTheV3Context (DQ-17): the source set an attempt of a
// source-set run works from is read from its immutable runtime v3 context: the
// digest of the pinned set and, per repository, the head commit the attempt
// began from. Every other attempt has none, and the v3 context stays readable
// to the deployment checks that need only its shared members.
func TestPinnedSourceSetFromTheV3Context(t *testing.T) {
	payload, doc := v3Context(t, "context_v3_work_item_writer_two_repositories")
	scope := domain.Scope{TenantID: domain.TenantID(doc["tenantId"].(string)), ProjectID: domain.ProjectID(doc["projectId"].(string))}
	attempt := execution.Attempt{Scope: scope, ID: domain.AttemptID(doc["attemptId"].(string)), RunID: domain.RunID(doc["runId"].(string)), StepID: doc["stepId"].(string), Context: payload}
	exec, err := adapters.NewExecution(fakeRuns{attempts: []*execution.Attempt{&attempt}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	pin, found, err := exec.PinnedSourceSet(ctx, scope, attempt.ID)
	if err != nil || !found {
		t.Fatalf("pinned source set: %v %v", found, err)
	}
	set := pin.Set
	want, _ := delivery.NewSourceSet("sha256:93ce5a7a7f6b65a2c28c95bbab4373c7419483d83c5ce1dce32303ebe7d20c59", []delivery.SourceCommit{
		{RepositoryID: "repo_01", Commit: strings.Repeat("1", 40)}, {RepositoryID: "repo_02", Commit: strings.Repeat("2", 40)}})
	if set != want {
		t.Fatalf("set %+v, want %+v", set.Commits(), want.Commits())
	}
	// The primary is the writable source with the smallest repository ID (repo_01,
	// "backend"), with its repository URL and the head the attempt began from.
	if pin.PrimaryURL != "https://git.example.invalid/git/t_acme/p_inventory/backend.git" || pin.PrimaryCommit != strings.Repeat("1", 40) {
		t.Fatalf("primary: %+v", pin)
	}

	pinned, err := exec.Pinned(ctx, scope, attempt.ID)
	if err != nil || pinned.AttemptID != attempt.ID || pinned.ActionKey != doc["actionKey"] || pinned.Workspace.Source != nil || pinned.Workspace.Access == runtimev1.AccessNone || pinned.Scope() != scope {
		t.Fatalf("the shared members of a v3 context: %+v %v", pinned, err)
	}
	if out, err := exec.Produced(ctx, scope, attempt.ID, "art_1"); err != nil || out.Accepted || out.ToolImage == "" || out.Step != attempt.StepID {
		t.Fatalf("produced: %+v %v", out, err)
	}

	t.Run("a foreign or unknown attempt is not found", func(t *testing.T) {
		other := domain.Scope{TenantID: "t_other", ProjectID: scope.ProjectID}
		if _, _, err := exec.PinnedSourceSet(ctx, other, attempt.ID); !errors.Is(err, delivery.ErrNotFound) {
			t.Fatalf("foreign tenant: %v", err)
		}
		if _, _, err := exec.PinnedSourceSet(ctx, scope, "att_nope"); !errors.Is(err, delivery.ErrNotFound) {
			t.Fatalf("unknown attempt: %v", err)
		}
	})
	t.Run("a source-free run has no set", func(t *testing.T) {
		raw, free := v3Context(t, "context_v3_cleanup_run_manual_origin_source_free_without_git")
		fscope := domain.Scope{TenantID: domain.TenantID(free["tenantId"].(string)), ProjectID: domain.ProjectID(free["projectId"].(string))}
		at := execution.Attempt{Scope: fscope, ID: domain.AttemptID(free["attemptId"].(string)), RunID: domain.RunID(free["runId"].(string)), StepID: free["stepId"].(string), Context: raw}
		e2, _ := adapters.NewExecution(fakeRuns{attempts: []*execution.Attempt{&at}})
		if _, found, err := e2.PinnedSourceSet(ctx, fscope, at.ID); err != nil || found {
			t.Fatalf("a source-free context: %v %v", found, err)
		}
	})
	t.Run("an unreadable context is an outage, never a set", func(t *testing.T) {
		bad := attempt
		bad.Context = []byte(`{"contextVersion":3,"bogus":true}`)
		e2, _ := adapters.NewExecution(fakeRuns{attempts: []*execution.Attempt{&bad}})
		if _, _, err := e2.PinnedSourceSet(ctx, scope, bad.ID); !errors.Is(err, delivery.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a context of another version has no set", func(t *testing.T) {
		v1 := runtimev1.Context{TenantID: scope.TenantID, ProjectID: scope.ProjectID, RunID: "run_1", StepID: "deploy", AttemptID: "att_v1", ActionKey: "act:1",
			Inputs: map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{}, Secrets: []runtimev1.SecretBinding{}}
		raw, _ := json.Marshal(v1)
		at := execution.Attempt{Scope: scope, ID: "att_v1", RunID: "run_1", StepID: "deploy", Context: raw}
		e2, _ := adapters.NewExecution(fakeRuns{attempts: []*execution.Attempt{&at}})
		if _, found, err := e2.PinnedSourceSet(ctx, scope, "att_v1"); err != nil && !errors.Is(err, delivery.ErrUnavailable) || found {
			t.Fatalf("a v1 context: %v %v", found, err)
		}
	})
}

type fakeLookup struct{ ref artifacts.Reference }

func (f fakeLookup) Reference(context.Context, domain.Scope, domain.ArtifactID) (artifacts.Reference, error) {
	return f.ref, nil
}
func (fakeLookup) Pin(context.Context, domain.Scope, domain.ArtifactID, artifacts.PinOwner) error {
	return nil
}
func (fakeLookup) Unpin(context.Context, domain.Scope, domain.ArtifactID, artifacts.PinOwner) error {
	return nil
}

// TestArtifactEvidenceCarriesTheSourceSet: the artifact owner's provenance of an
// image built by a source-set run reaches the deployment check as the same
// canonical set, and a single-source image keeps its repository and commit.
func TestArtifactEvidenceCarriesTheSourceSet(t *testing.T) {
	set, err := artifacts.NewSourceSet("sha256:"+strings.Repeat("d", 64), []artifacts.SourceCommit{{RepositoryID: "repo_b", Commit: strings.Repeat("2", 40)}, {RepositoryID: "repo_a", Commit: strings.Repeat("1", 40)}})
	if err != nil {
		t.Fatal(err)
	}
	scope := domain.Scope{TenantID: "t_a", ProjectID: "p_1"}
	ctx := context.Background()
	for name, c := range map[string]struct {
		prov artifacts.Provenance
		want delivery.Source
	}{
		"single": {artifacts.Provenance{RepositoryURL: "https://git.example.invalid/r.git", Commit: strings.Repeat("1", 40)},
			delivery.Source{RepositoryURL: "https://git.example.invalid/r.git", Commit: strings.Repeat("1", 40)}},
		"set": {artifacts.Provenance{SourceSet: set}, delivery.Source{SourceSet: func() delivery.SourceSet {
			s, _ := delivery.NewSourceSet(set.Digest, []delivery.SourceCommit{{RepositoryID: "repo_a", Commit: strings.Repeat("1", 40)}, {RepositoryID: "repo_b", Commit: strings.Repeat("2", 40)}})
			return s
		}()}},
	} {
		a, _ := adapters.NewArtifacts(fakeLookup{ref: artifacts.Reference{ID: "art_1", Scope: scope, Attempt: "att_1", Kind: artifacts.KindOCI, Role: artifacts.RoleOutput, Usable: true,
			Digest: artifacts.DigestOf([]byte("x")), Provenance: c.prov}})
		ev, err := a.Evidence(ctx, scope, "art_1")
		if err != nil || ev.Provenance != c.want {
			t.Errorf("%s: %+v %v", name, ev.Provenance, err)
		}
	}
}
