package eventstest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/memdb"
)

// MemOptions are the explicit policies of the in-memory event bus. They mirror
// the PostgreSQL adapter's options that the memory side models; none has a
// default (use DefaultMemOptions in tests).
type MemOptions struct {
	Backlog events.BacklogLimits
	Retry   events.RetryPolicy
	// Retention is validated and its MaxClockSkew bounds future events. Nothing
	// is pruned in memory.
	Retention events.RetentionPolicy
	// Authorizer decides replay; nil refuses every replay.
	Authorizer events.ReplayAuthorizer
	Clock      func() time.Time
}

// DefaultMemOptions are short, explicit test policies, the same values as
// DefaultOptions uses for PostgreSQL.
func DefaultMemOptions() MemOptions {
	o := DefaultOptions()
	return MemOptions{Backlog: o.Backlog, Retry: o.Retry, Retention: o.Retention}
}

// MemHandler is the local effect of a consumer in memory. It runs over the
// consumer's own memdb.Tx, the counterpart of postgres.TxHandler's pgx.Tx. An
// error discards every effect it wrote; the inbox outcome is still recorded.
type MemHandler func(ctx context.Context, tx memdb.Tx, env eventsv1.Envelope) error

// MemBinding attaches a local effect to a consumer identity.
type MemBinding struct {
	Spec   events.ConsumerSpec
	Handle MemHandler
}

// AppendResult describes the committed (or already committed) event.
type AppendResult struct {
	Scope events.Scope
	// Sequence is the event's observation sequence within its scope.
	Sequence int64
	Digest   string
	// Duplicate is true when the same event ID with identical content already
	// existed; nothing new was written.
	Duplicate bool
}

type outboxRec struct {
	Pub           events.Publication
	ProjectID     string
	Type          string
	Payload       []byte
	Digest        string
	Created       time.Time
	LastFailureAt time.Time
}

type obsRec struct {
	ScopeKey string
	Seq      int64
	EventID  string
	Type     string
	Payload  []byte
	Digest   string
	Recorded time.Time
}

type scopeRec struct {
	Key     string
	LastSeq int64
}

type backlogRec struct {
	Count int
	Bytes int64
}

type subjectRec struct{ Token string }

type inboxRec struct {
	Entry   events.InboxEntry
	Digest  string
	Type    string
	Payload []byte
}

type quarantineRec struct {
	ID          string
	Consumer    string
	DeliveryKey string
	EventID     string
	Reason      string
	Diagnostic  string
	State       string // "quarantined" or "resolved"
	Count       int
	ReplayBy    string
}

type replayRec struct {
	ID             string
	Request        events.ReplayRequest
	Classification events.ReplayClassification
	State          string
	NextSeq        int64
	Processed      int
	Skipped        int
	Failed         int
	CreatedAt      time.Time
	CompletedAt    time.Time
}

type effectRec struct{ Key string }

var (
	outboxT = memdb.NewTable("events_outbox", func(r outboxRec) string { return r.Pub.EventID })
	obsT    = memdb.NewTable("events_observations",
		func(r obsRec) string { return fmt.Sprintf("%s/%020d", r.ScopeKey, r.Seq) },
		func(r obsRec) string { return r.EventID })
	scopesT     = memdb.NewTable("events_scopes", func(r scopeRec) string { return "s/" + r.Key })
	backlogT    = memdb.NewTable("events_backlog", func(backlogRec) string { return "backlog" })
	subjectT    = memdb.NewTable("events_subject", func(subjectRec) string { return "subject" })
	inboxT      = memdb.NewTable("events_inbox", func(r inboxRec) string { return r.Entry.Consumer + "/" + r.Entry.EventID })
	quarantineT = memdb.NewTable("events_quarantine", func(r quarantineRec) string { return r.Consumer + "/" + r.DeliveryKey })
	replayT     = memdb.NewTable("events_replays", func(r replayRec) string { return r.ID })
	effectsT    = memdb.NewTable("events_effects", func(r effectRec) string { return r.Key })
)

// Mem is the in-memory event bus over memdb: the transaction-local append, the
// outbox publication store, the inbox processor with quarantine, and replay.
// Every write of one operation commits in one memdb.Update, so a producer's
// state and its event land together or not at all.
//
// Mem is development-only: it implements events.PublicationStore,
// events.StatsReader, events.Processor, events.TenantSource and
// events.SubjectResolver with the observable outcomes of the PostgreSQL
// adapter and none of its locking, retention or SQL. See MEMORY.md.
type Mem struct {
	store     *memdb.Store
	opts      MemOptions
	consumers map[string]MemBinding

	mu      sync.Mutex
	tenants map[domain.TenantID]struct{}
	tokens  map[string]domain.TenantID
	// running lists the replays a live caller is executing, so overlapping
	// ranges are refused only while someone is actually running them. An
	// interrupted replay leaves nothing behind that blocks its retry.
	running []runningReplay
}

type runningReplay struct {
	consumer string
	scope    events.Scope
	from, to int64
}

var (
	_ events.PublicationStore = (*Mem)(nil)
	_ events.StatsReader      = (*Mem)(nil)
	_ events.TenantSource     = (*Mem)(nil)
	_ events.SubjectResolver  = (*Mem)(nil)
	_ events.Processor        = (*Mem)(nil)
)

