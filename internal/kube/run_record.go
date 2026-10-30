package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Every run resource has one record Secret in the tenant namespace, created
// before any other object of the resource ("intent first"). It holds the
// resource's canonical spec (never a secret value), the external-effect record
// below and, for a workload with a runtime identity, the admission credential
// that is persisted before it is registered. The record outlives the workload
// until Remove: it is what fences a stopped resource against a second Ensure,
// what makes a lost create resolvable and what proves ownership of a Pod that
// outlives its Job. It is bookkeeping, not authority: execution's committed
// handle (scope, name, bound UID and fingerprint) decides, and the record is
// compared against it.

// Secret keys of the record Secret. The spec is one key; the credential keys are
// the ones the workload mounts (see credentialItems).
const (
	KeyResourceSpec  = "spec"
	secretRecSuffix  = "-rec"
	secretTombSuffix = "-tomb"
)

// recName is the record Secret of a resource object name.
func recName(name string) string { return name + secretRecSuffix }

// tombName is the tombstone Secret of a removed resource. Remove creates it
// before it deletes the record and nothing in this adapter ever deletes it: a
// removed resource is never created again by a repeated Ensure, whatever
// controller instance asks (execution recorded the cleanup and released the
// resource's capacity). It holds no credential and no spec: only the identity,
// the fingerprint and the bound UID the resource had. It is one small object per
// removed resource, deleted with the tenant namespace at offboarding.
func tombName(name string) string { return name + secretTombSuffix }

type recPhase string

const (
	// recPlanned: the record exists; nothing else was created and no
	// registration was made.
	recPlanned recPhase = "planned"
	// recRegistered: the credential (if any) is registered; no create was issued.
	recRegistered recPhase = "registered"
	// recIssued: a create was sent and its outcome may be unknown.
	recIssued recPhase = "issued"
	// recOwned: the resource's object was created or adopted and its UID bound.
	recOwned recPhase = "owned"
)

// resRecord is the external-effect record of one resource. Transitions are
// checked: a create may be issued only from registered, a bound UID never
// changes and a fence is never lifted.
type resRecord struct {
	Version int      `json:"v"`
	Name    string   `json:"name"`
	Phase   recPhase `json:"phase"`
	// Fenced is set once Stop began: the resource is never created or started
	// again.
	Fenced   bool      `json:"fenced,omitempty"`
	UID      string    `json:"uid,omitempty"`
	FencedAt time.Time `json:"fencedAt,omitempty"`
	// Issued is set once any create of the resource's objects was sent.
	Issued bool `json:"issued,omitempty"`
	// Pending are the creates that may still land, one entry per operation (its
	// unique id and when it was announced). An operation removes only its own entry,
	// once it read its object back; an entry no one removed (a lost response, a crashed
	// controller) is inert only after the create quiescence. No other Ensure can
	// settle an operation's uncertainty.
	Pending map[string]time.Time `json:"pending,omitempty"`
	// Seen are instance UIDs of primary objects that verified, by their controlled
	// spec, as the resource's although no UID was committed (a late create): kept
	// durably before the object is deleted, so cleanup that has to be repeated still
	// knows what it deleted and what volumes belong to it.
	Seen []string `json:"seen,omitempty"`
	// Launch is the launch evidence collected before the workload's Pods were
	// deleted; it only ever becomes more informative.
	Launch string `json:"launch,omitempty"`
	// Volume is a workspace's bound volume name, recorded before the claim is
	// deleted so the allocation can still be confirmed gone.
	Volume string `json:"volume,omitempty"`
	// Removed marks a tombstone: the resource was removed and is never created again.
	Removed bool `json:"removed,omitempty"`
	// Untracked marks a fence created by Stop for a resource whose record was never
	// seen (never planned) or was lost: nothing is known of earlier effects, so it
	// never yields not_started evidence.
	Untracked bool `json:"untracked,omitempty"`
	// Companions are the verified instance UIDs of the objects that accompany the
	// primary one (a service's Service and ingress policy, every network policy),
	// by object name. Cleanup deletes only what is recorded here or what verifies
	// against the committed spec.
	Companions map[string]string `json:"companions,omitempty"`
}

