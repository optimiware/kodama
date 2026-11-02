package gitwebhook

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// This file is the native half of the package (T-137, ADR 0011): triggers of
// the internal Git service. A native trigger reacts to the reference-only
// git.refs_updated event of an applied push record; a task route reacts to a
// work item becoming ready. Neither carries a secret or a URL: the HMAC
// receive-hook trigger of the external hosts (trigger.go, service.go) is a
// separate mechanism and is untouched.

const maxConfiguredByLen = 256

// PushTrigger is the stored configuration of one native trigger: the Git
// contract's TriggerConfig (one repository, one to 32 ref patterns, the origin
// filter, the definition and the enabled flag) with its owner and bookkeeping.
// Tenant and project come only from this record, never from an event.
type PushTrigger struct {
	Scope  domain.Scope
	Config gitv1.TriggerConfig
	// ConfiguredBy is the principal who created or last altered the trigger. Run
	// admission checks that principal's *current* run authority on every start
	// (a trigger never carries authority of its own), so a revoked configurer
	// stops the trigger from starting runs.
	ConfiguredBy string
	Version      int64
	// EnabledAt is when the trigger was created enabled or last re-enabled. A
	// push whose event was committed before it never starts a run, so a late
	// redelivery cannot start runs for pushes the trigger was not yet watching.
	EnabledAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ID is the trigger's identifier.
func (t PushTrigger) ID() string { return t.Config.TriggerID }

// Excludes reports whether a run's own push is excluded (the default).
func (t PushTrigger) Excludes() bool { return t.Config.Excludes() }

// Validate checks the invariants every stored trigger satisfies.
func (t PushTrigger) Validate() error {
	if err := t.Scope.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := t.Config.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if t.Config.ExcludeOriginRuns == nil {
		return invalidf("a stored trigger states excludeOriginRuns explicitly")
	}
	if t.ConfiguredBy == "" || len(t.ConfiguredBy) > maxConfiguredByLen {
		return invalidf("configuring principal must be 1..%d bytes", maxConfiguredByLen)
	}
	if t.Version < 1 {
		return invalidf("version starts at 1")
	}
	if t.EnabledAt.IsZero() {
		return invalidf("a stored trigger states when it was enabled")
	}
	return nil
}

// Clone returns a copy that shares no slice or pointer with t.
func (t PushTrigger) Clone() PushTrigger {
	t.Config = cloneConfig(t.Config)
	return t
}

func cloneConfig(c gitv1.TriggerConfig) gitv1.TriggerConfig {
	c.RefPatterns = append([]string(nil), c.RefPatterns...)
	if c.ExcludeOriginRuns != nil {
		v := *c.ExcludeOriginRuns
		c.ExcludeOriginRuns = &v
	}
	return c
}

// normalize makes the optional member explicit: an omitted excludeOriginRuns is
// true, and storage keeps the effective value.
func normalize(c gitv1.TriggerConfig) gitv1.TriggerConfig {
	c = cloneConfig(c)
	v := c.Excludes()
	c.ExcludeOriginRuns = &v
	return c
}

// TaskRoute is a project's configured pipeline for ready work: when an item
// becomes ready, a work_item run of the definition is admitted for it through
// the native source-set path (the same admission as every other origin).
// There is at most one route per project.
type TaskRoute struct {
	Scope        domain.Scope
	DefinitionID domain.DefinitionID
	Enabled      bool
	// ConfiguredBy is the principal whose current run authority admission checks.
	ConfiguredBy string
	Version      int64
	// EnabledAt is when the route was created enabled or last re-enabled; a
	// readiness event committed before it starts nothing.
	EnabledAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate checks the invariants every stored route satisfies.
func (r TaskRoute) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := r.DefinitionID.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if r.ConfiguredBy == "" || len(r.ConfiguredBy) > maxConfiguredByLen {
		return invalidf("configuring principal must be 1..%d bytes", maxConfiguredByLen)
	}
	if r.Version < 1 {
		return invalidf("version starts at 1")
	}
	if r.EnabledAt.IsZero() {
		return invalidf("a stored route states when it was enabled")
	}
	return nil
}

// InboxKind names what an inbox row deduplicates.
type InboxKind string

const (
	// InboxPush is one (trigger, push record, ref).
	InboxPush InboxKind = "push"
	// InboxTaskReady is one (project route, readiness transition).
	InboxTaskReady InboxKind = "task_ready"
)

// InboxKey identifies one admission decision. For InboxPush Source is the
// trigger, Subject the push record and Ref the pushed ref; for InboxTaskReady
// Source is the project, Subject the work item's readiness transition and Ref
// is empty.
type InboxKey struct {
	Scope   domain.Scope
	Kind    InboxKind
	Source  string
	Subject string
	Ref     string
}

// Validate checks the key's shape and bounds.
func (k InboxKey) Validate() error {
	if err := k.Scope.Validate(); err != nil {
		return invalidf("%v", err)
	}
	switch k.Kind {
	case InboxPush:
		if k.Source == "" || k.Subject == "" || k.Ref == "" {
			return invalidf("a push key names trigger, push record and ref")
		}
	case InboxTaskReady:
		if k.Source == "" || k.Subject == "" || k.Ref != "" {
			return invalidf("a task_ready key names the route and the transition only")
		}
	default:
		return invalidf("unknown inbox kind %q", k.Kind)
	}
	for _, v := range []string{k.Source, k.Subject, k.Ref} {
		if len(v) > 512 {
			return invalidf("inbox key member is longer than 512 bytes")
		}
	}
	return nil
}

// RunID is the stable identity of the run the key admits: a digest of the
// scope and every member of the key, so a redelivery, a second consumer and a
// restart all derive the same run, and the execution module's own run
// idempotency (the same run ID and declaration is a replay, another declaration
// a conflict) is the at-most-one guarantee of PX-24.
func (k InboxKey) RunID() domain.RunID {
	h := sha256.New()
	for _, part := range []string{"kodama-native-trigger-v1", string(k.Kind), string(k.Scope.TenantID), string(k.Scope.ProjectID), k.Source, k.Subject, k.Ref} {
		fmt.Fprintf(h, "%d:%s;", len(part), part)
	}
	return domain.RunID("run_nt_" + hex.EncodeToString(h.Sum(nil))[:40])
}

// InboxRecord is the durable claim and outcome of one InboxKey. It is created
// by the first consumer to see the key; the outcome is write-once.
type InboxRecord struct {
	Key InboxKey
	// LeaseHolder and LeaseUntil are the consumer attempting admission; they
	// limit contention only. Correctness rests on the run's stable identity.
	LeaseHolder string
	LeaseUntil  time.Time
	Done        bool
	Outcome     Outcome
	Reason      string
	RunID       domain.RunID
	ReceivedAt  time.Time
	CompletedAt time.Time
}

// InboxFinish is the terminal result handed to NativeStore.FinishInbox.
type InboxFinish struct {
	Outcome Outcome
	Reason  string
	RunID   domain.RunID
	// Holder is the lease holder recording a non-started result.
	Holder string
}

// Stable reasons of the native path, recorded with outcomes and audit.
const (
	ReasonOriginRunExcluded = "origin_run_excluded"
	ReasonRefFiltered       = "ref_filtered"
	ReasonEntryNotApplied   = "entry_not_applied"
	ReasonRefDeleted        = "ref_deleted"
	ReasonNotReady          = "work_item_not_ready"
	ReasonNotActionable     = "work_item_not_actionable"
	ReasonWorkCancelled     = "work_item_cancelled"
	ReasonWorkBlocked       = "work_item_blocked"
	// ReasonBeforeEnabled: the event was committed before the trigger or route
	// was created or last re-enabled.
	ReasonBeforeEnabled = "event_before_enabled"
)

// Audit action names of the native path.
const (
	AuditPushTriggerCreated  = "git_push_trigger.created"
	AuditPushTriggerUpdated  = "git_push_trigger.updated"
	AuditPushRunStarted      = "git_push_trigger.run_started"
	AuditPushRunRefused      = "git_push_trigger.run_refused"
	AuditPushRunIgnored      = "git_push_trigger.run_ignored"
	AuditTaskRouteConfigured = "git_task_route.configured"
	AuditTaskRunStarted      = "git_task_route.run_started"
	AuditTaskRunRefused      = "git_task_route.run_refused"
	AuditTaskRunIgnored      = "git_task_route.run_ignored"
)
