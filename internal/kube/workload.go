package kube

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Phase is the external-effect state of one attempt's workload. It is
// bookkeeping about Kubernetes objects and is deliberately separate from
// execution's attempt statuses: a Job being owned says nothing about whether
// the attempt succeeded, and a gone workload never settles an attempt by
// itself (execution owns settlement).
//
//	planned -> credentialed -> registered -> create_issued -> owned -> deleting -> gone
//	                              ^   |  definite refusal      |
//	                              +---+                        +-> (late create) deleting
//
// Rules the transitions enforce:
//   - only an object the verifier accepted can become owned (Verification);
//   - a confirmed Job UID never changes;
//   - a create may be issued only from registered: an issued create whose
//     outcome is not known blocks another one;
//   - deleting and gone never return to creating.
type Phase string

const (
	PhasePlanned      Phase = "planned"
	PhaseCredentialed Phase = "credentialed"
	PhaseRegistered   Phase = "registered"
	PhaseCreateIssued Phase = "create_issued"
	PhaseOwned        Phase = "owned"
	PhaseDeleting     Phase = "deleting"
	PhaseGone         Phase = "gone"
)

// Identity is the immutable identity of an attempt's workload.
type Identity struct {
	Scope         domain.Scope
	RunID         domain.RunID
	StepID        string
	AttemptID     domain.AttemptID
	JobName       string
	Namespace     string
	ContextDigest string
	Fingerprint   string
	// Access is the committed workspace access. Empty means unknown (an object
	// claimed through its own annotations); it selects which bootstrap Secret key
	// sets are accepted (see checkSecretKeys).
	Access execution.Access
	// Shared is true for a run_shared attempt: its bootstrap Secret never carries
	// checkout material, whatever the workspace access.
	Shared bool
}

// Labels are the labels of the attempt's objects.
func (id Identity) Labels() map[string]string {
	return map[string]string{LabelManagedBy: ValueManagedBy, LabelJob: id.JobName}
}

// Annotations record ownership evidence on the attempt's objects.
func (id Identity) Annotations() map[string]string {
	return map[string]string{
		AnnTenant: string(id.Scope.TenantID), AnnProject: string(id.Scope.ProjectID), AnnRun: string(id.RunID),
		AnnStep: id.StepID, AnnAttempt: string(id.AttemptID), AnnContextDigest: id.ContextDigest,
	}
}

// Verification is the proof that a Kubernetes object is the attempt's
// workload: it is produced only by VerifyJob, so an unverified object cannot be
// adopted.
type Verification struct {
	attempt domain.AttemptID
	jobName string
	uid     string
	ok      bool
}

// UID is the verified Job UID.
func (v Verification) UID() string { return v.uid }

// Workload is the transition-checked record of one attempt's external effects.
type Workload struct {
	Identity Identity
	Phase    Phase
	// JobUID is the confirmed Job UID once owned; it never changes.
	JobUID string
	// IssuedAt is when the create was issued (zero before).
	IssuedAt time.Time
	// Uncertain is true while a create's outcome is unknown.
	Uncertain bool
	// Launch is the launch evidence collected from Pods before they are
	// deleted. It only ever becomes more informative: unknown yields to a
	// definite value, never the reverse (started outranks not_started).
	Launch execution.LaunchEvidence
}

// NewWorkload starts a planned record.
func NewWorkload(id Identity) *Workload {
	return &Workload{Identity: id, Phase: PhasePlanned, Launch: execution.LaunchUnknown}
}

// NoteLaunch records launch evidence and reports whether the record changed.
// Evidence only gains information: started (the command may have run) outranks
// not_started, which outranks unknown, and nothing ever replaces a stronger
// value with a weaker one. Persisting the stronger value before a Pod is
// deleted keeps it across restarts.
func (w *Workload) NoteLaunch(l execution.LaunchEvidence) bool {
	if w.Launch == "" {
		w.Launch = execution.LaunchUnknown
	}
	merged := mergeLaunch(w.Launch, l)
	if merged == w.Launch {
		return false
	}
	w.Launch = merged
	return true
}

func (w *Workload) fail(to Phase) error {
	return wrap(ErrInvalidTransition, "%s -> %s", w.Phase, to)
}

// Credentialed records that the admission Secret holds the attempt's credential.
func (w *Workload) Credentialed() error {
	switch w.Phase {
	case PhasePlanned:
		w.Phase = PhaseCredentialed
		return nil
	case PhaseCredentialed:
		return nil
	}
	return w.fail(PhaseCredentialed)
}

// Registered records that runtime admission holds the credential's grant.
func (w *Workload) Registered() error {
	switch w.Phase {
	case PhaseCredentialed:
		w.Phase = PhaseRegistered
		return nil
	case PhaseRegistered:
		return nil
	}
	return w.fail(PhaseRegistered)
}

// CanCreate reports whether a Job create may be issued now.
func (w *Workload) CanCreate() bool { return w.Phase == PhaseRegistered }

// IssueCreate records the intent to create before the create is sent.
func (w *Workload) IssueCreate(now time.Time) error {
	if w.Phase != PhaseRegistered {
		return w.fail(PhaseCreateIssued)
	}
	w.Phase, w.IssuedAt, w.Uncertain = PhaseCreateIssued, now.UTC(), false
	return nil
}

// CreateRefused records that the create definitely was not applied (the API
// server refused it), which makes another create safe.
func (w *Workload) CreateRefused() error {
	if w.Phase != PhaseCreateIssued || w.JobUID != "" {
		return w.fail(PhaseRegistered)
	}
	w.Phase, w.IssuedAt, w.Uncertain = PhaseRegistered, time.Time{}, false
	return nil
}

