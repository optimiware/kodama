package logstest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
	"github.com/optimiware/kodama/internal/memdb"
)

// Memory is the in-memory log world of the `development` environment and the
// unit gate: the memory adapters of every log port over one memdb store, so the
// read service, the controller fallback and the retention policy run their real
// code with no database, Kubernetes API or object store.
//
//   - Store is logs.Store (tenant-scoped capture records, fenced claims, audit);
//   - Attempts and Workloads are the execution module's scoped lookups;
//   - Retained is logs.Retained over in-memory diagnostic artifacts and a staged
//     object map (an in-memory object-store adapter);
//   - Authority is a fake read authority; Live a fake Kubernetes log reader.
//
// What it does not model (SD-14): SQL, row-level security, row locks, lock
// order and isolation levels, artifact-module ownership constraints that the
// PostgreSQL guard trigger enforces on linkage, real object-store semantics and
// restart persistence. A pass here never certifies the atomicity of the real
// transaction; T-026 runs the same Contract suites against PostgreSQL.
type Memory struct {
	db *memdb.Store

	Clock     *MemoryClock
	Authority *MemoryAuthority
	Live      *MemoryLive
	Redact    *Redactions

	seq  atomic.Int64
	fail atomic.Int32

	mu     sync.Mutex
	staged map[string][]byte // frozen controller bytes already uploaded
	store  struct {
		err      error // returned once by the next Retained.Store, before any effect
		afterErr error // returned once after the next Store staged its bytes
	}
}

// NewMemory returns an empty world with the default read bounds of Limits().
func NewMemory() *Memory {
	m := &Memory{Clock: NewMemoryClock(), Authority: NewMemoryAuthority(), Live: NewMemoryLive(), Redact: &Redactions{}, staged: map[string][]byte{}}
	m.db = memdb.New(memdb.WithCommitFault(func() error {
		if m.fail.Load() > 0 && m.fail.Add(-1) >= 0 {
			return errors.New("logstest: injected commit failure")
		}
		return nil
	}))
	return m
}

// FailNextCommits makes the next n store transactions fail at commit: nothing
// they wrote is kept.
func (m *Memory) FailNextCommits(n int) { m.fail.Store(int32(n)) }

// MemoryClock is a controllable clock.
type MemoryClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewMemoryClock starts at a fixed instant.
func NewMemoryClock() *MemoryClock {
	return &MemoryClock{t: time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)}
}