// noteCompanion records a verified companion's UID and reports a change.
func (r *resRecord) noteCompanion(name, uid string) bool {
	if uid == "" || r.Companions[name] == uid {
		return false
	}
	if r.Companions == nil {
		r.Companions = map[string]string{}
	}
	r.Companions[name] = uid
	return true
}

func (r *resRecord) marshal() string {
	b, _ := json.Marshal(r)
	return string(b)
}

func parseRecord(name, s string) (*resRecord, error) {
	var r resRecord
	if err := json.Unmarshal([]byte(s), &r); err != nil || r.Version != 1 || r.Name != name {
		return nil, wrap(ErrForeignWorkload, "the resource record does not belong to this resource")
	}
	switch r.Phase {
	case recPlanned, recRegistered, recIssued, recOwned:
	default:
		return nil, wrap(ErrForeignWorkload, "unknown resource phase")
	}
	if r.Phase == recOwned && r.UID == "" {
		return nil, wrap(ErrForeignWorkload, "inconsistent resource record")
	}
	switch execution.LaunchEvidence(r.Launch) {
	case "", execution.LaunchUnknown, execution.LaunchStarted, execution.LaunchNotStarted:
	default:
		return nil, wrap(ErrForeignWorkload, "unknown launch evidence")
	}
	return &r, nil
}

func (r *resRecord) fail(to recPhase) error {
	return wrap(ErrInvalidTransition, "%s -> %s", r.Phase, to)
}

func (r *resRecord) registered() error {
	switch r.Phase {
	case recPlanned:
		r.Phase = recRegistered
		return nil
	case recRegistered:
		return nil
	}
	return r.fail(recRegistered)
}

// issue announces a create of the primary object (operation id) before it is
// sent. It is refused for a fenced resource.
func (r *resRecord) issue(now time.Time, op string) error {
	if r.Fenced || (r.Phase != recRegistered && r.Phase != recIssued) {
		return r.fail(recIssued)
	}
	r.Phase = recIssued
	r.beginOp(op, now)
	return nil
}

// beginOp announces a create (of any object of the resource) that may land.
func (r *resRecord) beginOp(op string, now time.Time) {
	if r.Pending == nil {
		r.Pending = map[string]time.Time{}
	}
	r.Pending[op] = now.UTC()
	r.Issued = true
}

// endOp removes the operation's own entry: its create was read back (or definitely
// refused). It never touches another operation's.
func (r *resRecord) endOp(op string) { delete(r.Pending, op) }

// refused records that the operation's create definitely was not applied.
func (r *resRecord) refused(op string) error {
	if r.Phase != recIssued || r.UID != "" {
		return r.fail(recRegistered)
	}
	r.endOp(op)
	if len(r.Pending) == 0 {
		r.Phase, r.Issued = recRegistered, false
	}
	return nil
}

// adopt binds the verified object's UID; it never changes afterwards.
func (r *resRecord) adopt(uid string) error {
	if uid == "" || (r.UID != "" && r.UID != uid) {
		return wrap(ErrForeignWorkload, "the bound resource UID cannot change")
	}
	switch r.Phase {
	case recRegistered, recIssued, recOwned:
		r.Phase, r.UID = recOwned, uid
		return nil
	}
	return r.fail(recOwned)
}

// fence records that Stop began. It is idempotent and never lifted.
func (r *resRecord) fence(now time.Time) bool {
	if r.Fenced {
		return false
	}
	r.Fenced, r.FencedAt = true, now.UTC()
	return true
}

// neverIssued reports that no create was ever sent: nothing of the resource can
// exist or arrive.
func (r *resRecord) neverIssued() bool {
	return (r.Phase == recPlanned || r.Phase == recRegistered) && !r.Issued && len(r.Pending) == 0 && !r.Untracked && !r.Removed
}

// quiescent reports that no announced create can still land: every operation that
// announced one did so at least the quiescence ago (the API request timeout is
// shorter, so such a create has landed or never will).
func (r *resRecord) quiescent(now time.Time, quiescence time.Duration) bool {
	for _, at := range r.Pending {
		if now.Sub(at) < quiescence {
			return false
		}
	}
	return true
}

func (r *resRecord) noteLaunch(l execution.LaunchEvidence) bool {
	cur := execution.LaunchEvidence(r.Launch)
	if cur == "" {
		cur = execution.LaunchUnknown
	}
	merged := mergeLaunch(cur, l)
	if merged == cur {
		return false
	}
	r.Launch = string(merged)
	return true
}

