package kube

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

var indexRe = regexp.MustCompile(`\[\d+\]`)

// Expectation is everything the executor expects of one attempt's Job, derived
// from committed attempt data and the policy alone.
type Expectation struct {
	Identity Identity
	// WorkspaceID is the committed workspace binding of the attempt.
	WorkspaceID string
	Recipe      Recipe
	// Job is the object the executor creates (fingerprint annotation included).
	Job kubernetes.Job
	// Context is the committed pinned context.
	Context runtimev1.Context
	// Shared is the committed run_shared launch (nil for attempt_private).
	Shared *execution.SharedLaunch
}

// Expect rebuilds the expected Job of a committed attempt. It verifies the
// persisted context against its digest and the rebuilt recipe against the
// persisted fingerprint, so a policy change (or a corrupted row) surfaces as
// ErrPolicyDrift and never as a silently different Job.
func (p ExecutionPolicy) Expect(rec AttemptRecord) (Expectation, error) {
	if !p.valid {
		return Expectation{}, wrap(ErrPolicy, "no execution policy is configured")
	}
	sum := sha256.Sum256(rec.Context)
	if rec.ContextDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		return Expectation{}, wrap(ErrNotDispatchable, "the stored context does not match its digest")
	}
	c, err := runtimev1.DecodeRequest[runtimev1.Context](rec.Context)
	if err != nil {
		return Expectation{}, wrap(ErrNotDispatchable, "the stored context is unreadable")
	}
	if c.Scope() != rec.Scope || c.AttemptID != rec.AttemptID || c.RunID != rec.RunID || c.StepID != rec.StepID {
		return Expectation{}, wrap(ErrNotDispatchable, "the stored context belongs to another attempt")
	}
	r, err := p.NewRecipe(c, rec.ContextDigest)
	if err != nil {
		return Expectation{}, err
	}
	if r.Access != rec.Access {
		return Expectation{}, wrap(ErrNotDispatchable, "the stored context declares another workspace access")
	}
	if r.JobName != rec.JobName {
		return Expectation{}, wrap(ErrNotDispatchable, "the stored Job name is not the attempt's deterministic name")
	}
	fp, err := r.Fingerprint()
	if err != nil {
		return Expectation{}, err
	}
	if fp != rec.SpecFingerprint {
		return Expectation{}, ErrPolicyDrift
	}
	// The fingerprint binds the attempt_private recipe the planner computed; the
	// run_shared shape is selected afterwards from the committed launch.
	if rec.Shared != nil {
		if r, err = r.WithShared(*rec.Shared); err != nil {
			return Expectation{}, err
		}
	}
	return Expectation{
		Identity: Identity{
			Scope: rec.Scope, RunID: rec.RunID, StepID: rec.StepID, AttemptID: rec.AttemptID, JobName: r.JobName,
			Namespace: r.Target.Namespace, ContextDigest: rec.ContextDigest, Fingerprint: fp, Access: rec.Access, Shared: rec.Shared != nil,
		},
		Shared:      r.Shared(),
		WorkspaceID: rec.WorkspaceID,
		Recipe:      r, Job: r.JobObject(fp), Context: c,
	}, nil
}

// VerifyJob decides whether an existing Job is the attempt's workload. Every
// check compares the object with the committed expectation: an annotation
// claiming the right fingerprint is insufficient, because the controlled spec
// fields are compared one by one and unexpected fields (containers, privileged
// settings, host volumes, owner references, TTLs) are mismatches. The returned
// error is ErrForeignWorkload and its message lists only field paths, never
// values.
func VerifyJob(exp Expectation, job kubernetes.Job) (Verification, error) {
	id := exp.Identity
	bad := func(why string) (Verification, error) {
		return Verification{}, wrap(ErrForeignWorkload, "%s", why)
	}
	if job.Metadata.Name != id.JobName || job.Metadata.Namespace != id.Namespace {
		return bad("name or namespace differs")
	}
	if job.Metadata.UID == "" {
		return bad("no UID")
	}
	if len(job.Metadata.OwnerReferences) != 0 {
		return bad("the Job has an owner")
	}
	for k, want := range exp.Job.Metadata.Annotations {
		if job.Metadata.Annotations[k] != want {
			return bad("annotation " + k + " differs")
		}
	}
	for k, want := range exp.Job.Metadata.Labels {
		if job.Metadata.Labels[k] != want {
			return bad("label " + k + " differs")
		}
	}
	for _, f := range job.Metadata.Finalizers {
		if f != "foregroundDeletion" && f != "orphan" {
			return bad("unexpected finalizer")
		}
	}
	if len(job.Raw) == 0 {
		return bad("no object document")
	}
	var actual map[string]any
	dec := json.NewDecoder(bytes.NewReader(job.Raw))
	dec.UseNumber()
	if err := dec.Decode(&actual); err != nil {
		return bad("undecodable object")
	}
	wantRaw, err := json.Marshal(exp.Job)
	if err != nil {
		return Verification{}, err
	}
	var want map[string]any
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		return Verification{}, err
	}
	spec, _ := actual["spec"].(map[string]any)
	if spec == nil {
		return bad("no spec")
	}
	diffs := specDiff(want["spec"], spec, "spec", job.Metadata.UID, id.JobName)
	if len(diffs) > 0 {
		sort.Strings(diffs)
		if len(diffs) > 5 {
			diffs = diffs[:5]
		}
		return bad("spec differs at " + strings.Join(diffs, ", "))
	}
	return Verification{attempt: id.AttemptID, jobName: id.JobName, uid: job.Metadata.UID, ok: true}, nil
}