// NewMem validates the policies and consumer bindings.
func NewMem(store *memdb.Store, opts MemOptions, bindings ...MemBinding) (*Mem, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: a memdb store is required", events.ErrInvalidConfiguration)
	}
	if err := opts.Backlog.Validate(); err != nil {
		return nil, err
	}
	if err := opts.Retry.Validate(); err != nil {
		return nil, err
	}
	if err := opts.Retention.Validate(); err != nil {
		return nil, err
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	m := &Mem{store: store, opts: opts, consumers: map[string]MemBinding{},
		tenants: map[domain.TenantID]struct{}{}, tokens: map[string]domain.TenantID{}}
	for _, b := range bindings {
		if err := b.Spec.Validate(); err != nil {
			return nil, err
		}
		if b.Handle == nil {
			return nil, fmt.Errorf("%w: consumer %s has no handler", events.ErrInvalidConfiguration, b.Spec.Name)
		}
		if _, dup := m.consumers[b.Spec.Name]; dup {
			return nil, fmt.Errorf("%w: consumer %s registered twice", events.ErrInvalidConfiguration, b.Spec.Name)
		}
		m.consumers[b.Spec.Name] = b
	}
	return m, nil
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*Mem) DevelopmentOnly() bool { return true }

func (m *Mem) now() time.Time { return m.opts.Clock().UTC() }

// Store returns the kernel the bus writes through, for peers that join it.
func (m *Mem) Store() *memdb.Store { return m.store }

// AddTenant makes the tenant known to TenantIDs before it has any event.
func (m *Mem) AddTenant(tenant domain.TenantID) {
	m.mu.Lock()
	m.tenants[tenant] = struct{}{}
	m.mu.Unlock()
}

// TenantIDs lists the tenants that were added or appended to, in order.
func (m *Mem) TenantIDs(context.Context) ([]domain.TenantID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.TenantID, 0, len(m.tenants))
	for t := range m.tenants {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (m *Mem) update(ctx context.Context, tenant domain.TenantID, fn func(memdb.Tx) error) error {
	return m.store.Update(ctx, tenant, fn)
}

// ---- append ------------------------------------------------------------

// Append validates env and joins the caller's memdb.Tx to record, atomically
// with the caller's own writes, the immutable outbox row, the retained
// observation with its scope sequence and the tenant backlog accounting. It
// never opens an Update of its own. A repeated event ID with identical content
// returns the existing result; with different content it fails with
// events.ErrConflict. A full backlog fails with events.ErrBacklogFull before
// anything is written, leaving tx usable.
func (m *Mem) Append(tx memdb.Tx, env eventsv1.Envelope) (AppendResult, error) {
	payload, err := eventsv1.Encode(env)
	if err != nil {
		return AppendResult{}, err
	}
	digest, err := eventsv1.Digest(env)
	if err != nil {
		return AppendResult{}, err
	}
	scope := events.ScopeOf(env)
	res := AppendResult{Scope: scope, Digest: digest}
	if tx.Tenant() != env.TenantID {
		return res, fmt.Errorf("%w: transaction tenant differs from the event's tenant", events.ErrScopeMismatch)
	}
	if limit := m.now().Add(m.opts.Retention.MaxClockSkew); env.OccurredAt.After(limit) {
		return res, fmt.Errorf("%w: event %s occurred at %s, later than %s", events.ErrFutureEvent, env.ID,
			env.OccurredAt.UTC().Format(time.RFC3339Nano), limit.UTC().Format(time.RFC3339Nano))
	}
	if existing, seq, found := lookupExisting(tx, env.ID); found {
		if existing != digest {
			return res, fmt.Errorf("%w: event %s", events.ErrConflict, env.ID)
		}
		res.Duplicate, res.Sequence = true, seq
		return res, nil
	}
	backlog, _ := backlogT.Get(tx, "backlog")
	size := int64(len(payload))
	if backlog.Count+1 > m.opts.Backlog.MaxEvents || backlog.Bytes+size > m.opts.Backlog.MaxBytes {
		return res, fmt.Errorf("%w: %d events, %d bytes pending", events.ErrBacklogFull, backlog.Count, backlog.Bytes)
	}
	backlog.Count++
	backlog.Bytes += size
	if err := backlogT.Put(tx, backlog); err != nil {
		return res, err
	}
	if _, err := m.ensureSubject(tx); err != nil {
		return res, err
	}
	sc, _ := scopesT.Get(tx, "s/"+scope.Key())
	sc.Key = scope.Key()
	sc.LastSeq++
	if err := scopesT.Put(tx, sc); err != nil {
		return res, err
	}
	now := m.now()
	if err := obsT.Put(tx, obsRec{ScopeKey: scope.Key(), Seq: sc.LastSeq, EventID: env.ID, Type: env.Type,
		Payload: bytes.Clone(payload), Digest: digest, Recorded: now}); err != nil {
		return res, err
	}
	if err := outboxT.Put(tx, outboxRec{
		Pub:       events.Publication{TenantID: string(env.TenantID), EventID: env.ID, State: events.PublicationPending, NextAttemptAt: now},
		ProjectID: string(env.ProjectID), Type: env.Type, Payload: bytes.Clone(payload), Digest: digest, Created: now,
	}); err != nil {
		return res, err
	}
	m.AddTenant(env.TenantID)
	res.Sequence = sc.LastSeq
	return res, nil
}

// lookupExisting reports whether any retained record knows the event ID: the
// observation, the outbox row, or an inbox entry.
func lookupExisting(tx memdb.Tx, eventID string) (digest string, seq int64, found bool) {
	obsT.Scan(tx, func(r obsRec) bool {
		if r.EventID == eventID {
			digest, seq, found = r.Digest, r.Seq, true
			return false
		}
		return true
	})
	if found {
		return
	}
	if r, ok := outboxT.Get(tx, eventID); ok {
		return r.Digest, 0, true
	}
	inboxT.Scan(tx, func(r inboxRec) bool {
		if r.Entry.EventID == eventID {
			digest, found = r.Digest, true
			return false
		}
		return true
	})
	return
}

func (m *Mem) ensureSubject(tx memdb.Tx) (string, error) {
	if r, ok := subjectT.Get(tx, "subject"); ok {
		return r.Token, nil
	}
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("events: subject token: %w", err)
	}
	token := hex.EncodeToString(raw[:])
	if err := subjectT.Put(tx, subjectRec{Token: token}); err != nil {
		return "", err
	}
	m.mu.Lock()
	m.tokens[token] = tx.Tenant()
	m.mu.Unlock()
	return token, nil
}