// Now returns the current instant.
func (c *MemoryClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward.
func (c *MemoryClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ---- rows ----

type attemptRow struct{ Info logs.AttemptInfo }

type recordRow struct{ R *logs.Record }

type auditRow struct {
	ID       string
	Project  domain.ProjectID
	Record   logs.RecordID
	Action   string
	Request  string
	Metadata map[string]any
}

type objectRow struct {
	ID      domain.ArtifactID
	Scope   domain.Scope
	Attempt domain.AttemptID
	Name    string
	Media   string
	Data    []byte
	Usable  bool
	Removed bool
}

var (
	attemptsTbl = memdb.NewTable("logs_attempts", func(r attemptRow) string { return string(r.Info.ID) })
	recordsTbl  = memdb.NewTable("logs_records",
		func(r recordRow) string { return string(r.R.Scope.ProjectID) + "/" + string(r.R.ID) },
		func(r recordRow) string {
			return fmt.Sprintf("%s/%s/%s/%d", r.R.Scope.ProjectID, r.R.Attempt, r.R.Source, r.R.Generation)
		})
	auditTbl   = memdb.NewTable("logs_audit", func(r auditRow) string { return r.ID })
	objectsTbl = memdb.NewTable("logs_objects", func(r objectRow) string { return string(r.ID) },
		func(r objectRow) string { return string(r.Scope.ProjectID) + "/" + string(r.Attempt) + "/" + r.Name })
)

func cloneRecord(r *logs.Record) *logs.Record {
	c := *r
	if r.Snapshot != nil {
		s := *r.Snapshot
		s.Reasons = append([]logs.Reason(nil), s.Reasons...)
		c.Snapshot = &s
	}
	if r.Link != nil {
		l := *r.Link
		l.Reasons = append([]logs.Reason(nil), l.Reasons...)
		if l.Meta != nil {
			mt := *l.Meta
			l.Meta = &mt
		}
		c.Link = &l
	}
	if r.RemovedAt != nil {
		t := *r.RemovedAt
		c.RemovedAt = &t
	}
	return &c
}

// ---- attempts (fixtures and the logs.Attempts / logs.Workloads ports) ----

// MemoryAttempt is a created attempt.
type MemoryAttempt struct {
	Scope domain.Scope
	Run   domain.RunID
	ID    domain.AttemptID
	Step  string
	Job   string
}

// AttemptOptions describe an attempt to create.
type AttemptOptions struct {
	// Run joins an existing run (several attempts of one run).
	Run domain.RunID
	// Observed records the Job as observed with UID JobUID, so a live workload
	// can be selected.
	Observed bool
	// Terminal marks the attempt finished.
	Terminal bool
}

// NewAttempt creates an attempt of a run in scope.
func (m *Memory) NewAttempt(tb testing.TB, scope domain.Scope, o AttemptOptions) MemoryAttempt {
	tb.Helper()
	n := m.seq.Add(1)
	a := MemoryAttempt{Scope: scope, Run: o.Run, ID: domain.AttemptID(fmt.Sprintf("att_%d", n)), Step: fmt.Sprintf("step%d", n), Job: fmt.Sprintf("job-%d", n)}
	if a.Run == "" {
		a.Run = domain.RunID(fmt.Sprintf("run_%d", n))
	}
	info := logs.AttemptInfo{Scope: scope, Run: a.Run, Step: a.Step, ID: a.ID, Ordinal: 1, JobName: a.Job, Terminal: o.Terminal}
	if o.Observed {
		info.JobUID = JobUID
	}
	if err := m.db.Update(context.Background(), scope.TenantID, func(tx memdb.Tx) error {
		info.Ordinal = len(m.attemptsOf(tx, scope, a.Run)) + 1
		return attemptsTbl.Put(tx, attemptRow{Info: info})
	}); err != nil {
		tb.Fatal(err)
	}
	return a
}

// Finish marks the attempt terminal.
func (m *Memory) Finish(tb testing.TB, a MemoryAttempt) {
	tb.Helper()
	err := m.db.Update(context.Background(), a.Scope.TenantID, func(tx memdb.Tx) error {
		row, ok := attemptsTbl.Get(tx, string(a.ID))
		if !ok {
			return logs.ErrNotFound
		}
		row.Info.Terminal = true
		return attemptsTbl.Put(tx, row)
	})
	if err != nil {
		tb.Fatal(err)
	}
}

func (m *Memory) attemptsOf(tx memdb.Tx, scope domain.Scope, run domain.RunID) []logs.AttemptInfo {
	var out []logs.AttemptInfo
	attemptsTbl.Scan(tx, func(r attemptRow) bool {
		if r.Info.Scope == scope && r.Info.Run == run {
			out = append(out, r.Info)
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out
}

// Attempts implements logs.Attempts over the world's attempts.
func (m *Memory) Attempts() logs.Attempts { return (*memAttempts)(m) }

type memAttempts Memory

func (a *memAttempts) Attempt(ctx context.Context, scope domain.Scope, run domain.RunID, id domain.AttemptID) (logs.AttemptInfo, error) {
	if scope.Validate() != nil || run.Validate() != nil || id.Validate() != nil {
		return logs.AttemptInfo{}, logs.ErrNotFound
	}
	var info logs.AttemptInfo
	err := a.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		row, ok := attemptsTbl.Get(tx, string(id))
		if !ok || row.Info.Scope != scope || row.Info.Run != run {
			return logs.ErrNotFound
		}
		info = row.Info
		return nil
	})
	return info, err
}

func (a *memAttempts) ByID(ctx context.Context, scope domain.Scope, id domain.AttemptID) (logs.AttemptInfo, error) {
	if scope.Validate() != nil || id.Validate() != nil {
		return logs.AttemptInfo{}, logs.ErrNotFound
	}
	var info logs.AttemptInfo
	err := a.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		row, ok := attemptsTbl.Get(tx, string(id))
		if !ok || row.Info.Scope != scope {
			return logs.ErrNotFound
		}
		info = row.Info
		return nil
	})
	return info, err
}

func (a *memAttempts) Attempts(ctx context.Context, scope domain.Scope, run domain.RunID) ([]logs.AttemptInfo, error) {
	if scope.Validate() != nil || run.Validate() != nil {
		return nil, logs.ErrNotFound
	}
	var out []logs.AttemptInfo
	err := a.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		out = (*Memory)(a).attemptsOf(tx, scope, run)
		return nil
	})
	return out, err
}

