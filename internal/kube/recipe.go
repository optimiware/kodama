package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Object keys the executor writes. Labels select managed objects; they are
// never authority. Long identifiers live in annotations (labels are limited to
// 63 characters), and every annotation is rechecked against committed attempt
// data before an object is adopted, modified or deleted.
const (
	LabelManagedBy = "kodama.optimi.com/managed-by"
	ValueManagedBy = "job-executor"
	LabelJob       = "kodama.optimi.com/job"

	AnnTenant        = "kodama.optimi.com/tenant"
	AnnProject       = "kodama.optimi.com/project"
	AnnRun           = "kodama.optimi.com/run"
	AnnStep          = "kodama.optimi.com/step"
	AnnAttempt       = "kodama.optimi.com/attempt"
	AnnContextDigest = "kodama.optimi.com/context-digest"
	AnnFingerprint   = "kodama.optimi.com/spec-fingerprint"
	AnnWorkload      = "kodama.optimi.com/workload"

	// PodLabelJobUID and PodLabelJobName are set on Pods by the Job controller.
	PodLabelJobUID  = "batch.kubernetes.io/controller-uid"
	PodLabelJobName = "batch.kubernetes.io/job-name"

	recipeVersion = "kodama/job-spec/v2"

	// PrimaryContainer and InjectContainer name the two containers.
	PrimaryContainer = "workload"
	InjectContainer  = "inject"
)

// Recipe is the complete, executor-controlled description of one attempt's Job.
// It is a pure function of the committed pinned context and the execution
// policy: no generated UID, timestamp, credential or API-server state.
type Recipe struct {
	Scope         domain.Scope
	RunID         domain.RunID
	StepID        string
	AttemptID     domain.AttemptID
	JobName       string
	ContextDigest string
	Target        TenantTarget
	Image         string
	Access        execution.Access
	Actions       []string

	policy ExecutionPolicy
}

// SecretName is the attempt-scoped admission Secret, derived from the Job name.
func SecretName(jobName string) string { return jobName + "-adm" }

// NewRecipe derives the recipe of an attempt. It fails with ErrPolicy when the
// policy does not cover the tenant.
func (p ExecutionPolicy) NewRecipe(c runtimev1.Context, contextDigest string) (Recipe, error) {
	tgt, err := p.Tenant(c.TenantID)
	if err != nil {
		return Recipe{}, err
	}
	access := execution.Access(c.Workspace.Access)
	actions, err := p.Actions(access)
	if err != nil {
		return Recipe{}, err
	}
	if err := c.Scope().Validate(); err != nil {
		return Recipe{}, wrap(ErrPolicy, "the context scope is invalid")
	}
	return Recipe{
		Scope: c.Scope(), RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID,
		JobName: execution.WorkloadName(c.TenantID, c.AttemptID), ContextDigest: contextDigest,
		Target: tgt, Image: c.Command.Image, Access: access, Actions: actions.Strings(), policy: p,
	}, nil
}

// Fingerprint binds the recipe: the complete Job template (namespace, service
// account, exact images, security settings, resource bounds, volumes, bootstrap
// arguments, ownership metadata), the action ceiling and the runtime CA. It
// excludes anything the cluster or the executor generates.
func (r Recipe) Fingerprint() (string, error) {
	doc := struct {
		Version string         `json:"version"`
		Job     kubernetes.Job `json:"job"`
		Secret  string         `json:"secret"`
		Actions []string       `json:"actions"`
		CA      string         `json:"ca"`
	}{recipeVersion, r.Job(), SecretName(r.JobName), r.Actions, r.policy.caHash}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Planner implements execution.Planner with the full recipe in the fingerprint.
type Planner struct{ policy ExecutionPolicy }

var _ execution.Planner = Planner{}

// NewPlanner requires a valid policy; a zero policy denies every plan.
func NewPlanner(p ExecutionPolicy) Planner { return Planner{policy: p} }

// Plan implements execution.Planner. It is pure: the canonical context and
// digest come from execution's default planner so the pinned context stays
// byte-identical to what runtime admission pins; only the fingerprint differs.
func (pl Planner) Plan(tenant domain.TenantID, c runtimev1.Context) (execution.WorkloadPlan, error) {
	if c.TenantID != tenant {
		return execution.WorkloadPlan{}, wrap(ErrPolicy, "the context belongs to another tenant")
	}
	base, err := execution.DefaultPlanner{}.Plan(tenant, c)
	if err != nil {
		return execution.WorkloadPlan{}, err
	}
	r, err := pl.policy.NewRecipe(c, base.ContextDigest)
	if err != nil {
		return execution.WorkloadPlan{}, err
	}
	fp, err := r.Fingerprint()
	if err != nil {
		return execution.WorkloadPlan{}, err
	}
	base.SpecFingerprint = fp
	return base, nil
}
