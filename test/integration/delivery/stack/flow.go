package stack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/oci"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/secrets"
)

// Image is a real image pushed to the scope's registry: what the OCI verifier
// fetched by digest and what the kubelet runs.
type Image struct {
	// Repository is the registered repository (host:port/path, no tag).
	Repository string
	Digest     artifacts.Digest
	// RegistryURL is the origin the repository policy authorizes.
	RegistryURL string
	// NodeRef is the digest-pinned reference the cluster's nodes pull.
	NodeRef string
}

type registryPolicy struct{ url string }

func (p registryPolicy) Authorize(context.Context, domain.Scope, domain.AttemptID, artifacts.RegistryRef) (oci.Endpoint, error) {
	return oci.Endpoint{BaseURL: p.url, Insecure: true}, nil
}

type fixedProvenance struct{ p artifacts.Provenance }

func (f fixedProvenance) Resolve(context.Context, oci.ProvenanceQuery) (artifacts.Provenance, error) {
	return f.p, nil
}

// FlowOptions vary the fixture flow.
type FlowOptions struct {
	// Image is the real image the build "produced".
	Image Image
	// Provenance overrides the trusted provenance the registry verification
	// reports for the image (a wrong repository or commit).
	Provenance *artifacts.Provenance
	// BuildFails makes the producing attempt fail instead of succeeding with
	// the image output.
	BuildFails bool
	// OutputOther makes the build's accepted result name a second artifact, so
	// the first is registered but not an accepted output of the build.
	OutputOther bool
	// Other is the second image of an OutputOther build.
	Other Image
	// Manifests are the candidate's deploy/ files (name to content), committed to
	// the repository the container checks out. Only with the Podman runtime.
	Manifests map[string]string
	// Env overrides entries of the deploy step's declared environment: the
	// override is part of the admitted definition, never of the runner.
	Env map[string]string
}

// Flow is one admitted two-step flow: a succeeded build that produced a
// registry-verified image and an admitted deploy run whose step is dispatched
// by Execute through the Podman runtime.
type Flow struct {
	Scope    domain.Scope
	Env      delivery.Environment
	Project  executiontest.Project
	BuildRun *execution.Run
	Run      *execution.Run
	Build    *deliverytest.Attempt
	Image    domain.ArtifactID
	Img      Image
	// Other is the second artifact of an OutputOther build.
	Other domain.ArtifactID
	// Source is the candidate the image was built from.
	Source delivery.Source
}

const buildStep = "{id: build, kind: container, workspace: {access: read, from: run}, image: " + pipelinetest.Image +
	", command: [/bin/sh, -ec], timeoutSeconds: 60, outputs: {image: {type: ociArtifact}}}"

// DeployOutputs are the outputs apply.sh documents; the deploy step declares
// exactly these so the result path accepts them.
const DeployOutputs = "{deployment_id: {type: string}, deployment_status: {type: string}, configuration_digest: {type: string}, " +
	"image_digest: {type: string}, source_commit: {type: string}, ready: {type: boolean}, object_count: {type: integer}}"

// DeployScript and DeleteScript are the commands of the tools image.
const (
	DeployScript = "/opt/kodama/deploy/apply.sh"
	DeleteScript = "/opt/kodama/deploy/delete.sh"
)