// Workloads implements logs.Workloads: the trusted target is built from the
// attempt's recorded Job identity and composition's placement, never from a
// caller.
func (m *Memory) Workloads() logs.Workloads { return memWorkloads{} }

type memWorkloads struct{}

func (memWorkloads) Target(_ context.Context, a logs.AttemptInfo) (logs.Target, error) {
	if a.JobName == "" || a.JobUID == "" {
		return logs.Target{}, logs.ErrWorkloadNotObserved
	}
	return logs.Target{Namespace: Namespace, JobName: a.JobName, JobUID: a.JobUID, Container: Container}, nil
}

// ---- authority ----

// MemoryAuthority is a fake logs.Authority: principals are members of scopes.
// A known principal outside the scope gets ErrNotFound (the scope is hidden); a
// denied member gets ErrDenied; anything else is an outage.
type MemoryAuthority struct {
	mu      sync.Mutex
	members map[string]map[domain.Scope]bool
	denied  map[string]map[domain.Scope]bool
	err     error
	asked   int
}

// NewMemoryAuthority returns an authority with no members.
func NewMemoryAuthority() *MemoryAuthority {
	return &MemoryAuthority{members: map[string]map[domain.Scope]bool{}, denied: map[string]map[domain.Scope]bool{}}
}

// Grant makes principal a reader of scope.
func (a *MemoryAuthority) Grant(principal string, scope domain.Scope) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.members[principal] == nil {
		a.members[principal] = map[domain.Scope]bool{}
	}
	a.members[principal][scope] = true
}

// Revoke removes the membership.
func (a *MemoryAuthority) Revoke(principal string, scope domain.Scope) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.members[principal], scope)
}

// Deny makes a member's read refused (known but not permitted).
func (a *MemoryAuthority) Deny(principal string, scope domain.Scope) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.denied[principal] == nil {
		a.denied[principal] = map[domain.Scope]bool{}
	}
	a.denied[principal][scope] = true
}

// Fail makes every question an outage (nil restores service).
func (a *MemoryAuthority) Fail(err error) {
	a.mu.Lock()
	a.err = err
	a.mu.Unlock()
}

// Asked is the number of questions asked.
func (a *MemoryAuthority) Asked() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked
}

// AuthorizeRead implements logs.Authority.
func (a *MemoryAuthority) AuthorizeRead(_ context.Context, principal string, scope domain.Scope) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked++
	switch {
	case a.err != nil:
		return a.err
	case a.denied[principal][scope]:
		return logs.ErrDenied
	case a.members[principal][scope]:
		return nil
	}
	return logs.ErrNotFound
}

// ---- live reader ----

// MemoryLive is a fake Kubernetes log reader: it serves the log of a Job after
// verifying the target the service resolved, applies the tail and byte bounds
// the query carries (as the log subresource does) and records every query.
type MemoryLive struct {
	mu      sync.Mutex
	jobs    map[string]memJob
	err     error
	queries []logs.LiveQuery
	targets []logs.Target
	open    int
}

type memJob struct {
	uid  string
	body string
}

// NewMemoryLive returns a reader with no workloads.
func NewMemoryLive() *MemoryLive { return &MemoryLive{jobs: map[string]memJob{}} }

// Set creates or replaces the Job's log.
func (l *MemoryLive) Set(job, uid, body string) {
	l.mu.Lock()
	l.jobs[job] = memJob{uid: uid, body: body}
	l.mu.Unlock()
}

// Remove deletes the Job.
func (l *MemoryLive) Remove(job string) {
	l.mu.Lock()
	delete(l.jobs, job)
	l.mu.Unlock()
}

// Fail makes every open fail with err (nil restores service).
func (l *MemoryLive) Fail(err error) {
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
}

// Queries returns the queries and targets of every open attempt.
func (l *MemoryLive) Queries() ([]logs.Target, []logs.LiveQuery) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]logs.Target(nil), l.targets...), append([]logs.LiveQuery(nil), l.queries...)
}