func (r *resRecord) launch() execution.LaunchEvidence {
	if r.Launch == "" {
		return execution.LaunchUnknown
	}
	return execution.LaunchEvidence(r.Launch)
}

// resRec is a loaded record Secret.
type resRec struct {
	secret kubernetes.Secret
	rec    *resRecord
}

// spec decodes the stored canonical spec.
func (a *resRec) spec() (execution.ResourceSpec, error) {
	var s execution.ResourceSpec
	if err := json.Unmarshal(a.secret.Data[KeyResourceSpec], &s); err != nil {
		return execution.ResourceSpec{}, wrap(ErrForeignWorkload, "the stored resource spec is unreadable")
	}
	return s, nil
}

var errRecMoved = errors.New("kube: the resource record changed concurrently")

// identityAnnotations are the annotations every object of a resource carries and
// verification compares against the committed handle.
func identityAnnotations(spec execution.ResourceSpec, fp string) map[string]string {
	return map[string]string{
		AnnTenant: string(spec.Scope.TenantID), AnnProject: string(spec.Scope.ProjectID), AnnRun: string(spec.RunID),
		AnnResourceKind: string(spec.Kind), AnnResourceKey: spec.Key, AnnResourceName: spec.Name, AnnFingerprint: fp,
		AnnPolicy: spec.Policy.ID + "@" + spec.Policy.Digest,
	}
}

// verifyRecSecret checks the Secret is the resource's own record: labels,
// identity annotations equal to the expectation, no owner, immutable data and a
// spec whose fingerprint is the annotated one.
func verifyRecSecret(s kubernetes.Secret, ns string, want map[string]string, labels map[string]string, secretName string) error {
	bad := func(why string) error { return wrap(ErrForeignWorkload, "resource record: %s", why) }
	if s.Metadata.Name != secretName || s.Metadata.Namespace != ns {
		return bad("name or namespace differs")
	}
	if len(s.Metadata.OwnerReferences) != 0 {
		return bad("has an owner")
	}
	for k, v := range labels {
		if s.Metadata.Labels[k] != v {
			return bad("label differs")
		}
	}
	for k, v := range want {
		if s.Metadata.Annotations[k] != v {
			return bad("annotation " + k + " differs")
		}
	}
	if s.Immutable == nil || !*s.Immutable {
		return bad("data is not immutable")
	}
	if s.Metadata.UID == "" {
		return bad("no UID")
	}
	return nil
}

// specMatches reports that the stored spec is the canonical encoding of spec.
func specMatches(stored []byte, spec execution.ResourceSpec) bool {
	canonical, err := spec.Canonical()
	return err == nil && bytes.Equal(stored, canonical)
}

// readRec reads and verifies a record Secret against the committed identity. It
// returns (nil, nil) only for a definite absence.
func readRec(ctx context.Context, cl Cluster, ns string, ident map[string]string, labels map[string]string, name string) (*resRec, error) {
	return readSecretRec(ctx, cl, ns, recName(name), ident, labels, name)
}

// readTomb reads the tombstone of a removed resource (nil when none).
func readTomb(ctx context.Context, cl Cluster, ns string, ident map[string]string, labels map[string]string, name string) (*resRec, error) {
	t, err := readSecretRec(ctx, cl, ns, tombName(name), ident, labels, name)
	if err != nil || t == nil {
		return t, err
	}
	if !t.rec.Removed {
		return nil, wrap(ErrForeignWorkload, "resource tombstone: not a removal marker")
	}
	return t, nil
}

func readSecretRec(ctx context.Context, cl Cluster, ns, secretName string, ident map[string]string, labels map[string]string, name string) (*resRec, error) {
	s, err := cl.GetSecret(ctx, ns, secretName)
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, wrapCause(ErrUnavailable, err, "reading the resource record")
	}
	if err := verifyRecSecret(s, ns, ident, labels, secretName); err != nil {
		return nil, err
	}
	rec, err := parseRecord(name, s.Metadata.Annotations[AnnRecord])
	if err != nil {
		return nil, err
	}
	return &resRec{secret: s, rec: rec}, nil
}