// yamlEnv renders an environment mapping as a YAML flow mapping of strings.
func yamlEnv(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+strconv.Quote(env[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// deployEnv is the environment of the admitted deploy step. The image artifact
// reaches the script as the pinned input of the step.
func (s *Stack) deployEnv(f *Flow, o FlowOptions) map[string]string {
	serverURL := "https://runtime.t067.invalid"
	if s.pod != nil {
		serverURL = s.pod.srv.URL
	}
	env := map[string]string{
		"KODAMA_SERVER_URL":        serverURL,
		"DEPLOY_MANIFESTS":         "/workspace/deploy",
		"DEPLOY_REPOSITORY_URL":    f.Source.RepositoryURL,
		"DEPLOY_COMMIT":            f.Source.Commit,
		"DEPLOY_IMAGE_ARTIFACT_ID": "${{ inputs.image }}",
		"DEPLOY_IMAGE":             f.Img.NodeRef,
		"DEPLOY_READY_TIMEOUT":     "240",
		"RT_RETRY_DELAY":           "0",
		"RT_ATTEMPTS":              "3",
	}
	for k, v := range o.Env {
		env[k] = v
	}
	return env
}

func (s *Stack) deployStep(f *Flow, env domain.EnvironmentID, o FlowOptions) string {
	return "{id: deploy, kind: container, workspace: {access: read, from: run}, environment: " + string(env) + ", image: " + f.Img.Repository + "@" + string(f.Img.Digest) +
		", command: [" + DeployScript + "], env: " + yamlEnv(s.deployEnv(f, o)) + ", timeoutSeconds: 600, outputs: " + DeployOutputs +
		", retry: {maxAttempts: 3, backoffSeconds: 1, on: [infrastructure]}}"
}

// Admit admits a run of the definition as the engineer.
func (s *Stack) Admit(p executiontest.Project, yaml string, inputs map[string]runtimev1.Value) *execution.Run {
	s.T.Helper()
	d := s.X.Define(p.Scope, yaml)
	commit := executiontest.CommitA
	if c, ok := s.commits[p.Item]; ok {
		commit = c
	}
	run, err := s.Exec.Admit(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t067"}, execution.AdmitCommand{
		Scope: p.Scope, WorkItemID: p.Item, DefinitionID: d.ID, ContentRevision: s.X.ContentRevision(p), SourceCommit: commit, Inputs: inputs,
	})
	must(s.T, err)
	return run
}

// FixtureImage publishes a manifest in the component fixture's bounded OCI
// registry and returns it as an Image: for tests that need a verified image
// artifact but no cluster to run it.
func (s *Stack) FixtureImage(name string) Image {
	repo := Prefix + "/" + name
	digest := s.D.Registry.Add(repo, ID("img"))
	host := s.D.Registry.Host()
	return Image{Repository: host + "/" + repo, Digest: digest, RegistryURL: "http://" + host, NodeRef: host + "/" + repo + "@" + string(digest)}
}

// NewFlow admits a build run and completes it with a registered image (verified
// against the real registry by digest) and admits the deploy run over env with
// that image as its pinned input. The deploy step is not dispatched: Execute
// runs it through the Podman runtime, DirectAttempt claims it without a
// workload for API-level tests.
func (s *Stack) NewFlow(scope domain.Scope, env delivery.Environment, o FlowOptions) *Flow {
	s.T.Helper()
	p := s.X.NewProject(scope)
	if s.pod != nil {
		s.attachSource(p, s.pod.repo.commit(o.Manifests))
	}
	f := &Flow{Scope: scope, Env: env, Project: p, Img: o.Image}
	f.BuildRun = s.Admit(p, pipelinetest.Doc(buildStep), nil)
	f.Build = s.claim(scope, f.BuildRun, false)
	s.X.Start(f.Build.Intent)
	src := f.Build.Context.Workspace.Source
	if src == nil {
		s.T.Fatal("the build has no source")
	}
	f.Source = delivery.Source{RepositoryURL: src.RepositoryURL, Commit: string(src.Commit)}
	prov := artifacts.Provenance{RepositoryURL: src.RepositoryURL, Commit: string(src.Commit)}
	if o.Provenance != nil {
		prov = *o.Provenance
	}
	f.Image = s.RegisterImage(f.Build, o.Image, prov)
	var err error
	switch {
	case o.BuildFails:
		_, err = s.X.Fail(f.Build.Intent)
	case o.OutputOther:
		f.Other = s.RegisterImage(f.Build, o.Other, prov)
		_, err = s.CommitBuild(f.Build, f.Other)
	default:
		_, err = s.CommitBuild(f.Build, f.Image)
	}
	must(s.T, err)
	f.Run = s.admitDeploy(f, env, o)
	return f
}

func (s *Stack) admitDeploy(f *Flow, env delivery.Environment, o FlowOptions) *execution.Run {
	return s.Admit(f.Project, pipelinetest.DocNamed("promote", "{image: {type: string, required: true}}", s.deployStep(f, env.ID(), o)),
		map[string]runtimev1.Value{"image": runtimev1.StringValue(string(f.Image))})
}

// Promote admits another deploy run, of another work item, that deploys the
// flow's verified image to env: the promotion of one digest, whose deploying
// attempt is never the producing attempt.
func (s *Stack) Promote(f *Flow, env delivery.Environment) *Flow {
	s.T.Helper()
	n := *f
	n.Env, n.Project = env, s.X.NewProject(f.Scope)
	if s.pod != nil {
		s.attachSource(n.Project, f.Source.Commit)
	}
	n.Run = s.admitDeploy(&n, env, FlowOptions{})
	return &n
}

// RegisterImage verifies the image with the real OCI verifier (a digest-
// addressed manifest fetch from the registry) and stores the registered
// artifact through the artifact repository for the build attempt.
func (s *Stack) RegisterImage(a *deliverytest.Attempt, img Image, prov artifacts.Provenance) domain.ArtifactID {
	s.T.Helper()
	ctx := context.Background()
	ver, err := oci.NewVerifier(oci.Config{Policy: registryPolicy{img.RegistryURL}, Provenance: fixedProvenance{prov}})
	must(s.T, err)
	run := a.Context.RunID
	v, err := ver.Verify(ctx, artifacts.OCIRequest{Scope: a.Scope, Attempt: a.ID(), Run: run, Registry: img.Repository, Digest: img.Digest})
	must(s.T, err)
	id := artifacts.RandomIDs{}.NewArtifactID()
	res := artifacts.Reservation{
		Scope: a.Scope, Attempt: a.ID(), Run: run, Step: a.Context.StepID, Name: ID("image"), Kind: artifacts.KindOCI, Role: artifacts.RoleOutput,
		Digest: img.Digest, Registry: v.Repository, Provenance: v.Provenance,
	}
	now := s.X.Clock.Now().Truncate(time.Microsecond)
	art, err := artifacts.NewRegistered(id, res, v, now)
	must(s.T, err)
	out, err := s.D.ArtStore.Reserve(ctx, artifacts.ReserveCommand{
		Artifact: art, Limits: artifacts.Limits{OutputBytes: 1 << 20, OutputFiles: 100, DiagnosticBytes: 1 << 20, DiagnosticFiles: 100, BundleBytes: 1 << 20, BundleFiles: 10},
		Actor: artifacts.Actor{Type: "workload", ID: string(a.ID())}, RequestID: "req_t067", Now: now,
	})
	must(s.T, err)
	if !out.Created {
		s.T.Fatal("the image registration replayed an existing artifact")
	}
	return id
}

// ---- attempts ----------------------------------------------------------------

// claim claims the next runnable step of the tenant, which must belong to run.
func (s *Stack) claim(scope domain.Scope, run *execution.Run, direct bool) *deliverytest.Attempt {
	s.T.Helper()
	in := s.X.MustClaim(scope.TenantID)
	if in.RunID != run.ID {
		s.T.Fatalf("claimed %s of run %s, wanted run %s: run one flow at a time", in.StepID, in.RunID, run.ID)
	}
	pinned, err := runtimev1.DecodeRequest[runtimev1.Context](in.Context)
	must(s.T, err)
	a := &deliverytest.Attempt{Intent: in, Context: pinned, Scope: scope}
	if direct {
		s.admitDirect(a)
	}
	return a
}

// DirectAttempt claims the flow's deploy step and gives it runtime authority
// without launching any workload: the API-level tests call the delivery service
// as that attempt. No command runs for it, so a retry after it is truthfully a
// not-started one.
func (s *Stack) DirectAttempt(f *Flow) *deliverytest.Attempt {
	s.T.Helper()
	return s.claim(f.Scope, f.Run, true)
}

func (s *Stack) admitDirect(a *deliverytest.Attempt) {
	s.T.Helper()
	ctx := context.Background()
	d := s.D
	cred, err := runtimeauth.GenerateCredential(nil)
	must(s.T, err)
	actions, err := deploymentActions(a.Intent.Access)
	must(s.T, err)
	_, err = s.Admission.Register(ctx, runtimeauth.RegisterCommand{Scope: a.Scope, AttemptID: a.ID(), Credential: cred, Actions: actions, Context: a.Context})
	must(s.T, err)
	sum := sha256.Sum256([]byte(a.ID()))
	h := hex.EncodeToString(sum[:])
	uid := func(off int) string {
		return h[off:off+8] + "-" + h[off+8:off+12] + "-4" + h[off+13:off+16] + "-8" + h[off+17:off+20] + "-" + h[off+20:off+32]
	}
	name := a.Intent.WorkloadName
	id := runtimev1.JobIdentity{Name: name, UID: uid(0), PodName: name + "-x7k2p", PodUID: uid(16)}
	d.Jobs.SetCluster(runtimeauth.JobEvidence{Namespace: podman.Namespace, JobName: id.Name, JobUID: id.UID, PodNamespace: podman.Namespace,
		PodName: id.PodName, PodUID: id.PodUID, PodOwnerUID: id.UID})
	resp, err := s.Admission.Admit(ctx, a.ID(), runtimev1.AdmitRequest{AdmissionCredential: cred.Reveal(), Nonce: "nnnnnnnnnnnnnnnnnnnnnn", ClientVersion: entrypoint.Version, Job: id})
	must(s.T, err)
	a.Token = resp.Token.Token
	claims, err := d.Verifier.Verify(ctx, a.Token)
	must(s.T, err)
	a.Session = claims.Session
}

// RetryDirect ends a direct attempt as the executor does for an infrastructure
// failure of a workload that was never launched (which is exactly the case: no
// workload exists for a direct attempt) and claims the step's next attempt: a new
// attempt ID and token for the same logical action key.
func (s *Stack) RetryDirect(f *Flow, a *deliverytest.Attempt) *deliverytest.Attempt {
	s.T.Helper()
	must(s.T, s.X.Observe(a.Intent, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure,
		Reason: execution.ReasonInfrastructure, Launch: execution.LaunchNotStarted}))
	must(s.T, s.X.Terminate(a.Intent, execution.LaunchNotStarted))
	s.X.Clock.Advance(time.Minute)
	return s.claim(f.Scope, f.Run, true)
}

// Outcome is what one dispatched attempt left behind.
type Outcome struct {
	Attempt *execution.Attempt
	Agg     *execution.Aggregate
	Logs    string
}

// Succeeded reports whether the attempt's accepted result is a completed one.
func (o *Outcome) Succeeded() bool { return o.Attempt.Status == execution.AttemptSucceeded }

// Output returns the accepted result's output of the name ("" when none).
func (o *Outcome) Output(name string) runtimev1.Value {
	if o.Attempt.Result == nil {
		return runtimev1.Value{}
	}
	return o.Attempt.Result.Outputs[name]
}

// Reason is the failure reason code of a failed accepted result.
func (o *Outcome) Reason() string {
	if o.Attempt.Result != nil && o.Attempt.Result.Reason != nil {
		return *o.Attempt.Result.Reason
	}
	return string(o.Attempt.Reason)
}

// Execute dispatches the flow's next runnable attempt through the Podman
// runtime and waits for the attempt to end.
func (s *Stack) Execute(f *Flow) *Outcome {
	s.T.Helper()
	return s.dispatch(f)
}

// attachSource points the project's workspace at the served repository and the
// commit, and makes the commit part of the workspace's accepted history.
func (s *Stack) attachSource(p executiontest.Project, commit string) {
	s.T.Helper()
	url := s.pod.srv.URL + "/repo.git"
	s.X.OwnerExec(`UPDATE workspaces SET repository_url = $4, initial_commit = $5, accepted_commit = $5 WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(p.Scope.TenantID), string(p.Scope.ProjectID), string(p.Workspace), url, commit)
	s.X.Sources.History[commit] = true
	s.X.Sources.Tip = commit
	s.commits[p.Item] = commit
}

func (s *Stack) dispatch(f *Flow) *Outcome {
	s.T.Helper()
	return s.Await(f, s.Dispatch(f))
}

// Dispatch starts the flow's next runnable attempt through the Podman runtime
// without waiting for it, and returns its ordinal. Nothing observes the
// workload until a reconciliation pass runs.
func (s *Stack) Dispatch(f *Flow) int {
	s.T.Helper()
	before := len(s.attempts(f))
	more, err := s.Exec.DispatchNext(context.Background(), f.Scope.TenantID)
	if err != nil || !more {
		s.T.Fatalf("dispatch: more=%v err=%v", more, err)
	}
	return before + 1
}

func (s *Stack) attempts(f *Flow) []*execution.Attempt {
	agg, err := s.ExecStore.Get(context.Background(), f.Scope, f.Run.ID)
	must(s.T, err)
	return agg.Attempts
}

// Await drives reconciliation until the n-th attempt of the flow's run has
// ended (and, for a succeeded one, the run has settled).
func (s *Stack) Await(f *Flow, n int) *Outcome {
	s.T.Helper()
	return s.AwaitRun(f.Scope, f.Run.ID, n)
}

// AwaitRun is Await for any run of the scope.
func (s *Stack) AwaitRun(scope domain.Scope, run domain.RunID, n int) *Outcome {
	s.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	for {
		if _, err := s.Exec.ReconcileTenant(ctx, scope.TenantID); err != nil && ctx.Err() == nil {
			s.T.Logf("reconcile: %v", err)
		}
		agg, err := s.ExecStore.Get(ctx, scope, run)
		must(s.T, err)
		if len(agg.Attempts) >= n {
			at := agg.Attempts[n-1]
			if at.Status != execution.AttemptRunning && (at.Status != execution.AttemptSucceeded || agg.Run.Status.Terminal()) {
				return &Outcome{Attempt: at, Agg: agg, Logs: s.Logs(at)}
			}
		}
		select {
		case <-ctx.Done():
			s.T.Fatalf("waiting for attempt %d of %s: %d attempts, run %s", n, run, len(agg.Attempts), agg.Run.Status)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// AwaitTerminal drives reconciliation until the run has ended.
func (s *Stack) AwaitTerminal(scope domain.Scope, run domain.RunID) execution.RunStatus {
	s.T.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		_, _ = s.Exec.ReconcileTenant(context.Background(), scope.TenantID)
		agg, err := s.ExecStore.Get(context.Background(), scope, run)
		must(s.T, err)
		if agg.Run.Status.Terminal() {
			return agg.Run.Status
		}
		if time.Now().After(deadline) {
			s.T.Fatalf("run %s is still %s", run, agg.Run.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ExecuteCleanup dispatches the cleanup run's step through the Podman runtime
// and waits for it.
func (s *Stack) ExecuteCleanup(scope domain.Scope, run domain.RunID) *Outcome {
	s.T.Helper()
	more, err := s.Exec.DispatchNext(context.Background(), scope.TenantID)
	if err != nil || !more {
		s.T.Fatalf("dispatch of the cleanup run: more=%v err=%v", more, err)
	}
	agg, err := s.ExecStore.Get(context.Background(), scope, run)
	must(s.T, err)
	if len(agg.Attempts) == 0 {
		var other string
		s.X.Scalar(&other, `SELECT string_agg(run_id || ':' || status, ',') FROM step_attempts WHERE tenant_id = $1 AND created_at > now() - interval '5 seconds'`, string(scope.TenantID))
		s.T.Fatalf("the dispatch did not start the cleanup run %s (status %s): another run was claimed: %s", run, agg.Run.Status, other)
	}
	return s.AwaitRun(scope, run, len(agg.Attempts))
}

// Reconcile runs one repair pass over the flow's tenant.
func (s *Stack) Reconcile(f *Flow) {
	s.T.Helper()
	if _, err := s.Exec.ReconcileTenant(context.Background(), f.Scope.TenantID); err != nil {
		s.T.Logf("reconcile: %v", err)
	}
}

// RunStatus is the persisted status of the flow's deploy run.
func (s *Stack) RunStatus(f *Flow) execution.RunStatus {
	s.T.Helper()
	agg, err := s.ExecStore.Get(context.Background(), f.Scope, f.Run.ID)
	must(s.T, err)
	return agg.Run.Status
}

// CommitBuild commits the build attempt's completed result naming the image.
func (s *Stack) CommitBuild(a *deliverytest.Attempt, image domain.ArtifactID) (execution.ResultAck, error) {
	zero := int64(0)
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, DiagnosticArtifactIDs: []domain.ArtifactID{},
		Outputs: map[string]runtimev1.Value{"image": runtimev1.StringValue(string(image))}}
	h, err := runtimev1.ResultHash(res)
	must(s.T, err)
	return s.Exec.CommitResult(context.Background(), execution.ResultCommit{
		Scope: a.Scope, AttemptID: a.ID(), Request: runtimev1.ResultRequest{ResultHash: h, Result: res},
		Artifacts: map[domain.ArtifactID]runtimev1.ArtifactRef{image: {ID: image, Scope: a.Scope, Attempt: a.ID(), Kind: runtimev1.KindOCI, Usable: true}},
	})
}

// ---- secrets and environments -------------------------------------------------

// Kubeconfig creates a project-owned kubeconfig secret holding value.
func (s *Stack) Kubeconfig(scope domain.Scope, name, value string) secrets.Secret {
	s.T.Helper()
	return s.D.Kubeconfig(scope, name, value)
}

// EnvSpec describes an environment to create.
type EnvSpec struct {
	Name       string
	Kind       delivery.EnvironmentKind
	Namespace  string
	Kubeconfig string // secret name
	Extra      []delivery.Credential
	Config     map[string]string
	PreviewTTL *time.Duration
}

// Environment creates the environment through the real delivery service.
func (s *Stack) Environment(scope domain.Scope, e EnvSpec) delivery.Environment {
	s.T.Helper()
	if e.Kind == "" {
		e.Kind = delivery.KindPreview
	}
	if e.Config == nil {
		e.Config = map[string]string{"REPLICAS": "1"}
	}
	env, err := s.Svc.CreateEnvironment(context.Background(), Caller(), scope, delivery.EnvironmentInput{
		Name: e.Name, Kind: e.Kind, Target: "kind", Namespace: e.Namespace,
		Kubeconfig: delivery.Credential{Name: delivery.DefaultKubeconfig, Ref: secrets.FormatSecretRef(e.Kubeconfig), Delivery: runtimev1.DeliveryFile},
		Secrets:    e.Extra, Config: e.Config, PreviewTTL: e.PreviewTTL,
	})
	must(s.T, err)
	return env
}

// cleanupStep renders the cleanup step: one ordinary container step declaring the
// deployment's environment and running delete.sh from the tools image with no
// workspace.
func (s *Stack) cleanupStep(env delivery.Environment, img Image, extra map[string]string) string {
	cenv := map[string]string{
		"KODAMA_SERVER_URL":     s.pod.srv.URL,
		"DEPLOY_DELETE_TIMEOUT": "60",
		"RT_RETRY_DELAY":        "0",
		"RT_ATTEMPTS":           "3",
	}
	for k, v := range extra {
		cenv[k] = v
	}
	return "{id: cleanup, kind: container, workspace: {access: none}, environment: " + string(env.ID()) + ", image: " + img.Repository + "@" + string(img.Digest) +
		", command: [" + DeleteScript + "], env: " + yamlEnv(cenv) + ", timeoutSeconds: 300, outputs: {deployment_id: {type: string}, cleanup_state: {type: string}, deleted_count: {type: integer}}}"
}

// CleanupDefinition registers the project's cleanup pipeline definition and makes
// it the scope's: ONE definition for every deployment of the project. It declares
// the string input deployment_id, which execution fills with the deployment the
// cleanup run was admitted for (T-092), and names no deployment anywhere else:
// delete.sh reads it as KODAMA_INPUT_DEPLOYMENT_ID. Registering it again is
// idempotent (the same definition).
func (s *Stack) CleanupDefinition(scope domain.Scope, env delivery.Environment, img Image) domain.DefinitionID {
	s.T.Helper()
	def := s.X.Define(scope, pipelinetest.DocNamed("cleanup", "{deployment_id: {type: string, required: true}}", s.cleanupStep(env, img, nil)))
	s.Defs.Set(scope, def.ID)
	return def.ID
}

// RetargetedCleanupDefinition registers a definition whose step overrides the
// target with DEPLOY_DEPLOYMENT_ID (which delete.sh prefers to the input), for the
// negative tests that show a run bound to one deployment cannot read or clean
// another that its own step names. It is not the scope's definition.
func (s *Stack) RetargetedCleanupDefinition(scope domain.Scope, env delivery.Environment, img Image, target delivery.DeploymentID) domain.DefinitionID {
	s.T.Helper()
	def := s.X.Define(scope, pipelinetest.DocNamed("cleanup-retargeted", "{deployment_id: {type: string, required: true}}",
		s.cleanupStep(env, img, map[string]string{"DEPLOY_DEPLOYMENT_ID": string(target)})))
	return def.ID
}

// ExecuteAny dispatches the next runnable step of the tenant, which must belong to
// one of runs, through the Podman runtime and waits for it. It returns the
// outcome and the run it belonged to.
func (s *Stack) ExecuteAny(scope domain.Scope, runs []domain.RunID) (*Outcome, domain.RunID) {
	s.T.Helper()
	count := func(r domain.RunID) int {
		agg, err := s.ExecStore.Get(context.Background(), scope, r)
		must(s.T, err)
		return len(agg.Attempts)
	}
	before := map[domain.RunID]int{}
	for _, r := range runs {
		before[r] = count(r)
	}
	more, err := s.Exec.DispatchNext(context.Background(), scope.TenantID)
	if err != nil || !more {
		s.T.Fatalf("dispatch: more=%v err=%v", more, err)
	}
	for _, r := range runs {
		if n := count(r); n > before[r] {
			return s.AwaitRun(scope, r, n), r
		}
	}
	s.T.Fatalf("the dispatch started none of the expected runs %v", runs)
	return nil, ""
}

// Describe summarizes a deployment row for failure messages.
func (s *Stack) Describe(scope domain.Scope, id delivery.DeploymentID) string {
	d, err := s.DeliveryStore.GetDeployment(context.Background(), scope, id)
	if err != nil {
		return fmt.Sprintf("deployment %s: %v", id, err)
	}
	r := d.Record()
	return fmt.Sprintf("deployment %s status=%s reason=%s cleanup=%s/%s objects=%d", id, r.Status, r.Reason, r.CleanupState, r.CleanupReason, len(r.Objects))
}