// OpenStreams is the number of streams currently open.
func (l *MemoryLive) OpenStreams() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.open
}

// Open implements logs.LiveReader.
func (l *MemoryLive) Open(_ context.Context, t logs.Target, q logs.LiveQuery) (io.ReadCloser, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.targets, l.queries = append(l.targets, t), append(l.queries, q)
	if l.err != nil {
		return nil, l.err
	}
	job, ok := l.jobs[t.JobName]
	switch {
	case !ok || t.Namespace != Namespace:
		return nil, logs.ErrWorkloadMissing
	case job.uid != t.JobUID || t.Container != Container:
		return nil, logs.ErrWorkloadMismatch
	}
	body := job.body
	if q.TailLines > 0 {
		lines := strings.SplitAfter(body, "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if int64(len(lines)) > q.TailLines {
			lines = lines[int64(len(lines))-q.TailLines:]
		}
		body = strings.Join(lines, "")
	}
	if q.LimitBytes > 0 && int64(len(body)) > q.LimitBytes {
		body = body[:q.LimitBytes]
	}
	l.open++
	return &memStream{Reader: strings.NewReader(body), l: l}, nil
}

type memStream struct {
	io.Reader
	l    *MemoryLive
	once sync.Once
}

func (s *memStream) Close() error {
	s.once.Do(func() {
		s.l.mu.Lock()
		s.l.open--
		s.l.mu.Unlock()
	})
	return nil
}

// ---- store ----

// Store implements logs.Store over memdb: tenant-scoped records with fenced
// claims, verified linkage that never changes, a bounded generation chain and
// one audit entry per change written in the same transaction.
func (m *Memory) Store() logs.Store { return (*memStore)(m) }

type memStore Memory

var _ logs.Store = (*memStore)(nil)

func sameProject(r *logs.Record, scope domain.Scope) bool { return r.Scope == scope }

// attempt requires the attempt to exist in the record's scope and run, as the
// composite foreign key does in PostgreSQL.
func (s *memStore) attempt(tx memdb.Tx, scope domain.Scope, run domain.RunID, id domain.AttemptID) error {
	row, ok := attemptsTbl.Get(tx, string(id))
	if !ok || row.Info.Scope != scope || row.Info.Run != run {
		return logs.ErrNotFound
	}
	return nil
}

func (s *memStore) latest(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID, src logs.Source) *logs.Record {
	var best *logs.Record
	recordsTbl.Scan(tx, func(r recordRow) bool {
		if r.R.Scope == scope && r.R.Attempt == attempt && r.R.Source == src && (best == nil || r.R.Generation > best.Generation) {
			best = r.R
		}
		return true
	})
	return best
}

// insert stores a new capture's record. Put replaces a row with the same
// primary key, so insertion refuses an identifier that already names any
// record of the project: PostgreSQL's primary key answers a collision with a
// conflict, and an identifier reused by another capture must never overwrite
// its retained evidence.
func (s *memStore) insert(tx memdb.Tx, r *logs.Record, a logs.Audit, at time.Time) error {
	if _, taken := recordsTbl.Get(tx, string(r.Scope.ProjectID)+"/"+string(r.ID)); taken {
		return fmt.Errorf("%w: the record identifier is already in use", logs.ErrConflict)
	}
	return s.put(tx, r, a, at)
}

// put replaces an existing record or stores a new one, with its audit entry.
func (s *memStore) put(tx memdb.Tx, r *logs.Record, a logs.Audit, at time.Time) error {
	if err := recordsTbl.Put(tx, recordRow{R: cloneRecord(r)}); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return fmt.Errorf("%w: capture exists", logs.ErrConflict)
		}
		return err
	}
	meta := map[string]any{"attemptId": string(r.Attempt), "source": string(r.Source), "generation": r.Generation, "state": string(r.State)}
	if r.Failure != "" {
		meta["failure"] = string(r.Failure)
	}
	if r.Link != nil {
		meta["completeness"] = string(r.Link.Completeness)
	}
	n := 0
	auditTbl.Scan(tx, func(auditRow) bool { n++; return true })
	return auditTbl.Put(tx, auditRow{ID: fmt.Sprintf("aud_%06d", n+1), Project: r.Scope.ProjectID, Record: r.ID, Action: a.Action, Request: a.RequestID, Metadata: meta})
}