// SubjectToken returns the tenant's server-assigned subject token, assigning one
// if needed.
func (m *Mem) SubjectToken(ctx context.Context, tenant domain.TenantID) (string, error) {
	var token string
	err := m.update(ctx, tenant, func(tx memdb.Tx) (err error) {
		token, err = m.ensureSubject(tx)
		return err
	})
	return token, err
}

// ResolveSubjectToken maps a subject token to its tenant. An unknown or
// malformed token, or one whose assignment never committed, is
// events.ErrUnknownSubject.
func (m *Mem) ResolveSubjectToken(ctx context.Context, token string) (domain.TenantID, error) {
	if err := events.ValidateSubjectToken(token); err != nil {
		return "", fmt.Errorf("%w: %v", events.ErrUnknownSubject, err)
	}
	m.mu.Lock()
	tenant, ok := m.tokens[token]
	m.mu.Unlock()
	if !ok {
		return "", events.ErrUnknownSubject
	}
	committed := false
	if err := m.store.View(ctx, tenant, func(tx memdb.Tx) error {
		r, found := subjectT.Get(tx, "subject")
		committed = found && r.Token == token
		return nil
	}); err != nil {
		return "", err
	}
	if !committed {
		return "", events.ErrUnknownSubject
	}
	return tenant, nil
}

// Watermark is the scope's committed high-water sequence in the caller's Tx.
func (m *Mem) Watermark(tx memdb.Tx, scope events.Scope) (int64, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if tx.Tenant() != scope.TenantID {
		return 0, fmt.Errorf("%w: transaction tenant differs from the scope's tenant", events.ErrScopeMismatch)
	}
	sc, _ := scopesT.Get(tx, "s/"+scope.Key())
	return sc.LastSeq, nil
}

// Observation is one retained event with its scope sequence.
type Observation struct {
	Sequence int64
	Envelope eventsv1.Envelope
	Payload  []byte
}

// ReadObservations returns up to limit observations of the scope with sequence
// greater than after, in order, in the caller's Tx.
func (m *Mem) ReadObservations(tx memdb.Tx, scope events.Scope, after int64, limit int) ([]Observation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if tx.Tenant() != scope.TenantID {
		return nil, fmt.Errorf("%w: transaction tenant differs from the scope's tenant", events.ErrScopeMismatch)
	}
	if after < 0 || limit < 1 {
		return nil, fmt.Errorf("%w: after must not be negative and limit must be positive", events.ErrInvalidConfiguration)
	}
	var out []Observation
	obsT.Scan(tx, func(r obsRec) bool {
		if r.ScopeKey != scope.Key() || r.Seq <= after {
			return true
		}
		env, err := eventsv1.Decode(r.Payload)
		if err != nil {
			return true
		}
		out = append(out, Observation{Sequence: r.Seq, Envelope: env, Payload: bytes.Clone(r.Payload)})
		return len(out) < limit
	})
	return out, nil
}

// ---- publication ---------------------------------------------------------

