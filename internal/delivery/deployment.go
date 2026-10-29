package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Status is the deployment lifecycle. Cleanup is a separate dimension recorded
// beside it and never rewrites an outcome.
//
//	pending --apply started--> running --ready--> succeeded
//	   |                          |  \--failed--> failed
//	   \--failed (never applied)--+----------------^
//
// running with reason outcome_unknown blocks a new apply until the objects are
// observed and the deployment is explicitly settled (reconciled).
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

// Terminal reports a settled outcome.
func (s Status) Terminal() bool { return s == StatusSucceeded || s == StatusFailed }

// Reasons the module itself assigns. A failure's reason is the reporter's code.
const (
	ReasonRegistered     = "registered"
	ReasonApplying       = "applying"
	ReasonOutcomeUnknown = "outcome_unknown"
	ReasonReady          = "ready"
)

var reasonCode = regexp.MustCompile(runtimev1.Patterns["ReasonCode"])

// CleanupState records the narrow cleanup lifecycle of a deployment.
type CleanupState string

const (
	CleanupNone      CleanupState = ""
	CleanupRequested CleanupState = "requested"
	CleanupFailed    CleanupState = "failed"
	CleanupUnknown   CleanupState = "unknown"
	CleanupDone      CleanupState = "done"
)

// MaxObjects bounds the objects a deployment owns.
const MaxObjects = 64

var (
	objectKind       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,62}$`)
	objectAPIVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9./-]{0,99}$`)
	objectName       = regexp.MustCompile(runtimev1.Patterns["KubernetesName"])
	objectUID        = regexp.MustCompile(runtimev1.Patterns["UID"])
)

// ObjectRef identifies one external object a deployment owns. Before apply it
// is the intended identity; afterwards it also carries the observed UID and
// readiness. It is a reference only: no manifest or content is retained.
type ObjectRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
	Ready      bool   `json:"ready,omitempty"`
}

func (o ObjectRef) key() string {
	return o.APIVersion + "|" + o.Kind + "|" + o.Namespace + "|" + o.Name
}

// Validate checks the identity and requires the deployment's namespace.
func (o ObjectRef) Validate(namespace string) error {
	switch {
	case !objectAPIVersion.MatchString(o.APIVersion), !objectKind.MatchString(o.Kind), !objectName.MatchString(o.Name):
		return invalidf("an object needs a valid apiVersion, kind and name")
	case o.Namespace != namespace:
		return Fail(ErrDenied, nil, "an object belongs to another namespace")
	case o.UID != "" && !objectUID.MatchString(o.UID):
		return invalidf("an object UID is a lowercase UUID")
	case o.Ready && o.UID == "":
		return invalidf("a ready object has an observed UID")
	}
	return nil
}

func checkObjects(namespace string, objs []ObjectRef) error {
	if len(objs) > MaxObjects {
		return invalidf("a deployment owns at most %d objects", MaxObjects)
	}
	seen := map[string]bool{}
	for _, o := range objs {
		if err := o.Validate(namespace); err != nil {
			return err
		}
		if seen[o.key()] {
			return invalidf("an object is listed once")
		}
		seen[o.key()] = true
	}
	return nil
}