func (s *memStore) Ensure(ctx context.Context, tmpl logs.Record) (*logs.Record, error) {
	var out *logs.Record
	err := s.db.Update(ctx, tmpl.Scope.TenantID, func(tx memdb.Tx) error {
		if err := s.attempt(tx, tmpl.Scope, tmpl.Run, tmpl.Attempt); err != nil {
			return err
		}
		if cur := s.latest(tx, tmpl.Scope, tmpl.Attempt, tmpl.Source); cur != nil {
			out = cloneRecord(cur)
			return nil
		}
		fresh, err := logs.NewRecord(tmpl.Scope, tmpl.ID, tmpl.Attempt, tmpl.Run, tmpl.Step, tmpl.Source, 1, tmpl.CreatedAt)
		if err != nil {
			return err
		}
		if err := s.insert(tx, fresh, logs.Audit{Action: "log.created"}, tmpl.CreatedAt); err != nil {
			return err
		}
		out = cloneRecord(fresh)
		return nil
	})
	return out, err
}

func (s *memStore) Get(ctx context.Context, scope domain.Scope, id logs.RecordID) (*logs.Record, error) {
	if scope.Validate() != nil {
		return nil, logs.ErrNotFound
	}
	var out *logs.Record
	err := s.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		row, ok := recordsTbl.Get(tx, string(scope.ProjectID)+"/"+string(id))
		if !ok || !sameProject(row.R, scope) {
			return logs.ErrNotFound
		}
		out = cloneRecord(row.R)
		return nil
	})
	return out, err
}

func (s *memStore) ForAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([]*logs.Record, error) {
	if scope.Validate() != nil {
		return nil, nil
	}
	var out []*logs.Record
	err := s.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		recordsTbl.Scan(tx, func(r recordRow) bool {
			if r.R.Scope == scope && r.R.Attempt == attempt {
				out = append(out, cloneRecord(r.R))
			}
			return true
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Generation < out[j].Generation
	})
	return out, err
}

func (s *memStore) Create(ctx context.Context, r *logs.Record) (*logs.Record, error) {
	var out *logs.Record
	err := s.db.Update(ctx, r.Scope.TenantID, func(tx memdb.Tx) error {
		if err := s.attempt(tx, r.Scope, r.Run, r.Attempt); err != nil {
			return err
		}
		var existing, previous *logs.Record
		recordsTbl.Scan(tx, func(row recordRow) bool {
			if row.R.Scope == r.Scope && row.R.Attempt == r.Attempt && row.R.Source == r.Source {
				if row.R.Generation == r.Generation {
					existing = row.R
				}
				if row.R.Generation == r.Generation-1 {
					previous = row.R
				}
			}
			return true
		})
		if existing != nil {
			out = cloneRecord(existing)
			return nil
		}
		// A generation above 1 follows lost frozen bytes only.
		if r.Generation > 1 && (previous == nil || previous.State != logs.StateUnavailable || previous.Failure != logs.FailureFrozenLost) {
			return fmt.Errorf("%w: a new capture generation follows lost frozen bytes only", logs.ErrConflict)
		}
		fresh, err := logs.NewRecord(r.Scope, r.ID, r.Attempt, r.Run, r.Step, r.Source, r.Generation, r.CreatedAt)
		if err != nil {
			return err
		}
		if err := s.insert(tx, fresh, logs.Audit{Action: "log.created"}, r.CreatedAt); err != nil {
			return err
		}
		out = cloneRecord(fresh)
		return nil
	})
	return out, err
}

func (s *memStore) Update(ctx context.Context, scope domain.Scope, id logs.RecordID, fn logs.Mutation) (*logs.Record, error) {
	if scope.Validate() != nil {
		return nil, logs.ErrNotFound
	}
	var out *logs.Record
	err := s.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		row, ok := recordsTbl.Get(tx, string(scope.ProjectID)+"/"+string(id))
		if !ok || !sameProject(row.R, scope) {
			return logs.ErrNotFound
		}
		r := cloneRecord(row.R)
		before := r.Version
		changed, a, err := fn(r)
		if err != nil {
			return err
		}
		out = r
		if !changed || r.Version == before {
			return nil
		}
		return s.put(tx, r, a, r.UpdatedAt)
	})
	if err != nil {
		return nil, err
	}
	return cloneRecord(out), nil
}