// specDiff compares the expected controlled document with the actual one. Every
// expected field must be present and equal; every additional actual field must
// be a known, benign server default (see benignDefault). It returns the paths
// of differences.
func specDiff(want, got any, path, jobUID, jobName string) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{path}
		}
		var out []string
		for k, wv := range w {
			gv, present := g[k]
			if !present {
				out = append(out, path+"."+k+"(missing)")
				continue
			}
			out = append(out, specDiff(wv, gv, path+"."+k, jobUID, jobName)...)
		}
		for k, gv := range g {
			if _, exp := w[k]; exp {
				continue
			}
			if !benignDefault(path+"."+k, gv, jobUID, jobName) {
				out = append(out, path+"."+k+"(unexpected)")
			}
		}
		return out
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return []string{path + "(length)"}
		}
		var out []string
		for i := range w {
			out = append(out, specDiff(w[i], g[i], fmt.Sprintf("%s[%d]", path, i), jobUID, jobName)...)
		}
		return out
	case string:
		g, ok := got.(string)
		if !ok {
			return []string{path}
		}
		if g == w || (isQuantityPath(path) && equalQuantity(g, w)) {
			return nil
		}
		return []string{path}
	case json.Number, float64, int, int32, int64:
		if fmt.Sprint(w) == fmt.Sprint(got) {
			return nil
		}
		gn, ok := got.(json.Number)
		if ok {
			if fmt.Sprint(w) == gn.String() {
				return nil
			}
		}
		return []string{path}
	case bool:
		if g, ok := got.(bool); ok && g == w {
			return nil
		}
		return []string{path}
	case nil:
		if got == nil {
			return nil
		}
		return []string{path}
	}
	return []string{path + "(type)"}
}

func isQuantityPath(p string) bool {
	return strings.Contains(p, ".resources.requests.") || strings.Contains(p, ".resources.limits.") || strings.HasSuffix(p, ".sizeLimit")
}

// benignDefault allows exactly the fields the API server (or the Job
// controller) adds to an unchanged Job. Anything else is a difference, so a new
// field is denied rather than trusted.
func benignDefault(path string, v any, jobUID, jobName string) bool {
	norm := indexRe.ReplaceAllString(path, "[]")
	str, _ := v.(string)
	switch norm {
	case "spec.completionMode":
		return str == "NonIndexed"
	case "spec.manualSelector":
		b, ok := v.(bool)
		return ok && !b
	case "spec.suspend":
		b, ok := v.(bool)
		return ok && !b
	case "spec.podReplacementPolicy":
		return str == "TerminatingOrFailed"
	case "spec.selector":
		m, _ := v.(map[string]any)
		ml, _ := m["matchLabels"].(map[string]any)
		if len(m) != 1 || len(ml) == 0 {
			return false
		}
		for k, val := range ml {
			if k != PodLabelJobUID && k != "controller-uid" {
				return false
			}
			if s, _ := val.(string); s != jobUID {
				return false
			}
		}
		return true
	case "spec.template.metadata.creationTimestamp":
		return v == nil
	case "spec.template.spec.dnsPolicy":
		return str == "ClusterFirst"
	case "spec.template.spec.schedulerName":
		return str == "default-scheduler"
	case "spec.template.spec.serviceAccount":
		return true // the deprecated alias of serviceAccountName, compared there
	case "spec.template.spec.containers[].terminationMessagePath", "spec.template.spec.initContainers[].terminationMessagePath":
		return str == "/dev/termination-log"
	case "spec.template.spec.containers[].terminationMessagePolicy", "spec.template.spec.initContainers[].terminationMessagePolicy":
		return str == "File"
	}
	if strings.HasPrefix(norm, "spec.template.metadata.labels.") {
		switch strings.TrimPrefix(norm, "spec.template.metadata.labels.") {
		case PodLabelJobUID, "controller-uid":
			return str == jobUID
		case PodLabelJobName, "job-name":
			return str == jobName
		}
	}
	return false
}