// CreateUnknown records that the create's outcome is unknown.
func (w *Workload) CreateUnknown() error {
	if w.Phase != PhaseCreateIssued {
		return w.fail(PhaseCreateIssued)
	}
	w.Uncertain = true
	return nil
}

// Adopt makes a verified Job the attempt's owned workload. The UID is fixed from
// then on.
func (w *Workload) Adopt(v Verification) error {
	if !v.ok || v.attempt != w.Identity.AttemptID || v.jobName != w.Identity.JobName || v.uid == "" {
		return wrap(ErrForeignWorkload, "the object was not verified for this attempt")
	}
	if w.JobUID != "" && w.JobUID != v.uid {
		return wrap(ErrForeignWorkload, "the confirmed Job UID cannot change")
	}
	switch w.Phase {
	case PhaseRegistered, PhaseCreateIssued, PhaseOwned:
		w.Phase, w.JobUID, w.Uncertain = PhaseOwned, v.uid, false
		return nil
	}
	return w.fail(PhaseOwned)
}

// BeginDelete records the decision to delete a verified Job. A late-created Job
// of a workload already observed gone may be deleted (it is never adopted).
func (w *Workload) BeginDelete(v Verification) error {
	if !v.ok || v.attempt != w.Identity.AttemptID || v.jobName != w.Identity.JobName || v.uid == "" {
		return wrap(ErrForeignWorkload, "only a verified workload can be deleted")
	}
	if w.JobUID != "" && w.JobUID != v.uid {
		return wrap(ErrForeignWorkload, "the confirmed Job UID cannot change")
	}
	switch w.Phase {
	case PhaseRegistered, PhaseCreateIssued, PhaseOwned, PhaseDeleting, PhaseGone:
		w.Phase, w.JobUID = PhaseDeleting, v.uid
		return nil
	}
	return w.fail(PhaseDeleting)
}

// ObserveGone records that the Job and its Pods are verified absent. From
// create_issued it requires quiescent: the time in which a still-in-flight
// create could land has passed. A create that may still land keeps the record
// uncertain.
func (w *Workload) ObserveGone(quiescent bool) error {
	switch w.Phase {
	case PhaseOwned, PhaseDeleting, PhaseGone:
		w.Phase = PhaseGone
		return nil
	case PhasePlanned, PhaseCredentialed, PhaseRegistered:
		// Never created: nothing can land later.
		w.Phase = PhaseGone
		return nil
	case PhaseCreateIssued:
		if !quiescent {
			return wrap(ErrCreateUncertain, "a create may still land")
		}
		w.Phase = PhaseGone
		return nil
	}
	return w.fail(PhaseGone)
}

// NeverCreated reports whether no create was ever issued: the executor holds
// affirmative evidence that no workload exists or can exist for this attempt
// through its own action.
func (w *Workload) NeverCreated() bool {
	switch w.Phase {
	case PhasePlanned, PhaseCredentialed, PhaseRegistered:
		return w.IssuedAt.IsZero()
	}
	return false
}

// record is the persisted form kept in the admission Secret's annotation. It is
// external-effect bookkeeping, not authority: nothing in it grants access.
type record struct {
	Version   int       `json:"v"`
	Attempt   string    `json:"attempt"`
	Phase     Phase     `json:"phase"`
	JobUID    string    `json:"uid,omitempty"`
	IssuedAt  time.Time `json:"issuedAt,omitempty"`
	Uncertain bool      `json:"uncertain,omitempty"`
	Launch    string    `json:"launch,omitempty"`
}

func (w *Workload) marshal() string {
	b, _ := json.Marshal(record{Version: 1, Attempt: string(w.Identity.AttemptID), Phase: w.Phase, JobUID: w.JobUID, IssuedAt: w.IssuedAt, Uncertain: w.Uncertain, Launch: string(w.Launch)})
	return string(b)
}

// parseWorkload restores a record for an identity. A record of another attempt
// or an unknown phase is refused.
func parseWorkload(id Identity, s string) (*Workload, error) {
	var r record
	if err := json.Unmarshal([]byte(s), &r); err != nil || r.Version != 1 || r.Attempt != string(id.AttemptID) {
		return nil, wrap(ErrForeignWorkload, "the workload record does not belong to this attempt")
	}
	switch r.Phase {
	case PhasePlanned, PhaseCredentialed, PhaseRegistered, PhaseCreateIssued, PhaseOwned, PhaseDeleting, PhaseGone:
	default:
		return nil, wrap(ErrForeignWorkload, "unknown workload phase")
	}
	if r.Phase == PhaseOwned && r.JobUID == "" {
		return nil, wrap(ErrForeignWorkload, "inconsistent workload record")
	}
	launch := execution.LaunchEvidence(r.Launch)
	switch launch {
	case "":
		launch = execution.LaunchUnknown
	case execution.LaunchUnknown, execution.LaunchStarted, execution.LaunchNotStarted:
	default:
		return nil, wrap(ErrForeignWorkload, "unknown launch evidence")
	}
	return &Workload{Identity: id, Phase: r.Phase, JobUID: r.JobUID, IssuedAt: r.IssuedAt.UTC(), Uncertain: r.Uncertain, Launch: launch}, nil
}

func (w *Workload) String() string {
	return fmt.Sprintf("workload{%s %s}", w.Identity.JobName, w.Phase)
}