// AuditEntry is the safe, content-free audit of one record change.
type AuditEntry struct {
	Action    string
	RequestID string
	Metadata  map[string]any
}

// Audit lists the record's audit entries in order.
func (m *Memory) Audit(tb testing.TB, scope domain.Scope, id logs.RecordID) []AuditEntry {
	tb.Helper()
	var out []AuditEntry
	err := m.db.View(context.Background(), scope.TenantID, func(tx memdb.Tx) error {
		var rows []auditRow
		auditTbl.Scan(tx, func(r auditRow) bool {
			if r.Project == scope.ProjectID && r.Record == id {
				rows = append(rows, r)
			}
			return true
		})
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
		for _, r := range rows {
			out = append(out, AuditEntry{Action: r.Action, RequestID: r.Request, Metadata: r.Metadata})
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return out
}

// ---- retained logs ----

// Retained implements logs.Retained over in-memory diagnostic artifacts: the
// read side validates every artifact as a scoped, same-attempt, usable
// diagnostic file and binds a sidecar to its log by ID, digest and size; the
// writer retains one frozen controller capture idempotently.
func (m *Memory) Retained() logs.Retained { return (*memRetained)(m) }

type memRetained Memory

var _ logs.Retained = (*memRetained)(nil)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// Upload stores a usable diagnostic file of the attempt, as the wrapper's
// runtime upload does, and returns its artifact ID.
func (m *Memory) Upload(tb testing.TB, a MemoryAttempt, name, mediaType string, data []byte) domain.ArtifactID {
	tb.Helper()
	id, err := m.put(context.Background(), a.Scope, a.ID, name, mediaType, data)
	if err != nil {
		tb.Fatalf("upload %s: %v", name, err)
	}
	return id
}

// put creates one usable diagnostic artifact. A repeat of the same name is the
// same artifact when the content is the same and a conflict otherwise.
func (m *Memory) put(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, name, media string, data []byte) (domain.ArtifactID, error) {
	var id domain.ArtifactID
	err := m.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		found := false
		objectsTbl.Scan(tx, func(o objectRow) bool {
			if o.Scope == scope && o.Attempt == attempt && o.Name == name {
				found, id = true, o.ID
				if !bytes.Equal(o.Data, data) || o.Media != media {
					id = ""
				}
				return false
			}
			return true
		})
		if found {
			if id == "" {
				return logs.ErrConflict
			}
			return nil
		}
		id = domain.ArtifactID(fmt.Sprintf("art_%d", m.seq.Add(1)))
		return objectsTbl.Put(tx, objectRow{ID: id, Scope: scope, Attempt: attempt, Name: name, Media: media, Data: append([]byte(nil), data...), Usable: true})
	})
	return id, err
}

// WrapperLog uploads the wrapper's command log and, when c is non-empty, a
// sidecar for it stating completeness c with reasons.
func (m *Memory) WrapperLog(tb testing.TB, a MemoryAttempt, text string, c contract.Completeness, reasons ...contract.Reason) domain.ArtifactID {
	tb.Helper()
	data := []byte(text)
	id := m.Upload(tb, a, contract.CommandLogName, contract.LogMediaType, data)
	if c == "" {
		return id
	}
	doc := contract.Metadata{Version: contract.Version, Source: contract.SourceWrapper, LogName: contract.CommandLogName,
		Log: contract.Ref{ArtifactID: id, Digest: sum(data), SizeBytes: int64(len(data))}, Completeness: c, Reasons: reasons}
	b, err := contract.Encode(doc)
	if err != nil {
		tb.Fatal(err)
	}
	name, _ := contract.MetadataName(contract.CommandLogName)
	m.Upload(tb, a, name, contract.MediaType, b)
	return id
}

// RemoveArtifact applies retention to the artifact: its bytes are gone.
func (m *Memory) RemoveArtifact(tb testing.TB, a MemoryAttempt, id domain.ArtifactID) {
	tb.Helper()
	err := m.db.Update(context.Background(), a.Scope.TenantID, func(tx memdb.Tx) error {
		o, ok := objectsTbl.Get(tx, string(id))
		if !ok || o.Scope != a.Scope {
			return logs.ErrNotFound
		}
		o.Removed, o.Usable, o.Data = true, false, nil
		return objectsTbl.Put(tx, o)
	})
	if err != nil {
		tb.Fatal(err)
	}
}

// LoseStagedBytes forgets the frozen bytes already uploaded for captures, as a
// restart after a failed upload does.
func (m *Memory) LoseStagedBytes() {
	m.mu.Lock()
	m.staged = map[string][]byte{}
	m.mu.Unlock()
}

// FailNextStoreAfterUpload makes the next Retained.Store stage the frozen bytes
// and then fail with err, as an interrupted upload does: a retry without a
// body finds the uploaded bytes.
func (m *Memory) FailNextStoreAfterUpload(err error) {
	m.mu.Lock()
	m.store.afterErr = err
	m.mu.Unlock()
}

// FailNextStore makes the next Retained.Store fail with err before any effect.
func (m *Memory) FailNextStore(err error) {
	m.mu.Lock()
	m.store.err = err
	m.mu.Unlock()
}

// object returns the attempt's artifact when it is a scoped diagnostic file.
func (r *memRetained) object(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID, id domain.ArtifactID) (objectRow, error) {
	o, ok := objectsTbl.Get(tx, string(id))
	if !ok || o.Scope != scope || o.Attempt != attempt {
		return objectRow{}, logs.ErrNotFound
	}
	if !o.Usable || o.Removed {
		return objectRow{}, logs.ErrNoLog
	}
	return o, nil
}

func ref(o objectRow) logs.ArtifactRef {
	return logs.ArtifactRef{ID: o.ID, Digest: sum(o.Data), Size: int64(len(o.Data))}
}

func (r *memRetained) Open(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, want logs.ArtifactRef) (io.ReadCloser, error) {
	var data []byte
	err := r.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		o, err := r.object(tx, scope, attempt, want.ID)
		if err != nil {
			return err
		}
		if ref(o) != want {
			return logs.ErrNotFound // the record names different content than the artifact holds
		}
		data = o.Data
		return nil
	})
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (r *memRetained) Usable(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, want logs.ArtifactRef) (bool, error) {
	var ok bool
	err := r.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		o, found := objectsTbl.Get(tx, string(want.ID))
		if !found || o.Scope != scope || o.Attempt != attempt {
			return logs.ErrNotFound
		}
		ok = o.Usable && !o.Removed
		return nil
	})
	return ok, err
}

