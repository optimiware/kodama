package kit

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/agents"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// ERPSetup selects the scenario's environment and bounds.
type ERPSetup struct {
	// TTL is the preview environment's TTL (default 1 hour; the API's minimum is 60 s).
	TTL time.Duration
	// ReadyTimeout bounds the apply script's readiness wait (default 240 s).
	ReadyTimeout int
	// Label names the namespace.
	Label string
}

// ERP is one scenario of the example pipeline: the project's preview
// environment over a namespace of the scope's kind cluster, the registered
// profile and the registered pipeline.
type ERP struct {
	E       *Env
	NS      *Namespace
	EnvID   string
	Profile string
	// Def is the candidate pipeline, PreviewDef the promotion that deploys its image.
	Def        client.PipelineDefinition
	PreviewDef client.PipelineDefinition
	BuildDef   client.PipelineDefinition
	Opts       ERPOptions
}

// NewERP creates the namespace and the scoped kubeconfig secret, defines the
// preview environment over them, registers the agent profile (the tools image
// as the harness image, the curl-form agent as its instructions), the ERP
// pipeline and the project's one cleanup pipeline. Options.Delivery is required.
func (e *Env) NewERP(t *testing.T, s ERPSetup) *ERP {
	t.Helper()
	if e.Tools.Digest == "" {
		t.Fatal("the ERP scenarios need Options.Delivery")
	}
	if s.TTL == 0 {
		s.TTL = time.Hour
	}
	if s.ReadyTimeout == 0 {
		s.ReadyTimeout = 240
	}
	if s.Label == "" {
		s.Label = "erp"
	}
	cl := RequireCluster(t)
	ns := cl.NewNamespace(t, t.TempDir(), s.Label)
	raw, err := readFile(ns.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	secret := "preview-kubeconfig-" + randHex(3)
	e.CreateSecret(secret, "kubeconfig", raw)
	b := e.MustDo("engineer", "POST", e.ProjectPath()+"/environments", map[string]any{
		"name": "preview-" + randHex(3), "kind": "preview", "target": "kind", "namespace": ns.Name,
		"kubeconfig": map[string]any{"name": "KUBECONFIG", "ref": "secret://project/" + secret, "delivery": "file"},
		"config":     map[string]any{"REPLICAS": "1"}, "previewTtlSeconds": int(s.TTL / time.Second),
	}, map[string]string{"Idempotency-Key": "env-" + secret}, http.StatusCreated)
	envID := IDOf(t, b)

	d := e.ProfileDraft("erp-coder", &agents.Limits{WallSeconds: 600, MaxToolCalls: ptr(int64(5))})
	d.Image = e.Tools.Ref()
	d.Instructions = ERPInstructions(e.Front.Origin, e.Stack.Front.URL)
	profile := e.RegisterDraft(d)

	p := &ERP{E: e, NS: ns, EnvID: envID, Profile: profile, Opts: ERPOptions{
		Profile: profile, Environment: envID, Tools: e.Tools, ServerURL: e.Stack.Front.URL, RepositoryURL: e.Repo.URL, ReadyTimeout: s.ReadyTimeout,
	}}
	p.Def = e.Register("inventory-candidate", ERPInputs(), e.ERPSteps(p.Opts)...)
	p.BuildDef = e.Register("inventory-build", ERPInputs(), e.BuildSteps(p.Opts)...)
	p.PreviewDef = e.Register("inventory-preview", PreviewInputs(), e.PreviewSteps(p.Opts)...)
	e.Register("kodama-cleanup", map[string]any{"deployment_id": map[string]any{"type": "string", "required": true}}, e.CleanupSteps(envID, e.Tools, e.Stack.Front.URL)...)
	return p
}

func ptr[T any](v T) *T { return &v }

// Start admits a run of the pipeline for the item.
func (p *ERP) Start(key string, item client.WorkItem) client.Run { return p.StartWith(key, item, "") }

// StartWith is Start with the conformance report mode the agent declares (see
// ERPInputs).
func (p *ERP) StartWith(key string, item client.WorkItem, report string) client.Run {
	p.E.T.Helper()
	in := map[string]any{"registration": p.E.Registration}
	if report != "" {
		in["report"] = report
	}
	return p.E.StartRun(key, item, p.Def.Id, in)
}

// Lite is a candidate a human wrote and the build run that verified its image.
type Lite struct {
	Commit   string
	BuildRun client.Run
	ImageID  string
	ImageRef string
}

// HumanCandidate has an engineer commit the candidate's files on the work
// item's branch and push them to the real Git host, then runs the build alone at
// that commit and waits for the verified image: the fast way to a deployable
// candidate for the scenarios about delivery rather than about the agent.
func (p *ERP) HumanCandidate(key string, item client.WorkItem) Lite {
	t := p.E.T
	t.Helper()
	var files []gitFile
	for _, f := range CandidateFiles {
		files = append(files, gitFile{Path: f.Path, Content: f.Content})
	}
	commit := p.E.Repo.HumanPush(t, "refs/heads/kodama/work/"+item.Id, "a human writes the candidate", toFiles(files)...)
	run := p.E.StartRunAt(key, item, p.BuildDef.Id, commit, map[string]any{"registration": p.E.Registration})
	f := p.E.World.WaitRun(run.Id, 5*time.Minute)
	if f.Status != client.RunStatusSucceeded {
		t.Fatalf("the build of %s is %s (%s)", commit, f.Status, f.Reason)
	}
	b := p.E.StepOf(run.Id, "build")
	return Lite{Commit: commit, BuildRun: f, ImageID: Output(b, "image"), ImageRef: Output(b, "image_ref")}
}

// PreviewLite admits the preview run of a human candidate's verified image.
func (p *ERP) PreviewLite(key string, item client.WorkItem, c Lite) client.Run {
	t := p.E.T
	t.Helper()
	in := map[string]any{"image": c.ImageID, "image_ref": c.ImageRef, "commit": c.Commit}
	return p.E.StartRunAt(key, item, p.PreviewDef.Id, c.Commit, in)
}

// Preview admits the preview run for the item from a succeeded candidate run:
// the verified image's artifact, its reference and the candidate commit are the
// run's inputs, and the candidate commit is its pinned source.
func (p *ERP) Preview(key string, item client.WorkItem, candidate client.Run) client.Run {
	t := p.E.T
	t.Helper()
	build := p.E.StepOf(candidate.Id, "build")
	impl := p.E.StepOf(candidate.Id, "implement")
	if impl.SourceCommit == nil {
		t.Fatalf("the writer published no commit: %+v", impl)
	}
	in := map[string]any{"image": Output(build, "image"), "image_ref": Output(build, "image_ref"), "commit": *impl.SourceCommit}
	req := map[string]any{"workItemId": item.Id, "definitionId": p.PreviewDef.Id, "contentRevision": int64(item.ContentRevision), "sourceCommit": *impl.SourceCommit, "inputs": in}
	code, b := p.E.Do("engineer", "POST", p.E.ProjectPath()+"/runs", req, map[string]string{"Idempotency-Key": key})
	if code != http.StatusCreated {
		t.Fatalf("admit the preview run: HTTP %d: %s", code, b)
	}
	var r client.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// Deployments lists the project's deployments through the public API.
func (e *Env) Deployments() []client.Deployment {
	t := e.T
	t.Helper()
	var code int
	var b []byte
	// A busy shared database answers 503 now and then; the listing is read-only.
	for attempt := 0; attempt < 8; attempt++ {
		code, b = e.Do("engineer", "GET", e.ProjectPath()+"/deployments", nil, nil)
		if code != http.StatusServiceUnavailable {
			break
		}
		time.Sleep(time.Second)
	}
	if code != http.StatusOK {
		t.Fatalf("list deployments: %d %s", code, b)
	}
	var page struct{ Items []client.Deployment }
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	return page.Items
}

type gitFile struct{ Path, Content string }

func toFiles(in []gitFile) []gitfx.File {
	out := make([]gitfx.File, 0, len(in))
	for _, f := range in {
		out = append(out, gitfx.File{Path: f.Path, Content: f.Content})
	}
	return out
}