// ClaimDue leases up to limit due outbox rows of one tenant under new fencing
// tokens (events.PublicationStore).
func (m *Mem) ClaimDue(ctx context.Context, tenant domain.TenantID, now time.Time, lease time.Duration, limit int) ([]events.Claim, error) {
	if limit < 1 {
		return nil, fmt.Errorf("%w: claim limit must be positive", events.ErrInvalidConfiguration)
	}
	var claims []events.Claim
	err := m.update(ctx, tenant, func(tx memdb.Tx) error {
		claims = claims[:0]
		var due []outboxRec
		outboxT.Scan(tx, func(r outboxRec) bool {
			if r.Pub.Due(now) {
				due = append(due, r)
			}
			return true
		})
		if len(due) == 0 {
			return nil
		}
		sort.Slice(due, func(i, j int) bool {
			a, b := due[i].Pub, due[j].Pub
			if !a.NextAttemptAt.Equal(b.NextAttemptAt) {
				return a.NextAttemptAt.Before(b.NextAttemptAt)
			}
			return a.EventID < b.EventID
		})
		if len(due) > limit {
			due = due[:limit]
		}
		token, err := m.ensureSubject(tx)
		if err != nil {
			return err
		}
		for _, r := range due {
			t, err := r.Pub.Claim(now, lease)
			if err != nil {
				return err
			}
			if err := outboxT.Put(tx, r); err != nil {
				return err
			}
			claims = append(claims, events.Claim{TenantID: tenant, EventID: r.Pub.EventID, EventType: r.Type, SubjectToken: token,
				Payload: bytes.Clone(r.Payload), Token: t, Attempt: r.Pub.Attempts, LeaseExpiresAt: r.Pub.LeaseExpiresAt})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func loadClaimed(tx memdb.Tx, c events.Claim) (outboxRec, error) {
	r, ok := outboxT.Get(tx, c.EventID)
	if !ok {
		return r, fmt.Errorf("%w: outbox event %s", events.ErrNotFound, c.EventID)
	}
	return r, nil
}

// Complete marks the event published iff c.Token is still the row's current
// fencing token, and releases its backlog accounting.
func (m *Mem) Complete(ctx context.Context, c events.Claim, now time.Time) error {
	return m.update(ctx, c.TenantID, func(tx memdb.Tx) error {
		r, err := loadClaimed(tx, c)
		if err != nil {
			return err
		}
		if err := r.Pub.Complete(c.Token, now); err != nil {
			return err
		}
		if err := outboxT.Put(tx, r); err != nil {
			return err
		}
		b, _ := backlogT.Get(tx, "backlog")
		b.Count--
		b.Bytes -= int64(len(r.Payload))
		return backlogT.Put(tx, b)
	})
}

// Fail records a classified failure and schedules the retry iff c.Token is the
// current fencing token.
func (m *Mem) Fail(ctx context.Context, c events.Claim, class events.FailureClass, message string, now time.Time) error {
	return m.update(ctx, c.TenantID, func(tx memdb.Tx) error {
		r, err := loadClaimed(tx, c)
		if err != nil {
			return err
		}
		if err := r.Pub.Fail(c.Token, class, message, now, m.opts.Retry); err != nil {
			return err
		}
		r.LastFailureAt = now
		return outboxT.Put(tx, r)
	})
}

// PublicationStats reports one tenant's unpublished backlog without payloads.
func (m *Mem) PublicationStats(ctx context.Context, tenant domain.TenantID, now time.Time) (events.PublicationStats, error) {
	var st events.PublicationStats
	err := m.store.View(ctx, tenant, func(tx memdb.Tx) error {
		var oldest time.Time
		var last outboxRec
		outboxT.Scan(tx, func(r outboxRec) bool {
			if r.Pub.State == events.PublicationPublished {
				return true
			}
			if r.Pub.State == events.PublicationPending {
				st.Pending++
				if st.NextRetryAt.IsZero() || r.Pub.NextAttemptAt.Before(st.NextRetryAt) {
					st.NextRetryAt = r.Pub.NextAttemptAt
				}
			} else {
				st.Claimed++
			}
			st.BacklogBytes += int64(len(r.Payload))
			st.Attempts += int64(r.Pub.Attempts)
			if oldest.IsZero() || r.Created.Before(oldest) {
				oldest = r.Created
			}
			if !r.LastFailureAt.IsZero() && (last.LastFailureAt.IsZero() || r.LastFailureAt.After(last.LastFailureAt)) {
				last = r
			}
			return true
		})
		if !oldest.IsZero() {
			st.OldestUnpublishedAge = now.Sub(oldest)
		}
		if !last.LastFailureAt.IsZero() {
			st.LastClass, st.LastError, st.LastFailureAt = last.Pub.LastClass, last.Pub.LastError, last.LastFailureAt
		}
		return nil
	})
	return st, err
}

// ---- consumption ---------------------------------------------------------

// Quarantine reason codes recorded with a quarantined delivery.
const (
	ReasonUnsupportedVersion = "unsupported_version"
	ReasonUnsupportedType    = "unsupported_type"
	ReasonMalformed          = "malformed"
	ReasonTooLarge           = "too_large"
	ReasonScopeMismatch      = "scope_mismatch"
	ReasonPermanentFailure   = "permanent_failure"
	ReasonRetriesExhausted   = "retries_exhausted"
	ReasonConflictingContent = "conflicting_content"
)

var (
	errContentConflict = errors.New("events: event content differs from the content first delivered under the same ID")
	errHandlerFailed   = errors.New("events: handler failed")
)

type memReplay struct {
	grant events.ReplayGrant
	id    string
}

// Process turns one received delivery into a durable local outcome under the
// delivery's trusted tenant (events.Processor): decode against the contract,
// quarantine what cannot be processed, otherwise run the inbox so the handler
// and the inbox result commit together.
func (m *Mem) Process(ctx context.Context, d events.Delivery) (events.Outcome, error) {
	b, ok := m.consumers[d.Consumer]
	if !ok {
		return events.Outcome{}, fmt.Errorf("%w: consumer %q is not registered", events.ErrInvalidConfiguration, d.Consumer)
	}
	if err := d.Tenant.Validate(); err != nil {
		return events.Outcome{}, err
	}
	env, derr := eventsv1.Decode(d.Payload)
	if derr != nil {
		return m.quarantineRaw(ctx, d, reasonFor(derr), derr.Error(), false)
	}
	if env.TenantID != d.Tenant {
		return m.quarantineRaw(ctx, d, ReasonScopeMismatch, "envelope tenant differs from the trusted subscription scope", false)
	}
	if !b.Spec.Accepts(env.Type) {
		return events.Outcome{Kind: events.OutcomeCompleted, Reason: "filtered"}, nil
	}
	res, err := m.processEnvelope(ctx, d.Tenant, b, env, d.Payload, nil)
	if errors.Is(err, errContentConflict) {
		return m.quarantineRaw(ctx, d, ReasonConflictingContent, "event content differs from the content already recorded for this event ID", true)
	}
	return res.outcome, err
}

func reasonFor(err error) string {
	switch {
	case errors.Is(err, eventsv1.ErrUnsupportedVersion):
		return ReasonUnsupportedVersion
	case errors.Is(err, eventsv1.ErrUnsupportedType):
		return ReasonUnsupportedType
	case errors.Is(err, eventsv1.ErrTooLarge):
		return ReasonTooLarge
	default:
		return ReasonMalformed
	}
}

func (m *Mem) quarantineRaw(ctx context.Context, d events.Delivery, reason, diagnostic string, contentKeyed bool) (events.Outcome, error) {
	sum := sha256.Sum256(d.Payload)
	rec := quarantineRec{Consumer: d.Consumer, DeliveryKey: "raw:" + hex.EncodeToString(sum[:]),
		Reason: reason, Diagnostic: events.BoundedMessage(diagnostic), State: "quarantined"}
	var loose struct {
		ID string `json:"id"`
	}
	if len(d.Payload) <= eventsv1.MaxEnvelopeBytes && json.Unmarshal(d.Payload, &loose) == nil && domain.TenantID(loose.ID).Validate() == nil {
		rec.EventID = loose.ID
		if !contentKeyed {
			rec.DeliveryKey = loose.ID
		}
	}
	var id string
	err := m.update(ctx, d.Tenant, func(tx memdb.Tx) (err error) {
		id, err = putQuarantine(tx, rec, false)
		return err
	})
	if err != nil {
		return events.Outcome{}, err
	}
	return events.Outcome{Kind: events.OutcomeQuarantined, QuarantineID: id, Reason: reason}, nil
}

// putQuarantine records or refreshes one quarantine entry. A redelivery of the
// same poison keeps its identity; reopen starts a new cycle for a resolved one.
func putQuarantine(tx memdb.Tx, rec quarantineRec, reopen bool) (string, error) {
	key := rec.Consumer + "/" + rec.DeliveryKey
	if cur, ok := quarantineT.Get(tx, key); ok {
		cur.Count++
		if reopen || cur.State == "quarantined" {
			cur.Reason, cur.Diagnostic = rec.Reason, rec.Diagnostic
		}
		if reopen {
			cur.State, cur.ReplayBy = "quarantined", ""
		}
		return cur.ID, quarantineT.Put(tx, cur)
	}
	rec.ID = newID("q_")
	rec.Count = 1
	return rec.ID, quarantineT.Put(tx, rec)
}

func newID(prefix string) string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(raw[:])
}

type processResult struct {
	outcome events.Outcome
	skipped bool
}

// processEnvelope runs one decoded event through the inbox. The success path is
// one Update (inbox completion and the handler's effects together). A failing
// handler discards that Update; the failure is then recorded in a second one,
// which is the memory counterpart of PostgreSQL's savepoint (see MEMORY.md).
func (m *Mem) processEnvelope(ctx context.Context, tenant domain.TenantID, b MemBinding, env eventsv1.Envelope, raw []byte, replay *memReplay) (processResult, error) {
	var res processResult
	var handlerErr error
	var seenAttempts int
	err := m.update(ctx, tenant, func(tx memdb.Tx) error {
		res, handlerErr = processResult{}, nil
		var err error
		res, handlerErr, seenAttempts, err = m.processInTx(ctx, tx, tenant, b, env, raw, replay)
		if err != nil {
			return err
		}
		if handlerErr != nil {
			return errHandlerFailed // discards the handler's writes and the attempt
		}
		return nil
	})
	if errors.Is(err, errHandlerFailed) && handlerErr != nil {
		return m.recordFailure(ctx, tenant, b, env, raw, replay, handlerErr, seenAttempts)
	}
	if err != nil {
		return processResult{}, err
	}
	return res, nil
}

func inboxKey(consumer, eventID string) string { return consumer + "/" + eventID }

func (m *Mem) processInTx(ctx context.Context, tx memdb.Tx, tenant domain.TenantID, b MemBinding, env eventsv1.Envelope, raw []byte, replay *memReplay) (res processResult, handlerErr error, attempts int, err error) {
	consumer := b.Spec.Name
	digest, derr := eventsv1.Digest(env)
	if derr != nil {
		return res, nil, 0, fmt.Errorf("events: digest delivered event: %w", derr)
	}
	rec, exists := inboxT.Get(tx, inboxKey(consumer, env.ID))
	if exists && rec.Digest != digest {
		return res, nil, 0, errContentConflict
	}
	if !exists {
		rec = inboxRec{Entry: events.NewInboxEntry(string(tenant), consumer, env.ID), Digest: digest, Type: env.Type, Payload: bytes.Clone(raw)}
	}
	now := m.now()
	entry := rec.Entry
	if entry.State == events.InboxQuarantined && replay != nil {
		if err := entry.Reopen(replay.grant); err != nil {
			return res, nil, 0, err
		}
		rec.Entry = entry
		if err := inboxT.Put(tx, rec); err != nil {
			return res, nil, 0, err
		}
		if q, ok := quarantineT.Get(tx, consumer+"/"+env.ID); ok && q.State == "quarantined" {
			q.ReplayBy = replay.grant.Request().Operator.ID
			if err := quarantineT.Put(tx, q); err != nil {
				return res, nil, 0, err
			}
		}
	}
	if replay == nil && !entry.Due(now) {
		res.outcome = events.Outcome{Kind: events.OutcomeRetry, RetryAfter: entry.RetryIn(now), Reason: "retry not yet due"}
		return res, nil, 0, nil
	}
	switch berr := entry.Begin(); {
	case errors.Is(berr, events.ErrAlreadyCompleted):
		res.outcome, res.skipped = events.Outcome{Kind: events.OutcomeDuplicate}, true
		return res, nil, 0, nil
	case errors.Is(berr, events.ErrQuarantined):
		res.outcome = events.Outcome{Kind: events.OutcomeQuarantined, Reason: "already quarantined"}
		return res, nil, 0, nil
	case berr != nil:
		return res, nil, 0, berr
	}
	if herr := runHandler(ctx, tx, b.Handle, env); herr != nil {
		return res, herr, entry.Attempts, nil
	}
	completedAt := now
	if env.OccurredAt.After(completedAt) {
		completedAt = env.OccurredAt.UTC()
	}
	if origin := retainedOrigin(tx, env.ID); origin.After(completedAt) {
		completedAt = origin
	}
	if err := entry.Complete(completedAt); err != nil {
		return res, nil, 0, err
	}
	rec.Entry = entry
	rec.Payload = nil
	if err := inboxT.Put(tx, rec); err != nil {
		return res, nil, 0, err
	}
	if q, ok := quarantineT.Get(tx, consumer+"/"+env.ID); ok && q.State == "quarantined" {
		operator := q.ReplayBy
		if replay != nil {
			operator = replay.grant.Request().Operator.ID
		}
		if operator != "" {
			q.State = "resolved"
			if err := quarantineT.Put(tx, q); err != nil {
				return res, nil, 0, err
			}
		}
	}
	res.outcome = events.Outcome{Kind: events.OutcomeCompleted}
	return res, nil, 0, nil
}

func retainedOrigin(tx memdb.Tx, eventID string) time.Time {
	var origin time.Time
	if r, ok := outboxT.Get(tx, eventID); ok {
		origin = r.Created
	}
	obsT.Scan(tx, func(r obsRec) bool {
		if r.EventID == eventID {
			if r.Recorded.After(origin) {
				origin = r.Recorded
			}
			return false
		}
		return true
	})
	return origin
}

func runHandler(ctx context.Context, tx memdb.Tx, h MemHandler, env eventsv1.Envelope) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = events.Transient(fmt.Errorf("handler panic: %v", r))
		}
	}()
	return h(ctx, tx, env)
}