func sortedObjects(in []ObjectRef) []ObjectRef {
	out := append([]ObjectRef{}, in...)
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

func sameObjects(a, b []ObjectRef) bool {
	a, b = sortedObjects(a), sortedObjects(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// DeploymentRecord is the plain persisted state of a Deployment.
type DeploymentRecord struct {
	Scope domain.Scope
	ID    DeploymentID

	Environment  domain.EnvironmentID
	ConfigDigest runtimev1.Digest
	Namespace    string

	Source   Source
	Image    Image
	Artifact DeployableArtifact
	Producer Producer

	Run       domain.RunID
	Step      string
	ActionKey string
	// Attempt is the originating attempt. Retry attempts of the same step
	// observe this record; they never replace it.
	Attempt domain.AttemptID
	// Fingerprint is the canonical identity of the logical request.
	Fingerprint runtimev1.Digest

	Status    Status
	Reason    string
	Objects   []ObjectRef
	Resources []ResourceRef

	ExpiresAt          *time.Time
	CleanupRequestedAt *time.Time
	CleanupState       CleanupState
	CleanupReason      string
	DeletedAt          *time.Time

	Version   domain.Version
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Deployment is the durable pre-apply record and its outcome: the intention is
// the row itself, there is no separate action subsystem.
type Deployment struct {
	rec   DeploymentRecord
	dirty bool
}

// Registration is everything a new deployment is created from. All of it is
// verified by the service before the record exists.
type Registration struct {
	Scope        domain.Scope
	Environment  domain.EnvironmentID
	ConfigDigest runtimev1.Digest
	Namespace    string
	Source       Source
	Image        Image
	Artifact     DeployableArtifact
	Producer     Producer
	Run          domain.RunID
	Step         string
	ActionKey    string
	Attempt      domain.AttemptID
	// ExpiresAt is the effective expiry, computed once from the pinned
	// snapshot's TTL. It is never recomputed.
	ExpiresAt *time.Time
}

var actionKeyPattern = regexp.MustCompile(runtimev1.Patterns["ActionKey"])

// Fingerprint is the canonical identity of the logical request: scope, pinned
// environment, source, image, run, step and action key. The originating attempt
// and caller are deliberately excluded: an authorized retry attempt of the same
// step asks for the same deployment.
func (r Registration) Fingerprint() (runtimev1.Digest, error) {
	var artifact *DeployableArtifact
	if r.Artifact.Kind != "" {
		a := r.Artifact
		artifact = &a
	}
	raw, err := json.Marshal(struct {
		Format      string              `json:"format"`
		Tenant      string              `json:"tenant"`
		Project     string              `json:"project"`
		Environment string              `json:"environment"`
		Config      string              `json:"config"`
		Source      Source              `json:"source"`
		Image       Image               `json:"image"`
		Artifact    *DeployableArtifact `json:"artifact,omitempty"`
		Run         string              `json:"run"`
		Step        string              `json:"step"`
		ActionKey   string              `json:"actionKey"`
	}{"kodama/deployment-request/v1", string(r.Scope.TenantID), string(r.Scope.ProjectID), string(r.Environment), string(r.ConfigDigest),
		r.Source, r.Image, artifact, string(r.Run), r.Step, r.ActionKey})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return runtimev1.Digest("sha256:" + hex.EncodeToString(sum[:])), nil
}

// NewDeployment creates the pre-apply record: pending/registered at version 1.
func NewDeployment(r Registration, now time.Time) (Deployment, error) {
	fp, err := r.Fingerprint()
	if err != nil {
		return Deployment{}, err
	}
	artifact, image := r.Artifact, r.Image
	if artifact.Kind == "" {
		artifact = FromImage(image)
	}
	if artifact.Kind == ArtifactOCIImage {
		image = artifact.Image()
	}
	rec := DeploymentRecord{
		Scope: r.Scope, ID: DeploymentIDFor(r.Scope, r.ActionKey), Environment: r.Environment, ConfigDigest: r.ConfigDigest, Namespace: r.Namespace,
		Source: r.Source, Image: image, Artifact: artifact, Producer: r.Producer, Run: r.Run, Step: r.Step, ActionKey: r.ActionKey, Attempt: r.Attempt,
		Fingerprint: fp, Status: StatusPending, Reason: ReasonRegistered, Objects: []ObjectRef{}, Resources: []ResourceRef{}, ExpiresAt: r.ExpiresAt,
		Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	d, err := RestoreDeployment(rec)
	if err != nil {
		return Deployment{}, err
	}
	d.dirty = true
	return d, nil
}

// RestoreDeployment rebuilds a deployment from persisted state and validates it.
func RestoreDeployment(rec DeploymentRecord) (Deployment, error) {
	if rec.Artifact.Kind == "" {
		rec.Artifact = FromImage(rec.Image)
	}
	for _, err := range []error{rec.Scope.Validate(), rec.ID.Validate(), rec.Environment.Validate(), rec.ConfigDigest.Validate(), rec.Artifact.Validate(),
		rec.Run.Validate(), rec.Attempt.Validate(), rec.Fingerprint.Validate(), rec.Version.Validate()} {
		if err != nil {
			return Deployment{}, invalidf("%v", err)
		}
	}
	if err := rec.Source.Validate(); err != nil {
		return Deployment{}, err
	}
	if err := rec.Producer.Validate(); err != nil {
		return Deployment{}, err
	}
	if !actionKeyPattern.MatchString(rec.ActionKey) || rec.Step == "" || len(rec.Step) > 128 {
		return Deployment{}, invalidf("a deployment needs a step and an action key")
	}
	if rec.Artifact.Kind == ArtifactOCIImage {
		if err := rec.Image.Validate(); err != nil {
			return Deployment{}, err
		}
		if rec.Image.ArtifactID != rec.Artifact.ArtifactID || rec.Image.Digest != rec.Artifact.Digest {
			return Deployment{}, invalidf("the legacy image projection does not match the artifact")
		}
	} else {
		if rec.Image != (Image{}) && (rec.Image.ArtifactID != rec.Artifact.ArtifactID || rec.Image.Digest != rec.Artifact.Digest) {
			return Deployment{}, invalidf("the legacy image projection does not match the bundle")
		}
		if rec.Namespace != "" && !namespaceName.MatchString(rec.Namespace) {
			return Deployment{}, invalidf("a deployment namespace is a Kubernetes DNS label")
		}
	}
	if rec.Artifact.Kind == ArtifactOCIImage && !namespaceName.MatchString(rec.Namespace) {
		return Deployment{}, invalidf("an OCI deployment needs a namespace")
	}
	if err := checkResources(rec.Resources); err != nil {
		return Deployment{}, err
	}
	switch rec.Status {
	case StatusPending, StatusRunning, StatusSucceeded, StatusFailed:
	default:
		return Deployment{}, invalidf("unknown deployment status")
	}
	if !reasonCode.MatchString(rec.Reason) {
		return Deployment{}, invalidf("a deployment reason is a code")
	}
	if err := checkObjects(rec.Namespace, rec.Objects); err != nil {
		return Deployment{}, err
	}
	rec.Objects = append([]ObjectRef{}, rec.Objects...)
	if rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() {
		return Deployment{}, invalidf("deployment timestamps are required")
	}
	return Deployment{rec: rec}, nil
}

// Record returns a copy of the persisted state.
func (d Deployment) Record() DeploymentRecord {
	r := d.rec
	r.Objects = append([]ObjectRef{}, r.Objects...)
	r.Resources = append([]ResourceRef{}, r.Resources...)
	return r
}

func (d Deployment) ID() DeploymentID        { return d.rec.ID }
func (d Deployment) Scope() domain.Scope     { return d.rec.Scope }
func (d Deployment) Status() Status          { return d.rec.Status }
func (d Deployment) Reason() string          { return d.rec.Reason }
func (d Deployment) Version() domain.Version { return d.rec.Version }
func (d Deployment) ActionKey() string       { return d.rec.ActionKey }
func (d Deployment) Dirty() bool             { return d.dirty }
func (d *Deployment) MarkClean()             { d.dirty = false }
func (d Deployment) OutcomeUnknown() bool {
	return d.rec.Status == StatusRunning && d.rec.Reason == ReasonOutcomeUnknown
}
func (d Deployment) Deleted() bool { return d.rec.DeletedAt != nil }
func (d Deployment) Pin() runtimev1.EnvironmentPin {
	return runtimev1.EnvironmentPin{ID: d.rec.Environment, ConfigDigest: d.rec.ConfigDigest}
}

// Expired reports a deployment past its effective expiry that was not deleted.
func (d Deployment) Expired(now time.Time) bool {
	return d.rec.ExpiresAt != nil && !now.Before(*d.rec.ExpiresAt) && d.rec.DeletedAt == nil
}

// Change describes one lifecycle transition for history and events.
type Change struct {
	FromStatus Status
	ToStatus   Status
	FromReason string
	ToReason   string
	// Changed is false for an idempotent replay.
	Changed bool
}

// StatusChanged reports a change of status (the event trigger).
func (c Change) StatusChanged() bool { return c.Changed && c.FromStatus != c.ToStatus }

func (d *Deployment) touch(now time.Time) {
	if !d.dirty {
		d.rec.Version++
		d.dirty = true
	}
	d.rec.UpdatedAt = now.UTC()
}

func (d *Deployment) move(to Status, reason string, now time.Time) Change {
	c := Change{FromStatus: d.rec.Status, ToStatus: to, FromReason: d.rec.Reason, ToReason: reason, Changed: true}
	d.rec.Status, d.rec.Reason = to, reason
	d.touch(now)
	return c
}

func (d *Deployment) same() Change {
	return Change{FromStatus: d.rec.Status, ToStatus: d.rec.Status, FromReason: d.rec.Reason, ToReason: d.rec.Reason}
}

// BeginApply records the intended external objects and moves pending to
// running, before any external change. A repeat with the same objects is a
// replay. While the outcome is unknown another apply is refused: the objects
// must be observed and reconciled first.
func (d *Deployment) BeginApply(intended []ObjectRef, now time.Time) (Change, error) {
	if len(intended) == 0 {
		return Change{}, invalidf("an apply names the objects it will create")
	}
	if err := checkObjects(d.rec.Namespace, intended); err != nil {
		return Change{}, err
	}
	for _, o := range intended {
		if o.UID != "" || o.Ready {
			return Change{}, invalidf("intended objects carry no observed state")
		}
	}
	switch {
	case d.OutcomeUnknown():
		return Change{}, Fail(ErrOutcomeUnknown, nil, "the previous apply has an unknown outcome; reconcile it first")
	case d.rec.Status == StatusPending:
		d.rec.Objects = sortedObjects(intended)
		return d.move(StatusRunning, ReasonApplying, now), nil
	case d.rec.Status == StatusRunning:
		if !sameObjects(d.rec.Objects, intended) {
			return Change{}, Fail(ErrConflict, nil, "the intended objects of a deployment do not change")
		}
		return d.same(), nil
	}
	return Change{}, Fail(ErrInvalidTransition, nil, "the deployment is %s", d.rec.Status)
}

// BeginApplyResources records the intended external resources of a
// provider-neutral target before any external change. It is the bundle/profile
// counterpart of BeginApply; a deployment uses one shape, not both.
func (d *Deployment) BeginApplyResources(intended []ResourceRef, now time.Time) (Change, error) {
	if len(intended) == 0 {
		return Change{}, invalidf("an apply names the resources it will create")
	}
	if err := checkResources(intended); err != nil {
		return Change{}, err
	}
	for _, r := range intended {
		if r.Ready {
			return Change{}, invalidf("intended resources carry no observed state")
		}
	}
	switch {
	case d.OutcomeUnknown():
		return Change{}, Fail(ErrOutcomeUnknown, nil, "the previous apply has an unknown outcome; reconcile it first")
	case d.rec.Status == StatusPending:
		d.rec.Resources = sortedResources(intended)
		return d.move(StatusRunning, ReasonApplying, now), nil
	case d.rec.Status == StatusRunning:
		if !sameResources(d.rec.Resources, intended) {
			return Change{}, Fail(ErrConflict, nil, "the intended resources of a deployment do not change")
		}
		return d.same(), nil
	}
	return Change{}, Fail(ErrInvalidTransition, nil, "the deployment is %s", d.rec.Status)
}

// MarkOutcomeUnknown records that the apply may have taken effect but could
// not be confirmed. It stays running; a new apply is blocked until settled.
func (d *Deployment) MarkOutcomeUnknown(now time.Time) (Change, error) {
	switch {
	case d.OutcomeUnknown():
		return d.same(), nil
	case d.rec.Status == StatusRunning:
		return d.move(StatusRunning, ReasonOutcomeUnknown, now), nil
	}
	return Change{}, Fail(ErrInvalidTransition, nil, "only a running deployment can have an unknown outcome")
}

// Settlement is a structured outcome: readiness observed per object, not a
// process exit code or output text.
type Settlement struct {
	Succeeded bool
	// Reason is the failure code of an unsuccessful settlement.
	Reason string
	// Objects are the observed Kubernetes objects with their UIDs and readiness.
	Objects []ObjectRef
	// Resources are the observed provider-neutral resources with their versions
	// and readiness. A bundle/profile deployment reports these instead of objects.
	Resources []ResourceRef
}

// Settle records the final outcome. It is also how an unknown outcome is
// reconciled. A replay of the identical settlement changes nothing; a
// different one never rewrites the settled history.
func (d *Deployment) Settle(s Settlement, now time.Time) (Change, error) {
	if err := checkObjects(d.rec.Namespace, s.Objects); err != nil {
		return Change{}, err
	}
	if err := checkResources(s.Resources); err != nil {
		return Change{}, err
	}
	intended := map[string]bool{}
	for _, o := range d.intendedKeys() {
		intended[o] = true
	}
	for _, o := range s.Objects {
		if !intended[o.key()] {
			return Change{}, Fail(ErrDenied, nil, "an observed object was not recorded for this deployment")
		}
	}
	intendedResources := map[string]bool{}
	for _, r := range d.intendedResourceKeys() {
		intendedResources[r] = true
	}
	for _, r := range s.Resources {
		if !intendedResources[r.key()] {
			return Change{}, Fail(ErrDenied, nil, "an observed resource was not recorded for this deployment")
		}
	}
	reason := ReasonReady
	if !s.Succeeded {
		reason = s.Reason
		if !reasonCode.MatchString(reason) || reason == ReasonReady || reason == ReasonApplying || reason == ReasonOutcomeUnknown || reason == ReasonRegistered {
			return Change{}, invalidf("a failure reports its own reason code")
		}
	}
	to := StatusFailed
	if s.Succeeded {
		to = StatusSucceeded
	}
	merged := d.merge(s.Objects)
	mergedResources := d.mergeResources(s.Resources)
	if d.rec.Status.Terminal() {
		if d.rec.Status == to && d.rec.Reason == reason && sameObjects(d.rec.Objects, merged) && sameResources(d.rec.Resources, mergedResources) {
			return d.same(), nil
		}
		return Change{}, Fail(ErrConflict, nil, "the deployment already settled with a different outcome")
	}
	if s.Succeeded {
		if d.rec.Status != StatusRunning {
			return Change{}, Fail(ErrInvalidTransition, nil, "a deployment must apply before it succeeds")
		}
		for _, o := range merged {
			if !o.Ready || o.UID == "" {
				return Change{}, invalidf("every intended object must be observed ready with its UID")
			}
		}
		for _, r := range mergedResources {
			if !r.Ready || r.ID == "" {
				return Change{}, invalidf("every intended resource must be observed ready")
			}
		}
	} else if d.rec.Status == StatusPending && (len(s.Objects) != 0 || len(s.Resources) != 0) {
		return Change{}, invalidf("a deployment that never applied has no observed state")
	}
	d.rec.Objects, d.rec.Resources = merged, mergedResources
	return d.move(to, reason, now), nil
}

func (d *Deployment) intendedKeys() []string {
	out := make([]string, 0, len(d.rec.Objects))
	for _, o := range d.rec.Objects {
		out = append(out, o.key())
	}
	return out
}

func (d *Deployment) intendedResourceKeys() []string {
	out := make([]string, 0, len(d.rec.Resources))
	for _, r := range d.rec.Resources {
		out = append(out, r.key())
	}
	return out
}

// merge overlays observed state on the recorded objects, keeping the intended
// identity of those not observed.
func (d *Deployment) merge(observed []ObjectRef) []ObjectRef {
	by := map[string]ObjectRef{}
	for _, o := range d.rec.Objects {
		by[o.key()] = o
	}
	for _, o := range observed {
		by[o.key()] = o
	}
	out := make([]ObjectRef, 0, len(by))
	for _, o := range by {
		out = append(out, o)
	}
	return sortedObjects(out)
}

func (d *Deployment) mergeResources(observed []ResourceRef) []ResourceRef {
	by := map[string]ResourceRef{}
	for _, r := range d.rec.Resources {
		by[r.key()] = r
	}
	for _, r := range observed {
		by[r.key()] = r
	}
	out := make([]ResourceRef, 0, len(by))
	for _, r := range by {
		out = append(out, r)
	}
	return sortedResources(out)
}

// RequestCleanup records the request for a cleanup of the owned objects. A
// deployment that is applying (or not yet applied) is not cleaned up; one whose
// outcome is unknown is, because its objects may exist. Repeating a request
// changes nothing.
func (d *Deployment) RequestCleanup(now time.Time) (bool, error) {
	if d.rec.DeletedAt != nil {
		return false, nil
	}
	if d.rec.CleanupRequestedAt != nil {
		return false, nil
	}
	if !d.rec.Status.Terminal() && !d.OutcomeUnknown() {
		return false, Fail(ErrInvalidTransition, nil, "a deployment that is not settled is not cleaned up")
	}
	t := now.UTC()
	d.rec.CleanupRequestedAt, d.rec.CleanupState = &t, CleanupRequested
	d.touch(now)
	return true, nil
}

// CleanupResult is a cleanup run's report about the owned objects.
type CleanupResult struct {
	State  CleanupState
	Reason string
	// Deleted lists the owned objects confirmed absent.
	Deleted []ObjectRef
	// DeletedResources lists the owned provider-neutral resources confirmed absent.
	DeletedResources []ResourceRef
}

// RecordCleanup applies a cleanup report. Deleting sets deleted_at without
// erasing the outcome or the recorded objects; failed and unknown results are
// kept as a visible exception and may be retried. A report about objects the
// deployment does not own, or before cleanup was requested, is refused.
func (d *Deployment) RecordCleanup(r CleanupResult, now time.Time) (bool, error) {
	if d.rec.CleanupRequestedAt == nil {
		return false, Fail(ErrInvalidTransition, nil, "cleanup was not requested for this deployment")
	}
	if err := checkObjects(d.rec.Namespace, r.Deleted); err != nil {
		return false, err
	}
	if err := checkResources(r.DeletedResources); err != nil {
		return false, err
	}
	owned := map[string]ObjectRef{}
	for _, o := range d.rec.Objects {
		owned[o.key()] = o
	}
	for _, o := range r.Deleted {
		rec, ok := owned[o.key()]
		if !ok || (rec.UID != "" && o.UID != "" && rec.UID != o.UID) {
			return false, Fail(ErrDenied, nil, "a reported object is not owned by this deployment")
		}
	}
	ownedResources := map[string]ResourceRef{}
	for _, r := range d.rec.Resources {
		ownedResources[r.key()] = r
	}
	for _, got := range r.DeletedResources {
		rec, ok := ownedResources[got.key()]
		if !ok || (rec.Version != "" && got.Version != "" && rec.Version != got.Version) {
			return false, Fail(ErrDenied, nil, "a reported resource is not owned by this deployment")
		}
	}
	switch r.State {
	case CleanupDone:
		if d.rec.DeletedAt != nil {
			return false, nil
		}
		if len(r.Deleted) != len(owned) || len(r.DeletedResources) != len(ownedResources) {
			return false, invalidf("a completed cleanup lists every owned object and resource")
		}
		t := now.UTC()
		d.rec.DeletedAt, d.rec.CleanupState, d.rec.CleanupReason = &t, CleanupDone, ""
		d.touch(now)
		return true, nil
	case CleanupFailed, CleanupUnknown:
		if d.rec.DeletedAt != nil {
			return false, Fail(ErrConflict, nil, "the deployment was already cleaned up")
		}
		if !reasonCode.MatchString(r.Reason) {
			return false, invalidf("a cleanup exception reports a reason code")
		}
		if d.rec.CleanupState == r.State && d.rec.CleanupReason == r.Reason {
			return false, nil
		}
		d.rec.CleanupState, d.rec.CleanupReason = r.State, r.Reason
		d.touch(now)
		return true, nil
	}
	return false, invalidf("a cleanup report is done, failed or unknown")
}

// AnnotationNames are the adapter-configured names of the reconciliation
// annotations a script puts on applied objects.
type AnnotationNames struct {
	Deployment    string
	Action        string
	Configuration string
}

// DefaultAnnotations is the fixture configuration published with this module.
var DefaultAnnotations = AnnotationNames{
	Deployment:    "kodama.optimi.com/deployment-id",
	Action:        "kodama.optimi.com/action-key",
	Configuration: "kodama.optimi.com/configuration-digest",
}

var annotationName = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)

// Validate checks that the names are distinct valid Kubernetes annotation keys.
func (a AnnotationNames) Validate() error {
	names := []string{a.Deployment, a.Action, a.Configuration}
	for i, n := range names {
		if !annotationName.MatchString(n) {
			return invalidf("annotation names must be valid Kubernetes annotation keys")
		}
		for _, m := range names[i+1:] {
			if n == m {
				return invalidf("annotation names must be distinct")
			}
		}
	}
	return nil
}

// Annotations returns the values a script writes on every applied object so a
// lost acknowledgment can be reconciled by observing the cluster.
func (d Deployment) Annotations(n AnnotationNames) map[string]string {
	return map[string]string{
		n.Deployment:    string(d.rec.ID),
		n.Action:        d.rec.ActionKey,
		n.Configuration: string(d.rec.ConfigDigest),
	}
}

// String never includes configuration or object content.
func (d Deployment) String() string {
	return fmt.Sprintf("deployment %s (%s/%s)", d.rec.ID, d.rec.Status, d.rec.Reason)
}
