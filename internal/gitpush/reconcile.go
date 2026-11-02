package gitpush

import (
	"context"
	"errors"
	"fmt"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

const (
	reconcilePage    = 50
	maxJournalPages  = 64
	journalPageLimit = gitv1.MaxPage
)

// Reconcile settles every unsettled entry of the repository: authorized entries
// refused, committing entries by gitv1.Settle over evidence the worker supplies
// (receive liveness, the ref's journal, the current value under the worker's
// lock) and the highest journal head seen before. Evidence is read first, with
// no SQL lock held; the decision is applied in its own short transaction, only
// to entries still in the state the evidence was gathered for. The refs, the
// reflog or the deletion of a Pod are never evidence on their own: only the
// journal attributes a transaction to a receive.
//
//   - wait (the receive still running, the worker unreachable, the ref not
//     quiescent) leaves the entry for the next pass;
//   - unknown records a visible reason and blocks writer reassignment
//     (UnknownBlocksWriter) and new seed grants (UnsettledPush);
//   - an error of the worker (Unknown or Unavailable outcome alike) leaves the
//     entry unchanged and is returned so the caller schedules another pass.
//
// It also repeats a pending emptiness report to the repository module.
func (s *Service) Reconcile(ctx context.Context, tenant domain.TenantID, repositoryID string) error {
	if s.d.Worker == nil {
		return fmt.Errorf("%w: a worker is required to reconcile", ErrNotConfigured)
	}
	if err := tenant.Validate(); err != nil {
		return notFound()
	}
	var errs []error
	if err := s.flushEmptyReports(ctx, tenant, repositoryID); err != nil {
		errs = append(errs, err)
	}
	after := ""
	for {
		var page []*Record
		err := s.d.Store.View(ctx, tenant, func(ctx context.Context, tx Tx) (err error) {
			page, err = tx.Unsettled(ctx, repositoryID, after, reconcilePage)
			return err
		})
		if err != nil {
			return errors.Join(append(errs, depErr(err, "push records"))...)
		}
		if len(page) == 0 {
			break
		}
		for _, rec := range page {
			if err := s.reconcileRecord(ctx, tenant, rec); err != nil {
				errs = append(errs, err)
			}
			if ctx.Err() != nil {
				return errors.Join(append(errs, ctx.Err())...)
			}
		}
		after = page[len(page)-1].ID
	}
	return errors.Join(errs...)
}

func (s *Service) flushEmptyReports(ctx context.Context, tenant domain.TenantID, repositoryID string) error {
	var pending []*Record
	err := s.d.Store.View(ctx, tenant, func(ctx context.Context, tx Tx) (err error) {
		pending, err = tx.EmptyReportPending(ctx, repositoryID, reconcilePage)
		return err
	})
	if err != nil {
		return depErr(err, "push records")
	}
	var errs []error
	for _, rec := range pending {
		if err := s.flushEmptyReport(ctx, tenant, rec); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type decision struct {
	ref  string
	from gitv1.EntryState
	d    gitv1.Decision
}

func (s *Service) reconcileRecord(ctx context.Context, tenant domain.TenantID, rec *Record) error {
	now := s.now()
	pastDeadline := !now.Before(rec.Deadline)
	var errs []error

	liveness := gitv1.LivenessUnavailable
	live, err := s.d.Worker.ReceiveLiveness(ctx, tenant, rec.RepositoryID, rec.ReceiveID)
	switch {
	case err != nil:
		errs = append(errs, unavailable(err, "receive liveness"))
	case live.Running:
		liveness = gitv1.LivenessRunning
	default:
		liveness = gitv1.LivenessEnded
	}

	var (
		decisions []decision
		head      int64
		committed []Entry
	)
	for _, e := range rec.Entries {
		switch e.State {
		case gitv1.EntryAuthorized:
			// A late prepared hook is refused commit intent, so settling an entry
			// without commit intent is always safe; it waits only while the worker
			// still runs the receive inside its deadline. After the deadline every
			// commit intent is refused (receive_expired).
			if liveness == gitv1.LivenessEnded || pastDeadline {
				decisions = append(decisions, decision{e.Ref, e.State, gitv1.Decision{Resolution: gitv1.ResolveRefused, Reason: gitv1.ReasonNoCommitIntent}})
			}
		case gitv1.EntryCommitting:
			committed = append(committed, e)
		}
	}
	if len(committed) > 0 && liveness == gitv1.LivenessEnded {
		known, err := s.knownHead(ctx, tenant, rec.RepositoryID)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		g := &gather{known: known, seen: known, giveUp: now.After(rec.Deadline.Add(s.o.EvidenceTimeout))}
		own, ownErr := s.readJournal(ctx, tenant, gitv1.ReadJournalRequest{RepositoryID: rec.RepositoryID, ReceiveID: &rec.ReceiveID, SinceSequence: 0, Limit: journalPageLimit}, g)
		for _, e := range committed {
			d, derr := s.decideCommitting(ctx, tenant, rec, e, own, ownErr, g)
			if derr != nil {
				errs = append(errs, derr)
			}
			if d.Resolution != gitv1.ResolveWait && derr == nil {
				decisions = append(decisions, decision{e.Ref, e.State, d})
			}
		}
		head = g.seen
		if g.regressed {
			// One observed regression taints the whole settlement: a decision taken
			// before it was seen, and an entry still waiting, cannot settle normally
			// once the journal catches up. Every committing entry becomes unknown,
			// which is terminal (the database refuses any move out of unknown).
			kept := decisions[:0]
			for _, d := range decisions {
				if d.from != gitv1.EntryCommitting {
					kept = append(kept, d)
				}
			}
			decisions = kept
			for _, e := range committed {
				decisions = append(decisions, decision{e.Ref, e.State, gitv1.Decision{Resolution: gitv1.ResolveUnknown, Reason: gitv1.ReasonJournalRegressed}})
			}
		}
	}
	if len(decisions) == 0 && head == 0 {
		return errors.Join(errs...)
	}

	var cur *Record
	err = s.d.Store.Do(ctx, tenant, func(ctx context.Context, tx Tx) error {
		c, err := tx.ByID(ctx, rec.ID, true)
		if err != nil {
			return err
		}
		before := c.Status()
		changed := false
		for _, d := range decisions {
			ok, err := c.Resolve(d.ref, d.from, d.d, now)
			if err != nil {
				return err
			}
			changed = changed || ok
		}
		if head > 0 {
			if err := tx.NoteHead(ctx, c.RepositoryID, head, now); err != nil {
				return err
			}
		}
		cur = c
		if !changed {
			return nil
		}
		c.Seal(now)
		return tx.Save(ctx, c, settlementEffects(before, c, SystemActor, nil))
	})
	if err != nil {
		return errors.Join(append(errs, mapStoreErr(err))...)
	}
	if err := s.flushEmptyReport(ctx, tenant, cur); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *Service) knownHead(ctx context.Context, tenant domain.TenantID, repositoryID string) (int64, error) {
	var head int64
	err := s.d.Store.View(ctx, tenant, func(ctx context.Context, tx Tx) (err error) {
		head, err = tx.KnownHead(ctx, repositoryID)
		return err
	})
	if err != nil {
		return 0, depErr(err, "journal head")
	}
	return head, nil
}

// journalRead is a complete read of the journal entries a request selects.
type journalRead struct {
	entries []gitv1.JournalEntry
	head    int64
}

// readJournal reads every page of a selection (bounded): a journal that does
// not end within the bound is an error, never a partial answer. The head of
// every page is observed by g as soon as it arrives, so a head lost to a later
// failure or hidden by a later page still counts.
func (s *Service) readJournal(ctx context.Context, tenant domain.TenantID, req gitv1.ReadJournalRequest, g *gather) (journalRead, error) {
	var out journalRead
	for page := 0; page < maxJournalPages; page++ {
		resp, err := s.d.Worker.ReadJournal(ctx, tenant, req)
		if err != nil {
			return journalRead{}, err
		}
		g.observe(resp.HeadSequence)
		out.entries = append(out.entries, resp.Entries...)
		out.head = resp.HeadSequence
		if !resp.HasMore || len(resp.Entries) == 0 {
			return out, nil
		}
		req.SinceSequence = resp.Entries[len(resp.Entries)-1].Sequence
	}
	return journalRead{}, fmt.Errorf("the journal of %s did not end within %d pages", truncate(req.RepositoryID, 64), maxJournalPages)
}

// gather is the evidence state of one record's reconciliation: the head the
// control plane knew, the highest head observed since, and whether an
// unreadable journal has been waited for long enough. Every head a worker
// reports (each page of every journal read, each value snapshot, quiescent or
// not) is observed at once: a head below the persisted one or below one already
// observed in this settlement is a regression (a volume restored from an older
// snapshot, even between two reads) and is latched, so every committing entry of
// the settlement is unknown (journal_regressed), whatever the later reads show.
// The highest head survives failures and is persisted.
type gather struct {
	known     int64
	seen      int64
	regressed bool
	giveUp    bool
}

func (g *gather) observe(head int64) {
	if head < g.seen {
		g.regressed = true
		return
	}
	g.seen = head
}

// decideCommitting gathers the journal evidence of one committing entry and
// applies gitv1.Settle. The evidence is one consistent snapshot: the ref's
// journal is read from the entry's own prepared entry on, and when a value
// under the lock is needed its journal head must not be beyond the journal read
// (otherwise another transaction may have run in between, so the journal is read
// again up to it) nor below a head already observed (a restore). It returns the
// decision and the worker error that left the entry unchanged, if any; a
// transport failure never settles, only a worker's explicit answer that its
// journal is unreadable (ErrJournalUnreadable) does, after the evidence timeout.
func (s *Service) decideCommitting(ctx context.Context, tenant domain.TenantID, rec *Record, e Entry, own journalRead, ownErr error, g *gather) (gitv1.Decision, error) {
	entry := gitv1.SettleEntry{ReceiveID: rec.ReceiveID, Ref: e.Ref, Old: e.Old, New: e.New, State: e.State}
	wait := gitv1.Decision{Resolution: gitv1.ResolveWait, Reason: gitv1.ReasonLivenessUnavailable}
	regressed := gitv1.Decision{Resolution: gitv1.ResolveUnknown, Reason: gitv1.ReasonJournalRegressed}
	evidence := gitv1.SettleEvidence{Liveness: gitv1.LivenessEnded, JournalReadable: true}

	unreadable := func(cause error) (gitv1.Decision, error) {
		if !errors.Is(cause, ErrJournalUnreadable) || !g.giveUp {
			// A transport failure (Unknown or Unavailable) is no evidence at all.
			return wait, unavailable(cause, "ref journal")
		}
		evidence.JournalReadable = false
		return gitv1.Settle(entry, evidence)
	}
	if g.regressed {
		return regressed, nil
	}
	if ownErr != nil {
		return unreadable(ownErr)
	}
	var prepared *gitv1.JournalEntry
	for i := range own.entries {
		j := own.entries[i]
		if j.Ref == e.Ref && j.State == gitv1.JournalPrepared && j.ReceiveID != nil && *j.ReceiveID == rec.ReceiveID && (prepared == nil || j.Sequence < prepared.Sequence) {
			prepared = &own.entries[i]
		}
	}
	if prepared == nil {
		evidence.Journal, evidence.JournalHead, evidence.KnownHead = own.entries, own.head, g.known
		return gitv1.Settle(entry, evidence)
	}

	// Every journal entry of the ref, whatever its attribution, from the entry's
	// own prepared entry on: the next prepared entry decides.
	ref := e.Ref
	readRef := func() (journalRead, error) {
		return s.readJournal(ctx, tenant, gitv1.ReadJournalRequest{RepositoryID: rec.RepositoryID, Ref: &ref, SinceSequence: prepared.Sequence - 1, Limit: journalPageLimit}, g)
	}
	evidence.KnownHead = g.seen
	rj, err := readRef()
	if g.regressed { // a regression seen on any page outranks a later failure
		return regressed, nil
	}
	if err != nil {
		return unreadable(err)
	}
	evidence.Journal, evidence.JournalHead = rj.entries, rj.head

	// snapshot reads the ref's value under the lock at a boundary the journal
	// evidence covers: it is quiescent, not below a head already observed (a
	// restore) and its journal head is not beyond the journal read (otherwise
	// another transaction ran in between, so the journal is read again up to it;
	// a transaction on this ref then has a prepared entry, which decides in place
	// of the value). A non-nil decision ends the settlement.
	snapshot := func() (*gitv1.CurrentRef, *gitv1.Decision, error) {
		values, err := s.d.Worker.ReadRefValues(ctx, tenant, gitv1.ReadRefValuesRequest{RepositoryID: rec.RepositoryID, Refs: []string{e.Ref}})
		if err != nil {
			return nil, &gitv1.Decision{Resolution: gitv1.ResolveWait, Reason: gitv1.ReasonRefNotQuiescent}, unavailable(err, "ref value")
		}
		g.observe(values.JournalHead) // quiescent or not: it is an observation
		if g.regressed {
			return nil, &regressed, nil
		}
		if !values.Quiescent {
			return nil, &gitv1.Decision{Resolution: gitv1.ResolveWait, Reason: gitv1.ReasonRefNotQuiescent}, nil
		}
		if values.JournalHead > evidence.JournalHead {
			again, err := readRef()
			if g.regressed {
				return nil, &regressed, nil
			}
			if err != nil {
				return nil, &gitv1.Decision{Resolution: gitv1.ResolveWait, Reason: gitv1.ReasonRefNotQuiescent}, unavailable(err, "ref journal")
			}
			if again.head < values.JournalHead {
				return nil, &regressed, nil // a restore, or a journal behind a snapshot taken before it
			}
			evidence.Journal, evidence.JournalHead, evidence.KnownHead = again.entries, again.head, g.known
		}
		current := gitv1.CurrentRef{Commit: gitv1.ZeroObjectID(len(e.New)), Quiescent: true}
		for _, v := range values.Values {
			if v.Ref == e.Ref && v.Commit != nil {
				current.Commit = *v.Commit
			}
		}
		return &current, nil, nil
	}

	d, err := gitv1.Settle(entry, evidence)
	if err != nil {
		return d, err
	}
	switch {
	case d.Resolution == gitv1.ResolveWait && d.Reason == gitv1.ReasonRefNotQuiescent:
		// Only the prepared entry and no later one: the value under the lock decides.
		cur, early, err := snapshot()
		if early != nil {
			return *early, err
		}
		evidence.Current = cur
		return gitv1.Settle(entry, evidence)
	case d.Resolution == gitv1.ResolveApplied:
		// The transaction's own committed entry settles it, but a committed hook
		// also runs for an update a batched transaction rejected, so the entry is
		// applied only if the ref agrees: the next prepared entry of the ref
		// observed the new value, or, with none, the value under the lock is the
		// new value. Disagreement is conflicting evidence, never a guess.
		conflicting := gitv1.Decision{Resolution: gitv1.ResolveUnknown, Reason: gitv1.ReasonConflictingEvidence}
		next := nextPrepared(evidence.Journal, e.Ref, prepared.Sequence)
		if next == nil {
			cur, early, err := snapshot()
			if early != nil {
				return *early, err
			}
			if next = nextPrepared(evidence.Journal, e.Ref, prepared.Sequence); next == nil {
				if cur.Commit != e.New {
					return conflicting, nil
				}
				return d, nil
			}
		}
		if next.Observed == nil || *next.Observed != e.New {
			return conflicting, nil
		}
	}
	return d, nil
}

// nextPrepared is the first prepared entry of ref after the given sequence,
// whatever its attribution: it observed the result of every earlier transaction
// on the ref.
func nextPrepared(journal []gitv1.JournalEntry, ref string, after int64) *gitv1.JournalEntry {
	var next *gitv1.JournalEntry
	for i := range journal {
		j := &journal[i]
		if j.Ref == ref && j.State == gitv1.JournalPrepared && j.Sequence > after && (next == nil || j.Sequence < next.Sequence) {
			next = j
		}
	}
	return next
}