// recordFailure records a failed attempt durably in its own Update after the
// handler's writes were discarded.
func (m *Mem) recordFailure(ctx context.Context, tenant domain.TenantID, b MemBinding, env eventsv1.Envelope, raw []byte, replay *memReplay, herr error, seenAttempts int) (processResult, error) {
	var res processResult
	err := m.update(ctx, tenant, func(tx memdb.Tx) error {
		res = processResult{}
		consumer := b.Spec.Name
		digest, derr := eventsv1.Digest(env)
		if derr != nil {
			return derr
		}
		rec, exists := inboxT.Get(tx, inboxKey(consumer, env.ID))
		if !exists {
			rec = inboxRec{Entry: events.NewInboxEntry(string(tenant), consumer, env.ID), Digest: digest, Type: env.Type, Payload: bytes.Clone(raw)}
		}
		entry := rec.Entry
		if entry.State == events.InboxQuarantined && replay != nil {
			if err := entry.Reopen(replay.grant); err != nil {
				return err
			}
		}
		if err := entry.Begin(); err != nil {
			return err
		}
		if entry.Attempts != seenAttempts {
			// Another delivery advanced the entry between the two Updates; its
			// outcome stands and this attempt records nothing.
			res.outcome = events.Outcome{Kind: events.OutcomeRetry, Reason: "entry changed concurrently"}
			return nil
		}
		now := m.now()
		class := events.ClassOrTransient(herr)
		quarantined, err := entry.Fail(class, herr.Error(), now, m.opts.Retry)
		if err != nil {
			return err
		}
		rec.Entry = entry
		if err := inboxT.Put(tx, rec); err != nil {
			return err
		}
		if !quarantined {
			res.outcome = events.Outcome{Kind: events.OutcomeRetry, RetryAfter: m.opts.Retry.Delay(entry.Attempts), Reason: entry.LastError}
			return nil
		}
		reason := ReasonPermanentFailure
		if class == events.ClassTransient {
			reason = ReasonRetriesExhausted
		}
		qid, err := putQuarantine(tx, quarantineRec{Consumer: consumer, DeliveryKey: env.ID, EventID: env.ID, Reason: reason,
			Diagnostic: entry.LastError, State: "quarantined"}, true)
		if err != nil {
			return err
		}
		res.outcome = events.Outcome{Kind: events.OutcomeQuarantined, QuarantineID: qid, Reason: reason}
		return nil
	})
	if err != nil {
		return processResult{}, err
	}
	return res, nil
}

