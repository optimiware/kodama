package memory

import (
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
)

// Rows are stored by value and copied on every read, so an aggregate handed to
// a mutation can never alias stored state: a rolled back mutation leaves no
// trace, as with a database. Tables hold no data; the rows live in the memdb
// Store and are reached only through a tenant-bound Tx.

func key(parts ...string) string { return strings.Join(parts, "\x00") }

func runKey(scope domain.Scope, id domain.RunID) string {
	return key(string(scope.ProjectID), string(id))
}

type stepRow struct {
	Project domain.ProjectID
	Step    execution.StepRun
}

type controlRow struct {
	Project domain.ProjectID
	Run     domain.RunID
	Control execution.Control
}

// removalRow records, once, that a settled attempt's workload is gone.
type removalRow struct {
	Project domain.ProjectID
	Attempt domain.AttemptID
	At      time.Time
}

// Evidence is one append-only late observation, late result, mismatch,
// termination or reconciliation record of an attempt.
type Evidence struct {
	Seq     string
	Scope   domain.Scope
	Run     domain.RunID
	Attempt domain.AttemptID
	Kind    execution.EvidenceKind
	Detail  map[string]any
	At      time.Time
}

// Reservation is one capacity reservation. ReleasedAt is set once.
type Reservation struct {
	Scope      domain.Scope
	Attempt    domain.AttemptID
	Run        domain.RunID
	HeldAt     time.Time
	ReleasedAt *time.Time
}

// Held reports whether the reservation still counts against capacity.
func (r Reservation) Held() bool { return r.ReleasedAt == nil }

// History is one immutable state-change row.
type History struct {
	Seq        string
	Scope      domain.Scope
	Run        domain.RunID
	Subject    execution.SubjectKind
	SubjectID  string
	StepID     string
	From, To   string
	Reason     execution.Reason
	RunVersion domain.Version
	Actor      execution.Actor
	At         time.Time
}

// Audit is one audit row written with a state change.
type Audit struct {
	Seq          string
	Scope        domain.Scope
	Action       string
	ResourceType string
	ResourceID   string
	Version      domain.Version
	Actor        execution.Actor
	Reason       string
	RequestID    string
	Metadata     map[string]any
	At           time.Time
}

// cancelRequest is the durable item-scoped cancellation intent.
type cancelRequest struct {
	Scope   domain.Scope
	Item    domain.WorkItemID
	Version domain.Version
	At      time.Time
}

// sequence is the tenant's monotonic counter of append-only rows (history,
// audit, evidence). It lives in the kernel next to the rows it numbers, so it
// commits and rolls back with them and every Store over the same kernel
// continues one numbering instead of restarting at zero.
type sequence struct {
	Name string
	N    int64
}

var (
	seqT = memdb.NewTable("execution_sequence", func(s sequence) string { return s.Name })

	runsT = memdb.NewTable("execution_runs", func(r execution.Run) string { return runKey(r.Scope, r.ID) })

	stepsT = memdb.NewTable("execution_steps", func(r stepRow) string {
		return key(string(r.Project), string(r.Step.RunID), r.Step.ID)
	})

	attemptsT = memdb.NewTable("execution_attempts", func(a execution.Attempt) string {
		return key(string(a.Scope.ProjectID), string(a.ID))
	})

	controlsT = memdb.NewTable("execution_controls", func(c controlRow) string {
		return key(string(c.Project), string(c.Control.AttemptID))
	})

	removalsT = memdb.NewTable("execution_removals", func(r removalRow) string {
		return key(string(r.Project), string(r.Attempt))
	})

	evidenceT = memdb.NewTable("execution_evidence", func(e Evidence) string {
		return key(string(e.Scope.ProjectID), e.Seq)
	})

	reservationsT = memdb.NewTable("execution_reservations", func(r Reservation) string {
		return key(string(r.Scope.ProjectID), string(r.Attempt))
	})

	cancelsT = memdb.NewTable("execution_cancellations", func(c cancelRequest) string {
		return key(string(c.Scope.ProjectID), string(c.Item))
	})

	historyT = memdb.NewTable("execution_history", func(h History) string {
		return key(string(h.Scope.ProjectID), h.Seq)
	})

	auditT = memdb.NewTable("execution_audit", func(a Audit) string {
		return key(string(a.Scope.ProjectID), a.Seq)
	})
)

// collect returns the tenant's rows of a table that keep accepts, in key order.
func collect[V any](tx memdb.Tx, t *memdb.Table[V], keep func(V) bool) []V {
	var out []V
	t.Scan(tx, func(v V) bool {
		if keep == nil || keep(v) {
			out = append(out, v)
		}
		return true
	})
	return out
}