func (r *memRetained) byName(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID, name string) (objectRow, bool) {
	var out objectRow
	var found bool
	objectsTbl.Scan(tx, func(o objectRow) bool {
		if o.Scope == scope && o.Attempt == attempt && o.Name == name {
			out, found = o, true
			return false
		}
		return true
	})
	return out, found
}

// WrapperLog implements logs.Retained: the wrapper's command.log and, when it
// is valid and bound to that exact artifact, its sidecar. Legacy logs and logs
// with an unusable or mismatched sidecar have unknown completeness.
func (r *memRetained) WrapperLog(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (logs.Link, error) {
	var link logs.Link
	err := r.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		logObj, ok := r.byName(tx, scope, attempt, contract.CommandLogName)
		if !ok {
			return logs.ErrNoLog
		}
		if !logObj.Usable || logObj.Removed {
			return logs.ErrNoLog
		}
		link = logs.Link{Log: ref(logObj)}
		link.Completeness, link.Reasons = contract.Unverified()
		metaName, _ := contract.MetadataName(contract.CommandLogName)
		metaObj, ok := r.byName(tx, scope, attempt, metaName)
		if !ok || !metaObj.Usable || metaObj.Removed || metaObj.Media != contract.MediaType {
			return nil
		}
		doc, err := contract.Decode(metaObj.Data)
		if err != nil || doc.Source != contract.SourceWrapper || doc.LogName != contract.CommandLogName ||
			doc.Bind(logObj.ID, sum(logObj.Data), int64(len(logObj.Data))) != nil {
			return nil // a sidecar is never trusted for an artifact it does not describe
		}
		meta := ref(metaObj)
		link.Meta, link.Completeness, link.Reasons = &meta, doc.Completeness, append([]logs.Reason(nil), doc.Reasons...)
		return nil
	})
	return link, err
}

func stagedKey(scope domain.Scope, attempt domain.AttemptID, name string) string {
	return string(scope.TenantID) + "/" + string(scope.ProjectID) + "/" + string(attempt) + "/" + name
}