// QuarantineEntry is the operator-visible description of a quarantined
// delivery. It never includes the payload.
type QuarantineEntry struct {
	ID         string
	Consumer   string
	EventID    string
	Reason     string
	Diagnostic string
	Count      int
}

// ListQuarantine returns the unresolved quarantine entries of the tenant,
// optionally for one consumer, in key order.
func (m *Mem) ListQuarantine(ctx context.Context, tenant domain.TenantID, consumer string) ([]QuarantineEntry, error) {
	var out []QuarantineEntry
	err := m.store.View(ctx, tenant, func(tx memdb.Tx) error {
		quarantineT.Scan(tx, func(r quarantineRec) bool {
			if r.State == "quarantined" && (consumer == "" || r.Consumer == consumer) {
				out = append(out, QuarantineEntry{ID: r.ID, Consumer: r.Consumer, EventID: r.EventID, Reason: r.Reason, Diagnostic: r.Diagnostic, Count: r.Count})
			}
			return true
		})
		return nil
	})
	return out, err
}

// ---- replay --------------------------------------------------------------

func (m *Mem) classify(tx memdb.Tx, req events.ReplayRequest, b MemBinding) (events.ReplayClassification, error) {
	c := events.ReplayClassification{Effect: b.Spec.Effect}
	high, err := m.Watermark(tx, req.Scope)
	if err != nil {
		return c, err
	}
	if req.ToSeq > high {
		return c, fmt.Errorf("%w: range ends at %d but the scope's last event is %d", events.ErrReplayUnsafe, req.ToSeq, high)
	}
	obsT.Scan(tx, func(o obsRec) bool {
		if o.ScopeKey != req.Scope.Key() || o.Seq < req.FromSeq || o.Seq > req.ToSeq || !b.Spec.Accepts(o.Type) {
			return true
		}
		c.Events++
		in, ok := inboxT.Get(tx, inboxKey(b.Spec.Name, o.EventID))
		switch {
		case !ok:
			c.Undelivered++
		case in.Entry.State == events.InboxCompleted:
			c.AlreadyCompleted++
		case in.Entry.State == events.InboxQuarantined:
			c.Quarantined++
		default:
			c.Pending++
		}
		return true
	})
	return c, nil
}

