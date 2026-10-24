package kit

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// Stage is the staging example (docs/examples/workflows/staging-and-acceptance.md)
// on the local composition: a stage environment over a namespace of the scope's
// kind cluster, a build that verifies a candidate's image, the stage run that
// deploys exactly that digest, and the acceptance run (end-to-end check,
// exploratory QA, deployment-bound human review).
//
// The example is one pipeline; the validator and the QA mapping force two runs,
// both for the reason the M2 preview needed a promotion form: an ociArtifact
// reference is accepted nowhere in env or args, and the mapping reads the
// deployment it judges from a pinned run INPUT (never from the report), so the
// stage deployment is created by one run and judged by the next (see
// docs/acceptance/m3.md, deviation 2).
type Stage struct {
	E     *Env
	NS    *m2.Namespace
	EnvID string
	Opts  m2.ERPOptions

	BuildDef, StageDef, AcceptDef client.PipelineDefinition
}

// NewStage creates the namespace and the scoped kubeconfig secret, defines the
// stage environment over them and registers the three pipelines. Options.Delivery
// is required (the tools image runs every step) and so is the kind cluster.
func (e *Env) NewStage(t *testing.T) *Stage { return e.NewStageWith(t, 0) }

// NewStageWith is NewStage; a positive ttl makes the environment a PREVIEW
// environment that expires after it (the API's minimum is 60 s) and registers
// the project's one cleanup pipeline for it, so the expiry sweep can clean the
// deployment. The project's cleanup pipeline names one environment: use it in an
// installation of its own.
func (e *Env) NewStageWith(t *testing.T, ttl time.Duration) *Stage {
	t.Helper()
	if e.Tools.Digest == "" {
		t.Fatal("the stage needs Options.Delivery")
	}
	cl := m2.RequireCluster(t)
	ns := cl.NewNamespace(t, t.TempDir(), "stage")
	raw, err := os.ReadFile(ns.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	secret := "stage-kubeconfig-" + randHex(3)
	e.CreateSecret(secret, "kubeconfig", raw)
	env := map[string]any{
		"name": "stage-" + randHex(3), "kind": "stage", "target": "kind", "namespace": ns.Name,
		"kubeconfig": map[string]any{"name": "KUBECONFIG", "ref": "secret://project/" + secret, "delivery": "file"},
		"config":     map[string]any{"REPLICAS": "1"},
	}
	if ttl > 0 {
		env["kind"], env["name"], env["previewTtlSeconds"] = "preview", "preview-"+randHex(3), int(ttl/time.Second)
	}
	b := e.MustDo("engineer", "POST", e.ProjectPath()+"/environments", env, map[string]string{"Idempotency-Key": "env-" + secret}, http.StatusCreated)
	s := &Stage{E: e, NS: ns, EnvID: m2.IDOf(t, b)}
	s.Opts = m2.ERPOptions{
		Environment: s.EnvID, Tools: e.Tools, ServerURL: e.Stack.Front.URL, RepositoryURL: e.Repo.URL, ReadyTimeout: 240,
	}
	s.BuildDef = e.Register("inventory-build", m2.ERPInputs(), e.BuildSteps(s.Opts)...)
	s.StageDef = e.Register("inventory-stage", m2.PreviewInputs(), e.PreviewSteps(s.Opts)...)
	s.AcceptDef = e.Register("inventory-acceptance", AcceptInputs(), s.AcceptSteps()...)
	if ttl > 0 {
		e.Register("kodama-cleanup", map[string]any{"deployment_id": map[string]any{"type": "string", "required": true}}, e.CleanupSteps(s.EnvID, e.Tools, e.Stack.Front.URL)...)
	}
	return s
}

// AcceptInputs are the declared inputs of the acceptance pipeline: everything
// the QA attempt pins about the deployment it inspects.
func AcceptInputs() map[string]any {
	in := map[string]any{"mode": map[string]any{"type": "string", "default": ""}}
	for _, n := range []string{DeploymentInput, "work_item", "revision", "environment_id", "config_digest", "commit", "image_digest"} {
		in[n] = map[string]any{"type": "string", "required": true}
	}
	return in
}

// AcceptSteps are the staging example's e2e, exploratory-qa and business-review
// steps. The QA step declares the stage environment (the configuration pin the
// mapping verifies) and a read workspace (an artifact produced without source
// cannot be bound into a gate subject); the gate binds the deployment.
func (s *Stage) AcceptSteps() []any {
	tools := s.Opts.Tools
	read := map[string]any{"access": "read", "from": "run"}
	e2e := map[string]any{
		"id": "e2e", "kind": "container", "image": tools.Ref(), "workspace": read, "timeoutSeconds": 300,
		"command": []string{"/bin/sh", "-ec"}, "args": []string{"cd /workspace && sh ci/e2e.sh"},
		"env":     map[string]string{"DEPLOYMENT_ID": "${{ inputs.deployment }}", "IMAGE_DIGEST": "${{ inputs.image_digest }}"},
		"outputs": map[string]any{"report": map[string]any{"type": "artifact", "path": "/kodama/outputs/e2e.json"}},
	}
	qa := map[string]any{
		"id": "qa", "kind": "container", "needs": []string{"e2e"}, "image": tools.Ref(), "workspace": read, "timeoutSeconds": 300,
		"environment": s.EnvID,
		"command":     []string{"/bin/sh", "-ec"}, "args": []string{qaScript},
		"env": map[string]string{
			"KODAMA_SERVER_URL": s.Opts.ServerURL, "QA_MODE": "${{ inputs.mode }}",
			"WORK_ITEM_ID": "${{ inputs.work_item }}", "CONTENT_REVISION": "${{ inputs.revision }}", "DEPLOYMENT_ID": "${{ inputs.deployment }}",
			"ENVIRONMENT_ID": "${{ inputs.environment_id }}", "CONFIG_DIGEST": "${{ inputs.config_digest }}",
			"CANDIDATE_COMMIT": "${{ inputs.commit }}", "IMAGE_DIGEST": "${{ inputs.image_digest }}",
		},
		"outputs": map[string]any{
			ReportOutput: map[string]any{"type": "artifact", "path": "/kodama/outputs/qa.json"},
			"trace":      map[string]any{"type": "artifact", "path": "/kodama/outputs/trace.json"},
		},
	}
	gate := m2.GateStep("business-review", []string{"${{ steps.e2e.outputs.report }}", "${{ steps.qa.outputs.report }}"}, "e2e", "qa")
	gate["gate"].(map[string]any)["deploymentId"] = "${{ inputs.deployment }}"
	return []any{e2e, qa, gate}
}

// CandidateFiles are the candidate a human (or, later, an agent) wrote: the M2
// quality/build scripts and manifests plus the end-to-end check.
func CandidateFiles(extra ...gitfx.File) []gitfx.File {
	var files []gitfx.File
	for _, f := range m2.CandidateFiles {
		files = append(files, gitfx.File{Path: f.Path, Content: f.Content})
	}
	files = append(files, gitfx.File{Path: "ci/e2e.sh", Content: e2eScript})
	return append(files, extra...)
}

// Candidate has an engineer commit the candidate on the work item's branch (on
// top of its tip) and runs the build alone at that commit, waiting for the
// verified image. extra files let a rework commit differ from the last.
func (s *Stage) Candidate(key string, item client.WorkItem, extra ...gitfx.File) m2.Lite {
	t := s.E.T
	t.Helper()
	commit := s.E.Repo.HumanPush(t, "refs/heads/kodama/work/"+item.Id, "an engineer writes the candidate ("+key+")", CandidateFiles(extra...)...)
	run := s.E.StartRunAt(key, item, s.BuildDef.Id, commit, map[string]any{"registration": s.E.Registration})
	f := s.E.World.WaitRun(run.Id, 6*time.Minute)
	if f.Status != client.RunStatusSucceeded {
		t.Fatalf("the build of %s is %s (%s)", commit, f.Status, f.Reason)
	}
	b := s.E.StepOf(run.Id, "build")
	return m2.Lite{Commit: commit, BuildRun: f, ImageID: m2.Output(b, "image"), ImageRef: m2.Output(b, "image_ref")}
}

// Stage deploys the candidate's verified image to the stage environment and
// returns the succeeded deployment.
func (s *Stage) Stage(key string, item client.WorkItem, c m2.Lite) (client.Run, client.Deployment) {
	t := s.E.T
	t.Helper()
	run := s.E.StartRunAt(key, item, s.StageDef.Id, c.Commit, map[string]any{"image": c.ImageID, "image_ref": c.ImageRef, "commit": c.Commit})
	f := s.E.World.WaitRun(run.Id, 8*time.Minute)
	if f.Status != client.RunStatusSucceeded {
		t.Fatalf("the stage run is %s (%s)", f.Status, f.Reason)
	}
	for _, d := range s.E.Deployments() {
		if d.RunId == run.Id {
			if d.Status != "succeeded" {
				t.Fatalf("deployment %+v", d)
			}
			return f, d
		}
	}
	t.Fatalf("run %s recorded no deployment", run.Id)
	return f, client.Deployment{}
}

// Accept admits the acceptance run (e2e, QA, deployment-bound gate) for the
// staged deployment of the candidate. mode is the QA report: "clean" or "defect".
func (s *Stage) Accept(key string, item client.WorkItem, c m2.Lite, d client.Deployment, mode string) client.Run {
	t := s.E.T
	t.Helper()
	in := map[string]any{
		"mode": mode, DeploymentInput: d.Id, "work_item": item.Id, "revision": fmt.Sprint(item.ContentRevision), "environment_id": d.EnvironmentId,
		"config_digest": string(d.ConfigurationDigest), "commit": c.Commit, "image_digest": string(d.Image.Digest),
	}
	return s.E.StartRunAt(key, item, s.AcceptDef.Id, c.Commit, in)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}