// Store implements logs.Retained for one frozen controller capture. Repeating
// it with the same snapshot returns the same link; with a nil body the bytes
// must already have been uploaded.
func (r *memRetained) Store(ctx context.Context, info logs.AttemptInfo, snap logs.Snapshot, body io.ReadSeeker) (logs.Link, error) {
	m := (*Memory)(r)
	if info.Scope.Validate() != nil || info.ID.Validate() != nil || info.Run.Validate() != nil || info.Step == "" {
		return logs.Link{}, logs.ErrInvalid
	}
	wantMeta, err := contract.MetadataName(snap.LogName)
	if err != nil || wantMeta != snap.MetaName || !strings.HasPrefix(snap.LogName, "controller-g") {
		return logs.Link{}, fmt.Errorf("%w: not a controller capture name", logs.ErrInvalid)
	}
	m.mu.Lock()
	injected := m.store.err
	m.store.err = nil
	m.mu.Unlock()
	if injected != nil {
		return logs.Link{}, injected
	}

	key := stagedKey(info.Scope, info.ID, snap.LogName)
	var data []byte
	m.mu.Lock()
	data, staged := m.staged[key]
	m.mu.Unlock()
	if !staged {
		if body == nil {
			return logs.Link{}, logs.ErrFrozenBytesLost
		}
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return logs.Link{}, logs.Unavailablef(err, "read capture")
		}
		if data, err = io.ReadAll(body); err != nil {
			return logs.Link{}, logs.Unavailablef(err, "read capture")
		}
		m.mu.Lock()
		m.staged[key] = append([]byte(nil), data...)
		m.mu.Unlock()
	}
	if int64(len(data)) != snap.Size || sum(data) != snap.Digest {
		return logs.Link{}, logs.ErrFrozenBytesLost // the staged bytes are not the frozen ones
	}
	m.mu.Lock()
	after := m.store.afterErr
	m.store.afterErr = nil
	m.mu.Unlock()
	if after != nil {
		return logs.Link{}, after
	}
	logID, err := m.put(ctx, info.Scope, info.ID, snap.LogName, contract.LogMediaType, data)
	if err != nil {
		return logs.Link{}, err
	}
	doc := contract.Metadata{Version: contract.Version, Source: contract.SourceController, LogName: snap.LogName,
		Log:          contract.Ref{ArtifactID: logID, Digest: snap.Digest, SizeBytes: snap.Size},
		Completeness: snap.Completeness, Reasons: snap.Reasons}
	sidecar, err := contract.Encode(doc)
	if err != nil {
		return logs.Link{}, fmt.Errorf("%w: %v", logs.ErrInvalid, err)
	}
	metaID, err := m.put(ctx, info.Scope, info.ID, snap.MetaName, contract.MediaType, sidecar)
	if err != nil {
		return logs.Link{}, err
	}
	meta := logs.ArtifactRef{ID: metaID, Digest: sum(sidecar), Size: int64(len(sidecar))}
	return logs.Link{Log: logs.ArtifactRef{ID: logID, Digest: snap.Digest, Size: snap.Size}, Meta: &meta, Statement: snap.Statement}, nil
}

// ---- assembled read service and controller ----

// Service builds the read service over the memory ports.
func (m *Memory) Service(tb testing.TB, lim logs.Limits) *logs.Service {
	tb.Helper()
	svc, err := logs.NewService(logs.Config{Authority: m.Authority, Attempts: m.Attempts(), Workloads: m.Workloads(), Live: m.Live, Retained: m.Retained(),
		Store: m.Store(), Redactions: m.Redact, Limits: lim, Clock: m.Clock.Now})
	if err != nil {
		tb.Fatal(err)
	}
	return svc
}

// Controller builds the fallback capture over the memory ports.
func (m *Memory) Controller(tb testing.TB, captureBytes int64) *logs.Controller {
	tb.Helper()
	if captureBytes == 0 {
		captureBytes = 1 << 20
	}
	c, err := logs.NewController(logs.ControllerConfig{Attempts: m.Attempts(), Workloads: m.Workloads(), Live: m.Live, Retained: m.Retained(), Store: m.Store(),
		Redactions: m.Redact, Clock: m.Clock.Now, CaptureBytes: captureBytes, CaptureTimeout: 20 * time.Second, Lease: 30 * time.Second, SpoolDir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}
	return c
}