// DryRun authorizes the operator and classifies what replaying the range for
// the consumer would do, without changing anything.
func (m *Mem) DryRun(ctx context.Context, req events.ReplayRequest) (events.ReplayClassification, error) {
	if _, err := events.Authorize(ctx, m.opts.Authorizer, req); err != nil {
		return events.ReplayClassification{}, err
	}
	b, ok := m.consumers[req.Consumer]
	if !ok {
		return events.ReplayClassification{}, fmt.Errorf("%w: consumer %q is not registered", events.ErrInvalidConfiguration, req.Consumer)
	}
	var c events.ReplayClassification
	err := m.store.View(ctx, req.Scope.TenantID, func(tx memdb.Tx) (err error) {
		c, err = m.classify(tx, req, b)
		return err
	})
	if err != nil {
		return c, err
	}
	return c, c.Check(b.Spec, req.AllowBeyondHorizon)
}

// ReplayResult is the outcome of a completed replay.
type ReplayResult struct {
	ID             string
	State          string
	Classification events.ReplayClassification
	Processed      int
	Skipped        int
	Failed         int
}

// Replay authorizes, classifies and, only if safe, runs a replay request to
// completion: events go through the same inbox transaction as live deliveries,
// completed ones are skipped and quarantined ones are reopened. It is the
// memory counterpart of PostgreSQL's RequestReplay plus RunReplay.
//
// A replay that is interrupted (cancellation, a failed commit) is recorded as
// "interrupted" and releases its range: repeating the same request resumes
// through the inbox, which skips every event that already completed, so no
// effect is repeated. Only a replay that is still being executed blocks an
// overlapping one. Nothing survives a restart.
func (m *Mem) Replay(ctx context.Context, req events.ReplayRequest) (ReplayResult, error) {
	grant, err := events.Authorize(ctx, m.opts.Authorizer, req)
	if err != nil {
		return ReplayResult{}, err
	}
	b, ok := m.consumers[req.Consumer]
	if !ok {
		return ReplayResult{}, fmt.Errorf("%w: consumer %q is not registered", events.ErrInvalidConfiguration, req.Consumer)
	}
	release, err := m.claimRange(req)
	if err != nil {
		return ReplayResult{}, err
	}
	defer release()

	rec := replayRec{ID: newID("rpl_"), Request: req, State: "running", NextSeq: req.FromSeq, CreatedAt: m.now()}
	type item struct {
		seq     int64
		payload []byte
	}
	var batch []item
	if err := m.update(ctx, req.Scope.TenantID, func(tx memdb.Tx) error {
		batch = batch[:0]
		c, err := m.classify(tx, req, b)
		if err != nil {
			return err
		}
		if err := c.Check(b.Spec, req.AllowBeyondHorizon); err != nil {
			return err
		}
		rec.Classification = c
		obsT.Scan(tx, func(o obsRec) bool {
			if o.ScopeKey == req.Scope.Key() && o.Seq >= req.FromSeq && o.Seq <= req.ToSeq && b.Spec.Accepts(o.Type) {
				batch = append(batch, item{seq: o.Seq, payload: bytes.Clone(o.Payload)})
			}
			return true
		})
		return replayT.Put(tx, rec)
	}); err != nil {
		return ReplayResult{}, err
	}
	result := func() ReplayResult {
		return ReplayResult{ID: rec.ID, State: rec.State, Classification: rec.Classification, Processed: rec.Processed, Skipped: rec.Skipped, Failed: rec.Failed}
	}
	// interrupt settles the admitted request out of its running state with a
	// context that outlives the cancellation that interrupted it. Failing to
	// record it changes nothing: the range is released either way.
	interrupt := func(cause error) (ReplayResult, error) {
		rec.State = "interrupted"
		_ = m.update(context.WithoutCancel(ctx), req.Scope.TenantID, func(tx memdb.Tx) error { return replayT.Put(tx, rec) })
		return result(), cause
	}
	rp := &memReplay{grant: grant, id: rec.ID}
	for _, it := range batch {
		env, derr := eventsv1.Decode(it.payload)
		if derr != nil {
			rec.Failed++
			continue
		}
		pr, err := m.processEnvelope(ctx, req.Scope.TenantID, b, env, it.payload, rp)
		if errors.Is(err, errContentConflict) {
			rec.Failed++
			continue
		}
		if err != nil {
			return interrupt(fmt.Errorf("events: replay event %s: %w", env.ID, err))
		}
		switch {
		case pr.skipped:
			rec.Skipped++
		case pr.outcome.Kind == events.OutcomeCompleted:
			rec.Processed++
		default:
			rec.Failed++
		}
		rec.NextSeq = it.seq + 1
	}
	rec.State, rec.CompletedAt = "completed", m.now()
	if err := m.update(ctx, req.Scope.TenantID, func(tx memdb.Tx) error { return replayT.Put(tx, rec) }); err != nil {
		return interrupt(err)
	}
	return result(), nil
}

