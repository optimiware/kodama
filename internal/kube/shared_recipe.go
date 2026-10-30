package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Object keys of the run_shared attempt Job (seams S1-S3).
const (
	// AnnRunModel marks a Job built for the run_shared model; its value is the
	// run model name.
	AnnRunModel = "kodama.optimi.com/run-model"
	// ValueRunShared is the run_shared model.
	ValueRunShared = "run_shared"
	// AnnSharedDigest binds the launch (role, workspace claim, network model) the
	// Job was built from. It is evidence; adoption compares every field.
	AnnSharedDigest = "kodama.optimi.com/shared-digest"

	// pathHome, pathRun and the volumes of the attempt-private directories.
	pathHome = "/kodama/home"
	pathRun  = "/kodama/run"

	volumeWorkspace = "workspace"
)

// sharedDigest is the digest of a launch's canonical encoding.
func sharedDigest(l execution.SharedLaunch) string {
	raw, _ := json.Marshal(l)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// checkShared verifies that a launch describes this attempt's run: a valid pin,
// a role whose digest matches the pinned one, a bound workspace claim of the
// attempt's run and a shape this adapter can render exactly.
func (r Recipe) checkShared(l execution.SharedLaunch) error {
	bad := func(why string) error { return wrap(ErrPolicy, "the shared launch is unusable: %s", why) }
	if err := l.Policy.Validate(); err != nil {
		return bad("no policy pin")
	}
	if d, err := l.Role.Digest(); err != nil || d != l.RoleDigest {
		return bad("the role does not match its pinned digest")
	}
	if err := l.Workspace.Validate(); err != nil {
		return bad("the workspace handle is malformed")
	}
	if l.Workspace.Kind != execution.KindWorkspace || l.Workspace.UID == "" || l.Workspace.Scope != r.Scope || l.Workspace.RunID != r.RunID {
		return bad("no bound workspace of the attempt's run")
	}
	switch l.Network.Isolation {
	case execution.IsolationTenant, execution.IsolationRun:
	default:
		return bad("no network model")
	}
	return nil
}

// WithShared selects the run_shared shape for the launch. The fingerprint is
// unchanged: it is computed over the attempt_private recipe the planner binds.
func (r Recipe) WithShared(l execution.SharedLaunch) (Recipe, error) {
	if err := r.checkShared(l); err != nil {
		return Recipe{}, err
	}
	sec, err := roleSecurityOf(l.Role, r.policy.cfg)
	if err != nil {
		return Recipe{}, err
	}
	c := l
	c.Role.CapabilitiesAdd = append([]string(nil), l.Role.CapabilitiesAdd...)
	if l.Role.CapabilitiesAdd == nil {
		c.Role.CapabilitiesAdd = nil
	}
	c.Role.Devices = append([]string(nil), l.Role.Devices...)
	if l.Role.Devices == nil {
		c.Role.Devices = nil
	}
	r.shared, r.sharedSec = &c, sec
	return r, nil
}

// Shared returns the launch the recipe was built for, if any.
func (r Recipe) Shared() *execution.SharedLaunch { return r.shared }

// sharedPodLabels are the labels run policies select a run_shared step Pod by.
func (r Recipe) sharedPodLabels() map[string]string {
	return map[string]string{
		LabelRunKey: RunKey(r.Scope, r.RunID), LabelRole: RoleLabelStep, LabelWorkload: WorkloadHash(r.Scope, r.RunID, r.StepID),
		LabelRunShared: "true",
	}
}

// sharedVolumes returns the run_shared volume list: the legacy list with the
// workspace replaced by the bound claim and two attempt-private directories
// (HOME for builder storage and the XDG runtime directory) added.
func (r Recipe) sharedVolumes(legacy []kubernetes.Volume) []kubernetes.Volume {
	c := r.policy.cfg
	out := make([]kubernetes.Volume, 0, len(legacy)+2)
	for _, v := range legacy {
		if v.Name == volumeWorkspace {
			v = kubernetes.Volume{Name: volumeWorkspace, PersistentVolumeClaim: &kubernetes.PVCVolume{ClaimName: r.shared.Workspace.Name}}
		}
		out = append(out, v)
	}
	return append(out,
		// HOME takes over the budget the attempt-private workspace volume had.
		kubernetes.Volume{Name: "home", EmptyDir: &kubernetes.EmptyDir{SizeLimit: c.Volumes.Workspace}},
		kubernetes.Volume{Name: "run", EmptyDir: &kubernetes.EmptyDir{Medium: "Memory", SizeLimit: c.Volumes.Identity}},
	)
}