// claimRange registers a running replay and refuses one whose range overlaps a
// replay of the same consumer and scope that is still being executed.
func (m *Mem) claimRange(req events.ReplayRequest) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.running {
		if r.consumer == req.Consumer && r.scope == req.Scope && r.from <= req.ToSeq && r.to >= req.FromSeq {
			return nil, fmt.Errorf("%w: an active replay already covers part of this range", events.ErrReplayUnsafe)
		}
	}
	mine := runningReplay{consumer: req.Consumer, scope: req.Scope, from: req.FromSeq, to: req.ToSeq}
	m.running = append(m.running, mine)
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for i, r := range m.running {
			if r == mine {
				m.running = append(m.running[:i], m.running[i+1:]...)
				return
			}
		}
	}, nil
}

// ---- local effects for contract and unit tests ---------------------------------

// Note writes a marker row in the caller's Tx. It is the stand-in for a
// producer's own state or a consumer's local effect: whatever commits or rolls
// back with the Tx does so with it.
func Note(tx memdb.Tx, key string) error { return effectsT.Put(tx, effectRec{Key: key}) }

// Notes lists the markers of the tenant in key order.
func (m *Mem) Notes(ctx context.Context, tenant domain.TenantID) ([]string, error) {
	var out []string
	err := m.store.View(ctx, tenant, func(tx memdb.Tx) error {
		effectsT.Scan(tx, func(r effectRec) bool { out = append(out, r.Key); return true })
		return nil
	})
	return out, err
}

// EventIDs lists the IDs in the tenant's outbox, published or not, in order.
func (m *Mem) EventIDs(ctx context.Context, tenant domain.TenantID) ([]string, error) {
	var out []string
	err := m.store.View(ctx, tenant, func(tx memdb.Tx) error {
		outboxT.Scan(tx, func(r outboxRec) bool { out = append(out, r.Pub.EventID); return true })
		return nil
	})
	return out, err
}
